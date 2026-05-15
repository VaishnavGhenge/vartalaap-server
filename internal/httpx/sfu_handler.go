package httpx

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"github.com/vaishnavghenge/vartalaap-server/internal/auth"
	"github.com/vaishnavghenge/vartalaap-server/internal/cfrealtime"
	"github.com/vaishnavghenge/vartalaap-server/internal/sfu"
	"github.com/vaishnavghenge/vartalaap-server/internal/signaling"
)

// SFUHandlers registers the /sfu/* proxy routes.
// All routes require a valid JWT — Cloudflare credentials never leave the server.
func SFUHandlers(mux *http.ServeMux, hub *signaling.Hub, registry *sfu.Registry, cf *cfrealtime.Client, cfg AuthConfig) {
	lim := NewRateLimiter(60, 120)

	wrap := func(method string, h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if !enforceAPIRequest(w, r, cfg.AllowedOrigins, method, lim) {
				return
			}
			RequireAuth(cfg.JWTSecret, h)(w, r)
		}
	}

	mux.HandleFunc("/sfu/sessions", wrap("POST", handleSFUCreateSession(registry, cf)))
	mux.HandleFunc("/sfu/sessions/", wrap("POST, PUT, DELETE", handleSFUSessionRoute(hub, registry, cf)))
}

func handleSFUCreateSession(registry *sfu.Registry, cf *cfrealtime.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		userID, _ := auth.UserIDFromContext(r.Context())

		var body struct {
			RoomID string `json:"roomId"`
			PeerID string `json:"peerId"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.RoomID == "" || body.PeerID == "" {
			http.Error(w, "roomId and peerId are required", http.StatusBadRequest)
			return
		}

		sessionID, err := cf.CreateSession(r.Context())
		if err != nil {
			slog.Error("sfu: create session", "err", err)
			http.Error(w, "could not create SFU session", http.StatusBadGateway)
			return
		}

		registry.Register(sessionID, userID, body.RoomID, body.PeerID)

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"sessionId": sessionID})
	}
}

// handleSFUSessionRoute dispatches sub-paths under /sfu/sessions/{id}/...
func handleSFUSessionRoute(hub *signaling.Hub, registry *sfu.Registry, cf *cfrealtime.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tail := strings.TrimPrefix(r.URL.Path, "/sfu/sessions/")
		parts := strings.SplitN(tail, "/", 2)
		sessionID := parts[0]
		subPath := ""
		if len(parts) == 2 {
			subPath = parts[1]
		}
		if sessionID == "" {
			http.Error(w, "missing session id", http.StatusBadRequest)
			return
		}

		userID, _ := auth.UserIDFromContext(r.Context())
		ownerID, roomID, peerID, ok := registry.Lookup(sessionID)
		if !ok || ownerID != userID {
			http.Error(w, "session not found", http.StatusNotFound)
			return
		}

		switch {
		case subPath == "tracks/new" && r.Method == http.MethodPost:
			sfuTracksNew(hub, cf, sessionID, roomID, peerID)(w, r)
		case subPath == "renegotiate" && r.Method == http.MethodPut:
			sfuRenegotiate(cf, sessionID)(w, r)
		case subPath == "" && r.Method == http.MethodDelete:
			sfuClose(registry, sessionID)(w, r)
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}
}

func sfuTracksNew(hub *signaling.Hub, cf *cfrealtime.Client, sessionID, roomID, peerID string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req cfrealtime.TracksNewRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}

		resp, err := cf.TracksNew(r.Context(), sessionID, req)
		if err != nil {
			slog.Error("sfu: tracks/new", "err", err, "session", sessionID)
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}

		// After a local publish (request has sessionDescription), broadcast sfu-tracks
		// so remote peers know to subscribe. Do not broadcast subscribe responses.
		if req.SessionDescription != nil {
			tracks := make([]signaling.SfuTrackInfo, 0, len(resp.Tracks))
			for _, t := range resp.Tracks {
				if t.Location == "" || t.Location == "local" {
					tracks = append(tracks, signaling.SfuTrackInfo{TrackName: t.TrackName, Mid: t.Mid})
				}
			}
			if len(tracks) > 0 {
				hub.BroadcastSfuTracks(roomID, peerID, signaling.SfuTracksData{
					SessionID: sessionID,
					Tracks:    tracks,
				})
			}
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}
}

func sfuRenegotiate(cf *cfrealtime.Client, sessionID string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// The client sends the sessionDescription as produced by its RTCPeerConnection.
		// For the subscribe flow this is type:"answer"; for ICE restart it may be "offer".
		// We proxy it verbatim to CF — CF returns only a success/error, no SDP.
		var body struct {
			SessionDescription struct {
				Type string `json:"type"`
				SDP  string `json:"sdp"`
			} `json:"sessionDescription"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil ||
			body.SessionDescription.SDP == "" || body.SessionDescription.Type == "" {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}

		if err := cf.Renegotiate(r.Context(), sessionID, body.SessionDescription.Type, body.SessionDescription.SDP); err != nil {
			slog.Error("sfu: renegotiate", "err", err, "session", sessionID)
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}

		w.WriteHeader(http.StatusNoContent)
	}
}

func sfuClose(registry *sfu.Registry, sessionID string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Remove from registry immediately so the session cannot be reused.
		// CF sessions expire automatically once the underlying RTCPeerConnection
		// closes — there is no session-level delete endpoint in the Realtime API.
		registry.Remove(sessionID)
		w.WriteHeader(http.StatusNoContent)
	}
}
