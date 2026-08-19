package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// CalendarConnection is one host's link to an external calendar. AccessToken
// and RefreshToken hold ciphertext produced by internal/secretbox — this
// struct never carries plaintext credentials, so a stray log of it leaks
// nothing usable.
//
// RevokedAt is the tombstone for a grant Google has permanently rejected. The
// row survives so the dashboard can distinguish "reconnect this" from "never
// connected", which are different prompts to the host.
type CalendarConnection struct {
	ID           string
	UserID       string
	Provider     string
	AccountEmail *string
	AccessToken  string
	RefreshToken string
	ExpiresAt    *time.Time
	CalendarID   string
	RevokedAt    *time.Time
	LastError    *string
	LastSyncedAt *time.Time
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// Connected reports whether this connection should be used. A revoked grant
// is still a row, but it is not a working calendar.
func (c *CalendarConnection) Connected() bool {
	return c != nil && c.RevokedAt == nil
}

// BookingCalendarEvent maps a booking to the event we created for it in an
// external calendar, so cancellation can find and delete the right one.
type BookingCalendarEvent struct {
	BookingID  string
	Provider   string
	EventID    string
	CalendarID string
	CreatedAt  time.Time
}

const calendarConnCols = `id, user_id, provider, account_email, access_token, refresh_token,
	expires_at, calendar_id, revoked_at, last_error, last_synced_at, created_at, updated_at`

func scanCalendarConnection(row pgx.Row, c *CalendarConnection) error {
	return row.Scan(
		&c.ID, &c.UserID, &c.Provider, &c.AccountEmail,
		&c.AccessToken, &c.RefreshToken, &c.ExpiresAt, &c.CalendarID,
		&c.RevokedAt, &c.LastError, &c.LastSyncedAt, &c.CreatedAt, &c.UpdatedAt,
	)
}

// UpsertCalendarConnection stores a freshly granted connection, replacing any
// existing one for the same (user, provider).
//
// The upsert clears revoked_at and last_error: reconnecting is precisely the
// action that resolves a revoked grant, and leaving the tombstone set would
// keep the dashboard showing "reconnect" after the host just did.
func (s *Store) UpsertCalendarConnection(ctx context.Context, c CalendarConnection) (*CalendarConnection, error) {
	if c.Provider == "" {
		c.Provider = "google"
	}
	if c.CalendarID == "" {
		c.CalendarID = "primary"
	}
	out := &CalendarConnection{}
	err := scanCalendarConnection(s.pool.QueryRow(ctx,
		`INSERT INTO calendar_connections
		   (user_id, provider, account_email, access_token, refresh_token,
		    expires_at, calendar_id)
		 VALUES ($1,$2,$3,$4,$5,$6,$7)
		 ON CONFLICT (user_id, provider) DO UPDATE SET
		   account_email = EXCLUDED.account_email,
		   access_token  = EXCLUDED.access_token,
		   refresh_token = EXCLUDED.refresh_token,
		   expires_at    = EXCLUDED.expires_at,
		   calendar_id   = EXCLUDED.calendar_id,
		   revoked_at    = NULL,
		   last_error    = NULL,
		   updated_at    = now()
		 RETURNING `+calendarConnCols,
		c.UserID, c.Provider, c.AccountEmail, c.AccessToken, c.RefreshToken,
		c.ExpiresAt, c.CalendarID,
	), out)
	if err != nil {
		return nil, fmt.Errorf("store: upsert calendar connection: %w", err)
	}
	return out, nil
}

// GetCalendarConnection returns the host's connection, revoked or not. The
// caller decides what a revoked connection means in its context — slot
// generation skips it, the status endpoint reports it.
func (s *Store) GetCalendarConnection(ctx context.Context, userID, provider string) (*CalendarConnection, error) {
	if provider == "" {
		provider = "google"
	}
	out := &CalendarConnection{}
	err := scanCalendarConnection(s.pool.QueryRow(ctx,
		`SELECT `+calendarConnCols+`
		 FROM calendar_connections WHERE user_id = $1 AND provider = $2`,
		userID, provider,
	), out)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("store: get calendar connection: %w", err)
	}
	return out, nil
}

