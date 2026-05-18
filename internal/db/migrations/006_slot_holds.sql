-- +goose Up
-- Short-lived reservations that block a slot while a guest is filling out
-- the booking form. Without these, two guests with the picker open at the
-- same time can both submit for the same slot and one of them gets a 409
-- only at submit — bad UX for the loser. With holds, the slot disappears
-- from the picker as soon as the first guest taps it.
--
-- Holds are scoped by host (NOT by event_type) because a host who exposes
-- multiple event types — say a 30-min "intro" and a 60-min "deep dive" —
-- can't take both at the same time. A hold on the 30-min intro at 09:00
-- must block the 09:00 slot in the 60-min picker too.
--
-- expires_at is the wall-clock TTL. Eviction is opportunistic: any read
-- query filters `expires_at > now()` so a missed cleanup never serves a
-- stale hold. A periodic DELETE could run later if the table ever grows
-- large enough to matter; at expected volumes (hundreds of holds at peak)
-- it doesn't.
--
-- hold_token is the opaque identifier the client uses to release the hold
-- (DELETE /holds/:token) and to claim it when submitting a booking. We
-- index it UNIQUE so the lookup is point-read.
CREATE TABLE IF NOT EXISTS slot_holds (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    host_id       uuid        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    event_type_id uuid        NOT NULL REFERENCES event_types(id) ON DELETE CASCADE,
    starts_at     timestamptz NOT NULL,
    ends_at       timestamptz NOT NULL,
    hold_token    text        NOT NULL UNIQUE,
    expires_at    timestamptz NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    CHECK (ends_at > starts_at),
    CHECK (expires_at > created_at)
);

-- Cross-event lookup: "is any active hold on this host's calendar inside
-- [from, to]?" — drives the slot picker's blocker list. The index covers
-- the host + range query without scanning the full table.
CREATE INDEX IF NOT EXISTS slot_holds_host_starts_idx
    ON slot_holds(host_id, starts_at);

-- TTL sweep: filtering on expires_at directly lets a periodic cleanup query
-- (when we add one) range-scan cheaply.
CREATE INDEX IF NOT EXISTS slot_holds_expires_idx
    ON slot_holds(expires_at);

-- +goose Down
DROP INDEX IF EXISTS slot_holds_expires_idx;
DROP INDEX IF EXISTS slot_holds_host_starts_idx;
DROP TABLE IF EXISTS slot_holds;
