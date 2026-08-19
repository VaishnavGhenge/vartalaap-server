package httpx

import (
	"context"
	"crypto/subtle"
	"net/http"
	"time"

	"github.com/vaishnavghenge/vartalaap-server/internal/auth"
	"github.com/vaishnavghenge/vartalaap-server/internal/store"
)

// GuestTokenDeps bundles collaborators for the guest token endpoint.
type GuestTokenDeps struct {
	Store      store.Storer
	JWTSecret  string
	RoomWindow BookingRoomWindow
}

type guestTokenRequest struct {
	MeetCode string `json:"meetCode"`
	Token    string `json:"token"`
}

type guestTokenResponse struct {
	SfuToken string `json:"sfuToken"`
}

// NewGuestTokenHandler returns POST /auth/guest — public (no auth required).
// Validates the cancel token from the booking confirmation email and issues
// a room-scoped guest JWT for SFU access.
func NewGuestTokenHandler(allowedOrigins []string, deps GuestTokenDeps) http.HandlerFunc {
	lim := NewRateLimiter(30, 60)
	return func(w http.ResponseWriter, r *http.Request) {
		if !enforceAPIRequest(w, r, allowedOrigins, http.MethodPost, lim) {
			return
		}
		var req guestTokenRequest
		if err := BindJSON(r, &req); err != nil {
			WriteFieldError(w, http.StatusBadRequest, err)
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()

		b, err := deps.Store.GetBookingByMeetCode(ctx, req.MeetCode)

		// Constant-time token comparison: always compare even on error so the
		// response time doesn't leak whether the meet code exists. We use a
		// fixed sentinel when there's no booking to compare against.
		sentinel := "00000000000000000000000000000000"
		storedToken := sentinel
		if err == nil {
			storedToken = b.CancelToken
		}
		tokenMatch := subtle.ConstantTimeCompare([]byte(req.Token), []byte(storedToken)) == 1

		// Both "no such booking" and "wrong token" return the same 403 so an
		// attacker probing meet codes can't distinguish the two cases.
		if err != nil || !tokenMatch {
			WriteError(w, http.StatusForbidden, "INVALID_TOKEN", "invalid credentials")
			return
		}

		if b.Status == "cancelled" {
			WriteError(w, http.StatusForbidden, "BOOKING_CANCELLED", "this booking was cancelled")
			return
		}

		access := BookingRoomAccessFor(*b, time.Now().UTC(), deps.RoomWindow)
		if access.Status == "ended" {
			WriteError(w, http.StatusForbidden, "ROOM_EXPIRED", "this session has ended")
			return
		}

		// Apply the same defaults as BookingRoomAccessFor when fields are zero.
		closeAfter := deps.RoomWindow.CloseAfter
		if closeAfter <= 0 {
			closeAfter = 30 * time.Minute
		}
		ttl := time.Until(b.EndsAt.UTC().Add(closeAfter))
		if ttl <= 0 {
			WriteError(w, http.StatusForbidden, "ROOM_EXPIRED", "this session has ended")
			return
		}

		token, err := auth.SignGuestToken("g:"+b.ID, b.MeetCode, deps.JWTSecret, ttl)
		if err != nil {
			WriteError(w, http.StatusInternalServerError, "INTERNAL", "could not issue token")
			return
		}

		WriteJSON(w, http.StatusOK, guestTokenResponse{SfuToken: token})
	}
}
