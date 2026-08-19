package httpx

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vaishnavghenge/vartalaap-server/internal/auth"
	"github.com/vaishnavghenge/vartalaap-server/internal/store"
)

// memStore is an in-memory implementation of store.Storer for tests.
type memStore struct {
	mu            sync.Mutex
	users         map[string]*store.User                 // key: id
	byEmail       map[string]string                      // email -> id
	tokens        map[string]*store.RefreshToken         // key: tokenHash
	avail         map[string][]store.AvailabilityRule    // hostID -> rules
	events        map[string]*store.EventType            // key: event id
	bookings      map[string]*store.Booking              // key: booking id
	holds         map[string]*store.SlotHold             // key: hold token
	calConns      map[string]*store.CalendarConnection   // key: userID|provider
	calEvents     map[string]*store.BookingCalendarEvent // key: bookingID|provider
	nextID        int
	nextAvailID   int
	nextEventID   int
	nextBookingID int
	nextHoldID    int
	nextCalID     int
}

func newMemStore() *memStore {
	return &memStore{
		users:     make(map[string]*store.User),
		byEmail:   make(map[string]string),
		tokens:    make(map[string]*store.RefreshToken),
		avail:     make(map[string][]store.AvailabilityRule),
		events:    make(map[string]*store.EventType),
		bookings:  make(map[string]*store.Booking),
		holds:     make(map[string]*store.SlotHold),
		calConns:  make(map[string]*store.CalendarConnection),
		calEvents: make(map[string]*store.BookingCalendarEvent),
	}
}

func (m *memStore) id() string {
	m.nextID++
	return fmt.Sprintf("user-%d", m.nextID)
}

func (m *memStore) CreateUser(_ context.Context, email, name, slug, passwordHash string) (*store.User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.byEmail[email]; exists {
		return nil, store.ErrConflict
	}
	u := &store.User{
		ID:           fmt.Sprintf("user-%d", m.nextID+1),
		Email:        email,
		Name:         name,
		Slug:         slug,
		PasswordHash: passwordHash,
		// Mirror the DB default — `plan text not null default 'free'`. Without
		// this the plan-gating handler tests would see empty-string and skip
		// every branch.
		Plan:      "free",
		CreatedAt: time.Now(),
	}
	m.nextID++
	m.users[u.ID] = u
	m.byEmail[email] = u.ID
	return u, nil
}

func (m *memStore) GetUserByEmail(_ context.Context, email string) (*store.User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id, ok := m.byEmail[email]
	if !ok {
		return nil, store.ErrNotFound
	}
	return m.users[id], nil
}

func (m *memStore) GetUserByID(_ context.Context, id string) (*store.User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	u, ok := m.users[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return u, nil
}

func (m *memStore) GetUserBySlug(_ context.Context, slug string) (*store.User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, u := range m.users {
		if u.Slug == slug {
			copy := *u
			return &copy, nil
		}
	}
	return nil, store.ErrNotFound
}

func (m *memStore) SlugExists(_ context.Context, slug string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, u := range m.users {
		if u.Slug == slug {
			return true, nil
		}
	}
	return false, nil
}

func (m *memStore) UpdateProfile(_ context.Context, userID, name, slug, timezone string, onboardingStep int, avatarURL *string) (*store.User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	u, ok := m.users[userID]
	if !ok {
		return nil, store.ErrNotFound
	}
	// Check slug uniqueness (skip if unchanged)
	if slug != u.Slug {
		for _, existing := range m.users {
			if existing.Slug == slug {
				return nil, store.ErrConflict
			}
		}
	}
	u.Name = name
	u.Slug = slug
	u.Timezone = timezone
	u.OnboardingStep = onboardingStep
	u.AvatarURL = avatarURL
	return u, nil
}

func (m *memStore) CreateRefreshToken(_ context.Context, userID, tokenHash string, expiresAt time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tokens[tokenHash] = &store.RefreshToken{
		ID:        tokenHash,
		UserID:    userID,
		TokenHash: tokenHash,
		ExpiresAt: expiresAt,
	}
	return nil
}

func (m *memStore) GetRefreshToken(_ context.Context, tokenHash string) (*store.RefreshToken, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rt, ok := m.tokens[tokenHash]
	if !ok {
		return nil, store.ErrNotFound
	}
	return rt, nil
}

func (m *memStore) DeleteRefreshToken(_ context.Context, tokenHash string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.tokens, tokenHash)
	return nil
}

