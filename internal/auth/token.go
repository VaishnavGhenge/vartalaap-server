package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

type Claims struct {
	UserID string `json:"uid"`
	RoomID string `json:"rid,omitempty"`
	// Purpose marks a token minted for one specific side-quest (currently only
	// the Google Calendar OAuth `state` round-trip). VerifyAccessToken rejects
	// any token carrying one, which is the point: a purpose token travels
	// through a browser redirect and a third party's servers, so it must never
	// be usable as an API credential if it leaks into a log or a referrer.
	Purpose string `json:"pur,omitempty"`
	jwt.RegisteredClaims
}

func SignAccessToken(userID, secret string, ttl time.Duration) (string, error) {
	claims := Claims{
		UserID: userID,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(ttl)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
	}
	t := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return t.SignedString([]byte(secret))
}

func VerifyAccessToken(tokenStr, secret string) (*Claims, error) {
	t, err := jwt.ParseWithClaims(tokenStr, &Claims{}, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return []byte(secret), nil
	})
	if err != nil {
		return nil, err
	}
	claims, ok := t.Claims.(*Claims)
	if !ok || !t.Valid {
		return nil, fmt.Errorf("invalid claims")
	}
	if claims.UserID == "" {
		return nil, fmt.Errorf("missing user id")
	}
	if claims.Purpose != "" {
		return nil, fmt.Errorf("purpose token is not an access token")
	}
	return claims, nil
}

// SignPurposeToken mints a short-lived, single-purpose token. Used for the
// OAuth `state` parameter: the value has to survive a browser round-trip
// through Google and come back provably ours and provably tied to one user,
// which is exactly a signed claim and nothing more.
//
// Keep TTLs tight. A state token only needs to outlive a consent screen.
func SignPurposeToken(userID, purpose, secret string, ttl time.Duration) (string, error) {
	if purpose == "" {
		return "", fmt.Errorf("purpose is required")
	}
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	claims := Claims{
		UserID:  userID,
		Purpose: purpose,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(ttl)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(secret))
}

// VerifyPurposeToken checks signature, expiry, AND that the token was minted
// for the purpose the caller expects. The purpose check is what stops a state
// token from being replayed against a different flow later.
func VerifyPurposeToken(tokenStr, purpose, secret string) (string, error) {
	t, err := jwt.ParseWithClaims(tokenStr, &Claims{}, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return []byte(secret), nil
	})
	if err != nil {
		return "", err
	}
	claims, ok := t.Claims.(*Claims)
	if !ok || !t.Valid {
		return "", fmt.Errorf("invalid claims")
	}
	if claims.UserID == "" {
		return "", fmt.Errorf("missing user id")
	}
	if claims.Purpose != purpose {
		return "", fmt.Errorf("wrong token purpose")
	}
	return claims.UserID, nil
}

// SignGuestToken creates a room-scoped JWT for unauthenticated call participants.
// The SFU handler enforces the room scope.
func SignGuestToken(guestID, roomID, secret string, ttl time.Duration) (string, error) {
	if ttl <= 0 {
		ttl = time.Hour
	}
	claims := Claims{
		UserID: guestID,
		RoomID: roomID,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(ttl)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
	}
	t := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return t.SignedString([]byte(secret))
}

// NewRefreshToken generates a cryptographically random opaque token.
// Returns the raw token (sent to client) and its SHA-256 hash (stored in DB).
func NewRefreshToken() (raw, hash string, err error) {
	b := make([]byte, 32)
	if _, err = rand.Read(b); err != nil {
		return "", "", err
	}
	raw = hex.EncodeToString(b)
	sum := sha256.Sum256([]byte(raw))
	hash = hex.EncodeToString(sum[:])
	return raw, hash, nil
}

func HashRefreshToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}
