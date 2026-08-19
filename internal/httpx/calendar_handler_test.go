package httpx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vaishnavghenge/vartalaap-server/internal/calendar"
	"github.com/vaishnavghenge/vartalaap-server/internal/store"
)

// ─── Test doubles ────────────────────────────────────────────────────────────

// fakeBusy stands in for a connected Google Calendar. `err` simulates Google
// being unreachable, which is the case the degradation logic exists for.
type fakeBusy struct {
	mu       sync.Mutex
	busy     []calendar.Interval
	err      error
	calls    int
	lastFrom time.Time
	lastTo   time.Time
}

func (f *fakeBusy) BusyPeriods(_ context.Context, _ string, fromUTC, toUTC time.Time) ([]calendar.Interval, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.lastFrom, f.lastTo = fromUTC, toUTC
	if f.err != nil {
		return nil, f.err
	}
	return f.busy, nil
}

func (f *fakeBusy) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

type fakeSync struct {
	mu        sync.Mutex
	created   []calendar.BookingEvent
	cancelled []string
}

func (f *fakeSync) SyncBookingCreated(_ context.Context, in calendar.BookingEvent) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created = append(f.created, in)
}

func (f *fakeSync) SyncBookingCancelled(_ context.Context, _, bookingID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancelled = append(f.cancelled, bookingID)
}

func (f *fakeSync) Created() []calendar.BookingEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]calendar.BookingEvent(nil), f.created...)
}

func (f *fakeSync) Cancelled() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.cancelled...)
}

// fakeConnector stands in for calendar.Service in the /me/calendar route tests.
type fakeConnector struct {
	authURL     string
	authErr     error
	completeErr error
	completedAs string
	disconnects int
	status      calendar.Status
	statusErr   error
}

func (f *fakeConnector) AuthURL(string) (string, error) { return f.authURL, f.authErr }
func (f *fakeConnector) Complete(_ context.Context, _, _ string) (string, error) {
	return f.completedAs, f.completeErr
}
func (f *fakeConnector) Disconnect(context.Context, string) error { f.disconnects++; return nil }
func (f *fakeConnector) Status(context.Context, string) (calendar.Status, error) {
	return f.status, f.statusErr
}
func (f *fakeConnector) SuccessRedirect() string { return "https://app.test/dashboard" }
func (f *fakeConnector) FailureRedirect() string { return "https://app.test/dashboard" }

// ─── Busy overlay in slot generation ─────────────────────────────────────────

// busySlotsFixture seeds a host bookable 09:00-10:00 UTC on the next Monday
// (two 30-minute slots) and returns the /slots URL for that day.
func busySlotsFixture(t *testing.T, st *memStore) (slotsURL string, monday time.Time) {
	t.Helper()
	host, event, _, _ := bookingFixture(t, st, "busyhost@example.com")
	monday = nextWeekday(time.Monday)
	if _, err := st.ReplaceAvailability(context.Background(), host.ID,
		[]store.AvailabilityRule{rule(1, "09:00", "10:00", "UTC")}); err != nil {
		t.Fatalf("seed availability: %v", err)
	}
	slotsURL = fmt.Sprintf("/u/%s/%s/slots?from=%s&to=%s",
		host.Slug, event.Slug,
		monday.Format("2006-01-02"),
		monday.AddDate(0, 0, 1).Format("2006-01-02"))
	return slotsURL, monday
}

func fetchSlots(t *testing.T, st *memStore, deps BookingDeps, url string) slotsResponse {
	t.Helper()
	rec := httptest.NewRecorder()
	dispatchPublicWithDeps(st, deps, rec, publicReq(http.MethodGet, url))
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var got slotsResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return got
}

// The headline behaviour of Phase 3: a meeting in the host's Google Calendar
// removes the overlapping Sessionly slot.
func TestPublicSlots_ExcludesGoogleBusyPeriods(t *testing.T) {
	st := newMemStore()
	url, monday := busySlotsFixture(t, st)

	busy := &fakeBusy{busy: []calendar.Interval{{
		Start: time.Date(monday.Year(), monday.Month(), monday.Day(), 9, 0, 0, 0, time.UTC),
		End:   time.Date(monday.Year(), monday.Month(), monday.Day(), 9, 30, 0, 0, time.UTC),
	}}}

	got := fetchSlots(t, st, BookingDeps{Busy: busy}, url)
	if len(got.Slots) != 1 {
		t.Fatalf("want 1 slot left after a 09:00 Google meeting, got %d: %v", len(got.Slots), got.Slots)
	}
	if !strings.Contains(got.Slots[0], "09:30") {
		t.Fatalf("remaining slot should be 09:30, got %s", got.Slots[0])
	}
	if got.CalendarSyncDegraded {
		t.Error("a successful lookup must not report degraded")
	}
	if busy.Calls() != 1 {
		t.Errorf("busy lookups = %d, want 1", busy.Calls())
	}
}

