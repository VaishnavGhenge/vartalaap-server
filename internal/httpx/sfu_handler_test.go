package httpx

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vaishnavghenge/vartalaap-server/internal/auth"
	"github.com/vaishnavghenge/vartalaap-server/internal/cfrealtime"
	"github.com/vaishnavghenge/vartalaap-server/internal/sfu"
)

const testSFUSecret = "sfu-test-secret-32-bytes-padded!"

// ─── Fake Cloudflare Realtime client ──────────────────────────────────────────

type fakeCFClient struct {
	// createSession returns this session ID (or error if err != nil)
	sessionID string
	createErr error

	// tracksNew returns this response (or error)
	tracksResp cfrealtime.TracksNewResponse
	tracksErr  error

	// renegotiate error
	regenErr error

	// recorded calls
	createdSessions []string
	tracksNewCalls  []cfrealtime.TracksNewRequest
	regenCalls      []struct{ sdpType, sdp string }
}

func (f *fakeCFClient) CreateSession(_ interface{}) (string, error) {
	f.createdSessions = append(f.createdSessions, f.sessionID)
	return f.sessionID, f.createErr
}

// ─── Registry helper ──────────────────────────────────────────────────────────

func makeTestRegistry() *sfu.Registry {
	return sfu.NewRegistry()
}

// ─── HTTP-level fake for Cloudflare Realtime ──────────────────────────────────
//
// The cfrealtime.Client calls the real CF HTTPS API. To avoid live calls in
// tests, we stand up a local httptest.Server that mimics CF responses, then
// point cfrealtime.New at it via a custom base URL. Since the baseURL is a
// package-level constant, we instead create a real *cfrealtime.Client aimed at
// a fake server using the unexported test hook approach: stand up the fake HTTP
// server and inject its URL via environment or by wrapping the mux.

// fakeCFServer stands up a test HTTP server acting as the CF Realtime API.
type fakeCFServer struct {
	server     *httptest.Server
	sessionID  string
	tracksResp cfrealtime.TracksNewResponse
	regenOK    bool

	// track calls
	createCalled bool
	tracksCalled bool
	regenCalled  bool
}

func newFakeCFServer(sessionID string, tracksResp cfrealtime.TracksNewResponse) *fakeCFServer {
	f := &fakeCFServer{sessionID: sessionID, tracksResp: tracksResp, regenOK: true}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/apps/", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case strings.HasSuffix(path, "/sessions/new") && r.Method == http.MethodPost:
			f.createCalled = true
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"sessionId": f.sessionID})
		case strings.Contains(path, "/tracks/new") && r.Method == http.MethodPost:
			f.tracksCalled = true
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(f.tracksResp)
		case strings.HasSuffix(path, "/renegotiate") && r.Method == http.MethodPut:
			f.regenCalled = true
			if f.regenOK {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]string{})
			} else {
				http.Error(w, "CF error", http.StatusInternalServerError)
			}
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	})
	f.server = httptest.NewServer(mux)
	return f
}

func (f *fakeCFServer) client() *cfrealtime.Client {
	return cfrealtime.NewWithBaseURL("app-id", "app-token", f.server.URL+"/v1")
}

func (f *fakeCFServer) close() { f.server.Close() }

// ─── Auth helpers ─────────────────────────────────────────────────────────────

func makeSFUAuthToken(userID string) string {
	tok, _ := auth.SignAccessToken(userID, testSFUSecret, time.Minute)
	return tok
}

func authCfgSFU() AuthConfig {
	return AuthConfig{
		AllowedOrigins: []string{"http://localhost:3000"},
		JWTSecret:      testSFUSecret,
	}
}

func sfuRequest(method, path string, body interface{}, token string) *http.Request {
	var buf *bytes.Buffer
	if body != nil {
		b, _ := json.Marshal(body)
		buf = bytes.NewBuffer(b)
	} else {
		buf = &bytes.Buffer{}
	}
	req := httptest.NewRequest(method, path, buf)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://localhost:3000")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return req
}

// ─── Tests: POST /sfu/sessions/new ────────────────────────────────────────────

