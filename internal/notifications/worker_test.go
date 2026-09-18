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

type reminderQueue struct {
	current *store.Booking
	saved   []byte
}

func (q *reminderQueue) ClaimOutboxJob(context.Context) (*store.OutboxJob, error) {
	return nil, nil
}
func (q *reminderQueue) FinishOutboxJob(context.Context, store.OutboxJob, time.Time, string) error {
	return nil
}
func (q *reminderQueue) SaveOutboxDelivery(_ context.Context, _ store.OutboxJob, payload []byte) ([]byte, error) {
	q.saved = payload
	return payload, nil
}
func (q *reminderQueue) OutboxBacklog(context.Context) (int64, float64, error) {
	return 0, 0, nil
}
func (q *reminderQueue) GetBookingByID(context.Context, string) (*store.Booking, error) {
	return q.current, nil
}
func (q *reminderQueue) GetCalendarConnection(context.Context, string, string) (*store.CalendarConnection, error) {
	return nil, store.ErrNotFound
}

type recordingMailer struct {
	messages []email.Message
}

func (m *recordingMailer) Send(_ context.Context, msg email.Message) error {
	m.messages = append(m.messages, msg)
	return nil
}

func (c *flakyCalendar) SyncBooking(_ context.Context, in calendar.BookingEvent) error {
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
		if _, err := pool.Exec(ctx, `UPDATE booking_outbox SET available_at=now()
			WHERE completed_at IS NULL AND attempts > 0`); err != nil {
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
	b := create(2)
	pending(3)
	var scheduled int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM booking_outbox
		WHERE booking_id=$1 AND action LIKE 'reminder:%' AND completed_at IS NULL`, b.ID).Scan(&scheduled); err != nil || scheduled != 4 {
		t.Fatalf("scheduled reminders=%d want=4 err=%v", scheduled, err)
	}
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
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM booking_outbox
		WHERE booking_id=$1 AND action LIKE 'reminder:%' AND completed_at IS NULL`, b.ID).Scan(&scheduled); err != nil || scheduled != 0 {
		t.Fatalf("cancellation left %d active reminders err=%v", scheduled, err)
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

	b2 := create(3)
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

	create(4)
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

	b4 := create(5)
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
	start := time.Now().AddDate(0, 0, 6)
	_, err = st.CreateBooking(ctx, store.Booking{HostID: u.ID, EventTypeID: e.ID, GuestName: "Guest", GuestEmail: "guest@example.com", MeetCode: "must-rollback", StartsAt: start, EndsAt: start.Add(30 * time.Minute), Status: "confirmed"})
	if err == nil {
		t.Fatal("expected create enqueue failure")
	}
	if _, err := st.GetBookingByMeetCode(ctx, "must-rollback"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("booking committed without notifications", err)
	}
}

func TestReminderDeliverySuppressesChangedCancelledAndPastBookings(t *testing.T) {
	start := time.Now().UTC().Add(2 * time.Hour)
	snapshot := store.Booking{
		ID: "booking-1", HostID: "host-1", GuestName: "Guest", GuestEmail: "guest@example.com",
		StartsAt: start, EndsAt: start.Add(30 * time.Minute), MeetCode: "abc-def", CancelToken: "secret", Status: "confirmed",
	}
	job := store.OutboxJob{
		BookingID: snapshot.ID, Action: fmt.Sprintf("reminder:1h:%d", start.UnixNano()), Channel: "guest_email",
		Payload: store.BookingNotification{Booking: snapshot, HostName: "Host", HostEmail: "host@example.com", HostTimezone: "UTC", EventTitle: "Catch up", EventMinutes: 30},
	}
	queue := &reminderQueue{current: &snapshot}
	mailer := &recordingMailer{}
	worker := &Worker{Queue: queue, Mailer: mailer, PublicAppURL: "https://app.test"}
	if err := worker.deliver(context.Background(), job); err != nil || len(mailer.messages) != 1 {
		t.Fatalf("current reminder was not delivered: sends=%d err=%v", len(mailer.messages), err)
	}
	if mailer.messages[0].IdempotencyKey != "booking/booking-1/"+job.Action+"/guest_email" {
		t.Fatalf("reminder lost its stable idempotency key: %q", mailer.messages[0].IdempotencyKey)
	}

	moved := snapshot
	moved.StartsAt = moved.StartsAt.Add(time.Hour)
	queue.current = &moved
	if err := worker.deliver(context.Background(), job); err != nil || len(mailer.messages) != 1 {
		t.Fatalf("rescheduled reminder was not suppressed: sends=%d err=%v", len(mailer.messages), err)
	}
	cancelled := snapshot
	cancelled.Status = "cancelled"
	queue.current = &cancelled
	if err := worker.deliver(context.Background(), job); err != nil || len(mailer.messages) != 1 {
		t.Fatalf("cancelled reminder was not suppressed: sends=%d err=%v", len(mailer.messages), err)
	}
	past := snapshot
	past.StartsAt = time.Now().UTC().Add(-time.Minute)
	job.Payload.Booking = past
	queue.current = &past
	if err := worker.deliver(context.Background(), job); err != nil || len(mailer.messages) != 1 {
		t.Fatalf("late reminder was not suppressed: sends=%d err=%v", len(mailer.messages), err)
	}
}

// recordingCalendar answers every call successfully and remembers what it was
// asked to do. flakyCalendar cannot serve here: it fails the first call per
// booking, which is the behaviour the retry test needs and noise for this one.
type recordingCalendar struct {
	synced    []calendar.BookingEvent
	cancelled []string
}

func (c *recordingCalendar) SyncBooking(_ context.Context, in calendar.BookingEvent) error {
	c.synced = append(c.synced, in)
	return nil
}

func (c *recordingCalendar) SyncBookingCancelled(_ context.Context, _, id string) error {
	c.cancelled = append(c.cancelled, id)
	return nil
}

// A reschedule moves the mirrored event. It must never delete it first: the
// event ID is derived from the booking ID, and Google will not let an insert
// reuse the ID of an event a delete cancelled, so the pair would leave the
// host with no entry at the new time and no error to show for it.
func TestRescheduledCalendarJobMovesEventInPlace(t *testing.T) {
	start := time.Now().UTC().Add(48 * time.Hour)
	moved := store.Booking{
		ID: "booking-1", HostID: "host-1", GuestName: "Guest", GuestEmail: "guest@example.com",
		StartsAt: start, EndsAt: start.Add(30 * time.Minute), MeetCode: "abc-def",
		CancelToken: "secret", Status: "confirmed", Revision: 2,
	}
	job := store.OutboxJob{
		BookingID: moved.ID, Action: fmt.Sprintf("rescheduled:%d", time.Now().UnixNano()), Channel: "calendar",
		Payload: store.BookingNotification{Booking: moved, HostName: "Host", HostEmail: "host@example.com",
			HostTimezone: "UTC", EventTitle: "Catch up", EventMinutes: 30},
	}
	cal := &recordingCalendar{}
	worker := &Worker{Queue: &reminderQueue{current: &moved}, Calendar: cal, PublicAppURL: "https://app.test"}
	if err := worker.deliver(context.Background(), job); err != nil {
		t.Fatalf("reschedule sync: %v", err)
	}
	if len(cal.cancelled) != 0 {
		t.Fatalf("reschedule deleted the event instead of moving it: %v", cal.cancelled)
	}
	if len(cal.synced) != 1 || !cal.synced[0].StartsAt.Equal(start) {
		t.Fatalf("event was not moved to the new time: %+v", cal.synced)
	}
}

// The email a reschedule sends has to say so. Both sides get the changed-time
// wording, and each reschedule carries its own idempotency key so a second
// move is not swallowed as a duplicate of the first.
func TestRescheduledEmailsSayRescheduled(t *testing.T) {
	start := time.Now().UTC().Add(48 * time.Hour)
	moved := store.Booking{
		ID: "booking-1", HostID: "host-1", GuestName: "Guest", GuestEmail: "guest@example.com",
		StartsAt: start, EndsAt: start.Add(30 * time.Minute), MeetCode: "abc-def",
		CancelToken: "secret", Status: "confirmed", Revision: 2,
	}
	action := fmt.Sprintf("rescheduled:%d", time.Now().UnixNano())
	worker := &Worker{Queue: &reminderQueue{current: &moved}, PublicAppURL: "https://app.test"}
	for _, channel := range []string{"guest_email", "host_email"} {
		msg := worker.message(store.OutboxJob{
			BookingID: moved.ID, Action: action, Channel: channel,
			Payload: store.BookingNotification{Booking: moved, HostName: "Host", HostEmail: "host@example.com",
				HostTimezone: "UTC", EventTitle: "Catch up", EventMinutes: 30},
		})
		if !strings.HasPrefix(msg.Subject, "Rescheduled:") {
			t.Fatalf("%s subject = %q, want a Rescheduled: prefix", channel, msg.Subject)
		}
		if !strings.Contains(msg.HTMLBody, ">Rescheduled</p>") {
			t.Fatalf("%s body still reads as a new booking: %s", channel, msg.HTMLBody)
		}
		if len(msg.Attachments) != 1 || !strings.Contains(string(msg.Attachments[0].Body), "\r\nSEQUENCE:2\r\n") {
			t.Fatalf("%s calendar attachment lost booking revision: %+v", channel, msg.Attachments)
		}
		if msg.IdempotencyKey != "booking/booking-1/"+action+"/"+channel {
			t.Fatalf("%s idempotency key = %q", channel, msg.IdempotencyKey)
		}
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
