package httpx

import (
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/vaishnavghenge/vartalaap-server/internal/auth"
	"github.com/vaishnavghenge/vartalaap-server/internal/calendar"
)

// CalendarHandlers wires the Google Calendar connection routes.
//
//	GET    /me/calendar/status            → connection state for the dashboard
//	GET    /me/calendar/connect/google    → returns the Google consent URL
//	GET    /me/calendar/callback/google   → OAuth callback (browser navigation)
//	DELETE /me/calendar/disconnect        → revoke at Google + forget locally
//
// Connect returns a URL as JSON rather than issuing a 302 itself. The client
// calls it with its Bearer token and then navigates, which keeps the access
// token out of a redirect chain that passes through Google.
//
// The callback is the one route here without RequireAuth, and deliberately so:
// it arrives as a top-level navigation from Google carrying no Authorization
// header. Identity comes from the signed, purpose-scoped `state` token instead
// (see calendar.Service.AuthURL), which is why forging one is not a way in.
func CalendarHandlers(mux *http.ServeMux, cfg AuthConfig, svc calendar.Connector) {
	lim := NewRateLimiter(30, 60)
	// The callback is unauthenticated and hit once per connection attempt. A
	// tighter bucket keeps state-guessing attempts from being free.
	callbackLim := NewRateLimiter(10, 20)

	mux.HandleFunc("/me/calendar/", func(w http.ResponseWriter, r *http.Request) {
		action := strings.TrimPrefix(r.URL.Path, "/me/calendar/")
		switch action {
		case "status":
			switch r.Method {
			case http.MethodOptions:
				meRoute(cfg, http.MethodGet, nil, func(http.ResponseWriter, *http.Request) {})(w, r)
			case http.MethodGet:
				meRoute(cfg, http.MethodGet, lim,
					RequireAuth(cfg.JWTSecret, handleCalendarStatus(svc)))(w, r)
			default:
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			}
		case "connect/google":
			switch r.Method {
			case http.MethodOptions:
				meRoute(cfg, http.MethodGet, nil, func(http.ResponseWriter, *http.Request) {})(w, r)
			case http.MethodGet:
				meRoute(cfg, http.MethodGet, lim,
					RequireAuth(cfg.JWTSecret, handleCalendarConnect(svc)))(w, r)
			default:
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			}
		case "callback/google":
			if r.Method != http.MethodGet {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			if !callbackLim.Allow(r.URL.Path + "|" + clientIP(r)) {
				http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
				return
			}
			handleCalendarCallback(svc)(w, r)
		case "disconnect":
			switch r.Method {
			case http.MethodOptions:
				meRoute(cfg, http.MethodDelete, nil, func(http.ResponseWriter, *http.Request) {})(w, r)
			case http.MethodDelete:
				meRoute(cfg, http.MethodDelete, lim,
					RequireAuth(cfg.JWTSecret, handleCalendarDisconnect(svc)))(w, r)
			default:
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			}
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	})
}

type calendarStatusResponse struct {
	Provider string `json:"provider"`
	// Connected is true only when the grant is live. A revoked connection
	// reports connected=false with needsReconnect=true so the dashboard shows
	// "reconnect", not "connect" — the host needs to know sync stopped.
	Connected      bool       `json:"connected"`
	NeedsReconnect bool       `json:"needsReconnect"`
	AccountEmail   string     `json:"accountEmail,omitempty"`
	CalendarID     string     `json:"calendarId,omitempty"`
	LastSyncedAt   *time.Time `json:"lastSyncedAt,omitempty"`
	LastError      string     `json:"lastError,omitempty"`
	// Available is false when the server has no Google credentials configured,
	// so the UI can hide the whole section instead of offering a button that
	// cannot work.
	Available bool `json:"available"`
}

type calendarConnectResponse struct {
	AuthURL string `json:"authUrl"`
}

func handleCalendarStatus(svc calendar.Connector) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := auth.UserIDFromContext(r.Context())
		if !ok {
			WriteError(w, http.StatusUnauthorized, "UNAUTHORIZED", "unauthorized")
			return
		}
		if svc == nil {
			WriteJSON(w, http.StatusOK, calendarStatusResponse{Provider: "google"})
			return
		}
		st, err := svc.Status(r.Context(), userID)
		if err != nil {
			slog.Error("calendar: status", "err", err, "user_id", userID)
			WriteError(w, http.StatusInternalServerError, "INTERNAL", "could not load calendar status")
			return
		}
		WriteJSON(w, http.StatusOK, calendarStatusResponse{
			Provider:       "google",
			Connected:      st.Connected,
			NeedsReconnect: st.NeedsReconnect,
			AccountEmail:   st.AccountEmail,
			CalendarID:     st.CalendarID,
			LastSyncedAt:   st.LastSyncedAt,
			LastError:      st.LastError,
			Available:      true,
		})
	}
}

