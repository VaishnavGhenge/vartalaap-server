package store_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go"
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