// Availability methods — persisted in-memory so me_handler_test.go can
// round-trip without a real DB. Rules are returned sorted by day_of_week then
// start_time to mirror the production query.
func (m *memStore) ListAvailability(_ context.Context, hostID string) ([]store.AvailabilityRule, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	src := m.avail[hostID]
	out := make([]store.AvailabilityRule, len(src))
	copy(out, src)
	sort.Slice(out, func(i, j int) bool {
		if out[i].DayOfWeek != out[j].DayOfWeek {
			return out[i].DayOfWeek < out[j].DayOfWeek
		}
		return out[i].StartTime < out[j].StartTime
	})
	return out, nil
}

func (m *memStore) ReplaceAvailability(_ context.Context, hostID string, rules []store.AvailabilityRule) ([]store.AvailabilityRule, error) {
	m.mu.Lock()
	saved := make([]store.AvailabilityRule, 0, len(rules))
	for _, r := range rules {
		m.nextAvailID++
		r.ID = fmt.Sprintf("avail-%d", m.nextAvailID)
		r.HostID = hostID
		saved = append(saved, r)
	}
	m.avail[hostID] = saved
	m.mu.Unlock()
	return m.ListAvailability(context.Background(), hostID)
}

// Event types — straight in-memory mirror of the Postgres-backed store. Tests
// that care about CHECK constraints should run against the real DB
// (store_test.go); these stubs are for handler-layer behaviour.
func (m *memStore) ListEventTypes(_ context.Context, hostID string, activeOnly bool) ([]store.EventType, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]store.EventType, 0)
	for _, e := range m.events {
		if e.HostID != hostID {
			continue
		}
		if activeOnly && !e.IsActive {
			continue
		}
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out, nil
}

func (m *memStore) GetEventTypeBySlug(_ context.Context, hostID, slug string) (*store.EventType, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range m.events {
		if e.HostID == hostID && e.Slug == slug {
			copy := *e
			return &copy, nil
		}
	}
	return nil, store.ErrNotFound
}

func (m *memStore) GetEventType(_ context.Context, hostID, id string) (*store.EventType, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.events[id]
	if !ok || e.HostID != hostID {
		return nil, store.ErrNotFound
	}
	copy := *e
	return &copy, nil
}

func (m *memStore) CreateEventType(_ context.Context, e store.EventType) (*store.EventType, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Enforce unique (host_id, slug) so the conflict path is reachable in tests.
	for _, existing := range m.events {
		if existing.HostID == e.HostID && existing.Slug == e.Slug {
			return nil, store.ErrConflict
		}
	}
	m.nextEventID++
	e.ID = fmt.Sprintf("event-%d", m.nextEventID)
	e.CreatedAt = time.Now()
	clone := e
	m.events[e.ID] = &clone
	out := clone
	return &out, nil
}

func (m *memStore) UpdateEventType(_ context.Context, e store.EventType) (*store.EventType, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.events[e.ID]
	if !ok || cur.HostID != e.HostID {
		return nil, store.ErrNotFound
	}
	for _, other := range m.events {
		if other.ID == e.ID {
			continue
		}
		if other.HostID == e.HostID && other.Slug == e.Slug {
			return nil, store.ErrConflict
		}
	}
	// Preserve CreatedAt (Update doesn't move it in SQL).
	e.CreatedAt = cur.CreatedAt
	clone := e
	m.events[e.ID] = &clone
	out := clone
	return &out, nil
}