func handleCalendarConnect(svc calendar.Connector) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := auth.UserIDFromContext(r.Context())
		if !ok {
			WriteError(w, http.StatusUnauthorized, "UNAUTHORIZED", "unauthorized")
			return
		}
		if svc == nil {
			WriteError(w, http.StatusServiceUnavailable, "CALENDAR_UNAVAILABLE",
				"calendar sync is not configured on this server")
			return
		}
		// `return` names where the callback should land the browser. The
		// service maps it through a closed allowlist, so an unknown or hostile
		// value degrades to the dashboard rather than becoming an open redirect.
		authURL, err := svc.AuthURL(userID, r.URL.Query().Get("return"))
		if err != nil {
			slog.Error("calendar: build auth url", "err", err, "user_id", userID)
			WriteError(w, http.StatusInternalServerError, "INTERNAL", "could not start calendar connection")
			return
		}
		WriteJSON(w, http.StatusOK, calendarConnectResponse{AuthURL: authURL})
	}
}

// handleCalendarCallback finishes the OAuth dance and bounces the browser back
// to the dashboard.
//
// Every outcome is a redirect with a fixed, enumerated reason code. Google's
// own error strings are logged but never reflected into the redirect URL —
// echoing a third party's text into a URL the browser then renders is how you
// get an injection bug in the dashboard.
func handleCalendarCallback(svc calendar.Connector) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if svc == nil {
			http.Error(w, "calendar sync is not configured", http.StatusServiceUnavailable)
			return
		}
		q := r.URL.Query()
		if gerr := q.Get("error"); gerr != "" {
			// The host pressed Cancel on the consent screen, or Google refused.
			// Google echoes `state` back even on denial, so the wizard still
			// resumes where the host left it.
			slog.Info("calendar: consent declined", "google_error", gerr)
			redirectCalendar(w, r, svc.RedirectTo(svc.ReturnPath(q.Get("state"))), "denied")
			return
		}
		code := q.Get("code")
		state := q.Get("state")
		if code == "" || state == "" {
			redirectCalendar(w, r, svc.RedirectTo(""), "invalid_callback")
			return
		}
		userID, returnTo, err := svc.Complete(r.Context(), state, code)
		if err != nil {
			slog.Warn("calendar: oauth completion failed", "err", err, "user_id", userID)
			redirectCalendar(w, r, svc.RedirectTo(returnTo), "connect_failed")
			return
		}
		redirectCalendar(w, r, svc.RedirectTo(returnTo), "connected")
	}
}

// redirectCalendar sends the browser to `base` with ?calendar=<reason>.
// Falls back to a plain text response when no redirect target is configured,
// so a misconfigured deployment shows the host something rather than a blank
// page with a 302 to nowhere.
func redirectCalendar(w http.ResponseWriter, r *http.Request, base, reason string) {
	if base == "" {
		WriteJSON(w, http.StatusOK, map[string]string{"calendar": reason})
		return
	}
	u, err := url.Parse(base)
	if err != nil {
		WriteJSON(w, http.StatusOK, map[string]string{"calendar": reason})
		return
	}
	q := u.Query()
	q.Set("calendar", reason)
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

func handleCalendarDisconnect(svc calendar.Connector) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := auth.UserIDFromContext(r.Context())
		if !ok {
			WriteError(w, http.StatusUnauthorized, "UNAUTHORIZED", "unauthorized")
			return
		}
		if svc == nil {
			// Nothing could have been connected, so the host's desired end
			// state already holds.
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if err := svc.Disconnect(r.Context(), userID); err != nil {
			if errors.Is(err, calendar.ErrNotConnected) {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			slog.Error("calendar: disconnect", "err", err, "user_id", userID)
			WriteError(w, http.StatusInternalServerError, "INTERNAL", "could not disconnect calendar")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}
