package httpx

import (
	"net/http"
	"strings"

	"github.com/vaishnavghenge/vartalaap-server/internal/auth"
)

// RequireAuth wraps a handler, validating the Bearer access token and
// injecting the userID into context. Returns 401 on any failure.
func RequireAuth(jwtSecret string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		header := r.Header.Get("Authorization")
		if !strings.HasPrefix(header, "Bearer ") {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		claims, err := auth.VerifyAccessToken(strings.TrimPrefix(header, "Bearer "), jwtSecret)
		if err != nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r.WithContext(auth.WithUserID(r.Context(), claims.UserID)))
	}
}
