// Package calendar owns Google Calendar sync: the OAuth lifecycle, reading a
// host's busy windows for slot generation, and mirroring bookings into their
// calendar.
//
// It sits between the HTTP layer and internal/gcal so that httpx never touches
// a raw token, and so the encrypt/refresh/revoke rules live in exactly one
// place. httpx depends on the narrow interfaces at the bottom of this file,
// not on *Service, so handler tests can fake calendar behaviour without a
// Google endpoint.
//
// Two failure postures, chosen deliberately and applied consistently:
//
//   - Reads (busy periods) return an error and let the caller degrade. Slot
//     generation keeps working without busy data rather than taking the whole
//     booking page down when Google is having a bad morning.
//   - Writes (mirroring a booking) never fail the caller. The booking is
//     already persisted and both parties already have the meet link; a missing
//     calendar entry is a sync gap to log and count, not a reason to 500 a
//     guest who has finished booking.
package calendar

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/vaishnavghenge/vartalaap-server/internal/gcal"
	"github.com/vaishnavghenge/vartalaap-server/internal/metrics"
	"github.com/vaishnavghenge/vartalaap-server/internal/secretbox"
	"github.com/vaishnavghenge/vartalaap-server/internal/store"
)

const (
	provider = "google"

	// statePurpose scopes the OAuth state token so it cannot be replayed
	// against any other flow. See auth.SignPurposeToken.
	statePurpose = "calendar-oauth"

	// stateTTL only has to outlive a consent screen.
	stateTTL = 10 * time.Minute
)

// ErrNotConnected means this host has no usable calendar: never connected, or
// connected and since revoked. Callers treat it as "no busy data exists",
// which is different from "we failed to fetch busy data".
var ErrNotConnected = errors.New("calendar: not connected")

// ErrReconnectRequired is returned to the HTTP layer when a grant is dead and
// only the host can fix it.
var ErrReconnectRequired = errors.New("calendar: reconnect required")

// Interval is a half-open busy window [Start, End) in UTC.
type Interval struct {
	Start time.Time
	End   time.Time
}

// BookingEvent is everything needed to mirror one booking. Kept flat so the
// calendar package doesn't need the event-type and user rows.
type BookingEvent struct {
	BookingID    string
	HostID       string
	HostTimezone string
	EventTitle   string
	GuestName    string
	GuestEmail   string
	StartsAt     time.Time
	EndsAt       time.Time
	MeetCode     string
	// RoomURL is the absolute join link, written into the calendar entry's
	// location so the host joins from their own calendar rather than digging
	// out the confirmation email.
	RoomURL string
}

// Status is what the dashboard renders.
type Status struct {
	Connected          bool
	NeedsReconnect     bool
	AccountEmail       string
	CalendarID         string
	LastSyncedAt       *time.Time
	LastError          string
	ConfiguredOnServer bool
}

// TokenSigner is the subset of internal/auth the service needs. Injected
// rather than imported so the OAuth state format stays swappable and the
// package has no opinion about JWTs.
type TokenSigner interface {
	SignPurposeToken(userID, purpose, returnTo, secret string, ttl time.Duration) (string, error)
	VerifyPurposeToken(tokenStr, purpose, secret string) (userID, returnTo string, err error)
}

// returnPaths is the closed set of destinations the OAuth callback may bounce
// a browser to, keyed by the token callers pass as ?return=. Closed on purpose:
// the value survives a round-trip through Google, and an open redirect is
// exactly the bug that turns "connect your calendar" into a phishing primitive.
var returnPaths = map[string]string{
	"dashboard":  "/dashboard",
	"onboarding": "/onboarding",
}

const defaultReturn = "dashboard"

// ResolveReturn maps a caller-supplied key to a path, falling back to the
// dashboard for anything unrecognised.
func ResolveReturn(key string) string {
	if p, ok := returnPaths[key]; ok {
		return p
	}
	return returnPaths[defaultReturn]
}

type Service struct {
	store  store.Storer
	client *gcal.Client
	box    *secretbox.Box
	signer TokenSigner
	secret string

	// publicAppURL is the frontend base the callback bounces back to. The
	// callback is a top-level navigation, so it has to end somewhere a human
	// can see.
	publicAppURL string
}

type Options struct {
	Store        store.Storer
	Client       *gcal.Client
	Box          *secretbox.Box
	Signer       TokenSigner
	JWTSecret    string
	PublicAppURL string
}

