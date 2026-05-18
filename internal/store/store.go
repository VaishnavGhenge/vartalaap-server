package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrNotFound = errors.New("not found")
var ErrConflict = errors.New("conflict")

// Storer is the interface the auth and scheduling handlers depend on.
// *Store satisfies it; tests can provide an in-memory double.
type Storer interface {
	CreateUser(ctx context.Context, email, name, slug, passwordHash string) (*User, error)
	GetUserByEmail(ctx context.Context, email string) (*User, error)
	GetUserByID(ctx context.Context, id string) (*User, error)
	GetUserBySlug(ctx context.Context, slug string) (*User, error)
	SlugExists(ctx context.Context, slug string) (bool, error)
	UpdateProfile(ctx context.Context, userID, name, slug, timezone string, onboardingStep int) (*User, error)
	CreateRefreshToken(ctx context.Context, userID, tokenHash string, expiresAt time.Time) error
	GetRefreshToken(ctx context.Context, tokenHash string) (*RefreshToken, error)
	DeleteRefreshToken(ctx context.Context, tokenHash string) error

	// Phase 2 — scheduling
	ListAvailability(ctx context.Context, hostID string) ([]AvailabilityRule, error)
	ReplaceAvailability(ctx context.Context, hostID string, rules []AvailabilityRule) ([]AvailabilityRule, error)

	ListEventTypes(ctx context.Context, hostID string, activeOnly bool) ([]EventType, error)
	GetEventType(ctx context.Context, hostID, id string) (*EventType, error)
	GetEventTypeBySlug(ctx context.Context, hostID, slug string) (*EventType, error)
	CreateEventType(ctx context.Context, e EventType) (*EventType, error)
	UpdateEventType(ctx context.Context, e EventType) (*EventType, error)
	DeleteEventType(ctx context.Context, hostID, id string) error
	CountActiveEventTypes(ctx context.Context, hostID string) (int, error)

	CreateBooking(ctx context.Context, b Booking) (*Booking, error)
	GetBookingByID(ctx context.Context, id string) (*Booking, error)
	GetBookingByMeetCode(ctx context.Context, meetCode string) (*Booking, error)
	ListBookingsForHost(ctx context.Context, hostID string, fromUTC time.Time, limit int) ([]Booking, error)
	ListBookingsForEventInRange(ctx context.Context, eventTypeID string, fromUTC, toUTC time.Time) ([]Booking, error)
	CancelBooking(ctx context.Context, id string) error
	CountBookingsInMonth(ctx context.Context, hostID string, year int, month time.Month) (int, error)

	// Slot holds: short-lived reservations created when a guest taps a slot
	// in the picker, released on submit/abandon/TTL. Scoped by host (not
	// event_type) so a host's separate event types share calendar time.
	CreateSlotHold(ctx context.Context, h SlotHold) (*SlotHold, error)
	GetSlotHoldByToken(ctx context.Context, token string) (*SlotHold, error)
	DeleteSlotHold(ctx context.Context, token string) error
	ListActiveHoldsForHostInRange(ctx context.Context, hostID string, fromUTC, toUTC time.Time) ([]SlotHold, error)
}

type User struct {
	ID             string
	Email          string
	Name           string
	Slug           string
	Timezone       string
	OnboardingStep int
	AvatarURL      *string
	PasswordHash   string
	Plan           string
	CreatedAt      time.Time
}

// EventType is a bookable session shape — duration, optional price, and the
// slug a guest sees in the booking URL. Soft-deleted by flipping IsActive
// rather than removing the row so existing bookings keep a foreign-key target.
type EventType struct {
	ID            string
	HostID        string
	Slug          string
	Title         string
	DurationMin   int
	BufferMin     int
	MaxPerDay     *int    // nil = unlimited
	IsPaid        bool
	PriceCents    *int    // nil iff IsPaid=false
	Currency      string
	PaymentTiming string  // "upfront" | "after"
	IsActive      bool
	Description   *string
	CreatedAt     time.Time
}

