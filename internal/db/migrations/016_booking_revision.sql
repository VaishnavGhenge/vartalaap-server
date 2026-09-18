-- +goose Up
ALTER TABLE bookings
    ADD COLUMN revision integer NOT NULL DEFAULT 0
    CHECK (revision >= 0);

-- +goose Down
ALTER TABLE bookings DROP COLUMN IF EXISTS revision;
