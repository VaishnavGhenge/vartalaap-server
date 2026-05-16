package httpx

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/vaishnavghenge/vartalaap-server/internal/auth"
	"github.com/vaishnavghenge/vartalaap-server/internal/store"
)

// Maximum availability rules a single host may submit in one PUT. The roadmap
// example UIs cap weekly schedules at ~6 slots/day × 7 days = 42, so 50 is a
// comfortable ceiling that still rejects pathological bodies cheaply before
// the transaction starts.
const maxAvailabilityRules = 50

// MeHandlers wires the authenticated `/me/*` scheduling routes onto mux.
// Mounted separately from AuthHandlers so the auth wiring stays focused on
// session lifecycle and the scheduling wiring on host-owned resources.
func MeHandlers(mux *http.ServeMux, st store.Storer, cfg AuthConfig) {
	lim := NewRateLimiter(30, 60)

	mux.HandleFunc("/me/availability", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodOptions:
			// Preflight needs to list every method the route accepts; PUT would
			// be blocked otherwise.
			meRoute(cfg, http.MethodGet+", "+http.MethodPut, nil,
				func(w http.ResponseWriter, r *http.Request) {})(w, r)
		case http.MethodGet:
			meRoute(cfg, http.MethodGet, lim,
				RequireAuth(cfg.JWTSecret, handleGetAvailability(st)))(w, r)
		case http.MethodPut:
			meRoute(cfg, http.MethodPut, lim,
				RequireAuth(cfg.JWTSecret, handlePutAvailability(st)))(w, r)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})

	// /me/event-types — collection: GET list, POST create.
	mux.HandleFunc("/me/event-types", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodOptions:
			meRoute(cfg, http.MethodGet+", "+http.MethodPost, nil,
				func(w http.ResponseWriter, r *http.Request) {})(w, r)
		case http.MethodGet:
			meRoute(cfg, http.MethodGet, lim,
				RequireAuth(cfg.JWTSecret, handleListEventTypes(st)))(w, r)
		case http.MethodPost:
			meRoute(cfg, http.MethodPost, lim,
				RequireAuth(cfg.JWTSecret, handleCreateEventType(st)))(w, r)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})

	// /me/event-types/{id} — member: PATCH update, DELETE soft-delete.
	mux.HandleFunc("/me/event-types/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/me/event-types/")
		// Reject empty IDs (a trailing-slash hit) and IDs with further path
		// segments — there's no sub-resource here.
		if id == "" || strings.Contains(id, "/") {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		switch r.Method {
		case http.MethodOptions:
			meRoute(cfg, http.MethodPatch+", "+http.MethodDelete, nil,
				func(w http.ResponseWriter, r *http.Request) {})(w, r)
		case http.MethodPatch:
			meRoute(cfg, http.MethodPatch, lim,
				RequireAuth(cfg.JWTSecret, handlePatchEventType(st, id)))(w, r)
		case http.MethodDelete:
			meRoute(cfg, http.MethodDelete, lim,
				RequireAuth(cfg.JWTSecret, handleDeleteEventType(st, id)))(w, r)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
}

func meRoute(cfg AuthConfig, method string, lim *RateLimiter, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !enforceAPIRequest(w, r, cfg.AllowedOrigins, method, lim) {
			return
		}
		next(w, r)
	}
}

// ─── Availability ────────────────────────────────────────────────────────────

type availabilityRuleDTO struct {
	ID        string `json:"id,omitempty"` // ignored on PUT; assigned by the DB
	DayOfWeek int    `json:"dayOfWeek"`    // 0=Sun..6=Sat
	StartTime string `json:"startTime"`    // "HH:MM"
	EndTime   string `json:"endTime"`      // "HH:MM"
	Timezone  string `json:"timezone"`
}

type availabilityResponse struct {
	Rules []availabilityRuleDTO `json:"rules"`
}

func toAvailabilityResponse(rules []store.AvailabilityRule) availabilityResponse {
	out := make([]availabilityRuleDTO, 0, len(rules))
	for _, r := range rules {
		out = append(out, availabilityRuleDTO{
			ID:        r.ID,
			DayOfWeek: r.DayOfWeek,
			StartTime: r.StartTime,
			EndTime:   r.EndTime,
			Timezone:  r.Timezone,
		})
	}
	return availabilityResponse{Rules: out}
}

func handleGetAvailability(st store.Storer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := auth.UserIDFromContext(r.Context())
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		rules, err := st.ListAvailability(r.Context(), userID)
		if err != nil {
			slog.Error("me/availability: list", "err", err, "user_id", userID)
			http.Error(w, "could not load availability", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, toAvailabilityResponse(rules))
	}
}

