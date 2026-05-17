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
	path := r.URL.Path
	if strings.HasPrefix(path, "/m/") {
		code := strings.TrimPrefix(path, "/m/")
		handleGetBookingByMeetCode(st, code)(w, r)
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

func TestGenerateSlots_BookingBlocksSlotWithBuffer(t *testing.T) {
	// 30m event, 15m buffer. Existing booking 10:00-10:30 means 09:30 (ends
	// 10:00, no buffer on the slot side — but 09:30 + 30m + booking buffer
	// 15m before = 09:45 overlap with booking start 10:00? Let's spell out:
	//   slot 09:30..10:00 vs booking-with-buffer 09:45..10:45 → overlap.
	// So 09:30 should be blocked. 09:00 (ends 09:30) is clear of 09:45.
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
	// Slots cleared: 09:00 (ends 09:30 < 09:45 booking-buffer-start). Blocked:
	// 09:30, 10:00 (the booking itself), 10:30 (10:30..11:00 overlaps the
	// post-booking 10:45 buffer). Cleared again from 11:00 onwards.
	wantBlocked := map[string]bool{"09:30": true, "10:00": true, "10:30": true}
	wantPresent := map[string]bool{"09:00": true, "11:00": true, "11:30": true}
	for s := range wantBlocked {
		for _, got := range starts {
			if got == s {
				t.Fatalf("slot %s should be blocked by booking-with-buffer, got %v", s, starts)
			}
		}
	}
	for s := range wantPresent {
		found := false
		for _, got := range starts {
			if got == s {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("slot %s should be available, got %v", s, starts)
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
