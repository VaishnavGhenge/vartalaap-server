package signaling

import (
	"encoding/json"
	"sync"
	"testing"
	"time"
)

func testClient(id string) *Client {
	return &Client{
		id:   id,
		send: make(chan []byte, 16),
	}
}

func setTestClientState(c *Client, name, presenceID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.name = name
	c.audio = true
	c.video = true
	c.presenceID = presenceID
}

func drainEnvelopes(t *testing.T, c *Client) []Envelope {
	t.Helper()

	var envelopes []Envelope
	for {
		select {
		case msg := <-c.send:
			var env Envelope
			if err := json.Unmarshal(msg, &env); err != nil {
				t.Fatalf("unmarshal envelope: %v", err)
			}
			envelopes = append(envelopes, env)
		default:
			return envelopes
		}
	}
}

func findEnvelope(envelopes []Envelope, typ MsgType) (Envelope, bool) {
	for _, env := range envelopes {
		if env.Type == typ {
			return env, true
		}
	}
	return Envelope{}, false
}

func boolPtr(v bool) *bool {
	return &v
}

func TestHubJoinReplacesStalePeerWithSamePresenceID(t *testing.T) {
	hub := NewHub()
	oldPeer := testClient("peer-old")
	observer := testClient("peer-observer")
	newPeer := testClient("peer-new")
	setTestClientState(oldPeer, "Alice", "presence-alice")
	setTestClientState(observer, "Bob", "presence-bob")
	setTestClientState(newPeer, "Alice", "presence-alice")

	hub.join(oldPeer, "room-1")
	hub.join(observer, "room-1")
	drainEnvelopes(t, oldPeer)
	drainEnvelopes(t, observer)

	hub.join(newPeer, "room-1")

	room := hub.rooms["room-1"]
	if room.get("peer-old") != nil {
		t.Fatal("expected old peer to be removed from the room")
	}
	if room.get("peer-new") == nil {
		t.Fatal("expected replacement peer to be added to the room")
	}

	newMessages := drainEnvelopes(t, newPeer)
	if _, ok := findEnvelope(newMessages, MsgPeerLeft); ok {
		t.Fatal("replacement peer should not receive peer-left for its own stale peer")
	}
	joined, ok := findEnvelope(newMessages, MsgJoined)
	if !ok {
		t.Fatal("expected replacement peer to receive joined")
	}
	var joinedData JoinedData
	if err := json.Unmarshal(joined.Data, &joinedData); err != nil {
		t.Fatalf("unmarshal joined data: %v", err)
	}
	for _, peer := range joinedData.Peers {
		if peer.ID == "peer-old" {
			t.Fatal("joined peer list included stale peer")
		}
	}

	observerMessages := drainEnvelopes(t, observer)
	left, ok := findEnvelope(observerMessages, MsgPeerLeft)
	if !ok {
		t.Fatal("expected observer to receive peer-left for stale peer")
	}
	var leftData PeerLeftData
	if err := json.Unmarshal(left.Data, &leftData); err != nil {
		t.Fatalf("unmarshal peer-left data: %v", err)
	}
	if leftData.PeerID != "peer-old" {
		t.Fatalf("expected peer-old to leave, got %s", leftData.PeerID)
	}
	joinedPeer, ok := findEnvelope(observerMessages, MsgPeerJoined)
	if !ok {
		t.Fatal("expected observer to receive peer-joined for replacement peer")
	}
	var joinedPeerData PeerJoinedData
	if err := json.Unmarshal(joinedPeer.Data, &joinedPeerData); err != nil {
		t.Fatalf("unmarshal peer-joined data: %v", err)
	}
	if joinedPeerData.PeerID != "peer-new" {
		t.Fatalf("expected peer-new to join, got %s", joinedPeerData.PeerID)
	}
}