// Booking is one scheduled session between a host and a guest. Guests are
// represented by name+email only — Sessionly intentionally does not require
// guests to sign up before booking. Status transitions are linear:
// pending_payment → confirmed → completed | cancelled.
type Booking struct {
	ID              string
	EventTypeID     string
	HostID          string
	GuestEmail      string
	GuestName       string
	StartsAt        time.Time
	EndsAt          time.Time
	MeetCode        string
	Status          string  // "pending_payment" | "confirmed" | "cancelled" | "completed"
	StripeSessionID *string
	// CancelToken authorises DELETE /m/{code}. The handler compares the
	// caller's `?t=` query param against this value with constant-time
	// equality. Distinct from MeetCode so leaking the join URL doesn't
	// confer cancel authority.
	CancelToken string
	CreatedAt   time.Time
}

// SlotHold is a short-lived reservation while a guest fills out the booking
// form. Token is the opaque identifier returned to the client; DeleteSlotHold
// and the consume-on-create path both look up by token rather than ID so the
// internal UUID never has to leak.
type SlotHold struct {
	ID          string
	HostID      string
	EventTypeID string
	StartsAt    time.Time
	EndsAt      time.Time
	Token       string
	ExpiresAt   time.Time
	CreatedAt   time.Time
}

// AvailabilityRule is one recurring weekly slot during which a host is bookable.
// DayOfWeek follows JS Date.getDay() (0=Sun..6=Sat). Times are wall-clock in the
// rule's Timezone — not the user's current timezone, because moving timezone
// shouldn't silently shift saved availability.
type AvailabilityRule struct {
	ID        string
	HostID    string
	DayOfWeek int    // 0=Sun .. 6=Sat
	StartTime string // "HH:MM" wall time
	EndTime   string // "HH:MM" wall time, > StartTime
	Timezone  string
}

type RefreshToken struct {
	ID        string
	UserID    string
	TokenHash string
	ExpiresAt time.Time
}

type Store struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

const userCols = `id, email, name, slug, timezone, onboarding_step, avatar_url, password_hash, plan, created_at`

func scanUser(row pgx.Row, u *User) error {
	return row.Scan(&u.ID, &u.Email, &u.Name, &u.Slug, &u.Timezone, &u.OnboardingStep, &u.AvatarURL, &u.PasswordHash, &u.Plan, &u.CreatedAt)
}

func (s *Store) CreateUser(ctx context.Context, email, name, slug, passwordHash string) (*User, error) {
	u := &User{}
	err := scanUser(s.pool.QueryRow(ctx,
		`INSERT INTO users (email, name, slug, password_hash)
		 VALUES ($1, $2, $3, $4)
		 RETURNING `+userCols,
		email, name, slug, passwordHash,
	), u)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrConflict
		}
		return nil, fmt.Errorf("store: create user: %w", err)
	}
	return u, nil
}

func (s *Store) GetUserByEmail(ctx context.Context, email string) (*User, error) {
	u := &User{}
	err := scanUser(s.pool.QueryRow(ctx,
		`SELECT `+userCols+` FROM users WHERE email = $1`, email,
	), u)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("store: get user by email: %w", err)
	}
	return u, nil
}

// GetUserBySlug resolves the public profile URL (`/u/{slug}`) to a user.
// Slug is unique site-wide (enforced by the schema), so no host disambiguation
// is required.
func (s *Store) GetUserBySlug(ctx context.Context, slug string) (*User, error) {
	u := &User{}
	err := scanUser(s.pool.QueryRow(ctx,
		`SELECT `+userCols+` FROM users WHERE slug = $1`, slug,
	), u)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("store: get user by slug: %w", err)
	}
	return u, nil
}

func (s *Store) GetUserByID(ctx context.Context, id string) (*User, error) {
	u := &User{}
	err := scanUser(s.pool.QueryRow(ctx,
		`SELECT `+userCols+` FROM users WHERE id = $1`, id,
	), u)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("store: get user by id: %w", err)
	}
	return u, nil
}