func handlePutAvailability(st store.Storer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := auth.UserIDFromContext(r.Context())
		if !ok {
			WriteError(w, http.StatusUnauthorized, "UNAUTHORIZED", "unauthorized")
			return
		}
		var body availabilityResponse
		if err := BindJSON(r, &body); err != nil {
			WriteFieldError(w, http.StatusBadRequest, err)
			return
		}
		if len(body.Rules) > maxAvailabilityRules {
			WriteError(w, http.StatusBadRequest, "TOO_MANY_RULES",
				"too many availability rules in one save")
			return
		}
		rules, err := validateAvailabilityRules(userID, body.Rules)
		if err != nil {
			WriteFieldError(w, http.StatusBadRequest, err)
			return
		}
		saved, err := st.ReplaceAvailability(r.Context(), userID, rules)
		if err != nil {
			slog.Error("me/availability: replace", "err", err, "user_id", userID)
			WriteError(w, http.StatusInternalServerError, "SAVE_FAILED", "could not save availability")
			return
		}
		slog.Info("me/availability: replaced",
			"user_id", userID, "rule_count", len(saved))
		WriteJSON(w, http.StatusOK, toAvailabilityResponse(saved))
	}
}

// validateAvailabilityRules parses and bounds-checks incoming DTOs and returns
// store-ready values. Errors are FieldError instances so the response carries
// a stable Code the UI can branch on (and a Field naming the offending row).
func validateAvailabilityRules(hostID string, in []availabilityRuleDTO) ([]store.AvailabilityRule, error) {
	out := make([]store.AvailabilityRule, 0, len(in))
	for i, r := range in {
		field := func(suffix string) string {
			return ruleFieldName(i, suffix)
		}
		if _, err := ValidateIntRange(field("dayOfWeek"), r.DayOfWeek, 0, 6); err != nil {
			return nil, err
		}
		start, err := ValidateClockTime(field("startTime"), r.StartTime)
		if err != nil {
			return nil, err
		}
		end, err := ValidateClockTime(field("endTime"), r.EndTime)
		if err != nil {
			return nil, err
		}
		if end <= start {
			return nil, badField(field("endTime"), "END_BEFORE_START", "endTime must be after startTime")
		}
		tz, err := ValidateTimezone(field("timezone"), r.Timezone)
		if err != nil {
			return nil, err
		}
		out = append(out, store.AvailabilityRule{
			HostID:    hostID,
			DayOfWeek: r.DayOfWeek,
			StartTime: start,
			EndTime:   end,
			Timezone:  tz,
		})
	}
	return out, nil
}

// ruleFieldName produces "rules[2].endTime" — the JSON-pointer-ish shape the
// UI uses to highlight the offending row in the schedule editor.
func ruleFieldName(i int, suffix string) string {
	return jsonArrayField("rules", i, suffix)
}

func jsonArrayField(base string, i int, suffix string) string {
	return base + "[" + intToStr(i) + "]." + suffix
}

// intToStr is a tiny non-recursive itoa to avoid importing strconv solely for
// field-name formatting. Caller guarantees the value is small (array index).
func intToStr(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// writeJSON is the legacy lower-case shim. New code should call WriteJSON
// directly; this exists so the refactor lands one handler at a time.
func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	WriteJSON(w, status, v)
}

// ─── Event types ─────────────────────────────────────────────────────────────

// Per-plan limit on active event types. The free plan ships with one slot so a
// new user can take a booking immediately; paid plans are unlimited.
const freePlanEventTypeLimit = 1

// Allowed durations come from the roadmap §Phase 2 schema. Keeping the set
// fixed lets the UI build a duration picker that can never collide with the
// DB CHECK constraint.
var allowedDurations = map[int]struct{}{
	15: {}, 30: {}, 45: {}, 60: {}, 90: {}, 120: {},
}

// Event slug shape is intentionally identical to the user-slug pattern in
// inputs.go::ValidateSlug — handled there so we have one slug regex.

type eventTypeDTO struct {
	ID            string  `json:"id,omitempty"`
	Slug          string  `json:"slug"`
	Title         string  `json:"title"`
	DurationMin   int     `json:"durationMin"`
	BufferMin     int     `json:"bufferMin"`
	MaxPerDay     *int    `json:"maxPerDay,omitempty"`
	IsPaid        bool    `json:"isPaid"`
	PriceCents    *int    `json:"priceCents,omitempty"`
	Currency      string  `json:"currency,omitempty"`
	PaymentTiming string  `json:"paymentTiming,omitempty"`
	IsActive      bool    `json:"isActive"`
	Description   *string `json:"description,omitempty"`
}

