package store_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/vaishnavghenge/vartalaap-server/internal/store"
)

func TestConcurrentBookingsAcrossEventTypes(t *testing.T) {
	st := newStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	u, err := st.CreateUser(ctx, unique("race")+"@example.com", "Host", unique("race"), "hash")
	if err != nil {
		t.Fatal(err)
	}
	var events []*store.EventType
	for i := 0; i < 2; i++ {
		e, err := st.CreateEventType(ctx, store.EventType{HostID: u.ID, Slug: fmt.Sprintf("event-%d", i), Title: "Session", DurationMin: 30, Currency: "usd", PaymentTiming: "upfront", IsActive: true})
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, e)
	}
	start := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Minute)
	gate := make(chan struct{})
	results := make(chan error, 10)
	for i := 0; i < 10; i++ {
		go func(i int) {
			<-gate
			_, err := st.CreateBooking(ctx, store.Booking{HostID: u.ID, EventTypeID: events[i%2].ID, GuestEmail: "guest@example.com", GuestName: "Guest", StartsAt: start, EndsAt: start.Add(30 * time.Minute), MeetCode: unique(fmt.Sprintf("race-%d", i)), Status: "confirmed"})
			results <- err
		}(i)
	}
	close(gate)
	winners := 0
	for i := 0; i < 10; i++ {
		err := <-results
		if err == nil {
			winners++
		} else if !errors.Is(err, store.ErrSlotTaken) {
			t.Errorf("unexpected booking error: %v", err)
		}
	}
	if winners != 1 {
		t.Fatalf("got %d successful overlapping bookings; want 1", winners)
	}
	bookings, err := st.ListBookingsForHostInRange(ctx, u.ID, start, start.Add(time.Hour))
	if err != nil || len(bookings) != 1 {
		t.Fatalf("bookings=%v err=%v", bookings, err)
	}
	// The winning event's buffer must block other event types too.
	winnerEvent, err := st.GetEventType(ctx, u.ID, bookings[0].EventTypeID)
	if err != nil {
		t.Fatal(err)
	}
	winnerEvent.BufferMin = 30
	if _, err := st.UpdateEventType(ctx, *winnerEvent); err != nil {
		t.Fatal(err)
	}
	adjacent := store.Booking{HostID: u.ID, EventTypeID: events[1].ID, GuestEmail: "guest@example.com", GuestName: "Guest", StartsAt: start.Add(45 * time.Minute), EndsAt: start.Add(75 * time.Minute), MeetCode: unique("buffered"), Status: "confirmed"}
	if _, err := st.CreateBooking(ctx, adjacent); !errors.Is(err, store.ErrSlotTaken) {
		t.Fatalf("expected existing event buffer to block booking, got %v", err)
	}
	blockers, err := st.ListBookingsForHostInRange(ctx, u.ID, adjacent.StartsAt, adjacent.EndsAt)
	if err != nil || len(blockers) != 1 || blockers[0].BufferAfterMin == nil || *blockers[0].BufferAfterMin != 30 {
		t.Fatalf("buffer-only overlap not returned: %+v, %v", blockers, err)
	}
	if err := st.CancelBooking(ctx, bookings[0].ID, "test", "host"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateBooking(ctx, store.Booking{HostID: u.ID, EventTypeID: events[1].ID, GuestEmail: "guest@example.com", GuestName: "Guest", StartsAt: start, EndsAt: start.Add(30 * time.Minute), MeetCode: unique("replacement"), Status: "confirmed"}); err != nil {
		t.Fatalf("cancelled slot should be reusable: %v", err)
	}
}

func TestRescheduleBookingIsAtomicAndPreservesIdentity(t *testing.T) {
	st := newStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	u, err := st.CreateUser(ctx, unique("move")+"@example.com", "Host", unique("move"), "hash")
	if err != nil {
		t.Fatal(err)
	}
	e, err := st.CreateEventType(ctx, store.EventType{HostID: u.ID, Slug: "session", Title: "Session", DurationMin: 30, Currency: "usd", PaymentTiming: "upfront", IsActive: true})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Minute)
	original, err := st.CreateBooking(ctx, store.Booking{HostID: u.ID, EventTypeID: e.ID, GuestEmail: "guest@example.com", GuestName: "Guest", StartsAt: start, EndsAt: start.Add(30 * time.Minute), MeetCode: unique("stable-room"), CancelToken: "stable-token", Status: "confirmed"})
	if err != nil {
		t.Fatal(err)
	}
	if original.Revision != 0 {
		t.Fatalf("new booking revision = %d, want 0", original.Revision)
	}
	blocker, err := st.CreateBooking(ctx, store.Booking{HostID: u.ID, EventTypeID: e.ID, GuestEmail: "other@example.com", GuestName: "Other", StartsAt: start.Add(time.Hour), EndsAt: start.Add(90 * time.Minute), MeetCode: unique("blocker"), Status: "confirmed"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.RescheduleBooking(ctx, original.ID, blocker.StartsAt, blocker.EndsAt, ""); !errors.Is(err, store.ErrSlotTaken) {
		t.Fatalf("occupied reschedule should fail with slot taken, got %v", err)
	}
	unchanged, _ := st.GetBookingByID(ctx, original.ID)
	if !unchanged.StartsAt.Equal(original.StartsAt) || unchanged.Revision != 0 {
		t.Fatalf("failed reschedule changed booking: before=%+v after=%+v", original, unchanged)
	}
	newStart := start.Add(2 * time.Hour)
	moved, err := st.RescheduleBooking(ctx, original.ID, newStart, newStart.Add(30*time.Minute), "")
	if err != nil {
		t.Fatal(err)
	}
	if moved.ID != original.ID || moved.MeetCode != original.MeetCode || moved.CancelToken != original.CancelToken || !moved.StartsAt.Equal(newStart) {
		t.Fatalf("reschedule changed booking identity: before=%+v after=%+v", original, moved)
	}
	if moved.Revision != 1 {
		t.Fatalf("first reschedule revision = %d, want 1", moved.Revision)
	}
	secondStart := start.Add(3 * time.Hour)
	movedAgain, err := st.RescheduleBooking(ctx, original.ID, secondStart, secondStart.Add(30*time.Minute), "")
	if err != nil {
		t.Fatal(err)
	}
	if movedAgain.Revision != 2 {
		t.Fatalf("second reschedule revision = %d, want 2", movedAgain.Revision)
	}
}
