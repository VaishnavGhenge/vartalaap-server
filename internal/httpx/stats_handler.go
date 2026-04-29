package httpx

import (
	"encoding/json"
	"net/http"

	"github.com/vaishnavghenge/vartalaap-server/internal/metrics"
)

func NewStatsHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(metrics.Gather())
	}
}
