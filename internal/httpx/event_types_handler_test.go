package httpx

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vaishnavghenge/vartalaap-server/internal/store"
)

// eventTypeFixture spins up a registered user and returns helpers for hitting
// /me/event-types as that user. Tests can mutate the plan after registration
// by reaching into the memStore for plan-gating coverage.
func eventTypeFixture(t *testing.T, st *memStore, email string) (userID string, request func(method, path, body string) *http.Request) {
	t.Helper()
	// handleRegister lowercases the stored email; mirror that here so the
	// fixture's GetUserByEmail lookup actually finds the user it just created.
	email = strings.ToLower(email)
	registerUser(t, st, email, "password123")
	u, err := st.GetUserByEmail(context.Background(), email)
	if err != nil {
		t.Fatalf("fixture: GetUserByEmail(%q): %v", email, err)
	}
	tok := tokenForUser(t, u.ID)
	return u.ID, func(method, path, body string) *http.Request {
		req := authReq(method, path, body)
		req.Header.Set("Authorization", "Bearer "+tok)
		return req
	}
}

// setPlan updates the canonical user record in the in-memory store so the
// plan-gating branch can be exercised without going through a billing flow
// the server doesn't have yet.
func setPlan(t *testing.T, st *memStore, userID, plan string) {
	t.Helper()
	st.mu.Lock()
	defer st.mu.Unlock()
	u, ok := st.users[userID]
	if !ok {
		t.Fatalf("setPlan: user %q not found", userID)
	}
	u.Plan = plan
}

func eventTypeBody(t *testing.T, dto eventTypeDTO) string {
	t.Helper()
	b, err := json.Marshal(dto)
	if err != nil {
		t.Fatalf("marshal event type: %v", err)
	}
	return string(b)
}

// validFreeEventType returns a minimal valid free-plan event type body. Each
// test customises the bits it cares about and leaves the rest at defaults so
// it's obvious what each case is checking.
func validFreeEventType() eventTypeDTO {
	return eventTypeDTO{
		Slug: "intro", Title: "Intro Call",
		DurationMin: 30, BufferMin: 0,
		IsPaid: false, IsActive: true,
	}
}

// ─── List + Create ────────────────────────────────────────────────────────────

func TestCreateEventType_HappyPath(t *testing.T) {
	st := newMemStore()
	_, req := eventTypeFixture(t, st, "host@example.com")

	rec := httptest.NewRecorder()
	dispatchEventTypes(st, rec, req(http.MethodPost, "/me/event-types",
		eventTypeBody(t, validFreeEventType())))
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var got eventTypeDTO
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ID == "" {
		t.Fatal("expected generated id")
	}
	if got.Slug != "intro" || got.DurationMin != 30 {
		t.Fatalf("unexpected created event: %+v", got)
	}
}

