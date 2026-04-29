package metrics

import "github.com/prometheus/client_golang/prometheus"

var (
	ActivePeers = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "vartalaap_active_peers",
		Help: "Current connected WebSocket peers.",
	})
	ActiveRooms = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "vartalaap_active_rooms",
		Help: "Current non-empty rooms.",
	})
	SignalsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vartalaap_signals_total",
		Help: "WebRTC signal messages forwarded, by kind (offer/answer/candidate).",
	}, []string{"kind"})
	JoinsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "vartalaap_joins_total",
		Help: "Total peer join events.",
	})
	IceRequestsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "vartalaap_ice_requests_total",
		Help: "Total ICE credential requests.",
	})
	IceErrorsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "vartalaap_ice_errors_total",
		Help: "Total ICE credential failures (Cloudflare unreachable).",
	})
)

func init() {
	prometheus.MustRegister(
		ActivePeers,
		ActiveRooms,
		SignalsTotal,
		JoinsTotal,
		IceRequestsTotal,
		IceErrorsTotal,
	)
}
