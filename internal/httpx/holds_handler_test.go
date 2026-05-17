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

	"github.com/vaishnavghenge/vartalaap-server/internal/store"
)

func dispatchHolds(st store.Storer, w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/holds" && r.Method == http.MethodPost {
		handleCreateHold(st)(w, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/holds/") && r.Method == http.MethodDelete {
		handleReleaseHold(st, strings.TrimPrefix(r.URL.Path, "/holds/"))(w, r)
		return
	}
	http.Error(w, "not found", http.StatusNotFound)
}

func TestCreateHold_HappyPath(t *testing.T) {
	st := newMemStore()
	host, event, _, _ := bookingFixture(t, st, "host@example.com")

	body := fmt.Sprintf(`{"hostSlug": %q, "eventTypeSlug": %q, "startsAt": %q}`,
		host.Slug, event.Slug, futureRFC3339(60))
	rec := httptest.NewRecorder()
	dispatchHolds(st, rec, authReq(http.MethodPost, "/holds", body))
	if rec.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var got createHoldResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.HoldToken == "" {
		t.Fatalf("missing hold token: %+v", got)
	}
	// TTL must be in the future (≤ 5min + slack) — proves the handler used
	// the holdTTL constant rather than echoing input.
	if time.Until(got.ExpiresAt) > holdTTL+5*time.Second || time.Until(got.ExpiresAt) < 4*time.Minute {
		t.Fatalf("expiresAt outside expected window: %v", got.ExpiresAt)
	}
}

