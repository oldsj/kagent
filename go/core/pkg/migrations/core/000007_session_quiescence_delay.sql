-- +goose Up
-- NULL preserves immediate eligibility for existing rows and older writers.
ALTER TABLE session_task_event ADD COLUMN quiescence_due_at TIMESTAMPTZ;

-- +goose Down
ALTER TABLE session_task_event DROP COLUMN quiescence_due_at;