func NewService(o Options) *Service {
	return &Service{
		store:        o.Store,
		client:       o.Client,
		box:          o.Box,
		signer:       o.Signer,
		secret:       o.JWTSecret,
		publicAppURL: strings.TrimSuffix(o.PublicAppURL, "/"),
	}
}

// RedirectTo builds the absolute browser destination for a return path.
func (s *Service) RedirectTo(path string) string {
	if path == "" {
		path = returnPaths[defaultReturn]
	}
	return s.publicAppURL + path
}

// ─── OAuth lifecycle ─────────────────────────────────────────────────────────

// AuthURL returns the Google consent URL for this host. The state parameter is
// a signed, 10-minute, purpose-scoped token carrying the user ID, which is how
// the callback identifies the host: the callback is a browser navigation from
// Google and carries no Authorization header of ours.
// returnKey names where to send the browser afterwards; unrecognised values
// fall back to the dashboard.
func (s *Service) AuthURL(userID, returnKey string) (string, error) {
	state, err := s.signer.SignPurposeToken(userID, statePurpose, ResolveReturn(returnKey), s.secret, stateTTL)
	if err != nil {
		return "", fmt.Errorf("calendar: sign state: %w", err)
	}
	return s.client.AuthCodeURL(state), nil
}

// Complete handles the OAuth callback: verify state, exchange the code, and
// store encrypted tokens. Returns the user ID so the caller can log it.
// Complete returns the user ID and the destination path the browser should be
// sent to, so a host who connects mid-onboarding resumes the wizard instead of
// being dropped on the dashboard.
func (s *Service) Complete(ctx context.Context, state, code string) (string, string, error) {
	userID, returnTo, err := s.signer.VerifyPurposeToken(state, statePurpose, s.secret)
	if err != nil {
		// Expired consent screen, tampered state, or a stray callback. All of
		// them mean the same thing to the host: start again.
		return "", "", fmt.Errorf("calendar: invalid state: %w", err)
	}
	// Re-validate after verification. The claim is signed, but the allowlist is
	// the thing that actually bounds the destination, and it may have shrunk
	// since the token was minted.
	if _, ok := pathAllowed(returnTo); !ok {
		returnTo = returnPaths[defaultReturn]
	}
	tokens, err := timedCall(ctx, "exchange", func(ctx context.Context) (gcal.Tokens, error) {
		return s.client.Exchange(ctx, code)
	})
	if err != nil {
		return userID, returnTo, fmt.Errorf("calendar: exchange: %w", err)
	}
	if tokens.RefreshToken == "" {
		// AuthCodeURL sets prompt=consent precisely so this cannot happen. If
		// it does, storing the connection would produce a calendar that works
		// for one hour and then silently stops — worse than refusing now.
		return userID, returnTo, errors.New("calendar: google returned no refresh token")
	}
	encAccess, err := s.box.Encrypt(tokens.AccessToken)
	if err != nil {
		return userID, returnTo, fmt.Errorf("calendar: encrypt access token: %w", err)
	}
	encRefresh, err := s.box.Encrypt(tokens.RefreshToken)
	if err != nil {
		return userID, returnTo, fmt.Errorf("calendar: encrypt refresh token: %w", err)
	}
	var accountEmail *string
	if tokens.AccountEmail != "" {
		accountEmail = &tokens.AccountEmail
	}
	expires := tokens.ExpiresAt
	if _, err := s.store.UpsertCalendarConnection(ctx, store.CalendarConnection{
		UserID:       userID,
		Provider:     provider,
		AccountEmail: accountEmail,
		AccessToken:  encAccess,
		RefreshToken: encRefresh,
		ExpiresAt:    &expires,
		CalendarID:   "primary",
	}); err != nil {
		return userID, returnTo, fmt.Errorf("calendar: store connection: %w", err)
	}
	slog.Info("calendar: connected", "user_id", userID, "account", tokens.AccountEmail)
	return userID, returnTo, nil
}

// ReturnPath recovers the destination from a state token WITHOUT completing
// the exchange. The denial path needs it: there is no code to trade, but the
// host should still land back where they started. Returns "" for anything it
// cannot verify, which the caller reads as "use the default".
func (s *Service) ReturnPath(state string) string {
	if state == "" {
		return ""
	}
	_, returnTo, err := s.signer.VerifyPurposeToken(state, statePurpose, s.secret)
	if err != nil {
		return ""
	}
	if _, ok := pathAllowed(returnTo); !ok {
		return ""
	}
	return returnTo
}