func (s *Store) UpdateProfile(ctx context.Context, userID, name, slug, timezone string, onboardingStep int) (*User, error) {
	u := &User{}
	err := scanUser(s.pool.QueryRow(ctx,
		`UPDATE users SET name=$2, slug=$3, timezone=$4, onboarding_step=$5
		 WHERE id=$1
		 RETURNING `+userCols,
		userID, name, slug, timezone, onboardingStep,
	), u)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		if isUniqueViolation(err) {
			return nil, ErrConflict
		}
		return nil, fmt.Errorf("store: update profile: %w", err)
	}
	return u, nil
}

func (s *Store) SlugExists(ctx context.Context, slug string) (bool, error) {
	var exists bool
	err := s.pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM users WHERE slug = $1)`, slug,
	).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("store: slug exists: %w", err)
	}
	return exists, nil
}

func (s *Store) CreateRefreshToken(ctx context.Context, userID, tokenHash string, expiresAt time.Time) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO refresh_tokens (user_id, token_hash, expires_at) VALUES ($1, $2, $3)`,
		userID, tokenHash, expiresAt,
	)
	if err != nil {
		return fmt.Errorf("store: create refresh token: %w", err)
	}
	return nil
}

func (s *Store) GetRefreshToken(ctx context.Context, tokenHash string) (*RefreshToken, error) {
	rt := &RefreshToken{}
	err := s.pool.QueryRow(ctx,
		`SELECT id, user_id, token_hash, expires_at
		 FROM refresh_tokens WHERE token_hash = $1`,
		tokenHash,
	).Scan(&rt.ID, &rt.UserID, &rt.TokenHash, &rt.ExpiresAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("store: get refresh token: %w", err)
	}
	return rt, nil
}

func (s *Store) DeleteRefreshToken(ctx context.Context, tokenHash string) error {
	_, err := s.pool.Exec(ctx,
		`DELETE FROM refresh_tokens WHERE token_hash = $1`, tokenHash,
	)
	if err != nil {
		return fmt.Errorf("store: delete refresh token: %w", err)
	}
	return nil
}

func (s *Store) DeleteExpiredRefreshTokens(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM refresh_tokens WHERE expires_at < now()`)
	if err != nil {
		return fmt.Errorf("store: delete expired tokens: %w", err)
	}
	return nil
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// ─── Availability ────────────────────────────────────────────────────────────

// ListAvailability returns every rule for a host, ordered by day then start.
// Empty result is not an error — a host with no rules is simply unbookable.
func (s *Store) ListAvailability(ctx context.Context, hostID string) ([]AvailabilityRule, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, host_id, day_of_week,
		        to_char(start_time, 'HH24:MI') AS start_time,
		        to_char(end_time,   'HH24:MI') AS end_time,
		        timezone
		 FROM availability_rules
		 WHERE host_id = $1
		 ORDER BY day_of_week, start_time`,
		hostID,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list availability: %w", err)
	}
	defer rows.Close()

	out := make([]AvailabilityRule, 0)
	for rows.Next() {
		var r AvailabilityRule
		if err := rows.Scan(&r.ID, &r.HostID, &r.DayOfWeek, &r.StartTime, &r.EndTime, &r.Timezone); err != nil {
			return nil, fmt.Errorf("store: scan availability: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate availability: %w", err)
	}
	return out, nil
}

