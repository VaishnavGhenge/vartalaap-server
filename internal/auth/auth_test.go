package auth_test

import (
	"context"
	"testing"
	"time"

	"github.com/vaishnavghenge/vartalaap-server/internal/auth"
)

func TestHashAndCheckPassword(t *testing.T) {
	hash, err := auth.HashPassword("correct-horse-battery")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if !auth.CheckPassword("correct-horse-battery", hash) {
		t.Fatal("CheckPassword: expected true for correct password")
	}
	if auth.CheckPassword("wrong-password", hash) {
		t.Fatal("CheckPassword: expected false for wrong password")
	}
}

func TestSignAndVerifyAccessToken(t *testing.T) {
	secret := "test-secret-32-bytes-long-enough!"
	userID := "user-123"

	token, err := auth.SignAccessToken(userID, secret, time.Minute)
	if err != nil {
		t.Fatalf("SignAccessToken: %v", err)
	}

	claims, err := auth.VerifyAccessToken(token, secret)
	if err != nil {
		t.Fatalf("VerifyAccessToken: %v", err)
	}
	if claims.UserID != userID {
		t.Fatalf("expected userID %q, got %q", userID, claims.UserID)
	}
}

func TestVerifyAccessTokenWrongSecret(t *testing.T) {
	token, _ := auth.SignAccessToken("u1", "secret-a", time.Minute)
	if _, err := auth.VerifyAccessToken(token, "secret-b"); err == nil {
		t.Fatal("expected error for wrong secret, got nil")
	}
}

func TestVerifyAccessTokenExpired(t *testing.T) {
	token, _ := auth.SignAccessToken("u1", "secret", -time.Second)
	if _, err := auth.VerifyAccessToken(token, "secret"); err == nil {
		t.Fatal("expected error for expired token, got nil")
	}
}

func TestVerifyAccessTokenRejectsEmptyUserID(t *testing.T) {
	token, _ := auth.SignAccessToken("", "secret", time.Minute)
	if _, err := auth.VerifyAccessToken(token, "secret"); err == nil {
		t.Fatal("expected error for empty user id, got nil")
	}
}

func TestNewRefreshTokenIsUnique(t *testing.T) {
	raw1, hash1, err := auth.NewRefreshToken()
	if err != nil {
		t.Fatalf("NewRefreshToken: %v", err)
	}
	raw2, hash2, _ := auth.NewRefreshToken()

	if raw1 == raw2 {
		t.Fatal("NewRefreshToken: expected unique raw tokens")
	}
	if hash1 == hash2 {
		t.Fatal("NewRefreshToken: expected unique hashes")
	}
}

func TestHashRefreshTokenDeterministic(t *testing.T) {
	raw := "some-opaque-token-value"
	h1 := auth.HashRefreshToken(raw)
	h2 := auth.HashRefreshToken(raw)
	if h1 != h2 {
		t.Fatal("HashRefreshToken: expected same hash for same input")
	}
	if h1 == raw {
		t.Fatal("HashRefreshToken: hash must differ from raw value")
	}
}

func TestContextRoundtrip(t *testing.T) {
	ctx := auth.WithUserID(context.Background(), "u-99")
	id, ok := auth.UserIDFromContext(ctx)
	if !ok || id != "u-99" {
		t.Fatalf("expected userID %q ok=true, got %q ok=%v", "u-99", id, ok)
	}
}

func TestContextMissing(t *testing.T) {
	_, ok := auth.UserIDFromContext(context.Background())
	if ok {
		t.Fatal("expected ok=false for context with no userID")
	}
}