// pathAllowed reports whether a path is one of the allowlisted destinations.
func pathAllowed(path string) (string, bool) {
	for _, p := range returnPaths {
		if p == path {
			return p, true
		}
	}
	return "", false
}

// Disconnect revokes the grant at Google and forgets it locally.
//
// Local deletion happens even when the revoke call fails. The host asked to
// disconnect; leaving a row behind because Google was unreachable would mean
// we keep reading their calendar after they told us to stop. The stale grant
// on Google's side is theirs to remove and is inert once we drop the tokens.
func (s *Service) Disconnect(ctx context.Context, userID string) error {
	conn, err := s.store.GetCalendarConnection(ctx, userID, provider)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil // already disconnected; idempotent
		}
		return err
	}
	if refresh, derr := s.box.Decrypt(conn.RefreshToken); derr == nil {
		revokeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := s.client.Revoke(revokeCtx, refresh)
		cancel()
		if err != nil {
			slog.Warn("calendar: revoke at google failed", "err", err, "user_id", userID)
			metrics.CalendarAPIRequests.WithLabelValues("revoke", classify(err)).Inc()
		} else {
			metrics.CalendarAPIRequests.WithLabelValues("revoke", "success").Inc()
		}
	} else {
		slog.Warn("calendar: refresh token undecryptable on disconnect", "user_id", userID)
	}
	if err := s.store.DeleteCalendarConnection(ctx, userID, provider); err != nil {
		return err
	}
	slog.Info("calendar: disconnected", "user_id", userID)
	return nil
}

// Status reports connection state for the dashboard.
func (s *Service) Status(ctx context.Context, userID string) (Status, error) {
	out := Status{ConfiguredOnServer: true}
	conn, err := s.store.GetCalendarConnection(ctx, userID, provider)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return out, nil
		}
		return out, err
	}
	out.CalendarID = conn.CalendarID
	out.LastSyncedAt = conn.LastSyncedAt
	if conn.AccountEmail != nil {
		out.AccountEmail = *conn.AccountEmail
	}
	if conn.LastError != nil {
		out.LastError = *conn.LastError
	}
	if conn.RevokedAt != nil {
		out.NeedsReconnect = true
		return out, nil
	}
	out.Connected = true
	return out, nil
}

// ─── Reads ───────────────────────────────────────────────────────────────────

// BusyPeriods returns the host's busy windows in [fromUTC, toUTC).
//
// Returns (nil, nil) when the host has no calendar connected — that is a
// legitimate "nothing is blocked", not a failure. A real fetch failure returns
// an error so the caller can decide whether to degrade or refuse; it must not
// be flattened into an empty list, because "free" and "unknown" lead to
// opposite decisions about whether a slot is bookable.
func (s *Service) BusyPeriods(ctx context.Context, hostID string, fromUTC, toUTC time.Time) ([]Interval, error) {
	conn, err := s.store.GetCalendarConnection(ctx, hostID, provider)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	if !conn.Connected() {
		return nil, nil
	}
	token, err := s.accessToken(ctx, conn)
	if err != nil {
		if errors.Is(err, ErrReconnectRequired) {
			return nil, nil // tombstoned by accessToken; nothing to overlay
		}
		return nil, err
	}
	busy, err := timedCall(ctx, "freebusy", func(ctx context.Context) ([]gcal.Interval, error) {
		return s.client.FreeBusy(ctx, token, conn.CalendarID, fromUTC, toUTC)
	})
	if err != nil {
		return nil, err
	}
	out := make([]Interval, 0, len(busy))
	for _, b := range busy {
		out = append(out, Interval{Start: b.Start, End: b.End})
	}
	return out, nil
}

// ─── Writes ──────────────────────────────────────────────────────────────────

