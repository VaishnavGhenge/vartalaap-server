-- +goose Up
-- One row per bookable "event type" (consult, intro call, paid coaching, etc).
-- Slug is unique per host so a host's URLs (e.g. /u/{host}/{event}) never collide
-- with their own other events, but two hosts can both have "intro-call".
--
-- is_paid + price_cents + currency + payment_timing exist now so the schema
-- doesn't need migration when Phase 3 wires Stripe Connect — the handler in
-- this slice keeps every event free until the plan-gating arrives.
CREATE TABLE IF NOT EXISTS event_types (
    id             uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    host_id        uuid        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    slug           text        NOT NULL,
    title          text        NOT NULL,
    duration_min   int         NOT NULL CHECK (duration_min IN (15, 30, 45, 60, 90, 120)),
    buffer_min     int         NOT NULL DEFAULT 0 CHECK (buffer_min >= 0 AND buffer_min <= 120),
    max_per_day    int                  CHECK (max_per_day IS NULL OR max_per_day > 0),
    is_paid        boolean     NOT NULL DEFAULT false,
    price_cents    int                  CHECK (price_cents IS NULL OR price_cents >= 0),
    currency       text        NOT NULL DEFAULT 'usd',
    -- Whether the guest pays at booking time or after the session. Free events
    -- ignore this; we still require the value to be coherent so the planner
    -- has fewer null cases.
    payment_timing text        NOT NULL DEFAULT 'upfront' CHECK (payment_timing IN ('upfront', 'after')),
    is_active      boolean     NOT NULL DEFAULT true,
    description    text,
    created_at     timestamptz NOT NULL DEFAULT now(),
    UNIQUE (host_id, slug),
    -- Paid events must carry a price; free events must not.
    CHECK ((is_paid = true  AND price_cents IS NOT NULL AND price_cents > 0)
        OR (is_paid = false AND price_cents IS NULL))
);

-- Hot path: list a host's active event types (for /u/{slug} and the dashboard).
CREATE INDEX IF NOT EXISTS event_types_host_active_idx
    ON event_types(host_id, is_active);

-- +goose Down
DROP INDEX IF EXISTS event_types_host_active_idx;
DROP TABLE IF EXISTS event_types;