func TestPublicSlots_BusyPeriodOutsideWindowKeepsSlots(t *testing.T) {
	st := newMemStore()
	url, monday := busySlotsFixture(t, st)
	busy := &fakeBusy{busy: []calendar.Interval{{
		Start: time.Date(monday.Year(), monday.Month(), monday.Day(), 14, 0, 0, 0, time.UTC),
		End:   time.Date(monday.Year(), monday.Month(), monday.Day(), 15, 0, 0, 0, time.UTC),
	}}}
	got := fetchSlots(t, st, BookingDeps{Busy: busy}, url)
	if len(got.Slots) != 2 {
		t.Fatalf("an afternoon meeting must not touch morning slots, got %v", got.Slots)
	}
}

// Google being down must not take the booking page down with it. The slots
// still render, and the response says the data may be stale.
func TestPublicSlots_DegradesWhenCalendarUnreachable(t *testing.T) {
	st := newMemStore()
	url, _ := busySlotsFixture(t, st)
	busy := &fakeBusy{err: errors.New("google is down")}

	got := fetchSlots(t, st, BookingDeps{Busy: busy}, url)
	if len(got.Slots) != 2 {
		t.Fatalf("slots must still be served when the calendar lookup fails, got %v", got.Slots)
	}
	if !got.CalendarSyncDegraded {
		t.Fatal("calendarSyncDegraded must be set — a silent degrade is the bug this flag prevents")
	}
}

// No calendar configured is the pre-Phase-3 path and must be untouched.
func TestPublicSlots_NoCalendarConfigured(t *testing.T) {
	st := newMemStore()
	url, _ := busySlotsFixture(t, st)
	got := fetchSlots(t, st, BookingDeps{}, url)
	if len(got.Slots) != 2 {
		t.Fatalf("want 2 slots, got %v", got.Slots)
	}
	if got.CalendarSyncDegraded {
		t.Error("no connection is not a degraded state")
	}
}

// ─── Busy overlay at booking time ────────────────────────────────────────────

