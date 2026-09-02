package signaling

import (
	"sort"
	"sync"
)

type Room struct {
	id      string
	mu      sync.RWMutex
	members map[string]*Client
	// peerID → published tracks. The authoritative view of who is sending
	// what, written only by the sfu-announce path.
	sfuTracks map[string]SfuTracksData
	// Bumped on every mutation of sfuTracks. Snapshots carry it so a client can
	// discard one that was built before an update it has already applied:
	// without it, a snapshot in flight when a peer announces would roll that
	// announcement back and the track would go dark until the next snapshot.
	version uint64
	// peerID → the admission request that peer is still waiting on. Kept as
	// room state, not a one-shot broadcast, so join() can replay it to a host
	// who arrives after the guest.
	pendingKnocks map[string]KnockRequestData
}

func newRoom(id string) *Room {
	return &Room{
		id:            id,
		members:       make(map[string]*Client),
		sfuTracks:     make(map[string]SfuTracksData),
		pendingKnocks: make(map[string]KnockRequestData),
	}
}

// setSfuTracks replaces the peer's stored track set wholesale. This is the
// only writer: the publisher announces its full current set after each CF push
// ack, so replace (never merge) is what keeps a track the peer stopped
// publishing from lingering in the room's view forever. Used by the
// sfu-announce path, where the client sends its complete current set —
// replace semantics drop track names left over from a previous CF session,
// which the merge in storeSfuTracks would keep forever.
// setSfuTracks returns the version the room is at after the write, so the
// broadcast that follows can carry it and be ordered against snapshots.
func (r *Room) setSfuTracks(peerID string, data SfuTracksData) uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.version++
	data.Version = r.version
	r.sfuTracks[peerID] = data
	return r.version
}

func (r *Room) removeSfuTracks(peerID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.sfuTracks[peerID]; !ok {
		return
	}
	r.version++
	delete(r.sfuTracks, peerID)
}

// addPendingKnock records a request for admission. Re-knocking replaces the
// entry rather than queueing a second one.
func (r *Room) addPendingKnock(peerID string, data KnockRequestData) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pendingKnocks[peerID] = data
}

// removePendingKnock drops a knock that was answered or abandoned.
func (r *Room) removePendingKnock(peerID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.pendingKnocks, peerID)
}

// pendingKnockSnapshot returns the knocks still waiting on a decision. Skips
// any whose peer has since left, which would otherwise be a ghost in the
// host's admit queue that no peer-left clears.
func (r *Room) pendingKnockSnapshot() []KnockRequestData {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]KnockRequestData, 0, len(r.pendingKnocks))
	for peerID, data := range r.pendingKnocks {
		if _, ok := r.members[peerID]; !ok {
			continue
		}
		out = append(out, data)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PeerID < out[j].PeerID })
	return out
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

// snapshot is the room's complete current state: who is present and what each
// of them publishes. It is what makes the protocol level-triggered — a client
// that missed any single broadcast converges from this without needing to know
// which edge it lost.
func (r *Room) snapshot() RoomSnapshotData {
	r.mu.RLock()
	defer r.mu.RUnlock()
	tracks := make([]RoomSnapshotPeerTracks, 0, len(r.sfuTracks))
	for peerID, data := range r.sfuTracks {
		// A stored set for someone no longer in the room is stale by
		// definition. Sending it would have every client subscribe to a
		// departed peer's tracks and then time them out.
		if _, ok := r.members[peerID]; !ok {
			continue
		}
		tracks = append(tracks, RoomSnapshotPeerTracks{
			PeerID:    peerID,
			SessionID: data.SessionID,
			Tracks:    data.Tracks,
		})
	}
	// Deterministic order keeps snapshots comparable in logs and tests; the
	// client does not depend on it.
	sort.Slice(tracks, func(i, j int) bool { return tracks[i].PeerID < tracks[j].PeerID })
	return RoomSnapshotData{
		Version: r.version,
		Peers:   r.peerInfosLocked(),
		Tracks:  tracks,
	}
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

// peerInfos returns info for all admitted members (needsAdmit=false).
// Unadmitted guests are excluded so they never appear in another peer's
// tile grid before the host lets them in, and so knock() can check
// "is there anyone who can admit this guest?" without false positives.
func (r *Room) peerInfos() []PeerInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.peerInfosLocked()
}

// Must be called with r.mu held (read or write).
func (r *Room) peerInfosLocked() []PeerInfo {
	infos := make([]PeerInfo, 0, len(r.members))
	for _, c := range r.members {
		c.mu.RLock()
		needsAdmit := c.needsAdmit
		c.mu.RUnlock()
		if needsAdmit {
			continue
		}
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

// broadcastAllBestEffort sends to every member, including the peer a message
// is about. Snapshots go to everyone: a publisher needs to see the room's view
// of its own tracks to notice when the server's copy has drifted from what it
// believes it is sending.
func (r *Room) broadcastAllBestEffort(payload []byte) {
	clients := r.clientsExceptIDs(nil)
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