// SyncBookingCreated mirrors a confirmed booking into the host's calendar.
// Never returns an error: the booking exists regardless, and a guest who has
// finished booking should not see a failure because Google timed out. Failures
// are logged, counted, and recorded on the connection so the dashboard can say
// "calendar sync is failing" instead of quietly drifting.
func (s *Service) SyncBookingCreated(ctx context.Context, in BookingEvent) {
	conn, ok := s.connectionFor(ctx, in.HostID, "create")
	if !ok {
		return
	}
	token, err := s.accessToken(ctx, conn)
	if err != nil {
		s.recordWriteFailure(ctx, conn, "create", in.BookingID, err)
		return
	}
	eventID, err := timedCall(ctx, "events.insert", func(ctx context.Context) (string, error) {
		return s.client.InsertEvent(ctx, token, conn.CalendarID, gcal.Event{
			BookingID:   in.BookingID,
			Summary:     in.EventTitle + " with " + in.GuestName,
			Description: describeBooking(in),
			Location:    in.RoomURL,
			Start:       in.StartsAt,
			End:         in.EndsAt,
			TimeZone:    in.HostTimezone,
			GuestEmail:  in.GuestEmail,
			GuestName:   in.GuestName,
		})
	})
	if err != nil {
		s.recordWriteFailure(ctx, conn, "create", in.BookingID, err)
		return
	}
	if err := s.store.CreateBookingCalendarEvent(ctx, store.BookingCalendarEvent{
		BookingID:  in.BookingID,
		Provider:   provider,
		EventID:    eventID,
		CalendarID: conn.CalendarID,
	}); err != nil {
		// The remote event exists but we lost the mapping. Cancellation will
		// not find it — log loudly, because this is the one write failure that
		// leaves a stale event in a host's calendar. The deterministic event ID
		// (gcal.EventID) makes it recoverable by hand.
		slog.Error("calendar: event created but mapping not saved",
			"err", err, "booking_id", in.BookingID, "event_id", eventID)
		metrics.CalendarWritebackFailures.WithLabelValues("create").Inc()
		return
	}
	_ = s.store.RecordCalendarSync(ctx, conn.ID, nil)
	slog.Info("calendar: booking mirrored", "booking_id", in.BookingID, "event_id", eventID)
}

// SyncBookingCancelled removes the mirrored event. Same never-fail posture:
// the booking is already cancelled and both parties have been emailed.
func (s *Service) SyncBookingCancelled(ctx context.Context, hostID, bookingID string) {
	mapping, err := s.store.GetBookingCalendarEvent(ctx, bookingID, provider)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			slog.Warn("calendar: lookup mirrored event", "err", err, "booking_id", bookingID)
		}
		return // never mirrored — nothing to delete
	}
	conn, ok := s.connectionFor(ctx, hostID, "delete")
	if !ok {
		return
	}
	token, err := s.accessToken(ctx, conn)
	if err != nil {
		s.recordWriteFailure(ctx, conn, "delete", bookingID, err)
		return
	}
	_, err = timedCall(ctx, "events.delete", func(ctx context.Context) (struct{}, error) {
		return struct{}{}, s.client.DeleteEvent(ctx, token, mapping.CalendarID, mapping.EventID)
	})
	if err != nil {
		// Keep the mapping row: it is the only record of what still needs
		// deleting, and dropping it would turn a retryable gap into a
		// permanent stale event in the host's calendar.
		s.recordWriteFailure(ctx, conn, "delete", bookingID, err)
		return
	}
	if err := s.store.DeleteBookingCalendarEvent(ctx, bookingID, provider); err != nil {
		slog.Warn("calendar: mapping cleanup failed", "err", err, "booking_id", bookingID)
	}
	_ = s.store.RecordCalendarSync(ctx, conn.ID, nil)
	slog.Info("calendar: mirrored event removed", "booking_id", bookingID)
}

// ─── Internals ───────────────────────────────────────────────────────────────

// connectionFor loads a usable connection, or reports that there is none.
func (s *Service) connectionFor(ctx context.Context, hostID, action string) (*store.CalendarConnection, bool) {
	conn, err := s.store.GetCalendarConnection(ctx, hostID, provider)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			slog.Warn("calendar: load connection", "err", err, "host_id", hostID, "action", action)
			metrics.CalendarWritebackFailures.WithLabelValues(action).Inc()
		}
		return nil, false
	}
	if !conn.Connected() {
		return nil, false
	}
	return conn, true
}

