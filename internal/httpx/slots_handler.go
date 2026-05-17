package httpx

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/vaishnavghenge/vartalaap-server/internal/store"
)

// Hard cap on the date range a single /slots query can span — protects against
// accidental "fetch the next decade" queries that would walk every booking in
// the table. Two months is enough for any reasonable booking UI scrollback.
const maxSlotsRangeDays = 62

// SlotHandlers wires the public host-facing endpoints under /u/ and the
// confirmation lookup under /m/. Mounted alongside BookingHandlers because
// they share the same security model (public, origin + rate limited).
//
//	GET /u/{slug}                        → host profile + active event types
//	GET /u/{slug}/{event}                → host + single event details
//	GET /u/{slug}/{event}/slots?from=…   → bookable slots in range
//	GET /m/{code}                        → resolve booking by meet code
//
// Go 1.22 path patterns aren't in use elsewhere in this codebase yet, so we
// parse manually to match the existing style in booking_handler.go.
func SlotHandlers(mux *http.ServeMux, st store.Storer, cfg AuthConfig) {
	lim := NewRateLimiter(60, 120)

	mux.HandleFunc("/u/", func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/u/"), "/")
		// Strip a trailing empty segment so /u/foo/ behaves like /u/foo.
		if n := len(parts); n > 0 && parts[n-1] == "" {
			parts = parts[:n-1]
		}
		if len(parts) == 0 || parts[0] == "" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		hostSlug := parts[0]
		switch len(parts) {
		case 1:
			route(w, r, cfg, lim, handleGetHostProfile(st, hostSlug))
		case 2:
			if parts[1] == "" {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			route(w, r, cfg, lim, handleGetPublicEvent(st, hostSlug, parts[1]))
		case 3:
			if parts[1] == "" || parts[2] != "slots" {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			route(w, r, cfg, lim, handleListSlots(st, hostSlug, parts[1]))
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	})

	mux.HandleFunc("/m/", func(w http.ResponseWriter, r *http.Request) {
		code := strings.TrimPrefix(r.URL.Path, "/m/")
		if code == "" || strings.Contains(code, "/") {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		route(w, r, cfg, lim, handleGetBookingByMeetCode(st, code))
	})
}

// route applies the standard OPTIONS+GET handling for public /u/ and /m/
// endpoints so each sub-handler doesn't repeat the switch.
func route(w http.ResponseWriter, r *http.Request, cfg AuthConfig, lim *RateLimiter, h http.HandlerFunc) {
	switch r.Method {
	case http.MethodOptions:
		bookingsRoute(cfg, http.MethodGet, nil,
			func(w http.ResponseWriter, r *http.Request) {})(w, r)
	case http.MethodGet:
		bookingsRoute(cfg, http.MethodGet, lim, h)(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// ─── Public DTOs ──────────────────────────────────────────────────────────────

type publicEventDTO struct {
	ID          string `json:"id"`
	Slug        string `json:"slug"`
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
	DurationMin int    `json:"durationMin"`
	IsPaid      bool   `json:"isPaid"`
}

type hostProfileResponse struct {
	Name       string           `json:"name"`
	Slug       string           `json:"slug"`
	Timezone   string           `json:"timezone"`
	EventTypes []publicEventDTO `json:"eventTypes"`
}

type publicEventResponse struct {
	Host  hostProfileMini `json:"host"`
	Event publicEventDTO  `json:"event"`
}

type hostProfileMini struct {
	Name     string `json:"name"`
	Slug     string `json:"slug"`
	Timezone string `json:"timezone"`
}

func toPublicEventDTO(e store.EventType) publicEventDTO {
	dto := publicEventDTO{
		ID:          e.ID,
		Slug:        e.Slug,
		Title:       e.Title,
		DurationMin: e.DurationMin,
		IsPaid:      e.IsPaid,
	}
	if e.Description != nil {
		dto.Description = *e.Description
	}
	return dto
}

// handleGetHostProfile serves GET /u/{slug}. Returns the host's public
// profile + their active event types. 404 when the host doesn't exist —
// same response shape as a real "no such page" to keep slug enumeration
// no more useful than guessing.
func handleGetHostProfile(st store.Storer, hostSlug string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		host, err := st.GetUserBySlug(r.Context(), hostSlug)
		if err != nil {
			WriteError(w, http.StatusNotFound, "NOT_FOUND", "not found")
			return
		}
		events, err := st.ListEventTypes(r.Context(), host.ID, true)
		if err != nil {
			slog.Error("public: list event types", "err", err, "host_id", host.ID)
			WriteError(w, http.StatusInternalServerError, "INTERNAL", "could not load profile")
			return
		}
		out := hostProfileResponse{
			Name:       host.Name,
			Slug:       host.Slug,
			Timezone:   host.Timezone,
			EventTypes: make([]publicEventDTO, 0, len(events)),
		}
		for _, e := range events {
			out.EventTypes = append(out.EventTypes, toPublicEventDTO(e))
		}
		WriteJSON(w, http.StatusOK, out)
	}
}

// handleGetPublicEvent serves GET /u/{slug}/{event}. Returns the metadata
// the booking-form page needs (host name/tz + event title/duration). The
// slot list is a separate call so the booking page can refetch slots when
// the date range changes without re-hitting the metadata route.
func handleGetPublicEvent(st store.Storer, hostSlug, eventSlug string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		host, event, err := resolveBookingTarget(r.Context(), st, hostSlug, eventSlug)
		if err != nil {
			WriteError(w, http.StatusNotFound, "NOT_FOUND", "not found")
			return
		}
		if !event.IsActive {
			WriteError(w, http.StatusNotFound, "EVENT_INACTIVE", "event is no longer available")
			return
		}
		WriteJSON(w, http.StatusOK, publicEventResponse{
			Host: hostProfileMini{
				Name: host.Name, Slug: host.Slug, Timezone: host.Timezone,
			},
			Event: toPublicEventDTO(*event),
		})
	}
}

// handleGetBookingByMeetCode serves GET /m/{code} — the public confirmation
// page reads here to show "you're booked with X at Y". The response is
// deliberately narrow: only what the guest already knows or needs to display.
func handleGetBookingByMeetCode(st store.Storer, code string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b, err := st.GetBookingByMeetCode(r.Context(), code)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				WriteError(w, http.StatusNotFound, "NOT_FOUND", "not found")
				return
			}
			slog.Error("m: get booking", "err", err, "code", code)
			WriteError(w, http.StatusInternalServerError, "INTERNAL", "could not load booking")
			return
		}
		event, _ := st.GetEventType(r.Context(), b.HostID, b.EventTypeID)
		host, _ := st.GetUserByID(r.Context(), b.HostID)
		WriteJSON(w, http.StatusOK, toBookingDTO(*b, event, host))
	}
}

type slotsResponse struct {
	EventTypeID   string    `json:"eventTypeId"`
	EventTitle    string    `json:"eventTitle"`
	DurationMin   int       `json:"durationMin"`
	HostName      string    `json:"hostName"`
	HostTimezone  string    `json:"hostTimezone"`
	Slots         []string  `json:"slots"` // RFC3339 in UTC; client formats per its own tz
}

func handleListSlots(st store.Storer, hostSlug, eventSlug string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		host, event, err := resolveBookingTarget(r.Context(), st, hostSlug, eventSlug)
		if err != nil {
			WriteError(w, http.StatusNotFound, "NOT_FOUND", "not found")
			return
		}
		if !event.IsActive {
			WriteError(w, http.StatusNotFound, "EVENT_INACTIVE", "event is no longer available")
			return
		}
		q := r.URL.Query()
		fromUTC, toUTC, ferr := parseSlotsRange(q.Get("from"), q.Get("to"))
		if ferr != nil {
			WriteFieldError(w, http.StatusBadRequest, ferr)
			return
		}

		rules, err := st.ListAvailability(r.Context(), host.ID)
		if err != nil {
			slog.Error("slots: list availability", "err", err, "host_id", host.ID)
			WriteError(w, http.StatusInternalServerError, "INTERNAL", "could not load slots")
			return
		}
		// No availability set → no slots; not an error.
		if len(rules) == 0 {
			WriteJSON(w, http.StatusOK, slotsResponse{
				EventTypeID:  event.ID,
				EventTitle:   event.Title,
				DurationMin:  event.DurationMin,
				HostName:     host.Name,
				HostTimezone: host.Timezone,
				Slots:        []string{},
			})
			return
		}
		bookings, err := st.ListBookingsForEventInRange(r.Context(), event.ID, fromUTC, toUTC)
		if err != nil {
			slog.Error("slots: list bookings", "err", err, "event_id", event.ID)
			WriteError(w, http.StatusInternalServerError, "INTERNAL", "could not load slots")
			return
		}
		slots := generateSlots(rules, *event, bookings, fromUTC, toUTC, time.Now().UTC())

		out := slotsResponse{
			EventTypeID:  event.ID,
			EventTitle:   event.Title,
			DurationMin:  event.DurationMin,
			HostName:     host.Name,
			HostTimezone: host.Timezone,
			Slots:        make([]string, 0, len(slots)),
		}
		for _, t := range slots {
			out.Slots = append(out.Slots, t.Format(time.RFC3339))
		}
		WriteJSON(w, http.StatusOK, out)
	}
}

