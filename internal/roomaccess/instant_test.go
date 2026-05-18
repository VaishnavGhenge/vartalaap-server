package roomaccess

import (
	"errors"
	"testing"
	"time"
)

func TestInstantRegistryActivatesOnFirstJoin(t *testing.T) {
	r := NewInstantRegistry()
	now := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
	r.Create("abc-defg-hij", now, time.Hour)

	if err := r.AllowActive("abc-defg-hij", now, 30*time.Minute); !errors.Is(err, ErrNotStarted) {
		t.Fatalf("before join: want ErrNotStarted, got %v", err)
	}
	if err := r.ActivateOrAllow("abc-defg-hij", now.Add(time.Minute), 30*time.Minute); err != nil {
		t.Fatalf("activate: %v", err)
	}
	if err := r.AllowActive("abc-defg-hij", now.Add(2*time.Minute), 30*time.Minute); err != nil {
		t.Fatalf("active room should allow infrastructure: %v", err)
	}
}

func TestInstantRegistryExpiresAfterEmptyGrace(t *testing.T) {
	r := NewInstantRegistry()
	now := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
	r.Create("abc-defg-hij", now, time.Hour)
	if err := r.ActivateOrAllow("abc-defg-hij", now, 30*time.Minute); err != nil {
		t.Fatalf("activate: %v", err)
	}
	r.MarkEmpty("abc-defg-hij", now.Add(5*time.Minute))

	if err := r.AllowActive("abc-defg-hij", now.Add(20*time.Minute), 30*time.Minute); err != nil {
		t.Fatalf("within empty grace should remain active: %v", err)
	}
	if err := r.AllowActive("abc-defg-hij", now.Add(36*time.Minute), 30*time.Minute); !errors.Is(err, ErrExpired) {
		t.Fatalf("after empty grace: want ErrExpired, got %v", err)
	}
}
