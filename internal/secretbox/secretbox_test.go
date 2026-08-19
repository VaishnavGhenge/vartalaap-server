package secretbox

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

func testKey(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, KeyBytes)
	if _, err := rand.Read(k); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return k
}

func TestRoundTrip(t *testing.T) {
	b, err := New(testKey(t))
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	const plain = "1//0gRefreshTokenLookingThing_-abc123"
	ct, err := b.Encrypt(plain)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if strings.Contains(ct, plain) {
		t.Fatal("ciphertext contains plaintext")
	}
	if !strings.HasPrefix(ct, "v1:") {
		t.Fatalf("missing version prefix: %q", ct)
	}
	got, err := b.Decrypt(ct)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if got != plain {
		t.Fatalf("want %q, got %q", plain, got)
	}
}

// Two encryptions of the same value must differ — a deterministic ciphertext
// would let anyone with the database tell which hosts share a token value.
func TestEncryptUsesFreshNonce(t *testing.T) {
	b, _ := New(testKey(t))
	a, _ := b.Encrypt("same")
	c, _ := b.Encrypt("same")
	if a == c {
		t.Fatal("nonce reuse: identical ciphertexts for identical plaintext")
	}
}

func TestDecryptRejectsTampering(t *testing.T) {
	b, _ := New(testKey(t))
	ct, _ := b.Encrypt("sensitive")
	raw := []byte(ct)
	// Flip a bit in the middle of the payload.
	raw[len(raw)/2] ^= 0x01
	if _, err := b.Decrypt(string(raw)); err == nil {
		t.Fatal("tampered ciphertext decrypted without error")
	}
}

func TestDecryptWithWrongKeyFails(t *testing.T) {
	a, _ := New(testKey(t))
	other, _ := New(testKey(t))
	ct, _ := a.Encrypt("sensitive")
	if _, err := other.Decrypt(ct); err == nil {
		t.Fatal("decrypted with the wrong key")
	}
}

func TestDecryptRejectsMalformed(t *testing.T) {
	b, _ := New(testKey(t))
	for name, input := range map[string]string{
		"empty":         "",
		"no prefix":     base64.RawURLEncoding.EncodeToString([]byte("whatever")),
		"bad base64":    "v1:!!!not base64!!!",
		"short payload": "v1:" + base64.RawURLEncoding.EncodeToString([]byte("tiny")),
	} {
		if _, err := b.Decrypt(input); err == nil {
			t.Fatalf("%s: expected error", name)
		}
	}
}

func TestNewRejectsWrongKeyLength(t *testing.T) {
	if _, err := New(make([]byte, 16)); err == nil {
		t.Fatal("accepted a 16-byte key for AES-256")
	}
}

// Operators reach for either `openssl rand -hex 32` or `-base64 32`; both must
// work, and a key that decodes to the wrong length must be rejected rather
// than silently padded.
func TestNewFromEncodedKey(t *testing.T) {
	key := testKey(t)
	encodings := map[string]string{
		"hex":        hex.EncodeToString(key),
		"base64 std": base64.StdEncoding.EncodeToString(key),
		"base64 raw": base64.RawStdEncoding.EncodeToString(key),
		"base64 url": base64.URLEncoding.EncodeToString(key),
		"whitespace": "  " + hex.EncodeToString(key) + "\n",
	}
	for name, encoded := range encodings {
		box, err := NewFromEncodedKey(encoded)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		ct, _ := box.Encrypt("x")
		if got, err := box.Decrypt(ct); err != nil || got != "x" {
			t.Fatalf("%s: round trip failed: %v", name, err)
		}
	}

	if _, err := NewFromEncodedKey(""); !errors.Is(err, ErrNoKey) {
		t.Fatalf("empty key: want ErrNoKey, got %v", err)
	}
	if _, err := NewFromEncodedKey("tooshort"); err == nil {
		t.Fatal("accepted a key that decodes to the wrong length")
	}
}

// All encodings of the same key must produce interchangeable boxes, or an
// operator who reformats the env var locks every host out of their calendar.
func TestEncodingsProduceSameKey(t *testing.T) {
	key := testKey(t)
	fromHex, _ := NewFromEncodedKey(hex.EncodeToString(key))
	fromB64, _ := NewFromEncodedKey(base64.StdEncoding.EncodeToString(key))
	ct, _ := fromHex.Encrypt("shared")
	got, err := fromB64.Decrypt(ct)
	if err != nil || got != "shared" {
		t.Fatalf("hex and base64 keys are not interchangeable: %v", err)
	}
}
