package metrics

import (
	"math"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

func TestObserveBoundedRejectsUnknownAndInvalidSamples(t *testing.T) {
	h := prometheus.NewHistogram(prometheus.HistogramOpts{Name: "test_sample", Help: "test"})
	for _, v := range []float64{-1, math.NaN(), math.Inf(1), math.Inf(-1), 101} {
		observeBounded(h, v, 100)
	}
	observeBounded(h, 0, 100)
	observeBounded(h, 100, 100)
	r := prometheus.NewRegistry()
	r.MustRegister(h)
	m, err := r.Gather()
	if err != nil || m[0].Metric[0].Histogram.GetSampleCount() != 2 {
		t.Fatalf("invalid samples entered histogram: %v, %v", m, err)
	}
}