// ReplaceAvailability atomically deletes every existing rule for the host and
// inserts the provided set. The transaction means an availability page that
// the user has been editing can never end up half-applied if the request fails.
//
// Caller is responsible for input validation (day_of_week range, end > start,
// non-empty timezone, count cap). The DB CHECK constraints exist as a backstop
// but should not be the primary error surface — they return generic 5xx-style
// failures that don't tell the UI which row was wrong.
func (s *Store) ReplaceAvailability(ctx context.Context, hostID string, rules []AvailabilityRule) ([]AvailabilityRule, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: replace availability begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx,
		`DELETE FROM availability_rules WHERE host_id = $1`, hostID,
	); err != nil {
		return nil, fmt.Errorf("store: replace availability delete: %w", err)
	}

	for _, r := range rules {
		if _, err := tx.Exec(ctx,
			`INSERT INTO availability_rules (host_id, day_of_week, start_time, end_time, timezone)
			 VALUES ($1, $2, $3::time, $4::time, $5)`,
			hostID, r.DayOfWeek, r.StartTime, r.EndTime, r.Timezone,
		); err != nil {
			return nil, fmt.Errorf("store: replace availability insert: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("store: replace availability commit: %w", err)
	}

	// Re-read so callers receive the canonical rows (with generated IDs and the
	// times round-tripped through Postgres formatting). One extra query is
	// trivial; the API contract benefits from matching the GET shape exactly.
	return s.ListAvailability(ctx, hostID)
}

// ─── Event types ─────────────────────────────────────────────────────────────

const eventTypeCols = `id, host_id, slug, title, duration_min, buffer_min, max_per_day,
		is_paid, price_cents, currency, payment_timing, is_active, description, created_at`

func scanEventType(row pgx.Row, e *EventType) error {
	return row.Scan(
		&e.ID, &e.HostID, &e.Slug, &e.Title,
		&e.DurationMin, &e.BufferMin, &e.MaxPerDay,
		&e.IsPaid, &e.PriceCents, &e.Currency, &e.PaymentTiming,
		&e.IsActive, &e.Description, &e.CreatedAt,
	)
}

// ListEventTypes returns a host's event types ordered by created_at ascending.
// activeOnly=true filters to is_active=true — used for the public profile
// endpoint where soft-deleted events should be invisible.
func (s *Store) ListEventTypes(ctx context.Context, hostID string, activeOnly bool) ([]EventType, error) {
	q := `SELECT ` + eventTypeCols + ` FROM event_types WHERE host_id = $1`
	if activeOnly {
		q += ` AND is_active = true`
	}
	q += ` ORDER BY created_at ASC`

	rows, err := s.pool.Query(ctx, q, hostID)
	if err != nil {
		return nil, fmt.Errorf("store: list event types: %w", err)
	}
	defer rows.Close()

	out := make([]EventType, 0)
	for rows.Next() {
		var e EventType
		if err := scanEventType(rows, &e); err != nil {
			return nil, fmt.Errorf("store: scan event type: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate event types: %w", err)
	}
	return out, nil
}

// GetEventTypeBySlug resolves the (host, event slug) pair used in public URLs
// like /u/{host}/{event}. host_id is required to disambiguate — two hosts can
// share an event slug.
func (s *Store) GetEventTypeBySlug(ctx context.Context, hostID, slug string) (*EventType, error) {
	e := &EventType{}
	err := scanEventType(s.pool.QueryRow(ctx,
		`SELECT `+eventTypeCols+` FROM event_types WHERE host_id = $1 AND slug = $2`,
		hostID, slug,
	), e)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("store: get event type by slug: %w", err)
	}
	return e, nil
}

// GetEventType returns one event by ID, scoped to host_id so a request that
// guessed an ID cannot read another host's data.
func (s *Store) GetEventType(ctx context.Context, hostID, id string) (*EventType, error) {
	e := &EventType{}
	err := scanEventType(s.pool.QueryRow(ctx,
		`SELECT `+eventTypeCols+` FROM event_types WHERE id = $1 AND host_id = $2`,
		id, hostID,
	), e)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("store: get event type: %w", err)
	}
	return e, nil
}

// CreateEventType inserts a new event. Caller is responsible for input
// validation and plan-tier enforcement; the DB CHECK constraints are a safety
// net, not the primary error surface.
func (s *Store) CreateEventType(ctx context.Context, e EventType) (*EventType, error) {
	out := &EventType{}
	err := scanEventType(s.pool.QueryRow(ctx,
		`INSERT INTO event_types
		   (host_id, slug, title, duration_min, buffer_min, max_per_day,
		    is_paid, price_cents, currency, payment_timing, is_active, description)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
		 RETURNING `+eventTypeCols,
		e.HostID, e.Slug, e.Title, e.DurationMin, e.BufferMin, e.MaxPerDay,
		e.IsPaid, e.PriceCents, e.Currency, e.PaymentTiming, e.IsActive, e.Description,
	), out)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrConflict
		}
		return nil, fmt.Errorf("store: create event type: %w", err)
	}
	return out, nil
}