func TestSFUCreateSession_OK(t *testing.T) {
	cf := newFakeCFServer("cf-session-abc", cfrealtime.TracksNewResponse{})
	defer cf.close()

	registry := makeTestRegistry()
	mux := http.NewServeMux()
	SFUHandlers(mux, registry, cf.client(), authCfgSFU())

	token := makeSFUAuthToken("user-1")
	req := sfuRequest(http.MethodPost, "/sfu/sessions/new?roomId=room-1&peerId=peer-alice&kind=publish", nil, token)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["sessionId"] != "cf-session-abc" {
		t.Fatalf("expected sessionId=cf-session-abc, got %q", resp["sessionId"])
	}
	// Session must be registered.
	userID, roomID, peerID, ok := registry.Lookup("cf-session-abc")
	if !ok {
		t.Fatal("session not registered")
	}
	if userID != "user-1" || roomID != "room-1" || peerID != "peer-alice" {
		t.Fatalf("unexpected registry entry: userID=%s roomID=%s peerID=%s", userID, roomID, peerID)
	}
}

func TestSFUCreateSession_MissingBody(t *testing.T) {
	cf := newFakeCFServer("sess", cfrealtime.TracksNewResponse{})
	defer cf.close()

	registry := makeTestRegistry()
	mux := http.NewServeMux()
	SFUHandlers(mux, registry, cf.client(), authCfgSFU())

	token := makeSFUAuthToken("user-1")

	for _, path := range []string{
		"/sfu/sessions/new?roomId=room-1",     // missing peerId
		"/sfu/sessions/new?peerId=peer-1",     // missing roomId
		"/sfu/sessions/new?roomId=&peerId=p1", // empty roomId
	} {
		req := sfuRequest(http.MethodPost, path, nil, token)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("expected 400, got %d for path %s", rec.Code, path)
		}
	}
}

func TestSFUCreateSession_Unauthenticated(t *testing.T) {
	cf := newFakeCFServer("sess", cfrealtime.TracksNewResponse{})
	defer cf.close()

	registry := makeTestRegistry()
	mux := http.NewServeMux()
	SFUHandlers(mux, registry, cf.client(), authCfgSFU())

	req := sfuRequest(http.MethodPost, "/sfu/sessions/new?roomId=room-1&peerId=peer-1", nil, "")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

// ─── Tests: POST /sfu/sessions/{id}/tracks/new ───────────────────────────────

func setupSessionAndMux(t *testing.T, cfServer *fakeCFServer) (mux *http.ServeMux, registry *sfu.Registry, sessionID, token string) {
	t.Helper()
	registry = makeTestRegistry()
	mux = http.NewServeMux()
	SFUHandlers(mux, registry, cfServer.client(), authCfgSFU())

	sessionID = cfServer.sessionID
	token = makeSFUAuthToken("user-1")
	registry.Register(sessionID, "user-1", "room-1", "peer-alice")
	return
}

func TestSFUTracksNew_Publish(t *testing.T) {
	tracksResp := cfrealtime.TracksNewResponse{
		Tracks: []cfrealtime.TrackObject{
			{TrackName: "audio", Mid: "0", Location: "local"},
			{TrackName: "video", Mid: "1", Location: "local"},
		},
		RequiresImmediateRenegotiation: false,
	}
	cf := newFakeCFServer("sess-1", tracksResp)
	defer cf.close()

	mux, _, sessionID, token := setupSessionAndMux(t, cf)

	body := cfrealtime.TracksNewRequest{
		SessionDescription: &cfrealtime.SessionDescription{Type: "offer", SDP: "v=0\r\n"},
		Tracks: []cfrealtime.TrackObject{
			{TrackName: "audio", Mid: "0"},
			{TrackName: "video", Mid: "1"},
		},
	}
	req := sfuRequest(http.MethodPost, fmt.Sprintf("/sfu/sessions/%s/tracks/new", sessionID), body, token)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp cfrealtime.TracksNewResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Tracks) != 2 {
		t.Fatalf("expected 2 tracks, got %d", len(resp.Tracks))
	}
}

