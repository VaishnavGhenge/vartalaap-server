package httpx

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Builds an authed PUT /me/availability request body containing the given DTOs
// without forcing each test to handwrite the JSON envelope.
func availabilityBody(t *testing.T, rules []availabilityRuleDTO) string {
	t.Helper()
	b, err := json.Marshal(availabilityResponse{Rules: rules})
	if err != nil {
		t.Fatalf("marshal availability body: %v", err)
	}
	return string(b)
}

// availabilityFixture registers exactly one host and returns a callback that
// builds authed /me/availability requests for that host. Tests that need a
// second host can call it again with a different email.
func availabilityFixture(t *testing.T, st *memStore, email string) func(method, body string) *http.Request {
	t.Helper()
	registerUser(t, st, email, "password123")
	u, _ := st.GetUserByEmail(context.Background(), email)
	tok := tokenForUser(t, u.ID)
	return func(method, body string) *http.Request {
		req := authReq(method, "/me/availability", body)
		req.Header.Set("Authorization", "Bearer "+tok)
		return req
	}
}

func TestAvailabilityRoundTrip(t *testing.T) {
	st := newMemStore()
	req := availabilityFixture(t, st, "host@example.com")
	body := availabilityBody(t, []availabilityRuleDTO{
		{DayOfWeek: 1, StartTime: "09:00", EndTime: "12:00", Timezone: "America/New_York"},
		{DayOfWeek: 1, StartTime: "13:00", EndTime: "17:00", Timezone: "America/New_York"},
		{DayOfWeek: 3, StartTime: "10:00", EndTime: "14:30", Timezone: "America/New_York"},
	})

	putRec := httptest.NewRecorder()
	RequireAuth(testSecret, handlePutAvailability(st))(putRec, req(http.MethodPut, body))
	if putRec.Code != http.StatusOK {
		t.Fatalf("PUT expected 200, got %d: %s", putRec.Code, putRec.Body.String())
	}
	var putResp availabilityResponse
	if err := json.NewDecoder(putRec.Body).Decode(&putResp); err != nil {
		t.Fatalf("decode PUT response: %v", err)
	}
	if len(putResp.Rules) != 3 {
		t.Fatalf("expected 3 saved rules, got %d", len(putResp.Rules))
	}
	for _, r := range putResp.Rules {
		if r.ID == "" {
			t.Fatalf("expected generated id on saved rule, got empty")
		}
	}

	getRec := httptest.NewRecorder()
	RequireAuth(testSecret, handleGetAvailability(st))(getRec, req(http.MethodGet, ""))
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET expected 200, got %d: %s", getRec.Code, getRec.Body.String())
	}
	var getResp availabilityResponse
	json.NewDecoder(getRec.Body).Decode(&getResp)
	if len(getResp.Rules) != 3 {
		t.Fatalf("expected 3 rules after GET, got %d", len(getResp.Rules))
	}
	if getResp.Rules[0].DayOfWeek != 1 || getResp.Rules[0].StartTime != "09:00" {
		t.Fatalf("expected first rule Mon 09:00, got %+v", getResp.Rules[0])
	}
	if getResp.Rules[2].DayOfWeek != 3 {
		t.Fatalf("expected last rule Wed, got %+v", getResp.Rules[2])
	}
}

func TestAvailabilityReplacesExisting(t *testing.T) {
	st := newMemStore()
	req := availabilityFixture(t, st, "host@example.com")

	first := availabilityBody(t, []availabilityRuleDTO{
		{DayOfWeek: 0, StartTime: "08:00", EndTime: "09:00", Timezone: "UTC"},
		{DayOfWeek: 1, StartTime: "08:00", EndTime: "09:00", Timezone: "UTC"},
	})
	rec1 := httptest.NewRecorder()
	RequireAuth(testSecret, handlePutAvailability(st))(rec1, req(http.MethodPut, first))
	if rec1.Code != http.StatusOK {
		t.Fatalf("first PUT failed: %d %s", rec1.Code, rec1.Body.String())
	}

	second := availabilityBody(t, []availabilityRuleDTO{
		{DayOfWeek: 5, StartTime: "15:00", EndTime: "18:00", Timezone: "UTC"},
	})
	rec2 := httptest.NewRecorder()
	RequireAuth(testSecret, handlePutAvailability(st))(rec2, req(http.MethodPut, second))
	if rec2.Code != http.StatusOK {
		t.Fatalf("second PUT failed: %d %s", rec2.Code, rec2.Body.String())
	}

	// GET must show ONLY the second set — the first must be gone.
	grec := httptest.NewRecorder()
	RequireAuth(testSecret, handleGetAvailability(st))(grec, req(http.MethodGet, ""))
	var resp availabilityResponse
	json.NewDecoder(grec.Body).Decode(&resp)
	if len(resp.Rules) != 1 {
		t.Fatalf("expected exactly 1 rule after replace, got %d: %+v", len(resp.Rules), resp.Rules)
	}
	if resp.Rules[0].DayOfWeek != 5 {
		t.Fatalf("expected the new Friday rule, got %+v", resp.Rules[0])
	}
}

