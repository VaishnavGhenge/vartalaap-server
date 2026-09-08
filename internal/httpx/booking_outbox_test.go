package httpx

import (
	"context"
	"testing"

	"github.com/vaishnavghenge/vartalaap-server/internal/store"
)

func TestDurableNotificationsDoNotAlsoDeliverInline(t *testing.T) {
	mailer := &recordingMailer{}
	syncer := &fakeSync{}
	deps := BookingDeps{DurableNotifications: true, Mailer: mailer, CalendarSync: syncer}
	b := &store.Booking{ID: "booking", HostID: "host"}
	e := &store.EventType{Title: "Session"}
	u := &store.User{ID: "host", Email: "host@example.com"}
	ctx := context.Background()
	sendBookingEmails(ctx, deps, b, e, u, "Guest", "guest@example.com")
	sendCancellationEmails(ctx, deps, b, e, u, "host")
	syncBookingToCalendar(ctx, deps, b, e, u)
	unsyncBookingFromCalendar(ctx, deps, b)
	if len(mailer.Messages()) != 0 || len(syncer.Created()) != 0 || len(syncer.cancelled) != 0 {
		t.Fatal("durable notifications also delivered inline")
	}
}