func TestCreateHold_BlocksSameSlotInPicker(t *testing.T) {
	st := newMemStore()
	host, event, _, _ := bookingFixture(t, st, "host@example.com")

	// Pick the next Monday at 09:00 UTC so the rule fits.
	day := nextWeekday(time.Monday)
	startsAt := day.Add(9 * time.Hour) // 09:00 UTC Monday
	if _, err := st.ReplaceAvailability(context.Background(), host.ID,
		[]store.AvailabilityRule{rule(1, "09:00", "10:00", "UTC")}); err != nil {
		t.Fatalf("seed availability: %v", err)
	}
	// Create a hold at 09:00.
	body := fmt.Sprintf(`{"hostSlug": %q, "eventTypeSlug": %q, "startsAt": %q}`,
		host.Slug, event.Slug, startsAt.Format(time.RFC3339))
	rec := httptest.NewRecorder()
	dispatchHolds(st, rec, authReq(http.MethodPost, "/holds", body))
	if rec.Code != http.StatusCreated {
		t.Fatalf("hold creation must succeed: %d %s", rec.Code, rec.Body.String())
	}
	// /slots must omit 09:00 now (held), but 09:30 still appears because
	// the 30m event has no buffer.
	url := fmt.Sprintf("/u/%s/%s/slots?from=%s&to=%s",
		host.Slug, event.Slug,
		day.Format("2006-01-02"),
		day.AddDate(0, 0, 1).Format("2006-01-02"))
	rec2 := httptest.NewRecorder()
	dispatchPublicWithDeps(st, BookingDeps{}, rec2, publicReq(http.MethodGet, url))
	var slots slotsResponse
	if err := json.NewDecoder(rec2.Body).Decode(&slots); err != nil {
		t.Fatalf("decode slots: %v", err)
	}
	taken := startsAt.Format(time.RFC3339)
	for _, s := range slots.Slots {
		if s == taken {
			t.Fatalf("held slot %s still listed: %v", taken, slots.Slots)
		}
	}
	// Sanity: 09:30 is still available.
	want := startsAt.Add(30 * time.Minute).Format(time.RFC3339)
	found := false
	for _, s := range slots.Slots {
		if s == want {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("non-conflicting 09:30 slot must remain; got %v", slots.Slots)
	}
}

func TestCreateHold_BlocksCrossEventOnSameHost(t *testing.T) {
	st := newMemStore()
	host, event30, _, _ := bookingFixture(t, st, "host@example.com")
	// Add a second event type (60 min) on the same host. Holding the 30-min
	// event at 09:00 must also block the 60-min picker's 09:00 (overlap).
	event60, err := st.CreateEventType(context.Background(), store.EventType{
		HostID: host.ID, Slug: "deep", Title: "Deep dive",
		DurationMin: 60, IsActive: true, IsPaid: false,
	})
	if err != nil {
		t.Fatalf("seed second event: %v", err)
	}
	day := nextWeekday(time.Monday)
	if _, err := st.ReplaceAvailability(context.Background(), host.ID,
		[]store.AvailabilityRule{rule(1, "09:00", "12:00", "UTC")}); err != nil {
		t.Fatalf("seed availability: %v", err)
	}
	startsAt := day.Add(9 * time.Hour)
	body := fmt.Sprintf(`{"hostSlug": %q, "eventTypeSlug": %q, "startsAt": %q}`,
		host.Slug, event30.Slug, startsAt.Format(time.RFC3339))
	rec := httptest.NewRecorder()
	dispatchHolds(st, rec, authReq(http.MethodPost, "/holds", body))
	if rec.Code != http.StatusCreated {
		t.Fatalf("hold on 30m event must succeed: %d %s", rec.Code, rec.Body.String())
	}
	// /slots on the 60-min event must not show 09:00 (overlaps the hold).
	url := fmt.Sprintf("/u/%s/%s/slots?from=%s&to=%s",
		host.Slug, event60.Slug,
		day.Format("2006-01-02"),
		day.AddDate(0, 0, 1).Format("2006-01-02"))
	rec2 := httptest.NewRecorder()
	dispatchPublicWithDeps(st, BookingDeps{}, rec2, publicReq(http.MethodGet, url))
	var slots slotsResponse
	if err := json.NewDecoder(rec2.Body).Decode(&slots); err != nil {
		t.Fatalf("decode 60m slots: %v", err)
	}
	taken := startsAt.Format(time.RFC3339)
	for _, s := range slots.Slots {
		if s == taken {
			t.Fatalf("60m picker still shows 09:00 despite cross-event hold: %v", slots.Slots)
		}
	}
}

func TestReleaseHold_FreesSlotImmediately(t *testing.T) {
	st := newMemStore()
	host, event, _, _ := bookingFixture(t, st, "host@example.com")
	day := nextWeekday(time.Monday)
	startsAt := day.Add(9 * time.Hour)
	if _, err := st.ReplaceAvailability(context.Background(), host.ID,
		[]store.AvailabilityRule{rule(1, "09:00", "10:00", "UTC")}); err != nil {
		t.Fatalf("seed availability: %v", err)
	}
	// Create hold.
	body := fmt.Sprintf(`{"hostSlug": %q, "eventTypeSlug": %q, "startsAt": %q}`,
		host.Slug, event.Slug, startsAt.Format(time.RFC3339))
	rec := httptest.NewRecorder()
	dispatchHolds(st, rec, authReq(http.MethodPost, "/holds", body))
	var created createHoldResponse
	json.NewDecoder(rec.Body).Decode(&created)

	// DELETE the hold.
	rec2 := httptest.NewRecorder()
	dispatchHolds(st, rec2, authReq(http.MethodDelete, "/holds/"+created.HoldToken, ""))
	if rec2.Code != http.StatusNoContent {
		t.Fatalf("release must return 204, got %d: %s", rec2.Code, rec2.Body.String())
	}

	// Slot is bookable again.
	url := fmt.Sprintf("/u/%s/%s/slots?from=%s&to=%s",
		host.Slug, event.Slug,
		day.Format("2006-01-02"),
		day.AddDate(0, 0, 1).Format("2006-01-02"))
	rec3 := httptest.NewRecorder()
	dispatchPublicWithDeps(st, BookingDeps{}, rec3, publicReq(http.MethodGet, url))
	var slots slotsResponse
	json.NewDecoder(rec3.Body).Decode(&slots)
	want := startsAt.Format(time.RFC3339)
	found := false
	for _, s := range slots.Slots {
		if s == want {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("released slot must reappear in picker; got %v", slots.Slots)
	}
}

func TestReleaseHold_IsIdempotent(t *testing.T) {
	st := newMemStore()
	// Releasing an unknown token must not 404 — a stale beacon should be
	// silently accepted so the client never has to second-guess.
	rec := httptest.NewRecorder()
	dispatchHolds(st, rec, authReq(http.MethodDelete, "/holds/never-existed", ""))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("want 204 on unknown token, got %d", rec.Code)
	}
}

func TestCreateHold_ExpiredHoldDoesNotBlock(t *testing.T) {
	st := newMemStore()
	host, event, _, _ := bookingFixture(t, st, "host@example.com")
	day := nextWeekday(time.Monday)
	startsAt := day.Add(9 * time.Hour)
	if _, err := st.ReplaceAvailability(context.Background(), host.ID,
		[]store.AvailabilityRule{rule(1, "09:00", "10:00", "UTC")}); err != nil {
		t.Fatalf("seed availability: %v", err)
	}
	// Hand-craft an expired hold in the store. Bypasses the handler so we
	// can set ExpiresAt in the past without time-travel.
	_, err := st.CreateSlotHold(context.Background(), store.SlotHold{
		HostID:      host.ID,
		EventTypeID: event.ID,
		StartsAt:    startsAt,
		EndsAt:      startsAt.Add(30 * time.Minute),
		Token:       "expired-token",
		ExpiresAt:   time.Now().UTC().Add(-1 * time.Minute),
	})
	if err != nil {
		t.Fatalf("seed expired hold: %v", err)
	}
	// /slots must show 09:00 because the hold is past its TTL.
	url := fmt.Sprintf("/u/%s/%s/slots?from=%s&to=%s",
		host.Slug, event.Slug,
		day.Format("2006-01-02"),
		day.AddDate(0, 0, 1).Format("2006-01-02"))
	rec := httptest.NewRecorder()
	dispatchPublicWithDeps(st, BookingDeps{}, rec, publicReq(http.MethodGet, url))
	var slots slotsResponse
	json.NewDecoder(rec.Body).Decode(&slots)
	want := startsAt.Format(time.RFC3339)
	found := false
	for _, s := range slots.Slots {
		if s == want {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expired hold must not block slot; got %v", slots.Slots)
	}
}

func TestBookingWithHoldToken_BypassesOwnHoldConflict(t *testing.T) {
	st := newMemStore()
	host, event, _, _ := bookingFixture(t, st, "host@example.com")
	day := nextWeekday(time.Monday)
	startsAt := day.Add(9 * time.Hour)
	if _, err := st.ReplaceAvailability(context.Background(), host.ID,
		[]store.AvailabilityRule{rule(1, "09:00", "10:00", "UTC")}); err != nil {
		t.Fatalf("seed availability: %v", err)
	}

	// Step 1: guest taps slot → server creates hold.
	holdBody := fmt.Sprintf(`{"hostSlug": %q, "eventTypeSlug": %q, "startsAt": %q}`,
		host.Slug, event.Slug, startsAt.Format(time.RFC3339))
	rec := httptest.NewRecorder()
	dispatchHolds(st, rec, authReq(http.MethodPost, "/holds", holdBody))
	var hold createHoldResponse
	json.NewDecoder(rec.Body).Decode(&hold)

	// Step 2: submit booking carrying the same token. Must succeed even
	// though there's an active hold on this slot (it's the guest's own).
	bookBody := fmt.Sprintf(`{
		"hostSlug": %q, "eventTypeSlug": %q, "startsAt": %q,
		"guestName": "Pat", "guestEmail": "pat@example.com",
		"holdToken": %q
	}`, host.Slug, event.Slug, startsAt.Format(time.RFC3339), hold.HoldToken)
	rec2 := httptest.NewRecorder()
	handleCreateBooking(st, BookingDeps{})(rec2, authReq(http.MethodPost, "/bookings", bookBody))
	if rec2.Code != http.StatusCreated {
		t.Fatalf("booking with own holdToken must succeed; got %d: %s", rec2.Code, rec2.Body.String())
	}

	// The hold must be consumed (deleted) after the booking lands.
	if _, err := st.GetSlotHoldByToken(context.Background(), hold.HoldToken); err == nil {
		t.Fatalf("hold should have been deleted after booking; still in store")
	}
}

func TestBookingWithoutHoldToken_BlockedByOthersHold(t *testing.T) {
	st := newMemStore()
	host, event, _, _ := bookingFixture(t, st, "host@example.com")
	day := nextWeekday(time.Monday)
	startsAt := day.Add(9 * time.Hour)
	if _, err := st.ReplaceAvailability(context.Background(), host.ID,
		[]store.AvailabilityRule{rule(1, "09:00", "10:00", "UTC")}); err != nil {
		t.Fatalf("seed availability: %v", err)
	}
	// Someone else holds the slot first.
	holdBody := fmt.Sprintf(`{"hostSlug": %q, "eventTypeSlug": %q, "startsAt": %q}`,
		host.Slug, event.Slug, startsAt.Format(time.RFC3339))
	dispatchHolds(st, httptest.NewRecorder(), authReq(http.MethodPost, "/holds", holdBody))

	// A second guest tries to book without a token — must 409 SLOT_TAKEN.
	bookBody := fmt.Sprintf(`{
		"hostSlug": %q, "eventTypeSlug": %q, "startsAt": %q,
		"guestName": "Pat", "guestEmail": "pat@example.com"
	}`, host.Slug, event.Slug, startsAt.Format(time.RFC3339))
	rec := httptest.NewRecorder()
	handleCreateBooking(st, BookingDeps{})(rec, authReq(http.MethodPost, "/bookings", bookBody))
	if rec.Code != http.StatusConflict {
		t.Fatalf("want 409 when slot is held by another guest, got %d: %s", rec.Code, rec.Body.String())
	}
}