// parseSlotsRange validates the from/to date-only query strings and returns
// a [fromUTC, toUTC) range. Missing `to` defaults to from+14d so a bare
// "next two weeks" probe is the natural default.
func parseSlotsRange(rawFrom, rawTo string) (time.Time, time.Time, error) {
	rawFrom = strings.TrimSpace(rawFrom)
	rawTo = strings.TrimSpace(rawTo)
	if rawFrom == "" {
		return time.Time{}, time.Time{}, badField("from", "REQUIRED", "from is required")
	}
	from, err := time.Parse("2006-01-02", rawFrom)
	if err != nil {
		return time.Time{}, time.Time{}, badField("from", "INVALID_DATE", "from must be YYYY-MM-DD")
	}
	var to time.Time
	if rawTo == "" {
		to = from.AddDate(0, 0, 14)
	} else {
		t, terr := time.Parse("2006-01-02", rawTo)
		if terr != nil {
			return time.Time{}, time.Time{}, badField("to", "INVALID_DATE", "to must be YYYY-MM-DD")
		}
		to = t
	}
	if !to.After(from) {
		return time.Time{}, time.Time{}, badField("to", "RANGE_INVALID", "to must be after from")
	}
	if to.Sub(from) > maxSlotsRangeDays*24*time.Hour {
		return time.Time{}, time.Time{}, badField("to", "RANGE_TOO_LARGE",
			"date range too large")
	}
	// Inclusive of `to` as a whole day: extend to end-of-day in UTC. Per-rule
	// timezone handling happens during slot generation; the outer range is in
	// UTC to keep the SQL filter unambiguous.
	return from.UTC(), to.AddDate(0, 0, 1).UTC(), nil
}

