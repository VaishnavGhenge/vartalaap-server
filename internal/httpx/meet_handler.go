package httpx

import (
	"crypto/rand"
	"encoding/json"
	"net/http"
	"time"
)

const meetAlphabet = "abcdefghijkmnpqrstuvwxyz23456789"

type newMeetResponse struct {
	MeetCode string `json:"meetCode"`
}

func NewMeetHandler(allowedOrigins []string, limiter *RateLimiter, registrars ...func(room string, now time.Time)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !enforceAPIRequest(w, r, allowedOrigins, http.MethodPost, limiter) {
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		code, err := generateMeetCode()
		if err != nil {
			http.Error(w, "could not create meet", http.StatusInternalServerError)
			return
		}
		if len(registrars) > 0 && registrars[0] != nil {
			registrars[0](code, time.Now().UTC())
		}

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(newMeetResponse{MeetCode: code})
	}
}

func generateMeetCode() (string, error) {
	bytes := make([]byte, 10)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}

	chars := make([]byte, len(bytes)+2)
	for i, b := range bytes {
		offset := i
		if i >= 3 {
			offset++
		}
		if i >= 7 {
			offset++
		}
		chars[offset] = meetAlphabet[int(b)%len(meetAlphabet)]
	}
	chars[3] = '-'
	chars[8] = '-'

	return string(chars), nil
}
