-- +goose Up
-- Record whether the host or guest initiated cancellation so both sides can
-- see the source of the cancellation alongside the reason.
ALTER TABLE bookings
    ADD COLUMN IF NOT EXISTS cancelled_by text
        CHECK (cancelled_by IS NULL OR cancelled_by IN ('host', 'guest'));

-- +goose Down
ALTER TABLE bookings
    DROP COLUMN IF EXISTS cancelled_by;