// UpdateCalendarAccessToken persists a refreshed access token. Google only
// issues a refresh token on first consent, so this deliberately does not touch
// refresh_token — overwriting it with the empty string on every refresh would
// break the connection an hour later.
func (s *Store) UpdateCalendarAccessToken(ctx context.Context, id, encAccessToken string, expiresAt time.Time) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE calendar_connections
		 SET access_token = $2, expires_at = $3, updated_at = now()
		 WHERE id = $1`,
		id, encAccessToken, expiresAt,
	)
	if err != nil {
		return fmt.Errorf("store: update calendar access token: %w", err)
	}
	return nil
}

// MarkCalendarRevoked tombstones a connection Google has permanently rejected.
// Idempotent: re-marking an already-revoked row keeps the original timestamp so
// "how long has this host been disconnected" stays answerable.
func (s *Store) MarkCalendarRevoked(ctx context.Context, id, reason string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE calendar_connections
		 SET revoked_at = COALESCE(revoked_at, now()), last_error = $2, updated_at = now()
		 WHERE id = $1`,
		id, reason,
	)
	if err != nil {
		return fmt.Errorf("store: mark calendar revoked: %w", err)
	}
	return nil
}

// RecordCalendarSync stamps the outcome of a sync attempt. Pass nil on
// success; the stored error is what the dashboard shows when sync is silently
// degraded, which is the failure mode a host would otherwise never notice.
func (s *Store) RecordCalendarSync(ctx context.Context, id string, syncErr *string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE calendar_connections
		 SET last_synced_at = now(), last_error = $2, updated_at = now()
		 WHERE id = $1`,
		id, syncErr,
	)
	if err != nil {
		return fmt.Errorf("store: record calendar sync: %w", err)
	}
	return nil
}

// DeleteCalendarConnection removes the host's connection entirely. Used by
// explicit disconnect, where the host's intent is "forget this", unlike the
// revoked case where we keep the row to prompt a reconnect.
func (s *Store) DeleteCalendarConnection(ctx context.Context, userID, provider string) error {
	if provider == "" {
		provider = "google"
	}
	_, err := s.pool.Exec(ctx,
		`DELETE FROM calendar_connections WHERE user_id = $1 AND provider = $2`,
		userID, provider,
	)
	if err != nil {
		return fmt.Errorf("store: delete calendar connection: %w", err)
	}
	return nil
}

// CreateBookingCalendarEvent records a mirrored event. ON CONFLICT DO UPDATE
// rather than DO NOTHING because a re-sync after a partial failure should
// leave the row pointing at the event that actually exists.
func (s *Store) CreateBookingCalendarEvent(ctx context.Context, e BookingCalendarEvent) error {
	if e.Provider == "" {
		e.Provider = "google"
	}
	if e.CalendarID == "" {
		e.CalendarID = "primary"
	}
	_, err := s.pool.Exec(ctx,
		`INSERT INTO booking_calendar_events (booking_id, provider, event_id, calendar_id)
		 VALUES ($1,$2,$3,$4)
		 ON CONFLICT (booking_id, provider) DO UPDATE SET
		   event_id = EXCLUDED.event_id, calendar_id = EXCLUDED.calendar_id`,
		e.BookingID, e.Provider, e.EventID, e.CalendarID,
	)
	if err != nil {
		return fmt.Errorf("store: create booking calendar event: %w", err)
	}
	return nil
}

func (s *Store) GetBookingCalendarEvent(ctx context.Context, bookingID, provider string) (*BookingCalendarEvent, error) {
	if provider == "" {
		provider = "google"
	}
	out := &BookingCalendarEvent{}
	err := s.pool.QueryRow(ctx,
		`SELECT booking_id, provider, event_id, calendar_id, created_at
		 FROM booking_calendar_events WHERE booking_id = $1 AND provider = $2`,
		bookingID, provider,
	).Scan(&out.BookingID, &out.Provider, &out.EventID, &out.CalendarID, &out.CreatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("store: get booking calendar event: %w", err)
	}
	return out, nil
}

func (s *Store) DeleteBookingCalendarEvent(ctx context.Context, bookingID, provider string) error {
	if provider == "" {
		provider = "google"
	}
	_, err := s.pool.Exec(ctx,
		`DELETE FROM booking_calendar_events WHERE booking_id = $1 AND provider = $2`,
		bookingID, provider,
	)
	if err != nil {
		return fmt.Errorf("store: delete booking calendar event: %w", err)
	}
	return nil
}
