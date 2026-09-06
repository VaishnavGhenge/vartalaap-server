package httpx

import (
	"crypto/subtle"
	"log/slog"
	"net/http"
	"strings"
)

// RequireOpsToken gates the operator surfaces (/dashboard, /stats, /metrics) behind a
// shared secret from OPS_TOKEN.
//
// These two routes sat on the public mux with no gate at all: anyone who
// guessed the path could read room counts, participant counts, TTFM
// percentiles and the connection success rate off api.getsessionly.com. Raw
// Prometheus at /metrics was correctly bound to 127.0.0.1:9091, so the pattern
// was already right one layer down; these were the exception.
//
// Fails CLOSED. With no OPS_TOKEN configured the routes return 404, not 200 —
// a deploy that forgets the variable loses the dashboard, which is a visible
// and recoverable problem. The alternative, defaulting to open, is how the gap
// got here.
//
// 404 rather than 401 when unconfigured: an unconfigured route does not exist
// as far as the internet is concerned, so there is nothing to probe. When a
// token IS set the response is a normal 401 with a Basic challenge, because at
// that point a human is expected to authenticate in a browser.
func RequireOpsToken(token string, h http.HandlerFunc) http.HandlerFunc {
	// One limiter shared across every gated route: the bucket key is the client
	// IP, so this bounds guesses per source rather than per path.
	lim := NewRateLimiter(20, 10)

	if token == "" {
		return func(w http.ResponseWriter, r *http.Request) {
			http.NotFound(w, r)
		}
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if !lim.Allow(clientIP(r)) {
			w.Header().Set("Retry-After", "60")
			http.Error(w, "too many requests", http.StatusTooManyRequests)
			return
		}
		if !opsTokenMatches(r, token) {
			slog.Warn("ops_auth_rejected", "path", r.URL.Path, "remote", clientIP(r))
			// Basic so a browser prompts for credentials; any username works.
			w.Header().Set("WWW-Authenticate", `Basic realm="vartalaap ops", charset="UTF-8"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		// Operator data is per-moment and must never land in a shared cache or
		// a search index.
		w.Header().Set("Cache-Control", "no-store, private")
		w.Header().Set("X-Robots-Tag", "noindex, nofollow")
		h(w, r)
	}
}

// opsTokenMatches accepts the token two ways: HTTP Basic (password field, any
// username) so the dashboard works in a browser and its fetch('/stats') is
// authenticated by the cached credentials, and a Bearer header for curl and
// scripts.
func opsTokenMatches(r *http.Request, token string) bool {
	if _, password, ok := r.BasicAuth(); ok && constantTimeEqual(password, token) {
		return true
	}
	if after, found := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); found {
		return constantTimeEqual(strings.TrimSpace(after), token)
	}
	return false
}

// constantTimeEqual compares without leaking length through timing. The length
// check short-circuits, which is unavoidable and not the secret worth
// protecting here.
func constantTimeEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
