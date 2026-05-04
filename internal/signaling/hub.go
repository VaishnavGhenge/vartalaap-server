package signaling

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"sync"

	"github.com/vaishnavghenge/vartalaap-server/internal/metrics"
)

type Hub struct {
	mu    sync.Mutex
	rooms map[string]*Room
}

func NewHub() *Hub {
	return &Hub{rooms: make(map[string]*Room)}
}

func (h *Hub) join(c *Client, roomID string) {
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
	existing := room.peerInfos()
	room.add(c)
	c.room = roomID
	h.mu.Unlock()

	joinedData, _ := json.Marshal(JoinedData{Peers: existing})
	c.sendJSON(&Envelope{Type: MsgJoined, Room: roomID, Data: joinedData})

	info := c.info()
	evt, _ := json.Marshal(PeerJoinedData{
		PeerID: info.ID, Name: info.Name, Audio: info.Audio, Video: info.Video, ScreenSharing: info.ScreenSharing,
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
	room.broadcastExcept(c.id, payload)
}

func (h *Hub) forwardSignal(from *Client, env *Envelope) {
	h.mu.Lock()
	room := h.rooms[from.room]
	h.mu.Unlock()
	if room == nil {
		from.sendError("not in a room")
		return
	}
	target := room.get(env.To)
	if target == nil {
		from.sendError("target peer not in room")
		return
	}
	out := Envelope{Type: MsgSignal, Room: from.room, From: from.id, To: env.To, Data: env.Data}
	b, _ := json.Marshal(out)
	select {
	case target.send <- b:
	default:
	}
}

// Must be called with h.mu held.
func (h *Hub) gcLocked(r *Room) {
	if r.empty() {
		delete(h.rooms, r.id)
		metrics.ActiveRooms.Dec()
	}
}

func newPeerID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
