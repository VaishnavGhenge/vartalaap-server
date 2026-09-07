package metrics

import (
	"math"

	"github.com/prometheus/client_golang/prometheus"
)

var (
	MediaRTT       = prometheus.NewHistogram(prometheus.HistogramOpts{Name: "vartalaap_media_rtt_seconds", Help: "Browser SFU RTT samples; stream-weighted, not a per-call percentile.", Buckets: []float64{.025, .05, .1, .2, .3, .5, 1, 2, 5}})
	MediaLoss      = prometheus.NewHistogram(prometheus.HistogramOpts{Name: "vartalaap_media_packet_loss_percent", Help: "Browser remote-stream packet loss samples in percent.", Buckets: []float64{0, .1, .5, 1, 2, 5, 10, 25, 50, 100}})
	MediaJitter    = prometheus.NewHistogram(prometheus.HistogramOpts{Name: "vartalaap_media_jitter_seconds", Help: "Browser remote-stream jitter samples.", Buckets: []float64{.005, .01, .02, .03, .05, .1, .2, .5, 1}})
	MediaVideoHeld = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "vartalaap_media_video_held_samples_total", Help: "Stream-report samples with local video intentionally held by adaptive quality; not freeze events."}, []string{"held"})
)

func init() {
	prometheus.MustRegister(MediaRTT, MediaLoss, MediaJitter, MediaVideoHeld)
	for _, result := range []string{"success", "slow", "failed", "error", "abandoned"} {
		CallAttempts.WithLabelValues(result)
	}
	MediaVideoHeld.WithLabelValues("true")
	MediaVideoHeld.WithLabelValues("false")
}

func ObserveMedia(rttMs, lossPercent, jitterMs float64, held bool) {
	observeBounded(MediaRTT, rttMs/1000, 60)
	observeBounded(MediaLoss, lossPercent, 100)
	observeBounded(MediaJitter, jitterMs/1000, 60)
	label := "false"
	if held {
		label = "true"
	}
	MediaVideoHeld.WithLabelValues(label).Inc()
}

func observeBounded(observer prometheus.Observer, value, max float64) {
	if !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0 && value <= max {
		observer.Observe(value)
	}
}
