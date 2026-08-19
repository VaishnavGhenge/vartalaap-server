package httpx

import (
	"context"
	"crypto/subtle"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/vaishnavghenge/vartalaap-server/internal/auth"
	"github.com/vaishnavghenge/vartalaap-server/internal/calendar"
	"github.com/vaishnavghenge/vartalaap-server/internal/email"
	"github.com/vaishnavghenge/vartalaap-server/internal/metrics"
	"github.com/vaishnavghenge/vartalaap-server/internal/plans"
	"github.com/vaishnavghenge/vartalaap-server/internal/store"
)

// BookingDeps bundles the cross-cutting collaborators booking handlers need
// alongside the Storer. Keeping these in one struct stops the handler
// signatures from sprouting positional parameters every time the booking
// flow grows a new side effect.
type BookingDeps struct {
	Mailer       email.Mailer
	PublicAppURL string
	RoomWindow   BookingRoomWindow

	// Busy reads the host's external-calendar busy windows so slot generation
	// and booking creation both see them. Nil when calendar sync is not
	// configured, which every call site must tolerate.
	Busy calendar.BusySource
	// CalendarSync mirrors bookings into the host's calendar. Nil-safe for the
	// same reason. Neither of its methods returns an error by design: a
	// calendar write must never be able to fail a booking.
	CalendarSync calendar.BookingSync
}

type BookingRoomWindow struct {
	OpenBefore time.Duration
	CloseAfter time.Duration
}

type bookingRoomAccess struct {
	Status   string
	Message  string
	OpensAt  time.Time
	ClosesAt time.Time
}

// meetCodeCollisionRetries bounds how many fresh meet codes we generate before
// giving up on an insert. With 32^10 raw codes the actual probability of
// collision is vanishingly small; the cap exists to prevent an infinite loop
// in the impossible-but-pathological case (e.g. RNG malfunction).
const meetCodeCollisionRetries = 5

// BookingHandlers wires the public booking surface and the host's own
// bookings list. Public routes (POST /bookings, GET /bookings/{id}) accept
// guests with no account; /me/bookings sits behind auth.
func BookingHandlers(mux *http.ServeMux, st store.Storer, cfg AuthConfig, deps BookingDeps) {
	publicLim := NewRateLimiter(20, 40)
	hostLim := NewRateLimiter(30, 60)
	if deps.Mailer == nil {
		// Default keeps callers tidy in tests and ensures we never panic on
		// a missing dep — a misconfigured prod env logs instead of crashing.
		deps.Mailer = &email.LogMailer{}
	}

	mux.HandleFunc("/bookings", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodOptions:
			bookingsRoute(cfg, http.MethodPost, nil,
				func(w http.ResponseWriter, r *http.Request) {})(w, r)
		case http.MethodPost:
			bookingsRoute(cfg, http.MethodPost, publicLim, handleCreateBooking(st, deps))(w, r)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})

	mux.HandleFunc("/bookings/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/bookings/")
		if id == "" || strings.Contains(id, "/") {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		switch r.Method {
		case http.MethodOptions:
			// Pre-flight covers both GET and DELETE — the browser sends one
			// OPTIONS per actual method, so advertising both keeps the
			// dashboard cancel path from being blocked by CORS.
			bookingsRoute(cfg, "GET, DELETE", nil,
				func(w http.ResponseWriter, r *http.Request) {})(w, r)
		case http.MethodGet:
			bookingsRoute(cfg, http.MethodGet, publicLim, handleGetBooking(st, deps, id))(w, r)
		case http.MethodDelete:
			bookingsRoute(cfg, http.MethodDelete, hostLim,
				RequireAuth(cfg.JWTSecret, handleHostCancelBooking(st, deps, id)))(w, r)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})

	mux.HandleFunc("/me/bookings", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodOptions:
			bookingsRoute(cfg, http.MethodGet, nil,
				func(w http.ResponseWriter, r *http.Request) {})(w, r)
		case http.MethodGet:
			bookingsRoute(cfg, http.MethodGet, hostLim,
				RequireAuth(cfg.JWTSecret, handleListMyBookings(st, deps.RoomWindow)))(w, r)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
}

