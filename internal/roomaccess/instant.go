package roomaccess

import (
	"errors"
	"sync"
	"time"
)

var (
	ErrUnknownRoom = errors.New("room not found")
	ErrNotStarted  = errors.New("instant room has not started")
	ErrExpired     = errors.New("instant room expired")
)

type InstantRegistry struct {
	mu    sync.Mutex
	rooms map[string]*instantRoom
}

type instantRoom struct {
	createdAt   time.Time
	activatedAt *time.Time
	emptySince  *time.Time
	expiresAt   time.Time
	closed      bool
}

func NewInstantRegistry() *InstantRegistry {
	return &InstantRegistry{rooms: make(map[string]*instantRoom)}
}

func (r *InstantRegistry) Create(room string, now time.Time, ttl time.Duration) {
	if ttl <= 0 {
		ttl = 12 * time.Hour
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rooms[room] = &instantRoom{
		createdAt: now.UTC(),
		expiresAt: now.UTC().Add(ttl),
	}
}

func (r *InstantRegistry) ActivateOrAllow(room string, now time.Time, emptyGrace time.Duration) error {
	now = now.UTC()
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.rooms[room]
	if !ok || entry.closed {
		return ErrUnknownRoom
	}
	if entry.expired(now, emptyGrace) {
		entry.closed = true
		return ErrExpired
	}
	if entry.activatedAt == nil {
		t := now
		entry.activatedAt = &t
	}
	entry.emptySince = nil
	return nil
}

func (r *InstantRegistry) AllowActive(room string, now time.Time, emptyGrace time.Duration) error {
	now = now.UTC()
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.rooms[room]
	if !ok || entry.closed {
		return ErrUnknownRoom
	}
	if entry.activatedAt == nil {
		return ErrNotStarted
	}
	if entry.expired(now, emptyGrace) {
		entry.closed = true
		return ErrExpired
	}
	return nil
}

func (r *InstantRegistry) MarkEmpty(room string, now time.Time) {
	now = now.UTC()
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.rooms[room]
	if !ok || entry.closed || entry.activatedAt == nil || entry.emptySince != nil {
		return
	}
	entry.emptySince = &now
}

func (r *InstantRegistry) MarkActive(room string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if entry := r.rooms[room]; entry != nil {
		entry.emptySince = nil
	}
}

func (r *instantRoom) expired(now time.Time, emptyGrace time.Duration) bool {
	if now.After(r.expiresAt) {
		return true
	}
	if r.emptySince != nil && emptyGrace >= 0 && now.After(r.emptySince.Add(emptyGrace)) {
		return true
	}
	return false
}
