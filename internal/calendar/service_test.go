package calendar

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vaishnavghenge/vartalaap-server/internal/gcal"
	"github.com/vaishnavghenge/vartalaap-server/internal/secretbox"
	"github.com/vaishnavghenge/vartalaap-server/internal/store"
)

// fakeStore implements only the calendar slice of store.Storer. The embedded
// nil interface makes any other method a loud panic rather than a silent
// wrong answer, which is what we want from a test double.
type fakeStore struct {
	store.Storer

	conn      *store.CalendarConnection
	mapping   *store.BookingCalendarEvent
	getErr    error
	upserted  *store.CalendarConnection
	revoked   string
	syncErr   *string
	syncCalls int32
	created   *store.BookingCalendarEvent
	createErr error
	deleted   bool
	tokenSet  string
}

func (f *fakeStore) GetCalendarConnection(_ context.Context, _, _ string) (*store.CalendarConnection, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	if f.conn == nil {
		return nil, store.ErrNotFound
	}
	cp := *f.conn
	return &cp, nil
}

func (f *fakeStore) UpsertCalendarConnection(_ context.Context, c store.CalendarConnection) (*store.CalendarConnection, error) {
	c.ID = "conn-1"
	f.upserted = &c
	return &c, nil
}

func (f *fakeStore) UpdateCalendarAccessToken(_ context.Context, _, enc string, exp time.Time) error {
	f.tokenSet = enc
	if f.conn != nil {
		f.conn.AccessToken = enc
		e := exp
		f.conn.ExpiresAt = &e
	}
	return nil
}

func (f *fakeStore) MarkCalendarRevoked(_ context.Context, _, reason string) error {
	f.revoked = reason
	return nil
}

func (f *fakeStore) RecordCalendarSync(_ context.Context, _ string, syncErr *string) error {
	atomic.AddInt32(&f.syncCalls, 1)
	f.syncErr = syncErr
	return nil
}

func (f *fakeStore) DeleteCalendarConnection(_ context.Context, _, _ string) error {
	f.deleted = true
	f.conn = nil
	return nil
}

func (f *fakeStore) CreateBookingCalendarEvent(_ context.Context, e store.BookingCalendarEvent) error {
	if f.createErr != nil {
		return f.createErr
	}
	f.created = &e
	return nil
}

func (f *fakeStore) GetBookingCalendarEvent(_ context.Context, _, _ string) (*store.BookingCalendarEvent, error) {
	if f.mapping == nil {
		return nil, store.ErrNotFound
	}
	cp := *f.mapping
	return &cp, nil
}

func (f *fakeStore) DeleteBookingCalendarEvent(_ context.Context, _, _ string) error {
	f.mapping = nil
	return nil
}

// stubSigner keeps state handling trivial and independent of JWT specifics.
type stubSigner struct{ fail bool }

// Encodes the return path into the fake state so the round-trip is observable.
func (s stubSigner) SignPurposeToken(userID, purpose, returnTo, _ string, _ time.Duration) (string, error) {
	return purpose + ":" + returnTo + ":" + userID, nil
}

func (s stubSigner) VerifyPurposeToken(tokenStr, purpose, _ string) (string, string, error) {
	if s.fail {
		return "", "", errors.New("bad state")
	}
	rest, ok := strings.CutPrefix(tokenStr, purpose+":")
	if !ok {
		return "", "", errors.New("wrong purpose")
	}
	parts := strings.SplitN(rest, ":", 2)
	if len(parts) != 2 {
		return "", "", errors.New("malformed stub state")
	}
	return parts[1], parts[0], nil
}

func newTestService(t *testing.T, st *fakeStore, h http.Handler) *Service {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	box, err := secretbox.NewFromEncodedKey(strings.Repeat("a1", 32))
	if err != nil {
		t.Fatalf("box: %v", err)
	}
	return NewService(Options{
		Store:        st,
		Client:       gcal.NewWithBase("cid", "secret", "https://api.test/cb", srv.URL),
		Box:          box,
		Signer:       stubSigner{},
		JWTSecret:    "secret",
		PublicAppURL: "https://app.test",
	})
}

func encrypted(t *testing.T, svc *Service, v string) string {
	t.Helper()
	out, err := svc.box.Encrypt(v)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	return out
}

