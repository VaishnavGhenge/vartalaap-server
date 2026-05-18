package httpx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/vaishnavghenge/vartalaap-server/internal/auth"
	"github.com/vaishnavghenge/vartalaap-server/internal/store"
)

const (
	refreshCookieName     = "rt"
	authSessionCookieName = "sessionly_session"
	refreshCookiePath     = "/auth"
	authSessionCookiePath = "/"
	refreshTokenTTL       = 30 * 24 * time.Hour
)

var slugSep = regexp.MustCompile(`-{2,}`)

type AuthConfig struct {
	AllowedOrigins []string
	JWTSecret      string
	AccessTokenTTL time.Duration
	SecureCookie   bool
}

type authUserResponse struct {
	ID             string  `json:"id"`
	Email          string  `json:"email"`
	Name           string  `json:"name"`
	Slug           string  `json:"slug"`
	Timezone       string  `json:"timezone"`
	OnboardingStep int     `json:"onboardingStep"`
	AvatarURL      *string `json:"avatarUrl,omitempty"`
	Plan           string  `json:"plan"`
}

type tokenResponse struct {
	AccessToken string           `json:"accessToken"`
	User        authUserResponse `json:"user"`
}

func toUserResponse(u *store.User) authUserResponse {
	return authUserResponse{
		ID:             u.ID,
		Email:          u.Email,
		Name:           u.Name,
		Slug:           u.Slug,
		Timezone:       u.Timezone,
		OnboardingStep: u.OnboardingStep,
		AvatarURL:      u.AvatarURL,
		Plan:           u.Plan,
	}
}

// AuthHandlers wires all /auth/* routes onto mux.
func AuthHandlers(mux *http.ServeMux, st store.Storer, cfg AuthConfig) {
	lim := NewRateLimiter(10, 20)

	mux.HandleFunc("/auth/register", authRoute(cfg, http.MethodPost, lim, handleRegister(st, cfg)))
	mux.HandleFunc("/auth/login", authRoute(cfg, http.MethodPost, lim, handleLogin(st, cfg)))
	mux.HandleFunc("/auth/refresh", authRoute(cfg, http.MethodPost, nil, handleRefresh(st, cfg)))
	mux.HandleFunc("/auth/logout", authRoute(cfg, http.MethodPost, nil, handleLogout(st, cfg)))
	mux.HandleFunc("/auth/me", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodOptions:
			// Preflight: advertise both methods so PATCH requests are not blocked.
			authRoute(cfg, http.MethodGet+", "+http.MethodPatch, nil,
				func(w http.ResponseWriter, r *http.Request) {})(w, r)
		case http.MethodGet:
			authRoute(cfg, http.MethodGet, nil, RequireAuth(cfg.JWTSecret, handleMe(st)))(w, r)
		case http.MethodPatch:
			authRoute(cfg, http.MethodPatch, nil, RequireAuth(cfg.JWTSecret, handleUpdateMe(st)))(w, r)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
}

func authRoute(cfg AuthConfig, method string, lim *RateLimiter, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !enforceAPIRequest(w, r, cfg.AllowedOrigins, method, lim) {
			return
		}
		next(w, r)
	}
}

func handleRegister(st store.Storer, cfg AuthConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Name     string `json:"name"`
			Email    string `json:"email"`
			Password string `json:"password"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteError(w, http.StatusBadRequest, "INVALID_JSON", "invalid request")
			return
		}
		body.Name = strings.TrimSpace(body.Name)
		body.Email = strings.ToLower(strings.TrimSpace(body.Email))

		if body.Name == "" || body.Email == "" {
			http.Error(w, "name and email are required", http.StatusBadRequest)
			return
		}
		if len(body.Password) < 8 {
			http.Error(w, "password must be at least 8 characters", http.StatusBadRequest)
			return
		}

		slug, err := uniqueSlug(r.Context(), st, body.Name)
		if err != nil {
			slog.Error("register: slug", "err", err)
			http.Error(w, "could not create account", http.StatusInternalServerError)
			return
		}

		hash, err := auth.HashPassword(body.Password)
		if err != nil {
			slog.Error("register: hash", "err", err)
			http.Error(w, "could not create account", http.StatusInternalServerError)
			return
		}

		u, err := st.CreateUser(r.Context(), body.Email, body.Name, slug, hash)
		if err != nil {
			if errors.Is(err, store.ErrConflict) {
				http.Error(w, "email already in use", http.StatusConflict)
				return
			}
			slog.Error("register: create user", "err", err)
			http.Error(w, "could not create account", http.StatusInternalServerError)
			return
		}

		writeTokens(w, r, st, cfg, u)
	}
}

func handleLogin(st store.Storer, cfg AuthConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Email    string `json:"email"`
			Password string `json:"password"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		body.Email = strings.ToLower(strings.TrimSpace(body.Email))

		u, err := st.GetUserByEmail(r.Context(), body.Email)
		// Unified error — don't leak whether the email exists.
		if err != nil || !auth.CheckPassword(body.Password, u.PasswordHash) {
			http.Error(w, "invalid email or password", http.StatusUnauthorized)
			return
		}

		writeTokens(w, r, st, cfg, u)
	}
}

