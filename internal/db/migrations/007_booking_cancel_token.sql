-- +goose Up
-- cancel_token gates DELETE /m/{code}. Distinct from meet_code so that the
-- room URL (which the host may share with a co-attendee) can't also be used
-- to cancel the booking — the token is only present in the confirmation
-- email and on the /m/{code} page when opened via the signed link.
--
-- Default uses gen_random_uuid()::text stripped of dashes so existing rows
-- get a usable token without an app-level backfill step. New rows get a
-- fresh token at INSERT time (the app passes it explicitly).
ALTER TABLE bookings
    ADD COLUMN IF NOT EXISTS cancel_token text NOT NULL
        DEFAULT replace(gen_random_uuid()::text, '-', '');

-- +goose Down
ALTER TABLE bookings DROP COLUMN IF EXISTS cancel_token;