func TestCreateBooking_RejectsGoogleBusyConflict(t *testing.T) {
	st := newMemStore()
	host, event, guest, _ := bookingFixture(t, st, "conflict@example.com")
	start := time.Now().UTC().Add(90 * time.Minute).Truncate(time.Minute)

	busy := &fakeBusy{busy: []calendar.Interval{{
		Start: start.Add(-10 * time.Minute),
		End:   start.Add(10 * time.Minute),
	}}}
	body := fmt.Sprintf(`{"hostSlug":%q,"eventTypeSlug":%q,"startsAt":%q,
		"guestName":"Pat","guestEmail":"pat@example.com"}`,
		host.Slug, event.Slug, start.Format(time.RFC3339))

	rec := httptest.NewRecorder()
	handleCreateBooking(st, BookingDeps{Busy: busy})(rec, guest(http.MethodPost, "/bookings", body))
	if rec.Code != http.StatusConflict {
		t.Fatalf("want 409 for a slot the host is busy in, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "SLOT_TAKEN") {
		t.Fatalf("want SLOT_TAKEN code, got %s", rec.Body.String())
	}
}

// Fail-open: a Google outage must not stop bookings. Losing real bookings to
// someone else's downtime is worse than the double-booking risk, and the host
// is emailed either way.
func TestCreateBooking_SucceedsWhenCalendarUnreachable(t *testing.T) {
	st := newMemStore()
	host, event, guest, _ := bookingFixture(t, st, "degraded@example.com")
	busy := &fakeBusy{err: errors.New("google is down")}

	body := fmt.Sprintf(`{"hostSlug":%q,"eventTypeSlug":%q,"startsAt":%q,
		"guestName":"Pat","guestEmail":"pat@example.com"}`,
		host.Slug, event.Slug, futureRFC3339(90))

	rec := httptest.NewRecorder()
	handleCreateBooking(st, BookingDeps{Busy: busy})(rec, guest(http.MethodPost, "/bookings", body))
	if rec.Code != http.StatusCreated {
		t.Fatalf("want 201 despite the calendar being down, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ─── Write-back triggers ─────────────────────────────────────────────────────

func TestCreateBooking_MirrorsToCalendar(t *testing.T) {
	st := newMemStore()
	host, event, guest, _ := bookingFixture(t, st, "mirror@example.com")
	sync := &fakeSync{}
	deps := BookingDeps{CalendarSync: sync, PublicAppURL: "https://app.test"}

	body := fmt.Sprintf(`{"hostSlug":%q,"eventTypeSlug":%q,"startsAt":%q,
		"guestName":"Pat Guest","guestEmail":"pat@example.com"}`,
		host.Slug, event.Slug, futureRFC3339(90))

	rec := httptest.NewRecorder()
	handleCreateBooking(st, deps)(rec, guest(http.MethodPost, "/bookings", body))
	if rec.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", rec.Code, rec.Body.String())
	}
	created := sync.Created()
	if len(created) != 1 {
		t.Fatalf("want 1 calendar sync, got %d", len(created))
	}
	got := created[0]
	if got.EventTitle != event.Title || got.GuestEmail != "pat@example.com" {
		t.Fatalf("unexpected sync payload: %+v", got)
	}
	// The host clicks through from their own calendar, so the link has to be
	// the same absolute room URL the guest gets by email.
	if !strings.HasPrefix(got.RoomURL, "https://app.test/room/") {
		t.Fatalf("room URL = %q", got.RoomURL)
	}
	if !strings.HasSuffix(got.RoomURL, got.MeetCode) {
		t.Fatalf("room URL %q does not end in meet code %q", got.RoomURL, got.MeetCode)
	}
}

// A calendar sync failure must never surface to the guest: the booking is
// already persisted and they have the meet link.
func TestCreateBooking_CalendarPanicFreeWithoutSync(t *testing.T) {
	st := newMemStore()
	host, event, guest, _ := bookingFixture(t, st, "nosync@example.com")
	body := fmt.Sprintf(`{"hostSlug":%q,"eventTypeSlug":%q,"startsAt":%q,
		"guestName":"Pat","guestEmail":"pat@example.com"}`,
		host.Slug, event.Slug, futureRFC3339(90))
	rec := httptest.NewRecorder()
	// Nil CalendarSync is the unconfigured deployment.
	handleCreateBooking(st, BookingDeps{})(rec, guest(http.MethodPost, "/bookings", body))
	if rec.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestGuestCancel_RemovesCalendarEvent(t *testing.T) {
	st := newMemStore()
	host, event, guest, _ := bookingFixture(t, st, "cancelsync@example.com")
	sync := &fakeSync{}
	deps := BookingDeps{CalendarSync: sync, PublicAppURL: "https://app.test"}

	body := fmt.Sprintf(`{"hostSlug":%q,"eventTypeSlug":%q,"startsAt":%q,
		"guestName":"Pat","guestEmail":"pat@example.com"}`,
		host.Slug, event.Slug, futureRFC3339(90))
	rec := httptest.NewRecorder()
	handleCreateBooking(st, deps)(rec, guest(http.MethodPost, "/bookings", body))
	if rec.Code != http.StatusCreated {
		t.Fatalf("setup booking failed: %d %s", rec.Code, rec.Body.String())
	}
	var created bookingDTO
	_ = json.NewDecoder(rec.Body).Decode(&created)

	b, err := st.GetBookingByID(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("load booking: %v", err)
	}

	rec2 := httptest.NewRecorder()
	dispatchPublicWithDeps(st, deps, rec2, publicReqWithBody(http.MethodDelete,
		"/m/"+b.MeetCode+"?t="+b.CancelToken, `{"reason":"Schedule changed"}`))
	if rec2.Code != http.StatusNoContent {
		t.Fatalf("cancel: want 204, got %d: %s", rec2.Code, rec2.Body.String())
	}
	cancelled := sync.Cancelled()
	if len(cancelled) != 1 || cancelled[0] != created.ID {
		t.Fatalf("want the cancelled booking mirrored for deletion, got %v", cancelled)
	}
}

// ─── /me/calendar routes ─────────────────────────────────────────────────────

func calendarMux(svc calendar.Connector) *http.ServeMux {
	mux := http.NewServeMux()
	CalendarHandlers(mux, testCfg(), svc)
	return mux
}

func TestCalendarStatus_RequiresAuth(t *testing.T) {
	rec := httptest.NewRecorder()
	req := authReq(http.MethodGet, "/me/calendar/status", "")
	calendarMux(&fakeConnector{}).ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401 without a token, got %d", rec.Code)
	}
}

func TestCalendarStatus_ReportsConnection(t *testing.T) {
	st := newMemStore()
	registerUser(t, st, "cal@example.com", "password123")
	u, _ := st.GetUserByEmail(context.Background(), "cal@example.com")

	svc := &fakeConnector{status: calendar.Status{
		Connected: true, AccountEmail: "host@gmail.com", CalendarID: "primary",
	}}
	req := authReq(http.MethodGet, "/me/calendar/status", "")
	req.Header.Set("Authorization", "Bearer "+tokenForUser(t, u.ID))

	rec := httptest.NewRecorder()
	calendarMux(svc).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var got calendarStatusResponse
	_ = json.NewDecoder(rec.Body).Decode(&got)
	if !got.Connected || !got.Available || got.AccountEmail != "host@gmail.com" {
		t.Fatalf("unexpected status: %+v", got)
	}
}

// When the server has no Google credentials the UI must be able to hide the
// section rather than offer a button that cannot work.
func TestCalendarStatus_UnavailableWhenUnconfigured(t *testing.T) {
	st := newMemStore()
	registerUser(t, st, "cal2@example.com", "password123")
	u, _ := st.GetUserByEmail(context.Background(), "cal2@example.com")

	req := authReq(http.MethodGet, "/me/calendar/status", "")
	req.Header.Set("Authorization", "Bearer "+tokenForUser(t, u.ID))
	rec := httptest.NewRecorder()
	calendarMux(nil).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	var got calendarStatusResponse
	_ = json.NewDecoder(rec.Body).Decode(&got)
	if got.Available || got.Connected {
		t.Fatalf("unconfigured server should report available=false: %+v", got)
	}
}

func TestCalendarConnect_ReturnsAuthURL(t *testing.T) {
	st := newMemStore()
	registerUser(t, st, "cal3@example.com", "password123")
	u, _ := st.GetUserByEmail(context.Background(), "cal3@example.com")

	svc := &fakeConnector{authURL: "https://accounts.google.com/o/oauth2/v2/auth?state=x"}
	req := authReq(http.MethodGet, "/me/calendar/connect/google", "")
	req.Header.Set("Authorization", "Bearer "+tokenForUser(t, u.ID))
	rec := httptest.NewRecorder()
	calendarMux(svc).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var got calendarConnectResponse
	_ = json.NewDecoder(rec.Body).Decode(&got)
	if got.AuthURL != svc.authURL {
		t.Fatalf("authUrl = %q", got.AuthURL)
	}
}

// The callback is a browser navigation with no Authorization header. It must
// work unauthenticated (identity comes from the signed state) and end in a
// redirect a human can see.
func TestCalendarCallback_RedirectsOnSuccess(t *testing.T) {
	svc := &fakeConnector{completedAs: "user-1"}
	req := httptest.NewRequest(http.MethodGet, "/me/calendar/callback/google?code=c&state=s", nil)
	rec := httptest.NewRecorder()
	calendarMux(svc).ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("want 302, got %d: %s", rec.Code, rec.Body.String())
	}
	loc := rec.Header().Get("Location")
	if !strings.Contains(loc, "calendar=connected") {
		t.Fatalf("redirect should report the outcome, got %q", loc)
	}
}

func TestCalendarCallback_HandlesConsentDenied(t *testing.T) {
	svc := &fakeConnector{}
	req := httptest.NewRequest(http.MethodGet, "/me/calendar/callback/google?error=access_denied", nil)
	rec := httptest.NewRecorder()
	calendarMux(svc).ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("want 302, got %d", rec.Code)
	}
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "calendar=denied") {
		t.Fatalf("location = %q", loc)
	}
}

