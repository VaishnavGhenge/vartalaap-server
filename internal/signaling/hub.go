package signaling

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"sync"

	"github.com/coder/websocket"
	"github.com/vaishnavghenge/vartalaap-server/internal/metrics"
)

type Hub struct {
	mu             sync.Mutex
	rooms          map[string]*Room
	onRoomEmpty    func(roomID string)
	guestTokenFn   func(peerID, roomID string) (string, error)
}

func NewHub() *Hub {
	return &Hub{rooms: make(map[string]*Room)}
}

func (h *Hub) SetRoomEmptyHandler(handler func(roomID string)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.onRoomEmpty = handler
}

func (h *Hub) join(c *Client, roomID string) {
	var replaced *Client

	h.mu.Lock()
	if c.room != "" && c.room != roomID {
		if old, ok := h.rooms[c.room]; ok {
			old.remove(c.id)
			h.gcLocked(old)
		}
	}
	room, ok := h.rooms[roomID]
	if !ok {
		room = newRoom(roomID)
		h.rooms[roomID] = room
		metrics.ActiveRooms.Inc()
	}
	c.mu.RLock()
	presenceID := c.presenceID
	c.mu.RUnlock()
	replaced = room.removeByPresenceID(presenceID, c.id)
	if replaced != nil {
		replaced.room = ""
		room.removeSfuTracks(replaced.id)
	}
	existing := room.peerInfos()
	room.add(c)
	c.room = roomID
	h.mu.Unlock()

	if replaced != nil {
		evt, _ := json.Marshal(PeerLeftData{PeerID: replaced.id})
		payload, _ := json.Marshal(Envelope{Type: MsgPeerLeft, Room: roomID, From: replaced.id, Data: evt})
		room.broadcastExceptIDs(map[string]bool{replaced.id: true, c.id: true}, payload)
		if replaced.conn != nil {
			_ = replaced.conn.Close(websocket.StatusNormalClosure, "replaced by reconnect")
		}
	}

	joinedData, _ := json.Marshal(JoinedData{Peers: existing})
	c.sendJSON(&Envelope{Type: MsgJoined, Room: roomID, Data: joinedData})

	// Replay each existing peer's published SFU tracks so the new joiner can subscribe.
	for fromPeerID, trackData := range room.sfuTrackSnapshot() {
		b, _ := json.Marshal(trackData)
		c.sendJSON(&Envelope{Type: MsgSfuTracks, Room: roomID, From: fromPeerID, Data: b})
	}

	info := c.info()
	evt, _ := json.Marshal(PeerJoinedData{
		PeerID: info.ID, Name: info.Name, Audio: info.Audio, Video: info.Video, ScreenSharing: info.ScreenSharing, VideoHeld: info.VideoHeld,
	})
	payload, _ := json.Marshal(Envelope{Type: MsgPeerJoined, Room: roomID, From: c.id, Data: evt})
	room.broadcastExcept(c.id, payload)
}

func (h *Hub) leaveAll(c *Client) {
	h.mu.Lock()
	roomID := c.room
	if roomID == "" {
		h.mu.Unlock()
		return
	}
	c.room = ""
	room, ok := h.rooms[roomID]
	if !ok {
		h.mu.Unlock()
		return
	}
	room.remove(c.id)
	room.removeSfuTracks(c.id)
	h.gcLocked(room)
	h.mu.Unlock()

	evt, _ := json.Marshal(PeerLeftData{PeerID: c.id})
	payload, _ := json.Marshal(Envelope{Type: MsgPeerLeft, Room: roomID, From: c.id, Data: evt})
	room.broadcastExcept(c.id, payload)
}

func (h *Hub) broadcastState(c *Client, st PeerStateData) {
	h.mu.Lock()
	room := h.rooms[c.room]
	h.mu.Unlock()
	if room == nil {
		return
	}
	data, _ := json.Marshal(st)
	payload, _ := json.Marshal(Envelope{Type: MsgPeerState, Room: c.room, From: c.id, Data: data})
	room.broadcastExceptBestEffort(c.id, payload)
}

