package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrNotFound = errors.New("not found")
var ErrConflict = errors.New("conflict")

// Storer is the interface the auth handlers depend on.
// *Store satisfies it; tests can provide an in-memory double.
type Storer interface {
	CreateUser(ctx context.Context, email, name, slug, passwordHash string) (*User, error)
	GetUserByEmail(ctx context.Context, email string) (*User, error)
	GetUserByID(ctx context.Context, id string) (*User, error)
	SlugExists(ctx context.Context, slug string) (bool, error)
	UpdateProfile(ctx context.Context, userID, name, slug, timezone string, onboardingStep int) (*User, error)
	CreateRefreshToken(ctx context.Context, userID, tokenHash string, expiresAt time.Time) error
	GetRefreshToken(ctx context.Context, tokenHash string) (*RefreshToken, error)
	DeleteRefreshToken(ctx context.Context, tokenHash string) error
}

type User struct {
	ID             string
	Email          string
	Name           string
	Slug           string
	Timezone       string
	OnboardingStep int
	AvatarURL      *string
	PasswordHash   string
	CreatedAt      time.Time
}

type RefreshToken struct {
	ID        string
	UserID    string
	TokenHash string
	ExpiresAt time.Time
}

type Store struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

const userCols = `id, email, name, slug, timezone, onboarding_step, avatar_url, password_hash, created_at`

func scanUser(row pgx.Row, u *User) error {
	return row.Scan(&u.ID, &u.Email, &u.Name, &u.Slug, &u.Timezone, &u.OnboardingStep, &u.AvatarURL, &u.PasswordHash, &u.CreatedAt)
}

func (s *Store) CreateUser(ctx context.Context, email, name, slug, passwordHash string) (*User, error) {
	u := &User{}
	err := scanUser(s.pool.QueryRow(ctx,
		`INSERT INTO users (email, name, slug, password_hash)
		 VALUES ($1, $2, $3, $4)
		 RETURNING `+userCols,
		email, name, slug, passwordHash,
	), u)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrConflict
		}
		return nil, fmt.Errorf("store: create user: %w", err)
	}
	return u, nil
}

func (s *Store) GetUserByEmail(ctx context.Context, email string) (*User, error) {
	u := &User{}
	err := scanUser(s.pool.QueryRow(ctx,
		`SELECT `+userCols+` FROM users WHERE email = $1`, email,
	), u)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("store: get user by email: %w", err)
	}
	return u, nil
}

func (s *Store) GetUserByID(ctx context.Context, id string) (*User, error) {
	u := &User{}
	err := scanUser(s.pool.QueryRow(ctx,
		`SELECT `+userCols+` FROM users WHERE id = $1`, id,
	), u)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("store: get user by id: %w", err)
	}
	return u, nil
}

func (s *Store) UpdateProfile(ctx context.Context, userID, name, slug, timezone string, onboardingStep int) (*User, error) {
	u := &User{}
	err := scanUser(s.pool.QueryRow(ctx,
		`UPDATE users SET name=$2, slug=$3, timezone=$4, onboarding_step=$5
		 WHERE id=$1
		 RETURNING `+userCols,
		userID, name, slug, timezone, onboardingStep,
	), u)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		if isUniqueViolation(err) {
			return nil, ErrConflict
		}
		return nil, fmt.Errorf("store: update profile: %w", err)
	}
	return u, nil
}

func (s *Store) SlugExists(ctx context.Context, slug string) (bool, error) {
	var exists bool
	err := s.pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM users WHERE slug = $1)`, slug,
	).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("store: slug exists: %w", err)
	}
	return exists, nil
}

func (s *Store) CreateRefreshToken(ctx context.Context, userID, tokenHash string, expiresAt time.Time) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO refresh_tokens (user_id, token_hash, expires_at) VALUES ($1, $2, $3)`,
		userID, tokenHash, expiresAt,
	)
	if err != nil {
		return fmt.Errorf("store: create refresh token: %w", err)
	}
	return nil
}

func (s *Store) GetRefreshToken(ctx context.Context, tokenHash string) (*RefreshToken, error) {
	rt := &RefreshToken{}
	err := s.pool.QueryRow(ctx,
		`SELECT id, user_id, token_hash, expires_at
		 FROM refresh_tokens WHERE token_hash = $1`,
		tokenHash,
	).Scan(&rt.ID, &rt.UserID, &rt.TokenHash, &rt.ExpiresAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("store: get refresh token: %w", err)
	}
	return rt, nil
}

func (s *Store) DeleteRefreshToken(ctx context.Context, tokenHash string) error {
	_, err := s.pool.Exec(ctx,
		`DELETE FROM refresh_tokens WHERE token_hash = $1`, tokenHash,
	)
	if err != nil {
		return fmt.Errorf("store: delete refresh token: %w", err)
	}
	return nil
}

func (s *Store) DeleteExpiredRefreshTokens(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM refresh_tokens WHERE expires_at < now()`)
	if err != nil {
		return fmt.Errorf("store: delete expired tokens: %w", err)
	}
	return nil
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
