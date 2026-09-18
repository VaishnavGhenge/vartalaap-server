package notifications

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/mail"
	"strings"
	"time"

	"github.com/vaishnavghenge/vartalaap-server/internal/calendar"
	"github.com/vaishnavghenge/vartalaap-server/internal/email"
	"github.com/vaishnavghenge/vartalaap-server/internal/metrics"
	"github.com/vaishnavghenge/vartalaap-server/internal/store"
)

type Queue interface {
	ClaimOutboxJob(context.Context) (*store.OutboxJob, error)
	FinishOutboxJob(context.Context, store.OutboxJob, time.Time, string) error
	SaveOutboxDelivery(context.Context, store.OutboxJob, []byte) ([]byte, error)
	OutboxBacklog(context.Context) (int64, float64, error)
	GetBookingByID(context.Context, string) (*store.Booking, error)
	GetCalendarConnection(context.Context, string, string) (*store.CalendarConnection, error)
}

type Worker struct {
	Queue        Queue
	Mailer       email.Mailer
	Calendar     calendar.BookingSync
	PublicAppURL string
}

func (w *Worker) Run(ctx context.Context) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			worked, err := w.ProcessOne(ctx)
			if err != nil && ctx.Err() == nil {
				slog.Error("outbox: processing failed", "err", err)
			}
			delay := time.Second
			if worked && err == nil {
				delay = 50 * time.Millisecond
			}
			timer.Reset(delay)
		}
	}
}

func (w *Worker) ProcessOne(parent context.Context) (bool, error) {
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	job, err := w.Queue.ClaimOutboxJob(ctx)
	if err != nil {
		return false, err
	}
	count, oldest, statsErr := w.Queue.OutboxBacklog(ctx)
	if statsErr == nil {
		metrics.OutboxPending.Set(float64(count))
		metrics.OutboxOldest.Set(oldest)
	}
	if job == nil {
		return false, nil
	}
	start := time.Now()
	metrics.OutboxActive.Inc()
	err = w.deliver(ctx, *job)
	metrics.OutboxActive.Dec()
	result := "success"
	failure := ""
	if err != nil {
		result = "retry"
		failure = "provider_error"
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			failure = "timeout"
		}
		if errors.Is(err, calendar.ErrReconnectRequired) {
			failure = "reconnect_required"
		}
		slog.Warn("outbox: delivery deferred", "job_id", job.ID, "channel", job.Channel, "attempt", job.Attempts, "reason", failure)
	}
	metrics.OutboxAttempts.WithLabelValues(job.Channel, result).Inc()
	metrics.OutboxDuration.WithLabelValues(job.Channel).Observe(time.Since(start).Seconds())
	// A delivery timeout must not prevent persisting its retry schedule.
	finishCtx, finishCancel := context.WithTimeout(context.WithoutCancel(parent), 5*time.Second)
	defer finishCancel()
	return true, w.Queue.FinishOutboxJob(finishCtx, *job, time.Now().Add(retryDelay(job.Attempts)), failure)
}

func retryDelay(attempt int) time.Duration {
	base := min(30*time.Second*time.Duration(1<<min(max(attempt-1, 0), 7)), time.Hour)
	return base/2 + time.Duration(rand.Int64N(int64(base/2)+1))
}

