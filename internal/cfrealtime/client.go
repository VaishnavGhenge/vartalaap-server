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

const defaultBaseURL = "https://rtc.live.cloudflare.com/v1"

type Client struct {
	appID    string
	appToken string
	http     *http.Client
	baseURL  string
}

func New(appID, appToken string) *Client {
	return &Client{
		appID:    appID,
		appToken: appToken,
		http:     &http.Client{Timeout: 15 * time.Second},
		baseURL:  defaultBaseURL,
	}
}

// NewWithBaseURL creates a Client that sends requests to baseURL instead of the
// default Cloudflare endpoint. Intended for tests only.
func NewWithBaseURL(appID, appToken, base string) *Client {
	return &Client{
		appID:    appID,
		appToken: appToken,
		http:     &http.Client{Timeout: 15 * time.Second},
		baseURL:  base,
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

// renegotiateResponse carries only an optional error — no SDP is returned.
type renegotiateResponse struct {
	ErrorCode        string `json:"errorCode,omitempty"`
	ErrorDescription string `json:"errorDescription,omitempty"`
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

// Renegotiate sends an SDP answer (produced after a subscribe-triggered renegotiation)
// to Cloudflare. CF returns only a success/error — no SDP is returned.
// sdpType should be "answer" for the subscribe flow; "offer" for ICE-restart scenarios.
func (c *Client) Renegotiate(ctx context.Context, sessionID, sdpType, sdp string) error {
	req := renegotiateRequest{SessionDescription: SessionDescription{Type: sdpType, SDP: sdp}}
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	b, err := c.do(ctx, http.MethodPut, fmt.Sprintf("/apps/%s/sessions/%s/renegotiate", c.appID, sessionID), body)
	if err != nil {
		return err
	}
	var out renegotiateResponse
	if err := json.Unmarshal(b, &out); err != nil {
		return fmt.Errorf("cfrealtime: decode renegotiate: %w", err)
	}
	if out.ErrorCode != "" {
		return fmt.Errorf("cfrealtime: renegotiate error %s: %s", out.ErrorCode, out.ErrorDescription)
	}
	return nil
}

// CloseTrack closes specific tracks by mid. Use this to cleanly remove
// tracks from a session. Sessions themselves expire when the underlying
// WebRTC connection closes — there is no session-level delete endpoint.
func (c *Client) CloseTrack(ctx context.Context, sessionID string, mids []string, offerSDP string) error {
	type trackClose struct {
		Mid string `json:"mid"`
	}
	type req struct {
		Tracks             []trackClose        `json:"tracks"`
		SessionDescription *SessionDescription `json:"sessionDescription,omitempty"`
	}
	tracks := make([]trackClose, len(mids))
	for i, m := range mids {
		tracks[i] = trackClose{Mid: m}
	}
	body, err := json.Marshal(req{Tracks: tracks})
	if err != nil {
		return err
	}
	_, err = c.do(ctx, http.MethodPut, fmt.Sprintf("/apps/%s/sessions/%s/tracks/close", c.appID, sessionID), body)
	return err
}

func (c *Client) do(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, r)
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