func TestHubForwardStateTargetsOnePeer(t *testing.T) {
	hub := NewHub()
	alice := testClient("peer-alice")
	bob := testClient("peer-bob")
	carol := testClient("peer-carol")
	setTestClientState(alice, "Alice", "presence-alice")
	setTestClientState(bob, "Bob", "presence-bob")
	setTestClientState(carol, "Carol", "presence-carol")

	hub.join(alice, "room-1")
	hub.join(bob, "room-1")
	hub.join(carol, "room-1")
	drainEnvelopes(t, alice)
	drainEnvelopes(t, bob)
	drainEnvelopes(t, carol)

	hub.forwardState(alice, bob.id, PeerStateData{
		Audio: true, Video: true, VideoHeld: boolPtr(true),
	})

	bobMessages := drainEnvelopes(t, bob)
	state, ok := findEnvelope(bobMessages, MsgPeerState)
	if !ok {
		t.Fatal("expected target peer to receive peer-state")
	}
	if state.From != alice.id || state.To != bob.id {
		t.Fatalf("expected targeted state from %s to %s, got from=%s to=%s", alice.id, bob.id, state.From, state.To)
	}
	var stateData PeerStateData
	if err := json.Unmarshal(state.Data, &stateData); err != nil {
		t.Fatalf("unmarshal peer-state data: %v", err)
	}
	if stateData.VideoHeld == nil || !*stateData.VideoHeld {
		t.Fatal("expected targeted state to carry videoHeld=true")
	}
	if got := drainEnvelopes(t, carol); len(got) != 0 {
		t.Fatalf("expected non-target peer to receive no messages, got %d", len(got))
	}
	if alice.info().VideoHeld {
		t.Fatal("targeted videoHeld state should not mutate sender room presence")
	}
}

func TestHubBroadcastSfuTracks(t *testing.T) {
	hub := NewHub()
	publisher := testClient("peer-publisher")
	subscriber1 := testClient("peer-sub1")
	subscriber2 := testClient("peer-sub2")
	setTestClientState(publisher, "Alice", "presence-alice")
	setTestClientState(subscriber1, "Bob", "presence-bob")
	setTestClientState(subscriber2, "Carol", "presence-carol")

	hub.join(publisher, "room-1")
	hub.join(subscriber1, "room-1")
	hub.join(subscriber2, "room-1")
	drainEnvelopes(t, publisher)
	drainEnvelopes(t, subscriber1)
	drainEnvelopes(t, subscriber2)

	hub.BroadcastSfuTracks("room-1", publisher.id, SfuTracksData{
		SessionID: "cf-session-abc",
		Tracks: []SfuTrackInfo{
			{TrackName: "audio", Mid: "0"},
			{TrackName: "video", Mid: "1"},
		},
	})

	// Subscribers receive sfu-tracks; publisher does not.
	for _, sub := range []*Client{subscriber1, subscriber2} {
		msgs := drainEnvelopes(t, sub)
		env, ok := findEnvelope(msgs, MsgSfuTracks)
		if !ok {
			t.Fatalf("%s: expected sfu-tracks message", sub.id)
		}
		if env.From != publisher.id {
			t.Fatalf("%s: expected From=%s, got %s", sub.id, publisher.id, env.From)
		}
		var data SfuTracksData
		if err := json.Unmarshal(env.Data, &data); err != nil {
			t.Fatalf("%s: unmarshal sfu-tracks data: %v", sub.id, err)
		}
		if data.SessionID != "cf-session-abc" {
			t.Fatalf("%s: expected sessionId=cf-session-abc, got %q", sub.id, data.SessionID)
		}
		if len(data.Tracks) != 2 {
			t.Fatalf("%s: expected 2 tracks, got %d", sub.id, len(data.Tracks))
		}
	}

	if msgs := drainEnvelopes(t, publisher); len(msgs) != 0 {
		t.Fatalf("publisher should not receive its own sfu-tracks broadcast, got %d messages", len(msgs))
	}
}

func TestHubBroadcastSfuTracks_NoRoom(t *testing.T) {
	hub := NewHub()
	// BroadcastSfuTracks on a non-existent room must not panic.
	hub.BroadcastSfuTracks("nonexistent-room", "peer-1", SfuTracksData{
		SessionID: "sess",
		Tracks:    []SfuTrackInfo{{TrackName: "audio"}},
	})
}