// UpdateEventType applies a full overwrite of mutable fields. The ID and
// host_id come from e and the WHERE clause enforces host ownership.
func (s *Store) UpdateEventType(ctx context.Context, e EventType) (*EventType, error) {
	out := &EventType{}
	err := scanEventType(s.pool.QueryRow(ctx,
		`UPDATE event_types
		    SET slug=$3, title=$4, duration_min=$5, buffer_min=$6, max_per_day=$7,
		        is_paid=$8, price_cents=$9, currency=$10, payment_timing=$11,
		        is_active=$12, description=$13
		  WHERE id=$1 AND host_id=$2
		  RETURNING `+eventTypeCols,
		e.ID, e.HostID, e.Slug, e.Title, e.DurationMin, e.BufferMin, e.MaxPerDay,
		e.IsPaid, e.PriceCents, e.Currency, e.PaymentTiming, e.IsActive, e.Description,
	), out)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		if isUniqueViolation(err) {
			return nil, ErrConflict
		}
		return nil, fmt.Errorf("store: update event type: %w", err)
	}
	return out, nil
}

// DeleteEventType soft-deletes by setting is_active=false. We keep the row so
// the future bookings table can foreign-key into it without losing referential
// integrity when a host removes an event from their booking page. ErrNotFound
// returned if the event doesn't exist or belongs to another host.
func (s *Store) DeleteEventType(ctx context.Context, hostID, id string) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE event_types SET is_active = false WHERE id = $1 AND host_id = $2`,
		id, hostID,
	)
	if err != nil {
		return fmt.Errorf("store: delete event type: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// CountActiveEventTypes powers the free-plan limit check. Counting in SQL
// avoids a round-trip-and-len pattern that races with concurrent creates.
func (s *Store) CountActiveEventTypes(ctx context.Context, hostID string) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM event_types WHERE host_id = $1 AND is_active = true`,
		hostID,
	).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: count event types: %w", err)
	}
	return n, nil
}

// ─── Bookings ────────────────────────────────────────────────────────────────

const bookingCols = `id, event_type_id, host_id, guest_email, guest_name,
		starts_at, ends_at, meet_code, status, stripe_session_id,
		cancel_token, created_at`

func scanBooking(row pgx.Row, b *Booking) error {
	return row.Scan(
		&b.ID, &b.EventTypeID, &b.HostID,
		&b.GuestEmail, &b.GuestName,
		&b.StartsAt, &b.EndsAt,
		&b.MeetCode, &b.Status, &b.StripeSessionID,
		&b.CancelToken, &b.CreatedAt,
	)
}

// CreateBooking inserts a booking. Caller is responsible for generating
// MeetCode (uniqueness re-tried on conflict at the handler layer is cleaner
// than a retry loop here — the handler owns the generation strategy).
func (s *Store) CreateBooking(ctx context.Context, b Booking) (*Booking, error) {
	out := &Booking{}
	// CancelToken: caller passes one when it cares about determinism (tests);
	// otherwise the column default (random uuid hex) fills in. The NULLIF
	// pattern lets us keep one INSERT path for both cases.
	err := scanBooking(s.pool.QueryRow(ctx,
		`INSERT INTO bookings
		   (event_type_id, host_id, guest_email, guest_name,
		    starts_at, ends_at, meet_code, status, stripe_session_id,
		    cancel_token)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,
		         COALESCE(NULLIF($10, ''), replace(gen_random_uuid()::text, '-', '')))
		 RETURNING `+bookingCols,
		b.EventTypeID, b.HostID, b.GuestEmail, b.GuestName,
		b.StartsAt, b.EndsAt, b.MeetCode, b.Status, b.StripeSessionID,
		b.CancelToken,
	), out)
	if err != nil {
		if isUniqueViolation(err) {
			// Meet code collision — caller retries with a fresh code.
			return nil, ErrConflict
		}
		return nil, fmt.Errorf("store: create booking: %w", err)
	}
	return out, nil
}