// accessToken returns a usable access token, refreshing first when the stored
// one has expired.
//
// A concurrent refresh (two requests for the same host at once) is harmless:
// Google honours the old access token until it expires and both writers store
// a valid one, so the loser of the race costs an extra API call, not a broken
// connection. Serialising it would need a lock held across a network call,
// which is the worse trade on a single-process server.
func (s *Service) accessToken(ctx context.Context, conn *store.CalendarConnection) (string, error) {
	if conn.ExpiresAt != nil && time.Now().UTC().Before(*conn.ExpiresAt) {
		token, err := s.box.Decrypt(conn.AccessToken)
		if err == nil {
			return token, nil
		}
		// Undecryptable ciphertext means the encryption key changed or the row
		// is corrupt. Fall through to a refresh, which rewrites it under the
		// current key — a self-heal rather than a permanent failure.
		slog.Warn("calendar: access token undecryptable, refreshing", "conn_id", conn.ID)
	}
	refresh, err := s.box.Decrypt(conn.RefreshToken)
	if err != nil {
		// Without a refresh token there is no path back; the host must reconnect.
		_ = s.store.MarkCalendarRevoked(ctx, conn.ID, "stored credentials unreadable")
		return "", ErrReconnectRequired
	}
	tokens, err := timedCall(ctx, "refresh", func(ctx context.Context) (gcal.Tokens, error) {
		return s.client.Refresh(ctx, refresh)
	})
	if err != nil {
		if errors.Is(err, gcal.ErrInvalidGrant) {
			_ = s.store.MarkCalendarRevoked(ctx, conn.ID, "google revoked access — reconnect required")
			slog.Warn("calendar: grant revoked at google", "user_id", conn.UserID)
			return "", ErrReconnectRequired
		}
		return "", err
	}
	encAccess, encErr := s.box.Encrypt(tokens.AccessToken)
	if encErr == nil {
		if err := s.store.UpdateCalendarAccessToken(ctx, conn.ID, encAccess, tokens.ExpiresAt); err != nil {
			// The token in hand is still good for this request; the next
			// request just pays for another refresh.
			slog.Warn("calendar: persist refreshed token", "err", err, "conn_id", conn.ID)
		}
	}
	return tokens.AccessToken, nil
}

func (s *Service) recordWriteFailure(ctx context.Context, conn *store.CalendarConnection, action, bookingID string, err error) {
	metrics.CalendarWritebackFailures.WithLabelValues(action).Inc()
	slog.Warn("calendar: write-back failed",
		"err", err, "action", action, "booking_id", bookingID, "host_id", conn.UserID)
	msg := action + " failed: " + err.Error()
	if len(msg) > 300 {
		msg = msg[:300]
	}
	_ = s.store.RecordCalendarSync(ctx, conn.ID, &msg)
}

// timedCall wraps one Google call with the duration histogram and the
// outcome counter, so every path through this package is instrumented without
// each call site remembering to do it.
func timedCall[T any](ctx context.Context, op string, fn func(context.Context) (T, error)) (T, error) {
	start := time.Now()
	out, err := fn(ctx)
	metrics.CalendarAPIDuration.WithLabelValues(op).Observe(time.Since(start).Seconds())
	metrics.CalendarAPIRequests.WithLabelValues(op, classifyResult(err)).Inc()
	return out, err
}

func classifyResult(err error) string {
	if err == nil {
		return "success"
	}
	return classify(err)
}

func classify(err error) string {
	switch {
	case errors.Is(err, gcal.ErrInvalidGrant):
		return "revoked"
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return "timeout"
	default:
		return "error"
	}
}

func describeBooking(in BookingEvent) string {
	return fmt.Sprintf(
		"Sessionly booking with %s (%s).\n\nJoin: %s\nMeet code: %s",
		in.GuestName, in.GuestEmail, in.RoomURL, in.MeetCode,
	)
}

// ─── Interfaces consumed by the HTTP layer ───────────────────────────────────

// BusySource is what slot generation depends on.
type BusySource interface {
	BusyPeriods(ctx context.Context, hostID string, fromUTC, toUTC time.Time) ([]Interval, error)
}

// BookingSync is what the booking handlers depend on. Neither method returns
// an error, which is the contract: calendar sync must never be able to fail a
// booking.
type BookingSync interface {
	SyncBookingCreated(ctx context.Context, in BookingEvent)
	SyncBookingCancelled(ctx context.Context, hostID, bookingID string)
}

// Connector is what the /me/calendar handlers depend on.
type Connector interface {
	AuthURL(userID, returnKey string) (string, error)
	Complete(ctx context.Context, state, code string) (userID, returnTo string, err error)
	Disconnect(ctx context.Context, userID string) error
	Status(ctx context.Context, userID string) (Status, error)
	ReturnPath(state string) string
	RedirectTo(path string) string
}

var (
	_ BusySource  = (*Service)(nil)
	_ BookingSync = (*Service)(nil)
	_ Connector   = (*Service)(nil)
)
