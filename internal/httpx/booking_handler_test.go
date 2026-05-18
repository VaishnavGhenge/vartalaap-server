package httpx

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vaishnavghenge/vartalaap-server/internal/plans"
	"github.com/vaishnavghenge/vartalaap-server/internal/store"
)

// bookingFixture provisions a host + one active event type and returns helpers
// to (a) hit the public POST /bookings endpoint as a guest and (b) hit the
// authed /me/bookings as the host.
func bookingFixture(t *testing.T, st *memStore, hostEmail string) (hostUser *store.User, event *store.EventType, guestReq func(method, path, body string) *http.Request, hostReq func(method, path, body string) *http.Request) {
	t.Helper()
	hostEmail = strings.ToLower(hostEmail)
	registerUser(t, st, hostEmail, "password123")
	u, err := st.GetUserByEmail(context.Background(), hostEmail)
	if err != nil {
		t.Fatalf("fixture: GetUserByEmail: %v", err)
	}
	// Give the host a deterministic slug to mirror the public URL contract.
	u.Slug = "host-" + u.ID
	st.users[u.ID] = u

	created, err := st.CreateEventType(context.Background(), store.EventType{
		HostID:        u.ID,
		Slug:          "intro",
		Title:         "Intro call",
		DurationMin:   30,
		BufferMin:     0,
		IsPaid:        false,
		Currency:      "usd",
		PaymentTiming: "upfront",
		IsActive:      true,
	})
	if err != nil {
		t.Fatalf("fixture: CreateEventType: %v", err)
	}

	tok := tokenForUser(t, u.ID)
	guestReq = func(method, path, body string) *http.Request {
		return authReq(method, path, body)
	}
	hostReq = func(method, path, body string) *http.Request {
		req := authReq(method, path, body)
		req.Header.Set("Authorization", "Bearer "+tok)
		return req
	}
	return u, created, guestReq, hostReq
}

func futureRFC3339(minutesFromNow int) string {
	return time.Now().UTC().Add(time.Duration(minutesFromNow) * time.Minute).Format(time.RFC3339)
}

// dispatchBookings routes a /bookings or /bookings/{id} or /me/bookings
// request to the appropriate handler — same pattern as dispatchEventTypes,
// avoids spinning up a full mux for handler-layer tests.
func dispatchBookings(st store.Storer, w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/bookings" && r.Method == http.MethodPost {
		handleCreateBooking(st, BookingDeps{})(w, r)
		return
	}
	if r.URL.Path == "/me/bookings" && r.Method == http.MethodGet {
		RequireAuth(testSecret, handleListMyBookings(st, BookingRoomWindow{}))(w, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/bookings/") && r.Method == http.MethodGet {
		id := strings.TrimPrefix(r.URL.Path, "/bookings/")
		handleGetBooking(st, BookingDeps{}, id)(w, r)
		return
	}
	http.Error(w, "not found", http.StatusNotFound)
}

// ─── POST /bookings ───────────────────────────────────────────────────────────

func TestCreateBooking_HappyPath(t *testing.T) {
	st := newMemStore()
	host, event, guest, _ := bookingFixture(t, st, "host@example.com")

	body := fmt.Sprintf(`{
		"hostSlug": %q, "eventTypeSlug": %q,
		"startsAt": %q,
		"guestName": "Pat Guest", "guestEmail": "pat@example.com"
	}`, host.Slug, event.Slug, futureRFC3339(60))

	rec := httptest.NewRecorder()
	dispatchBookings(st, rec, guest(http.MethodPost, "/bookings", body))
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var got bookingDTO
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ID == "" || got.MeetCode == "" {
		t.Fatalf("expected id and meet code, got %+v", got)
	}
	if got.Status != "confirmed" {
		t.Fatalf("expected status=confirmed, got %q", got.Status)
	}
	if got.EventTitle != event.Title {
		t.Fatalf("expected event title %q, got %q", event.Title, got.EventTitle)
	}
	if got.EndsAt.Sub(got.StartsAt) != 30*time.Minute {
		t.Fatalf("expected 30m duration, got %v", got.EndsAt.Sub(got.StartsAt))
	}
}

