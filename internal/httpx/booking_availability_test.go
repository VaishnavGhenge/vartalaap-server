package httpx

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/vaishnavghenge/vartalaap-server/internal/store"
)

func TestBookingAndHoldRejectUnavailableTime(t *testing.T) {
	for _, path := range []string{"/bookings", "/holds"} {
		t.Run(path, func(t *testing.T) {
			st := newMemStore()
			host, event, request, _ := bookingFixture(t, st, "unavailable@example.com")
			if _, err := st.ReplaceAvailability(context.Background(), host.ID, nil); err != nil {
				t.Fatal(err)
			}
			body := fmt.Sprintf(`{"hostSlug":%q,"eventTypeSlug":%q,"startsAt":%q,"guestName":"Guest","guestEmail":"guest@example.com"}`, host.Slug, event.Slug, futureRFC3339(60))
			if path == "/holds" {
				body = fmt.Sprintf(`{"hostSlug":%q,"eventTypeSlug":%q,"startsAt":%q}`, host.Slug, event.Slug, futureRFC3339(60))
			}
			rec := httptest.NewRecorder()
			req := request(http.MethodPost, path, body)
			if path == "/bookings" {
				handleCreateBooking(st, BookingDeps{})(rec, req)
			} else {
				handleCreateHold(st)(rec, req)
			}
			if rec.Code != http.StatusConflict {
				t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestBookingRejectsOtherEventOverlap(t *testing.T) {
	st := newMemStore()
	host, event, request, _ := bookingFixture(t, st, "cross-event@example.com")
	start := time.Now().UTC().AddDate(0, 0, 1).Truncate(24 * time.Hour).Add(10 * time.Hour)
	_, err := st.CreateBooking(context.Background(), store.Booking{HostID: host.ID, EventTypeID: "another-event", StartsAt: start, EndsAt: start.Add(time.Hour), MeetCode: "existing", Status: "confirmed"})
	if err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"hostSlug":%q,"eventTypeSlug":%q,"startsAt":%q,"guestName":"Guest","guestEmail":"guest@example.com"}`, host.Slug, event.Slug, start.Format(time.RFC3339))
	rec := httptest.NewRecorder()
	handleCreateBooking(st, BookingDeps{})(rec, request(http.MethodPost, "/bookings", body))
	if rec.Code != http.StatusConflict {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestBookingFitsAvailability(t *testing.T) {
	now := time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)
	rules := []store.AvailabilityRule{{DayOfWeek: 1, StartTime: "09:00", EndTime: "17:00", Timezone: "Asia/Kolkata"}}
	for _, tc := range []struct {
		name, start     string
		notice, horizon int
		rules           []store.AvailabilityRule
		want            bool
	}{
		{"inside local hours", "2026-09-07T04:00:00Z", 0, 0, rules, true},
		{"before local hours", "2026-09-07T03:00:00Z", 0, 0, rules, false},
		{"duration crosses closing", "2026-09-07T11:15:00Z", 0, 0, rules, false},
		{"wrong weekday", "2026-09-08T04:00:00Z", 0, 0, rules, false},
		{"insufficient notice", "2026-09-07T04:00:00Z", 5, 0, rules, false},
		{"beyond horizon", "2026-09-14T04:00:00Z", 0, 3, rules, false},
		{"no availability", "2026-09-07T04:00:00Z", 0, 0, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			start, err := time.Parse(time.RFC3339, tc.start)
			if err != nil {
				t.Fatal(err)
			}
			event := store.EventType{DurationMin: 30, MinNoticeHours: tc.notice, MaxDaysAhead: tc.horizon}
			if got := bookingFitsAvailability(tc.rules, event, start, now); got != tc.want {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}