// GetBookingByID looks up by primary key. Used internally — guests use
// GetBookingByMeetCode because the meet code is the URL fragment they receive.
func (s *Store) GetBookingByID(ctx context.Context, id string) (*Booking, error) {
	out := &Booking{}
	err := scanBooking(s.pool.QueryRow(ctx,
		`SELECT `+bookingCols+` FROM bookings WHERE id = $1`, id,
	), out)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("store: get booking by id: %w", err)
	}
	return out, nil
}

// GetBookingByMeetCode is the lookup the public confirmation page and the
// /ws join flow use. Cancelled bookings still resolve — the handler decides
// whether to surface or hide them based on the calling context.
func (s *Store) GetBookingByMeetCode(ctx context.Context, meetCode string) (*Booking, error) {
	out := &Booking{}
	err := scanBooking(s.pool.QueryRow(ctx,
		`SELECT `+bookingCols+` FROM bookings WHERE meet_code = $1`, meetCode,
	), out)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("store: get booking by meet code: %w", err)
	}
	return out, nil
}

// ListBookingsForHost returns the host's bookings starting at or after fromUTC,
// ordered by starts_at ascending. Cancelled bookings are filtered out — they
// muddy the dashboard "upcoming" view and the API doesn't currently surface
// a "show cancelled" toggle.
func (s *Store) ListBookingsForHost(ctx context.Context, hostID string, fromUTC time.Time, limit int) ([]Booking, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx,
		`SELECT `+bookingCols+` FROM bookings
		 WHERE host_id = $1 AND status <> 'cancelled' AND starts_at >= $2
		 ORDER BY starts_at ASC
		 LIMIT $3`,
		hostID, fromUTC, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list bookings: %w", err)
	}
	defer rows.Close()

	out := make([]Booking, 0)
	for rows.Next() {
		var b Booking
		if err := scanBooking(rows, &b); err != nil {
			return nil, fmt.Errorf("store: scan booking: %w", err)
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate bookings: %w", err)
	}
	return out, nil
}

// ListBookingsForEventInRange returns non-cancelled bookings for one event
// type whose [starts_at, ends_at] overlap [fromUTC, toUTC]. Used by both slot
// generation (subtract booked time from the candidate grid) and the conflict
// guard in POST /bookings (reject overlapping inserts before the SQL CHECK
// would).
//
// The overlap predicate `starts_at < toUTC AND ends_at > fromUTC` is the
// standard half-open-interval overlap and matches the bookings_event_starts_idx
// access pattern.
func (s *Store) ListBookingsForEventInRange(ctx context.Context, eventTypeID string, fromUTC, toUTC time.Time) ([]Booking, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+bookingCols+` FROM bookings
		 WHERE event_type_id = $1 AND status <> 'cancelled'
		   AND starts_at < $3 AND ends_at > $2
		 ORDER BY starts_at ASC`,
		eventTypeID, fromUTC, toUTC,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list bookings in range: %w", err)
	}
	defer rows.Close()

	out := make([]Booking, 0)
	for rows.Next() {
		var b Booking
		if err := scanBooking(rows, &b); err != nil {
			return nil, fmt.Errorf("store: scan booking: %w", err)
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate bookings: %w", err)
	}
	return out, nil
}