func liveConn(t *testing.T, svc *Service) *store.CalendarConnection {
	t.Helper()
	future := time.Now().UTC().Add(time.Hour)
	return &store.CalendarConnection{
		ID:           "conn-1",
		UserID:       "host-1",
		Provider:     "google",
		AccessToken:  encrypted(t, svc, "at-live"),
		RefreshToken: encrypted(t, svc, "rt-live"),
		ExpiresAt:    &future,
		CalendarID:   "primary",
	}
}

// ─── BusyPeriods ─────────────────────────────────────────────────────────────

// No connection is not an error. It means nothing is blocked, and slot
// generation must proceed normally.
func TestBusyPeriodsWithoutConnection(t *testing.T) {
	st := &fakeStore{}
	svc := newTestService(t, st, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("Google must not be called when no calendar is connected")
	}))
	got, err := svc.BusyPeriods(context.Background(), "host-1", time.Now(), time.Now().Add(time.Hour))
	if err != nil || got != nil {
		t.Fatalf("want (nil, nil), got (%v, %v)", got, err)
	}
}

// A revoked grant behaves like no connection for reads. Calling Google with a
// dead token on every public /slots request would be a self-inflicted rate
// limit.
func TestBusyPeriodsSkipsRevokedConnection(t *testing.T) {
	st := &fakeStore{}
	svc := newTestService(t, st, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("Google must not be called for a revoked connection")
	}))
	conn := liveConn(t, svc)
	now := time.Now().UTC()
	conn.RevokedAt = &now
	st.conn = conn

	got, err := svc.BusyPeriods(context.Background(), "host-1", time.Now(), time.Now().Add(time.Hour))
	if err != nil || got != nil {
		t.Fatalf("want (nil, nil), got (%v, %v)", got, err)
	}
}

func TestBusyPeriodsReturnsIntervals(t *testing.T) {
	st := &fakeStore{}
	svc := newTestService(t, st, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer at-live" {
			t.Errorf("auth = %q, want the stored access token", got)
		}
		_, _ = w.Write([]byte(`{"calendars":{"primary":{"busy":[
			{"start":"2026-09-01T09:00:00Z","end":"2026-09-01T10:00:00Z"}]}}}`))
	}))
	st.conn = liveConn(t, svc)

	got, err := svc.BusyPeriods(context.Background(), "host-1",
		time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("busy: %v", err)
	}
	if len(got) != 1 || !got[0].Start.Equal(time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)) {
		t.Fatalf("unexpected intervals: %+v", got)
	}
}

// A fetch failure must NOT be flattened into an empty busy list — the caller
// has to be able to tell "free" from "unknown".
func TestBusyPeriodsPropagatesFetchFailure(t *testing.T) {
	st := &fakeStore{}
	svc := newTestService(t, st, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	st.conn = liveConn(t, svc)

	got, err := svc.BusyPeriods(context.Background(), "host-1", time.Now(), time.Now().Add(time.Hour))
	if err == nil {
		t.Fatal("a failed lookup must return an error, not an empty list")
	}
	if got != nil {
		t.Fatalf("no intervals should be returned on failure, got %v", got)
	}
}

func TestBusyPeriodsRefreshesExpiredToken(t *testing.T) {
	st := &fakeStore{}
	var refreshed, fetched int32
	svc := newTestService(t, st, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/token"):
			atomic.AddInt32(&refreshed, 1)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": "at-fresh", "expires_in": 3600,
			})
		default:
			atomic.AddInt32(&fetched, 1)
			if got := r.Header.Get("Authorization"); got != "Bearer at-fresh" {
				t.Errorf("free/busy used %q, want the refreshed token", got)
			}
			_, _ = w.Write([]byte(`{"calendars":{"primary":{"busy":[]}}}`))
		}
	}))
	conn := liveConn(t, svc)
	past := time.Now().UTC().Add(-time.Hour)
	conn.ExpiresAt = &past
	st.conn = conn

	if _, err := svc.BusyPeriods(context.Background(), "host-1", time.Now(), time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("busy: %v", err)
	}
	if refreshed != 1 || fetched != 1 {
		t.Fatalf("refreshed=%d fetched=%d, want 1 and 1", refreshed, fetched)
	}
	// The refreshed token must be persisted, encrypted, or every request pays
	// for another refresh.
	if st.tokenSet == "" {
		t.Fatal("refreshed access token was not persisted")
	}
	if strings.Contains(st.tokenSet, "at-fresh") {
		t.Fatal("access token was persisted in plaintext")
	}
}

