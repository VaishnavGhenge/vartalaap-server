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

	"github.com/vaishnavghenge/vartalaap-server/internal/email"
	"github.com/vaishnavghenge/vartalaap-server/internal/store"
)

// recordingMailer captures every Send for assertion in booking-email tests.
// Thread-safe so multiple concurrent bookings (if a test ever exercises that)
// don't tear the recorded slice.
type recordingMailer struct {
	mu   sync.Mutex
	sent []email.Message
	err  error // when non-nil, returned from every Send to simulate failure
}

func (r *recordingMailer) Send(_ context.Context, msg email.Message) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return r.err
	}
	r.sent = append(r.sent, msg)
	return nil
}
func (r *recordingMailer) Messages() []email.Message {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]email.Message, len(r.sent))
	copy(out, r.sent)
	return out
}

// dispatchPublic mirrors dispatchBookings — exposes the public /u/ and /m/
// handlers under a thin router so individual tests stay focused on the
// handler's behaviour, not the routing glue. Production wiring lives in
// SlotHandlers() and is exercised by the build tests + the e2e walkthrough.
func dispatchPublic(st store.Storer, w http.ResponseWriter, r *http.Request) {
	dispatchPublicWithDeps(st, BookingDeps{}, w, r)
}

func dispatchPublicWithDeps(st store.Storer, deps BookingDeps, w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if strings.HasPrefix(path, "/m/") {
		code := strings.TrimPrefix(path, "/m/")
		switch r.Method {
		case http.MethodGet:
			handleGetBookingByMeetCode(st, code)(w, r)
		case http.MethodDelete:
			handleGuestCancelBooking(st, deps, code)(w, r)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
		return
	}
	if !strings.HasPrefix(path, "/u/") {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	parts := strings.Split(strings.TrimPrefix(path, "/u/"), "/")
	switch len(parts) {
	case 1:
		handleGetHostProfile(st, parts[0])(w, r)
	case 2:
		handleGetPublicEvent(st, parts[0], parts[1])(w, r)
	case 3:
		if parts[2] != "slots" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		handleListSlots(st, parts[0], parts[1])(w, r)
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

// publicReq builds a GET request with the API origin so origin checks (if any
// get added to the public routes later) keep passing without test churn.
func publicReq(method, path string) *http.Request {
	r := httptest.NewRequest(method, path, nil)
	r.Header.Set("Origin", "https://app.vartalaap.test")
	return r
}

// ─── generateSlots: pure function tests ──────────────────────────────────────

// midnightUTC builds a UTC date with hour/minute zeroed, the canonical form
// for "from"/"to" bounds in the test bodies.
func midnightUTC(year int, month time.Month, day int) time.Time {
	return time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
}

// rule constructs a single AvailabilityRule. The dow values use the wire
// numbering: 0=Sun..6=Sat.
func rule(dow int, start, end, tz string) store.AvailabilityRule {
	return store.AvailabilityRule{
		DayOfWeek: dow,
		StartTime: start,
		EndTime:   end,
		Timezone:  tz,
	}
}

// event30 is the canonical 30-minute non-paid event used across slot tests
// so they don't repeat the same struct literal.
func event30(buffer int) store.EventType {
	return store.EventType{
		ID:          "evt",
		DurationMin: 30,
		BufferMin:   buffer,
	}
}

func TestGenerateSlots_SingleWindowYieldsBackToBackSlots(t *testing.T) {
	// 2026-05-18 is a Monday in UTC; dow 1.
	from := midnightUTC(2026, 5, 18)
	to := midnightUTC(2026, 5, 19)
	now := midnightUTC(2026, 5, 17) // anything before `from` works

	rules := []store.AvailabilityRule{rule(1, "09:00", "11:00", "UTC")}
	got := generateSlots(rules, event30(0), nil, from, to, now)

	wantStarts := []string{"09:00", "09:30", "10:00", "10:30"}
	if len(got) != len(wantStarts) {
		t.Fatalf("want %d slots, got %d: %v", len(wantStarts), len(got), got)
	}
	for i, slot := range got {
		gotHHMM := slot.Format("15:04")
		if gotHHMM != wantStarts[i] {
			t.Fatalf("slot %d: want %s, got %s", i, wantStarts[i], gotHHMM)
		}
	}
}

func TestGenerateSlots_SplitShiftsLeaveGap(t *testing.T) {
	// Two rules on the same day: 09-11 and 13-15 (typical lunch break). The
	// generator should never produce a 11:00, 11:30, 12:00, 12:30 slot.
	from := midnightUTC(2026, 5, 18)
	to := midnightUTC(2026, 5, 19)
	rules := []store.AvailabilityRule{
		rule(1, "09:00", "11:00", "UTC"),
		rule(1, "13:00", "15:00", "UTC"),
	}
	got := generateSlots(rules, event30(0), nil, from, to, midnightUTC(2026, 5, 17))
	want := []string{"09:00", "09:30", "10:00", "10:30", "13:00", "13:30", "14:00", "14:30"}
	if len(got) != len(want) {
		t.Fatalf("want %d slots, got %d: %v", len(want), len(got), got)
	}
	for i, slot := range got {
		if slot.Format("15:04") != want[i] {
			t.Fatalf("slot %d: want %s, got %s", i, want[i], slot.Format("15:04"))
		}
	}
}

func TestGenerateSlots_SkipsPast(t *testing.T) {
	// 09:00 has already passed; only 09:30 and 10:00 should remain.
	from := midnightUTC(2026, 5, 18)
	to := midnightUTC(2026, 5, 19)
	now := time.Date(2026, 5, 18, 9, 15, 0, 0, time.UTC)
	rules := []store.AvailabilityRule{rule(1, "09:00", "10:30", "UTC")}
	got := generateSlots(rules, event30(0), nil, from, to, now)
	if len(got) != 2 {
		t.Fatalf("want 2 slots after now=09:15, got %d: %v", len(got), got)
	}
	if got[0].Format("15:04") != "09:30" || got[1].Format("15:04") != "10:00" {
		t.Fatalf("unexpected slots: %v", got)
	}
}

func TestGenerateSlots_DurationMustFit(t *testing.T) {
	// 10:50 wouldn't have time for a full 30m slot before 11:00 — drop it.
	from := midnightUTC(2026, 5, 18)
	to := midnightUTC(2026, 5, 19)
	rules := []store.AvailabilityRule{rule(1, "09:00", "11:00", "UTC")}
	got := generateSlots(rules, event30(0), nil, from, to, midnightUTC(2026, 5, 17))
	// 09:00, 09:30, 10:00, 10:30 — and not 11:00.
	last := got[len(got)-1]
	if last.Format("15:04") != "10:30" {
		t.Fatalf("last slot must be 10:30, got %s", last.Format("15:04"))
	}
}

func TestGenerateSlots_BufferAffectsCadenceWithoutBookings(t *testing.T) {
	// 30m event, 15m buffer, no existing bookings. Cadence must be 45m so a
	// guest who picks back-to-back slots still leaves the configured gap for
	// the host between sessions.
	from := midnightUTC(2026, 5, 18)
	to := midnightUTC(2026, 5, 19)
	rules := []store.AvailabilityRule{rule(1, "09:00", "12:00", "UTC")}
	got := generateSlots(rules, event30(15), nil, from, to, midnightUTC(2026, 5, 17))
	want := []string{"09:00", "09:45", "10:30", "11:15"}
	starts := make([]string, 0, len(got))
	for _, t := range got {
		starts = append(starts, t.Format("15:04"))
	}
	if len(starts) != len(want) {
		t.Fatalf("want cadence-spaced %v, got %v", want, starts)
	}
	for i, s := range starts {
		if s != want[i] {
			t.Fatalf("slot %d: want %s, got %s (all=%v)", i, want[i], s, starts)
		}
	}
}

func TestGenerateSlots_BookingBlocksSlotWithBuffer(t *testing.T) {
	// 30m event, 15m buffer → cadence 45m, so candidates are 09:00, 09:45,
	// 10:30, 11:15. Existing booking 10:00-10:30 buffered to 09:45-10:45
	// blocks the 09:45 (overlaps the pre-buffer) and 10:30 (overlaps the
	// post-buffer) candidates. 09:00 (ends 09:30, clear of 09:45) and 11:15
	// (starts after 10:45) remain.
	from := midnightUTC(2026, 5, 18)
	to := midnightUTC(2026, 5, 19)
	rules := []store.AvailabilityRule{rule(1, "09:00", "12:00", "UTC")}
	bookings := []store.Booking{{
		StartsAt: time.Date(2026, 5, 18, 10, 0, 0, 0, time.UTC),
		EndsAt:   time.Date(2026, 5, 18, 10, 30, 0, 0, time.UTC),
	}}
	got := generateSlots(rules, event30(15), bookings, from, to, midnightUTC(2026, 5, 17))
	starts := make([]string, 0, len(got))
	for _, t := range got {
		starts = append(starts, t.Format("15:04"))
	}
	want := []string{"09:00", "11:15"}
	if len(starts) != len(want) {
		t.Fatalf("want %v after buffered booking, got %v", want, starts)
	}
	for i, s := range starts {
		if s != want[i] {
			t.Fatalf("slot %d: want %s, got %s (all=%v)", i, want[i], s, starts)
		}
	}
}

func TestGenerateSlots_DSTSafeAcrossSpringForward(t *testing.T) {
	// 2026-03-08 02:00 → 03:00 in America/New_York (spring forward). A rule
	// of 09:00-12:00 NY time on a Sunday should still produce the right
	// number of slots regardless of the UTC clock shift.
	from := midnightUTC(2026, 3, 8)
	to := midnightUTC(2026, 3, 9)
	rules := []store.AvailabilityRule{rule(0, "09:00", "12:00", "America/New_York")} // Sunday=0
	got := generateSlots(rules, event30(0), nil, from, to, midnightUTC(2026, 3, 7))
	// 09:00, 09:30, 10:00, 10:30, 11:00, 11:30 → 6 slots.
	if len(got) != 6 {
		t.Fatalf("DST window must still yield 6 slots, got %d: %v", len(got), got)
	}
	// First slot must be 09:00 NY = 13:00 UTC during EDT.
	wantUTC := time.Date(2026, 3, 8, 13, 0, 0, 0, time.UTC)
	if !got[0].Equal(wantUTC) {
		t.Fatalf("DST first slot: want %s UTC, got %s UTC", wantUTC, got[0])
	}
}

func TestGenerateSlots_RangeBoundExcludesBleed(t *testing.T) {
	// Asia/Kolkata is UTC+5:30 — a rule's "Mon 09:00" actually lives in UTC
	// Sunday 03:30. If the requested range is Mon-only in UTC, we should not
	// emit those Sunday-in-UTC slots even though they're "Monday" locally.
	from := midnightUTC(2026, 5, 18) // Mon 00:00 UTC
	to := midnightUTC(2026, 5, 19)
	rules := []store.AvailabilityRule{rule(1, "09:00", "10:00", "Asia/Kolkata")}
	got := generateSlots(rules, event30(0), nil, from, to, midnightUTC(2026, 5, 17))
	for _, slot := range got {
		if slot.Before(from) || !slot.Before(to) {
			t.Fatalf("slot %s outside [%s, %s)", slot, from, to)
		}
	}
}

// ─── isSlotConflicted ────────────────────────────────────────────────────────

func TestIsSlotConflicted_HalfOpenOverlap(t *testing.T) {
	slot := time.Date(2026, 5, 18, 10, 0, 0, 0, time.UTC)
	duration := 30 * time.Minute
	booking := store.Booking{
		StartsAt: time.Date(2026, 5, 18, 10, 30, 0, 0, time.UTC),
		EndsAt:   time.Date(2026, 5, 18, 11, 0, 0, 0, time.UTC),
	}
	// No buffer, touching at 10:30 — half-open means clear.
	if isSlotConflicted(slot, duration, 0, []store.Booking{booking}) {
		t.Fatalf("touching ends should not collide under half-open rule")
	}
	// 15m buffer extends booking start back to 10:15 — now we overlap.
	if !isSlotConflicted(slot, duration, 15*time.Minute, []store.Booking{booking}) {
		t.Fatalf("buffer should cause overlap")
	}
}

// ─── checkBookingConflict ────────────────────────────────────────────────────

func TestCheckBookingConflict_DetectsOverlap(t *testing.T) {
	st := newMemStore()
	_, event, _, _ := bookingFixture(t, st, "host@example.com")

	start := time.Now().UTC().Add(2 * time.Hour).Truncate(time.Minute)
	end := start.Add(30 * time.Minute)
	if _, err := st.CreateBooking(context.Background(), store.Booking{
		EventTypeID: event.ID, HostID: event.HostID,
		GuestEmail: "first@example.com", GuestName: "First",
		StartsAt: start, EndsAt: end,
		MeetCode: "abc-defg-hij", Status: "confirmed",
	}); err != nil {
		t.Fatalf("seed booking: %v", err)
	}

	// Same slot must conflict.
	if err := checkBookingConflict(context.Background(), st, *event, start, end); !errors.Is(err, errSlotTaken) {
		t.Fatalf("expected errSlotTaken, got %v", err)
	}
	// Adjacent slot is fine.
	if err := checkBookingConflict(context.Background(), st, *event, end, end.Add(30*time.Minute)); err != nil {
		t.Fatalf("adjacent slot should be free, got %v", err)
	}
}

func TestCheckBookingConflict_IgnoresCancelled(t *testing.T) {
	st := newMemStore()
	_, event, _, _ := bookingFixture(t, st, "host@example.com")
	start := time.Now().UTC().Add(2 * time.Hour).Truncate(time.Minute)
	end := start.Add(30 * time.Minute)
	b, err := st.CreateBooking(context.Background(), store.Booking{
		EventTypeID: event.ID, HostID: event.HostID,
		GuestEmail: "first@example.com", GuestName: "First",
		StartsAt: start, EndsAt: end,
		MeetCode: "abc-defg-jkl", Status: "confirmed",
	})
	if err != nil {
		t.Fatalf("seed booking: %v", err)
	}
	// Cancel it; the slot must free up.
	b.Status = "cancelled"
	st.bookings[b.ID] = b
	if err := checkBookingConflict(context.Background(), st, *event, start, end); err != nil {
		t.Fatalf("cancelled booking should not block, got %v", err)
	}
}

// ─── POST /bookings now goes through conflict check ──────────────────────────

func TestCreateBooking_RejectsExactDoubleBook(t *testing.T) {
	st := newMemStore()
	host, event, guest, _ := bookingFixture(t, st, "host@example.com")
	body := func(slot string) string {
		return fmt.Sprintf(`{
			"hostSlug": %q, "eventTypeSlug": %q, "startsAt": %q,
			"guestName": "First", "guestEmail": "first@example.com"
		}`, host.Slug, event.Slug, slot)
	}
	slot := futureRFC3339(60)

	rec1 := httptest.NewRecorder()
	dispatchBookings(st, rec1, guest(http.MethodPost, "/bookings", body(slot)))
	if rec1.Code != http.StatusCreated {
		t.Fatalf("first booking should succeed, got %d: %s", rec1.Code, rec1.Body.String())
	}

	rec2 := httptest.NewRecorder()
	dispatchBookings(st, rec2, guest(http.MethodPost, "/bookings", body(slot)))
	if rec2.Code != http.StatusConflict {
		t.Fatalf("second booking must 409, got %d: %s", rec2.Code, rec2.Body.String())
	}
	if !strings.Contains(rec2.Body.String(), "SLOT_TAKEN") {
		t.Fatalf("expected SLOT_TAKEN code, got %q", rec2.Body.String())
	}
}

// ─── Email side effects on booking creation ──────────────────────────────────

func TestCreateBooking_SendsGuestAndHostEmails(t *testing.T) {
	st := newMemStore()
	host, event, _, _ := bookingFixture(t, st, "host@example.com")
	mailer := &recordingMailer{}
	deps := BookingDeps{Mailer: mailer, PublicAppURL: "https://app.sessionly.test"}

	body := fmt.Sprintf(`{
		"hostSlug": %q, "eventTypeSlug": %q, "startsAt": %q,
		"guestName": "Pat Guest", "guestEmail": "pat@example.com"
	}`, host.Slug, event.Slug, futureRFC3339(60))
	rec := httptest.NewRecorder()
	req := authReq(http.MethodPost, "/bookings", body)
	handleCreateBooking(st, deps)(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", rec.Code, rec.Body.String())
	}

	msgs := mailer.Messages()
	if len(msgs) != 2 {
		t.Fatalf("want 2 emails (guest + host), got %d", len(msgs))
	}
	// First send is to the guest; second to the host (handler's call order).
	if !strings.Contains(msgs[0].To[0], "pat@example.com") {
		t.Fatalf("first send should target guest, got %v", msgs[0].To)
	}
	if !strings.Contains(msgs[1].To[0], "host@example.com") {
		t.Fatalf("second send should target host, got %v", msgs[1].To)
	}
	// Room link uses the configured PublicAppURL + meet code.
	if !strings.Contains(msgs[0].TextBody, "https://app.sessionly.test/room/") {
		t.Fatalf("guest text body missing room URL: %q", msgs[0].TextBody)
	}
	// .ics attachment carries the booking summary.
	if len(msgs[0].Attachments) != 1 || msgs[0].Attachments[0].Filename != "booking.ics" {
		t.Fatalf("guest message should have booking.ics attached; got %+v", msgs[0].Attachments)
	}
}

func TestCreateBooking_MailerFailureDoesNotBreakBooking(t *testing.T) {
	st := newMemStore()
	host, event, _, _ := bookingFixture(t, st, "host@example.com")
	mailer := &recordingMailer{err: errors.New("smtp down")}
	deps := BookingDeps{Mailer: mailer, PublicAppURL: "https://app.sessionly.test"}

	body := fmt.Sprintf(`{
		"hostSlug": %q, "eventTypeSlug": %q, "startsAt": %q,
		"guestName": "Pat", "guestEmail": "pat@example.com"
	}`, host.Slug, event.Slug, futureRFC3339(60))
	rec := httptest.NewRecorder()
	handleCreateBooking(st, deps)(rec, authReq(http.MethodPost, "/bookings", body))
	if rec.Code != http.StatusCreated {
		t.Fatalf("booking must still succeed when mailer fails; got %d: %s", rec.Code, rec.Body.String())
	}
}

// ─── GET /u/{slug} ───────────────────────────────────────────────────────────

func TestPublicProfile_ListsActiveEventTypesOnly(t *testing.T) {
	st := newMemStore()
	host, _, _, _ := bookingFixture(t, st, "host@example.com")
	// Add a second event type, inactive — must not leak through the public
	// profile.
	_, err := st.CreateEventType(context.Background(), store.EventType{
		HostID: host.ID, Slug: "hidden", Title: "Hidden",
		DurationMin: 30, IsActive: false,
	})
	if err != nil {
		t.Fatalf("seed inactive event: %v", err)
	}

	rec := httptest.NewRecorder()
	dispatchPublic(st, rec, publicReq(http.MethodGet, "/u/"+host.Slug))
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var got hostProfileResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Slug != host.Slug || got.Name != host.Name {
		t.Fatalf("profile shape: %+v", got)
	}
	if len(got.EventTypes) != 1 {
		t.Fatalf("inactive event must not appear; got %+v", got.EventTypes)
	}
}

func TestPublicProfile_UnknownHostIs404(t *testing.T) {
	st := newMemStore()
	rec := httptest.NewRecorder()
	dispatchPublic(st, rec, publicReq(http.MethodGet, "/u/nobody"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", rec.Code)
	}
}

// ─── GET /u/{slug}/{event} ───────────────────────────────────────────────────

func TestPublicEvent_HappyPath(t *testing.T) {
	st := newMemStore()
	host, event, _, _ := bookingFixture(t, st, "host@example.com")
	rec := httptest.NewRecorder()
	dispatchPublic(st, rec, publicReq(http.MethodGet, "/u/"+host.Slug+"/"+event.Slug))
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var got publicEventResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Host.Slug != host.Slug || got.Event.Slug != event.Slug {
		t.Fatalf("event payload: %+v", got)
	}
}

func TestPublicEvent_InactiveHidden(t *testing.T) {
	st := newMemStore()
	host, event, _, _ := bookingFixture(t, st, "host@example.com")
	if err := st.DeleteEventType(context.Background(), host.ID, event.ID); err != nil {
		t.Fatalf("DeleteEventType: %v", err)
	}
	rec := httptest.NewRecorder()
	dispatchPublic(st, rec, publicReq(http.MethodGet, "/u/"+host.Slug+"/"+event.Slug))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("inactive event must 404, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ─── GET /u/{slug}/{event}/slots ─────────────────────────────────────────────

func TestPublicSlots_ReturnsAvailableSlots(t *testing.T) {
	st := newMemStore()
	host, event, _, _ := bookingFixture(t, st, "host@example.com")

	// Find a future Monday in UTC so the rule actually applies during the
	// requested window.
	monday := nextWeekday(time.Monday)
	if _, err := st.ReplaceAvailability(context.Background(), host.ID,
		[]store.AvailabilityRule{rule(1, "09:00", "10:00", "UTC")}); err != nil {
		t.Fatalf("seed availability: %v", err)
	}

	url := fmt.Sprintf("/u/%s/%s/slots?from=%s&to=%s",
		host.Slug, event.Slug,
		monday.Format("2006-01-02"),
		monday.AddDate(0, 0, 1).Format("2006-01-02"))
	rec := httptest.NewRecorder()
	dispatchPublic(st, rec, publicReq(http.MethodGet, url))
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var got slotsResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Slots) != 2 {
		t.Fatalf("want 2 slots (09:00, 09:30) on Monday, got %d: %v", len(got.Slots), got.Slots)
	}
}

func TestPublicSlots_RejectsInvalidRange(t *testing.T) {
	st := newMemStore()
	host, event, _, _ := bookingFixture(t, st, "host@example.com")
	rec := httptest.NewRecorder()
	dispatchPublic(st, rec, publicReq(http.MethodGet,
		fmt.Sprintf("/u/%s/%s/slots?from=2026-12-01&to=2027-12-01", host.Slug, event.Slug)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for >62d range, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ─── GET /m/{code} ───────────────────────────────────────────────────────────

func TestGetBookingByMeetCode_HappyPath(t *testing.T) {
	st := newMemStore()
	host, event, _, _ := bookingFixture(t, st, "host@example.com")
	start := time.Now().UTC().Add(2 * time.Hour).Truncate(time.Minute)
	created, err := st.CreateBooking(context.Background(), store.Booking{
		EventTypeID: event.ID, HostID: host.ID,
		GuestEmail: "g@example.com", GuestName: "Guest",
		StartsAt: start, EndsAt: start.Add(30 * time.Minute),
		MeetCode: "abc-defg-hij", Status: "confirmed",
	})
	if err != nil {
		t.Fatalf("seed booking: %v", err)
	}
	rec := httptest.NewRecorder()
	dispatchPublic(st, rec, publicReq(http.MethodGet, "/m/"+created.MeetCode))
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var got bookingDTO
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.MeetCode != created.MeetCode {
		t.Fatalf("meet code: want %s, got %s", created.MeetCode, got.MeetCode)
	}
}

func TestGetBookingByMeetCode_NotFound(t *testing.T) {
	st := newMemStore()
	rec := httptest.NewRecorder()
	dispatchPublic(st, rec, publicReq(http.MethodGet, "/m/zzz-zzzz-zzz"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown code must 404, got %d", rec.Code)
	}
}

// ─── Cancel paths ────────────────────────────────────────────────────────────

// seedBooking provisions a host, event, and one confirmed booking. Returns the
// booking and the host's token for follow-up host-side requests.
func seedBooking(t *testing.T, st *memStore) (*store.Booking, *store.User, *store.EventType, string) {
	t.Helper()
	host, event, _, _ := bookingFixture(t, st, "host@example.com")
	start := time.Now().UTC().Add(2 * time.Hour).Truncate(time.Minute)
	b, err := st.CreateBooking(context.Background(), store.Booking{
		EventTypeID: event.ID, HostID: host.ID,
		GuestName: "Pat", GuestEmail: "pat@example.com",
		StartsAt: start, EndsAt: start.Add(30 * time.Minute),
		MeetCode: "can-celab-le1", Status: "confirmed",
	})
	if err != nil {
		t.Fatalf("seed booking: %v", err)
	}
	return b, host, event, tokenForUser(t, host.ID)
}

func TestGuestCancel_HappyPath(t *testing.T) {
	st := newMemStore()
	b, _, _, _ := seedBooking(t, st)
	mailer := &recordingMailer{}
	deps := BookingDeps{Mailer: mailer, PublicAppURL: "https://app.test"}

	rec := httptest.NewRecorder()
	dispatchPublicWithDeps(st, deps, rec, publicReq(http.MethodDelete, "/m/"+b.MeetCode))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("want 204, got %d: %s", rec.Code, rec.Body.String())
	}
	// Booking is now cancelled in the store.
	got, _ := st.GetBookingByID(context.Background(), b.ID)
	if got.Status != "cancelled" {
		t.Fatalf("want status=cancelled, got %q", got.Status)
	}
	// Email fired with both recipients.
	if msgs := mailer.Messages(); len(msgs) != 1 || len(msgs[0].To) != 2 {
		t.Fatalf("expected 1 cancellation email to both parties, got %+v", msgs)
	}
}

func TestGuestCancel_UnknownCodeIs404(t *testing.T) {
	st := newMemStore()
	rec := httptest.NewRecorder()
	dispatchPublicWithDeps(st, BookingDeps{}, rec, publicReq(http.MethodDelete, "/m/no-such-code"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("want 404 for unknown code, got %d", rec.Code)
	}
}

func TestGuestCancel_Idempotent(t *testing.T) {
	st := newMemStore()
	b, _, _, _ := seedBooking(t, st)
	deps := BookingDeps{Mailer: &recordingMailer{}}

	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		dispatchPublicWithDeps(st, deps, rec, publicReq(http.MethodDelete, "/m/"+b.MeetCode))
		if rec.Code != http.StatusNoContent {
			t.Fatalf("cancel attempt %d: want 204, got %d", i, rec.Code)
		}
	}
}

func TestHostCancel_HappyPath(t *testing.T) {
	st := newMemStore()
	b, _, _, token := seedBooking(t, st)
	mailer := &recordingMailer{}
	deps := BookingDeps{Mailer: mailer, PublicAppURL: "https://app.test"}

	req := authReq(http.MethodDelete, "/bookings/"+b.ID, "")
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	RequireAuth(testSecret, handleHostCancelBooking(st, deps, b.ID))(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("want 204, got %d: %s", rec.Code, rec.Body.String())
	}
	got, _ := st.GetBookingByID(context.Background(), b.ID)
	if got.Status != "cancelled" {
		t.Fatalf("want cancelled, got %q", got.Status)
	}
}

func TestHostCancel_RejectsOtherHosts(t *testing.T) {
	st := newMemStore()
	b, _, _, _ := seedBooking(t, st)
	// Register a different host and use their token — must get 404, not 403,
	// so cross-host probing can't enumerate booking IDs.
	registerUser(t, st, "intruder@example.com", "password123")
	intruder, _ := st.GetUserByEmail(context.Background(), "intruder@example.com")
	otherTok := tokenForUser(t, intruder.ID)

	req := authReq(http.MethodDelete, "/bookings/"+b.ID, "")
	req.Header.Set("Authorization", "Bearer "+otherTok)
	rec := httptest.NewRecorder()
	RequireAuth(testSecret, handleHostCancelBooking(st, BookingDeps{}, b.ID))(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-host cancel must 404, got %d: %s", rec.Code, rec.Body.String())
	}
	got, _ := st.GetBookingByID(context.Background(), b.ID)
	if got.Status != "confirmed" {
		t.Fatalf("status must not have changed; got %q", got.Status)
	}
}

func TestCancelledSlotBecomesBookableAgain(t *testing.T) {
	st := newMemStore()
	b, host, event, _ := seedBooking(t, st)
	// Cancel it.
	if err := st.CancelBooking(context.Background(), b.ID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	// Re-booking the exact same slot via POST /bookings must succeed.
	body := fmt.Sprintf(`{
		"hostSlug": %q, "eventTypeSlug": %q, "startsAt": %q,
		"guestName": "Second", "guestEmail": "second@example.com"
	}`, host.Slug, event.Slug, b.StartsAt.Format(time.RFC3339))
	rec := httptest.NewRecorder()
	handleCreateBooking(st, BookingDeps{Mailer: &recordingMailer{}})(rec, authReq(http.MethodPost, "/bookings", body))
	if rec.Code != http.StatusCreated {
		t.Fatalf("re-booking a cancelled slot must succeed; got %d: %s", rec.Code, rec.Body.String())
	}
}

// ─── parseSlotsRange ─────────────────────────────────────────────────────────

func TestParseSlotsRange_DefaultsToTwoWeeks(t *testing.T) {
	from, to, err := parseSlotsRange("2026-05-18", "")
	if err != nil {
		t.Fatalf("parseSlotsRange: %v", err)
	}
	if to.Sub(from) != 15*24*time.Hour {
		// Inclusive of `to` adds one extra day on top of the 14d default.
		t.Fatalf("default range = 15d (14d + inclusive-to), got %v", to.Sub(from))
	}
}

func TestParseSlotsRange_RejectsBackwards(t *testing.T) {
	if _, _, err := parseSlotsRange("2026-05-18", "2026-05-17"); err == nil {
		t.Fatalf("expected error for to < from")
	}
}

// nextWeekday returns midnight UTC of the next occurrence of the requested
// weekday at least 1 day in the future. Used by /slots tests so the assertion
// stays stable regardless of when the suite runs.
func nextWeekday(target time.Weekday) time.Time {
	d := time.Now().UTC().AddDate(0, 0, 1)
	d = time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, time.UTC)
	for d.Weekday() != target {
		d = d.AddDate(0, 0, 1)
	}
	return d
}
