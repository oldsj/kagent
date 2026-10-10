-- +goose Up
ALTER TABLE session_task_event ADD COLUMN quiescence_due_at TIMESTAMPTZ;

-- Existing settled boundaries keep immediate eligibility, including issued work.
UPDATE session_task_event SET quiescence_due_at = created_at
WHERE published AND quiescence_pending IS NOT NULL;

ALTER TABLE session_task_event ADD CONSTRAINT session_task_event_quiescence_due_check
    CHECK ((quiescence_due_at IS NOT NULL) = (published AND quiescence_pending IS NOT NULL));

-- +goose Down
ALTER TABLE session_task_event DROP CONSTRAINT session_task_event_quiescence_due_check;
ALTER TABLE session_task_event DROP COLUMN quiescence_due_at;
