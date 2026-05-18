package signaling

import (
	"encoding/json"
	"testing"
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
