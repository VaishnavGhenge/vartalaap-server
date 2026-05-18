package sfu

import "sync"

type entry struct {
	userID string
	roomID string
	peerID string // WebSocket-assigned peer ID from the signaling layer
}

// Registry tracks Cloudflare Realtime session ownership.
// Each authenticated user may hold one active SFU session at a time.
type Registry struct {
	mu       sync.Mutex
	sessions map[string]entry  // CF sessionID → entry
	users    map[string]string // userID → latest CF sessionID
}

func NewRegistry() *Registry {
	return &Registry{
		sessions: make(map[string]entry),
		users:    make(map[string]string),
	}
}

func (r *Registry) Register(sessionID, userID, roomID, peerID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sessions[sessionID] = entry{userID: userID, roomID: roomID, peerID: peerID}
	r.users[userID] = sessionID
}

// Lookup returns the entry for sessionID and whether it was found.
func (r *Registry) Lookup(sessionID string) (userID, roomID, peerID string, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.sessions[sessionID]
	return e.userID, e.roomID, e.peerID, ok
}

func (r *Registry) Remove(sessionID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.sessions[sessionID]; ok {
		if r.users[e.userID] == sessionID {
			delete(r.users, e.userID)
		}
		delete(r.sessions, sessionID)
	}
}
