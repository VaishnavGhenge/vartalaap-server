package signaling

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/vaishnavghenge/vartalaap-server/internal/metrics"
)

type Hub struct {
	mu           sync.Mutex
	rooms        map[string]*Room
	onRoomEmpty  func(roomID string)
	guestTokenFn func(peerID, roomID string) (string, error)
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

	// Replay each existing peer's published SFU tracks so the new joiner can
	// subscribe. The stored Version is deliberately stripped: it records when
	// THAT PEER last announced, not where the room is now, and the replay walks
	// a map in arbitrary order. Sent as-is, a peer whose set was written at v3
	// arriving after one written at v5 would look stale and be discarded, and
	// that peer's media would never be pulled. A replay is a bulk state dump,
	// not an ordered update, so it carries no ordering information.
	for fromPeerID, trackData := range room.sfuTrackSnapshot() {
		trackData.Version = 0
		b, _ := json.Marshal(trackData)
		c.sendJSON(&Envelope{Type: MsgSfuTracks, Room: roomID, From: fromPeerID, Data: b})
	}

	// Then the authoritative view, which sets the joiner's version floor. The
	// replay above stays for its own sake: it is what a client that predates
	// snapshots needs, so server and client can deploy in either order.
	snap := room.snapshot()
	snapBytes, _ := json.Marshal(snap)
	c.sendJSON(&Envelope{Type: MsgRoomSnapshot, Room: roomID, Data: snapBytes})

	// Knocking guests (NeedsAdmit=true) must not appear in the host's tile grid
	// until the host explicitly admits them. Defer peer-joined until knockAdmit.
	c.mu.RLock()
	needsAdmit := c.needsAdmit
	c.mu.RUnlock()
	if !needsAdmit {
		info := c.info()
		evt, _ := json.Marshal(PeerJoinedData{
			PeerID: info.ID, Name: info.Name, Audio: info.Audio, Video: info.Video, ScreenSharing: info.ScreenSharing, VideoHeld: info.VideoHeld,
		})
		payload, _ := json.Marshal(Envelope{Type: MsgPeerJoined, Room: roomID, From: c.id, Data: evt})
		room.broadcastExcept(c.id, payload)
	}
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

// AnnounceSfuTracks handles a client's sfu-announce: the authoritative full
// set of tracks it currently publishes. The stored set is replaced (not
// merged) and rebroadcast as sfu-tracks. This is the self-healing path — the
// tracks/new interception fires only once per publish, so after a signaling
// reconnect wipes the stored set, only the client can restore it.
// SendSnapshot replies to a client's sync request with its room's full state.
func (h *Hub) SendSnapshot(c *Client) {
	h.mu.Lock()
	room := h.rooms[c.room]
	h.mu.Unlock()
	if room == nil {
		return
	}
	snap := room.snapshot()
	b, _ := json.Marshal(snap)
	c.sendJSON(&Envelope{Type: MsgRoomSnapshot, Room: room.id, Data: b})
}

// StartSnapshotLoop pushes a room snapshot to every occupied room on a timer,
// so a client that missed a broadcast converges without having to notice it
// missed one. Returns the stop function; the caller owns the goroutine's
// lifetime.
//
// One goroutine for the whole server, not one per room: the rooms are walked
// on each tick. On a small box a per-room goroutine would be the wrong thing
// to scale with participant count.
func (h *Hub) StartSnapshotLoop(interval time.Duration) func() {
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				h.broadcastSnapshots()
			}
		}
	}()
	return func() { close(done) }
}

func (h *Hub) broadcastSnapshots() {
	h.mu.Lock()
	rooms := make([]*Room, 0, len(h.rooms))
	for _, room := range h.rooms {
		rooms = append(rooms, room)
	}
	h.mu.Unlock()

	for _, room := range rooms {
		if room.empty() {
			continue
		}
		snap := room.snapshot()
		// A room where nobody publishes anything yet has nothing to converge
		// on, and the peers list is already maintained by join/leave. Skipping
		// keeps an idle room silent instead of ticking at every participant.
		if len(snap.Tracks) == 0 {
			continue
		}
		b, _ := json.Marshal(snap)
		payload, _ := json.Marshal(Envelope{Type: MsgRoomSnapshot, Room: room.id, Data: b})
		// Best-effort: a snapshot is a periodic convergence aid, so dropping one
		// for a peer whose send queue is backed up is correct. Blocking on a
		// slow client would stall the loop for every other room.
		room.broadcastAllBestEffort(payload)
	}
}

func (h *Hub) AnnounceSfuTracks(c *Client, data SfuTracksData) {
	h.mu.Lock()
	room := h.rooms[c.room]
	h.mu.Unlock()
	if room == nil {
		c.sendErrorCode("NOT_IN_ROOM", "join a room before announcing tracks")
		return
	}
	if room.get(c.id) == nil {
		return
	}
	metrics.SfuAnnounces.Inc()
	// Stamp the broadcast with the version the write landed at so a client can
	// order it against snapshots that may arrive out of sequence.
	data.Version = room.setSfuTracks(c.id, data)
	b, _ := json.Marshal(data)
	payload, _ := json.Marshal(Envelope{Type: MsgSfuTracks, Room: c.room, From: c.id, Data: b})
	room.broadcastExcept(c.id, payload)
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

	// Clear the deferred-join flag and announce the guest to all other peers now.
	target.mu.Lock()
	target.needsAdmit = false
	target.mu.Unlock()

	info := target.info()
	peerJoinedEvt, _ := json.Marshal(PeerJoinedData{
		PeerID: info.ID, Name: info.Name, Audio: info.Audio, Video: info.Video,
		ScreenSharing: info.ScreenSharing, VideoHeld: info.VideoHeld,
	})
	peerJoinedPayload, _ := json.Marshal(Envelope{Type: MsgPeerJoined, Room: from.room, From: target.id, Data: peerJoinedEvt})
	room.broadcastExcept(target.id, peerJoinedPayload)
}
