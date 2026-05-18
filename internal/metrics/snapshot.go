package metrics

import (
	"math"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/vaishnavghenge/vartalaap-server/internal/quality"
)

type HistogramSummary struct {
	Count float64 `json:"count"`
	Sum   float64 `json:"sum"`
	P50   float64 `json:"p50"`
	P95   float64 `json:"p95"`
	P99   float64 `json:"p99"`
}

type CallAttemptsSummary struct {
	Success   float64 `json:"success"`
	Timeout   float64 `json:"timeout"`
	Error     float64 `json:"error"`
	Abandoned float64 `json:"abandoned"`
	// SuccessRatePct is success/(success+timeout+error+abandoned) × 100,
	// or 0 if no attempts have been recorded. SLO target ≥ 99.5%.
	SuccessRatePct float64 `json:"success_rate_pct"`
}

type Snapshot struct {
	ActivePeers      float64           `json:"active_peers"`
	ActiveRooms      float64           `json:"active_rooms"`
	JoinsTotal       float64           `json:"joins_total"`
	IceRequestsTotal float64           `json:"ice_requests_total"`
	IceErrorsTotal   float64           `json:"ice_errors_total"`
	SignalsOffer     float64           `json:"signals_offer"`
	SignalsAnswer    float64           `json:"signals_answer"`
	SignalsCandidate float64           `json:"signals_candidate"`
	Quality          quality.Aggregate `json:"quality"`

	// Time-to-first-media SLO. Seconds. Target: p95 ≤ 3, p99 ≤ 6, fail > 10.
	TTFM HistogramSummary `json:"ttfm_seconds"`
	// Server-side HTTP request latency across all routes. Seconds.
	HTTP HistogramSummary `json:"http_seconds"`
	// Call setup outcome counters + derived success rate.
	CallAttempts CallAttemptsSummary `json:"call_attempts"`
}

func Gather() Snapshot {
	mfs, _ := prometheus.DefaultGatherer.Gather()
	raw := make(map[string]float64)
	var ttfmHist, httpHist *dto.Histogram
	for _, mf := range mfs {
		name := mf.GetName()
		if !strings.HasPrefix(name, "vartalaap_") {
			continue
		}
		for _, m := range mf.GetMetric() {
			key := name
			for _, lp := range m.GetLabel() {
				key += ":" + lp.GetValue()
			}
			switch mf.GetType() {
			case dto.MetricType_GAUGE:
				raw[key] = m.GetGauge().GetValue()
			case dto.MetricType_COUNTER:
				raw[key] = m.GetCounter().GetValue()
			case dto.MetricType_HISTOGRAM:
				switch name {
				case "vartalaap_time_to_first_media_seconds":
					ttfmHist = m.GetHistogram()
				case "vartalaap_http_request_duration_seconds":
					// Aggregate across all label combinations (route+method+status).
					if httpHist == nil {
						httpHist = m.GetHistogram()
					} else {
						httpHist = mergeHistograms(httpHist, m.GetHistogram())
					}
				}
			}
		}
	}

	attempts := CallAttemptsSummary{
		Success:   raw["vartalaap_call_attempts_total:success"],
		Timeout:   raw["vartalaap_call_attempts_total:timeout"],
		Error:     raw["vartalaap_call_attempts_total:error"],
		Abandoned: raw["vartalaap_call_attempts_total:abandoned"],
	}
	total := attempts.Success + attempts.Timeout + attempts.Error + attempts.Abandoned
	if total > 0 {
		attempts.SuccessRatePct = 100 * attempts.Success / total
	}

	return Snapshot{
		ActivePeers:      raw["vartalaap_active_peers"],
		ActiveRooms:      raw["vartalaap_active_rooms"],
		JoinsTotal:       raw["vartalaap_joins_total"],
		IceRequestsTotal: raw["vartalaap_ice_requests_total"],
		IceErrorsTotal:   raw["vartalaap_ice_errors_total"],
		SignalsOffer:     raw["vartalaap_signals_total:offer"],
		SignalsAnswer:    raw["vartalaap_signals_total:answer"],
		SignalsCandidate: raw["vartalaap_signals_total:candidate"],
		Quality:          quality.Default.Aggregate(),
		TTFM:             summarizeHistogram(ttfmHist),
		HTTP:             summarizeHistogram(httpHist),
		CallAttempts:     attempts,
	}
}

// mergeHistograms adds the bucket counts and sum/count of b into a, leaving a
// usable for further merges. Returns the merged copy so callers can reassign.
// Both inputs are assumed to share the same bucket boundaries — which is true
// because the same registered Histogram defines them.
func mergeHistograms(a, b *dto.Histogram) *dto.Histogram {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	merged := &dto.Histogram{
		SampleCount: ptrU64(a.GetSampleCount() + b.GetSampleCount()),
		SampleSum:   ptrF64(a.GetSampleSum() + b.GetSampleSum()),
	}
	aBuckets := a.GetBucket()
	bBuckets := b.GetBucket()
	if len(aBuckets) != len(bBuckets) {
		// Different bucket layouts — should never happen for a single registered
		// HistogramVec. Fall back to a alone rather than producing garbage.
		return a
	}
	merged.Bucket = make([]*dto.Bucket, len(aBuckets))
	for i := range aBuckets {
		merged.Bucket[i] = &dto.Bucket{
			UpperBound:      aBuckets[i].UpperBound,
			CumulativeCount: ptrU64(aBuckets[i].GetCumulativeCount() + bBuckets[i].GetCumulativeCount()),
		}
	}
	return merged
}

func ptrU64(v uint64) *uint64    { return &v }
func ptrF64(v float64) *float64  { return &v }

// summarizeHistogram derives p50/p95/p99 by linear interpolation across
// cumulative bucket counts. This is the same method `histogram_quantile`
// uses in PromQL — sufficient for a dashboard, not a billing system.
//
// Returns zeros if no observations have been recorded.
func summarizeHistogram(h *dto.Histogram) HistogramSummary {
	if h == nil || h.GetSampleCount() == 0 {
		return HistogramSummary{}
	}
	return HistogramSummary{
		Count: float64(h.GetSampleCount()),
		Sum:   h.GetSampleSum(),
		P50:   quantile(h, 0.50),
		P95:   quantile(h, 0.95),
		P99:   quantile(h, 0.99),
	}
}

func quantile(h *dto.Histogram, q float64) float64 {
	buckets := h.GetBucket()
	if len(buckets) == 0 {
		return 0
	}
	total := float64(h.GetSampleCount())
	if total == 0 {
		return 0
	}
	target := q * total

	prevCount := 0.0
	prevBound := 0.0
	for _, b := range buckets {
		count := float64(b.GetCumulativeCount())
		bound := b.GetUpperBound()
		if math.IsInf(bound, +1) {
			// +Inf bucket — fall back to previous bound's count.
			if prevCount >= target {
				return prevBound
			}
			return prevBound
		}
		if count >= target {
			// Linear interpolation within this bucket.
			bucketCount := count - prevCount
			if bucketCount <= 0 {
				return bound
			}
			frac := (target - prevCount) / bucketCount
			return prevBound + frac*(bound-prevBound)
		}
		prevCount = count
		prevBound = bound
	}
	return prevBound
}
