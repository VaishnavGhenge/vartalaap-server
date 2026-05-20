package httpx

import (
	"net/http"
	"strings"

	"github.com/vaishnavghenge/vartalaap-server/internal/auth"
)

// RequireAuth wraps a handler that needs a real user identity. Validates the
// Bearer access token, REJECTS guest/room-scoped tokens (those whose RoomID
// claim is set — they identify a transient guest, not a user record), and
// injects the userID into context. Returns 401 on any failure.
//
// Guest tokens use the same JWT signature and reuse the UserID claim for a
// synthetic guest ID, so without the RoomID check they would pass verification
// and reach handlers like /auth/me, where GetUserByID(guestID) blows up with
// a 500. This middleware is the defense.
func RequireAuth(jwtSecret string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, ok := verifyBearer(w, r, jwtSecret)
		if !ok {
			return
		}
		if claims.RoomID != "" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		ctx := auth.WithUserID(r.Context(), claims.UserID)
		next(w, r.WithContext(ctx))
	}
}

// RequireRoomMember wraps a handler that accepts either a logged-in user OR a
// guest with a room-scoped token. Used by the SFU proxy, which must serve both
// (hosts authenticate as users; invited guests authenticate via guest JWTs).
// Handlers downstream must enforce the room scope themselves by comparing the
// requested roomId against auth.RoomIDFromContext.
func RequireRoomMember(jwtSecret string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, ok := verifyBearer(w, r, jwtSecret)
		if !ok {
			return
		}
		ctx := auth.WithUserID(r.Context(), claims.UserID)
		ctx = auth.WithRoomID(ctx, claims.RoomID)
		next(w, r.WithContext(ctx))
	}
}

func verifyBearer(w http.ResponseWriter, r *http.Request, jwtSecret string) (*auth.Claims, bool) {
	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(header, "Bearer ") {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return nil, false
	}
	claims, err := auth.VerifyAccessToken(strings.TrimPrefix(header, "Bearer "), jwtSecret)
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return nil, false
	}
	return claims, true
}