// ──────────────────────────────────────────────────────────────────────────────
// Knock / admit lifecycle — the bug class previously shipped as
// "peer-joined-before-admit". A guest joining with NeedsAdmit=true must NOT
// appear in other peers' tile grids until knockAdmit clears the flag. Each
// test below pins one observable property of that lifecycle.
// ──────────────────────────────────────────────────────────────────────────────

// setTestClientStateAdmit configures a client with the needsAdmit flag set
// (i.e., an un-admitted guest knocking for entry).
func setTestClientStateAdmit(c *Client, name, presenceID string, needsAdmit bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.name = name
	c.audio = true
	c.video = true
	c.presenceID = presenceID
	c.needsAdmit = needsAdmit
}

// A knocking guest (needsAdmit=true) joining a room must NOT trigger a
// peer-joined broadcast. If this regresses, host UIs show ghost tiles for
// uninvited guests before the host has admitted them.
func TestHubJoin_KnockingGuestDoesNotBroadcastPeerJoined(t *testing.T) {
	hub := NewHub()
	host := testClient("peer-host")
	guest := testClient("peer-guest")
	setTestClientStateAdmit(host, "Host", "p-host", false)
	setTestClientStateAdmit(guest, "Guest", "p-guest", true) // knocking

	hub.join(host, "room-1")
	drainEnvelopes(t, host)

	hub.join(guest, "room-1")

	hostMessages := drainEnvelopes(t, host)
	if _, ok := findEnvelope(hostMessages, MsgPeerJoined); ok {
		t.Fatal("host received peer-joined for an un-admitted guest — admission lifecycle broken")
	}
}

// peerInfos must exclude knocking guests so a third peer joining after the
// guest doesn't see the guest in the "joined" peer list either. Tile grid
// invariant: only admitted peers count.
func TestHubJoin_NewJoinerDoesNotSeeKnockingGuestInPeerList(t *testing.T) {
	hub := NewHub()
	host := testClient("peer-host")
	guest := testClient("peer-guest")
	thirdJoiner := testClient("peer-third")
	setTestClientStateAdmit(host, "Host", "p-host", false)
	setTestClientStateAdmit(guest, "Guest", "p-guest", true)
	setTestClientStateAdmit(thirdJoiner, "Third", "p-third", false)

	hub.join(host, "room-1")
	hub.join(guest, "room-1")
	drainEnvelopes(t, host)
	drainEnvelopes(t, guest)

	hub.join(thirdJoiner, "room-1")
	msgs := drainEnvelopes(t, thirdJoiner)
	joined, ok := findEnvelope(msgs, MsgJoined)
	if !ok {
		t.Fatal("expected joined envelope")
	}
	var jd JoinedData
	if err := json.Unmarshal(joined.Data, &jd); err != nil {
		t.Fatalf("unmarshal joined: %v", err)
	}
	for _, p := range jd.Peers {
		if p.ID == guest.id {
			t.Fatal("knocking guest appeared in peer list before admission")
		}
	}
}