func (m *memStore) DeleteEventType(_ context.Context, hostID, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.events[id]
	if !ok || e.HostID != hostID {
		return store.ErrNotFound
	}
	e.IsActive = false
	return nil
}

func (m *memStore) CountActiveEventTypes(_ context.Context, hostID string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, e := range m.events {
		if e.HostID == hostID && e.IsActive {
			n++
		}
	}
	return n, nil
}

// Bookings — straightforward in-memory mirror. Meet-code uniqueness is
// enforced so the handler's collision-retry path is reachable in tests.
func (m *memStore) CreateBooking(_ context.Context, b store.Booking) (*store.Booking, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, existing := range m.bookings {
		if existing.MeetCode == b.MeetCode {
			return nil, store.ErrConflict
		}
	}
	m.nextBookingID++
	b.ID = fmt.Sprintf("booking-%d", m.nextBookingID)
	b.CreatedAt = time.Now()
	// Mirror the SQL default: bookings(cancel_token) fills in a random value
	// when the caller doesn't supply one. Without this the double diverges
	// from production and the guest cancel link is untestable end to end,
	// because handleGuestCancelBooking rejects an empty token.
	if b.CancelToken == "" {
		b.CancelToken = fmt.Sprintf("cancel-%s", b.ID)
	}
	clone := b
	m.bookings[b.ID] = &clone
	out := clone
	return &out, nil
}

func (m *memStore) GetBookingByID(_ context.Context, id string) (*store.Booking, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.bookings[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	copy := *b
	return &copy, nil
}

func (m *memStore) GetBookingByMeetCode(_ context.Context, meetCode string) (*store.Booking, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, b := range m.bookings {
		if b.MeetCode == meetCode {
			copy := *b
			return &copy, nil
		}
	}
	return nil, store.ErrNotFound
}

// Mirrors store.Store.ListBookingsForHost: cancelled bookings are NOT filtered
// out — the host dashboard surfaces them on a "Cancelled" tab so hosts can see
// who cancelled and why. See the SQL implementation in internal/store/store.go.
func (m *memStore) ListBookingsForHost(_ context.Context, hostID string, fromUTC time.Time, limit int) ([]store.Booking, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if limit <= 0 {
		limit = 50
	}
	out := make([]store.Booking, 0)
	for _, b := range m.bookings {
		if b.HostID != hostID {
			continue
		}
		if b.StartsAt.Before(fromUTC) {
			continue
		}
		out = append(out, *b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartsAt.Before(out[j].StartsAt) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (m *memStore) ListBookingsForEventInRange(_ context.Context, eventTypeID string, fromUTC, toUTC time.Time) ([]store.Booking, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]store.Booking, 0)
	for _, b := range m.bookings {
		if b.EventTypeID != eventTypeID || b.Status == "cancelled" {
			continue
		}
		// Half-open interval overlap.
		if b.StartsAt.Before(toUTC) && b.EndsAt.After(fromUTC) {
			out = append(out, *b)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartsAt.Before(out[j].StartsAt) })
	return out, nil
}

func (m *memStore) CancelBooking(_ context.Context, id, reason, cancelledBy string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.bookings[id]
	if !ok || b.Status == "cancelled" {
		return store.ErrNotFound
	}
	b.Status = "cancelled"
	b.CancellationReason = &reason
	b.CancelledBy = &cancelledBy
	return nil
}

func (m *memStore) CountBookingsInMonth(_ context.Context, hostID string, year int, month time.Month) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	start := time.Date(year, month, 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 1, 0)
	n := 0
	for _, b := range m.bookings {
		if b.HostID != hostID {
			continue
		}
		if b.Status == "cancelled" {
			continue
		}
		if b.CreatedAt.Before(start) || !b.CreatedAt.Before(end) {
			continue
		}
		n++
	}
	return n, nil
}

func (m *memStore) CreateSlotHold(_ context.Context, h store.SlotHold) (*store.SlotHold, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.holds[h.Token]; exists {
		return nil, store.ErrConflict
	}
	m.nextHoldID++
	stored := h
	stored.ID = fmt.Sprintf("hold-%d", m.nextHoldID)
	stored.CreatedAt = time.Now().UTC()
	m.holds[h.Token] = &stored
	return &stored, nil
}

func (m *memStore) GetSlotHoldByToken(_ context.Context, token string) (*store.SlotHold, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	h, ok := m.holds[token]
	if !ok {
		return nil, store.ErrNotFound
	}
	cp := *h
	return &cp, nil
}

func (m *memStore) DeleteSlotHold(_ context.Context, token string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.holds, token)
	return nil
}