func TestAvailabilityValidationErrors(t *testing.T) {
	cases := []struct {
		name string
		rule availabilityRuleDTO
		want string
	}{
		{
			name: "day out of range high",
			rule: availabilityRuleDTO{DayOfWeek: 7, StartTime: "09:00", EndTime: "10:00", Timezone: "UTC"},
			want: "dayOfWeek",
		},
		{
			name: "day out of range low",
			rule: availabilityRuleDTO{DayOfWeek: -1, StartTime: "09:00", EndTime: "10:00", Timezone: "UTC"},
			want: "dayOfWeek",
		},
		{
			name: "bad start time",
			rule: availabilityRuleDTO{DayOfWeek: 1, StartTime: "9am", EndTime: "10:00", Timezone: "UTC"},
			want: "startTime",
		},
		{
			name: "bad end time",
			rule: availabilityRuleDTO{DayOfWeek: 1, StartTime: "09:00", EndTime: "10pm", Timezone: "UTC"},
			want: "endTime",
		},
		{
			name: "end before start",
			rule: availabilityRuleDTO{DayOfWeek: 1, StartTime: "10:00", EndTime: "09:00", Timezone: "UTC"},
			want: "endTime must be after",
		},
		{
			name: "end equals start",
			rule: availabilityRuleDTO{DayOfWeek: 1, StartTime: "10:00", EndTime: "10:00", Timezone: "UTC"},
			want: "endTime must be after",
		},
		{
			name: "missing timezone",
			rule: availabilityRuleDTO{DayOfWeek: 1, StartTime: "09:00", EndTime: "10:00", Timezone: "   "},
			want: "timezone",
		},
		{
			name: "unknown timezone",
			rule: availabilityRuleDTO{DayOfWeek: 1, StartTime: "09:00", EndTime: "10:00", Timezone: "Mars/Olympus"},
			want: "IANA",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newMemStore()
			req := availabilityFixture(t, st, "host@example.com")
			body := availabilityBody(t, []availabilityRuleDTO{tc.rule})
			rec := httptest.NewRecorder()
			RequireAuth(testSecret, handlePutAvailability(st))(rec, req(http.MethodPut, body))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.want) {
				t.Fatalf("body %q missing expected substring %q", rec.Body.String(), tc.want)
			}
		})
	}
}

func TestAvailabilityTooManyRules(t *testing.T) {
	st := newMemStore()
	req := availabilityFixture(t, st, "host@example.com")
	rules := make([]availabilityRuleDTO, maxAvailabilityRules+1)
	for i := range rules {
		rules[i] = availabilityRuleDTO{
			DayOfWeek: i % 7,
			StartTime: "09:00",
			EndTime:   "10:00",
			Timezone:  "UTC",
		}
	}
	rec := httptest.NewRecorder()
	RequireAuth(testSecret, handlePutAvailability(st))(rec, req(http.MethodPut, availabilityBody(t, rules)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestAvailabilityRequiresAuth(t *testing.T) {
	st := newMemStore()
	req := authReq(http.MethodGet, "/me/availability", "")
	rec := httptest.NewRecorder()
	RequireAuth(testSecret, handleGetAvailability(st))(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestAvailabilityIsolatedPerHost(t *testing.T) {
	st := newMemStore()

	// Host A writes two rules.
	registerUser(t, st, "a@example.com", "password123")
	uA, _ := st.GetUserByEmail(context.Background(), "a@example.com")
	tokA := tokenForUser(t, uA.ID)
	bodyA := availabilityBody(t, []availabilityRuleDTO{
		{DayOfWeek: 1, StartTime: "08:00", EndTime: "09:00", Timezone: "UTC"},
		{DayOfWeek: 2, StartTime: "08:00", EndTime: "09:00", Timezone: "UTC"},
	})
	rA := authReq(http.MethodPut, "/me/availability", bodyA)
	rA.Header.Set("Authorization", "Bearer "+tokA)
	recA := httptest.NewRecorder()
	RequireAuth(testSecret, handlePutAvailability(st))(recA, rA)
	if recA.Code != http.StatusOK {
		t.Fatalf("host A PUT failed: %d %s", recA.Code, recA.Body.String())
	}

	// Host B writes one rule on a different day.
	registerUser(t, st, "b@example.com", "password123")
	uB, _ := st.GetUserByEmail(context.Background(), "b@example.com")
	tokB := tokenForUser(t, uB.ID)
	bodyB := availabilityBody(t, []availabilityRuleDTO{
		{DayOfWeek: 6, StartTime: "20:00", EndTime: "22:00", Timezone: "UTC"},
	})
	rB := authReq(http.MethodPut, "/me/availability", bodyB)
	rB.Header.Set("Authorization", "Bearer "+tokB)
	recB := httptest.NewRecorder()
	RequireAuth(testSecret, handlePutAvailability(st))(recB, rB)
	if recB.Code != http.StatusOK {
		t.Fatalf("host B PUT failed: %d %s", recB.Code, recB.Body.String())
	}

	// Host A still sees its own two rules — B's write must not have leaked over.
	gA := authReq(http.MethodGet, "/me/availability", "")
	gA.Header.Set("Authorization", "Bearer "+tokA)
	grecA := httptest.NewRecorder()
	RequireAuth(testSecret, handleGetAvailability(st))(grecA, gA)
	var respA availabilityResponse
	json.NewDecoder(grecA.Body).Decode(&respA)
	if len(respA.Rules) != 2 {
		t.Fatalf("host A expected 2 rules, got %d", len(respA.Rules))
	}
}
