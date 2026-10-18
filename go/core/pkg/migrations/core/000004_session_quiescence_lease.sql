-- +goose Up
-- A claimed idle-boundary attempt holds its claim only until this lease ends.
-- NULL marks a claim made before leases existed; it is treated as expired.
ALTER TABLE session_task_event ADD COLUMN quiescence_expires_at TIMESTAMPTZ;
ALTER TABLE session_task_event ADD CONSTRAINT session_task_event_quiescence_lease_claimed
    CHECK (quiescence_expires_at IS NULL OR quiescence_executor_id IS NOT NULL);

-- +goose Down
ALTER TABLE session_task_event DROP CONSTRAINT session_task_event_quiescence_lease_claimed;
ALTER TABLE session_task_event DROP COLUMN quiescence_expires_at;
