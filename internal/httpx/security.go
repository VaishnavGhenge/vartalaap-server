package httpx

import (
	"math"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// maxAPIRequestBytes caps request bodies for regular API endpoints (auth,
	// meet, ice). These accept small JSON payloads — 16 KiB is generous.
	maxAPIRequestBytes = 16 << 10

	// maxSFURequestBytes caps request bodies for the /sfu proxy. SDP offers
	// for a single peer can include many m-sections (one per published track
	// plus one per subscribed remote track), and each m-section adds
	// codecs/extensions/ICE candidates that easily push a multi-peer offer
	// past tens of KiB. 256 KiB leaves room for ~30+ m-sections.
	maxSFURequestBytes = 256 << 10
)

type RateLimiter struct {
	mu      sync.Mutex
	rate    float64
	burst   float64
	buckets map[string]*rateBucket
}

type rateBucket struct {
	tokens float64
	seen   time.Time
}

func NewRateLimiter(perMinute, burst int) *RateLimiter {
	return &RateLimiter{
		rate:    float64(perMinute) / 60,
		burst:   float64(burst),
		buckets: make(map[string]*rateBucket),
	}
}

func (l *RateLimiter) Allow(key string) bool {
	allowed, _ := l.AllowWithRetry(key)
	return allowed
}

// AllowWithRetry is Allow plus how long the caller must wait for the bucket to
// hold a whole token again. A rejected client that is told nothing retries on
// its own schedule, which is how a repair loop keeps an emptied bucket empty.
func (l *RateLimiter) AllowWithRetry(key string) (bool, time.Duration) {
	now := time.Now()

	l.mu.Lock()
	defer l.mu.Unlock()

	b, ok := l.buckets[key]
	if !ok {
		l.buckets[key] = &rateBucket{tokens: l.burst - 1, seen: now}
		return true, 0
	}

	elapsed := now.Sub(b.seen).Seconds()
	b.tokens = min(l.burst, b.tokens+elapsed*l.rate)
	b.seen = now

	if b.tokens < 1 {
		return false, time.Duration((1-b.tokens)/l.rate*float64(time.Second)) + time.Second
	}
	b.tokens--

	for k, bucket := range l.buckets {
		if now.Sub(bucket.seen) > 10*time.Minute {
			delete(l.buckets, k)
		}
	}

	return true, 0
}

// writeRateLimited answers a throttled request. Retry-After is what turns a
// rejection into backpressure the client can obey instead of a failure it
// retries into.
func writeRateLimited(w http.ResponseWriter, retryAfter time.Duration) {
	seconds := int(math.Ceil(retryAfter.Seconds()))
	if seconds < 1 {
		seconds = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(seconds))
	http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
}

func enforceAPIRequest(w http.ResponseWriter, r *http.Request, allowedOrigins []string, methods string, limiter *RateLimiter, extraHeaders ...string) bool {
	return enforceAPIRequestWithLimit(w, r, allowedOrigins, methods, limiter, maxAPIRequestBytes, extraHeaders...)
}

// enforceAPIRequestWithLimit is the same as enforceAPIRequest but lets the
// caller choose the maximum body size. Used by the /sfu proxy where SDP
// offers exceed the default 16 KiB cap.
func enforceAPIRequestWithLimit(w http.ResponseWriter, r *http.Request, allowedOrigins []string, methods string, limiter *RateLimiter, maxBytes int64, extraHeaders ...string) bool {
	if !applyCORS(w, r, allowedOrigins, methods) {
		return false
	}
	allowedHeaders := "Content-Type, Authorization"
	if len(extraHeaders) > 0 {
		allowedHeaders += ", " + strings.Join(extraHeaders, ", ")
	}
	w.Header().Set("Access-Control-Allow-Headers", allowedHeaders)

	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return false
	}

	if limiter != nil {
		if allowed, retryAfter := limiter.AllowWithRetry(r.URL.Path + "|" + clientIP(r)); !allowed {
			writeRateLimited(w, retryAfter)
			return false
		}
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	return true
}

func applyCORS(w http.ResponseWriter, r *http.Request, allowedOrigins []string, methods string) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	if !slices.Contains(allowedOrigins, origin) {
		http.Error(w, "origin not allowed", http.StatusForbidden)
		return false
	}
	w.Header().Set("Access-Control-Allow-Origin", origin)
	w.Header().Set("Access-Control-Allow-Credentials", "true")
	w.Header().Set("Vary", "Origin")
	w.Header().Set("Access-Control-Allow-Methods", methods+", OPTIONS")
	return true
}

func clientIP(r *http.Request) string {
	for _, h := range []string{"CF-Connecting-IP", "X-Real-IP"} {
		if ip := strings.TrimSpace(r.Header.Get(h)); ip != "" {
			return ip
		}
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if ip := strings.TrimSpace(strings.Split(xff, ",")[0]); ip != "" {
			return ip
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil && host != "" {
		return host
	}
	return r.RemoteAddr
}