func (m *memStore) ListActiveHoldsForHostInRange(_ context.Context, hostID string, fromUTC, toUTC time.Time) ([]store.SlotHold, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now().UTC()
	var out []store.SlotHold
	for _, h := range m.holds {
		if h.HostID != hostID {
			continue
		}
		if !h.ExpiresAt.After(now) {
			continue
		}
		// Half-open overlap [from, to) ∩ [starts, ends).
		if !h.StartsAt.Before(toUTC) || !h.EndsAt.After(fromUTC) {
			continue
		}
		cp := *h
		out = append(out, cp)
	}
	return out, nil
}

// --- calendar sync (Phase 3) ---

func calKey(a, b string) string {
	if b == "" {
		b = "google"
	}
	return a + "|" + b
}

func (m *memStore) UpsertCalendarConnection(_ context.Context, c store.CalendarConnection) (*store.CalendarConnection, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c.Provider == "" {
		c.Provider = "google"
	}
	if c.CalendarID == "" {
		c.CalendarID = "primary"
	}
	stored := c
	if existing, ok := m.calConns[calKey(c.UserID, c.Provider)]; ok {
		stored.ID = existing.ID
		stored.CreatedAt = existing.CreatedAt
	} else {
		m.nextCalID++
		stored.ID = fmt.Sprintf("cal-%d", m.nextCalID)
		stored.CreatedAt = time.Now().UTC()
	}
	// Reconnecting clears the revoked tombstone — mirrors the SQL upsert.
	stored.RevokedAt = nil
	stored.LastError = nil
	stored.UpdatedAt = time.Now().UTC()
	m.calConns[calKey(c.UserID, c.Provider)] = &stored
	cp := stored
	return &cp, nil
}

func (m *memStore) GetCalendarConnection(_ context.Context, userID, provider string) (*store.CalendarConnection, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.calConns[calKey(userID, provider)]
	if !ok {
		return nil, store.ErrNotFound
	}
	cp := *c
	return &cp, nil
}

func (m *memStore) findCalConnLocked(id string) *store.CalendarConnection {
	for _, c := range m.calConns {
		if c.ID == id {
			return c
		}
	}
	return nil
}

func (m *memStore) UpdateCalendarAccessToken(_ context.Context, id, encAccessToken string, expiresAt time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := m.findCalConnLocked(id)
	if c == nil {
		return store.ErrNotFound
	}
	c.AccessToken = encAccessToken
	exp := expiresAt
	c.ExpiresAt = &exp
	c.UpdatedAt = time.Now().UTC()
	return nil
}

func (m *memStore) MarkCalendarRevoked(_ context.Context, id, reason string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := m.findCalConnLocked(id)
	if c == nil {
		return store.ErrNotFound
	}
	if c.RevokedAt == nil {
		now := time.Now().UTC()
		c.RevokedAt = &now
	}
	r := reason
	c.LastError = &r
	return nil
}

func (m *memStore) RecordCalendarSync(_ context.Context, id string, syncErr *string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := m.findCalConnLocked(id)
	if c == nil {
		return store.ErrNotFound
	}
	now := time.Now().UTC()
	c.LastSyncedAt = &now
	c.LastError = syncErr
	return nil
}

