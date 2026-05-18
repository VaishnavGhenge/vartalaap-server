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
	mu          sync.Mutex
	rooms       map[string]*Room
	onRoomEmpty func(roomID string)
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
