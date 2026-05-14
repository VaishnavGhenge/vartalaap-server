package httpx

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vaishnavghenge/vartalaap-server/internal/auth"
	"github.com/vaishnavghenge/vartalaap-server/internal/store"
)

// memStore is an in-memory implementation of store.Storer for tests.
type memStore struct {
	mu     sync.Mutex
	users  map[string]*store.User  // key: id
	byEmail map[string]string       // email -> id
	tokens map[string]*store.RefreshToken // key: tokenHash
	nextID int
}

func newMemStore() *memStore {
	return &memStore{
		users:   make(map[string]*store.User),
		byEmail: make(map[string]string),
		tokens:  make(map[string]*store.RefreshToken),
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
		CreatedAt:    time.Now(),
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