func bookingsRoute(cfg AuthConfig, method string, lim *RateLimiter, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !enforceAPIRequest(w, r, cfg.AllowedOrigins, method, lim) {
			return
		}
		next(w, r)
	}
}

// ─── Wire DTOs ────────────────────────────────────────────────────────────────

type createBookingRequest struct {
	HostSlug      string `json:"hostSlug"`
	EventTypeSlug string `json:"eventTypeSlug"`
	StartsAt      string `json:"startsAt"`
	GuestName     string `json:"guestName"`
	GuestEmail    string `json:"guestEmail"`
	// HoldToken is optional. When present, the conflict checks ignore any
	// hold with this token (so a guest's own held slot doesn't block their
	// own submit) and the hold is consumed on success.
	HoldToken string `json:"holdToken,omitempty"`
}

type bookingDTO struct {
	ID                 string    `json:"id"`
	EventTypeID        string    `json:"eventTypeId"`
	EventTypeSlug      string    `json:"eventTypeSlug,omitempty"`
	EventTitle         string    `json:"eventTitle,omitempty"`
	HostID             string    `json:"hostId"`
	HostSlug           string    `json:"hostSlug,omitempty"`
	GuestName          string    `json:"guestName"`
	GuestEmail         string    `json:"guestEmail"`
	StartsAt           time.Time `json:"startsAt"`
	EndsAt             time.Time `json:"endsAt"`
	MeetCode           string    `json:"meetCode"`
	Status             string    `json:"status"`
	CancellationReason *string   `json:"cancellationReason,omitempty"`
	CancelledBy        *string   `json:"cancelledBy,omitempty"`
	RoomStatus         string    `json:"roomStatus"`
	RoomMessage        string    `json:"roomMessage,omitempty"`
	RoomOpensAt        time.Time `json:"roomOpensAt"`
	RoomClosesAt       time.Time `json:"roomClosesAt"`
	ServerNow          time.Time `json:"serverNow"`
}

type cancelBookingRequest struct {
	Reason string `json:"reason"`
}

type bookingListResponse struct {
	Bookings []bookingDTO `json:"bookings"`
}

func toBookingDTO(b store.Booking, event *store.EventType, host *store.User) bookingDTO {
	return toBookingDTOWithWindow(b, event, host, BookingRoomWindow{})
}

func toBookingDTOWithWindow(b store.Booking, event *store.EventType, host *store.User, window BookingRoomWindow) bookingDTO {
	now := time.Now().UTC()
	access := BookingRoomAccessFor(b, now, window)
	dto := bookingDTO{
		ID:                 b.ID,
		EventTypeID:        b.EventTypeID,
		HostID:             b.HostID,
		GuestName:          b.GuestName,
		GuestEmail:         b.GuestEmail,
		StartsAt:           b.StartsAt.UTC(),
		EndsAt:             b.EndsAt.UTC(),
		MeetCode:           b.MeetCode,
		Status:             b.Status,
		CancellationReason: b.CancellationReason,
		CancelledBy:        b.CancelledBy,
		RoomStatus:         access.Status,
		RoomMessage:        access.Message,
		RoomOpensAt:        access.OpensAt,
		RoomClosesAt:       access.ClosesAt,
		ServerNow:          now,
	}
	if event != nil {
		dto.EventTypeSlug = event.Slug
		dto.EventTitle = event.Title
	}
	if host != nil {
		dto.HostSlug = host.Slug
	}
	return dto
}

func BookingRoomAccessFor(b store.Booking, now time.Time, window BookingRoomWindow) bookingRoomAccess {
	if window.OpenBefore <= 0 {
		window.OpenBefore = 10 * time.Minute
	}
	if window.CloseAfter <= 0 {
		window.CloseAfter = 30 * time.Minute
	}
	opensAt := b.StartsAt.UTC().Add(-window.OpenBefore)
	closesAt := b.EndsAt.UTC().Add(window.CloseAfter)
	if b.Status == "cancelled" {
		return bookingRoomAccess{Status: "cancelled", Message: "This booking was cancelled.", OpensAt: opensAt, ClosesAt: closesAt}
	}
	if now.Before(opensAt) {
		return bookingRoomAccess{Status: "too_early", Message: "This room is not open yet.", OpensAt: opensAt, ClosesAt: closesAt}
	}
	if now.After(closesAt) {
		return bookingRoomAccess{Status: "ended", Message: "This room is no longer available.", OpensAt: opensAt, ClosesAt: closesAt}
	}
	return bookingRoomAccess{Status: "open", OpensAt: opensAt, ClosesAt: closesAt}
}