// generateSlots is the pure core of slot generation. Given the host's
// availability rules and existing bookings for this event, it walks every
// day in [fromUTC, toUTC) and emits candidate start times that:
//
//   - fall inside an availability window for that day-of-week (with the rule's
//     own timezone honoured — DST-safe because we use time.LoadLocation per rule)
//   - are at least `nowUTC` (no slots in the past)
//   - don't overlap any existing booking (including buffer-min on either side)
//   - end before the availability window's end (so duration fits)
//
// Slot cadence is the event's duration (back-to-back). Buffer is applied
// before/after each booking, not between candidate slots, so the picker stays
// dense — a 30-min event with 15-min buffer surfaces 09:00, 09:30, 10:00, …
// as candidates and only removes the ones that collide with the buffer of an
// existing booking.
func generateSlots(
	rules []store.AvailabilityRule,
	event store.EventType,
	bookings []store.Booking,
	fromUTC, toUTC time.Time,
	nowUTC time.Time,
) []time.Time {
	if event.DurationMin <= 0 {
		return nil
	}
	duration := time.Duration(event.DurationMin) * time.Minute
	buffer := time.Duration(event.BufferMin) * time.Minute

	// Bucket rules by day-of-week so the per-day walk is O(rules-for-this-day)
	// not O(all-rules) per iteration.
	rulesByDOW := make(map[int][]store.AvailabilityRule, 7)
	for _, r := range rules {
		rulesByDOW[r.DayOfWeek] = append(rulesByDOW[r.DayOfWeek], r)
	}

	out := make([]time.Time, 0, 64)
	// Iterate one calendar day at a time. We anchor the day in the rule's own
	// timezone when expanding it; the outer loop walks UTC dates because the
	// `from..to` range was given in UTC.
	for day := fromUTC; day.Before(toUTC); day = day.AddDate(0, 0, 1) {
		// JS Date.getDay() and our schema agree on 0=Sun..6=Sat.
		dow := int(day.Weekday())
		dayRules := rulesByDOW[dow]
		if len(dayRules) == 0 {
			continue
		}
		for _, rule := range dayRules {
			loc, err := time.LoadLocation(rule.Timezone)
			if err != nil {
				// A malformed timezone would have been rejected at write time;
				// be conservative if we somehow see one anyway and skip it
				// rather than crashing the request.
				continue
			}
			// The "wall-clock" date for this rule is the calendar day in its
			// own timezone — which may differ from `day`'s UTC date near
			// midnight. Find the start of the day in `loc`, then walk forward.
			localMidnight := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, loc)
			start := combineDateTime(localMidnight, rule.StartTime, loc)
			end := combineDateTime(localMidnight, rule.EndTime, loc)
			if start.IsZero() || end.IsZero() || !end.After(start) {
				continue
			}

			for slot := start; !slot.Add(duration).After(end); slot = slot.Add(duration) {
				slotUTC := slot.UTC()
				if slotUTC.Before(nowUTC) {
					continue
				}
				// Restrict to the requested outer range so weekday rules that
				// straddle the range boundary in their local timezone don't
				// emit slots outside [fromUTC, toUTC).
				if slotUTC.Before(fromUTC) || !slotUTC.Before(toUTC) {
					continue
				}
				if isSlotConflicted(slotUTC, duration, buffer, bookings) {
					continue
				}
				out = append(out, slotUTC)
			}
		}
	}
	return out
}

