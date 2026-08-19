package store_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
	"github.com/vaishnavghenge/vartalaap-server/internal/db"
	"github.com/vaishnavghenge/vartalaap-server/internal/store"
)

// sharedStore is created once in TestMain and reused across all tests.
// Each test calls newStore(t) which returns sharedStore — fast, no per-test container.
var sharedStore *store.Store

func TestMain(m *testing.M) {
	ctx := context.Background()

	ctr, err := tcpostgres.Run(ctx,
		"postgres:17-alpine",
		tcpostgres.WithDatabase("testdb"),
		tcpostgres.WithUsername("test"),
		tcpostgres.WithPassword("test"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(30*time.Second),
		),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "start postgres container: %v\n", err)
		os.Exit(1)
	}
	defer ctr.Terminate(ctx) //nolint:errcheck

	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		fmt.Fprintf(os.Stderr, "connection string: %v\n", err)
		os.Exit(1)
	}

	pool, err := db.Open(ctx, dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "db.Open: %v\n", err)
		os.Exit(1)
	}
	defer pool.Close()

	sharedStore = store.New(pool)
	os.Exit(m.Run())
}

// newStore returns the shared store. Each test must use unique emails/slugs
// since all tests share one DB — no truncation between tests.
func newStore(t *testing.T) *store.Store {
	t.Helper()
	return sharedStore
}

// unique returns a value that won't collide across parallel tests.
func unique(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}

// --- CreateUser / GetUserByEmail / GetUserByID ---

func TestCreateAndGetUser(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	email := unique("alice") + "@example.com"
	slug := unique("alice-smith")

	u, err := st.CreateUser(ctx, email, "Alice Smith", slug, "hashed")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if u.ID == "" {
		t.Fatal("expected non-empty ID")
	}
	if u.Slug != slug {
		t.Fatalf("expected slug %q, got %q", slug, u.Slug)
	}

	byEmail, err := st.GetUserByEmail(ctx, email)
	if err != nil {
		t.Fatalf("GetUserByEmail: %v", err)
	}
	if byEmail.ID != u.ID {
		t.Fatalf("GetUserByEmail: ID mismatch")
	}

	byID, err := st.GetUserByID(ctx, u.ID)
	if err != nil {
		t.Fatalf("GetUserByID: %v", err)
	}
	if byID.Email != email {
		t.Fatalf("GetUserByID: email mismatch")
	}
}

func TestCreateUserDuplicateEmail(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	email := unique("dup") + "@example.com"

	st.CreateUser(ctx, email, "Alice", unique("slug-a"), "h")
	_, err := st.CreateUser(ctx, email, "Alice2", unique("slug-b"), "h")
	if err != store.ErrConflict {
		t.Fatalf("expected ErrConflict, got %v", err)
	}
}

func TestCreateUserDuplicateSlug(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	slug := unique("shared-slug")

	st.CreateUser(ctx, unique("u1")+"@example.com", "Alice", slug, "h")
	_, err := st.CreateUser(ctx, unique("u2")+"@example.com", "Bob", slug, "h")
	if err != store.ErrConflict {
		t.Fatalf("expected ErrConflict for duplicate slug, got %v", err)
	}
}

