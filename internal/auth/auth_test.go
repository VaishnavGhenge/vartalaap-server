package auth_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
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

// Guest tokens are the security boundary for invited (non-account) call
// participants: the RoomID claim is what scopes them to one specific room.
// If the wire encoding ever changes (e.g. someone renames the JSON tag from
// "rid" or drops it on sign), the SFU room-scope check silently degrades to
// "any guest can join any room". This test pins the round-trip.
func TestSignGuestToken_CarriesRoomIDAndUserIDClaims(t *testing.T) {
	secret := "test-secret"
	tok, err := auth.SignGuestToken("guest-abc", "room-xyz", secret, time.Minute)
	if err != nil {
		t.Fatalf("SignGuestToken: %v", err)
	}
	claims, err := auth.VerifyAccessToken(tok, secret)
	if err != nil {
		t.Fatalf("VerifyAccessToken: %v", err)
	}
	if claims.UserID != "guest-abc" {
		t.Errorf("UserID: expected guest-abc, got %q", claims.UserID)
	}
	if claims.RoomID != "room-xyz" {
		t.Errorf("RoomID: expected room-xyz, got %q — the SFU scope check needs this claim", claims.RoomID)
	}
}

// SignGuestToken documents a 1h default when ttl <= 0. The default matters
// because invite links typically pre-sign tokens before the room is even
// open, and a 0-default would mint instantly-expired tokens.
func TestSignGuestToken_DefaultsToOneHourTTL(t *testing.T) {
	secret := "test-secret"
	before := time.Now()
	tok, err := auth.SignGuestToken("g1", "r1", secret, 0)
	if err != nil {
		t.Fatalf("SignGuestToken: %v", err)
	}
	claims, err := auth.VerifyAccessToken(tok, secret)
	if err != nil {
		t.Fatalf("VerifyAccessToken: %v", err)
	}
	expSeconds := claims.ExpiresAt.Sub(before).Seconds()
	// Accept anything in [59m, 61m]. Tighter and we get flakes from scheduling.
	if expSeconds < 59*60 || expSeconds > 61*60 {
		t.Fatalf("expected ~1h TTL by default, got %.0fs (claims.ExpiresAt=%v)", expSeconds, claims.ExpiresAt)
	}
}

// The "alg=none" / algorithm-confusion attack: a maliciously-forged token sets
// its header alg to "none" (or to an asymmetric algorithm) so the verifier
// skips signature checking. token.go has an explicit
//
//	if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok { return error }
//
// guard against this. The test below crafts a raw alg=none JWT and asserts
// VerifyAccessToken rejects it, even though the payload is otherwise valid.
// Without this guard, an attacker who only knows a userID can mint a token.
func TestVerifyAccessToken_RejectsAlgNoneAttack(t *testing.T) {
	enc := func(v any) string {
		b, _ := json.Marshal(v)
		return strings.TrimRight(base64.URLEncoding.EncodeToString(b), "=")
	}
	header := enc(map[string]string{"alg": "none", "typ": "JWT"})
	payload := enc(map[string]any{
		"uid": "user-anyone",
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	forged := header + "." + payload + "." // alg=none = empty signature

	if _, err := auth.VerifyAccessToken(forged, "any-secret"); err == nil {
		t.Fatal("alg=none token must be rejected; verify accepted it (algorithm-confusion vulnerability)")
	}
}

func TestRoomIDContextRoundtrip(t *testing.T) {
	ctx := auth.WithRoomID(context.Background(), "room-42")
	if got := auth.RoomIDFromContext(ctx); got != "room-42" {
		t.Fatalf("expected room-42, got %q", got)
	}
}

func TestRoomIDFromContext_EmptyWhenUnset(t *testing.T) {
	// Important: this returns "" — NOT a "missing" signal — because handlers
	// distinguish "authenticated user with no room scope" (regular user) from
	// "guest with RoomID claim" by checking guestRoom != "". An accidental
	// switch to a sentinel like "unknown" would silently break that branch.
	if got := auth.RoomIDFromContext(context.Background()); got != "" {
		t.Fatalf("expected empty string when room scope not set, got %q", got)
	}
}