// ─── Handlers ────────────────────────────────────────────────────────────────

func handleCreateBooking(st store.Storer, deps BookingDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req createBookingRequest
		if err := BindJSON(r, &req); err != nil {
			WriteFieldError(w, http.StatusBadRequest, err)
			return
		}
		host, event, err := resolveBookingTarget(r.Context(), st, req.HostSlug, req.EventTypeSlug)
		if err != nil {
			// 404 for both "host doesn't exist" and "event doesn't exist" so a
			// guest probing slugs can't enumerate hosts.
			WriteError(w, http.StatusNotFound, "NOT_FOUND", "not found")
			return
		}
		if !event.IsActive {
			WriteError(w, http.StatusNotFound, "EVENT_INACTIVE", "this event type is no longer available")
			return
		}
		// Paid events have no booking flow yet — Stripe Connect lands in
		// Phase 3. Reject with a clear message so the UI can disable the
		// option rather than the request 500ing.
		if event.IsPaid {
			WriteError(w, http.StatusServiceUnavailable, "PAID_NOT_AVAILABLE",
				"paid event types are not yet bookable in this release")
			return
		}
		guestName, err := ValidateLen("guestName", req.GuestName, 1, 200)
		if err != nil {
			WriteFieldError(w, http.StatusBadRequest, err)
			return
		}
		guestEmail, err := ValidateEmail("guestEmail", req.GuestEmail)
		if err != nil {
			WriteFieldError(w, http.StatusBadRequest, err)
			return
		}
		// Small grace window keeps clock-skew between client and server from
		// rejecting otherwise-valid bookings.
		startsAt, err := ValidateRFC3339Future("startsAt", req.StartsAt, time.Minute)
		if err != nil {
			WriteFieldError(w, http.StatusBadRequest, err)
			return
		}
		endsAt := startsAt.Add(time.Duration(event.DurationMin) * time.Minute)

		// Reject slot collisions before we burn a meet-code generation
		// attempt. The same predicate is used by /slots so the picker and
		// the create call agree on which times are taken.
		if cerr := checkBookingConflict(r.Context(), st, *event, startsAt.UTC(), endsAt.UTC()); cerr != nil {
			if errors.Is(cerr, errSlotTaken) {
				WriteError(w, http.StatusConflict, "SLOT_TAKEN", "this slot is no longer available")
				return
			}
			slog.Error("bookings: conflict check", "err", cerr, "host_id", host.ID, "event_id", event.ID)
			WriteError(w, http.StatusInternalServerError, "INTERNAL", "could not create booking")
			return
		}
		// Hold collisions on the same host (cross-event). The guest's own
		// hold (if any) is exempt via ignoreToken so they don't 409 against
		// themselves.
		if cerr := checkHoldConflict(r.Context(), st, host.ID, *event, startsAt.UTC(), endsAt.UTC(), req.HoldToken); cerr != nil {
			if errors.Is(cerr, errSlotTaken) {
				WriteError(w, http.StatusConflict, "SLOT_TAKEN", "this slot is being booked by someone else")
				return
			}
			slog.Error("bookings: hold-conflict check", "err", cerr, "host_id", host.ID)
			WriteError(w, http.StatusInternalServerError, "INTERNAL", "could not create booking")
			return
		}

		// External calendar conflict. Checked here as well as in /slots because
		// the picker's view can be seconds stale — the host may have accepted a
		// meeting in Google between the guest loading slots and submitting.
		//
		// Fails OPEN: if Google is unreachable we take the booking rather than
		// turning a Google outage into a Sessionly outage. The host is emailed
		// either way and can cancel. The alternative — rejecting bookings we
		// cannot prove are conflict-free — loses real revenue to someone else's
		// downtime. vartalaap_calendar_busy_degraded_total counts these.
		if busyErr := checkExternalBusyConflict(r.Context(), deps, host.ID, *event, startsAt.UTC(), endsAt.UTC()); busyErr != nil {
			if errors.Is(busyErr, errSlotTaken) {
				WriteError(w, http.StatusConflict, "SLOT_TAKEN",
					"this slot is no longer available")
				return
			}
			slog.Warn("bookings: calendar busy check degraded", "err", busyErr, "host_id", host.ID)
			metrics.CalendarBusyDegraded.Inc()
		}

		// Monthly booking cap. CountBookingsInMonth uses created_at (bookings
		// *taken* this month, not sessions delivered). The limit is defined in
		// internal/plans — change it there, not here.
		lim := plans.For(host.Plan)
		if lim.MonthlyBookings != plans.Unlimited {
			now := time.Now().UTC()
			n, err := st.CountBookingsInMonth(r.Context(), host.ID, now.Year(), now.Month())
			if err != nil {
				slog.Error("bookings: month count", "err", err, "host_id", host.ID)
				WriteError(w, http.StatusInternalServerError, "INTERNAL", "could not create booking")
				return
			}
			if plans.Exceeds(lim.MonthlyBookings, n) {
				WriteError(w, http.StatusForbidden, "HOST_MONTHLY_CAP",
					"this host has reached their monthly booking limit")
				return
			}
		}

		// Generate a meet code and retry on the rare collision. The retry loop
		// covers a fundamentally rare event (32^10 ≈ 1.1 × 10^15 codes); the
		// cap matters more as a safety net than as a tuning knob.
		var created *store.Booking
		for attempt := 0; attempt < meetCodeCollisionRetries; attempt++ {
			code, gerr := generateMeetCode()
			if gerr != nil {
				slog.Error("bookings: meet code gen", "err", gerr)
				WriteError(w, http.StatusInternalServerError, "INTERNAL", "could not create booking")
				return
			}
			b, cerr := st.CreateBooking(r.Context(), store.Booking{
				EventTypeID: event.ID,
				HostID:      host.ID,
				GuestEmail:  guestEmail,
				GuestName:   guestName,
				StartsAt:    startsAt.UTC(),
				EndsAt:      endsAt.UTC(),
				MeetCode:    code,
				Status:      "confirmed",
			})
			if cerr == nil {
				created = b
				break
			}
			if !errors.Is(cerr, store.ErrConflict) {
				slog.Error("bookings: create", "err", cerr, "host_id", host.ID, "event_id", event.ID)
				WriteError(w, http.StatusInternalServerError, "INTERNAL", "could not create booking")
				return
			}
			// Otherwise: collision, loop.
		}
		if created == nil {
			slog.Error("bookings: meet code collisions exhausted")
			WriteError(w, http.StatusInternalServerError, "INTERNAL", "could not create booking")
			return
		}

		slog.Info("bookings: created",
			"booking_id", created.ID, "host_id", host.ID, "event_id", event.ID,
			"starts_at", created.StartsAt, "guest_email", guestEmail)

		// Consume the hold (if any) so the row isn't left blocking other
		// pickers until its TTL. DeleteSlotHold is idempotent so a missing
		// or already-released hold is fine.
		if req.HoldToken != "" {
			if err := st.DeleteSlotHold(r.Context(), req.HoldToken); err != nil {
				slog.Warn("bookings: hold release failed", "err", err, "booking_id", created.ID)
			}
		}

		sendBookingEmails(r.Context(), deps, created, event, host, guestName, guestEmail)
		syncBookingToCalendar(r.Context(), deps, created, event, host)

		WriteJSON(w, http.StatusCreated, toBookingDTOWithWindow(*created, event, host, deps.RoomWindow))
	}
}

