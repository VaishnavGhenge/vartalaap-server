package googleauth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestAuthCodeURLUsesIdentityScopesAndPKCE(t *testing.T) {
	c := NewWithBase("client", "secret", "https://api.example/auth/google/callback", "https://google.test")
	u, err := url.Parse(c.AuthCodeURL("state-value", "challenge-value"))
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if q.Get("scope") != "openid email profile" {
		t.Fatalf("unexpected scope %q", q.Get("scope"))
	}
	if q.Get("code_challenge") != "challenge-value" || q.Get("code_challenge_method") != "S256" {
		t.Fatal("expected S256 PKCE challenge")
	}
	if q.Get("access_type") != "" {
		t.Fatal("identity login must not request offline calendar access")
	}
}

func TestExchangeReturnsVerifiedIdentity(t *testing.T) {
	var verifier string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			_ = r.ParseForm()
			verifier = r.Form.Get("code_verifier")
			_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "access"})
		case "/userinfo":
			if r.Header.Get("Authorization") != "Bearer access" {
				t.Error("missing bearer token")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"sub": "google-123", "email": "Alice@Example.com", "email_verified": true,
				"name": "Alice Smith", "picture": "https://images.example/alice.jpg",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	c := NewWithBase("client", "secret", "https://api.example/callback", ts.URL)
	id, err := c.Exchange(context.Background(), "code", "verifier")
	if err != nil {
		t.Fatal(err)
	}
	if verifier != "verifier" {
		t.Fatalf("verifier = %q", verifier)
	}
	if id.Subject != "google-123" || id.Email != "alice@example.com" || id.Name != "Alice Smith" {
		t.Fatalf("unexpected identity: %+v", id)
	}
}

func TestExchangeRejectsUnverifiedEmail(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "access"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"sub": "google-123", "email": "a@example.com", "email_verified": false})
	}))
	defer ts.Close()

	c := NewWithBase("client", "secret", "https://api.example/callback", ts.URL)
	if _, err := c.Exchange(context.Background(), "code", "verifier"); err == nil {
		t.Fatal("expected unverified email to be rejected")
	}
}
