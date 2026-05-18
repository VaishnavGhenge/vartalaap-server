-- +goose Up
-- Capture the reason supplied by the host or guest when a booking is cancelled.
-- It is nullable because existing and active bookings have not been cancelled.
ALTER TABLE bookings
    ADD COLUMN IF NOT EXISTS cancellation_reason text;

-- +goose Down
ALTER TABLE bookings
    DROP COLUMN IF EXISTS cancellation_reason;
