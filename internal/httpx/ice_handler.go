package httpx

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"time"

	"github.com/vaishnavghenge/vartalaap-server/internal/cfturn"
)

func NewIceHandler(cf *cfturn.Client, allowedOrigins []string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && slices.Contains(allowedOrigins, origin) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
		}
		w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
		defer cancel()

		creds, err := cf.Generate(ctx, 3600) // 1 hour
		if err != nil {
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
