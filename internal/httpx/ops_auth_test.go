package httpx

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"
)

func opsRequest(t *testing.T, mutate func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/stats", nil)
	req.RemoteAddr = "203.0.113.10:12345"
	if mutate != nil {
		mutate(req)
	}
	rec := httptest.NewRecorder()
	RequireOpsToken("s3cret-token", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("payload"))
	})(rec, req)
	return rec
}

// Fail closed: a deploy that forgets OPS_TOKEN must lose the dashboard rather
// than publish it. Defaulting to open is how these routes ended up readable
// from the internet in the first place.
func TestRequireOpsToken_UnconfiguredReturns404(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	rec := httptest.NewRecorder()
	called := false
	RequireOpsToken("", func(w http.ResponseWriter, r *http.Request) { called = true })(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 when unconfigured, got %d", rec.Code)
	}
	if called {
		t.Fatal("handler must not run when no ops token is configured")
	}
}

func TestRequireOpsToken_RejectsAnonymous(t *testing.T) {
	rec := opsRequest(t, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
	// The browser needs a challenge to prompt with, otherwise the dashboard is
	// unreachable for a human.
	if got := rec.Header().Get("WWW-Authenticate"); got == "" {
		t.Fatal("expected a WWW-Authenticate challenge")
	}
	if rec.Body.String() == "payload" {
		t.Fatal("body leaked past the gate")
	}
}

func TestRequireOpsToken_RejectsWrongToken(t *testing.T) {
	rec := opsRequest(t, func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer wrong-token-x")
	})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for a wrong bearer token, got %d", rec.Code)
	}
}

// Basic auth is what makes the dashboard usable in a browser: the prompt
// supplies the credentials, and the page's own fetch('/stats') then carries
// them automatically.
func TestRequireOpsToken_AcceptsBasicPassword(t *testing.T) {
	rec := opsRequest(t, func(r *http.Request) {
		r.SetBasicAuth("ops", "s3cret-token")
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for correct basic password, got %d", rec.Code)
	}
	if rec.Body.String() != "payload" {
		t.Fatalf("expected handler body, got %q", rec.Body.String())
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store, private" {
		t.Fatalf("operator data must not be cacheable, got %q", got)
	}
}

// The username is ignored on purpose — there is one shared operator secret, so
// pinning a username would add a second thing to remember and nothing else.
func TestRequireOpsToken_IgnoresBasicUsername(t *testing.T) {
	rec := opsRequest(t, func(r *http.Request) {
		r.SetBasicAuth("anything-at-all", "s3cret-token")
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 regardless of username, got %d", rec.Code)
	}
}

func TestRequireOpsToken_AcceptsBearer(t *testing.T) {
	rec := opsRequest(t, func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer s3cret-token")
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for correct bearer token, got %d", rec.Code)
	}
}

// A wrong password must not be distinguishable by response shape from a wrong
// username, and a prefix of the real token must not pass.
func TestRequireOpsToken_RejectsTokenPrefix(t *testing.T) {
	rec := opsRequest(t, func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer s3cret")
	})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for a token prefix, got %d", rec.Code)
	}
}

// Malformed credentials are a rejection, never a panic.
func TestRequireOpsToken_RejectsMalformedBasicHeader(t *testing.T) {
	rec := opsRequest(t, func(r *http.Request) {
		r.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("no-colon-here")))
	})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for a malformed basic header, got %d", rec.Code)
	}
}

// Brute forcing the shared secret should get slow fast. The limiter is keyed by
// client IP so one prober cannot spend another operator's budget.
func TestRequireOpsToken_RateLimitsGuesses(t *testing.T) {
	gate := RequireOpsToken("s3cret-token", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	limited := false
	for i := 0; i < 40; i++ {
		req := httptest.NewRequest(http.MethodGet, "/stats", nil)
		req.RemoteAddr = "198.51.100.7:5555"
		req.Header.Set("Authorization", "Bearer guess-guess-gue")
		rec := httptest.NewRecorder()
		gate(rec, req)
		if rec.Code == http.StatusTooManyRequests {
			limited = true
			break
		}
	}
	if !limited {
		t.Fatal("expected repeated wrong guesses from one IP to be rate limited")
	}
}
