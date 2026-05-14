package signaling

import "sync"

type Room struct {
	id      string
	mu      sync.RWMutex
	members map[string]*Client
}

func newRoom(id string) *Room {
	return &Room{id: id, members: make(map[string]*Client)}
}

func (r *Room) add(c *Client) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.members[c.id] = c
}

func (r *Room) remove(peerID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.members, peerID)
}

func (r *Room) removeByPresenceID(presenceID, exceptPeerID string) *Client {
	if presenceID == "" {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for peerID, c := range r.members {
		if peerID == exceptPeerID {
			continue
		}
		c.mu.RLock()
		matches := c.presenceID == presenceID
		c.mu.RUnlock()
		if matches {
			delete(r.members, peerID)
			return c
		}
	}
	return nil
}

func (r *Room) empty() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.members) == 0
}

func (r *Room) peerInfos() []PeerInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()
	infos := make([]PeerInfo, 0, len(r.members))
	for _, c := range r.members {
		infos = append(infos, c.info())
	}
	return infos
}

func (r *Room) get(peerID string) *Client {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.members[peerID]
}

func (r *Room) broadcastExcept(exceptID string, payload []byte) {
	clients := r.clientsExcept(exceptID)
	for _, c := range clients {
		c.enqueue(payload)
	}
}

func (r *Room) broadcastExceptIDs(exceptIDs map[string]bool, payload []byte) {
	clients := r.clientsExceptIDs(exceptIDs)
	for _, c := range clients {
		c.enqueue(payload)
	}
}

func (r *Room) broadcastExceptBestEffort(exceptID string, payload []byte) {
	clients := r.clientsExcept(exceptID)
	for _, c := range clients {
		c.enqueueBestEffort(payload)
	}
}

func (r *Room) clientsExcept(exceptID string) []*Client {
	return r.clientsExceptIDs(map[string]bool{exceptID: true})
}

func (r *Room) clientsExceptIDs(exceptIDs map[string]bool) []*Client {
	r.mu.RLock()
	defer r.mu.RUnlock()
	clients := make([]*Client, 0, len(r.members))
	for id, c := range r.members {
		if exceptIDs[id] {
			continue
		}
		clients = append(clients, c)
	}
	return clients
}