// knockAdmit must (1) deliver knock-granted with an SFU token to the target,
// (2) clear the target's needsAdmit flag, and (3) broadcast peer-joined to
// every other peer in the room. All three are part of one atomic action from
// the host's perspective — splitting them caused the "SFU race after
// setIsKnocking(false)" bug.
func TestHubKnockAdmit_DeliversTokenAndAnnouncesPeer(t *testing.T) {
	hub := NewHub()
	hub.SetGuestTokenFn(func(peerID, roomID string) (string, error) {
		return "guest-token-" + peerID + "-" + roomID, nil
	})

	host := testClient("peer-host")
	guest := testClient("peer-guest")
	bystander := testClient("peer-bystander")
	setTestClientStateAdmit(host, "Host", "p-host", false)
	setTestClientStateAdmit(guest, "Guest", "p-guest", true)
	setTestClientStateAdmit(bystander, "Bystander", "p-by", false)

	hub.join(host, "room-1")
	hub.join(bystander, "room-1")
	hub.join(guest, "room-1") // knocking, deferred
	drainEnvelopes(t, host)
	drainEnvelopes(t, bystander)
	drainEnvelopes(t, guest)

	hub.knockAdmit(host, guest.id)

	// (1) Guest received knock-granted with the token.
	guestMsgs := drainEnvelopes(t, guest)
	granted, ok := findEnvelope(guestMsgs, MsgKnockGranted)
	if !ok {
		t.Fatal("guest did not receive knock-granted")
	}
	var gd KnockGrantedData
	if err := json.Unmarshal(granted.Data, &gd); err != nil {
		t.Fatalf("unmarshal knock-granted: %v", err)
	}
	if gd.SfuToken != "guest-token-peer-guest-room-1" {
		t.Fatalf("expected SFU token to be issued for the admitted guest, got %q", gd.SfuToken)
	}

	// (2) The needsAdmit flag must be cleared, otherwise a subsequent joiner
	// would still skip the guest in its peer list — the original bug.
	guest.mu.RLock()
	stillKnocking := guest.needsAdmit
	guest.mu.RUnlock()
	if stillKnocking {
		t.Error("needsAdmit flag was not cleared after admission")
	}

	// (3) Bystander received peer-joined for the now-admitted guest.
	bMsgs := drainEnvelopes(t, bystander)
	announced, ok := findEnvelope(bMsgs, MsgPeerJoined)
	if !ok {
		t.Fatal("bystander did not receive peer-joined for the admitted guest")
	}
	var pd PeerJoinedData
	if err := json.Unmarshal(announced.Data, &pd); err != nil {
		t.Fatalf("unmarshal peer-joined: %v", err)
	}
	if pd.PeerID != guest.id {
		t.Fatalf("expected peer-joined for %s, got %s", guest.id, pd.PeerID)
	}
}

// A knocking guest in an otherwise empty room cannot self-admit. The hub must
// respond with KNOCK_NO_HOST instead of silently dropping the knock — the UI
// needs to surface "no one's here yet, try again".
func TestHubKnock_EmptyRoomReturnsKnockNoHost(t *testing.T) {
	hub := NewHub()
	guest := testClient("peer-guest")
	setTestClientStateAdmit(guest, "Guest", "p-guest", true)
	hub.join(guest, "room-1")
	drainEnvelopes(t, guest)

	hub.knock(guest)

	msgs := drainEnvelopes(t, guest)
	errEnv, ok := findEnvelope(msgs, MsgError)
	if !ok {
		t.Fatal("expected error envelope when knocking with no other peer present")
	}
	var ed ErrorData
	if err := json.Unmarshal(errEnv.Data, &ed); err != nil {
		t.Fatalf("unmarshal error data: %v", err)
	}
	if ed.Code != "KNOCK_NO_HOST" {
		t.Fatalf("expected code KNOCK_NO_HOST, got %q (msg=%q)", ed.Code, ed.Message)
	}
}

// A client that has not joined any room cannot knock. Must respond with
// NOT_IN_ROOM, not a crash and not silence.
func TestHubKnock_NotInRoomReturnsNotInRoom(t *testing.T) {
	hub := NewHub()
	guest := testClient("peer-guest")
	setTestClientStateAdmit(guest, "Guest", "p-guest", true)
	// Intentionally NOT joining a room.

	hub.knock(guest)

	msgs := drainEnvelopes(t, guest)
	errEnv, ok := findEnvelope(msgs, MsgError)
	if !ok {
		t.Fatal("expected error envelope for knock before join")
	}
	var ed ErrorData
	_ = json.Unmarshal(errEnv.Data, &ed)
	if ed.Code != "NOT_IN_ROOM" {
		t.Fatalf("expected NOT_IN_ROOM, got code=%q msg=%q", ed.Code, ed.Message)
	}
}