// A dead grant tombstones the connection and reads as "no calendar", so the
// booking page keeps working while the dashboard tells the host to reconnect.
func TestBusyPeriodsMarksRevokedOnInvalidGrant(t *testing.T) {
	st := &fakeStore{}
	svc := newTestService(t, st, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
	}))
	conn := liveConn(t, svc)
	past := time.Now().UTC().Add(-time.Hour)
	conn.ExpiresAt = &past
	st.conn = conn

	got, err := svc.BusyPeriods(context.Background(), "host-1", time.Now(), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("a revoked grant should degrade quietly, got %v", err)
	}
	if got != nil {
		t.Fatalf("want no intervals, got %v", got)
	}
	if st.revoked == "" {
		t.Fatal("connection was not tombstoned — the host would never be told to reconnect")
	}
}

// ─── Write-back ──────────────────────────────────────────────────────────────

func bookingEvent() BookingEvent {
	return BookingEvent{
		BookingID:    "book-1",
		HostID:       "host-1",
		HostTimezone: "Asia/Kolkata",
		EventTitle:   "Intro call",
		GuestName:    "Guest",
		GuestEmail:   "guest@example.com",
		StartsAt:     time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC),
		EndsAt:       time.Date(2026, 9, 1, 9, 30, 0, 0, time.UTC),
		MeetCode:     "abc123",
		RoomURL:      "https://app.test/room/abc123",
	}
}

func TestSyncBookingCreatedStoresMapping(t *testing.T) {
	st := &fakeStore{}
	svc := newTestService(t, st, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["location"] != "https://app.test/room/abc123" {
			t.Errorf("location = %v, want the room link", body["location"])
		}
		_, _ = w.Write([]byte(`{"id":"ignored"}`))
	}))
	st.conn = liveConn(t, svc)

	svc.SyncBookingCreated(context.Background(), bookingEvent())

	if st.created == nil {
		t.Fatal("mapping was not stored — cancellation could never find the event")
	}
	if st.created.EventID != gcal.EventID("book-1") {
		t.Fatalf("event id = %q", st.created.EventID)
	}
	if st.syncErr != nil {
		t.Fatalf("success recorded an error: %v", *st.syncErr)
	}
}

// The booking already exists by this point. A Google failure must be recorded,
// not propagated, and must never panic the request.
func TestSyncBookingCreatedAbsorbsFailure(t *testing.T) {
	st := &fakeStore{}
	svc := newTestService(t, st, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	st.conn = liveConn(t, svc)

	svc.SyncBookingCreated(context.Background(), bookingEvent())

	if st.created != nil {
		t.Fatal("mapping stored despite the insert failing")
	}
	if st.syncErr == nil || !strings.Contains(*st.syncErr, "create failed") {
		t.Fatalf("failure not recorded on the connection: %v", st.syncErr)
	}
}

func TestSyncBookingCreatedNoConnectionIsNoop(t *testing.T) {
	st := &fakeStore{}
	svc := newTestService(t, st, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("Google must not be called without a connection")
	}))
	svc.SyncBookingCreated(context.Background(), bookingEvent())
	if st.created != nil {
		t.Fatal("mapping stored without a connection")
	}
}

func TestSyncBookingCancelledDeletesEventAndMapping(t *testing.T) {
	st := &fakeStore{}
	var deleted int32
	svc := newTestService(t, st, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			atomic.AddInt32(&deleted, 1)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
	}))
	st.conn = liveConn(t, svc)
	st.mapping = &store.BookingCalendarEvent{
		BookingID: "book-1", Provider: "google",
		EventID: gcal.EventID("book-1"), CalendarID: "primary",
	}

	svc.SyncBookingCancelled(context.Background(), "host-1", "book-1")

	if deleted != 1 {
		t.Fatalf("delete calls = %d, want 1", deleted)
	}
	if st.mapping != nil {
		t.Fatal("mapping should be cleared after a successful delete")
	}
}