// sendBookingEmails fires the guest + host confirmations. Errors are logged
// but never surfaced to the caller — the booking has already been persisted,
// and a failed email is a recoverable miss (the guest still has the meet
// code in the response, the host can re-fetch from /me/bookings). The
// 6-second context cap is enough for two SMTP round-trips on any healthy
// provider while keeping the request from hanging on a stuck one.
func sendBookingEmails(ctx context.Context, deps BookingDeps, b *store.Booking, event *store.EventType, host *store.User, guestName, guestEmail string) {
	if deps.Mailer == nil {
		return
	}
	in := email.BookingInput{
		GuestName:    guestName,
		GuestEmail:   guestEmail,
		HostName:     host.Name,
		HostEmail:    host.Email,
		HostTimezone: host.Timezone,
		EventTitle:   event.Title,
		EventMinutes: event.DurationMin,
		StartsAt:     b.StartsAt,
		EndsAt:       b.EndsAt,
		MeetCode:     b.MeetCode,
		CancelToken:  b.CancelToken,
		PublicAppURL: deps.PublicAppURL,
	}
	sendCtx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	if err := deps.Mailer.Send(sendCtx, email.RenderBookingConfirmation(in, "")); err != nil {
		slog.Warn("bookings: guest email failed", "err", err, "booking_id", b.ID)
	} else {
		slog.Info("bookings: guest email sent", "booking_id", b.ID, "to", guestEmail)
	}
	if err := deps.Mailer.Send(sendCtx, email.RenderBookingNotification(in, "")); err != nil {
		slog.Warn("bookings: host email failed", "err", err, "booking_id", b.ID)
	} else {
		slog.Info("bookings: host email sent", "booking_id", b.ID, "to", host.Email)
	}
}