func TestCreateBooking_RejectsPastStart(t *testing.T) {
	st := newMemStore()
	host, event, guest, _ := bookingFixture(t, st, "host@example.com")

	body := fmt.Sprintf(`{
		"hostSlug": %q, "eventTypeSlug": %q,
		"startsAt": %q,
		"guestName": "Pat", "guestEmail": "pat@example.com"
	}`, host.Slug, event.Slug, futureRFC3339(-30))

	rec := httptest.NewRecorder()
	dispatchBookings(st, rec, guest(http.MethodPost, "/bookings", body))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for past startsAt, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "future") {
		t.Fatalf("expected 'future' in body, got %q", rec.Body.String())
	}
}

func TestCreateBooking_RejectsInvalidEmail(t *testing.T) {
	st := newMemStore()
	host, event, guest, _ := bookingFixture(t, st, "host@example.com")

	body := fmt.Sprintf(`{
		"hostSlug": %q, "eventTypeSlug": %q,
		"startsAt": %q,
		"guestName": "Pat", "guestEmail": "not-an-email"
	}`, host.Slug, event.Slug, futureRFC3339(60))

	rec := httptest.NewRecorder()
	dispatchBookings(st, rec, guest(http.MethodPost, "/bookings", body))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid email, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestCreateBooking_RejectsInactiveEvent(t *testing.T) {
	st := newMemStore()
	host, event, guest, _ := bookingFixture(t, st, "host@example.com")
	// Soft-delete the event before the booking attempt.
	if err := st.DeleteEventType(context.Background(), host.ID, event.ID); err != nil {
		t.Fatalf("DeleteEventType: %v", err)
	}

	body := fmt.Sprintf(`{
		"hostSlug": %q, "eventTypeSlug": %q,
		"startsAt": %q,
		"guestName": "Pat", "guestEmail": "pat@example.com"
	}`, host.Slug, event.Slug, futureRFC3339(60))

	rec := httptest.NewRecorder()
	dispatchBookings(st, rec, guest(http.MethodPost, "/bookings", body))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for inactive event, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestCreateBooking_UnknownHostIs404(t *testing.T) {
	st := newMemStore()
	_, event, guest, _ := bookingFixture(t, st, "host@example.com")

	// Note: hostSlug doesn't exist. We must return 404 — same as event-not-found
	// — so an attacker can't enumerate real host slugs.
	body := fmt.Sprintf(`{
		"hostSlug": "no-such-host", "eventTypeSlug": %q,
		"startsAt": %q,
		"guestName": "Pat", "guestEmail": "pat@example.com"
	}`, event.Slug, futureRFC3339(60))

	rec := httptest.NewRecorder()
	dispatchBookings(st, rec, guest(http.MethodPost, "/bookings", body))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for unknown host, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestCreateBooking_FreeHostMonthlyLimit(t *testing.T) {
	st := newMemStore()
	host, event, guest, _ := bookingFixture(t, st, "free@example.com")

	// Pre-fill the host's bookings for this month right up to the cap.
	now := time.Now().UTC()
	for i := 0; i < plans.For(plans.Free).MonthlyBookings; i++ {
		_, err := st.CreateBooking(context.Background(), store.Booking{
			EventTypeID: event.ID,
			HostID:      host.ID,
			GuestEmail:  fmt.Sprintf("g%d@example.com", i),
			GuestName:   fmt.Sprintf("Guest %d", i),
			StartsAt:    now.Add(time.Duration(i+1) * 24 * time.Hour),
			EndsAt:      now.Add(time.Duration(i+1)*24*time.Hour + 30*time.Minute),
			MeetCode:    fmt.Sprintf("pre-fill-%02d", i),
			Status:      "confirmed",
		})
		if err != nil {
			t.Fatalf("seed booking %d: %v", i, err)
		}
	}

	// 11th booking attempt — must 403 with the host-cap message.
	body := fmt.Sprintf(`{
		"hostSlug": %q, "eventTypeSlug": %q,
		"startsAt": %q,
		"guestName": "Cap Test", "guestEmail": "cap@example.com"
	}`, host.Slug, event.Slug, futureRFC3339(120))

	rec := httptest.NewRecorder()
	dispatchBookings(st, rec, guest(http.MethodPost, "/bookings", body))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 at free cap, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestCreateBooking_SoloHostNoCap(t *testing.T) {
	st := newMemStore()
	host, event, guest, _ := bookingFixture(t, st, "solo@example.com")
	setPlan(t, st, host.ID, "solo")

	// Pre-fill past the free cap.
	now := time.Now().UTC()
	for i := 0; i < plans.For(plans.Free).MonthlyBookings+1; i++ {
		_, err := st.CreateBooking(context.Background(), store.Booking{
			EventTypeID: event.ID, HostID: host.ID,
			GuestEmail: fmt.Sprintf("g%d@example.com", i), GuestName: "G",
			StartsAt: now.Add(time.Duration(i+1) * 24 * time.Hour),
			EndsAt:   now.Add(time.Duration(i+1)*24*time.Hour + 30*time.Minute),
			MeetCode: fmt.Sprintf("solo-%02d", i), Status: "confirmed",
		})
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	body := fmt.Sprintf(`{
		"hostSlug": %q, "eventTypeSlug": %q,
		"startsAt": %q,
		"guestName": "Solo Test", "guestEmail": "s@example.com"
	}`, host.Slug, event.Slug, futureRFC3339(120))

	rec := httptest.NewRecorder()
	dispatchBookings(st, rec, guest(http.MethodPost, "/bookings", body))
	if rec.Code != http.StatusCreated {
		t.Fatalf("solo plan must not hit free cap, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ─── GET /bookings/{id} ───────────────────────────────────────────────────────

func TestGetBooking_PublicConfirmation(t *testing.T) {
	st := newMemStore()
	host, event, guest, _ := bookingFixture(t, st, "host@example.com")

	// Seed a booking through the handler so the response shape comes from the
	// real path, not a direct store insert.
	createBody := fmt.Sprintf(`{
		"hostSlug": %q, "eventTypeSlug": %q,
		"startsAt": %q,
		"guestName": "Pat", "guestEmail": "pat@example.com"
	}`, host.Slug, event.Slug, futureRFC3339(60))
	createRec := httptest.NewRecorder()
	dispatchBookings(st, createRec, guest(http.MethodPost, "/bookings", createBody))
	if createRec.Code != http.StatusCreated {
		t.Fatalf("seed: %d %s", createRec.Code, createRec.Body.String())
	}
	var created bookingDTO
	json.NewDecoder(createRec.Body).Decode(&created)

	rec := httptest.NewRecorder()
	dispatchBookings(st, rec, guest(http.MethodGet, "/bookings/"+created.ID, ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var got bookingDTO
	json.NewDecoder(rec.Body).Decode(&got)
	if got.ID != created.ID || got.MeetCode != created.MeetCode {
		t.Fatalf("get returned wrong booking: %+v", got)
	}
}

func TestGetBooking_NotFound(t *testing.T) {
	st := newMemStore()
	_, _, guest, _ := bookingFixture(t, st, "host@example.com")

	rec := httptest.NewRecorder()
	dispatchBookings(st, rec, guest(http.MethodGet, "/bookings/booking-99999", ""))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

// ─── GET /me/bookings ─────────────────────────────────────────────────────────

func TestListMyBookings_ScopedToHost(t *testing.T) {
	st := newMemStore()
	hostA, eventA, _, hostAReq := bookingFixture(t, st, "a@example.com")
	hostB, eventB, _, _ := bookingFixture(t, st, "b@example.com")

	// One booking for each host. Each call to CreateBooking returns a *store.Booking;
	// using the real store path keeps the seed truthful to the wire shape.
	now := time.Now().UTC()
	if _, err := st.CreateBooking(context.Background(), store.Booking{
		EventTypeID: eventA.ID, HostID: hostA.ID,
		GuestEmail: "g1@example.com", GuestName: "G1",
		StartsAt: now.Add(2 * time.Hour),
		EndsAt:   now.Add(2*time.Hour + 30*time.Minute),
		MeetCode: "abc-defgh-ijk", Status: "confirmed",
	}); err != nil {
		t.Fatalf("seed A: %v", err)
	}
	if _, err := st.CreateBooking(context.Background(), store.Booking{
		EventTypeID: eventB.ID, HostID: hostB.ID,
		GuestEmail: "g2@example.com", GuestName: "G2",
		StartsAt: now.Add(3 * time.Hour),
		EndsAt:   now.Add(3*time.Hour + 30*time.Minute),
		MeetCode: "mno-pqrst-uvw", Status: "confirmed",
	}); err != nil {
		t.Fatalf("seed B: %v", err)
	}

	rec := httptest.NewRecorder()
	dispatchBookings(st, rec, hostAReq(http.MethodGet, "/me/bookings", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp bookingListResponse
	json.NewDecoder(rec.Body).Decode(&resp)
	if len(resp.Bookings) != 1 {
		t.Fatalf("host A expected exactly 1 booking, got %d: %+v", len(resp.Bookings), resp.Bookings)
	}
	if resp.Bookings[0].MeetCode != "abc-defgh-ijk" {
		t.Fatalf("wrong booking surfaced: %+v", resp.Bookings[0])
	}
}

func TestListMyBookings_RequiresAuth(t *testing.T) {
	st := newMemStore()
	_, _, guest, _ := bookingFixture(t, st, "host@example.com")

	rec := httptest.NewRecorder()
	dispatchBookings(st, rec, guest(http.MethodGet, "/me/bookings", ""))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestListMyBookings_FiltersPastAndCancelled(t *testing.T) {
	st := newMemStore()
	host, event, _, hostReq := bookingFixture(t, st, "host@example.com")
	now := time.Now().UTC()

	// One past booking — should NOT appear.
	if _, err := st.CreateBooking(context.Background(), store.Booking{
		EventTypeID: event.ID, HostID: host.ID,
		GuestEmail: "past@example.com", GuestName: "Past",
		StartsAt: now.Add(-1 * time.Hour),
		EndsAt:   now.Add(-30 * time.Minute),
		MeetCode: "past-xxxxx-yyy", Status: "confirmed",
	}); err != nil {
		t.Fatalf("seed past: %v", err)
	}
	// One cancelled future booking — should NOT appear.
	cancelled, err := st.CreateBooking(context.Background(), store.Booking{
		EventTypeID: event.ID, HostID: host.ID,
		GuestEmail: "cxl@example.com", GuestName: "Cxl",
		StartsAt: now.Add(2 * time.Hour),
		EndsAt:   now.Add(2*time.Hour + 30*time.Minute),
		MeetCode: "cxl-xxxxx-yyy", Status: "confirmed",
	})
	if err != nil {
		t.Fatalf("seed cancelled: %v", err)
	}
	if err := st.CancelBooking(context.Background(), cancelled.ID, "No longer needed", "host"); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	// One live future booking — SHOULD appear.
	if _, err := st.CreateBooking(context.Background(), store.Booking{
		EventTypeID: event.ID, HostID: host.ID,
		GuestEmail: "live@example.com", GuestName: "Live",
		StartsAt: now.Add(3 * time.Hour),
		EndsAt:   now.Add(3*time.Hour + 30*time.Minute),
		MeetCode: "live-xxxxx-yyy", Status: "confirmed",
	}); err != nil {
		t.Fatalf("seed live: %v", err)
	}

	rec := httptest.NewRecorder()
	dispatchBookings(st, rec, hostReq(http.MethodGet, "/me/bookings", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var resp bookingListResponse
	json.NewDecoder(rec.Body).Decode(&resp)
	if len(resp.Bookings) != 1 {
		t.Fatalf("expected 1 booking, got %d: %+v", len(resp.Bookings), resp.Bookings)
	}
	if resp.Bookings[0].GuestName != "Live" {
		t.Fatalf("expected Live booking, got %+v", resp.Bookings[0])
	}
}