// Keeping the mapping on failure is deliberate: it is the only record of what
// still needs deleting. Dropping it would strand a stale event forever.
func TestSyncBookingCancelledKeepsMappingOnFailure(t *testing.T) {
	st := &fakeStore{}
	svc := newTestService(t, st, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	st.conn = liveConn(t, svc)
	st.mapping = &store.BookingCalendarEvent{
		BookingID: "book-1", Provider: "google", EventID: "evt", CalendarID: "primary",
	}

	svc.SyncBookingCancelled(context.Background(), "host-1", "book-1")

	if st.mapping == nil {
		t.Fatal("mapping was dropped despite the delete failing")
	}
	if st.syncErr == nil || !strings.Contains(*st.syncErr, "delete failed") {
		t.Fatalf("failure not recorded: %v", st.syncErr)
	}
}

func TestSyncBookingCancelledWithoutMappingIsNoop(t *testing.T) {
	st := &fakeStore{}
	svc := newTestService(t, st, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("Google must not be called when nothing was mirrored")
	}))
	st.conn = liveConn(t, svc)
	svc.SyncBookingCancelled(context.Background(), "host-1", "book-1")
}

// ─── OAuth lifecycle ─────────────────────────────────────────────────────────

func TestCompleteStoresEncryptedTokens(t *testing.T) {
	st := &fakeStore{}
	svc := newTestService(t, st, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "at-new", "refresh_token": "rt-new", "expires_in": 3600,
		})
	}))
	userID, _, err := svc.Complete(context.Background(), statePurpose+":/dashboard:host-9", "code")
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if userID != "host-9" {
		t.Fatalf("user = %q", userID)
	}
	if st.upserted == nil {
		t.Fatal("connection not stored")
	}
	if strings.Contains(st.upserted.AccessToken, "at-new") ||
		strings.Contains(st.upserted.RefreshToken, "rt-new") {
		t.Fatal("tokens were stored in plaintext")
	}
	// Round-trip proves the stored ciphertext is usable, not just opaque.
	if got, err := svc.box.Decrypt(st.upserted.RefreshToken); err != nil || got != "rt-new" {
		t.Fatalf("stored refresh token does not decrypt: %v", err)
	}
}

func TestCompleteRejectsBadState(t *testing.T) {
	st := &fakeStore{}
	svc := newTestService(t, st, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("code must not be exchanged when state fails verification")
	}))
	svc.signer = stubSigner{fail: true}
	if _, _, err := svc.Complete(context.Background(), "forged", "code"); err == nil {
		t.Fatal("forged state accepted")
	}
	if st.upserted != nil {
		t.Fatal("connection stored despite invalid state")
	}
}

// Storing a connection with no refresh token produces a calendar that works
// for one hour and then silently stops. Refusing is the better failure.
func TestCompleteRejectsMissingRefreshToken(t *testing.T) {
	st := &fakeStore{}
	svc := newTestService(t, st, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "at-new", "expires_in": 3600,
		})
	}))
	if _, _, err := svc.Complete(context.Background(), statePurpose+":/dashboard:host-9", "code"); err == nil {
		t.Fatal("accepted a grant with no refresh token")
	}
	if st.upserted != nil {
		t.Fatal("stored a connection that would die in an hour")
	}
}

// The host asked to disconnect. A failure to reach Google must not leave us
// still holding usable tokens.
func TestDisconnectDeletesLocallyEvenWhenRevokeFails(t *testing.T) {
	st := &fakeStore{}
	svc := newTestService(t, st, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	st.conn = liveConn(t, svc)

	if err := svc.Disconnect(context.Background(), "host-1"); err != nil {
		t.Fatalf("disconnect: %v", err)
	}
	if !st.deleted {
		t.Fatal("local connection survived a failed revoke")
	}
}

func TestDisconnectIsIdempotent(t *testing.T) {
	st := &fakeStore{}
	svc := newTestService(t, st, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("nothing to revoke")
	}))
	if err := svc.Disconnect(context.Background(), "host-1"); err != nil {
		t.Fatalf("disconnecting an unconnected host should be a no-op, got %v", err)
	}
}

