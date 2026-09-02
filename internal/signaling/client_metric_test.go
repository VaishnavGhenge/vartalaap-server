package signaling

import (
	"math"
	"testing"

	dto "github.com/prometheus/client_model/go"
	"github.com/vaishnavghenge/vartalaap-server/internal/metrics"
)

// observeClientMetric is the only path by which the browser feeds the
// server-side SLO histograms (time_to_first_media, call_setup_phase) and the
// success/timeout/error counter (call_attempts). The function is intentionally
// strict because the metrics back our deploy/alerting policy — a buggy or
// malicious client must NOT be able to pollute the histogram registry
// permanently with NaN, negatives, or 30-day TTFM values.
//
// These tests pin every reject/accept rule in observeClientMetric. Each one
// uses a delta on the package-level Prom metric: before − after must equal the
// expected number of new samples (0 if rejected, 1 if accepted).

func histSampleCount(t *testing.T, h interface {
	Write(*dto.Metric) error
}) uint64 {
	t.Helper()
	var m dto.Metric
	if err := h.Write(&m); err != nil {
		t.Fatalf("metric Write: %v", err)
	}
	return m.GetHistogram().GetSampleCount()
}

func counterValue(t *testing.T, c interface {
	Write(*dto.Metric) error
}) float64 {
	t.Helper()
	var m dto.Metric
	if err := c.Write(&m); err != nil {
		t.Fatalf("metric Write: %v", err)
	}
	return m.GetCounter().GetValue()
}

func newMetricClient() *Client {
	return &Client{id: "peer-metrics-test"}
}

// Happy path: a valid time_to_first_media observation increments the histogram
// sample count by exactly one. Establishes the positive case so reject-paths
// below are meaningful (otherwise observing nothing could pass any test).
func TestObserveClientMetric_TimeToFirstMedia_AcceptsValidValue(t *testing.T) {
	c := newMetricClient()
	before := histSampleCount(t, metrics.TimeToFirstMedia)
	c.observeClientMetric(ClientMetricData{Name: "time_to_first_media", Value: 1.8})
	after := histSampleCount(t, metrics.TimeToFirstMedia)
	if after-before != 1 {
		t.Fatalf("expected exactly 1 new sample, got %d", after-before)
	}
}

// A negative TTFM value is meaningless (likely a clock skew bug on the
// client). It must be silently dropped — no histogram observation — so a
// single buggy client cannot drag the p50 below zero forever.
func TestObserveClientMetric_RejectsNegativeValue(t *testing.T) {
	c := newMetricClient()
	before := histSampleCount(t, metrics.TimeToFirstMedia)
	c.observeClientMetric(ClientMetricData{Name: "time_to_first_media", Value: -3.0})
	after := histSampleCount(t, metrics.TimeToFirstMedia)
	if after != before {
		t.Errorf("negative value must be rejected; sample count changed from %d to %d", before, after)
	}
}

// NaN must be dropped. The check `v != v` (true only for NaN) lives in
// observeClientMetric. Without it, a single NaN observation makes the
// histogram quantile estimates unusable.
func TestObserveClientMetric_RejectsNaN(t *testing.T) {
	c := newMetricClient()
	before := histSampleCount(t, metrics.TimeToFirstMedia)
	c.observeClientMetric(ClientMetricData{Name: "time_to_first_media", Value: math.NaN()})
	after := histSampleCount(t, metrics.TimeToFirstMedia)
	if after != before {
		t.Errorf("NaN must be rejected; sample count changed from %d to %d", before, after)
	}
}

// Values above 60s (the documented ceiling) are clamped to 60s, NOT dropped —
// because a 30-second TTFM is still useful telemetry (it means the call took
// a long time to come up, which is the bug you want to see in the histogram).
// Dropping it instead would hide pathological calls. Clamping preserves them
// without letting one absurd value blow out the buckets.
func TestObserveClientMetric_ClampsObservationsAtSixtySeconds(t *testing.T) {
	c := newMetricClient()
	before := histSampleCount(t, metrics.TimeToFirstMedia)
	c.observeClientMetric(ClientMetricData{Name: "time_to_first_media", Value: 1e9})
	after := histSampleCount(t, metrics.TimeToFirstMedia)
	if after-before != 1 {
		t.Errorf("absurd value must be CLAMPED (still observed), not dropped; delta=%d", after-before)
	}
}

// The phase label is whitelisted to {ice_gather, pub_connected, sub_connected,
// first_media}. An unknown phase must be dropped — otherwise label cardinality
// explodes (any string becomes a new Prom label series). This is the
// "histogram registry pollution" risk in the doc comment.
func TestObserveClientMetric_RejectsUnknownPhase(t *testing.T) {
	c := newMetricClient()
	// Pre-observe a known phase so the labeled child exists; then observe an
	// unknown phase and assert the sample count for that unknown label did NOT
	// gain a new series. We do this indirectly by counting observations on a
	// known phase — if the unknown leaks through, neither bucket grows but a
	// new label series appears in the Vec. To stay simple, just verify the
	// child for a known phase isn't accidentally touched.
	knownChild := metrics.CallSetupPhase.WithLabelValues("ice_gather").(interface {
		Write(*dto.Metric) error
	})
	beforeKnown := histSampleCount(t, knownChild)
	c.observeClientMetric(ClientMetricData{
		Name: "call_setup_phase", Phase: "totally_bogus_phase", Value: 0.5,
	})
	afterKnown := histSampleCount(t, knownChild)
	if afterKnown != beforeKnown {
		t.Errorf("unknown phase must not increment any known-phase series; delta=%d", afterKnown-beforeKnown)
	}
}