func (w *Worker) deliver(ctx context.Context, job store.OutboxJob) error {
	current, err := w.Queue.GetBookingByID(ctx, job.BookingID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	b := job.Payload.Booking
	if _, reminder := reminderLead(job.Action); reminder {
		// The payload is a snapshot of the time this reminder was scheduled
		// for. A cancellation or reschedule makes that snapshot stale; finish
		// it silently even if it was already claimed when the booking changed.
		if current.Status != "confirmed" || !current.StartsAt.Equal(b.StartsAt) || !time.Now().Before(current.StartsAt) {
			return nil
		}
	}
	if job.Channel == "calendar" {
		if w.Calendar == nil {
			_, err := w.Queue.GetCalendarConnection(ctx, b.HostID, "google")
			if errors.Is(err, store.ErrNotFound) {
				return nil
			}
			if err != nil {
				return err
			}
			return errors.New("calendar integration is not configured")
		}
		if current.Status == "cancelled" {
			return w.Calendar.SyncBookingCancelled(ctx, b.HostID, b.ID)
		}
		if time.Now().After(current.EndsAt) {
			return nil
		}
		// Create and reschedule are the same write: SyncBooking moves an
		// existing event rather than deleting and re-inserting it, so a
		// reschedule needs no separate branch here.
		return w.Calendar.SyncBooking(ctx, calendar.BookingEvent{
			BookingID: b.ID, HostID: b.HostID, HostTimezone: job.Payload.HostTimezone,
			EventTitle: job.Payload.EventTitle, GuestName: b.GuestName, GuestEmail: b.GuestEmail,
			StartsAt: b.StartsAt, EndsAt: b.EndsAt, MeetCode: b.MeetCode,
			RoomURL: strings.TrimRight(w.PublicAppURL, "/") + "/room/" + b.MeetCode,
		})
	}
	if (job.Action == "created" || strings.HasPrefix(job.Action, "rescheduled:")) &&
		(current.Status == "cancelled" || time.Now().After(current.EndsAt)) {
		return nil
	}
	if w.Mailer == nil {
		return errors.New("email is not configured")
	}
	if _, logOnly := w.Mailer.(*email.LogMailer); logOnly {
		return errors.New("email is log-only; delivery remains pending")
	}
	raw := job.DeliveryPayload
	if len(raw) == 0 {
		msg := email.Prepare(w.Mailer, w.message(job))
		raw, err = json.Marshal(msg)
		if err != nil {
			return err
		}
		raw, err = w.Queue.SaveOutboxDelivery(ctx, job, raw)
		if err != nil {
			return err
		}
	}
	var msg email.Message
	if err := json.Unmarshal(raw, &msg); err != nil {
		return err
	}
	return w.Mailer.Send(ctx, msg)
}

func (w *Worker) message(job store.OutboxJob) email.Message {
	p := job.Payload
	b := p.Booking
	in := email.BookingInput{
		CreatedAt: job.CreatedAt, GuestName: b.GuestName, GuestEmail: b.GuestEmail,
		HostName: p.HostName, HostEmail: p.HostEmail, HostTimezone: p.HostTimezone,
		EventTitle: p.EventTitle, EventMinutes: p.EventMinutes,
		StartsAt: b.StartsAt, EndsAt: b.EndsAt, MeetCode: b.MeetCode,
		Sequence: b.Revision, CancelToken: b.CancelToken, PublicAppURL: w.PublicAppURL,
	}
	var msg email.Message
	if lead, reminder := reminderLead(job.Action); reminder {
		msg = email.RenderBookingReminder(in, "", job.Channel == "host_email", lead)
	} else if job.Action == "cancelled" {
		by := ""
		if b.CancelledBy != nil {
			by = *b.CancelledBy
		}
		if b.CancellationReason != nil {
			in.CancellationReason = *b.CancellationReason
		}
		msg = email.RenderBookingCancellation(in, "", by)
		address := mail.Address{Name: b.GuestName, Address: b.GuestEmail}
		if job.Channel == "host_email" {
			address = mail.Address{Name: p.HostName, Address: p.HostEmail}
		}
		msg.To = []string{address.String()}
	} else if strings.HasPrefix(job.Action, "rescheduled:") {
		msg = email.RenderBookingRescheduled(in, "", job.Channel == "host_email")
	} else if job.Channel == "host_email" {
		msg = email.RenderBookingNotification(in, "")
	} else {
		msg = email.RenderBookingConfirmation(in, "")
	}
	msg.IdempotencyKey = fmt.Sprintf("booking/%s/%s/%s", b.ID, job.Action, job.Channel)
	return msg
}

func reminderLead(action string) (time.Duration, bool) {
	if strings.HasPrefix(action, "reminder:24h:") {
		return 24 * time.Hour, true
	}
	if strings.HasPrefix(action, "reminder:1h:") {
		return time.Hour, true
	}
	return 0, false
}
