package plans_test

import (
	"testing"

	"github.com/vaishnavghenge/vartalaap-server/internal/plans"
)

// The plans package is the single source of truth for subscription tier limits,
// and its tables are quoted in PRODUCT_ROADMAP.md as the product contract.
// These tests pin: (1) every documented limit value so silent drift between
// the table and the roadmap shows up; (2) the security-critical
// fail-closed-on-unknown behaviour; (3) the Exceeds half-open boundary.

func TestFor_KnownPlansReturnDocumentedLimits(t *testing.T) {
	tests := []struct {
		name              string
		plan              string
		monthlyBookings   int
		activeEventTypes  int
		paidEvents        bool
		priceCentsMonthly int
	}{
		{"free", plans.Free, 10, 1, false, 0},
		{"solo", plans.Solo, plans.Unlimited, plans.Unlimited, true, 1200},
		{"teams", plans.Teams, plans.Unlimited, plans.Unlimited, true, 2900},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := plans.For(tc.plan)
			if got.MonthlyBookings != tc.monthlyBookings {
				t.Errorf("MonthlyBookings = %d, want %d", got.MonthlyBookings, tc.monthlyBookings)
			}
			if got.ActiveEventTypes != tc.activeEventTypes {
				t.Errorf("ActiveEventTypes = %d, want %d", got.ActiveEventTypes, tc.activeEventTypes)
			}
			if got.PaidEvents != tc.paidEvents {
				t.Errorf("PaidEvents = %v, want %v", got.PaidEvents, tc.paidEvents)
			}
			if got.PriceCentsMonthly != tc.priceCentsMonthly {
				t.Errorf("PriceCentsMonthly = %d, want %d", got.PriceCentsMonthly, tc.priceCentsMonthly)
			}
		})
	}
}

// Unknown plan strings must "fail closed" — return the most-restricted (Free)
// limits, not the most-permissive ones. A malformed users.plan column value
// must NEVER grant Solo/Teams access. The comment in plans.go calls this out
// explicitly; this test pins it against silent regression.
func TestFor_UnknownPlanFallsBackToFree(t *testing.T) {
	free := plans.For(plans.Free)
	cases := []string{"", "garbage-tier", "TEAMS", "  solo ", "premium"}
	for _, plan := range cases {
		t.Run(plan, func(t *testing.T) {
			got := plans.For(plan)
			if got != free {
				t.Errorf("unknown plan %q returned %+v, want free %+v — fail-closed contract broken",
					plan, got, free)
			}
		})
	}
}

// Exceeds is the half-open boundary check used in the booking-creation path
// to enforce monthly caps. It must return true when count has MET the limit
// (e.g. limit=10, count=10 means the 11th booking would exceed the cap).
// A subtle off-by-one here would let one extra booking through per month per
// free host — invisible at low volume, expensive at scale.
func TestExceeds_BoundaryBehavior(t *testing.T) {
	tests := []struct {
		limit, count int
		want         bool
		reason       string
	}{
		{10, 9, false, "9 of 10 — still allowed"},
		{10, 10, true, "AT the limit — next booking would exceed (off-by-one guard)"},
		{10, 11, true, "over the limit"},
		{1, 0, false, "haven't used any yet"},
		{1, 1, true, "single-event Free tier consumed its one slot"},
	}
	for _, tc := range tests {
		t.Run(tc.reason, func(t *testing.T) {
			if got := plans.Exceeds(tc.limit, tc.count); got != tc.want {
				t.Errorf("Exceeds(limit=%d, count=%d) = %v, want %v", tc.limit, tc.count, got, tc.want)
			}
		})
	}
}

// The Unlimited sentinel (zero value 0) means "no cap". Without the explicit
// guard `limit != Unlimited`, a paid host (Solo/Teams) with MonthlyBookings=0
// would be reported as exceeding their cap on their first booking — the
// callers don't special-case Unlimited; plans.Exceeds is the gate.
func TestExceeds_UnlimitedNeverExceeded(t *testing.T) {
	for _, count := range []int{0, 1, 100, 1_000_000} {
		if plans.Exceeds(plans.Unlimited, count) {
			t.Errorf("Exceeds(Unlimited, %d) = true; Unlimited must NEVER exceed", count)
		}
	}
}
