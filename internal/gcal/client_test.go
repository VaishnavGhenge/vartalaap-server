package gcal

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func newTestClient(t *testing.T, h http.Handler) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c := NewWithBase("client-id", "client-secret", "https://api.test/cb", srv.URL)
	// Keep the retry backoff from dominating test runtime while still
	// exercising the retry path.
	return c, srv
}

func idToken(email string) string {
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"email":"` + email + `"}`))
	return "header." + payload + ".signature"
}

func TestAuthCodeURLCarriesOfflineConsent(t *testing.T) {
	c := New("cid", "secret", "https://api.test/cb")
	raw := c.AuthCodeURL("state-token")
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	q := u.Query()
	// access_type=offline + prompt=consent are what guarantee a refresh token.
	// Without both, a reconnect yields a connection that dies in an hour.
	if q.Get("access_type") != "offline" {
		t.Errorf("access_type = %q, want offline", q.Get("access_type"))
	}
	if q.Get("prompt") != "consent" {
		t.Errorf("prompt = %q, want consent", q.Get("prompt"))
	}
	if q.Get("state") != "state-token" {
		t.Errorf("state = %q", q.Get("state"))
	}
	if !strings.Contains(q.Get("scope"), "calendar.freebusy") ||
		!strings.Contains(q.Get("scope"), "calendar.events") {
		t.Errorf("scope missing calendar scopes: %q", q.Get("scope"))
	}
	// Full-calendar read would expose event contents we have no use for.
	if strings.Contains(q.Get("scope"), "auth/calendar.readonly") ||
		strings.Contains(q.Get("scope"), "auth/calendar ") {
		t.Errorf("scope is broader than needed: %q", q.Get("scope"))
	}
}

func TestExchangeParsesTokensAndEmail(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/token" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		_ = r.ParseForm()
		if r.Form.Get("grant_type") != "authorization_code" {
			t.Errorf("grant_type = %q", r.Form.Get("grant_type"))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "at-1",
			"refresh_token": "rt-1",
			"expires_in":    3600,
			"id_token":      idToken("host@example.com"),
		})
	}))
	tok, err := c.Exchange(context.Background(), "code-1")
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if tok.AccessToken != "at-1" || tok.RefreshToken != "rt-1" {
		t.Fatalf("unexpected tokens: %+v", tok)
	}
	if tok.AccountEmail != "host@example.com" {
		t.Fatalf("account email = %q", tok.AccountEmail)
	}
	// Expiry is shortened by a minute so a token never expires mid-flight.
	if d := time.Until(tok.ExpiresAt); d > 59*time.Minute+30*time.Second {
		t.Fatalf("expiry not shaved: %v", d)
	}
}

// invalid_grant is permanent. It must be classified, not retried: retrying
// hammers Google with a request that can never succeed and burns the caller's
// deadline before the "reconnect required" state is recorded.
func TestRefreshInvalidGrantIsPermanent(t *testing.T) {
	var calls int32
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error":             "invalid_grant",
			"error_description": "Token has been expired or revoked.",
		})
	}))
	_, err := c.Refresh(context.Background(), "dead-token")
	if !errors.Is(err, ErrInvalidGrant) {
		t.Fatalf("want ErrInvalidGrant, got %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("invalid_grant retried %d times, want 1 attempt", got)
	}
}

func TestRefreshRetriesServerErrorThenSucceeds(t *testing.T) {
	var calls int32
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"backend_error"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "at-2", "expires_in": 3600,
		})
	}))
	tok, err := c.Refresh(context.Background(), "rt")
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if tok.AccessToken != "at-2" {
		t.Fatalf("token = %q", tok.AccessToken)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("calls = %d, want 2 (one failure + one retry)", got)
	}
}

// A 4xx that isn't invalid_grant is our bug (bad client id, malformed body).
// Retrying it is pure waste.
func TestClientErrorIsNotRetried(t *testing.T) {
	var calls int32
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid_client"}`))
	}))
	if _, err := c.Exchange(context.Background(), "code"); err == nil {
		t.Fatal("want error")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("calls = %d, want 1", got)
	}
}

