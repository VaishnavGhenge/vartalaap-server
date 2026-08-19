// Package gcal is a thin Google Calendar + OAuth client covering exactly the
// five calls Sessionly makes: authorise, exchange, refresh, free/busy, and
// event insert/delete.
//
// Hand-rolled rather than google.golang.org/api on purpose. That SDK pulls in
// gRPC and most of the Google API surface for what amounts to five JSON
// endpoints, and production runs on a 1 GB droplet (see CLAUDE.md). The house
// style for external services is already a small net/http client — see
// internal/cfrealtime.
package gcal

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	defaultAuthURL   = "https://accounts.google.com/o/oauth2/v2/auth"
	defaultTokenURL  = "https://oauth2.googleapis.com/token"
	defaultRevokeURL = "https://oauth2.googleapis.com/revoke"
	defaultAPIBase   = "https://www.googleapis.com/calendar/v3"

	// Scopes are deliberately minimal. `calendar.freebusy` reads busy windows
	// without exposing event titles or attendees, and `calendar.events` is
	// scoped to events rather than whole-calendar management. A host granting
	// access is not handing over the contents of their calendar.
	scopes = "openid email " +
		"https://www.googleapis.com/auth/calendar.freebusy " +
		"https://www.googleapis.com/auth/calendar.events"

	// eventIDPrefix makes inserted event IDs deterministic per booking, which
	// is what makes insert idempotent — see InsertEvent. Google requires event
	// IDs to be base32hex (0-9, a-v), so the prefix is constrained to that
	// alphabet.
	eventIDPrefix = "ses"
)

// ErrInvalidGrant means the refresh token is dead: the host revoked access in
// their Google account, changed password, or the grant expired. It is
// permanent, so callers must stop retrying and ask the host to reconnect
// rather than backing off and trying again forever.
var ErrInvalidGrant = errors.New("gcal: invalid_grant (reconnect required)")

// APIError carries a non-retryable HTTP failure with its body, so the caller
// can log something specific enough to debug from.
type APIError struct {
	Op         string
	StatusCode int
	Body       string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("gcal %s: http %d: %s", e.Op, e.StatusCode, truncate(e.Body, 400))
}

// Retryable reports whether the failure might succeed on a second attempt.
// 429 and 5xx are transient; 4xx is our bug or the host's revoked grant.
func (e *APIError) Retryable() bool {
	return e.StatusCode == http.StatusTooManyRequests || e.StatusCode >= 500
}

type Client struct {
	clientID     string
	clientSecret string
	redirectURI  string
	http         *http.Client

	authURL   string
	tokenURL  string
	revokeURL string
	apiBase   string
}

func New(clientID, clientSecret, redirectURI string) *Client {
	return &Client{
		clientID:     clientID,
		clientSecret: clientSecret,
		redirectURI:  redirectURI,
		// Per-request contexts carry the real deadline; this is the backstop
		// for a connection that never establishes.
		http:      &http.Client{Timeout: 15 * time.Second},
		authURL:   defaultAuthURL,
		tokenURL:  defaultTokenURL,
		revokeURL: defaultRevokeURL,
		apiBase:   defaultAPIBase,
	}
}

// NewWithBase points every endpoint at a test server. Tests only.
func NewWithBase(clientID, clientSecret, redirectURI, base string) *Client {
	c := New(clientID, clientSecret, redirectURI)
	c.authURL = base + "/auth"
	c.tokenURL = base + "/token"
	c.revokeURL = base + "/revoke"
	c.apiBase = base + "/calendar/v3"
	return c
}

// Tokens is the credential set for one connected calendar. RefreshToken is
// empty on a refresh response — Google only issues it on first consent, so
// callers must keep the one they already hold.
type Tokens struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    time.Time
	AccountEmail string
}

