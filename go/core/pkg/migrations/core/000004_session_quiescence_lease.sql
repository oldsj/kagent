-- +goose Up
-- A claimed idle-boundary attempt holds its claim only until this lease ends.
-- NULL marks a claim made before leases existed; it is treated as expired.
ALTER TABLE session_task_event ADD COLUMN quiescence_expires_at TIMESTAMPTZ;

-- +goose Down
ALTER TABLE session_task_event DROP COLUMN quiescence_expires_at;
