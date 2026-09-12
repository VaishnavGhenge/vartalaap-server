package googleauth

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	defaultAuthURL     = "https://accounts.google.com/o/oauth2/v2/auth"
	defaultTokenURL    = "https://oauth2.googleapis.com/token"
	defaultUserInfoURL = "https://openidconnect.googleapis.com/v1/userinfo"
	identityScopes     = "openid email profile"
)

type Identity struct {
	Subject            string
	Email              string
	EmailVerified      bool
	EmailAuthoritative bool
	Name               string
	Picture            string
}

type Client struct {
	clientID     string
	clientSecret string
	redirectURI  string
	http         *http.Client
	authURL      string
	tokenURL     string
	userInfoURL  string
}

func New(clientID, clientSecret, redirectURI string) *Client {
	return &Client{
		clientID: clientID, clientSecret: clientSecret, redirectURI: redirectURI,
		http:    &http.Client{Timeout: 15 * time.Second},
		authURL: defaultAuthURL, tokenURL: defaultTokenURL, userInfoURL: defaultUserInfoURL,
	}
}

func NewWithBase(clientID, clientSecret, redirectURI, base string) *Client {
	c := New(clientID, clientSecret, redirectURI)
	c.authURL = base + "/auth"
	c.tokenURL = base + "/token"
	c.userInfoURL = base + "/userinfo"
	return c
}

func (c *Client) AuthCodeURL(state, codeChallenge string) string {
	q := url.Values{
		"client_id":             {c.clientID},
		"redirect_uri":          {c.redirectURI},
		"response_type":         {"code"},
		"scope":                 {identityScopes},
		"state":                 {state},
		"code_challenge":        {codeChallenge},
		"code_challenge_method": {"S256"},
		"prompt":                {"select_account"},
	}
	return c.authURL + "?" + q.Encode()
}

func (c *Client) Exchange(ctx context.Context, code, codeVerifier string) (Identity, error) {
	form := url.Values{
		"code":          {code},
		"client_id":     {c.clientID},
		"client_secret": {c.clientSecret},
		"redirect_uri":  {c.redirectURI},
		"grant_type":    {"authorization_code"},
		"code_verifier": {codeVerifier},
	}
	body, status, err := c.doWithRetry(ctx, func(ctx context.Context) (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.tokenURL, strings.NewReader(form.Encode()))
		if err == nil {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		return req, err
	})
	if err != nil {
		return Identity{}, fmt.Errorf("google auth: exchange: %w", err)
	}
	if status != http.StatusOK {
		return Identity{}, fmt.Errorf("google auth: exchange http %d: %s", status, truncate(body))
	}
	var tokens struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &tokens); err != nil || tokens.AccessToken == "" {
		return Identity{}, fmt.Errorf("google auth: exchange returned no access token")
	}

	infoBody, status, err := c.doWithRetry(ctx, func(ctx context.Context) (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.userInfoURL, nil)
		if err == nil {
			req.Header.Set("Authorization", "Bearer "+tokens.AccessToken)
		}
		return req, err
	})
	if err != nil {
		return Identity{}, fmt.Errorf("google auth: userinfo: %w", err)
	}
	if status != http.StatusOK {
		return Identity{}, fmt.Errorf("google auth: userinfo http %d: %s", status, truncate(infoBody))
	}
	var out struct {
		Subject       string `json:"sub"`
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
		Name          string `json:"name"`
		Picture       string `json:"picture"`
		HostedDomain  string `json:"hd"`
	}
	if err := json.Unmarshal(infoBody, &out); err != nil {
		return Identity{}, fmt.Errorf("google auth: userinfo decode: %w", err)
	}
	if out.Subject == "" || out.Email == "" || !out.EmailVerified {
		return Identity{}, fmt.Errorf("google auth: verified identity required")
	}
	email := strings.ToLower(strings.TrimSpace(out.Email))
	return Identity{Subject: out.Subject, Email: email, EmailVerified: true,
		EmailAuthoritative: strings.HasSuffix(email, "@gmail.com") || out.HostedDomain != "",
		Name:               strings.TrimSpace(out.Name), Picture: strings.TrimSpace(out.Picture)}, nil
}

func (c *Client) doWithRetry(ctx context.Context, build func(context.Context) (*http.Request, error)) ([]byte, int, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		req, err := build(ctx)
		if err != nil {
			return nil, 0, err
		}
		resp, err := c.http.Do(req)
		if err == nil {
			body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			_ = resp.Body.Close()
			if readErr != nil {
				lastErr = readErr
			} else if resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode < 500 {
				return body, resp.StatusCode, nil
			} else {
				lastErr = fmt.Errorf("http %d", resp.StatusCode)
				if attempt == 2 {
					return body, resp.StatusCode, nil
				}
			}
		} else {
			lastErr = err
		}
		if attempt == 2 {
			break
		}
		jitter, _ := cryptorand.Int(cryptorand.Reader, big.NewInt(100))
		var jitterMS int64
		if jitter != nil {
			jitterMS = jitter.Int64()
		}
		delay := time.Duration(int64(150*(1<<attempt))+jitterMS) * time.Millisecond
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, 0, ctx.Err()
		case <-timer.C:
		}
	}
	return nil, 0, lastErr
}

func truncate(body []byte) string {
	s := string(body)
	if len(s) > 300 {
		return s[:300]
	}
	return s
}
