package httpx

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/vaishnavghenge/vartalaap-server/internal/auth"
	"github.com/vaishnavghenge/vartalaap-server/internal/cfrealtime"
	"github.com/vaishnavghenge/vartalaap-server/internal/metrics"
	"github.com/vaishnavghenge/vartalaap-server/internal/sfu"
)

// SFUHandlers registers the /sfu/* proxy routes.
// All routes require a valid JWT — Cloudflare credentials never leave the server.
//
// The routes mirror the wire protocol the partytracks client library expects
// (see partytracks/client). The roomId/peerId for a session are passed via
// query params on /sfu/sessions/new so partytracks doesn't need to know
// about our room model.
func SFUHandlers(mux *http.ServeMux, registry *sfu.Registry, cf *cfrealtime.Client, cfg AuthConfig, gates ...RoomAccessGate) {
	lim := NewRateLimiter(60, 120)
	var gate RoomAccessGate
	if len(gates) > 0 {
		gate = gates[0]
	}

	wrap := func(method string, h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			// SFU bodies (SDP offers/answers) are much larger than other
			// endpoints' JSON; use the SFU-specific body cap.
			if !enforceAPIRequestWithLimit(w, r, cfg.AllowedOrigins, method, lim, maxSFURequestBytes) {
				return
			}
			RequireRoomMember(cfg.JWTSecret, h)(w, r)
		}
	}

	mux.HandleFunc("/sfu/sessions/", wrap("POST, PUT, DELETE", handleSFUSessionRoute(registry, cf, gate)))
}

func handleSFUCreateSession(registry *sfu.Registry, cf *cfrealtime.Client, gate RoomAccessGate) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		userID, _ := auth.UserIDFromContext(r.Context())

		// partytracks sends roomId and peerId as query params via apiExtraParams.
		// `kind` is diagnostic only — clients create two CF sessions per peer
		// (one publish-only, one subscribe-only) and pass kind=publish|subscribe
		// so server logs can distinguish them.
		roomID := r.URL.Query().Get("roomId")
		peerID := r.URL.Query().Get("peerId")
		kind := r.URL.Query().Get("kind")
		if roomID == "" || peerID == "" {
			http.Error(w, "roomId and peerId query params are required", http.StatusBadRequest)
			return
		}
		if guestRoom := auth.RoomIDFromContext(r.Context()); guestRoom != "" && guestRoom != roomID {
			WriteError(w, http.StatusForbidden, "ROOM_MISMATCH", "token is not valid for this room")
			return
		}
		if gate != nil {
			if err := gate(r.Context(), roomID, false); err != nil {
				WriteError(w, http.StatusForbidden, roomAccessCode(err), roomAccessMessage(err))
				return
			}
		}

		sessionID, err := cf.CreateSession(r.Context())
		if err != nil {
			slog.Error("sfu: create session", "err", err)
			http.Error(w, "could not create SFU session", http.StatusBadGateway)
			return
		}

		registry.Register(sessionID, userID, roomID, peerID)
		slog.Info("sfu: session created", "session", sessionID, "peer", peerID, "room", roomID, "kind", kind)

		w.Header().Set("Content-Type", "application/json")
		// partytracks reads the entire JSON body and extracts sessionId.
		_ = json.NewEncoder(w).Encode(map[string]string{"sessionId": sessionID})
	}
}

// handleSFUSessionRoute dispatches sub-paths under /sfu/sessions/{id}/...
// Also handles /sfu/sessions/new which creates a new session.
func handleSFUSessionRoute(registry *sfu.Registry, cf *cfrealtime.Client, gate RoomAccessGate) http.HandlerFunc {
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

		// partytracks creates sessions at POST /sfu/sessions/new.
		if sessionID == "new" && subPath == "" && r.Method == http.MethodPost {
			handleSFUCreateSession(registry, cf, gate)(w, r)
			return
		}

		userID, _ := auth.UserIDFromContext(r.Context())
		ownerID, roomID, peerID, ok := registry.Lookup(sessionID)
		if !ok || ownerID != userID {
			http.Error(w, "session not found", http.StatusNotFound)
			return
		}
		if guestRoom := auth.RoomIDFromContext(r.Context()); guestRoom != "" && guestRoom != roomID {
			WriteError(w, http.StatusForbidden, "ROOM_MISMATCH", "token is not valid for this room")
			return
		}
		if gate != nil {
			if err := gate(r.Context(), roomID, false); err != nil {
				WriteError(w, http.StatusForbidden, roomAccessCode(err), roomAccessMessage(err))
				return
			}
		}

		switch {
		case subPath == "tracks/new" && r.Method == http.MethodPost:
			sfuTracksNew(cf, sessionID, roomID, peerID)(w, r)
		case subPath == "renegotiate" && r.Method == http.MethodPut:
			sfuRenegotiate(cf, sessionID)(w, r)
		case subPath == "tracks/update" && r.Method == http.MethodPut:
			sfuTracksPassthrough(cf, sessionID, "tracks/update")(w, r)
		case subPath == "tracks/close" && r.Method == http.MethodPut:
			sfuTracksPassthrough(cf, sessionID, "tracks/close")(w, r)
		case subPath == "" && r.Method == http.MethodDelete:
			sfuClose(registry, sessionID)(w, r)
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}
}

