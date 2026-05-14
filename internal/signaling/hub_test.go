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