// checkExternalBusyConflict overlays the host's external calendar on a
// candidate booking. Returns errSlotTaken on a genuine overlap, a non-sentinel
// error when the lookup itself failed, and nil when the slot is clear or no
// calendar is connected.
func checkExternalBusyConflict(ctx context.Context, deps BookingDeps, hostID string, event store.EventType, startsAt, endsAt time.Time) error {
	if deps.Busy == nil {
		return nil
	}
	bufferBefore := time.Duration(event.BufferBeforeMin) * time.Minute
	bufferAfter := time.Duration(event.BufferMin) * time.Minute
	busy, err := deps.Busy.BusyPeriods(ctx, hostID,
		startsAt.Add(-bufferBefore), endsAt.Add(bufferAfter))
	if err != nil {
		return err
	}
	if isSlotConflicted(startsAt, endsAt.Sub(startsAt), bufferBefore, bufferAfter, busyAsBookings(busy)) {
		return errSlotTaken
	}
	return nil
}

// syncBookingToCalendar mirrors a confirmed booking into the host's calendar.
// Synchronous by design: CLAUDE.md forbids fire-and-forget goroutines, and the
// same reasoning as sendBookingEmails applies — a bounded call on the request
// path is easier to reason about than a background worker whose failures
// nobody sees. The 8-second cap covers gcal's three-attempt retry schedule.
func syncBookingToCalendar(ctx context.Context, deps BookingDeps, b *store.Booking, event *store.EventType, host *store.User) {
	if deps.CalendarSync == nil || event == nil || host == nil {
		return
	}
	syncCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	deps.CalendarSync.SyncBookingCreated(syncCtx, calendar.BookingEvent{
		BookingID:    b.ID,
		HostID:       host.ID,
		HostTimezone: host.Timezone,
		EventTitle:   event.Title,
		GuestName:    b.GuestName,
		GuestEmail:   b.GuestEmail,
		StartsAt:     b.StartsAt,
		EndsAt:       b.EndsAt,
		MeetCode:     b.MeetCode,
		RoomURL:      roomURL(deps.PublicAppURL, b.MeetCode),
	})
}

// unsyncBookingFromCalendar removes the mirrored event after a cancellation.
func unsyncBookingFromCalendar(ctx context.Context, deps BookingDeps, b *store.Booking) {
	if deps.CalendarSync == nil || b == nil {
		return
	}
	syncCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	deps.CalendarSync.SyncBookingCancelled(syncCtx, b.HostID, b.ID)
}

// roomURL builds the absolute join link. Mirrors the email package's link
// construction so a host clicking through from their calendar and a guest
// clicking through from their inbox land on the same URL.
func roomURL(publicAppURL, meetCode string) string {
	return strings.TrimSuffix(publicAppURL, "/") + "/room/" + meetCode
}

