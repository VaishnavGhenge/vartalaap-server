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
	//
	// The connection-success SLO (target 99.5%) is
	//   (success + slow) / (success + slow + failed + error)
	//
	// `slow` is in the numerator on purpose: it means media arrived after the
	// 10s ceiling, which is a working call the user is sitting in. Counting it
	// as a failure made the SLO disagree with what people experienced, and made
	// the number get worse when a slow call was rescued rather than better. How
	// long setup took is the TTFM histogram's question, not this counter's.
	//
	// `abandoned` is excluded from both sides: nobody was publishing, so no
	// media was owed and there was no call to succeed or fail at.
	//
	// `timeout` is legacy, retained for historical series.
	CallAttempts = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vartalaap_call_attempts_total",
		Help: "Call setup attempts by outcome (success, slow, failed, error, abandoned; timeout is legacy). Success SLO = (success+slow)/(success+slow+failed+error).",
	}, []string{"result"})

	// CallSetupFailures breaks a timeout (CallAttempts result=timeout) down by
	// root cause — the errors-by-type golden signal. Lets us tell a server
	// broadcast gap (no_tracks_announced) from a dead CF pull
	// (tracks_announced_not_pulled) without reading client logs. The reason
	// label is whitelisted in client.go to keep cardinality bounded.
	CallSetupFailures = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vartalaap_call_setup_failures_total",
		Help: "Call setup timeouts by root cause (no_tracks_announced, tracks_announced_not_pulled, pull_errored, peers_present_none_publishing, unknown).",
	}, []string{"reason"})

	// SfuPublishAccepted and SfuAnnounces are a deliberate pair. Cloudflare
	// accepting a publish (200 on tracks/new) does NOT mean the track is live:
	// the publisher's browser still has to apply the answer, and only then does
	// it announce over sfu-announce. Announcing server-side from the 200 raced
	// ahead of the publisher and let subscribers pull tracks nobody was sending
	// (the 2026-09-01 dead-track incident), so the announce is now the
	// publisher's alone.
	//
	// The gap between these two counters is the cost of that correctness:
	// accepted-but-never-announced means a push that Cloudflare took and the
	// browser never confirmed. It should be near zero. A persistent gap is a
	// real client-side publish failure, which is exactly what we want visible
	// rather than masked by a server-side broadcast.
	SfuPublishAccepted = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "vartalaap_sfu_publish_accepted_total",
		Help: "Publishes Cloudflare accepted on tracks/new. Compare with vartalaap_sfu_announces_total: the gap is pushes the publisher never confirmed.",
	})

	SfuAnnounces = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "vartalaap_sfu_announces_total",
		Help: "Track-set announcements received from publishers after a CF push ack. The sole trigger for broadcasting sfu-tracks to a room.",
	})

	// SfuRepairs counts the self-healing ladder. `stage` is publish or
	// subscribe, `rung` is how far it escalated (1 = retry in place, 2 = rebuild
	// that direction's CF session), `outcome` is attempted or recovered.
	//
	// The pair is what makes it readable. attempted{rung="1"} with a matching
	// recovered{rung="1"} is the system working: a cheap retry fixed it. A
	// large attempted count with almost no recovered is a ladder spinning
	// without healing anything, which is worse than no ladder because it hides
	// the failure behind activity. Everything landing on rung 2 means the cheap
	// retry never works and rung 1 should be reconsidered.
	SfuRepairs = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vartalaap_sfu_repairs_total",
		Help: "Media repair attempts and recoveries by stage (publish/subscribe), rung (1=retry, 2=rebuild session) and outcome (attempted/recovered).",
	}, []string{"stage", "rung", "outcome"})

	// ─── Calendar sync (Phase 3) ────────────────────────────────────────────
	//
	// CalendarAPIRequests is the errors-by-type golden signal for Google
	// Calendar. `result` separates the failures that need different responses:
	// "revoked" means the host must reconnect (a human action), "error" and
	// "timeout" are ours to retry or absorb. Op keeps cardinality bounded to
	// the five calls in internal/gcal.
	CalendarAPIRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vartalaap_calendar_api_requests_total",
		Help: "Google Calendar API calls by operation and outcome (success, error, timeout, revoked).",
	}, []string{"op", "result"})

	CalendarAPIDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "vartalaap_calendar_api_duration_seconds",
		Help:    "Google Calendar API latency by operation, including retries.",
		Buckets: []float64{0.1, 0.25, 0.5, 1, 2, 3, 5, 8, 12},
	}, []string{"op"})

	// CalendarBusyDegraded counts slot-generation requests that ran WITHOUT
	// the host's busy data because Google was unreachable. Every increment is
	// a booking page that could double-book. This is the alert to wire first:
	// the failure is invisible to the guest and to the host until someone
	// shows up to a slot the host was already busy in.
	CalendarBusyDegraded = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "vartalaap_calendar_busy_degraded_total",
		Help: "Slot generations that omitted Google busy periods due to a failed lookup.",
	})

	// CalendarWritebackFailures counts bookings whose calendar event could not
	// be created or deleted. The booking itself is fine; the host's calendar is
	// out of sync until they reconnect or the booking is re-synced.
	CalendarWritebackFailures = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vartalaap_calendar_writeback_failures_total",
		Help: "Calendar event write-backs that failed, by action (create, delete).",
	}, []string{"action"})
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
		CallSetupFailures,
		SfuPublishAccepted,
		SfuAnnounces,
		SfuRepairs,
		CalendarAPIRequests,
		CalendarAPIDuration,
		CalendarBusyDegraded,
		CalendarWritebackFailures,
	)
}
