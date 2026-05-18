package httpx

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/getsentry/sentry-go"
	"github.com/vaishnavghenge/vartalaap-server/internal/cfturn"
	"github.com/vaishnavghenge/vartalaap-server/internal/metrics"
)

type RoomAccessGate func(ctx context.Context, room string, activate bool) error

func NewIceHandler(cf *cfturn.Client, allowedOrigins []string, limiter *RateLimiter, gates ...RoomAccessGate) http.HandlerFunc {
	var gate RoomAccessGate
	if len(gates) > 0 {
		gate = gates[0]
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if !enforceAPIRequest(w, r, allowedOrigins, http.MethodPost, limiter) {
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			RoomID string `json:"roomId"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.RoomID == "" {
			WriteError(w, http.StatusBadRequest, "ROOM_REQUIRED", "roomId is required")
			return
		}
		if gate != nil {
			if err := gate(r.Context(), req.RoomID, false); err != nil {
				WriteError(w, http.StatusForbidden, roomAccessCode(err), roomAccessMessage(err))
				return
			}
		}

		ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
		defer cancel()

		metrics.IceRequestsTotal.Inc()
		creds, err := cf.Generate(ctx, 3600) // 1 hour
		if err != nil {
			metrics.IceErrorsTotal.Inc()
			// context.Canceled means the client disconnected before we responded
			// (AbortController timeout or navigation). Not a Cloudflare failure.
			if !errors.Is(err, context.Canceled) {
				sentry.CaptureException(err)
			}
			// Cloudflare TURN unavailable — return a public STUN fallback so
			// peers can still connect on the same network (e.g. localhost dev).
			creds = cfturn.CredentialsResponse{
				IceServers: []cfturn.IceServer{
					{URLs: []string{"stun:stun.l.google.com:19302"}},
				},
			}
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(creds)
	}
}
