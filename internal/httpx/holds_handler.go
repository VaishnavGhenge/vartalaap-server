package httpx

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/vaishnavghenge/vartalaap-server/internal/store"
)

// holdTTL is the wall-clock lifetime of a slot hold. Five minutes is the
// answer to "long enough for a slow guest to type name + email and submit,
// short enough that an abandoned form doesn't lock the slot for the next
// guest". If a real session needs more, the picker re-holds on each pick so
// the user effectively refreshes their own hold every interaction.
const holdTTL = 5 * time.Minute

// HoldHandlers wires POST /holds and DELETE /holds/{token}. The flow:
//
//	guest taps a slot      → POST /holds {hostSlug, eventSlug, startsAt}
//	server replies         → {holdToken, expiresAt}
//	guest submits booking  → POST /bookings {…, holdToken}     (token consumed)
//	guest changes pick OR  → DELETE /holds/{token}
//	   closes page         → navigator.sendBeacon DELETE /holds/{token}
//	silence > 5 minutes    → expires_at filter ignores the row
//
// Public — no auth needed. Same trust model as POST /bookings.
func HoldHandlers(mux *http.ServeMux, st store.Storer, cfg AuthConfig) {
	lim := NewRateLimiter(30, 60)

	mux.HandleFunc("/holds", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodOptions:
			bookingsRoute(cfg, http.MethodPost, nil,
				func(w http.ResponseWriter, r *http.Request) {})(w, r)
		case http.MethodPost:
			bookingsRoute(cfg, http.MethodPost, lim, handleCreateHold(st))(w, r)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})

	mux.HandleFunc("/holds/", func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.URL.Path, "/holds/")
		if token == "" || strings.Contains(token, "/") {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		switch r.Method {
		case http.MethodOptions:
			bookingsRoute(cfg, http.MethodDelete, nil,
				func(w http.ResponseWriter, r *http.Request) {})(w, r)
		case http.MethodDelete:
			bookingsRoute(cfg, http.MethodDelete, lim, handleReleaseHold(st, token))(w, r)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
}

type createHoldRequest struct {
	HostSlug      string `json:"hostSlug"`
	EventTypeSlug string `json:"eventTypeSlug"`
	StartsAt      string `json:"startsAt"`
}

type createHoldResponse struct {
	HoldToken string    `json:"holdToken"`
	ExpiresAt time.Time `json:"expiresAt"`
}

func handleCreateHold(st store.Storer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req createHoldRequest
		if err := BindJSON(r, &req); err != nil {
			WriteFieldError(w, http.StatusBadRequest, err)
			return
		}
		host, event, err := resolveBookingTarget(r.Context(), st, req.HostSlug, req.EventTypeSlug)
		if err != nil {
			WriteError(w, http.StatusNotFound, "NOT_FOUND", "not found")
			return
		}
		if !event.IsActive {
			WriteError(w, http.StatusNotFound, "EVENT_INACTIVE", "event is no longer available")
			return
		}
		// Same future-only rule as bookings; a hold for a past slot is
		// always meaningless.
		startsAt, ferr := ValidateRFC3339Future("startsAt", req.StartsAt, time.Minute)
		if ferr != nil {
			WriteFieldError(w, http.StatusBadRequest, ferr)
			return
		}
		endsAt := startsAt.Add(time.Duration(event.DurationMin) * time.Minute)

		// Same conflict check as POST /bookings — if a booking or an active
		// hold already covers this window, refuse.
		if cerr := checkBookingConflict(r.Context(), st, *event, startsAt.UTC(), endsAt.UTC()); cerr != nil {
			if errors.Is(cerr, errSlotTaken) {
				WriteError(w, http.StatusConflict, "SLOT_TAKEN", "this slot is no longer available")
				return
			}
			slog.Error("holds: conflict check", "err", cerr, "host_id", host.ID, "event_id", event.ID)
			WriteError(w, http.StatusInternalServerError, "INTERNAL", "could not hold slot")
			return
		}
		if cerr := checkHoldConflict(r.Context(), st, host.ID, *event, startsAt.UTC(), endsAt.UTC(), ""); cerr != nil {
			if errors.Is(cerr, errSlotTaken) {
				WriteError(w, http.StatusConflict, "SLOT_TAKEN", "this slot is being booked by someone else")
				return
			}
			slog.Error("holds: hold-conflict check", "err", cerr, "host_id", host.ID)
			WriteError(w, http.StatusInternalServerError, "INTERNAL", "could not hold slot")
			return
		}

		token, err := newHoldToken()
		if err != nil {
			slog.Error("holds: token gen", "err", err)
			WriteError(w, http.StatusInternalServerError, "INTERNAL", "could not hold slot")
			return
		}
		expiresAt := time.Now().UTC().Add(holdTTL)
		created, err := st.CreateSlotHold(r.Context(), store.SlotHold{
			HostID:      host.ID,
			EventTypeID: event.ID,
			StartsAt:    startsAt.UTC(),
			EndsAt:      endsAt.UTC(),
			Token:       token,
			ExpiresAt:   expiresAt,
		})
		if err != nil {
			if errors.Is(err, store.ErrConflict) {
				// Token collision on a 192-bit value should never happen; if it
				// somehow does, surfacing 500 lets the client retry rather than
				// silently lose the hold.
				WriteError(w, http.StatusInternalServerError, "INTERNAL", "could not hold slot")
				return
			}
			slog.Error("holds: create", "err", err, "host_id", host.ID, "event_id", event.ID)
			WriteError(w, http.StatusInternalServerError, "INTERNAL", "could not hold slot")
			return
		}
		slog.Info("holds: created",
			"hold_id", created.ID, "host_id", host.ID, "event_id", event.ID,
			"starts_at", created.StartsAt, "expires_at", created.ExpiresAt)
		WriteJSON(w, http.StatusCreated, createHoldResponse{
			HoldToken: created.Token,
			ExpiresAt: created.ExpiresAt,
		})
	}
}

func handleReleaseHold(st store.Storer, token string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Idempotent on purpose: a stale beacon firing after the booking was
		// already submitted (and the hold consumed) shouldn't 404.
		if err := st.DeleteSlotHold(r.Context(), token); err != nil {
			slog.Error("holds: release", "err", err)
			WriteError(w, http.StatusInternalServerError, "INTERNAL", "could not release hold")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// newHoldToken returns a 192-bit URL-safe random token. 24 bytes of entropy
// is plenty to make collision a non-event over the entire app's lifetime;
// the UNIQUE constraint in slot_holds is belt-and-braces.
func newHoldToken() (string, error) {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}
