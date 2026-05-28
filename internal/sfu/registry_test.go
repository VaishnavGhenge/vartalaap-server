package sfu_test

import (
	"fmt"
	"sync"
	"testing"

	"github.com/vaishnavghenge/vartalaap-server/internal/sfu"
)

// The SFU registry tracks (sessionID → userID/roomID/peerID) for every active
// Cloudflare Realtime session. It backs two security-relevant decisions:
//
//  1. /sfu/sessions/{id}/... routes look up `ownerID` and reject the request
//     when ownerID != current userID. A leaky registry would let user-A
//     operate on user-B's session.
//  2. The room-scope guard reads `roomID` to compare against the URL roomId.
//     A stale entry could route a guest into the wrong room.
//
// Both invariants ride on the registry being correct under reconnect and
// concurrency. Those are exactly the cases tested here.

func TestRegistry_Register_LookupRoundtrip(t *testing.T) {
	r := sfu.NewRegistry()
	r.Register("sess-1", "user-1", "room-1", "peer-alice")

	userID, roomID, peerID, ok := r.Lookup("sess-1")
	if !ok {
		t.Fatal("expected ok=true for registered session")
	}
	if userID != "user-1" || roomID != "room-1" || peerID != "peer-alice" {
		t.Errorf("got userID=%q roomID=%q peerID=%q, want user-1/room-1/peer-alice",
			userID, roomID, peerID)
	}
}

func TestRegistry_Lookup_UnknownSessionReturnsFalse(t *testing.T) {
	r := sfu.NewRegistry()
	_, _, _, ok := r.Lookup("never-registered")
	if ok {
		t.Fatal("expected ok=false for unknown session")
	}
}

func TestRegistry_Remove_ClearsBothMaps(t *testing.T) {
	r := sfu.NewRegistry()
	r.Register("sess-1", "user-1", "room-1", "peer-alice")
	r.Remove("sess-1")

	if _, _, _, ok := r.Lookup("sess-1"); ok {
		t.Error("session entry remains in sessions map after Remove")
	}
	// Re-registering the same user with a NEW session must succeed AND become
	// the active mapping. If the user→session map weren't cleared, this would
	// look healthy but the user→latest-session lookup (if added later) could
	// still point at sess-1 forever.
	r.Register("sess-2", "user-1", "room-1", "peer-alice")
	if _, _, _, ok := r.Lookup("sess-2"); !ok {
		t.Error("expected sess-2 to be registered after Remove(sess-1) + Register(sess-2)")
	}
}

func TestRegistry_Remove_UnknownSessionIsNoop(t *testing.T) {
	r := sfu.NewRegistry()
	// Should not panic and should not affect anything.
	r.Remove("never-existed")
	r.Register("sess-1", "user-1", "room-1", "p")
	r.Remove("never-existed-2")
	if _, _, _, ok := r.Lookup("sess-1"); !ok {
		t.Fatal("unrelated session must remain after no-op Remove")
	}
}

// Reconnect: the same user registers a second session before the first is
// torn down (network flap, browser refresh). Pins the observable property that
// Removing the OLD sessionID does not also remove the NEW one from the
// sessions map. A naive implementation that, on Remove, iterated and cleared
// every entry sharing the userID would fail this test and break reconnects.
//
// Note: the internal `users` map and the `r.users[e.userID] == sessionID`
// guard in Remove are also part of the design (they prevent a stale Remove
// from wiping the live pointer), but `users` is unexported and not read
// anywhere outside the package today, so its effect is not externally
// observable. If a future feature exposes "current session for user X",
// extend this test to assert that path stays pointed at sess-new.
func TestRegistry_Reconnect_RemoveOldSessionDoesNotAffectNew(t *testing.T) {
	r := sfu.NewRegistry()

	r.Register("sess-old", "user-1", "room-1", "peer-alice")
	r.Register("sess-new", "user-1", "room-1", "peer-alice")

	if _, _, _, ok := r.Lookup("sess-old"); !ok {
		t.Fatal("sess-old must still be registered before Remove")
	}
	if _, _, _, ok := r.Lookup("sess-new"); !ok {
		t.Fatal("sess-new must be registered")
	}

	r.Remove("sess-old")

	if _, _, _, ok := r.Lookup("sess-new"); !ok {
		t.Fatal("Remove(sess-old) also removed sess-new — reconnect broken")
	}
	if _, _, _, ok := r.Lookup("sess-old"); ok {
		t.Error("sess-old still resolvable after Remove")
	}
}

// The mutex contract: Register, Lookup, and Remove must be safe to call
// concurrently. Run under `go test -race` to catch any access not held under
// r.mu. This intentionally has no assertions — the race detector is the
// assertion. A flaky data race here would fail under -race only.
func TestRegistry_ConcurrentAccessIsRaceFree(t *testing.T) {
	r := sfu.NewRegistry()
	const N = 50
	var wg sync.WaitGroup
	wg.Add(N * 3)

	for i := 0; i < N; i++ {
		id := fmt.Sprintf("sess-%d", i)
		user := fmt.Sprintf("user-%d", i%5) // intentional collision: multiple sessions per user
		go func() { defer wg.Done(); r.Register(id, user, "room-1", "peer") }()
		go func() { defer wg.Done(); _, _, _, _ = r.Lookup(id) }()
		go func() { defer wg.Done(); r.Remove(id) }()
	}
	wg.Wait()
}
