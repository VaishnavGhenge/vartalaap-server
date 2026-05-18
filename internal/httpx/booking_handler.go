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
	"github.com/vaishnavghenge/vartalaap-server/internal/email"
	"github.com/vaishnavghenge/vartalaap-server/internal/store"
)

// BookingDeps bundles the cross-cutting collaborators booking handlers need
// alongside the Storer. Keeping these in one struct stops the handler
// signatures from sprouting positional parameters every time the booking
// flow grows a new side effect.
type BookingDeps struct {
	Mailer       email.Mailer
	PublicAppURL string
}

// freePlanMonthlyBookingLimit caps the bookings a free-tier host can take in a
// calendar month. The roadmap names this number explicitly; codifying it as a
// const means the value is greppable when Phase 3 wires plan upgrades.
const freePlanMonthlyBookingLimit = 10

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
			bookingsRoute(cfg, http.MethodGet, publicLim, handleGetBooking(st, id))(w, r)
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
				RequireAuth(cfg.JWTSecret, handleListMyBookings(st)))(w, r)
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
	HoldToken     string `json:"holdToken,omitempty"`
}

type bookingDTO struct {
	ID            string    `json:"id"`
	EventTypeID   string    `json:"eventTypeId"`
	EventTypeSlug string    `json:"eventTypeSlug,omitempty"`
	EventTitle    string    `json:"eventTitle,omitempty"`
	HostID        string    `json:"hostId"`
	HostSlug      string    `json:"hostSlug,omitempty"`
	GuestName     string    `json:"guestName"`
	GuestEmail    string    `json:"guestEmail"`
	StartsAt      time.Time `json:"startsAt"`
	EndsAt        time.Time `json:"endsAt"`
	MeetCode      string    `json:"meetCode"`
	Status        string    `json:"status"`
}

type bookingListResponse struct {
	Bookings []bookingDTO `json:"bookings"`
}

func toBookingDTO(b store.Booking, event *store.EventType, host *store.User) bookingDTO {
	dto := bookingDTO{
		ID:          b.ID,
		EventTypeID: b.EventTypeID,
		HostID:      b.HostID,
		GuestName:   b.GuestName,
		GuestEmail:  b.GuestEmail,
		StartsAt:    b.StartsAt.UTC(),
		EndsAt:      b.EndsAt.UTC(),
		MeetCode:    b.MeetCode,
		Status:      b.Status,
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

		// Free-plan cap. CountBookingsInMonth uses created_at (this month's
		// quota counts bookings *taken* this month, not sessions delivered).
		if host.Plan == "free" {
			now := time.Now().UTC()
			n, err := st.CountBookingsInMonth(r.Context(), host.ID, now.Year(), now.Month())
			if err != nil {
				slog.Error("bookings: month count", "err", err, "host_id", host.ID)
				WriteError(w, http.StatusInternalServerError, "INTERNAL", "could not create booking")
				return
			}
			if n >= freePlanMonthlyBookingLimit {
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

		WriteJSON(w, http.StatusCreated, toBookingDTO(*created, event, host))
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
	}
	if err := deps.Mailer.Send(sendCtx, email.RenderBookingNotification(in, "")); err != nil {
		slog.Warn("bookings: host email failed", "err", err, "booking_id", b.ID)
	}
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
		if err := st.CancelBooking(r.Context(), b.ID); err != nil {
			slog.Error("bookings: host cancel", "err", err, "booking_id", b.ID, "user_id", userID)
			WriteError(w, http.StatusInternalServerError, "INTERNAL", "could not cancel")
			return
		}
		host, _ := st.GetUserByID(r.Context(), b.HostID)
		event, _ := st.GetEventType(r.Context(), b.HostID, b.EventTypeID)
		sendCancellationEmails(r.Context(), deps, b, event, host, "host")
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
		if err := st.CancelBooking(r.Context(), b.ID); err != nil {
			slog.Error("bookings: guest cancel", "err", err, "booking_id", b.ID)
			WriteError(w, http.StatusInternalServerError, "INTERNAL", "could not cancel")
			return
		}
		host, _ := st.GetUserByID(r.Context(), b.HostID)
		event, _ := st.GetEventType(r.Context(), b.HostID, b.EventTypeID)
		sendCancellationEmails(r.Context(), deps, b, event, host, "guest")
		slog.Info("bookings: guest cancelled", "booking_id", b.ID, "meet_code", code)
		w.WriteHeader(http.StatusNoContent)
	}
}

// sendCancellationEmails notifies both parties. `cancelledBy` is "host" or
// "guest" — used only in the subject/body so each party reads the right
// framing ("X cancelled" vs "you cancelled").
func sendCancellationEmails(ctx context.Context, deps BookingDeps, b *store.Booking, event *store.EventType, host *store.User, cancelledBy string) {
	if deps.Mailer == nil || event == nil || host == nil {
		return
	}
	in := email.BookingInput{
		GuestName:    b.GuestName,
		GuestEmail:   b.GuestEmail,
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
	if err := deps.Mailer.Send(sendCtx, email.RenderBookingCancellation(in, "", cancelledBy)); err != nil {
		slog.Warn("bookings: cancellation email failed", "err", err, "booking_id", b.ID)
	}
}

func handleGetBooking(st store.Storer, id string) http.HandlerFunc {
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
		WriteJSON(w, http.StatusOK, toBookingDTO(*b, event, host))
	}
}

func handleListMyBookings(st store.Storer) http.HandlerFunc {
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
			out = append(out, toBookingDTO(b, ev, nil))
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