// combineDateTime takes a localised midnight and an "HH:MM" wall-clock string
// and produces the absolute time. Returns the zero time on parse failure so
// the caller can skip the slot rather than misplace it.
func combineDateTime(localMidnight time.Time, hhmm string, loc *time.Location) time.Time {
	t, err := time.ParseInLocation("15:04", hhmm, loc)
	if err != nil {
		return time.Time{}
	}
	return time.Date(
		localMidnight.Year(), localMidnight.Month(), localMidnight.Day(),
		t.Hour(), t.Minute(), 0, 0, loc,
	)
}

// isSlotConflicted returns true when the candidate slot [slotUTC, slotUTC+duration]
// would collide with any booking (including the booking's buffer on either side).
func isSlotConflicted(slotUTC time.Time, duration, buffer time.Duration, bookings []store.Booking) bool {
	slotStart := slotUTC
	slotEnd := slotUTC.Add(duration)
	for _, b := range bookings {
		bStart := b.StartsAt.Add(-buffer)
		bEnd := b.EndsAt.Add(buffer)
		// Half-open overlap: any sliver of contact disqualifies the slot.
		if slotStart.Before(bEnd) && slotEnd.After(bStart) {
			return true
		}
	}
	return false
}

// checkBookingConflict returns ErrConflict-equivalent when a candidate
// booking would overlap an existing one (taking buffer into account on both
// sides). Used by handleCreateBooking before the insert so the failure is a
// clean 409 rather than a generic 500.
//
// Returns nil when the slot is clear and a non-nil sentinel-equivalent error
// otherwise.
func checkBookingConflict(ctx context.Context, st store.Storer, event store.EventType, startsAt, endsAt time.Time) error {
	buffer := time.Duration(event.BufferMin) * time.Minute
	// Query a window large enough to catch any booking whose buffer touches
	// ours. starts_at < endsAt+buffer AND ends_at > startsAt-buffer covers
	// every collision case.
	bookings, err := st.ListBookingsForEventInRange(ctx, event.ID,
		startsAt.Add(-buffer), endsAt.Add(buffer))
	if err != nil {
		return err
	}
	if isSlotConflicted(startsAt, endsAt.Sub(startsAt), buffer, bookings) {
		return errSlotTaken
	}
	return nil
}

var errSlotTaken = errors.New("slot already taken")