func TestSFUTracksNew_SessionNotFound(t *testing.T) {
	cf := newFakeCFServer("sess-1", cfrealtime.TracksNewResponse{})
	defer cf.close()

	registry := makeTestRegistry()
	mux := http.NewServeMux()
	SFUHandlers(mux, registry, cf.client(), authCfgSFU())

	token := makeSFUAuthToken("user-1")
	body := cfrealtime.TracksNewRequest{Tracks: []cfrealtime.TrackObject{{TrackName: "audio"}}}
	req := sfuRequest(http.MethodPost, "/sfu/sessions/unknown-session/tracks/new", body, token)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

func TestSFUTracksNew_WrongOwner(t *testing.T) {
	cf := newFakeCFServer("sess-1", cfrealtime.TracksNewResponse{})
	defer cf.close()

	registry := makeTestRegistry()
	registry.Register("sess-1", "user-2", "room-1", "peer-bob") // owned by user-2
	mux := http.NewServeMux()
	SFUHandlers(mux, registry, cf.client(), authCfgSFU())

	token := makeSFUAuthToken("user-1") // authenticated as user-1
	body := cfrealtime.TracksNewRequest{Tracks: []cfrealtime.TrackObject{{TrackName: "audio"}}}
	req := sfuRequest(http.MethodPost, "/sfu/sessions/sess-1/tracks/new", body, token)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 (owner mismatch), got %d", rec.Code)
	}
}

// ─── Tests: PUT /sfu/sessions/{id}/renegotiate ───────────────────────────────

func TestSFURenegotiate_OK(t *testing.T) {
	cf := newFakeCFServer("sess-1", cfrealtime.TracksNewResponse{})
	defer cf.close()

	mux, _, sessionID, token := setupSessionAndMux(t, cf)

	body := map[string]interface{}{
		"sessionDescription": map[string]string{
			"type": "answer",
			"sdp":  "v=0\r\n",
		},
	}
	req := sfuRequest(http.MethodPut, fmt.Sprintf("/sfu/sessions/%s/renegotiate", sessionID), body, token)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if strings.TrimSpace(rec.Body.String()) != "{}" {
		t.Fatalf("expected JSON empty object, got %q", rec.Body.String())
	}
}

func TestSFURenegotiate_MissingSDP(t *testing.T) {
	cf := newFakeCFServer("sess-1", cfrealtime.TracksNewResponse{})
	defer cf.close()

	mux, _, sessionID, token := setupSessionAndMux(t, cf)

	body := map[string]interface{}{
		"sessionDescription": map[string]string{"type": "answer"}, // sdp missing
	}
	req := sfuRequest(http.MethodPut, fmt.Sprintf("/sfu/sessions/%s/renegotiate", sessionID), body, token)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

// ─── Tests: DELETE /sfu/sessions/{id} ────────────────────────────────────────

func TestSFUClose_OK(t *testing.T) {
	cf := newFakeCFServer("sess-1", cfrealtime.TracksNewResponse{})
	defer cf.close()

	mux, registry, sessionID, token := setupSessionAndMux(t, cf)

	req := sfuRequest(http.MethodDelete, fmt.Sprintf("/sfu/sessions/%s", sessionID), nil, token)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body.String())
	}
	// Session must be removed from registry.
	if _, _, _, ok := registry.Lookup(sessionID); ok {
		t.Fatal("expected session to be removed from registry")
	}
}

// ─── Tests: guest room-scope enforcement ─────────────────────────────────────
//
// The security model: an invited (non-account) guest is issued a JWT that
// carries a RoomID claim. RequireRoomMember puts that claim into the request
// context, and every SFU handler MUST refuse the request when the guest's
// RoomID disagrees with the roomId in the URL/query. Without this guard, a
// guest with a valid token for room-A could create an SFU session in room-B,
// or operate on a session belonging to a different room — bypassing the entire
// room-membership model.
//
// The check lives at sfu_handler.go:63 and :118. Both call sites are exercised
// here; deleting either guard fails one of these tests.

func makeSFUGuestToken(guestID, roomID string) string {
	tok, _ := auth.SignGuestToken(guestID, roomID, testSFUSecret, time.Minute)
	return tok
}

