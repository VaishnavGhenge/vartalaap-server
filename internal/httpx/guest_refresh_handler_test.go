package httpx

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/vaishnavghenge/vartalaap-server/internal/auth"
)

func TestGuestTokenRefreshPreservesGuestAndRoom(t *testing.T) {
	const secret = "test-secret"
	old, err := auth.SignGuestToken("g:peer-1", "abc-defg-hij", secret, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	var gatedRoom string
	h := NewGuestTokenRefreshHandler(nil, secret, func(_ context.Context, room string, activate bool) error {
		gatedRoom = room
		if activate {
			t.Fatal("refresh must not activate an expired room")
		}
		return nil
	})
	req := httptest.NewRequest(http.MethodPost, "/auth/guest/refresh", nil)
	req.Header.Set("Authorization", "Bearer "+old)
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var body guestTokenResponse
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	claims, err := auth.VerifyAccessToken(body.SfuToken, secret)
	if err != nil {
		t.Fatal(err)
	}
	if claims.UserID != "g:peer-1" || claims.RoomID != "abc-defg-hij" {
		t.Fatalf("unexpected refreshed claims: %+v", claims)
	}
	if gatedRoom != "abc-defg-hij" {
		t.Fatalf("gated room=%q", gatedRoom)
	}
}

func TestGuestTokenRefreshRejectsAccessToken(t *testing.T) {
	const secret = "test-secret"
	token, err := auth.SignAccessToken("user-1", secret, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	h := NewGuestTokenRefreshHandler(nil, secret, nil)
	req := httptest.NewRequest(http.MethodPost, "/auth/guest/refresh", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d", rec.Code)
	}
}

func TestGuestTokenRefreshHonorsRoomGate(t *testing.T) {
	const secret = "test-secret"
	token, err := auth.SignGuestToken("g:peer-1", "abc-defg-hij", secret, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	h := NewGuestTokenRefreshHandler(nil, secret, func(context.Context, string, bool) error {
		return context.DeadlineExceeded
	})
	req := httptest.NewRequest(http.MethodPost, "/auth/guest/refresh", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d", rec.Code)
	}
}
