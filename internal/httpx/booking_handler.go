package httpx

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/vaishnavghenge/vartalaap-server/internal/auth"
	"github.com/vaishnavghenge/vartalaap-server/internal/store"
)

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
func BookingHandlers(mux *http.ServeMux, st store.Storer, cfg AuthConfig) {
	publicLim := NewRateLimiter(20, 40)
	hostLim := NewRateLimiter(30, 60)

	mux.HandleFunc("/bookings", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodOptions:
			bookingsRoute(cfg, http.MethodPost, nil,
				func(w http.ResponseWriter, r *http.Request) {})(w, r)
		case http.MethodPost:
			bookingsRoute(cfg, http.MethodPost, publicLim, handleCreateBooking(st))(w, r)
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
			bookingsRoute(cfg, http.MethodGet, nil,
				func(w http.ResponseWriter, r *http.Request) {})(w, r)
		case http.MethodGet:
			bookingsRoute(cfg, http.MethodGet, publicLim, handleGetBooking(st, id))(w, r)
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

func handleCreateBooking(st store.Storer) http.HandlerFunc {
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
		WriteJSON(w, http.StatusCreated, toBookingDTO(*created, event, host))
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
		from := time.Now().UTC()
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