// A guest holding a token for room-1 IS allowed to create an SFU session in
// room-1 — establishes the positive case so the negative case below is a real
// distinction rather than a global ban.
func TestSFUCreateSession_GuestTokenForOwnRoomSucceeds(t *testing.T) {
	cf := newFakeCFServer("cf-session-guest", cfrealtime.TracksNewResponse{})
	defer cf.close()

	registry := makeTestRegistry()
	mux := http.NewServeMux()
	SFUHandlers(mux, registry, cf.client(), authCfgSFU())

	token := makeSFUGuestToken("guest-alice", "room-1")
	req := sfuRequest(http.MethodPost, "/sfu/sessions/new?roomId=room-1&peerId=peer-guest&kind=publish", nil, token)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

// The keystone test: a guest with a token for room-1 attempting to create an
// SFU session in room-2 must be rejected with 403 ROOM_MISMATCH. If this test
// fails, the room-membership boundary is broken — any guest invite link
// becomes a key to every room.
func TestSFUCreateSession_GuestTokenForWrongRoomReturns403(t *testing.T) {
	cf := newFakeCFServer("cf-session-x", cfrealtime.TracksNewResponse{})
	defer cf.close()

	registry := makeTestRegistry()
	mux := http.NewServeMux()
	SFUHandlers(mux, registry, cf.client(), authCfgSFU())

	token := makeSFUGuestToken("guest-alice", "room-1")
	// Same token, different roomId in the request — must be denied.
	req := sfuRequest(http.MethodPost, "/sfu/sessions/new?roomId=room-2&peerId=peer-guest", nil, token)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 (room mismatch), got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "ROOM_MISMATCH") {
		t.Errorf("expected error code ROOM_MISMATCH in body, got: %s", rec.Body.String())
	}
	// And no session must have been registered.
	if _, _, _, ok := registry.Lookup("cf-session-x"); ok {
		t.Error("session was registered despite room mismatch — guard failed")
	}
}

// The /sfu/sessions/{id}/{...} routes have their own room-scope guard at
// sfu_handler.go:118 (separate from the /new guard). A guest who somehow
// obtained the session ID of a room-1 session must not be able to operate on
// it with a room-2 token.
func TestSFUSessionRoute_GuestTokenForWrongRoomReturns403(t *testing.T) {
	cf := newFakeCFServer("sess-room1", cfrealtime.TracksNewResponse{})
	defer cf.close()

	registry := makeTestRegistry()
	// Session belongs to guest-alice in room-1.
	registry.Register("sess-room1", "guest-alice", "room-1", "peer-alice")
	mux := http.NewServeMux()
	SFUHandlers(mux, registry, cf.client(), authCfgSFU())

	// Attacker holds a valid guest token but for room-2, not room-1.
	wrongToken := makeSFUGuestToken("guest-alice", "room-2")
	body := cfrealtime.TracksNewRequest{Tracks: []cfrealtime.TrackObject{{TrackName: "audio"}}}
	req := sfuRequest(http.MethodPost, "/sfu/sessions/sess-room1/tracks/new", body, wrongToken)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 on sub-route with wrong room scope, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "ROOM_MISMATCH") {
		t.Errorf("expected ROOM_MISMATCH error code, got: %s", rec.Body.String())
	}
}

// A regular (non-guest) user token has no RoomID claim, so the scope guard
// must be a no-op for them — they're already gated by other handlers (room
// access gate). Without this distinction, the SFU would be unreachable for
// real users. This pins the "guestRoom != ”" half of the guard.
func TestSFUCreateSession_RegularUserTokenHasNoRoomScopeRestriction(t *testing.T) {
	cf := newFakeCFServer("cf-session-user", cfrealtime.TracksNewResponse{})
	defer cf.close()

	registry := makeTestRegistry()
	mux := http.NewServeMux()
	SFUHandlers(mux, registry, cf.client(), authCfgSFU())

	// Regular user — no RoomID claim. Should work for any room.
	token := makeSFUAuthToken("user-1")
	req := sfuRequest(http.MethodPost, "/sfu/sessions/new?roomId=any-room&peerId=peer-1", nil, token)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("regular user must not be subject to guest room-scope guard; got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestSFUClose_NotFound(t *testing.T) {
	cf := newFakeCFServer("sess-1", cfrealtime.TracksNewResponse{})
	defer cf.close()

	registry := makeTestRegistry()
	mux := http.NewServeMux()
	SFUHandlers(mux, registry, cf.client(), authCfgSFU())

	token := makeSFUAuthToken("user-1")
	req := sfuRequest(http.MethodDelete, "/sfu/sessions/nonexistent", nil, token)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}