// handleHostCancelBooking soft-cancels a booking the authed host owns. We
// scope by host_id rather than just bookingID so a token leak can't be used
// to cancel another host's bookings via a known UUID. Returns 204 on success,
// 404 for both "doesn't exist" and "exists but belongs to someone else" — same
// reasoning as resolveBookingTarget.
func handleHostCancelBooking(st store.Storer, deps BookingDeps, id string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := auth.UserIDFromContext(r.Context())
		if !ok {
			WriteError(w, http.StatusUnauthorized, "UNAUTHORIZED", "unauthorized")
			return
		}
		b, err := st.GetBookingByID(r.Context(), id)
		if err != nil || b.HostID != userID {
			WriteError(w, http.StatusNotFound, "NOT_FOUND", "not found")
			return
		}
		if b.Status == "cancelled" {
			// Idempotent: a re-cancel is success, not 404.
			w.WriteHeader(http.StatusNoContent)
			return
		}
		reason, err := readCancellationReason(r)
		if err != nil {
			WriteFieldError(w, http.StatusBadRequest, err)
			return
		}
		if err := st.CancelBooking(r.Context(), b.ID, reason, "host"); err != nil {
			slog.Error("bookings: host cancel", "err", err, "booking_id", b.ID, "user_id", userID)
			WriteError(w, http.StatusInternalServerError, "INTERNAL", "could not cancel")
			return
		}
		b.CancellationReason = &reason
		cancelledBy := "host"
		b.CancelledBy = &cancelledBy
		host, _ := st.GetUserByID(r.Context(), b.HostID)
		event, _ := st.GetEventType(r.Context(), b.HostID, b.EventTypeID)
		sendCancellationEmails(r.Context(), deps, b, event, host, "host")
		unsyncBookingFromCalendar(r.Context(), deps, b)
		slog.Info("bookings: host cancelled", "booking_id", b.ID, "user_id", userID)
		w.WriteHeader(http.StatusNoContent)
	}
}

// handleGuestCancelBooking is the unauthed counterpart, looked up by meet
// code. Knowing the meet code is the same trust model the booking
// confirmation page uses; an attacker without the code can't enumerate
// (codes are 32^10 ≈ 10^15).
func handleGuestCancelBooking(st store.Storer, deps BookingDeps, code string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b, err := st.GetBookingByMeetCode(r.Context(), code)
		if err != nil {
			WriteError(w, http.StatusNotFound, "NOT_FOUND", "not found")
			return
		}
		// Magic-link auth: the cancel token from the confirmation email must
		// match. Constant-time compare to avoid timing leaks on token shape.
		// 404 (not 401/403) so an attacker probing meet codes can't tell
		// "valid code, wrong token" from "no such booking".
		token := r.URL.Query().Get("t")
		if token == "" || subtle.ConstantTimeCompare([]byte(token), []byte(b.CancelToken)) != 1 {
			WriteError(w, http.StatusNotFound, "NOT_FOUND", "not found")
			return
		}
		if b.Status == "cancelled" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		reason, err := readCancellationReason(r)
		if err != nil {
			WriteFieldError(w, http.StatusBadRequest, err)
			return
		}
		if err := st.CancelBooking(r.Context(), b.ID, reason, "guest"); err != nil {
			slog.Error("bookings: guest cancel", "err", err, "booking_id", b.ID)
			WriteError(w, http.StatusInternalServerError, "INTERNAL", "could not cancel")
			return
		}
		b.CancellationReason = &reason
		cancelledBy := "guest"
		b.CancelledBy = &cancelledBy
		host, _ := st.GetUserByID(r.Context(), b.HostID)
		event, _ := st.GetEventType(r.Context(), b.HostID, b.EventTypeID)
		sendCancellationEmails(r.Context(), deps, b, event, host, "guest")
		unsyncBookingFromCalendar(r.Context(), deps, b)
		slog.Info("bookings: guest cancelled", "booking_id", b.ID, "meet_code", code)
		w.WriteHeader(http.StatusNoContent)
	}
}

func readCancellationReason(r *http.Request) (string, error) {
	var req cancelBookingRequest
	if err := BindJSON(r, &req); err != nil {
		return "", err
	}
	return ValidateLen("reason", req.Reason, 3, 500)
}

