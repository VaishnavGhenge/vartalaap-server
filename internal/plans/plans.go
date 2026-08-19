// Package plans is the single source of truth for subscription tier definitions.
// Adjust the table below to change limits, pricing, or feature access — no
// other file should hardcode plan-specific numbers.
package plans

// Plan name constants — the canonical string stored in users.plan.
const (
	Free  = "free"
	Solo  = "solo"
	Teams = "teams"
)

// Unlimited is the zero-value sentinel: a limit field set to Unlimited means
// no cap. Use Exceeds for comparisons so the zero case is handled correctly.
const Unlimited = 0

// Limits describes the full set of constraints and entitlements for one plan
// tier. Add new fields here whenever a new feature needs per-plan gating;
// the zero value for each type should be the "most restricted" default.
type Limits struct {
	// MonthlyBookings is the maximum non-cancelled bookings a host may
	// accumulate within a calendar month (counted by created_at, not
	// starts_at). Unlimited = no cap.
	MonthlyBookings int

	// ActiveEventTypes is the maximum simultaneously-active event types.
	// Unlimited = no cap.
	ActiveEventTypes int

	// PaidEvents controls whether the host may create event types with
	// IsPaid=true. Requires Solo or Teams (blocked until Phase 3 payments
	// land, but the gating logic is wired now).
	PaidEvents bool

	// PriceCentsMonthly is the monthly subscription price in USD cents.
	// Informational only — billing is not yet wired (see PRODUCT_ROADMAP.md
	// §Launch plan). Set to 0 for free tiers.
	PriceCentsMonthly int

	// PriceCentsAnnual is the annual subscription price in USD cents.
	// Solo: $108/yr (2 months free), Teams: $264/yr.
	PriceCentsAnnual int
}

// table is the authoritative per-tier configuration. All values come from
// PRODUCT_ROADMAP.md §Pricing tiers. Change here and nowhere else.
var table = map[string]Limits{
	Free: {
		MonthlyBookings:   10,
		ActiveEventTypes:  1,
		PaidEvents:        false,
		PriceCentsMonthly: 0,
		PriceCentsAnnual:  0,
	},
	Solo: {
		MonthlyBookings:   Unlimited,
		ActiveEventTypes:  Unlimited,
		PaidEvents:        true,
		PriceCentsMonthly: 1200,  // $12/mo
		PriceCentsAnnual:  10800, // $108/yr — 2 months free
	},
	Teams: {
		MonthlyBookings:   Unlimited,
		ActiveEventTypes:  Unlimited,
		PaidEvents:        true,
		PriceCentsMonthly: 2900,  // $29/mo
		PriceCentsAnnual:  26400, // $264/yr — 2 months free
	},
}

// For returns the Limits for the given plan name. Unknown plan strings
// fall back to Free limits so a malformed users.plan value fails closed
// (most restricted) rather than open (fewest restrictions).
func For(plan string) Limits {
	if l, ok := table[plan]; ok {
		return l
	}
	return table[Free]
}

// Exceeds reports whether count has met or passed the given limit.
// Always returns false when limit is Unlimited (0) — callers do not need
// to handle that case separately.
func Exceeds(limit, count int) bool {
	return limit != Unlimited && count >= limit
}
