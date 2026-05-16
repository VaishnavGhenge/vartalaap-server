package httpx

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/mail"
	"regexp"
	"strings"
	"time"
)

// Default maximum request body size. Endpoints that need to accept larger
// bodies (SDP offers for the SFU, for instance) override via the
// enforceAPIRequestWithLimit path instead of decoding through BindJSON.
const defaultMaxBodyBytes = 64 * 1024

// FieldError is returned by validators so handlers can render precise messages
// without a sprawl of stringly-typed branches. The handler is expected to
// translate (Field, Code, Message) into a writeError call.
//
// Code is the stable, machine-readable identifier; UI gates branch on it.
// Message is the human-readable fallback shown if the client doesn't have a
// translation for the code yet.
type FieldError struct {
	Field   string
	Code    string
	Message string
}

func (e *FieldError) Error() string {
	if e.Field == "" {
		return e.Message
	}
	return fmt.Sprintf("%s: %s", e.Field, e.Message)
}

// Field constructors keep validator call sites short. They wrap a constant
// code so we can grep for every place that emits a given error.
func badField(field, code, msg string) *FieldError {
	return &FieldError{Field: field, Code: code, Message: msg}
}

// ─── Decoding ────────────────────────────────────────────────────────────────

// BindJSON decodes the request body into dst with a size cap and rejection of
// unknown fields. Unknown-field strictness catches typos in client payloads
// at the boundary instead of letting them silently default; the cost is that
// a client adding new optional fields will break against old servers, but
// that's the right trade for a small API team.
func BindJSON(r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(nil, r.Body, defaultMaxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		if errors.Is(err, io.EOF) {
			return badField("", "EMPTY_BODY", "request body is required")
		}
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return badField("", "BODY_TOO_LARGE", "request body too large")
		}
		// json.Decode errors here are all client-side mistakes; surface the
		// message verbatim so the UI can show "unknown field xyz" or "invalid
		// JSON". Server-side faults can't reach this branch.
		return badField("", "INVALID_JSON", err.Error())
	}
	return nil
}

// ─── String normalization ────────────────────────────────────────────────────

// ValidateEmail trims, lowercases, and parses an email. The parsed canonical
// form is what we persist; if you store raw user input you'll discover the
// hard way that "Alice@Example.com" and "alice@example.com" both exist.
func ValidateEmail(field, raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", badField(field, "REQUIRED", field+" is required")
	}
	addr, err := mail.ParseAddress(raw)
	if err != nil {
		return "", badField(field, "INVALID_EMAIL", "must be a valid email address")
	}
	return strings.ToLower(addr.Address), nil
}

var slugPattern = regexp.MustCompile(`^[a-z0-9-]{1,40}$`)

// ValidateSlug enforces the public-URL-safe slug shape (lowercase letters,
// digits, hyphens; 1–40 chars). Trims + lowercases first so a slightly sloppy
// client doesn't trip the regex.
func ValidateSlug(field, raw string) (string, error) {
	v := strings.ToLower(strings.TrimSpace(raw))
	if v == "" {
		return "", badField(field, "REQUIRED", field+" is required")
	}
	if !slugPattern.MatchString(v) {
		return "", badField(field, "INVALID_SLUG", field+" must be 1–40 lowercase letters, digits, or hyphens")
	}
	return v, nil
}

// ValidateClockTime accepts "HH:MM" or "HH:MM:SS" and returns the canonical
// "HH:MM" form. Used for availability rule edges where Postgres receives a
// `time` value — Postgres-format formatting happens at the store layer.
func ValidateClockTime(field, raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", badField(field, "REQUIRED", field+" is required")
	}
	if t, err := time.Parse("15:04", raw); err == nil {
		return t.Format("15:04"), nil
	}
	if t, err := time.Parse("15:04:05", raw); err == nil {
		return t.Format("15:04"), nil
	}
	return "", badField(field, "INVALID_TIME", field+" must be HH:MM")
}

// ValidateTimezone rejects unknown IANA zones at the API boundary so a buggy
// client can't poison stored rules with values Postgres accepts but our slot
// generator later trips over. Returns the input trimmed but otherwise
// unmodified — IANA names are case-sensitive.
func ValidateTimezone(field, raw string) (string, error) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return "", badField(field, "REQUIRED", field+" is required")
	}
	if _, err := time.LoadLocation(v); err != nil {
		return "", badField(field, "INVALID_TIMEZONE", field+" must be a valid IANA timezone")
	}
	return v, nil
}

// ValidateRFC3339Future parses an RFC3339 timestamp and rejects anything more
// than `grace` in the past, accommodating small clock skew. Returns the time
// in UTC so downstream comparisons are unambiguous.
func ValidateRFC3339Future(field, raw string, grace time.Duration) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, badField(field, "REQUIRED", field+" is required")
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, badField(field, "INVALID_TIMESTAMP", field+" must be an RFC3339 timestamp")
	}
	if t.Before(time.Now().Add(-grace)) {
		return time.Time{}, badField(field, "PAST_TIMESTAMP", field+" must be in the future")
	}
	return t.UTC(), nil
}

// ValidateOneOf returns the input if it's in the allowed set, else a stable
// error code derived from the field name. Used for enum-style inputs like
// duration, payment timing, status.
func ValidateOneOf[T comparable](field string, value T, allowed []T) (T, error) {
	for _, a := range allowed {
		if a == value {
			return value, nil
		}
	}
	var zero T
	return zero, badField(field, "INVALID_VALUE", fmt.Sprintf("%s must be one of %v", field, allowed))
}

// ValidateLen bounds a string by character (rune) count. Trims first because
// trailing whitespace is the most common reason a "max 200" field accidentally
// passes 201.
func ValidateLen(field, raw string, minLen, maxLen int) (string, error) {
	v := strings.TrimSpace(raw)
	n := len([]rune(v))
	if n < minLen {
		return "", badField(field, "TOO_SHORT", fmt.Sprintf("%s must be at least %d characters", field, minLen))
	}
	if n > maxLen {
		return "", badField(field, "TOO_LONG", fmt.Sprintf("%s must be ≤ %d characters", field, maxLen))
	}
	return v, nil
}

// ValidateIntRange — bounds an int inclusive. Convenience to keep handlers
// declarative.
func ValidateIntRange(field string, v, minV, maxV int) (int, error) {
	if v < minV || v > maxV {
		return 0, badField(field, "OUT_OF_RANGE", fmt.Sprintf("%s must be between %d and %d", field, minV, maxV))
	}
	return v, nil
}