// AuthCodeURL builds the consent URL the host's browser is sent to.
//
// access_type=offline is what makes Google issue a refresh token, and
// prompt=consent forces one even on a re-authorisation where the host has
// already granted these scopes (without it, a reconnect returns an access
// token with no refresh token and the connection dies in an hour).
func (c *Client) AuthCodeURL(state string) string {
	q := url.Values{
		"client_id":     {c.clientID},
		"redirect_uri":  {c.redirectURI},
		"response_type": {"code"},
		"scope":         {scopes},
		"access_type":   {"offline"},
		"prompt":        {"consent"},
		"state":         {state},
		// Ask Google to include granted scopes in the callback so a host who
		// unticks calendar access is detectable at connect time, not at the
		// first free/busy call.
		"include_granted_scopes": {"true"},
	}
	return c.authURL + "?" + q.Encode()
}

type tokenResponse struct {
	AccessToken      string `json:"access_token"`
	RefreshToken     string `json:"refresh_token"`
	ExpiresIn        int    `json:"expires_in"`
	IDToken          string `json:"id_token"`
	Scope            string `json:"scope"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

// Exchange trades an authorisation code for tokens.
func (c *Client) Exchange(ctx context.Context, code string) (Tokens, error) {
	return c.tokenRequest(ctx, "exchange", url.Values{
		"code":          {code},
		"client_id":     {c.clientID},
		"client_secret": {c.clientSecret},
		"redirect_uri":  {c.redirectURI},
		"grant_type":    {"authorization_code"},
	})
}

// Refresh mints a new access token. Returns ErrInvalidGrant when the refresh
// token is permanently dead.
func (c *Client) Refresh(ctx context.Context, refreshToken string) (Tokens, error) {
	return c.tokenRequest(ctx, "refresh", url.Values{
		"refresh_token": {refreshToken},
		"client_id":     {c.clientID},
		"client_secret": {c.clientSecret},
		"grant_type":    {"refresh_token"},
	})
}

func (c *Client) tokenRequest(ctx context.Context, op string, form url.Values) (Tokens, error) {
	var out Tokens
	err := c.retry(ctx, func(attemptCtx context.Context) error {
		req, err := http.NewRequestWithContext(attemptCtx, http.MethodPost, c.tokenURL,
			strings.NewReader(form.Encode()))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		resp, err := c.http.Do(req)
		if err != nil {
			return err
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

		var tr tokenResponse
		// Google returns the error as JSON with a 4xx; decode before checking
		// status so invalid_grant is classified even on a malformed status.
		_ = json.Unmarshal(body, &tr)
		if tr.Error == "invalid_grant" {
			return ErrInvalidGrant
		}
		if resp.StatusCode != http.StatusOK {
			return &APIError{Op: op, StatusCode: resp.StatusCode, Body: string(body)}
		}
		if tr.AccessToken == "" {
			return &APIError{Op: op, StatusCode: resp.StatusCode, Body: "no access_token in response"}
		}
		out = Tokens{
			AccessToken:  tr.AccessToken,
			RefreshToken: tr.RefreshToken,
			// Shave 60s off so a token that expires mid-flight is refreshed
			// before use rather than failing one call first.
			ExpiresAt:    time.Now().UTC().Add(time.Duration(tr.ExpiresIn)*time.Second - time.Minute),
			AccountEmail: emailFromIDToken(tr.IDToken),
		}
		return nil
	})
	return out, err
}

// Revoke tells Google to drop the grant. Best-effort: a failure here leaves a
// live grant on Google's side that the host can remove manually, so the caller
// should still delete its local copy.
func (c *Client) Revoke(ctx context.Context, token string) error {
	form := url.Values{"token": {token}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.revokeURL,
		strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	// 400 means the token was already invalid — the desired end state.
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusBadRequest {
		return &APIError{Op: "revoke", StatusCode: resp.StatusCode, Body: string(body)}
	}
	return nil
}

// Interval is a half-open busy window [Start, End) in UTC.
type Interval struct {
	Start time.Time
	End   time.Time
}

type freeBusyRequest struct {
	TimeMin string             `json:"timeMin"`
	TimeMax string             `json:"timeMax"`
	Items   []freeBusyCalendar `json:"items"`
}

type freeBusyCalendar struct {
	ID string `json:"id"`
}

type freeBusyResponse struct {
	Calendars map[string]struct {
		Busy []struct {
			Start time.Time `json:"start"`
			End   time.Time `json:"end"`
		} `json:"busy"`
		Errors []struct {
			Domain string `json:"domain"`
			Reason string `json:"reason"`
		} `json:"errors"`
	} `json:"calendars"`
}

// FreeBusy returns the host's busy windows in [from, to). Per-calendar errors
// (calendar deleted, access lost) are surfaced as a real error rather than an
// empty busy list, because "no busy periods" and "we could not read the
// calendar" must not look the same to the slot generator: one means the day is
// free, the other means we do not know.
func (c *Client) FreeBusy(ctx context.Context, accessToken, calendarID string, from, to time.Time) ([]Interval, error) {
	if calendarID == "" {
		calendarID = "primary"
	}
	body, err := json.Marshal(freeBusyRequest{
		TimeMin: from.UTC().Format(time.RFC3339),
		TimeMax: to.UTC().Format(time.RFC3339),
		Items:   []freeBusyCalendar{{ID: calendarID}},
	})
	if err != nil {
		return nil, err
	}
	var out []Interval
	err = c.retry(ctx, func(attemptCtx context.Context) error {
		raw, err := c.doAPI(attemptCtx, "freebusy", http.MethodPost, "/freeBusy", accessToken, body)
		if err != nil {
			return err
		}
		var fb freeBusyResponse
		if err := json.Unmarshal(raw, &fb); err != nil {
			return fmt.Errorf("gcal freebusy: decode: %w", err)
		}
		cal, ok := fb.Calendars[calendarID]
		if !ok {
			return &APIError{Op: "freebusy", StatusCode: http.StatusOK,
				Body: "calendar " + calendarID + " missing from response"}
		}
		if len(cal.Errors) > 0 {
			return &APIError{Op: "freebusy", StatusCode: http.StatusOK,
				Body: "calendar error: " + cal.Errors[0].Reason}
		}
		out = make([]Interval, 0, len(cal.Busy))
		for _, b := range cal.Busy {
			out = append(out, Interval{Start: b.Start.UTC(), End: b.End.UTC()})
		}
		return nil
	})
	return out, err
}

// Event is the subset of a Google Calendar event Sessionly writes.
type Event struct {
	// BookingID makes the remote event ID deterministic, which is what makes
	// InsertEvent safe to retry.
	BookingID   string
	Summary     string
	Description string
	Location    string
	Start       time.Time
	End         time.Time
	TimeZone    string
	GuestEmail  string
	GuestName   string
}

type gcalEventTime struct {
	DateTime string `json:"dateTime"`
	TimeZone string `json:"timeZone,omitempty"`
}

type gcalAttendee struct {
	Email       string `json:"email"`
	DisplayName string `json:"displayName,omitempty"`
}

type gcalEvent struct {
	ID          string         `json:"id,omitempty"`
	Summary     string         `json:"summary"`
	Description string         `json:"description,omitempty"`
	Location    string         `json:"location,omitempty"`
	Start       gcalEventTime  `json:"start"`
	End         gcalEventTime  `json:"end"`
	Attendees   []gcalAttendee `json:"attendees,omitempty"`
	Source      *struct {
		Title string `json:"title"`
		URL   string `json:"url"`
	} `json:"source,omitempty"`
}

// EventID is the deterministic Google event ID for a booking.
func EventID(bookingID string) string {
	return eventIDPrefix + strings.ToLower(strings.ReplaceAll(bookingID, "-", ""))
}

// InsertEvent writes the booking into the host's calendar and returns the
// remote event ID.
//
// Idempotent by construction: the event ID is derived from the booking ID, so
// a retry after a lost response hits Google's duplicate check (409) instead of
// creating a second event. We treat that 409 as success, because the only way
// to get it is that our own earlier attempt landed.
//
// sendUpdates=none because Sessionly already emails both parties. Letting
// Google send its own invitation would mean two mails per booking saying the
// same thing.
func (c *Client) InsertEvent(ctx context.Context, accessToken, calendarID string, ev Event) (string, error) {
	if calendarID == "" {
		calendarID = "primary"
	}
	tz := ev.TimeZone
	if tz == "" {
		tz = "UTC"
	}
	payload := gcalEvent{
		ID:          EventID(ev.BookingID),
		Summary:     ev.Summary,
		Description: ev.Description,
		Location:    ev.Location,
		Start:       gcalEventTime{DateTime: ev.Start.UTC().Format(time.RFC3339), TimeZone: tz},
		End:         gcalEventTime{DateTime: ev.End.UTC().Format(time.RFC3339), TimeZone: tz},
	}
	if ev.GuestEmail != "" {
		payload.Attendees = []gcalAttendee{{Email: ev.GuestEmail, DisplayName: ev.GuestName}}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	path := "/calendars/" + url.PathEscape(calendarID) + "/events?sendUpdates=none"
	eventID := payload.ID
	err = c.retry(ctx, func(attemptCtx context.Context) error {
		_, err := c.doAPI(attemptCtx, "events.insert", http.MethodPost, path, accessToken, body)
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusConflict {
			// Our own prior attempt succeeded. Nothing left to do.
			return nil
		}
		return err
	})
	if err != nil {
		return "", err
	}
	return eventID, nil
}

// DeleteEvent removes a mirrored event. 404 and 410 count as success: the
// event is gone, which is the outcome the caller wanted.
func (c *Client) DeleteEvent(ctx context.Context, accessToken, calendarID, eventID string) error {
	if calendarID == "" {
		calendarID = "primary"
	}
	path := "/calendars/" + url.PathEscape(calendarID) + "/events/" + url.PathEscape(eventID) + "?sendUpdates=none"
	return c.retry(ctx, func(attemptCtx context.Context) error {
		_, err := c.doAPI(attemptCtx, "events.delete", http.MethodDelete, path, accessToken, nil)
		var apiErr *APIError
		if errors.As(err, &apiErr) &&
			(apiErr.StatusCode == http.StatusNotFound || apiErr.StatusCode == http.StatusGone) {
			return nil
		}
		return err
	})
}

func (c *Client) doAPI(ctx context.Context, op, method, path, accessToken string, body []byte) ([]byte, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.apiBase+path, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, &APIError{Op: op, StatusCode: resp.StatusCode, Body: string(raw)}
	}
	return raw, nil
}

// retryAttempts and retryBase define the backoff schedule: ~200ms, ~400ms
// between three attempts, each with up to 50% jitter so a Google blip doesn't
// turn every server into a synchronised retry stampede.
const (
	retryAttempts = 3
	retryBase     = 200 * time.Millisecond
	// perAttemptTimeout bounds a single attempt so one slow call can't consume
	// the caller's whole budget and leave no room for the retry.
	perAttemptTimeout = 6 * time.Second
)

func (c *Client) retry(ctx context.Context, fn func(context.Context) error) error {
	var lastErr error
	for attempt := 0; attempt < retryAttempts; attempt++ {
		if attempt > 0 {
			delay := retryBase * time.Duration(1<<(attempt-1))
			jitter := time.Duration(rand.Int63n(int64(delay) / 2)) //nolint:gosec // jitter, not a secret
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay + jitter):
			}
		}
		attemptCtx, cancel := context.WithTimeout(ctx, perAttemptTimeout)
		err := fn(attemptCtx)
		cancel()
		if err == nil {
			return nil
		}
		lastErr = err

		// A dead grant will never recover; retrying wastes the caller's
		// deadline and hammers Google with a request we know will fail.
		if errors.Is(err, ErrInvalidGrant) {
			return err
		}
		var apiErr *APIError
		if errors.As(err, &apiErr) && !apiErr.Retryable() {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	return lastErr
}

// emailFromIDToken pulls the `email` claim out of Google's id_token without
// verifying the signature. Safe here and only here: the token arrived in the
// body of a TLS response from Google's own token endpoint, so there is no
// untrusted party in the path. It is used for display ("connected as x@y")
// and never for authorisation.
func emailFromIDToken(idToken string) string {
	parts := strings.Split(idToken, ".")
	if len(parts) != 3 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Email string `json:"email"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return ""
	}
	return claims.Email
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