func handleRefresh(st store.Storer, cfg AuthConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(refreshCookieName)
		if err != nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		hash := auth.HashRefreshToken(cookie.Value)
		rt, err := st.GetRefreshToken(r.Context(), hash)
		if err != nil || time.Now().After(rt.ExpiresAt) {
			clearRefreshCookie(w, cfg.SecureCookie)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		// Rotate: delete old before issuing new (prevents replay).
		if err := st.DeleteRefreshToken(r.Context(), hash); err != nil {
			slog.Error("refresh: delete", "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		u, err := st.GetUserByID(r.Context(), rt.UserID)
		if err != nil {
			clearRefreshCookie(w, cfg.SecureCookie)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		writeTokens(w, r, st, cfg, u)
	}
}

func handleLogout(st store.Storer, cfg AuthConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if cookie, err := r.Cookie(refreshCookieName); err == nil {
			_ = st.DeleteRefreshToken(r.Context(), auth.HashRefreshToken(cookie.Value))
		}
		clearRefreshCookie(w, cfg.SecureCookie)
		w.WriteHeader(http.StatusNoContent)
	}
}

func handleMe(st store.Storer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, _ := auth.UserIDFromContext(r.Context())
		u, err := st.GetUserByID(r.Context(), userID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(toUserResponse(u))
	}
}

var slugRe = regexp.MustCompile(`^[a-z0-9-]{3,30}$`)

func handleUpdateMe(st store.Storer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, _ := auth.UserIDFromContext(r.Context())

		var body struct {
			Name           string  `json:"name"`
			Slug           string  `json:"slug"`
			Timezone       string  `json:"timezone"`
			OnboardingStep int     `json:"onboardingStep"`
			AvatarURL      *string `json:"avatarUrl"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		body.Name = strings.TrimSpace(body.Name)
		body.Slug = strings.TrimSpace(body.Slug)
		if body.Name == "" {
			WriteFieldError(w, http.StatusBadRequest, badField("name", "REQUIRED", "Name is required."))
			return
		}
		if body.Slug != "" && !slugRe.MatchString(body.Slug) {
			WriteFieldError(w, http.StatusBadRequest, badField("slug", "INVALID_SLUG", "Use 3-30 lowercase letters, numbers, or hyphens."))
			return
		}
		if body.Timezone == "" {
			body.Timezone = "UTC"
		}

		u, err := st.UpdateProfile(r.Context(), userID, body.Name, body.Slug, body.Timezone, body.OnboardingStep, body.AvatarURL)
		if err != nil {
			if errors.Is(err, store.ErrConflict) {
				WriteFieldError(w, http.StatusConflict, badField("slug", "SLUG_TAKEN", "This booking URL is already taken."))
				return
			}
			if errors.Is(err, store.ErrNotFound) {
				WriteError(w, http.StatusNotFound, "NOT_FOUND", "not found")
				return
			}
			slog.Error("auth: update profile", "err", err)
			WriteError(w, http.StatusInternalServerError, "INTERNAL", "internal error")
			return
		}
		WriteJSON(w, http.StatusOK, toUserResponse(u))
	}
}

func writeTokens(w http.ResponseWriter, r *http.Request, st store.Storer, cfg AuthConfig, u *store.User) {
	accessToken, err := auth.SignAccessToken(u.ID, cfg.JWTSecret, cfg.AccessTokenTTL)
	if err != nil {
		slog.Error("auth: sign access token", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	rawRT, hashRT, err := auth.NewRefreshToken()
	if err != nil {
		slog.Error("auth: new refresh token", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	if err := st.CreateRefreshToken(r.Context(), u.ID, hashRT, time.Now().Add(refreshTokenTTL)); err != nil {
		slog.Error("auth: store refresh token", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     refreshCookieName,
		Value:    rawRT,
		Path:     refreshCookiePath,
		HttpOnly: true,
		Secure:   cfg.SecureCookie,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int(refreshTokenTTL.Seconds()),
	})
	http.SetCookie(w, &http.Cookie{
		Name:     authSessionCookieName,
		Value:    "1",
		Path:     authSessionCookiePath,
		Secure:   cfg.SecureCookie,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int(refreshTokenTTL.Seconds()),
	})

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(tokenResponse{AccessToken: accessToken, User: toUserResponse(u)})
}

func clearRefreshCookie(w http.ResponseWriter, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     refreshCookieName,
		Value:    "",
		Path:     refreshCookiePath,
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   -1,
	})
	http.SetCookie(w, &http.Cookie{
		Name:     authSessionCookieName,
		Value:    "",
		Path:     authSessionCookiePath,
		Secure:   secure,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   -1,
	})
}

// uniqueSlug builds a URL-safe slug from name and ensures uniqueness in the DB.
func uniqueSlug(ctx context.Context, st store.Storer, name string) (string, error) {
	base := strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return unicode.ToLower(r)
		}
		return '-'
	}, name)
	base = slugSep.ReplaceAllString(base, "-")
	base = strings.Trim(base, "-")
	if base == "" {
		base = "user"
	}

	slug := base
	for i := 2; i <= 20; i++ {
		exists, err := st.SlugExists(ctx, slug)
		if err != nil {
			return "", err
		}
		if !exists {
			return slug, nil
		}
		slug = fmt.Sprintf("%s-%d", base, i)
	}
	return "", fmt.Errorf("could not find unique slug for %q", name)
}
