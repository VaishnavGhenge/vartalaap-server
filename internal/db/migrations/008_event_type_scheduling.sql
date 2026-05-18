-- +goose Up
-- Scheduling constraints for event types. All three default to 0 (no limit)
-- so existing rows behave identically until a host explicitly configures them.
ALTER TABLE event_types
    ADD COLUMN IF NOT EXISTS buffer_before_min int NOT NULL DEFAULT 0
        CHECK (buffer_before_min >= 0 AND buffer_before_min <= 120),
    ADD COLUMN IF NOT EXISTS min_notice_hours  int NOT NULL DEFAULT 0
        CHECK (min_notice_hours >= 0 AND min_notice_hours <= 336),
    ADD COLUMN IF NOT EXISTS max_days_ahead    int NOT NULL DEFAULT 0
        CHECK (max_days_ahead >= 0 AND max_days_ahead <= 365);

-- +goose Down
ALTER TABLE event_types
    DROP COLUMN IF EXISTS buffer_before_min,
    DROP COLUMN IF EXISTS min_notice_hours,
    DROP COLUMN IF EXISTS max_days_ahead;
