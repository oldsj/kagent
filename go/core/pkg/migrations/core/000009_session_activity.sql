-- +goose Up
-- Runtime activity outside A2A tasks, such as an attached client. One row per
-- Session whose runtime reports activity; runtimes that never report have none.
CREATE TABLE session_activity (
    session_id   UUID        PRIMARY KEY REFERENCES session(id) ON DELETE CASCADE,
    -- The latest report's lease end; an idle report ends it immediately.
    active_until TIMESTAMPTZ NOT NULL,
    -- When idle suspension may claim the boundary: lease end plus quiescence delay.
    idle_due_at  TIMESTAMPTZ NOT NULL,
    -- Database time of the latest report; extends the idle expiration clock.
    reported_at  TIMESTAMPTZ NOT NULL,
    -- The runtime was observed running since its last idle suspension.
    pending      BOOLEAN     NOT NULL,
    -- Counts reports, so a claim cannot clear a boundary reported after it.
    report_seq   BIGINT      NOT NULL CHECK (report_seq > 0),
    -- An idle suspension claim. It fences new execution and, like task-boundary
    -- claims, never expires: only finishing it or retiring idle work clears it.
    executor_id  UUID,
    claimed_seq  BIGINT,
    CHECK ((executor_id IS NULL) = (claimed_seq IS NULL)),
    CHECK (executor_id IS NULL OR pending)
);
CREATE INDEX session_activity_due_idx ON session_activity (idle_due_at) WHERE pending;

-- +goose Down
DROP TABLE session_activity;
