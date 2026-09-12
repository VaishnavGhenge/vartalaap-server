package httpx

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/vaishnavghenge/vartalaap-server/internal/auth"
	"github.com/vaishnavghenge/vartalaap-server/internal/googleauth"
	"github.com/vaishnavghenge/vartalaap-server/internal/store"
)

const (
	refreshCookieName     = "rt"
	authSessionCookieName = "sessionly_session"
	refreshCookiePath     = "/auth"
	authSessionCookiePath = "/"
	refreshTokenTTL       = 30 * 24 * time.Hour
	googleOAuthCookiePath = "/auth/google"
	googleOAuthTTL        = 10 * time.Minute
)

var slugSep = regexp.MustCompile(`-{2,}`)

type AuthConfig struct {
	AllowedOrigins []string
	JWTSecret      string
	AccessTokenTTL time.Duration
	SecureCookie   bool
	PublicAppURL   string
}

type GoogleAuthProvider interface {
	AuthCodeURL(state, codeChallenge string) string
	Exchange(ctx context.Context, code, codeVerifier string) (googleauth.Identity, error)
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
func AuthHandlers(mux *http.ServeMux, st store.Storer, cfg AuthConfig, googleProviders ...GoogleAuthProvider) {
	lim := NewRateLimiter(10, 20)

	mux.HandleFunc("/auth/register", authRoute(cfg, http.MethodPost, lim, handleRegister(st, cfg)))
	mux.HandleFunc("/auth/login", authRoute(cfg, http.MethodPost, lim, handleLogin(st, cfg)))
	mux.HandleFunc("/auth/refresh", authRoute(cfg, http.MethodPost, nil, handleRefresh(st, cfg)))
	mux.HandleFunc("/auth/logout", authRoute(cfg, http.MethodPost, nil, handleLogout(st, cfg)))
	if len(googleProviders) > 0 && googleProviders[0] != nil {
		googleProvider := googleProviders[0]
		mux.HandleFunc("/auth/google", authRoute(cfg, http.MethodGet, lim, handleGoogleStart(cfg, googleProvider)))
		mux.HandleFunc("/auth/google/callback", handleGoogleCallback(st, cfg, googleProvider, NewRateLimiter(10, 20)))
	}
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

func handleGoogleStart(cfg AuthConfig, provider GoogleAuthProvider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		next := safeAuthReturnPath(r.URL.Query().Get("next"))
		nonce, _, err := auth.NewRefreshToken()
		if err != nil {
			WriteError(w, http.StatusInternalServerError, "INTERNAL", "could not start Google sign-in")
			return
		}
		state, err := auth.SignPurposeTokenWithReturn(nonce, "google-sign-in", next, cfg.JWTSecret, googleOAuthTTL)
		if err != nil {
			WriteError(w, http.StatusInternalServerError, "INTERNAL", "could not start Google sign-in")
			return
		}
		verifier, _, err := auth.NewRefreshToken()
		if err != nil {
			WriteError(w, http.StatusInternalServerError, "INTERNAL", "could not start Google sign-in")
			return
		}
		challengeBytes := sha256.Sum256([]byte(verifier))
		challenge := base64.RawURLEncoding.EncodeToString(challengeBytes[:])
		setGoogleOAuthCookie(w, "sessionly_google_state", state, cfg.SecureCookie)
		setGoogleOAuthCookie(w, "sessionly_google_pkce", verifier, cfg.SecureCookie)
		WriteJSON(w, http.StatusOK, map[string]string{"authUrl": provider.AuthCodeURL(state, challenge)})
	}
}

func handleGoogleCallback(st store.Storer, cfg AuthConfig, provider GoogleAuthProvider, limiter *RateLimiter) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !limiter.Allow(r.URL.Path + "|" + clientIP(r)) {
			http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
			return
		}
		q := r.URL.Query()
		stateCookie, stateErr := r.Cookie("sessionly_google_state")
		pkceCookie, pkceErr := r.Cookie("sessionly_google_pkce")
		clearGoogleOAuthCookies(w, cfg.SecureCookie)
		if stateErr != nil || pkceErr != nil || q.Get("state") == "" ||
			subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(stateCookie.Value)) != 1 {
			redirectGoogleAuth(w, r, cfg.PublicAppURL, "/login", "invalid_state")
			return
		}
		_, next, err := auth.VerifyPurposeTokenWithReturn(q.Get("state"), "google-sign-in", cfg.JWTSecret)
		if err != nil {
			redirectGoogleAuth(w, r, cfg.PublicAppURL, "/login", "invalid_state")
			return
		}
		if q.Get("error") != "" || q.Get("code") == "" {
			redirectGoogleAuth(w, r, cfg.PublicAppURL, "/login", "denied")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		identity, err := provider.Exchange(ctx, q.Get("code"), pkceCookie.Value)
		cancel()
		if err != nil {
			slog.Warn("google sign-in: exchange", "err", err)
			redirectGoogleAuth(w, r, cfg.PublicAppURL, "/login", "failed")
			return
		}
		if !identity.EmailVerified {
			redirectGoogleAuth(w, r, cfg.PublicAppURL, "/login", "failed")
			return
		}
		if u, lookupErr := st.GetUserByOAuthIdentity(r.Context(), "google", identity.Subject); lookupErr == nil {
			finishGoogleSignIn(w, r, st, cfg, u, next)
			return
		} else if !errors.Is(lookupErr, store.ErrNotFound) {
			slog.Error("google sign-in: identity lookup", "err", lookupErr)
			redirectGoogleAuth(w, r, cfg.PublicAppURL, "/login", "failed")
			return
		}
		// Google warns that a verified address outside Gmail or a hosted Google
		// domain may later change owners. Do not use such an address to attach a
		// new identity to an existing password account.
		if !identity.EmailAuthoritative {
			if _, emailErr := st.GetUserByEmail(r.Context(), identity.Email); emailErr == nil {
				redirectGoogleAuth(w, r, cfg.PublicAppURL, "/login", "account_conflict")
				return
			} else if !errors.Is(emailErr, store.ErrNotFound) {
				redirectGoogleAuth(w, r, cfg.PublicAppURL, "/login", "failed")
				return
			}
		}
		name := identity.Name
		if name == "" {
			name = strings.Split(identity.Email, "@")[0]
		}
		slug, err := uniqueSlug(r.Context(), st, name)
		if err != nil {
			redirectGoogleAuth(w, r, cfg.PublicAppURL, "/login", "failed")
			return
		}
		randomPassword, _, err := auth.NewRefreshToken()
		if err != nil {
			redirectGoogleAuth(w, r, cfg.PublicAppURL, "/login", "failed")
			return
		}
		passwordHash, err := auth.HashPassword(randomPassword)
		if err != nil {
			redirectGoogleAuth(w, r, cfg.PublicAppURL, "/login", "failed")
			return
		}
		var avatar *string
		if identity.Picture != "" {
			avatar = &identity.Picture
		}
		u, err := st.CreateOrLinkOAuthUser(r.Context(), "google", identity.Subject, identity.Email, name, slug, passwordHash, avatar)
		if errors.Is(err, store.ErrConflict) {
			// A simultaneous callback may have won the unique identity race.
			u, err = st.GetUserByOAuthIdentity(r.Context(), "google", identity.Subject)
		}
		if err != nil {
			slog.Error("google sign-in: resolve user", "err", err)
			redirectGoogleAuth(w, r, cfg.PublicAppURL, "/login", "account_conflict")
			return
		}
		finishGoogleSignIn(w, r, st, cfg, u, next)
	}
}

