package cfrealtime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const baseURL = "https://rtc.live.cloudflare.com/v1"

type Client struct {
	appID    string
	appToken string
	http     *http.Client
}

func New(appID, appToken string) *Client {
	return &Client{
		appID:    appID,
		appToken: appToken,
		http:     &http.Client{Timeout: 15 * time.Second},
	}
}

type SessionDescription struct {
	Type string `json:"type"`
	SDP  string `json:"sdp"`
}

// TrackObject is one track entry in a tracks/new request or response.
// Location "local" (or empty) means the track is published by this session.
// Location "remote" means this session is subscribing to a remote track.
type TrackObject struct {
	Location  string `json:"location,omitempty"` // "local" | "remote"
	TrackName string `json:"trackName"`
	Mid       string `json:"mid,omitempty"`
	SessionID string `json:"sessionId,omitempty"` // remote subscription target
}

type TracksNewRequest struct {
	SessionDescription *SessionDescription `json:"sessionDescription,omitempty"`
	Tracks             []TrackObject       `json:"tracks"`
}

type TracksNewResponse struct {
	SessionDescription             *SessionDescription `json:"sessionDescription,omitempty"`
	Tracks                         []TrackObject       `json:"tracks"`
	RequiresImmediateRenegotiation bool                `json:"requiresImmediateRenegotiation"`
}

type renegotiateRequest struct {
	SessionDescription SessionDescription `json:"sessionDescription"`
}

type renegotiateResponse struct {
	SessionDescription SessionDescription `json:"sessionDescription"`
}

// CreateSession creates a new Cloudflare Realtime SFU session and returns its ID.
func (c *Client) CreateSession(ctx context.Context) (string, error) {
	b, err := c.do(ctx, http.MethodPost, fmt.Sprintf("/apps/%s/sessions/new", c.appID), nil)
	if err != nil {
		return "", err
	}
	var out struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return "", fmt.Errorf("cfrealtime: decode createSession: %w", err)
	}
	return out.SessionID, nil
}

// TracksNew publishes local tracks (when req.SessionDescription is set) or subscribes
// to remote tracks (when tracks have Location="remote").
func (c *Client) TracksNew(ctx context.Context, sessionID string, req TracksNewRequest) (TracksNewResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return TracksNewResponse{}, err
	}
	b, err := c.do(ctx, http.MethodPost, fmt.Sprintf("/apps/%s/sessions/%s/tracks/new", c.appID, sessionID), body)
	if err != nil {
		return TracksNewResponse{}, err
	}
	var out TracksNewResponse
	if err := json.Unmarshal(b, &out); err != nil {
		return TracksNewResponse{}, fmt.Errorf("cfrealtime: decode tracksNew: %w", err)
	}
	return out, nil
}

// Renegotiate sends a fresh SDP offer after an ICE restart and returns the answer SDP.
func (c *Client) Renegotiate(ctx context.Context, sessionID, offerSDP string) (string, error) {
	req := renegotiateRequest{SessionDescription: SessionDescription{Type: "offer", SDP: offerSDP}}
	body, err := json.Marshal(req)
	if err != nil {
		return "", err
	}
	b, err := c.do(ctx, http.MethodPut, fmt.Sprintf("/apps/%s/sessions/%s/renegotiate", c.appID, sessionID), body)
	if err != nil {
		return "", err
	}
	var out renegotiateResponse
	if err := json.Unmarshal(b, &out); err != nil {
		return "", fmt.Errorf("cfrealtime: decode renegotiate: %w", err)
	}
	return out.SessionDescription.SDP, nil
}

// CloseSession terminates the Cloudflare Realtime session.
func (c *Client) CloseSession(ctx context.Context, sessionID string) error {
	_, err := c.do(ctx, http.MethodDelete, fmt.Sprintf("/apps/%s/sessions/%s", c.appID, sessionID), nil)
	return err
}

func (c *Client) do(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, baseURL+path, r)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.appToken)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cfrealtime %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("cfrealtime %s %s: %d %s", method, path, resp.StatusCode, string(b))
	}
	return b, nil
}
