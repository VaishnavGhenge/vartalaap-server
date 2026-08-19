// Package secretbox encrypts small secrets at rest with AES-256-GCM.
//
// Used for OAuth tokens (internal/store calendar_connections). The threat it
// addresses is narrow and worth stating plainly: a stolen database dump, or a
// backup left somewhere it shouldn't be, must not hand the attacker live
// access to a host's Google Calendar. It does nothing about an attacker who
// already has the running process's environment — the key lives there.
//
// Ciphertext format is "v1:<base64url(nonce||ciphertext||tag)>". The version
// prefix exists so a future key rotation can decrypt old values while writing
// new ones, without a migration that touches every row.
package secretbox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// KeyBytes is the AES-256 key length. Keys shorter than this are rejected at
// construction rather than silently stretched.
const KeyBytes = 32

const versionPrefix = "v1:"

// ErrNoKey is returned by NewFromEnvValue when the key is absent. Callers use
// it to distinguish "operator hasn't configured calendar sync" (a warning, the
// feature stays off) from "the configured key is broken" (a startup failure).
var ErrNoKey = errors.New("secretbox: no key configured")

type Box struct {
	aead cipher.AEAD
}

// New builds a Box from raw key bytes.
func New(key []byte) (*Box, error) {
	if len(key) != KeyBytes {
		return nil, fmt.Errorf("secretbox: key must be %d bytes, got %d", KeyBytes, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("secretbox: new cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("secretbox: new gcm: %w", err)
	}
	return &Box{aead: aead}, nil
}

// NewFromEncodedKey accepts the key as 64 hex characters or as base64 (either
// standard or URL alphabet, padded or not). Both encodings are accepted
// because `openssl rand -hex 32` and `openssl rand -base64 32` are equally
// likely to be what an operator reaches for, and silently misreading one as
// the other would produce a working-but-wrong key.
func NewFromEncodedKey(s string) (*Box, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, ErrNoKey
	}
	if b, err := hex.DecodeString(s); err == nil && len(b) == KeyBytes {
		return New(b)
	}
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding, base64.RawStdEncoding,
		base64.URLEncoding, base64.RawURLEncoding,
	} {
		if b, err := enc.DecodeString(s); err == nil && len(b) == KeyBytes {
			return New(b)
		}
	}
	return nil, fmt.Errorf("secretbox: key must decode to %d bytes from hex or base64", KeyBytes)
}

// Encrypt seals plaintext with a fresh random nonce.
func (b *Box) Encrypt(plaintext string) (string, error) {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("secretbox: nonce: %w", err)
	}
	sealed := b.aead.Seal(nonce, nonce, []byte(plaintext), nil)
	return versionPrefix + base64.RawURLEncoding.EncodeToString(sealed), nil
}

// Decrypt opens a value produced by Encrypt. A tampered or truncated value
// fails authentication and returns an error rather than partial plaintext.
func (b *Box) Decrypt(encoded string) (string, error) {
	if !strings.HasPrefix(encoded, versionPrefix) {
		return "", errors.New("secretbox: missing version prefix")
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(encoded, versionPrefix))
	if err != nil {
		return "", fmt.Errorf("secretbox: decode: %w", err)
	}
	ns := b.aead.NonceSize()
	if len(raw) < ns {
		return "", errors.New("secretbox: ciphertext too short")
	}
	plaintext, err := b.aead.Open(nil, raw[:ns], raw[ns:], nil)
	if err != nil {
		// Deliberately vague: the caller logs this, and distinguishing
		// "wrong key" from "tampered" tells an attacker reading logs more
		// than it tells us.
		return "", errors.New("secretbox: decrypt failed")
	}
	return string(plaintext), nil
}
