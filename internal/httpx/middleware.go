package httpx

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/vaishnavghenge/vartalaap-server/internal/metrics"
)

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (sr *statusRecorder) WriteHeader(code int) {
	sr.status = code
	sr.ResponseWriter.WriteHeader(code)
}

func (sr *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return sr.ResponseWriter.(http.Hijacker).Hijack()
}

// requestIDKey is the context key for the per-request UUID. Unexported so
// callers must go through RequestIDFromContext.
type requestIDKey struct{}

// RequestIDFromContext returns the request ID attached by RequestIDMiddleware,
// or "" if the request did not pass through that middleware.
func RequestIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(requestIDKey{}).(string); ok {
		return v
	}
	return ""
}

func newRequestID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// RequestIDMiddleware assigns each request a short hex ID, attaches it to the
// context, and echoes it back as X-Request-Id. If the client supplied an
// X-Request-Id header, that value is preserved so a single logical request can
// be correlated across edge proxies, the Go server, and the browser.
//
// /healthz is skipped — Kubernetes liveness probes would otherwise add a UUID
// to every probe log line without giving us anything to correlate.
func RequestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		id := strings.TrimSpace(r.Header.Get("X-Request-Id"))
		if id == "" || len(id) > 64 {
			id = newRequestID()
		}
		w.Header().Set("X-Request-Id", id)
		ctx := context.WithValue(r.Context(), requestIDKey{}, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// MetricsMiddleware observes every request's latency into HTTPRequestDuration.
// Status code is bucketed to {2xx, 3xx, 4xx, 5xx} to keep label cardinality
// bounded — full status stays in logs. /metrics and /healthz are excluded
// because scrape/probe traffic dwarfs real traffic and skews the distribution.
func MetricsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if path == "/healthz" || path == "/metrics" {
			next.ServeHTTP(w, r)
			return
		}
		// /ws is a WebSocket upgrade — its "duration" is the full connection
		// lifetime, which would skew the HTTP histogram into uselessness.
		// Active peers + ws_connect/ws_disconnect logs cover that signal.
		if path == "/ws" {
			next.ServeHTTP(w, r)
			return
		}
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		next.ServeHTTP(rec, r)
		metrics.HTTPRequestDuration.WithLabelValues(
			routeLabel(r.URL.Path),
			r.Method,
			statusClass(rec.status),
		).Observe(time.Since(start).Seconds())
	})
}

// LogMiddleware logs every request except /healthz as structured JSON.
// For WebSocket upgrades the duration reflects the full connection lifetime.
func LogMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		next.ServeHTTP(rec, r)
		slog.Info("http",
			"request_id", RequestIDFromContext(r.Context()),
			"method", r.Method,
			"path", r.URL.Path,
			"route", routeLabel(r.URL.Path),
			"status", rec.status,
			"ms", time.Since(start).Milliseconds(),
			"remote", r.RemoteAddr,
		)
	})
}

func statusClass(code int) string {
	if code < 100 || code >= 600 {
		return "unknown"
	}
	return strconv.Itoa(code/100) + "xx"
}

// routeLabel maps a raw URL path to a low-cardinality route template suitable
// for use as a Prometheus label. The SFU routes embed a session ID; the auth
// routes are all static. New dynamic routes must be added here — otherwise
// they'll fall through to "/other" and we'll lose per-route visibility.
func routeLabel(path string) string {
	switch path {
	case "/", "/healthz", "/metrics", "/stats", "/dashboard",
		"/ws", "/meets/new", "/ice-servers", "/room/status",
		"/auth/register", "/auth/login", "/auth/refresh", "/auth/logout", "/auth/me",
		"/auth/guest",
		"/me/availability", "/me/event-types", "/me/bookings", "/bookings", "/holds",
		// Calendar routes are all static despite the /me/calendar/ prefix
		// registration — the handler switches on a fixed action set, so an
		// unrecognised action is a 404 and correctly falls through to "/other"
		// rather than minting a label per garbage path.
		"/me/calendar/status", "/me/calendar/connect/google",
		"/me/calendar/callback/google", "/me/calendar/disconnect":
		return path
	}
	// /me/event-types/{id} — normalise the ID out of the label.
	if strings.HasPrefix(path, "/me/event-types/") {
		return "/me/event-types/:id"
	}
	if strings.HasPrefix(path, "/bookings/") {
		return "/bookings/:id"
	}
	if strings.HasPrefix(path, "/holds/") {
		return "/holds/:token"
	}
	// /u/{slug}[/{event}[/slots]] — public host/profile/slot routes.
	if strings.HasPrefix(path, "/u/") {
		tail := strings.TrimPrefix(path, "/u/")
		parts := strings.Split(tail, "/")
		switch len(parts) {
		case 1:
			return "/u/:slug"
		case 2:
			return "/u/:slug/:event"
		case 3:
			if parts[2] == "slots" {
				return "/u/:slug/:event/slots"
			}
		}
		return "/other"
	}
	// /m/{code} — public meet-code lookup for the confirmation page.
	if strings.HasPrefix(path, "/m/") {
		return "/m/:code"
	}
	// /sfu/sessions/* — normalize session ID and sub-route.
	if strings.HasPrefix(path, "/sfu/sessions/") {
		tail := strings.TrimPrefix(path, "/sfu/sessions/")
		// /sfu/sessions/new (literal)
		if tail == "new" {
			return "/sfu/sessions/new"
		}
		// /sfu/sessions/{id}[/<sub>...]
		parts := strings.SplitN(tail, "/", 2)
		if len(parts) == 1 {
			return "/sfu/sessions/:id"
		}
		return "/sfu/sessions/:id/" + parts[1]
	}
	return "/other"
}