func stringPtrValue(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

// sendCancellationEmails notifies both parties. `cancelledBy` is "host" or
// "guest" — used only in the subject/body so each party reads the right
// framing ("X cancelled" vs "you cancelled").
func sendCancellationEmails(ctx context.Context, deps BookingDeps, b *store.Booking, event *store.EventType, host *store.User, cancelledBy string) {
	if deps.Mailer == nil || event == nil || host == nil {
		return
	}
	in := email.BookingInput{
		GuestName:          b.GuestName,
		GuestEmail:         b.GuestEmail,
		HostName:           host.Name,
		HostEmail:          host.Email,
		HostTimezone:       host.Timezone,
		EventTitle:         event.Title,
		EventMinutes:       event.DurationMin,
		StartsAt:           b.StartsAt,
		EndsAt:             b.EndsAt,
		MeetCode:           b.MeetCode,
		CancelToken:        b.CancelToken,
		CancellationReason: stringPtrValue(b.CancellationReason),
		PublicAppURL:       deps.PublicAppURL,
	}
	sendCtx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	if err := deps.Mailer.Send(sendCtx, email.RenderBookingCancellation(in, "", cancelledBy)); err != nil {
		slog.Warn("bookings: cancellation email failed", "err", err, "booking_id", b.ID)
	}
}

func handleGetBooking(st store.Storer, deps BookingDeps, id string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b, err := st.GetBookingByID(r.Context(), id)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				WriteError(w, http.StatusNotFound, "NOT_FOUND", "not found")
				return
			}
			slog.Error("bookings: get", "err", err, "booking_id", id)
			WriteError(w, http.StatusInternalServerError, "INTERNAL", "could not load booking")
			return
		}
		// Load event + host for the response shape. Public endpoint — never
		// reveal anything beyond what the guest already knows (slug, title).
		event, _ := st.GetEventType(r.Context(), b.HostID, b.EventTypeID)
		host, _ := st.GetUserByID(r.Context(), b.HostID)
		WriteJSON(w, http.StatusOK, toBookingDTOWithWindow(*b, event, host, deps.RoomWindow))
	}
}

func handleListMyBookings(st store.Storer, window BookingRoomWindow) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := auth.UserIDFromContext(r.Context())
		if !ok {
			WriteError(w, http.StatusUnauthorized, "UNAUTHORIZED", "unauthorized")
			return
		}
		// Look back 2 hours so sessions that have already started (but not
		// yet ended) appear on the dashboard alongside upcoming ones.
		from := time.Now().UTC().Add(-2 * time.Hour)
		bookings, err := st.ListBookingsForHost(r.Context(), userID, from, 100)
		if err != nil {
			slog.Error("me/bookings: list", "err", err, "user_id", userID)
			WriteError(w, http.StatusInternalServerError, "INTERNAL", "could not load bookings")
			return
		}
		// Resolve event titles in a single batch by remembering what we've
		// already looked up. Most dashboards have a small number of distinct
		// event types so the cache amortises to O(distinct events).
		events := map[string]*store.EventType{}
		out := make([]bookingDTO, 0, len(bookings))
		for _, b := range bookings {
			ev, ok := events[b.EventTypeID]
			if !ok {
				ev, _ = st.GetEventType(r.Context(), userID, b.EventTypeID)
				events[b.EventTypeID] = ev
			}
			out = append(out, toBookingDTOWithWindow(b, ev, nil, window))
		}
		WriteJSON(w, http.StatusOK, bookingListResponse{Bookings: out})
	}
}

// resolveBookingTarget centralises the (host_slug, event_slug) → (User, EventType)
// lookup used by POST /bookings and (later) the public /u/{slug}/{event} page.
// Returns one generic error message so probing can't distinguish host-not-found
// from event-not-found.
func resolveBookingTarget(ctx context.Context, st store.Storer, hostSlug, eventSlug string) (*store.User, *store.EventType, error) {
	hostSlug = strings.TrimSpace(hostSlug)
	eventSlug = strings.TrimSpace(eventSlug)
	if hostSlug == "" || eventSlug == "" {
		return nil, nil, errors.New("not found")
	}
	host, err := st.GetUserBySlug(ctx, hostSlug)
	if err != nil {
		return nil, nil, errors.New("not found")
	}
	event, err := st.GetEventTypeBySlug(ctx, host.ID, eventSlug)
	if err != nil {
		return nil, nil, errors.New("not found")
	}
	return host, event, nil
}

// generateMeetCode lives in meet_handler.go; bookings reuse it so the URL
// pattern stays uniform across ad-hoc /meets/new codes and booked calls.
