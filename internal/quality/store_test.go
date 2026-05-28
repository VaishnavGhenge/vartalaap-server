package quality_test

import (
	"sync"
	"testing"

	"github.com/vaishnavghenge/vartalaap-server/internal/quality"
)

// The quality store backs the /stats snapshot endpoint that on-call uses to
// read live call health. Each test below pins one observable property of the
// aggregation function; silent drift in any of these (e.g. negative RTTs no
// longer skipped) makes the on-call dashboard lie.

// Use a fresh Store per test so the package-level `Default` singleton doesn't
// leak state across tests.
func newStore() *quality.Store {
	return quality.NewStore()
}

func newReport(peerID, qual string, rtt, loss float64) quality.PeerReport {
	return quality.PeerReport{
		PeerID:            peerID,
		Quality:           qual,
		RoundTripTimeMs:   rtt,
		PacketLossPercent: loss,
	}
}

// Set/Aggregate roundtrip: one report counts as one peer in the matching
// bucket. Establishes the positive case so misclassification tests below
// have a baseline.
func TestSet_BasicCountsOneInBucket(t *testing.T) {
	s := newStore()
	s.Set(newReport("peer-1", "good", 50, 0.1))
	agg := s.Aggregate()

	if agg.PeersGood != 1 || agg.PeersMedium != 0 || agg.PeersPoor != 0 {
		t.Errorf("expected 1 good 0 medium 0 poor, got %+v", agg)
	}
}

// Delete must remove a peer from the aggregate. Without this, a peer that
// disconnects continues showing up in the stats snapshot, polluting the
// SLO dashboard with stale data.
func TestDelete_RemovesPeerFromAggregate(t *testing.T) {
	s := newStore()
	s.Set(newReport("peer-1", "good", 50, 0))
	s.Set(newReport("peer-2", "poor", 800, 5))

	s.Delete("peer-1")
	agg := s.Aggregate()

	if agg.PeersGood != 0 {
		t.Errorf("deleted peer still counted: PeersGood=%d", agg.PeersGood)
	}
	if agg.PeersPoor != 1 {
		t.Errorf("survivor lost: expected PeersPoor=1, got %d", agg.PeersPoor)
	}
}

// Delete of an unknown peerID must be a no-op (not a panic, not a side-effect
// on other peers). Disconnect handlers may call Delete on a peer that never
// sent a stats-report.
func TestDelete_UnknownPeerIsNoop(t *testing.T) {
	s := newStore()
	s.Set(newReport("peer-1", "good", 50, 0))
	s.Delete("never-existed")

	if got := s.Aggregate().PeersGood; got != 1 {
		t.Errorf("unrelated peer affected by no-op Delete: PeersGood=%d", got)
	}
}

// Aggregate on an empty store returns zero values for everything, including
// the averages (no divide-by-zero). Empty rooms are common — e.g. between
// calls — and a panic here would crash the metrics endpoint.
func TestAggregate_EmptyStoreReturnsZeros(t *testing.T) {
	s := newStore()
	agg := s.Aggregate()

	if agg.PeersGood != 0 || agg.AvgRttMs != 0 || agg.AvgLossPct != 0 || agg.RelayCount != 0 {
		t.Errorf("empty store aggregate non-zero: %+v", agg)
	}
}

// THE intent-bearing rule: negative RTTs are excluded from the average. The
// client emits -1 (or similar sentinel) when getStats() couldn't read the
// roundTripTime — counting that as 0ms would pull the average toward zero
// and make a degraded call look healthy on the dashboard.
func TestAggregate_NegativeRTTValuesSkippedFromAverage(t *testing.T) {
	s := newStore()
	s.Set(newReport("good", "good", 100, 0))
	s.Set(newReport("missing", "good", -1, 0))  // sentinel for "no data"
	s.Set(newReport("also-good", "good", 200, 0))

	agg := s.Aggregate()
	want := (100.0 + 200.0) / 2.0 // skip -1, average of the two valid values
	if agg.AvgRttMs != want {
		t.Errorf("AvgRttMs = %.2f, want %.2f (negative RTT must be skipped)", agg.AvgRttMs, want)
	}
}