func TestStatusReportsReconnectNeeded(t *testing.T) {
	st := &fakeStore{}
	svc := newTestService(t, st, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	conn := liveConn(t, svc)
	now := time.Now().UTC()
	conn.RevokedAt = &now
	msg := "google revoked access"
	conn.LastError = &msg
	email := "host@example.com"
	conn.AccountEmail = &email
	st.conn = conn

	got, err := svc.Status(context.Background(), "host-1")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if got.Connected {
		t.Error("a revoked connection must not report connected")
	}
	if !got.NeedsReconnect {
		t.Error("needsReconnect should be true so the dashboard prompts")
	}
	if got.AccountEmail != "host@example.com" {
		t.Errorf("account email = %q", got.AccountEmail)
	}
}

func TestStatusUnconnected(t *testing.T) {
	svc := newTestService(t, &fakeStore{}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	got, err := svc.Status(context.Background(), "host-1")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if got.Connected || got.NeedsReconnect {
		t.Fatalf("unconnected host reported as %+v", got)
	}
}

func TestAuthURLCarriesState(t *testing.T) {
	svc := newTestService(t, &fakeStore{}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	u, err := svc.AuthURL("host-7", "dashboard")
	if err != nil {
		t.Fatalf("auth url: %v", err)
	}
	if !strings.Contains(u, "state="+statePurpose+"%3A%2Fdashboard%3Ahost-7") {
		t.Fatalf("state not carried: %s", u)
	}
}

// ─── Return destination allowlist ────────────────────────────────────────────

// The return value survives a round-trip through Google, so anything outside
// the closed set must collapse to the default. An open redirect here would
// turn "connect your calendar" into a phishing primitive.
func TestResolveReturnIsClosed(t *testing.T) {
	if got := ResolveReturn("onboarding"); got != "/onboarding" {
		t.Errorf("onboarding -> %q", got)
	}
	if got := ResolveReturn("dashboard"); got != "/dashboard" {
		t.Errorf("dashboard -> %q", got)
	}
	for _, hostile := range []string{
		"", "unknown", "//evil.example", "https://evil.example",
		"/dashboard/../../etc", "javascript:alert(1)", "/onboarding?x=1",
	} {
		if got := ResolveReturn(hostile); got != "/dashboard" {
			t.Errorf("ResolveReturn(%q) = %q, want the default /dashboard", hostile, got)
		}
	}
}

// Even a validly signed token gets its destination re-checked, because the
// allowlist may have shrunk since the token was minted.
func TestCompleteRejectsUnlistedReturnPath(t *testing.T) {
	st := &fakeStore{}
	svc := newTestService(t, st, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "at", "refresh_token": "rt", "expires_in": 3600,
		})
	}))
	// stubSigner encodes the return path verbatim, standing in for a token
	// signed when a now-removed destination was still allowed.
	_, returnTo, err := svc.Complete(context.Background(), statePurpose+":https://evil.example:host-1", "code")
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if returnTo != "/dashboard" {
		t.Fatalf("returnTo = %q, want the default /dashboard", returnTo)
	}
}

func TestReturnPathWithoutExchange(t *testing.T) {
	svc := newTestService(t, &fakeStore{}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("ReturnPath must not call Google")
	}))
	if got := svc.ReturnPath(statePurpose + ":/onboarding:host-1"); got != "/onboarding" {
		t.Fatalf("ReturnPath = %q", got)
	}
	if got := svc.ReturnPath(""); got != "" {
		t.Fatalf("empty state -> %q, want \"\"", got)
	}
	if got := svc.ReturnPath("garbage"); got != "" {
		t.Fatalf("unverifiable state -> %q, want \"\"", got)
	}
}

func TestRedirectToBuildsAbsoluteURL(t *testing.T) {
	svc := newTestService(t, &fakeStore{}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	if got := svc.RedirectTo("/onboarding"); got != "https://app.test/onboarding" {
		t.Errorf("RedirectTo(/onboarding) = %q", got)
	}
	if got := svc.RedirectTo(""); got != "https://app.test/dashboard" {
		t.Errorf("RedirectTo(\"\") = %q, want the default", got)
	}
}