func TestListEventTypes_RoundTrip(t *testing.T) {
	st := newMemStore()
	_, req := eventTypeFixture(t, st, "host@example.com")

	for _, slug := range []string{"intro", "deep-dive"} {
		dto := validFreeEventType()
		dto.Slug = slug
		dto.IsActive = slug == "intro" // only one active to stay under free cap
		rec := httptest.NewRecorder()
		dispatchEventTypes(st, rec, req(http.MethodPost, "/me/event-types", eventTypeBody(t, dto)))
		if rec.Code != http.StatusCreated {
			t.Fatalf("seed %q: %d %s", slug, rec.Code, rec.Body.String())
		}
	}

	rec := httptest.NewRecorder()
	dispatchEventTypes(st, rec, req(http.MethodGet, "/me/event-types", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var list eventTypeListResponse
	json.NewDecoder(rec.Body).Decode(&list)
	if len(list.EventTypes) != 2 {
		t.Fatalf("expected 2 events, got %d", len(list.EventTypes))
	}
}

// ─── Plan enforcement ─────────────────────────────────────────────────────────

func TestCreateEventType_FreePlanCapsAtOneActive(t *testing.T) {
	st := newMemStore()
	_, req := eventTypeFixture(t, st, "free@example.com")

	first := validFreeEventType()
	first.Slug = "first-event"
	rec1 := httptest.NewRecorder()
	dispatchEventTypes(st, rec1, req(http.MethodPost, "/me/event-types", eventTypeBody(t, first)))
	if rec1.Code != http.StatusCreated {
		t.Fatalf("first event should succeed: %d %s", rec1.Code, rec1.Body.String())
	}

	second := validFreeEventType()
	second.Slug = "second-event"
	rec2 := httptest.NewRecorder()
	dispatchEventTypes(st, rec2, req(http.MethodPost, "/me/event-types", eventTypeBody(t, second)))
	if rec2.Code != http.StatusForbidden {
		t.Fatalf("second active event on free plan should 403, got %d: %s",
			rec2.Code, rec2.Body.String())
	}
	if !strings.Contains(rec2.Body.String(), "plan allows") {
		t.Fatalf("expected plan-cap message, got %q", rec2.Body.String())
	}
}

func TestCreateEventType_SoloPlanAllowsMultipleActive(t *testing.T) {
	st := newMemStore()
	uid, req := eventTypeFixture(t, st, "solo@example.com")
	setPlan(t, st, uid, "solo")

	for _, slug := range []string{"a", "b", "c"} {
		dto := validFreeEventType()
		dto.Slug = slug
		rec := httptest.NewRecorder()
		dispatchEventTypes(st, rec, req(http.MethodPost, "/me/event-types", eventTypeBody(t, dto)))
		if rec.Code != http.StatusCreated {
			t.Fatalf("solo plan should allow %q: %d %s", slug, rec.Code, rec.Body.String())
		}
	}
}

func TestCreateEventType_FreePlanInactiveDoesNotCount(t *testing.T) {
	st := newMemStore()
	_, req := eventTypeFixture(t, st, "free@example.com")

	// One inactive event — should not consume the active slot.
	dto1 := validFreeEventType()
	dto1.Slug = "archived"
	dto1.IsActive = false
	rec1 := httptest.NewRecorder()
	dispatchEventTypes(st, rec1, req(http.MethodPost, "/me/event-types", eventTypeBody(t, dto1)))
	if rec1.Code != http.StatusCreated {
		t.Fatalf("inactive create should succeed: %d %s", rec1.Code, rec1.Body.String())
	}

	// Now one active — should still succeed.
	dto2 := validFreeEventType()
	dto2.Slug = "live"
	rec2 := httptest.NewRecorder()
	dispatchEventTypes(st, rec2, req(http.MethodPost, "/me/event-types", eventTypeBody(t, dto2)))
	if rec2.Code != http.StatusCreated {
		t.Fatalf("active create after inactive should succeed: %d %s", rec2.Code, rec2.Body.String())
	}
}

func TestCreateEventType_FreePlanRejectsPaid(t *testing.T) {
	st := newMemStore()
	_, req := eventTypeFixture(t, st, "free@example.com")

	dto := validFreeEventType()
	dto.IsPaid = true
	price := 5000
	dto.PriceCents = &price
	rec := httptest.NewRecorder()
	dispatchEventTypes(st, rec, req(http.MethodPost, "/me/event-types", eventTypeBody(t, dto)))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("free + paid should 403, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestCreateEventType_SoloPlanAllowsPaid(t *testing.T) {
	st := newMemStore()
	uid, req := eventTypeFixture(t, st, "solo@example.com")
	setPlan(t, st, uid, "solo")

	dto := validFreeEventType()
	dto.IsPaid = true
	price := 5000
	dto.PriceCents = &price
	rec := httptest.NewRecorder()
	dispatchEventTypes(st, rec, req(http.MethodPost, "/me/event-types", eventTypeBody(t, dto)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("solo + paid should succeed: %d %s", rec.Code, rec.Body.String())
	}
}

// ─── Validation ───────────────────────────────────────────────────────────────

func TestCreateEventType_ValidationErrors(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*eventTypeDTO)
		want   string
	}{
		{"bad slug punctuation", func(d *eventTypeDTO) { d.Slug = "bad slug!" }, "slug"},
		{"empty slug", func(d *eventTypeDTO) { d.Slug = "" }, "slug"},
		{"slug too long", func(d *eventTypeDTO) { d.Slug = strings.Repeat("a", 41) }, "slug"},
		{"empty title", func(d *eventTypeDTO) { d.Title = "" }, "title"},
		{"title too long", func(d *eventTypeDTO) { d.Title = strings.Repeat("t", 121) }, "title"},
		{"bad duration", func(d *eventTypeDTO) { d.DurationMin = 25 }, "durationMin"},
		{"negative buffer", func(d *eventTypeDTO) { d.BufferMin = -1 }, "bufferMin"},
		{"price without paid", func(d *eventTypeDTO) {
			p := 100
			d.PriceCents = &p
		}, "priceCents must be omitted"},
		{"zero maxPerDay", func(d *eventTypeDTO) {
			z := 0
			d.MaxPerDay = &z
		}, "maxPerDay"},
		{"bad currency", func(d *eventTypeDTO) { d.Currency = "DOLLARS" }, "currency"},
		{"bad paymentTiming", func(d *eventTypeDTO) { d.PaymentTiming = "later" }, "paymentTiming"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newMemStore()
			_, req := eventTypeFixture(t, st, fmt.Sprintf("%s@example.com",
				strings.ReplaceAll(tc.name, " ", "-")))
			dto := validFreeEventType()
			tc.mutate(&dto)
			rec := httptest.NewRecorder()
			dispatchEventTypes(st, rec, req(http.MethodPost, "/me/event-types", eventTypeBody(t, dto)))
			if rec.Code != http.StatusBadRequest && rec.Code != http.StatusForbidden {
				t.Fatalf("expected 4xx, got %d: %s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.want) {
				t.Fatalf("body %q missing %q", rec.Body.String(), tc.want)
			}
		})
	}
}

// ─── Update + Delete ──────────────────────────────────────────────────────────

func TestUpdateEventType_HappyPath(t *testing.T) {
	st := newMemStore()
	_, req := eventTypeFixture(t, st, "host@example.com")

	// Seed an event.
	rec := httptest.NewRecorder()
	dispatchEventTypes(st, rec, req(http.MethodPost, "/me/event-types", eventTypeBody(t, validFreeEventType())))
	if rec.Code != http.StatusCreated {
		t.Fatalf("seed: %d %s", rec.Code, rec.Body.String())
	}
	var created eventTypeDTO
	json.NewDecoder(rec.Body).Decode(&created)

	// Update title + duration.
	patched := created
	patched.Title = "New Title"
	patched.DurationMin = 60
	rec2 := httptest.NewRecorder()
	dispatchEventTypes(st, rec2, req(http.MethodPatch, "/me/event-types/"+created.ID, eventTypeBody(t, patched)))
	if rec2.Code != http.StatusOK {
		t.Fatalf("PATCH: %d %s", rec2.Code, rec2.Body.String())
	}
	var updated eventTypeDTO
	json.NewDecoder(rec2.Body).Decode(&updated)
	if updated.Title != "New Title" || updated.DurationMin != 60 {
		t.Fatalf("update not applied: %+v", updated)
	}
}

func TestUpdateEventType_OwnershipEnforced(t *testing.T) {
	st := newMemStore()
	// Host A creates an event.
	_, reqA := eventTypeFixture(t, st, "a@example.com")
	rec := httptest.NewRecorder()
	dispatchEventTypes(st, rec, reqA(http.MethodPost, "/me/event-types", eventTypeBody(t, validFreeEventType())))
	if rec.Code != http.StatusCreated {
		t.Fatalf("seed A: %d %s", rec.Code, rec.Body.String())
	}
	var created eventTypeDTO
	json.NewDecoder(rec.Body).Decode(&created)

	// Host B tries to PATCH it — should get 404, never 200/403 (we don't
	// leak existence to other hosts).
	_, reqB := eventTypeFixture(t, st, "b@example.com")
	patched := created
	patched.Title = "Hijacked"
	rec2 := httptest.NewRecorder()
	dispatchEventTypes(st, rec2, reqB(http.MethodPatch, "/me/event-types/"+created.ID, eventTypeBody(t, patched)))
	if rec2.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for cross-host PATCH, got %d: %s", rec2.Code, rec2.Body.String())
	}
}

func TestDeleteEventType_SoftDeletes(t *testing.T) {
	st := newMemStore()
	uid, req := eventTypeFixture(t, st, "host@example.com")

	// Seed an event.
	rec := httptest.NewRecorder()
	dispatchEventTypes(st, rec, req(http.MethodPost, "/me/event-types", eventTypeBody(t, validFreeEventType())))
	var created eventTypeDTO
	json.NewDecoder(rec.Body).Decode(&created)

	rec2 := httptest.NewRecorder()
	dispatchEventTypes(st, rec2, req(http.MethodDelete, "/me/event-types/"+created.ID, ""))
	if rec2.Code != http.StatusNoContent {
		t.Fatalf("DELETE: %d %s", rec2.Code, rec2.Body.String())
	}

	// The row must still exist (so future bookings keep their FK target) but
	// is_active=false; the active-count helper should now return 0.
	got, err := st.GetEventType(context.Background(), uid, created.ID)
	if err != nil {
		t.Fatalf("GetEventType after delete: %v", err)
	}
	if got.IsActive {
		t.Fatal("expected is_active=false after delete")
	}
	n, _ := st.CountActiveEventTypes(context.Background(), uid)
	if n != 0 {
		t.Fatalf("expected 0 active events after delete, got %d", n)
	}
}

func TestDeleteEventType_NotFound(t *testing.T) {
	st := newMemStore()
	_, req := eventTypeFixture(t, st, "host@example.com")

	rec := httptest.NewRecorder()
	dispatchEventTypes(st, rec, req(http.MethodDelete, "/me/event-types/no-such-id", ""))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

// ─── Routing helper ───────────────────────────────────────────────────────────

// dispatchEventTypes routes a /me/event-types or /me/event-types/{id} request
// to the appropriate handler. Tests use it instead of constructing a full mux
// so they only exercise the handler code being changed.
func dispatchEventTypes(st store.Storer, w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/me/event-types" {
		switch r.Method {
		case http.MethodGet:
			RequireAuth(testSecret, handleListEventTypes(st))(w, r)
		case http.MethodPost:
			RequireAuth(testSecret, handleCreateEventType(st))(w, r)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/me/event-types/")
	switch r.Method {
	case http.MethodPatch:
		RequireAuth(testSecret, handlePatchEventType(st, id))(w, r)
	case http.MethodDelete:
		RequireAuth(testSecret, handleDeleteEventType(st, id))(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}