func TestGetUserByEmailNotFound(t *testing.T) {
	_, err := newStore(t).GetUserByEmail(context.Background(), "nobody-"+unique("")+"@example.com")
	if err != store.ErrNotFound {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestGetUserByIDNotFound(t *testing.T) {
	_, err := newStore(t).GetUserByID(context.Background(), "00000000-0000-0000-0000-000000000000")
	if err != store.ErrNotFound {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

// --- SlugExists ---

func TestSlugExists(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	slug := unique("slug-exists-test")

	exists, err := st.SlugExists(ctx, slug)
	if err != nil {
		t.Fatalf("SlugExists before create: %v", err)
	}
	if exists {
		t.Fatal("expected slug to not exist before creation")
	}

	st.CreateUser(ctx, unique("slug-test")+"@example.com", "Alice", slug, "h")

	exists, err = st.SlugExists(ctx, slug)
	if err != nil {
		t.Fatalf("SlugExists after create: %v", err)
	}
	if !exists {
		t.Fatal("expected slug to exist after creation")
	}
}

// --- Refresh tokens ---

func TestCreateAndGetRefreshToken(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	u, _ := st.CreateUser(ctx, unique("rt")+"@example.com", "Alice", unique("rt-slug"), "h")
	hash := unique("token-hash")
	expiresAt := time.Now().Add(24 * time.Hour)

	if err := st.CreateRefreshToken(ctx, u.ID, hash, expiresAt); err != nil {
		t.Fatalf("CreateRefreshToken: %v", err)
	}

	rt, err := st.GetRefreshToken(ctx, hash)
	if err != nil {
		t.Fatalf("GetRefreshToken: %v", err)
	}
	if rt.UserID != u.ID {
		t.Fatalf("expected userID %q, got %q", u.ID, rt.UserID)
	}
	if rt.ExpiresAt.Before(time.Now()) {
		t.Fatal("expected future expiry")
	}
}

func TestGetRefreshTokenNotFound(t *testing.T) {
	_, err := newStore(t).GetRefreshToken(context.Background(), unique("nonexistent"))
	if err != store.ErrNotFound {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestDeleteRefreshToken(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	u, _ := st.CreateUser(ctx, unique("del-rt")+"@example.com", "Alice", unique("del-rt-slug"), "h")
	hash := unique("hash-to-delete")
	st.CreateRefreshToken(ctx, u.ID, hash, time.Now().Add(time.Hour))

	if err := st.DeleteRefreshToken(ctx, hash); err != nil {
		t.Fatalf("DeleteRefreshToken: %v", err)
	}

	_, err := st.GetRefreshToken(ctx, hash)
	if err != store.ErrNotFound {
		t.Fatalf("expected ErrNotFound after delete, got %v", err)
	}
}

func TestDeleteExpiredRefreshTokens(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	u, _ := st.CreateUser(ctx, unique("exp-rt")+"@example.com", "Alice", unique("exp-rt-slug"), "h")
	expiredHash := unique("expired-hash")
	validHash := unique("valid-hash")

	st.CreateRefreshToken(ctx, u.ID, expiredHash, time.Now().Add(-time.Hour))
	st.CreateRefreshToken(ctx, u.ID, validHash, time.Now().Add(time.Hour))

	if err := st.DeleteExpiredRefreshTokens(ctx); err != nil {
		t.Fatalf("DeleteExpiredRefreshTokens: %v", err)
	}

	if _, err := st.GetRefreshToken(ctx, expiredHash); err != store.ErrNotFound {
		t.Fatal("expected expired token to be deleted")
	}
	if _, err := st.GetRefreshToken(ctx, validHash); err != nil {
		t.Fatalf("expected valid token to still exist: %v", err)
	}
}

// --- ListAvailability / ReplaceAvailability ---

// availabilityHost creates a fresh user for an availability test and returns
// the user ID. Centralised so each test isn't 5 lines of register noise.
func availabilityHost(t *testing.T, st *store.Store) string {
	t.Helper()
	u, err := st.CreateUser(context.Background(),
		unique("avail")+"@example.com",
		"Avail Host",
		unique("avail-host"),
		"hashed",
	)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	return u.ID
}

func TestListAvailability_Empty(t *testing.T) {
	st := newStore(t)
	hostID := availabilityHost(t, st)

	rules, err := st.ListAvailability(context.Background(), hostID)
	if err != nil {
		t.Fatalf("ListAvailability: %v", err)
	}
	if len(rules) != 0 {
		t.Fatalf("expected 0 rules for new host, got %d", len(rules))
	}
}

func TestReplaceAvailability_InsertsAndOrders(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	hostID := availabilityHost(t, st)

	// Deliberately out of order on insert; ListAvailability must sort by day
	// then start_time so the API returns a stable shape.
	input := []store.AvailabilityRule{
		{DayOfWeek: 3, StartTime: "14:00", EndTime: "17:00", Timezone: "UTC"},
		{DayOfWeek: 1, StartTime: "13:00", EndTime: "17:00", Timezone: "UTC"},
		{DayOfWeek: 1, StartTime: "09:00", EndTime: "12:00", Timezone: "UTC"},
	}
	saved, err := st.ReplaceAvailability(ctx, hostID, input)
	if err != nil {
		t.Fatalf("ReplaceAvailability: %v", err)
	}
	if len(saved) != 3 {
		t.Fatalf("expected 3 rules, got %d", len(saved))
	}
	// Returned set is the ordered ListAvailability shape.
	if saved[0].DayOfWeek != 1 || saved[0].StartTime != "09:00" {
		t.Fatalf("expected first Mon 09:00, got %+v", saved[0])
	}
	if saved[1].DayOfWeek != 1 || saved[1].StartTime != "13:00" {
		t.Fatalf("expected second Mon 13:00, got %+v", saved[1])
	}
	if saved[2].DayOfWeek != 3 {
		t.Fatalf("expected third Wed, got %+v", saved[2])
	}
	for _, r := range saved {
		if r.ID == "" {
			t.Fatalf("expected generated id, got empty for %+v", r)
		}
		if r.HostID != hostID {
			t.Fatalf("expected host_id %q, got %q", hostID, r.HostID)
		}
	}
}

func TestReplaceAvailability_DeletesPriorRules(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	hostID := availabilityHost(t, st)

	// First write — 2 rules.
	_, err := st.ReplaceAvailability(ctx, hostID, []store.AvailabilityRule{
		{DayOfWeek: 0, StartTime: "08:00", EndTime: "09:00", Timezone: "UTC"},
		{DayOfWeek: 1, StartTime: "08:00", EndTime: "09:00", Timezone: "UTC"},
	})
	if err != nil {
		t.Fatalf("first replace: %v", err)
	}

	// Second write — 1 rule on a different day. The two earlier rules must
	// be gone; this is the property the UI relies on to make "edit my week"
	// idempotent.
	saved, err := st.ReplaceAvailability(ctx, hostID, []store.AvailabilityRule{
		{DayOfWeek: 5, StartTime: "15:00", EndTime: "18:00", Timezone: "UTC"},
	})
	if err != nil {
		t.Fatalf("second replace: %v", err)
	}
	if len(saved) != 1 || saved[0].DayOfWeek != 5 {
		t.Fatalf("expected exactly Friday rule, got %+v", saved)
	}
}

func TestReplaceAvailability_EmptySetClearsAll(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	hostID := availabilityHost(t, st)

	if _, err := st.ReplaceAvailability(ctx, hostID, []store.AvailabilityRule{
		{DayOfWeek: 2, StartTime: "10:00", EndTime: "11:00", Timezone: "UTC"},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Passing an empty slice is how the UI says "I am unavailable every day".
	// It must not error and must leave the host with zero rules.
	saved, err := st.ReplaceAvailability(ctx, hostID, []store.AvailabilityRule{})
	if err != nil {
		t.Fatalf("clear replace: %v", err)
	}
	if len(saved) != 0 {
		t.Fatalf("expected 0 rules after empty replace, got %d", len(saved))
	}
}

func TestReplaceAvailability_PerHostIsolation(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	hostA := availabilityHost(t, st)
	hostB := availabilityHost(t, st)

	if _, err := st.ReplaceAvailability(ctx, hostA, []store.AvailabilityRule{
		{DayOfWeek: 1, StartTime: "08:00", EndTime: "09:00", Timezone: "UTC"},
		{DayOfWeek: 2, StartTime: "08:00", EndTime: "09:00", Timezone: "UTC"},
	}); err != nil {
		t.Fatalf("seed A: %v", err)
	}
	if _, err := st.ReplaceAvailability(ctx, hostB, []store.AvailabilityRule{
		{DayOfWeek: 6, StartTime: "20:00", EndTime: "22:00", Timezone: "UTC"},
	}); err != nil {
		t.Fatalf("seed B: %v", err)
	}

	rulesA, _ := st.ListAvailability(ctx, hostA)
	rulesB, _ := st.ListAvailability(ctx, hostB)
	if len(rulesA) != 2 {
		t.Fatalf("host A expected 2 rules, got %d", len(rulesA))
	}
	if len(rulesB) != 1 {
		t.Fatalf("host B expected 1 rule, got %d", len(rulesB))
	}

	// Replacing A's set must not perturb B.
	if _, err := st.ReplaceAvailability(ctx, hostA, []store.AvailabilityRule{}); err != nil {
		t.Fatalf("clear A: %v", err)
	}
	rulesB2, _ := st.ListAvailability(ctx, hostB)
	if len(rulesB2) != 1 {
		t.Fatalf("host B expected to still have 1 rule after A cleared, got %d", len(rulesB2))
	}
}

func TestNewUserDefaultsToFreePlan(t *testing.T) {
	st := newStore(t)
	u, err := st.CreateUser(context.Background(),
		unique("plan")+"@example.com", "Plan User", unique("plan-user"), "h",
	)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if u.Plan != "free" {
		t.Fatalf("expected default plan 'free', got %q", u.Plan)
	}
}