// If the host's knockAdmit names a peer that has since left, the target peer
// cannot receive the token. The hub must surface an error to the admitter so
// the UI can re-render, instead of silently dropping the admit.
func TestHubKnockAdmit_PeerNotFoundSendsError(t *testing.T) {
	hub := NewHub()
	hub.SetGuestTokenFn(func(peerID, roomID string) (string, error) { return "tok", nil })
	host := testClient("peer-host")
	setTestClientStateAdmit(host, "Host", "p-host", false)
	hub.join(host, "room-1")
	drainEnvelopes(t, host)

	hub.knockAdmit(host, "peer-ghost")

	msgs := drainEnvelopes(t, host)
	if _, ok := findEnvelope(msgs, MsgError); !ok {
		t.Fatal("expected error envelope when admitting a non-existent peer")
	}
}

// If guestTokenFn is not configured (a wiring error in cmd/server), knockAdmit
// must NOT silently no-op — it must error so the misconfiguration is visible.
func TestHubKnockAdmit_NoTokenFnSendsError(t *testing.T) {
	hub := NewHub() // no SetGuestTokenFn
	host := testClient("peer-host")
	guest := testClient("peer-guest")
	setTestClientStateAdmit(host, "Host", "p-host", false)
	setTestClientStateAdmit(guest, "Guest", "p-guest", true)
	hub.join(host, "room-1")
	hub.join(guest, "room-1")
	drainEnvelopes(t, host)

	hub.knockAdmit(host, guest.id)

	msgs := drainEnvelopes(t, host)
	if _, ok := findEnvelope(msgs, MsgError); !ok {
		t.Fatal("expected error envelope when guestTokenFn is not configured")
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Late-joiner SFU track replay — the previously-shipped bug class where a peer
// joining after another peer had already published tracks never received the
// existing tracks, so its decoder stayed idle and the tile rendered black.
// ──────────────────────────────────────────────────────────────────────────────

// When a peer joins, the hub must replay every existing peer's currently
// stored sfu-tracks so the late joiner can subscribe. Without this, a late
// joiner has no way to learn about already-published tracks.
func TestHubJoin_LateJoinerReceivesExistingSfuTracksReplay(t *testing.T) {
	hub := NewHub()
	publisher := testClient("peer-pub")
	lateJoiner := testClient("peer-late")
	setTestClientStateAdmit(publisher, "Pub", "p-pub", false)
	setTestClientStateAdmit(lateJoiner, "Late", "p-late", false)

	hub.join(publisher, "room-1")
	hub.BroadcastSfuTracks("room-1", publisher.id, SfuTracksData{
		SessionID: "cf-sess-pub",
		Tracks: []SfuTrackInfo{
			{TrackName: "audio", Mid: "0"},
			{TrackName: "video", Mid: "1"},
		},
	})
	drainEnvelopes(t, publisher)

	// Now the late joiner arrives — it must receive a sfu-tracks message
	// carrying the publisher's existing tracks during its join sequence.
	hub.join(lateJoiner, "room-1")
	msgs := drainEnvelopes(t, lateJoiner)

	tracks, ok := findEnvelope(msgs, MsgSfuTracks)
	if !ok {
		t.Fatal("late joiner did not receive sfu-tracks replay — would never subscribe to existing tracks")
	}
	if tracks.From != publisher.id {
		t.Errorf("expected sfu-tracks from publisher, got %q", tracks.From)
	}
	var data SfuTracksData
	if err := json.Unmarshal(tracks.Data, &data); err != nil {
		t.Fatalf("unmarshal sfu-tracks: %v", err)
	}
	if data.SessionID != "cf-sess-pub" {
		t.Errorf("expected sessionID cf-sess-pub, got %q", data.SessionID)
	}
	if len(data.Tracks) != 2 {
		t.Errorf("expected 2 tracks replayed, got %d", len(data.Tracks))
	}
}

// The room must MERGE sfu-tracks by trackName across multiple publishes. A peer
// that publishes audio first, then publishes video later, must end up with BOTH
// in the snapshot — otherwise the late-joiner replay only carries the latest
// publish (the original bug shape: peer subscribes only to video, audio lost).
func TestRoomStoreSfuTracks_MergesByTrackNameAcrossPublishes(t *testing.T) {
	hub := NewHub()
	publisher := testClient("peer-pub")
	setTestClientStateAdmit(publisher, "Pub", "p-pub", false)
	hub.join(publisher, "room-1")

	// First publish: audio only.
	hub.BroadcastSfuTracks("room-1", publisher.id, SfuTracksData{
		SessionID: "cf-sess-1",
		Tracks:    []SfuTrackInfo{{TrackName: "audio", Mid: "0"}},
	})
	// Later publish: video added.
	hub.BroadcastSfuTracks("room-1", publisher.id, SfuTracksData{
		SessionID: "cf-sess-1",
		Tracks:    []SfuTrackInfo{{TrackName: "video", Mid: "1"}},
	})

	room := hub.rooms["room-1"]
	snap := room.sfuTrackSnapshot()
	merged, ok := snap[publisher.id]
	if !ok {
		t.Fatal("publisher's tracks not stored")
	}
	if len(merged.Tracks) != 2 {
		t.Fatalf("expected merged audio+video (2 tracks), got %d: %+v", len(merged.Tracks), merged.Tracks)
	}
	names := map[string]bool{}
	for _, t := range merged.Tracks {
		names[t.TrackName] = true
	}
	if !names["audio"] || !names["video"] {
		t.Errorf("expected both audio and video after merge, got %v", names)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Room GC — when the last peer leaves, the room must be removed from the hub,
// onRoomEmpty must fire, and sfu-tracks state must be released. A leak here
// means orphaned rooms accumulate, which on the 1GB-RAM droplet becomes
// observable as RSS growth.
// ──────────────────────────────────────────────────────────────────────────────

func TestHubLeaveAll_LastPeerTriggersRoomGCAndCallback(t *testing.T) {
	hub := NewHub()

	var (
		emptyMu sync.Mutex
		emptied []string
	)
	hub.SetRoomEmptyHandler(func(roomID string) {
		emptyMu.Lock()
		emptied = append(emptied, roomID)
		emptyMu.Unlock()
	})

	c := testClient("peer-1")
	setTestClientStateAdmit(c, "C", "p-1", false)
	hub.join(c, "room-gc")

	if _, ok := hub.rooms["room-gc"]; !ok {
		t.Fatal("expected room to exist after join")
	}

	hub.leaveAll(c)

	if _, ok := hub.rooms["room-gc"]; ok {
		t.Error("room must be deleted from hub when the last peer leaves (memory leak risk)")
	}

	// Callback is fired in a goroutine; give it a moment.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		emptyMu.Lock()
		got := len(emptied)
		emptyMu.Unlock()
		if got >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	emptyMu.Lock()
	if len(emptied) != 1 || emptied[0] != "room-gc" {
		t.Errorf("expected onRoomEmpty(\"room-gc\") to fire exactly once, got %v", emptied)
	}
	emptyMu.Unlock()
}

// A peer that disconnects without an explicit leave must still have its
// sfu-tracks state cleaned up. Otherwise late joiners replay stale tracks for
// a peer that is no longer present.
func TestHubLeaveAll_ClearsSfuTracksEntry(t *testing.T) {
	hub := NewHub()
	publisher := testClient("peer-pub")
	observer := testClient("peer-obs")
	setTestClientStateAdmit(publisher, "Pub", "p-pub", false)
	setTestClientStateAdmit(observer, "Obs", "p-obs", false)

	hub.join(publisher, "room-1")
	hub.join(observer, "room-1") // keeps the room alive after publisher leaves
	hub.BroadcastSfuTracks("room-1", publisher.id, SfuTracksData{
		SessionID: "cf-sess-pub",
		Tracks:    []SfuTrackInfo{{TrackName: "audio"}},
	})

	room := hub.rooms["room-1"]
	if _, ok := room.sfuTrackSnapshot()[publisher.id]; !ok {
		t.Fatal("precondition: publisher's tracks should be stored before leave")
	}

	hub.leaveAll(publisher)

	if _, ok := room.sfuTrackSnapshot()[publisher.id]; ok {
		t.Error("publisher's sfu-tracks must be cleaned up on leave to prevent stale-track replay")
	}
}
