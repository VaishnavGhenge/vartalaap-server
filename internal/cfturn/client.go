package cfturn

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

type Client struct {
	keyID    string
	apiToken string
	baseURL  string
	http     *http.Client
}

func New(keyID, apiToken string) *Client {
	return &Client{
		keyID:    keyID,
		apiToken: apiToken,
		baseURL:  "https://rtc.live.cloudflare.com/v1",
		http:     &http.Client{Timeout: 10 * time.Second},
	}
}

type IceServer struct {
	URLs       []string `json:"urls"`
	Username   string   `json:"username,omitempty"`
	Credential string   `json:"credential,omitempty"`
}

type CredentialsResponse struct {
	IceServers []IceServer `json:"iceServers"`
}

// Generate returns ICE server configs (STUN + TURN) with the given TTL in seconds.
// Endpoint per docs/cf-turn-api.md.
func (c *Client) Generate(ctx context.Context, ttlSeconds int) (CredentialsResponse, error) {
	body, _ := json.Marshal(map[string]int{"ttl": ttlSeconds})
	url := fmt.Sprintf("%s/turn/keys/%s/credentials/generate-ice-servers", c.baseURL, c.keyID)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return CredentialsResponse{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return CredentialsResponse{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return CredentialsResponse{}, fmt.Errorf("cf turn: %d %s", resp.StatusCode, string(b))
	}
	var out CredentialsResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return CredentialsResponse{}, err
	}
	return out, nil
}
