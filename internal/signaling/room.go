package signaling

import "sync"

type Room struct {
	id         string
	mu         sync.RWMutex
	members    map[string]*Client
	sfuTracks  map[string]SfuTracksData // peerID → published tracks
}

func newRoom(id string) *Room {
	return &Room{id: id, members: make(map[string]*Client), sfuTracks: make(map[string]SfuTracksData)}
}

// storeSfuTracks merges the new tracks into the peer's cumulative set, keyed by
// trackName. Returns the full merged payload so the caller can broadcast it.
// This ensures both live peers and late joiners always see all published tracks,
// not just the last batch (e.g. audio-only first publish, video added later).
func (r *Room) storeSfuTracks(peerID string, data SfuTracksData) SfuTracksData {
	r.mu.Lock()
	defer r.mu.Unlock()
	existing, ok := r.sfuTracks[peerID]
	if !ok {
		r.sfuTracks[peerID] = data
		return data
	}
	// Upsert by trackName — new data wins on conflict. SessionID is always updated
	// because it changes when a peer leaves and rejoins with a new CF session.
	byName := make(map[string]SfuTrackInfo, len(existing.Tracks)+len(data.Tracks))
	for _, t := range existing.Tracks {
		byName[t.TrackName] = t
	}
	for _, t := range data.Tracks {
		byName[t.TrackName] = t
	}
	merged := make([]SfuTrackInfo, 0, len(byName))
	for _, t := range byName {
		merged = append(merged, t)
	}
	result := SfuTracksData{SessionID: data.SessionID, Tracks: merged}
	r.sfuTracks[peerID] = result
	return result
}

func (r *Room) removeSfuTracks(peerID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.sfuTracks, peerID)
}

// sfuTrackSnapshot returns a copy of all stored sfu-tracks entries.
// Safe to call without the lock held by the caller.
func (r *Room) sfuTrackSnapshot() map[string]SfuTracksData {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]SfuTracksData, len(r.sfuTracks))
	for k, v := range r.sfuTracks {
		out[k] = v
	}
	return out
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