func TestFreeBusyParsesIntervals(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/calendar/v3/freeBusy" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer at-1" {
			t.Errorf("auth header = %q", got)
		}
		_, _ = w.Write([]byte(`{"calendars":{"primary":{"busy":[
			{"start":"2026-09-01T09:00:00Z","end":"2026-09-01T10:00:00Z"},
			{"start":"2026-09-01T14:00:00Z","end":"2026-09-01T14:30:00Z"}]}}}`))
	}))
	got, err := c.FreeBusy(context.Background(), "at-1", "primary",
		time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("freebusy: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 intervals, got %d", len(got))
	}
	if !got[0].Start.Equal(time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)) {
		t.Fatalf("first interval start = %v", got[0].Start)
	}
}

// A per-calendar error inside a 200 response must NOT read as "no busy
// periods". Free and unknown lead to opposite booking decisions.
func TestFreeBusyCalendarErrorIsAnError(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"calendars":{"primary":{"busy":[],
			"errors":[{"domain":"calendar","reason":"notFound"}]}}}`))
	}))
	_, err := c.FreeBusy(context.Background(), "at", "primary", time.Now(), time.Now().Add(time.Hour))
	if err == nil {
		t.Fatal("per-calendar error was swallowed as an empty busy list")
	}
	if !strings.Contains(err.Error(), "notFound") {
		t.Fatalf("error should name the reason: %v", err)
	}
}

func TestFreeBusyMissingCalendarIsAnError(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"calendars":{}}`))
	}))
	if _, err := c.FreeBusy(context.Background(), "at", "primary", time.Now(), time.Now().Add(time.Hour)); err == nil {
		t.Fatal("absent calendar key was treated as free")
	}
}

func TestEventIDIsDeterministicAndValid(t *testing.T) {
	id := EventID("3f8a1c2e-1111-4222-8333-abcdef012345")
	if id != EventID("3f8a1c2e-1111-4222-8333-abcdef012345") {
		t.Fatal("not deterministic")
	}
	// Google requires base32hex: digits 0-9 and letters a-v, length 5..1024.
	if len(id) < 5 || len(id) > 1024 {
		t.Fatalf("length %d out of range", len(id))
	}
	for _, r := range id {
		if !(r >= '0' && r <= '9') && !(r >= 'a' && r <= 'v') {
			t.Fatalf("character %q is outside Google's base32hex alphabet (id=%s)", r, id)
		}
	}
}

func TestInsertEventSendsDeterministicIDAndNoGoogleInvites(t *testing.T) {
	var body map[string]any
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("sendUpdates"); got != "none" {
			t.Errorf("sendUpdates = %q, want none (Sessionly sends its own email)", got)
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"whatever"}`))
	}))
	id, err := c.InsertEvent(context.Background(), "at", "primary", Event{
		BookingID: "abc-123", Summary: "Intro call", Start: time.Now(), End: time.Now().Add(time.Hour),
		GuestEmail: "guest@example.com", GuestName: "Guest",
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if id != EventID("abc-123") {
		t.Fatalf("returned id = %q, want the deterministic one", id)
	}
	if body["id"] != EventID("abc-123") {
		t.Fatalf("request id = %v, want deterministic id", body["id"])
	}
}

// The whole point of the deterministic event ID: a retry after a lost response
// hits Google's duplicate check instead of creating a second event on the
// host's calendar.
func TestInsertEventTreats409AsSuccess(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":{"message":"The requested identifier already exists."}}`))
	}))
	id, err := c.InsertEvent(context.Background(), "at", "primary", Event{
		BookingID: "abc-123", Start: time.Now(), End: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("409 should be success, got %v", err)
	}
	if id != EventID("abc-123") {
		t.Fatalf("id = %q", id)
	}
}

func TestDeleteEventTreatsGoneAsSuccess(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusGone} {
		c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
		}))
		if err := c.DeleteEvent(context.Background(), "at", "primary", "evt"); err != nil {
			t.Fatalf("status %d should be success, got %v", status, err)
		}
	}
}

func TestDeleteEventSurfacesRealFailure(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"forbidden"}`))
	}))
	if err := c.DeleteEvent(context.Background(), "at", "primary", "evt"); err == nil {
		t.Fatal("403 must surface — the event is still on the host's calendar")
	}
}

// A cancelled context must abort promptly rather than working through the
// backoff schedule.
func TestRetryRespectsContextCancellation(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.FreeBusy(ctx, "at", "primary", time.Now(), time.Now().Add(time.Hour)); err == nil {
		t.Fatal("want error on cancelled context")
	}
}