func finishGoogleSignIn(w http.ResponseWriter, r *http.Request, st store.Storer, cfg AuthConfig, u *store.User, next string) {
	if _, err := issueTokens(w, r, st, cfg, u); err != nil {
		slog.Error("google sign-in: issue session", "err", err)
		redirectGoogleAuth(w, r, cfg.PublicAppURL, "/login", "failed")
		return
	}
	destination := "/auth/google/callback"
	if next != "" {
		destination += "?next=" + url.QueryEscape(next)
	}
	http.Redirect(w, r, strings.TrimRight(cfg.PublicAppURL, "/")+destination, http.StatusFound)
}

func safeAuthReturnPath(next string) string {
	if next == "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.Contains(next, "\\") {
		return ""
	}
	switch strings.Split(next, "?")[0] {
	case "/login", "/register", "/auth/google/callback":
		return ""
	}
	return next
}

func setGoogleOAuthCookie(w http.ResponseWriter, name, value string, secure bool) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: googleOAuthCookiePath, HttpOnly: true,
		Secure: secure, SameSite: http.SameSiteLaxMode, MaxAge: int(googleOAuthTTL.Seconds())})
}

func clearGoogleOAuthCookies(w http.ResponseWriter, secure bool) {
	for _, name := range []string{"sessionly_google_state", "sessionly_google_pkce"} {
		http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: googleOAuthCookiePath, HttpOnly: true,
			Secure: secure, SameSite: http.SameSiteLaxMode, MaxAge: -1})
	}
}

func redirectGoogleAuth(w http.ResponseWriter, r *http.Request, appURL, path, reason string) {
	destination := strings.TrimRight(appURL, "/") + path
	if reason != "" {
		destination += "?oauth=" + url.QueryEscape(reason)
	}
	http.Redirect(w, r, destination, http.StatusFound)
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
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			slog.Error("refresh: lookup", "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if errors.Is(err, store.ErrNotFound) || time.Now().After(rt.ExpiresAt) {
			// A concurrent refresh may have already replaced this cookie. A
			// rejected request must never delete the winner's fresh cookie.
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		u, err := st.GetUserByID(r.Context(), rt.UserID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
			} else {
				http.Error(w, "internal error", http.StatusInternalServerError)
			}
			return
		}

		writeTokens(w, r, st, cfg, u, hash)
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

func writeTokens(w http.ResponseWriter, r *http.Request, st store.Storer, cfg AuthConfig, u *store.User, oldHash ...string) {
	resp, err := issueTokens(w, r, st, cfg, u, oldHash...)
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if err != nil {
		slog.Error("auth: issue tokens", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(resp)
}

func issueTokens(w http.ResponseWriter, r *http.Request, st store.Storer, cfg AuthConfig, u *store.User, oldHash ...string) (tokenResponse, error) {
	accessToken, err := auth.SignAccessToken(u.ID, cfg.JWTSecret, cfg.AccessTokenTTL)
	if err != nil {
		return tokenResponse{}, fmt.Errorf("sign access token: %w", err)
	}

	rawRT, hashRT, err := auth.NewRefreshToken()
	if err != nil {
		return tokenResponse{}, fmt.Errorf("new refresh token: %w", err)
	}

	if len(oldHash) > 0 {
		err = st.RotateRefreshToken(r.Context(), oldHash[0], hashRT, time.Now().Add(refreshTokenTTL))
	} else {
		err = st.CreateRefreshToken(r.Context(), u.ID, hashRT, time.Now().Add(refreshTokenTTL))
	}
	if errors.Is(err, store.ErrNotFound) {
		return tokenResponse{}, store.ErrNotFound
	}
	if err != nil {
		return tokenResponse{}, fmt.Errorf("store refresh token: %w", err)
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

	return tokenResponse{AccessToken: accessToken, User: toUserResponse(u)}, nil
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
