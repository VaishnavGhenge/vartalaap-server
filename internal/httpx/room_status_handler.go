package httpx

import (
	"context"
	"net/http"
	"regexp"
	"time"
)

// RoomStatusResult is the response body for GET /room/status.
type RoomStatusResult struct {
	Status   string     `json:"status"` // open | too_early | ended | cancelled | not_found
	Message  string     `json:"message,omitempty"`
	OpensAt  *time.Time `json:"opensAt,omitempty"`  // set when status == too_early
	ClosesAt *time.Time `json:"closesAt,omitempty"` // set when status == open so clients can show expiry warnings
}

// RoomStatusChecker is called by NewRoomStatusHandler to determine access.
// Implementations must be concurrency-safe.
type RoomStatusChecker func(ctx context.Context, room string) RoomStatusResult

var roomCodePattern = regexp.MustCompile(`^[a-z2-9]{3}-[a-z2-9]{4}-[a-z2-9]{3}$`)

// NewRoomStatusHandler returns a public GET /room/status?code={code} handler.
// No auth required — it reveals only whether a room is accessible, not its contents.
func NewRoomStatusHandler(allowedOrigins []string, check RoomStatusChecker) http.HandlerFunc {
	lim := NewRateLimiter(60, 120)
	return func(w http.ResponseWriter, r *http.Request) {
		if !enforceAPIRequest(w, r, allowedOrigins, "GET", lim) {
			return
		}
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		code := r.URL.Query().Get("code")
		if !roomCodePattern.MatchString(code) {
			WriteError(w, http.StatusBadRequest, "INVALID_CODE", "invalid room code")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		WriteJSON(w, http.StatusOK, check(ctx, code))
	}
}