// CancelBooking marks a booking as cancelled. We keep the row so the audit
// trail survives — a paid booking that gets refunded later still needs the
// original record. ErrNotFound if the ID doesn't exist or is already cancelled.
func (s *Store) CancelBooking(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE bookings SET status = 'cancelled'
		 WHERE id = $1 AND status <> 'cancelled'`,
		id,
	)
	if err != nil {
		return fmt.Errorf("store: cancel booking: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// CountBookingsInMonth powers the free-plan 10-bookings/month limit. Counts
// only non-cancelled bookings created within the calendar month — the policy
// intentionally measures "bookings taken" not "sessions delivered", since the
// guest could otherwise spam-then-cancel to bypass the cap.
//
// month is the time.Month value (1-12).
// CreateSlotHold inserts a fresh hold. The caller decides the TTL by setting
// ExpiresAt; we don't enforce a maximum because the public POST /holds
// endpoint is the one place that should set it, and that decision lives in
// the handler.
func (s *Store) CreateSlotHold(ctx context.Context, h SlotHold) (*SlotHold, error) {
	var out SlotHold
	err := s.pool.QueryRow(ctx, `
		INSERT INTO slot_holds (host_id, event_type_id, starts_at, ends_at, hold_token, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id, host_id, event_type_id, starts_at, ends_at, hold_token, expires_at, created_at`,
		h.HostID, h.EventTypeID, h.StartsAt, h.EndsAt, h.Token, h.ExpiresAt,
	).Scan(
		&out.ID, &out.HostID, &out.EventTypeID, &out.StartsAt, &out.EndsAt,
		&out.Token, &out.ExpiresAt, &out.CreatedAt,
	)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return nil, ErrConflict
		}
		return nil, fmt.Errorf("store: create slot hold: %w", err)
	}
	return &out, nil
}

func (s *Store) GetSlotHoldByToken(ctx context.Context, token string) (*SlotHold, error) {
	var out SlotHold
	err := s.pool.QueryRow(ctx, `
		SELECT id, host_id, event_type_id, starts_at, ends_at, hold_token, expires_at, created_at
		FROM slot_holds WHERE hold_token = $1`,
		token,
	).Scan(
		&out.ID, &out.HostID, &out.EventTypeID, &out.StartsAt, &out.EndsAt,
		&out.Token, &out.ExpiresAt, &out.CreatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("store: get slot hold: %w", err)
	}
	return &out, nil
}

// DeleteSlotHold removes the row outright. Holds are ephemeral and not
// referenced by anything else, so there's no soft-delete semantics needed —
// once the guest releases (or the booking consumes it), the row is gone.
// Returns nil even if the token doesn't exist; the caller has no business
// distinguishing "already released" from "never existed".
func (s *Store) DeleteSlotHold(ctx context.Context, token string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM slot_holds WHERE hold_token = $1`, token)
	if err != nil {
		return fmt.Errorf("store: delete slot hold: %w", err)
	}
	return nil
}

// ListActiveHoldsForHostInRange returns every non-expired hold for a host
// whose window overlaps [fromUTC, toUTC). Used by the slot generator to
// treat in-progress reservations as if they were already bookings — cross-
// event because a host can't be in two sessions at once.
func (s *Store) ListActiveHoldsForHostInRange(ctx context.Context, hostID string, fromUTC, toUTC time.Time) ([]SlotHold, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, host_id, event_type_id, starts_at, ends_at, hold_token, expires_at, created_at
		FROM slot_holds
		WHERE host_id = $1
		  AND expires_at > now()
		  AND starts_at < $3 AND ends_at > $2
		ORDER BY starts_at`,
		hostID, fromUTC, toUTC,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list slot holds: %w", err)
	}
	defer rows.Close()
	var out []SlotHold
	for rows.Next() {
		var h SlotHold
		if err := rows.Scan(
			&h.ID, &h.HostID, &h.EventTypeID, &h.StartsAt, &h.EndsAt,
			&h.Token, &h.ExpiresAt, &h.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("store: scan slot hold: %w", err)
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

func (s *Store) CountBookingsInMonth(ctx context.Context, hostID string, year int, month time.Month) (int, error) {
	// Calendar month boundaries in UTC. Using created_at (not starts_at) so
	// "a booking taken this month" semantics match the user's expectation —
	// a host who books out their entire next month shouldn't blow this
	// month's quota.
	start := time.Date(year, month, 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 1, 0)
	var n int
	err := s.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM bookings
		 WHERE host_id = $1 AND status <> 'cancelled'
		   AND created_at >= $2 AND created_at < $3`,
		hostID, start, end,
	).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: count bookings in month: %w", err)
	}
	return n, nil
}
