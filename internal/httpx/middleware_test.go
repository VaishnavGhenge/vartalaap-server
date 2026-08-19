package httpx

import "testing"

// routeLabel feeds a Prometheus label, so it has two jobs that pull against
// each other: name every real route (or its latency and errors vanish into a
// shared "/other" bucket) while never minting a label from attacker-controlled
// path text (or cardinality explodes and the metric becomes unusable).
func TestRouteLabel(t *testing.T) {
	cases := map[string]string{
		// Calendar. All static despite the prefix-based mux registration.
		"/me/calendar/status":          "/me/calendar/status",
		"/me/calendar/connect/google":  "/me/calendar/connect/google",
		"/me/calendar/callback/google": "/me/calendar/callback/google",
		"/me/calendar/disconnect":      "/me/calendar/disconnect",

		// Previously unlabelled and sharing the "/other" bucket with genuine
		// junk, which made that bucket unreadable.
		"/auth/guest":  "/auth/guest",
		"/room/status": "/room/status",

		// Existing statics, pinned so a future edit to the switch can't drop one.
		"/healthz":         "/healthz",
		"/auth/login":      "/auth/login",
		"/me/availability": "/me/availability",
		"/bookings":        "/bookings",
		"/holds":           "/holds",

		// Dynamic segments must normalise, not pass through.
		"/me/event-types/6f1c":            "/me/event-types/:id",
		"/bookings/abc-123":               "/bookings/:id",
		"/holds/tok_9":                    "/holds/:token",
		"/m/abc-defg-hij":                 "/m/:code",
		"/u/pat":                          "/u/:slug",
		"/u/pat/intro":                    "/u/:slug/:event",
		"/u/pat/intro/slots":              "/u/:slug/:event/slots",
		"/sfu/sessions/new":               "/sfu/sessions/new",
		"/sfu/sessions/sess-1":            "/sfu/sessions/:id",
		"/sfu/sessions/sess-1/tracks/new": "/sfu/sessions/:id/tracks/new",

		// Unknown paths, including ones shaped like real routes, stay in the
		// catch-all. An unrecognised calendar action is a 404, not a route.
		"/me/calendar/garbage": "/other",
		"/me/calendar/":        "/other",
		"/nope":                "/other",
		"/u/pat/intro/nope":    "/other",
	}
	for path, want := range cases {
		if got := routeLabel(path); got != want {
			t.Errorf("routeLabel(%q) = %q, want %q", path, got, want)
		}
	}
}

// A label derived from caller-controlled text is a cardinality bomb: enough
// distinct paths and the metrics backend falls over. Every unmatched shape
// must collapse to exactly one label.
func TestRouteLabelBoundsCardinality(t *testing.T) {
	seen := map[string]bool{}
	for _, p := range []string{
		"/random/1", "/random/2", "/attack/" + string(make([]byte, 200)),
		"/me/calendar/aaa", "/me/calendar/bbb", "/wp-admin", "/.env",
	} {
		seen[routeLabel(p)] = true
	}
	if len(seen) != 1 || !seen["/other"] {
		t.Fatalf("unmatched paths produced %d labels, want exactly {\"/other\"}: %v", len(seen), seen)
	}
}