// Loss policy intentionally differs from RTT: loss is averaged across ALL
// reports, not just non-negative ones. A peer reporting 0% loss IS valid
// data; skipping it would inflate the apparent average. Pinned here so the
// asymmetry isn't accidentally "fixed" to match RTT.
func TestAggregate_LossAveragedAcrossAllPeers(t *testing.T) {
	s := newStore()
	s.Set(newReport("p1", "good", 50, 0))
	s.Set(newReport("p2", "medium", 50, 4))
	s.Set(newReport("p3", "poor", 50, 8))

	agg := s.Aggregate()
	want := (0.0 + 4.0 + 8.0) / 3.0
	if agg.AvgLossPct != want {
		t.Errorf("AvgLossPct = %.4f, want %.4f", agg.AvgLossPct, want)
	}
}

// Quality strings outside {good, medium, poor} are NOT counted anywhere.
// Defensive — the wire format could grow new values (e.g. "unknown") and
// we want them to silently not register rather than miscount as one of
// the known buckets.
func TestAggregate_UnknownQualityValueNotBucketed(t *testing.T) {
	s := newStore()
	s.Set(newReport("p1", "good", 50, 0))
	s.Set(newReport("p2", "", 50, 0))           // empty string
	s.Set(newReport("p3", "excellent", 50, 0))  // hypothetical future value

	agg := s.Aggregate()
	if agg.PeersGood != 1 || agg.PeersMedium != 0 || agg.PeersPoor != 0 {
		t.Errorf("unknown quality leaked into a bucket: %+v", agg)
	}
}

// RelayCount = peers using a TURN relay. Used by the on-call dashboard to
// detect ICE-direct failure mode (when most peers are forced to relay,
// something is wrong with the NAT path).
func TestAggregate_RelayCountReflectsCandidateType(t *testing.T) {
	s := newStore()
	r := newReport("p1", "good", 50, 0)
	r.CandidateType = "relay"
	s.Set(r)

	r2 := newReport("p2", "good", 50, 0)
	r2.CandidateType = "host"
	s.Set(r2)

	r3 := newReport("p3", "good", 50, 0)
	r3.CandidateType = "relay"
	s.Set(r3)

	agg := s.Aggregate()
	if agg.RelayCount != 2 {
		t.Errorf("RelayCount = %d, want 2", agg.RelayCount)
	}
}

// VideoHeld counter — number of peers currently in audio-only / video-held
// state. Used for cost forecasting: held tracks still consume CF subscribe
// minutes but at reduced bitrate.
func TestAggregate_VideoHeldCount(t *testing.T) {
	s := newStore()
	r1 := newReport("p1", "good", 50, 0)
	r1.VideoHeld = true
	s.Set(r1)
	r2 := newReport("p2", "good", 50, 0)
	r2.VideoHeld = false
	s.Set(r2)

	agg := s.Aggregate()
	if agg.VideoHeld != 1 {
		t.Errorf("VideoHeld = %d, want 1", agg.VideoHeld)
	}
}

// Resetting the same peer ID overwrites the prior report (it's a map, not
// a list). Otherwise a chatty client would multiply-count itself.
func TestSet_SamePeerIDOverwritesPriorReport(t *testing.T) {
	s := newStore()
	s.Set(newReport("p1", "good", 50, 0))
	s.Set(newReport("p1", "poor", 800, 5))  // same peer, fresh report

	agg := s.Aggregate()
	if agg.PeersGood != 0 {
		t.Errorf("stale 'good' lingered after fresh 'poor' report: PeersGood=%d", agg.PeersGood)
	}
	if agg.PeersPoor != 1 {
		t.Errorf("fresh 'poor' not counted: PeersPoor=%d", agg.PeersPoor)
	}
}

// The Store is shared across the signaling read pump (one goroutine per
// peer) and the HTTP /stats handler. Mutex correctness is invisible from
// behavior alone — the race detector is the assertion. No explicit checks.
func TestStore_ConcurrentSetAggregateDeleteIsRaceFree(t *testing.T) {
	s := newStore()
	const N = 100
	var wg sync.WaitGroup
	wg.Add(N * 3)
	for i := 0; i < N; i++ {
		id := "p" + string(rune('a'+i%26))
		go func() { defer wg.Done(); s.Set(newReport(id, "good", 50, 0)) }()
		go func() { defer wg.Done(); _ = s.Aggregate() }()
		go func() { defer wg.Done(); s.Delete(id) }()
	}
	wg.Wait()
}