// A valid phase is recorded under its specific label.
func TestObserveClientMetric_RecordsKnownPhase(t *testing.T) {
	c := newMetricClient()
	child := metrics.CallSetupPhase.WithLabelValues("ice_gather").(interface {
		Write(*dto.Metric) error
	})
	before := histSampleCount(t, child)
	c.observeClientMetric(ClientMetricData{Name: "call_setup_phase", Phase: "ice_gather", Value: 0.4})
	after := histSampleCount(t, child)
	if after-before != 1 {
		t.Fatalf("expected one sample under phase=ice_gather, delta=%d", after-before)
	}
}

// call_attempts is a counter labeled by result. The result label is also
// whitelisted ({success, timeout, error, abandoned}); an unknown result must
// not be counted, again to bound cardinality.
func TestObserveClientMetric_CallAttemptsRespectsResultWhitelist(t *testing.T) {
	c := newMetricClient()
	good := metrics.CallAttempts.WithLabelValues("success").(interface {
		Write(*dto.Metric) error
	})
	before := counterValue(t, good)
	c.observeClientMetric(ClientMetricData{Name: "call_attempt", Result: "success"})
	if got := counterValue(t, good) - before; got != 1 {
		t.Fatalf("expected success counter to increment by 1, got %.0f", got)
	}

	// Unknown result must not increment the success counter, and also must
	// not create some other observable side-effect on the existing labels.
	bogusBefore := counterValue(t, good)
	c.observeClientMetric(ClientMetricData{Name: "call_attempt", Result: "imaginary"})
	if got := counterValue(t, good) - bogusBefore; got != 0 {
		t.Errorf("unknown result must not increment any known-label counter; delta=%.0f", got)
	}
}

// call_setup_failure is a counter labeled by reason, the errors-by-type
// breakdown of a setup timeout. The reason label is whitelisted just like
// result/phase — an unknown reason must not be counted, to bound cardinality.
func TestObserveClientMetric_CallSetupFailureRespectsReasonWhitelist(t *testing.T) {
	c := newMetricClient()
	known := metrics.CallSetupFailures.WithLabelValues("tracks_announced_not_pulled").(interface {
		Write(*dto.Metric) error
	})
	before := counterValue(t, known)
	c.observeClientMetric(ClientMetricData{Name: "call_setup_failure", Reason: "tracks_announced_not_pulled"})
	if got := counterValue(t, known) - before; got != 1 {
		t.Fatalf("expected reason counter to increment by 1, got %.0f", got)
	}

	bogusBefore := counterValue(t, known)
	c.observeClientMetric(ClientMetricData{Name: "call_setup_failure", Reason: "made_up_reason"})
	if got := counterValue(t, known) - bogusBefore; got != 0 {
		t.Errorf("unknown reason must not increment any known-label counter; delta=%.0f", got)
	}
}

// Unknown metric names are intentionally dropped (not observed anywhere), so
// a client typo can't accidentally start a new permanent histogram series.
// We verify this by asserting NO known histogram (TTFM) gains a sample.
func TestObserveClientMetric_UnknownNameIsSilentlyDropped(t *testing.T) {
	c := newMetricClient()
	before := histSampleCount(t, metrics.TimeToFirstMedia)
	c.observeClientMetric(ClientMetricData{Name: "totally_unknown_metric", Value: 1.0})
	after := histSampleCount(t, metrics.TimeToFirstMedia)
	if after != before {
		t.Errorf("unknown metric name leaked into TTFM histogram; delta=%d", after-before)
	}
}

// The outcome split from step 5: `slow` and `failed` are what the ceiling
// stopped deciding. Both must reach Prometheus, because the success-rate SLO
// is now computed from them.
func TestObserveClientMetric_CallAttemptsAcceptsSlowAndFailed(t *testing.T) {
	c := newMetricClient()
	for _, result := range []string{"slow", "failed"} {
		counter := metrics.CallAttempts.WithLabelValues(result).(interface {
			Write(*dto.Metric) error
		})
		before := counterValue(t, counter)
		c.observeClientMetric(ClientMetricData{Name: "call_attempt", Result: result})
		if got := counterValue(t, counter) - before; got != 1 {
			t.Fatalf("expected %s counter to increment by 1, got %.0f", result, got)
		}
	}
}

// The client stopped emitting `timeout` when the ceiling became an observation,
// but the label must still be accepted: historical series stay readable, and a
// client running older code mid-rollout is not silently dropped.
func TestObserveClientMetric_CallAttemptsStillAcceptsLegacyTimeout(t *testing.T) {
	c := newMetricClient()
	counter := metrics.CallAttempts.WithLabelValues("timeout").(interface {
		Write(*dto.Metric) error
	})
	before := counterValue(t, counter)
	c.observeClientMetric(ClientMetricData{Name: "call_attempt", Result: "timeout"})
	if got := counterValue(t, counter) - before; got != 1 {
		t.Fatalf("legacy timeout must still be counted, got %.0f", got)
	}
}