func (h *Hub) forwardState(from *Client, to string, st PeerStateData) {
	h.mu.Lock()
	room := h.rooms[from.room]
	h.mu.Unlock()
	if room == nil {
		from.sendError("not in a room")
		return
	}
	target := room.get(to)
	if target == nil {
		from.sendError("target peer not in room")
		return
	}
	data, _ := json.Marshal(st)
	payload, _ := json.Marshal(Envelope{Type: MsgPeerState, Room: from.room, From: from.id, To: to, Data: data})
	if !target.enqueue(payload) {
		from.sendError("target peer state queue is full")
	}
}

// BroadcastSfuTracks merges the new tracks into the peer's cumulative set, then
// broadcasts the full merged list to all other peers in the room. Storing the
// merged set (not just the latest batch) ensures late joiners receive all tracks
// when the hub replays sfu-tracks during join.
func (h *Hub) BroadcastSfuTracks(roomID, peerID string, data SfuTracksData) {
	h.mu.Lock()
	room := h.rooms[roomID]
	h.mu.Unlock()
	if room == nil {
		return
	}
	merged := room.storeSfuTracks(peerID, data)
	b, _ := json.Marshal(merged)
	payload, _ := json.Marshal(Envelope{Type: MsgSfuTracks, Room: roomID, From: peerID, Data: b})
	room.broadcastExcept(peerID, payload)
}

// Must be called with h.mu held.
func (h *Hub) gcLocked(r *Room) {
	if r.empty() {
		delete(h.rooms, r.id)
		metrics.ActiveRooms.Dec()
		if h.onRoomEmpty != nil {
			go h.onRoomEmpty(r.id)
		}
	}
}

func newPeerID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// SetGuestTokenFn registers the function used by knockAdmit to mint a
// room-scoped guest JWT. Must be called before the hub starts serving
// knock/knock-admit messages.
func (h *Hub) SetGuestTokenFn(fn func(peerID, roomID string) (string, error)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.guestTokenFn = fn
}

// knock broadcasts a knock-request to all other peers in c's room.
// If the room is empty (no one else is present), it sends a KNOCK_NO_HOST
// error back to c instead.
func (h *Hub) knock(c *Client) {
	h.mu.Lock()
	room := h.rooms[c.room]
	h.mu.Unlock()

	if room == nil {
		c.sendErrorCode("NOT_IN_ROOM", "you must join a room before knocking")
		return
	}

	peers := room.peerInfos()
	hasOther := false
	for _, p := range peers {
		if p.ID != c.id {
			hasOther = true
			break
		}
	}
	if !hasOther {
		c.sendErrorCode("KNOCK_NO_HOST", "No one else is in the room yet.")
		return
	}

	c.mu.RLock()
	name := c.name
	c.mu.RUnlock()

	data, _ := json.Marshal(KnockRequestData{PeerID: c.id, Name: name})
	payload, _ := json.Marshal(Envelope{Type: MsgKnockRequest, Room: c.room, From: c.id, Data: data})
	room.broadcastExcept(c.id, payload)
}

// knockAdmit generates a guest JWT for targetPeerID and delivers it via a
// knock-granted message. Only peers in the same room as `from` may admit.
func (h *Hub) knockAdmit(from *Client, targetPeerID string) {
	h.mu.Lock()
	room := h.rooms[from.room]
	tokenFn := h.guestTokenFn
	h.mu.Unlock()

	if room == nil {
		from.sendError("not in a room")
		return
	}
	if tokenFn == nil {
		from.sendError("guest tokens not configured")
		return
	}

	target := room.get(targetPeerID)
	if target == nil {
		from.sendError("peer not in room")
		return
	}

	token, err := tokenFn(targetPeerID, from.room)
	if err != nil {
		from.sendError("could not issue token")
		return
	}

	data, _ := json.Marshal(KnockGrantedData{SfuToken: token})
	payload, _ := json.Marshal(Envelope{Type: MsgKnockGranted, Room: from.room, Data: data})
	target.enqueue(payload)
}