func sfuTracksNew(cf *cfrealtime.Client, sessionID, roomID, peerID string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req cfrealtime.TracksNewRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}

		resp, err := cf.TracksNew(r.Context(), sessionID, req)
		if err != nil {
			// When CF returns a structured error (e.g. session_error with errorCode +
			// errorDescription), forward the JSON body verbatim so partytracks on the
			// browser side can read errorCode and decide whether to recreate the
			// session. Wrapping in plain text via http.Error breaks partytracks's
			// res.json() call with a JSON parse error.
			var cfErr *cfrealtime.HTTPError
			if errors.As(err, &cfErr) {
				slog.Error("sfu: tracks/new",
					"err", err, "session", sessionID,
					"cf_status", cfErr.StatusCode, "cf_body", string(cfErr.Body))
				if len(cfErr.Body) > 0 && json.Valid(cfErr.Body) {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusBadGateway)
					_, _ = w.Write(cfErr.Body)
					return
				}
			} else {
				slog.Error("sfu: tracks/new", "err", err, "session", sessionID)
			}
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}

		// A 200 here means Cloudflare ACCEPTED the offer, not that the track is
		// live. The response still carries a sessionDescription the publisher's
		// browser has to apply, and partytracks only emits the track metadata
		// once that succeeds. Announcing from this point raced ahead of the
		// publisher by ~0.5s and produced the 2026-09-01 failure: a Firefox
		// video push that never acked, already broadcast to the room, pulled by
		// Chrome, dead for the rest of the call. Two Sentry issues, one cause.
		//
		// So the announce now comes only from the publisher, over sfu-announce,
		// after its push acks (SfuSession.onLocalTracksChanged →
		// hub.AnnounceSfuTracks). That path is level-triggered and re-sends the
		// full set on every change and after every reconnect, which is why this
		// one-shot interception is not needed as a second writer.
		//
		// Counted rather than silent: a publish seen here with no matching
		// announce shortly after means the ack never arrived, which is a real
		// client-side failure we want visible instead of papered over by a
		// server-side broadcast.
		if req.SessionDescription != nil {
			local := 0
			for _, t := range resp.Tracks {
				if t.Location == "" || t.Location == "local" {
					local++
				}
			}
			metrics.SfuPublishAccepted.Inc()
			slog.Info("sfu: publish accepted (awaiting publisher ack before announce)",
				"session", sessionID, "peer", peerID, "room", roomID,
				"resp_tracks", len(resp.Tracks), "local_tracks", local,
				"requires_renegotiation", resp.RequiresImmediateRenegotiation)
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

		respBody, err := cf.Renegotiate(r.Context(), sessionID, body.SessionDescription.Type, body.SessionDescription.SDP)
		if err != nil {
			slog.Error("sfu: renegotiate", "err", err, "session", sessionID)
			// If CF returned an error body, forward it so partytracks can read errorCode.
			if len(respBody) > 0 {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadGateway)
				_, _ = w.Write(respBody)
				return
			}
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		if len(respBody) == 0 {
			// CF returned an empty body on success; emit {} so partytracks' res.json() succeeds.
			_, _ = w.Write([]byte("{}"))
			return
		}
		_, _ = w.Write(respBody)
	}
}

// sfuTracksPassthrough proxies tracks/update and tracks/close to CF verbatim.
// Both endpoints accept the same JSON shape (tracks + optional sessionDescription)
// and we don't need to inspect or broadcast them — they don't introduce new
// publications, just modify or remove existing ones.
//
// partytracks' #fetchWithRecordedHistory calls .clone().json() on every response
// for its debug history log, so every response MUST contain valid JSON — even
// if CF returns an empty body on success.
func sfuTracksPassthrough(cf *cfrealtime.Client, sessionID, subPath string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		respBody, status, err := cf.Passthrough(r.Context(), r.Method, sessionID, subPath, r.Body)
		if err != nil {
			slog.Error("sfu: passthrough", "err", err, "session", sessionID, "subPath", subPath)
			if len(respBody) > 0 {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadGateway)
				_, _ = w.Write(respBody)
				return
			}
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if len(respBody) == 0 {
			_, _ = w.Write([]byte("{}"))
			return
		}
		_, _ = w.Write(respBody)
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
