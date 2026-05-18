package httpx

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"
)

func TestNewMeetHandlerIssuesServerMeetCode(t *testing.T) {
	handler := NewMeetHandler([]string{"https://app.example"}, NewRateLimiter(10, 10))

	req := httptest.NewRequest(http.MethodPost, "/meets/new", nil)
	req.Header.Set("Origin", "https://app.example")
	rec := httptest.NewRecorder()

	handler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://app.example" {
		t.Fatalf("expected CORS allow origin, got %q", got)
	}

	var body newMeetResponse
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !regexp.MustCompile(`^[a-z2-9]{3}-[a-z2-9]{4}-[a-z2-9]{3}$`).MatchString(body.MeetCode) {
		t.Fatalf("unexpected meet code %q", body.MeetCode)
	}
}

func TestNewMeetHandlerRejectsDisallowedOrigin(t *testing.T) {
	handler := NewMeetHandler([]string{"https://app.example"}, NewRateLimiter(10, 10))

	req := httptest.NewRequest(http.MethodPost, "/meets/new", nil)
	req.Header.Set("Origin", "https://evil.example")
	rec := httptest.NewRecorder()

	handler(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rec.Code)
	}
}

func TestNewMeetHandlerRateLimitsPerClient(t *testing.T) {
	handler := NewMeetHandler([]string{"https://app.example"}, NewRateLimiter(1, 1))

	for i, want := range []int{http.StatusOK, http.StatusTooManyRequests} {
		req := httptest.NewRequest(http.MethodPost, "/meets/new", nil)
		req.RemoteAddr = "203.0.113.10:12345"
		req.Header.Set("Origin", "https://app.example")
		rec := httptest.NewRecorder()

		handler(rec, req)

		if rec.Code != want {
			t.Fatalf("request %d: expected %d, got %d", i+1, want, rec.Code)
		}
	}
}
