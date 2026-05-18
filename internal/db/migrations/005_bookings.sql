-- +goose Up
-- One row per confirmed (or pending) booking. host_id is denormalised from
-- event_types so the dashboard's "my upcoming bookings" query doesn't need a
-- join — it's the hot path for hosts and the redundancy is cheap.
--
-- meet_code is generated server-side (independent of the existing /meets/new
-- code generator) and is the URL fragment guests use to join the call. It is
-- UNIQUE so a guess can't land on a real booking.
--
-- starts_at/ends_at are stored as `timestamptz` — the application converts
-- from the host's availability_rules timezone before insert. Storing in UTC
-- keeps comparisons unambiguous when guests in different timezones list slots.
--
-- stripe_session_id is nullable now and only populated once Phase 3 wires
-- Stripe Checkout. Including the column today keeps the migration order
-- linear when payments land.
CREATE TABLE IF NOT EXISTS bookings (
    id                uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    event_type_id     uuid        NOT NULL REFERENCES event_types(id),
    host_id           uuid        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    guest_email       text        NOT NULL,
    guest_name        text        NOT NULL,
    starts_at         timestamptz NOT NULL,
    ends_at           timestamptz NOT NULL,
    meet_code         text        NOT NULL UNIQUE,
    status            text        NOT NULL DEFAULT 'confirmed'
                                  CHECK (status IN ('pending_payment','confirmed','cancelled','completed')),
    stripe_session_id text,
    created_at        timestamptz NOT NULL DEFAULT now(),
    CHECK (ends_at > starts_at)
);

-- Host dashboard: "my upcoming bookings, soonest first" filtered by status.
CREATE INDEX IF NOT EXISTS bookings_host_starts_idx
    ON bookings(host_id, starts_at);

-- Slot generation: "is this event already booked at time T?" — needs to
-- filter by event_type_id and time range cheaply.
CREATE INDEX IF NOT EXISTS bookings_event_starts_idx
    ON bookings(event_type_id, starts_at);

-- +goose Down
DROP INDEX IF EXISTS bookings_event_starts_idx;
DROP INDEX IF EXISTS bookings_host_starts_idx;
DROP TABLE IF EXISTS bookings;