// Google's error text must never reach the redirect URL — only enumerated
// reason codes do.
func TestCalendarCallback_DoesNotReflectGoogleErrorText(t *testing.T) {
	svc := &fakeConnector{}
	req := httptest.NewRequest(http.MethodGet,
		"/me/calendar/callback/google?error=%3Cscript%3Ealert(1)%3C/script%3E", nil)
	rec := httptest.NewRecorder()
	calendarMux(svc).ServeHTTP(rec, req)

	if loc := rec.Header().Get("Location"); strings.Contains(loc, "script") {
		t.Fatalf("google error text was reflected into the redirect: %q", loc)
	}
}

func TestCalendarCallback_FailedCompletionRedirects(t *testing.T) {
	svc := &fakeConnector{completeErr: errors.New("bad state")}
	req := httptest.NewRequest(http.MethodGet, "/me/calendar/callback/google?code=c&state=forged", nil)
	rec := httptest.NewRecorder()
	calendarMux(svc).ServeHTTP(rec, req)

	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "calendar=connect_failed") {
		t.Fatalf("location = %q", loc)
	}
}

func TestCalendarDisconnect(t *testing.T) {
	st := newMemStore()
	registerUser(t, st, "cal4@example.com", "password123")
	u, _ := st.GetUserByEmail(context.Background(), "cal4@example.com")

	svc := &fakeConnector{}
	req := authReq(http.MethodDelete, "/me/calendar/disconnect", "")
	req.Header.Set("Authorization", "Bearer "+tokenForUser(t, u.ID))
	rec := httptest.NewRecorder()
	calendarMux(svc).ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("want 204, got %d: %s", rec.Code, rec.Body.String())
	}
	if svc.disconnects != 1 {
		t.Fatalf("disconnect calls = %d", svc.disconnects)
	}
}

func TestCalendarUnknownAction404s(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/me/calendar/nope", nil)
	rec := httptest.NewRecorder()
	calendarMux(&fakeConnector{}).ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", rec.Code)
	}
}
