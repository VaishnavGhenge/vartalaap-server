-- +goose Up
-- Phase 2: scheduling core. This migration ships only what `/me/availability`
-- needs end-to-end; event_types and bookings follow in a later migration so
-- each shippable slice stays reviewable.
--
-- Plan tier on users so server-side enforcement of free/solo/teams limits is
-- straightforward to add (per PRODUCT_ROADMAP §Pricing tiers). Default 'free'
-- is the safe state for existing users — none of them are paying yet.
ALTER TABLE users
    ADD COLUMN IF NOT EXISTS plan text NOT NULL DEFAULT 'free'
        CHECK (plan IN ('free', 'solo', 'teams'));

-- One row per recurring weekly availability slot. day_of_week uses ISO-like
-- 0=Sunday..6=Saturday to match JS Date.getDay() and so the UI doesn't need
-- a translation step. timezone is stored per-row so changes to users.timezone
-- don't silently shift saved availability — explicit beats implicit here.
CREATE TABLE IF NOT EXISTS availability_rules (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    host_id     uuid        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    day_of_week int         NOT NULL CHECK (day_of_week BETWEEN 0 AND 6),
    start_time  time        NOT NULL,
    end_time    time        NOT NULL,
    timezone    text        NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    CHECK (end_time > start_time)
);

-- The hot read pattern is "list all rules for a host, ordered by day + start" —
-- index host_id and let the planner sort the small per-host result set.
CREATE INDEX IF NOT EXISTS availability_rules_host_id_idx
    ON availability_rules(host_id);

-- +goose Down
DROP INDEX IF EXISTS availability_rules_host_id_idx;
DROP TABLE IF EXISTS availability_rules;
ALTER TABLE users DROP COLUMN IF EXISTS plan;
