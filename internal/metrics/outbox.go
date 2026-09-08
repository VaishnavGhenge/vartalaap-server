package metrics

import "github.com/prometheus/client_golang/prometheus"

var (
	OutboxPending  = prometheus.NewGauge(prometheus.GaugeOpts{Name: "vartalaap_outbox_pending", Help: "Unfinished booking notification jobs in PostgreSQL."})
	OutboxOldest   = prometheus.NewGauge(prometheus.GaugeOpts{Name: "vartalaap_outbox_oldest_seconds", Help: "Age of the oldest unfinished notification."})
	OutboxActive   = prometheus.NewGauge(prometheus.GaugeOpts{Name: "vartalaap_outbox_active", Help: "Currently executing notification jobs on this instance."})
	OutboxAttempts = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "vartalaap_outbox_attempts_total", Help: "Notification attempts by channel and outcome."}, []string{"channel", "result"})
	OutboxDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "vartalaap_outbox_attempt_seconds", Help: "Notification delivery attempt duration.", Buckets: []float64{.1, .5, 1, 2, 5, 10, 20, 30}}, []string{"channel"})
)

func init() {
	prometheus.MustRegister(OutboxPending, OutboxOldest, OutboxActive, OutboxAttempts, OutboxDuration)
}