type eventTypeListResponse struct {
	EventTypes []eventTypeDTO `json:"eventTypes"`
}

func toEventTypeDTO(e store.EventType) eventTypeDTO {
	return eventTypeDTO{
		ID: e.ID, Slug: e.Slug, Title: e.Title,
		DurationMin: e.DurationMin, BufferMin: e.BufferMin, MaxPerDay: e.MaxPerDay,
		IsPaid: e.IsPaid, PriceCents: e.PriceCents,
		Currency: e.Currency, PaymentTiming: e.PaymentTiming,
		IsActive: e.IsActive, Description: e.Description,
	}
}

func handleListEventTypes(st store.Storer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := auth.UserIDFromContext(r.Context())
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		// activeOnly=false so the dashboard can show archived events too;
		// public profile fetchers will pass activeOnly=true via a different
		// (unauthenticated) endpoint added in a later slice.
		events, err := st.ListEventTypes(r.Context(), userID, false)
		if err != nil {
			slog.Error("me/event-types: list", "err", err, "user_id", userID)
			http.Error(w, "could not load event types", http.StatusInternalServerError)
			return
		}
		out := make([]eventTypeDTO, 0, len(events))
		for _, e := range events {
			out = append(out, toEventTypeDTO(e))
		}
		writeJSON(w, http.StatusOK, eventTypeListResponse{EventTypes: out})
	}
}

func handleCreateEventType(st store.Storer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := auth.UserIDFromContext(r.Context())
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var dto eventTypeDTO
		if err := json.NewDecoder(r.Body).Decode(&dto); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		// Plan-tier check requires the canonical user record — the JWT only
		// carries the user ID; we don't trust plan claims in the token.
		user, err := st.GetUserByID(r.Context(), userID)
		if err != nil {
			slog.Error("me/event-types: load user", "err", err, "user_id", userID)
			http.Error(w, "could not create event type", http.StatusInternalServerError)
			return
		}
		ev, status, msg := validateEventTypeDTO(dto, userID, user.Plan)
		if msg != "" {
			http.Error(w, msg, status)
			return
		}
		// Active-count cap — only enforced on free. Paid plans are uncapped.
		if user.Plan == "free" && ev.IsActive {
			n, err := st.CountActiveEventTypes(r.Context(), userID)
			if err != nil {
				slog.Error("me/event-types: count", "err", err, "user_id", userID)
				http.Error(w, "could not create event type", http.StatusInternalServerError)
				return
			}
			if n >= freePlanEventTypeLimit {
				http.Error(w,
					"free plan allows 1 active event type — upgrade to Solo for unlimited",
					http.StatusForbidden)
				return
			}
		}
		created, err := st.CreateEventType(r.Context(), ev)
		if err != nil {
			if errors.Is(err, store.ErrConflict) {
				http.Error(w, "an event type with that slug already exists", http.StatusConflict)
				return
			}
			slog.Error("me/event-types: create", "err", err, "user_id", userID)
			http.Error(w, "could not create event type", http.StatusInternalServerError)
			return
		}
		slog.Info("me/event-types: created",
			"user_id", userID, "event_id", created.ID, "slug", created.Slug)
		writeJSON(w, http.StatusCreated, toEventTypeDTO(*created))
	}
}

func handlePatchEventType(st store.Storer, id string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := auth.UserIDFromContext(r.Context())
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var dto eventTypeDTO
		if err := json.NewDecoder(r.Body).Decode(&dto); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		// Load current to fail fast on cross-host attempts before reading user.
		existing, err := st.GetEventType(r.Context(), userID, id)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			slog.Error("me/event-types: get existing", "err", err, "user_id", userID, "id", id)
			http.Error(w, "could not update event type", http.StatusInternalServerError)
			return
		}
		user, err := st.GetUserByID(r.Context(), userID)
		if err != nil {
			slog.Error("me/event-types: load user", "err", err, "user_id", userID)
			http.Error(w, "could not update event type", http.StatusInternalServerError)
			return
		}
		ev, status, msg := validateEventTypeDTO(dto, userID, user.Plan)
		if msg != "" {
			http.Error(w, msg, status)
			return
		}
		ev.ID = id
		// Re-enforce the free-plan active cap if this update is what flips
		// the event from inactive to active. Otherwise an upgrade-then-downgrade
		// trick could let a host activate more than one event on a free plan.
		if user.Plan == "free" && ev.IsActive && !existing.IsActive {
			n, err := st.CountActiveEventTypes(r.Context(), userID)
			if err != nil {
				slog.Error("me/event-types: count", "err", err, "user_id", userID)
				http.Error(w, "could not update event type", http.StatusInternalServerError)
				return
			}
			if n >= freePlanEventTypeLimit {
				http.Error(w,
					"free plan allows 1 active event type — upgrade to Solo for unlimited",
					http.StatusForbidden)
				return
			}
		}
		updated, err := st.UpdateEventType(r.Context(), ev)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			if errors.Is(err, store.ErrConflict) {
				http.Error(w, "an event type with that slug already exists", http.StatusConflict)
				return
			}
			slog.Error("me/event-types: update", "err", err, "user_id", userID, "id", id)
			http.Error(w, "could not update event type", http.StatusInternalServerError)
			return
		}
		slog.Info("me/event-types: updated",
			"user_id", userID, "event_id", updated.ID, "slug", updated.Slug)
		writeJSON(w, http.StatusOK, toEventTypeDTO(*updated))
	}
}

