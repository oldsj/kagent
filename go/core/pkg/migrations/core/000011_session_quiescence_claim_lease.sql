-- +goose Up
-- An idle-suspension claim lasts as long as its worker's attempt. Afterwards
-- the worker has stopped issuing runtime work, so a forced delete may supersede
-- the claim. It still blocks new turns and other lifecycle work.
ALTER TABLE session_task_event ADD COLUMN quiescence_executor_expires_at TIMESTAMPTZ;
-- Earlier claims had no bound. Give an attempt that may still be running the
-- full default interval (DefaultRuntimeOperationTimeout).
UPDATE session_task_event SET quiescence_executor_expires_at = NOW() + INTERVAL '10 minutes'
WHERE quiescence_executor_id IS NOT NULL;
-- One-directional, so an older binary that claims without an expiry still
-- works on this schema after a rollback. Its claims never expire, as before.
ALTER TABLE session_task_event ADD CONSTRAINT session_task_event_quiescence_lease
    CHECK (quiescence_executor_expires_at IS NULL OR quiescence_executor_id IS NOT NULL);

-- +goose Down
ALTER TABLE session_task_event DROP CONSTRAINT session_task_event_quiescence_lease;
ALTER TABLE session_task_event DROP COLUMN quiescence_executor_expires_at;