func (m *memStore) DeleteCalendarConnection(_ context.Context, userID, provider string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.calConns, calKey(userID, provider))
	return nil
}

func (m *memStore) CreateBookingCalendarEvent(_ context.Context, e store.BookingCalendarEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e.Provider == "" {
		e.Provider = "google"
	}
	if e.CalendarID == "" {
		e.CalendarID = "primary"
	}
	e.CreatedAt = time.Now().UTC()
	stored := e
	m.calEvents[calKey(e.BookingID, e.Provider)] = &stored
	return nil
}

func (m *memStore) GetBookingCalendarEvent(_ context.Context, bookingID, provider string) (*store.BookingCalendarEvent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.calEvents[calKey(bookingID, provider)]
	if !ok {
		return nil, store.ErrNotFound
	}
	cp := *e
	return &cp, nil
}

func (m *memStore) DeleteBookingCalendarEvent(_ context.Context, bookingID, provider string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.calEvents, calKey(bookingID, provider))
	return nil
}

// --- test helpers ---

const (
	testOrigin = "https://app.example"
	testSecret = "test-jwt-secret-32-bytes-long!!!"
)

func testCfg() AuthConfig {
	return AuthConfig{
		AllowedOrigins: []string{testOrigin},
		JWTSecret:      testSecret,
		AccessTokenTTL: time.Minute,
		SecureCookie:   false,
	}
}

func authReq(method, path, body string, cookies ...*http.Cookie) *http.Request {
	var bodyReader *strings.Reader
	if body != "" {
		bodyReader = strings.NewReader(body)
	} else {
		bodyReader = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, bodyReader)
	req.Header.Set("Origin", testOrigin)
	req.Header.Set("Content-Type", "application/json")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	return req
}

