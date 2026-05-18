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
	// SignalsTotal counts every WS message handled by the signaling server.
	// Kind is the message type (join, leave, peer-state, sfu-tracks, stats-report,
	// client-metric, ping). The legacy offer/answer/candidate kinds remain in the
	// snapshot for back-compat but will stay at 0 in SFU mode.
	SignalsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vartalaap_signals_total",
		Help: "WS messages handled, by message type.",
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

	// HTTPRequestDuration measures every HTTP request's latency in seconds.
	// route is the normalized template (e.g. "/sfu/sessions/:id/tracks/new"),
	// not the raw path, so cardinality stays bounded.
	HTTPRequestDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "vartalaap_http_request_duration_seconds",
		Help:    "HTTP request latency by route, method, and status class.",
		Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10},
	}, []string{"route", "method", "status_class"})

	// TimeToFirstMedia is the headline call-quality SLO: seconds from the
	// client's `join` message being sent to the first remote MediaStreamTrack
	// being emitted by the SFU subscribe PC. Buckets chosen so we can read
	// p50/p95/p99 against the targets in CLAUDE.md (p95 ≤ 3s, p99 ≤ 6s, hard
	// fail at 10s).
	TimeToFirstMedia = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "vartalaap_time_to_first_media_seconds",
		Help:    "Seconds from join sent to first remote track. SLO: p95 ≤ 3s, p99 ≤ 6s, fail > 10s.",
		Buckets: []float64{0.5, 1, 1.5, 2, 3, 4, 6, 8, 10, 15},
	})

	// CallSetupPhase breaks TTFM into observable sub-phases so we can attribute
	// regressions to ICE, SDP, or first-frame-after-subscribe.
	CallSetupPhase = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "vartalaap_call_setup_phase_seconds",
		Help:    "Per-phase call setup latency. Phases: ice_gather, pub_connected, sub_connected, first_media.",
		Buckets: []float64{0.1, 0.25, 0.5, 1, 1.5, 2, 3, 4, 6, 8, 10},
	}, []string{"phase"})

	// CallAttempts counts every attempt to establish a call, labeled by outcome.
	// Success ratio = success / total — the connection-success SLO (target 99.5%).
	CallAttempts = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vartalaap_call_attempts_total",
		Help: "Call setup attempts by outcome (success, timeout, error, abandoned).",
	}, []string{"result"})
)

func init() {
	prometheus.MustRegister(
		ActivePeers,
		ActiveRooms,
		SignalsTotal,
		JoinsTotal,
		IceRequestsTotal,
		IceErrorsTotal,
		HTTPRequestDuration,
		TimeToFirstMedia,
		CallSetupPhase,
		CallAttempts,
	)
}
