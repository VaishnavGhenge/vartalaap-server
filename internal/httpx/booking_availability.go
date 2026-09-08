package httpx

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/vaishnavghenge/vartalaap-server/internal/store"
)

func enforceBookingAvailability(w http.ResponseWriter, r *http.Request, st store.Storer, event store.EventType, start time.Time) bool {
	rules, err := st.ListAvailability(r.Context(), event.HostID)
	if err != nil {
		slog.ErrorContext(r.Context(), "bookings: availability lookup", "err", err, "host_id", event.HostID)
		WriteError(w, http.StatusInternalServerError, "INTERNAL", "could not verify availability")
		return false
	}
	if !bookingFitsAvailability(rules, event, start, time.Now().UTC()) {
		WriteError(w, http.StatusConflict, "SLOT_UNAVAILABLE", "this time is outside the host's booking availability; please choose another slot")
		return false
	}
	return true
}

func bookingFitsAvailability(rules []store.AvailabilityRule, event store.EventType, start, now time.Time) bool {
	if event.DurationMin <= 0 || start.Before(now.Add(time.Duration(event.MinNoticeHours)*time.Hour)) {
		return false
	}
	if event.MaxDaysAhead > 0 && !start.Before(now.AddDate(0, 0, event.MaxDaysAhead)) {
		return false
	}
	end := start.Add(time.Duration(event.DurationMin) * time.Minute)
	for _, rule := range rules {
		loc, err := time.LoadLocation(rule.Timezone)
		if err != nil {
			continue
		}
		local := start.In(loc)
		if int(local.Weekday()) != rule.DayOfWeek {
			continue
		}
		windowStart := combineDateTime(local, rule.StartTime, loc)
		windowEnd := combineDateTime(local, rule.EndTime, loc)
		if !windowStart.IsZero() && !windowEnd.IsZero() && !start.Before(windowStart) && !end.After(windowEnd) {
			return true
		}
	}
	return false
}