func cookieFromResponse(rec *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// --- register ---

func TestRegisterHappyPath(t *testing.T) {
	st := newMemStore()
	h := handleRegister(st, testCfg())

	req := authReq(http.MethodPost, "/auth/register",
		`{"name":"Alice Smith","email":"alice@example.com","password":"password123"}`)
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp tokenResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.AccessToken == "" {
		t.Fatal("expected non-empty access token")
	}
	if resp.User.Email != "alice@example.com" {
		t.Fatalf("expected alice@example.com, got %q", resp.User.Email)
	}
	if resp.User.Slug != "alice-smith" {
		t.Fatalf("expected slug %q, got %q", "alice-smith", resp.User.Slug)
	}
	if c := cookieFromResponse(rec, refreshCookieName); c == nil {
		t.Fatal("expected refresh cookie to be set")
	}
	if c := cookieFromResponse(rec, authSessionCookieName); c == nil {
		t.Fatal("expected auth session marker cookie to be set")
	}
}

func TestRegisterDuplicateEmail(t *testing.T) {
	st := newMemStore()
	h := handleRegister(st, testCfg())

	body := `{"name":"Alice","email":"alice@example.com","password":"password123"}`
	h(httptest.NewRecorder(), authReq(http.MethodPost, "/", body))

	rec := httptest.NewRecorder()
	h(rec, authReq(http.MethodPost, "/", body))

	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestRegisterPasswordTooShort(t *testing.T) {
	st := newMemStore()
	h := handleRegister(st, testCfg())

	rec := httptest.NewRecorder()
	h(rec, authReq(http.MethodPost, "/", `{"name":"Bob","email":"bob@example.com","password":"short"}`))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestRegisterSlugDeduplication(t *testing.T) {
	st := newMemStore()
	h := handleRegister(st, testCfg())

	h(httptest.NewRecorder(), authReq(http.MethodPost, "/",
		`{"name":"Alice Smith","email":"alice1@example.com","password":"password123"}`))

	rec := httptest.NewRecorder()
	h(rec, authReq(http.MethodPost, "/",
		`{"name":"Alice Smith","email":"alice2@example.com","password":"password123"}`))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp tokenResponse
	json.NewDecoder(rec.Body).Decode(&resp)
	if resp.User.Slug == "alice-smith" {
		t.Fatalf("expected de-duplicated slug, got %q", resp.User.Slug)
	}
}

// --- login ---

func registerUser(t *testing.T, st *memStore, email, password string) *http.Cookie {
	t.Helper()
	h := handleRegister(st, testCfg())
	body := `{"name":"Test User","email":"` + email + `","password":"` + password + `"}`
	rec := httptest.NewRecorder()
	h(rec, authReq(http.MethodPost, "/", body))
	if rec.Code != http.StatusOK {
		t.Fatalf("setup register failed: %d %s", rec.Code, rec.Body.String())
	}
	return cookieFromResponse(rec, refreshCookieName)
}

func TestLoginHappyPath(t *testing.T) {
	st := newMemStore()
	registerUser(t, st, "alice@example.com", "password123")

	h := handleLogin(st, testCfg())
	rec := httptest.NewRecorder()
	h(rec, authReq(http.MethodPost, "/",
		`{"email":"alice@example.com","password":"password123"}`))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp tokenResponse
	json.NewDecoder(rec.Body).Decode(&resp)
	if resp.AccessToken == "" {
		t.Fatal("expected access token")
	}
}

func TestLoginWrongPassword(t *testing.T) {
	st := newMemStore()
	registerUser(t, st, "alice@example.com", "password123")

	h := handleLogin(st, testCfg())
	rec := httptest.NewRecorder()
	h(rec, authReq(http.MethodPost, "/",
		`{"email":"alice@example.com","password":"wrongpassword"}`))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestLoginUnknownEmail(t *testing.T) {
	h := handleLogin(newMemStore(), testCfg())
	rec := httptest.NewRecorder()
	h(rec, authReq(http.MethodPost, "/",
		`{"email":"nobody@example.com","password":"password123"}`))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

// --- refresh ---

func TestRefreshHappyPath(t *testing.T) {
	st := newMemStore()
	rtCookie := registerUser(t, st, "alice@example.com", "password123")

	h := handleRefresh(st, testCfg())
	rec := httptest.NewRecorder()
	h(rec, authReq(http.MethodPost, "/", "", rtCookie))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp tokenResponse
	json.NewDecoder(rec.Body).Decode(&resp)
	if resp.AccessToken == "" {
		t.Fatal("expected new access token")
	}
	if newCookie := cookieFromResponse(rec, refreshCookieName); newCookie == nil {
		t.Fatal("expected new refresh cookie")
	}
	if c := cookieFromResponse(rec, authSessionCookieName); c == nil {
		t.Fatal("expected auth session marker cookie")
	}
}

func TestRefreshRotatesToken(t *testing.T) {
	st := newMemStore()
	rt1 := registerUser(t, st, "alice@example.com", "password123")

	h := handleRefresh(st, testCfg())
	rec := httptest.NewRecorder()
	h(rec, authReq(http.MethodPost, "/", "", rt1))

	rt2 := cookieFromResponse(rec, refreshCookieName)
	if rt2 == nil {
		t.Fatal("expected new refresh cookie")
	}
	if rt2.Value == rt1.Value {
		t.Fatal("expected rotated (different) refresh token")
	}

	// Old token must be rejected.
	rec2 := httptest.NewRecorder()
	h(rec2, authReq(http.MethodPost, "/", "", rt1))
	if rec2.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for reused token, got %d", rec2.Code)
	}
}

func TestRefreshNoCookie(t *testing.T) {
	h := handleRefresh(newMemStore(), testCfg())
	rec := httptest.NewRecorder()
	h(rec, authReq(http.MethodPost, "/", ""))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

// --- logout ---

func TestLogoutClearsCookie(t *testing.T) {
	st := newMemStore()
	rt := registerUser(t, st, "alice@example.com", "password123")

	h := handleLogout(st, testCfg())
	rec := httptest.NewRecorder()
	h(rec, authReq(http.MethodPost, "/", "", rt))

	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", rec.Code)
	}
	c := cookieFromResponse(rec, refreshCookieName)
	if c == nil || c.MaxAge >= 0 {
		t.Fatal("expected refresh cookie to be cleared (MaxAge < 0)")
	}
	marker := cookieFromResponse(rec, authSessionCookieName)
	if marker == nil || marker.MaxAge >= 0 {
		t.Fatal("expected auth session marker cookie to be cleared (MaxAge < 0)")
	}
}

func TestRefreshAfterLogout(t *testing.T) {
	st := newMemStore()
	rt := registerUser(t, st, "alice@example.com", "password123")

	handleLogout(st, testCfg())(httptest.NewRecorder(),
		authReq(http.MethodPost, "/", "", rt))

	rec := httptest.NewRecorder()
	handleRefresh(st, testCfg())(rec, authReq(http.MethodPost, "/", "", rt))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 after logout, got %d", rec.Code)
	}
}

// --- /auth/me ---

func tokenForUser(t *testing.T, userID string) string {
	t.Helper()
	tok, err := auth.SignAccessToken(userID, testSecret, time.Minute)
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return tok
}

func TestMeHappyPath(t *testing.T) {
	st := newMemStore()
	registerUser(t, st, "alice@example.com", "password123")

	u, _ := st.GetUserByEmail(context.Background(), "alice@example.com")
	token := tokenForUser(t, u.ID)

	h := RequireAuth(testSecret, handleMe(st))
	req := authReq(http.MethodGet, "/auth/me", "")
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp authUserResponse
	json.NewDecoder(rec.Body).Decode(&resp)
	if resp.Email != "alice@example.com" {
		t.Fatalf("expected alice@example.com, got %q", resp.Email)
	}
}

func TestMeNoToken(t *testing.T) {
	h := RequireAuth(testSecret, handleMe(newMemStore()))
	rec := httptest.NewRecorder()
	h(rec, authReq(http.MethodGet, "/auth/me", ""))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestMeExpiredToken(t *testing.T) {
	tok, _ := auth.SignAccessToken("u1", testSecret, -time.Second)
	h := RequireAuth(testSecret, handleMe(newMemStore()))
	req := authReq(http.MethodGet, "/auth/me", "")
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

// Guest tokens are signed with the same secret but carry a RoomID claim.
// Before the fix, they reached handleMe with claims.UserID = guestID and
// GetUserByID(guestID) returned a 500 (or worse, a stale row). RequireAuth
// must now reject them at the middleware boundary.
func TestMeRejectsGuestToken(t *testing.T) {
	tok, _ := auth.SignGuestToken("guest-abc", "room-1", testSecret, time.Minute)
	h := RequireAuth(testSecret, handleMe(newMemStore()))
	req := authReq(http.MethodGet, "/auth/me", "")
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for guest token, got %d: %s", rec.Code, rec.Body.String())
	}
}

// --- slug generation ---

func TestUniqueSlugSpecialChars(t *testing.T) {
	cases := []struct {
		name string
		want string
	}{
		{"Alice Smith", "alice-smith"},
		{"  Spaced  ", "spaced"},
		{"José García", "josé-garcía"},
		{"123 Numbers", "123-numbers"},
		{"---", "user"},
	}
	for _, tc := range cases {
		slug, err := uniqueSlug(context.Background(), newMemStore(), tc.name)
		if err != nil {
			t.Errorf("name=%q: %v", tc.name, err)
			continue
		}
		if slug != tc.want {
			t.Errorf("name=%q: expected %q, got %q", tc.name, tc.want, slug)
		}
	}
}
