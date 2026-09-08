package notifications

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
	"github.com/vaishnavghenge/vartalaap-server/internal/calendar"
	"github.com/vaishnavghenge/vartalaap-server/internal/db"
	"github.com/vaishnavghenge/vartalaap-server/internal/email"
	"github.com/vaishnavghenge/vartalaap-server/internal/store"
)

type flakyMailer struct {
	mu        sync.Mutex
	failed    map[string]bool
	payloads  map[string]string
	delivered map[string]email.Message
}

func (m *flakyMailer) Send(_ context.Context, msg email.Message) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	raw, _ := json.Marshal(msg)
	key := msg.IdempotencyKey
	if key == "" {
		return errors.New("missing idempotency key")
	}
	if old, ok := m.payloads[key]; ok && old != string(raw) {
		return errors.New("retry changed payload")
	}
	m.payloads[key] = string(raw)
	if !m.failed[key] {
		m.failed[key] = true
		// The provider accepted the email but its response was lost. The
		// repeated key and identical payload must represent the same send.
		m.delivered[key] = msg
		return context.DeadlineExceeded
	}
	m.delivered[key] = msg
	return nil
}

type flakyCalendar struct {
	mu      sync.Mutex
	failed  map[string]bool
	events  map[string]bool
	creates int
}

func (c *flakyCalendar) SyncBookingCreated(_ context.Context, in calendar.BookingEvent) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.failed["create/"+in.BookingID] {
		c.failed["create/"+in.BookingID] = true
		return errors.New("simulated calendar outage")
	}
	c.events[in.BookingID] = true
	c.creates++
	return nil
}

func (c *flakyCalendar) SyncBookingCancelled(_ context.Context, _, id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.failed["delete/"+id] {
		c.failed["delete/"+id] = true
		return errors.New("simulated calendar outage")
	}
	delete(c.events, id)
	return nil
}

