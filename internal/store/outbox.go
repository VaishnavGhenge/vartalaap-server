package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

type BookingNotification struct {
	Booking      Booking
	HostName     string
	HostEmail    string
	HostTimezone string
	EventTitle   string
	EventMinutes int
}

type OutboxJob struct {
	DeliveryPayload []byte
	ID              int64
	BookingID       string
	Action          string
	Channel         string
	Payload         BookingNotification
	CreatedAt       time.Time
	Attempts        int
	LeaseToken      string
}

func enqueueBookingNotifications(ctx context.Context, tx pgx.Tx, b Booking, action string) error {
	if action == "cancelled" || strings.HasPrefix(action, "rescheduled:") {
		if _, err := tx.Exec(ctx, `UPDATE booking_outbox
			SET completed_at=now(), lease_until=NULL, lease_token=NULL, last_error=NULL,
				payload='{}'::jsonb, delivery_payload=NULL
			WHERE booking_id=$1 AND completed_at IS NULL AND action LIKE 'reminder:%'`, b.ID); err != nil {
			return err
		}
		// Don't leave cancellation behind an hour-long create retry. The
		// earlier job will re-read the booking and converge to the current
		// state. Reschedules do the same so earlier lifecycle jobs cannot block
		// the changed-time email behind a retry delay.
		if _, err := tx.Exec(ctx, `UPDATE booking_outbox SET available_at=now()
			WHERE booking_id=$1 AND completed_at IS NULL`, b.ID); err != nil {
			return err
		}
	}
	payload := BookingNotification{Booking: b}
	if err := tx.QueryRow(ctx, `SELECT u.name, u.email, u.timezone, e.title, e.duration_min
		FROM users u JOIN event_types e ON e.host_id=u.id WHERE u.id=$1 AND e.id=$2`, b.HostID, b.EventTypeID).
		Scan(&payload.HostName, &payload.HostEmail, &payload.HostTimezone, &payload.EventTitle, &payload.EventMinutes); err != nil {
		return fmt.Errorf("outbox: snapshot booking: %w", err)
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO booking_outbox(booking_id, action, channel, payload)
		SELECT $1, $2, channel, $3::jsonb FROM unnest(ARRAY['guest_email','host_email','calendar']) AS channel
		ON CONFLICT (booking_id, action, channel) DO NOTHING`, b.ID, action, raw)
	if err != nil {
		return err
	}
	if action == "created" || strings.HasPrefix(action, "rescheduled:") {
		return enqueueBookingReminders(ctx, tx, b, raw)
	}
	return nil
}

func enqueueBookingReminders(ctx context.Context, tx pgx.Tx, b Booking, payload []byte) error {
	for _, reminder := range []struct {
		label  string
		before time.Duration
	}{
		{label: "24h", before: 24 * time.Hour},
		{label: "1h", before: time.Hour},
	} {
		action := fmt.Sprintf("reminder:%s:%d", reminder.label, b.StartsAt.UTC().UnixNano())
		availableAt := b.StartsAt.UTC().Add(-reminder.before)
		if _, err := tx.Exec(ctx, `INSERT INTO booking_outbox(booking_id, action, channel, payload, available_at)
			SELECT $1, $2, channel, $3::jsonb, $4
			FROM unnest(ARRAY['guest_email','host_email']) AS channel
			WHERE $4 > now()
			ON CONFLICT (booking_id, action, channel) DO NOTHING`, b.ID, action, payload, availableAt); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) ClaimOutboxJob(ctx context.Context) (*OutboxJob, error) {
	var job OutboxJob
	var raw []byte
	err := s.pool.QueryRow(ctx, `WITH candidate AS (
		SELECT q.id FROM booking_outbox q
		WHERE q.completed_at IS NULL AND q.available_at <= now()
		AND (q.lease_until IS NULL OR q.lease_until < now())
		AND NOT EXISTS (SELECT 1 FROM booking_outbox earlier
			WHERE earlier.booking_id=q.booking_id AND earlier.channel=q.channel
			AND earlier.id < q.id AND earlier.completed_at IS NULL)
		ORDER BY q.available_at, q.id FOR UPDATE OF q SKIP LOCKED LIMIT 1
	) UPDATE booking_outbox q SET lease_until=now()+interval '2 minutes',
		lease_token=gen_random_uuid(), attempts=attempts+1
	FROM candidate WHERE q.id=candidate.id
	RETURNING q.id, q.booking_id, q.action, q.channel, q.payload, q.created_at, q.attempts, q.lease_token, q.delivery_payload`,
	).Scan(&job.ID, &job.BookingID, &job.Action, &job.Channel, &raw, &job.CreatedAt, &job.Attempts, &job.LeaseToken, &job.DeliveryPayload)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("outbox: claim: %w", err)
	}
	if err := json.Unmarshal(raw, &job.Payload); err != nil {
		return nil, fmt.Errorf("outbox: payload: %w", err)
	}
	return &job, nil
}

func (s *Store) SaveOutboxDelivery(ctx context.Context, job OutboxJob, payload []byte) ([]byte, error) {
	var saved []byte
	err := s.pool.QueryRow(ctx, `UPDATE booking_outbox SET delivery_payload=COALESCE(delivery_payload,$3::jsonb)
		WHERE id=$1 AND lease_token=$2 RETURNING delivery_payload`, job.ID, job.LeaseToken, payload).Scan(&saved)
	return saved, err
}

func (s *Store) FinishOutboxJob(ctx context.Context, job OutboxJob, retryAt time.Time, failure string) error {
	var err error
	var id int64
	if failure == "" {
		err = s.pool.QueryRow(ctx, `UPDATE booking_outbox SET completed_at=now(), lease_until=NULL, lease_token=NULL, last_error=NULL,
			payload='{}'::jsonb, delivery_payload=NULL
			WHERE id=$1 AND lease_token=$2 RETURNING id`, job.ID, job.LeaseToken).Scan(&id)
	} else {
		err = s.pool.QueryRow(ctx, `UPDATE booking_outbox SET available_at=$3, last_error=$4, lease_until=NULL, lease_token=NULL
			WHERE id=$1 AND lease_token=$2 RETURNING id`, job.ID, job.LeaseToken, retryAt, failure).Scan(&id)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrConflict
	}
	return err
}

func (s *Store) OutboxBacklog(ctx context.Context) (int64, float64, error) {
	var count int64
	var oldest float64
	err := s.pool.QueryRow(ctx, `SELECT count(*), COALESCE(EXTRACT(EPOCH FROM now()-min(
		CASE WHEN action LIKE 'reminder:%' AND attempts=0 THEN available_at ELSE created_at END
	)),0)::float8
		FROM booking_outbox WHERE completed_at IS NULL
		AND (action NOT LIKE 'reminder:%' OR attempts > 0 OR available_at <= now())`).Scan(&count, &oldest)
	return count, oldest, err
}