func handleDeleteEventType(st store.Storer, id string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := auth.UserIDFromContext(r.Context())
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if err := st.DeleteEventType(r.Context(), userID, id); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			slog.Error("me/event-types: delete", "err", err, "user_id", userID, "id", id)
			http.Error(w, "could not delete event type", http.StatusInternalServerError)
			return
		}
		slog.Info("me/event-types: deleted", "user_id", userID, "event_id", id)
		w.WriteHeader(http.StatusNoContent)
	}
}

// validateEventTypeDTO maps a wire DTO to a store-ready EventType. Returns
// (zero, status, message) when the DTO is invalid; the handler writes that
// status/message verbatim so error messages stay specific.
func validateEventTypeDTO(dto eventTypeDTO, hostID, plan string) (store.EventType, int, string) {
	slug, err := ValidateSlug("slug", dto.Slug)
	if err != nil {
		return store.EventType{}, http.StatusBadRequest, err.Error()
	}
	title := strings.TrimSpace(dto.Title)
	if title == "" || len(title) > 120 {
		return store.EventType{}, http.StatusBadRequest, "title must be 1–120 characters"
	}
	if _, ok := allowedDurations[dto.DurationMin]; !ok {
		return store.EventType{}, http.StatusBadRequest, "durationMin must be 15, 30, 45, 60, 90, or 120"
	}
	if dto.BufferMin < 0 || dto.BufferMin > 120 {
		return store.EventType{}, http.StatusBadRequest, "bufferMin must be 0–120"
	}
	if dto.MaxPerDay != nil && *dto.MaxPerDay <= 0 {
		return store.EventType{}, http.StatusBadRequest, "maxPerDay must be a positive integer"
	}
	// Paid sessions are a Solo/Teams feature — free hosts cannot accept money
	// directly through Sessionly. Enforced server-side so a tampered request
	// can't bypass the UI gating.
	if dto.IsPaid {
		if plan == "free" {
			return store.EventType{}, http.StatusForbidden,
				"paid event types require the Solo or Teams plan"
		}
		if dto.PriceCents == nil || *dto.PriceCents <= 0 {
			return store.EventType{}, http.StatusBadRequest,
				"priceCents must be a positive integer when isPaid is true"
		}
		if *dto.PriceCents > 10_000_00 {
			return store.EventType{}, http.StatusBadRequest,
				"priceCents must be ≤ 10000.00"
		}
	} else if dto.PriceCents != nil {
		return store.EventType{}, http.StatusBadRequest,
			"priceCents must be omitted when isPaid is false"
	}
	currency := strings.ToLower(strings.TrimSpace(dto.Currency))
	if currency == "" {
		currency = "usd"
	}
	if len(currency) != 3 {
		return store.EventType{}, http.StatusBadRequest, "currency must be a 3-letter ISO code"
	}
	paymentTiming := dto.PaymentTiming
	if paymentTiming == "" {
		paymentTiming = "upfront"
	}
	if paymentTiming != "upfront" && paymentTiming != "after" {
		return store.EventType{}, http.StatusBadRequest, "paymentTiming must be 'upfront' or 'after'"
	}
	var description *string
	if dto.Description != nil {
		d := strings.TrimSpace(*dto.Description)
		if len(d) > 2000 {
			return store.EventType{}, http.StatusBadRequest, "description must be ≤ 2000 characters"
		}
		if d != "" {
			description = &d
		}
	}
	return store.EventType{
		HostID:        hostID,
		Slug:          slug,
		Title:         title,
		DurationMin:   dto.DurationMin,
		BufferMin:     dto.BufferMin,
		MaxPerDay:     dto.MaxPerDay,
		IsPaid:        dto.IsPaid,
		PriceCents:    dto.PriceCents,
		Currency:      currency,
		PaymentTiming: paymentTiming,
		IsActive:      dto.IsActive,
		Description:   description,
	}, 0, ""
}