func TestDurableDeliveryRecovery(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	ctr, err := tcpostgres.Run(ctx, "postgres:17-alpine", tcpostgres.WithDatabase("outboxtest"), tcpostgres.WithUsername("test"), tcpostgres.WithPassword("test"),
		testcontainers.WithWaitStrategy(wait.ForLog("database system is ready to accept connections").WithOccurrence(2).WithStartupTimeout(30*time.Second)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ctr.Terminate(context.Background()) })
	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	pool, err := db.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	st := store.New(pool)
	u, err := st.CreateUser(ctx, "host@example.com", "Host", "host", "hash")
	if err != nil {
		t.Fatal(err)
	}
	e, err := st.CreateEventType(ctx, store.EventType{HostID: u.ID, Slug: "intro", Title: "Intro", DurationMin: 30, Currency: "usd", PaymentTiming: "upfront", IsActive: true})
	if err != nil {
		t.Fatal(err)
	}
	create := func(day int) *store.Booking {
		t.Helper()
		start := time.Now().UTC().AddDate(0, 0, day)
		b, err := st.CreateBooking(ctx, store.Booking{HostID: u.ID, EventTypeID: e.ID, GuestName: "Guest", GuestEmail: "guest@example.com", MeetCode: fmt.Sprintf("test-%d", day), StartsAt: start, EndsAt: start.Add(30 * time.Minute), Status: "confirmed"})
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	m := &flakyMailer{failed: map[string]bool{}, payloads: map[string]string{}, delivered: map[string]email.Message{}}
	c := &flakyCalendar{failed: map[string]bool{}, events: map[string]bool{}}
	w := &Worker{Queue: st, Mailer: m, Calendar: c, PublicAppURL: "https://app.test"}
	run := func(n int) {
		t.Helper()
		for i := 0; i < n; i++ {
			if _, err := w.ProcessOne(ctx); err != nil {
				t.Fatal(err)
			}
		}
	}
	retryNow := func() {
		t.Helper()
		if _, err := pool.Exec(ctx, `UPDATE booking_outbox SET available_at=now() WHERE completed_at IS NULL`); err != nil {
			t.Fatal(err)
		}
	}
	pending := func(want int64) {
		t.Helper()
		n, _, err := st.OutboxBacklog(ctx)
		if err != nil || n != want {
			t.Fatalf("pending=%d want=%d err=%v", n, want, err)
		}
	}
	b := create(1)
	pending(3)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := w.ProcessOne(ctx); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	pending(3)
	var attempts int
	if err := pool.QueryRow(ctx, `SELECT sum(attempts) FROM booking_outbox`).Scan(&attempts); err != nil || attempts != 3 {
		t.Fatalf("duplicate claims: attempts=%d err=%v", attempts, err)
	}
	// A fresh worker/store instance recovers solely from persisted state.
	w = &Worker{Queue: store.New(pool), Mailer: m, Calendar: c, PublicAppURL: "https://app.test"}
	retryNow()
	run(3)
	pending(0)
	if len(m.delivered) != 2 || !c.events[b.ID] {
		t.Fatal("confirmation did not recover")
	}
	if err := st.CancelBooking(ctx, b.ID, "test cancellation", "host"); err != nil {
		t.Fatal(err)
	}
	run(3)
	pending(3)
	retryNow()
	run(3)
	pending(0)
	if len(m.delivered) != 4 || c.events[b.ID] {
		t.Fatal("cancellation did not recover")
	}
	for _, msg := range m.delivered {
		if len(msg.To) != 1 {
			t.Fatal("recipients must have independent delivery jobs")
		}
	}

	b2 := create(2)
	if err := st.CancelBooking(ctx, b2.ID, "cancel before delivery", "guest"); err != nil {
		t.Fatal(err)
	}
	run(6)
	retryNow()
	run(6)
	pending(0)
	if c.creates != 1 {
		t.Fatal("cancelled booking was recreated")
	}
	for key := range m.delivered {
		if strings.Contains(key, b2.ID+"/created/") {
			t.Fatal("stale confirmation delivered")
		}
	}

	create(3)
	stale, err := st.ClaimOutboxJob(ctx)
	if err != nil || stale == nil {
		t.Fatal("claim failed", err)
	}
	for i := 0; i < 2; i++ {
		job, err := st.ClaimOutboxJob(ctx)
		if err != nil || job == nil {
			t.Fatal("claim failed", err)
		}
		if err := st.FinishOutboxJob(ctx, *job, time.Time{}, ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE booking_outbox SET lease_until=now()-interval '1 second' WHERE id=$1`, stale.ID); err != nil {
		t.Fatal(err)
	}
	reclaimed, err := st.ClaimOutboxJob(ctx)
	if err != nil || reclaimed == nil || reclaimed.ID != stale.ID || reclaimed.LeaseToken == stale.LeaseToken {
		t.Fatal("lease was not reclaimed", err)
	}
	if err := st.FinishOutboxJob(ctx, *stale, time.Time{}, ""); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("stale worker acknowledgement accepted: %v", err)
	}
	if err := st.FinishOutboxJob(ctx, *reclaimed, time.Time{}, ""); err != nil {
		t.Fatal(err)
	}
	pending(0)

	b4 := create(4)
	_, err = pool.Exec(ctx, `CREATE FUNCTION reject_test_outbox() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'simulated outbox disk failure'; END $$;
		CREATE TRIGGER reject_test_outbox BEFORE INSERT ON booking_outbox FOR EACH ROW EXECUTE FUNCTION reject_test_outbox();`)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CancelBooking(ctx, b4.ID, "must roll back", "host"); err == nil {
		t.Fatal("expected enqueue failure")
	}
	unchanged, err := st.GetBookingByID(ctx, b4.ID)
	if err != nil || unchanged.Status != "confirmed" {
		t.Fatal("cancellation committed without its notifications", err)
	}
	start := time.Now().AddDate(0, 0, 5)
	_, err = st.CreateBooking(ctx, store.Booking{HostID: u.ID, EventTypeID: e.ID, GuestName: "Guest", GuestEmail: "guest@example.com", MeetCode: "must-rollback", StartsAt: start, EndsAt: start.Add(30 * time.Minute), Status: "confirmed"})
	if err == nil {
		t.Fatal("expected create enqueue failure")
	}
	if _, err := st.GetBookingByMeetCode(ctx, "must-rollback"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("booking committed without notifications", err)
	}
}

func TestRetryDelayIsBoundedAndJittered(t *testing.T) {
	seen := map[time.Duration]bool{}
	for i := 0; i < 100; i++ {
		d := retryDelay(1)
		if d < 15*time.Second || d > 30*time.Second {
			t.Fatal(d)
		}
		seen[d] = true
		if d := retryDelay(1000000); d < 30*time.Minute || d > time.Hour {
			t.Fatal(d)
		}
	}
	if len(seen) < 2 {
		t.Fatal("retry delays have no jitter")
	}
}
