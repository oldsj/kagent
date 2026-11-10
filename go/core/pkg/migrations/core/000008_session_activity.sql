-- +goose Up
-- Runtime activity outside A2A tasks, such as an attached client. One row per
-- Session whose runtime reports activity; runtimes that never report have none.
CREATE TABLE session_activity (
    session_id       UUID        PRIMARY KEY REFERENCES session(id) ON DELETE CASCADE,
    -- The latest report's lease end; an idle report ends it immediately.
    active_until     TIMESTAMPTZ NOT NULL,
    -- When idle quiescence may claim the boundary: lease end plus quiesce delay.
    idle_due_at      TIMESTAMPTZ NOT NULL,
    -- Database time of the latest report; extends the idle expiration clock.
    reported_at      TIMESTAMPTZ NOT NULL,
    -- The runtime was observed running since its last idle quiescence.
    pending          BOOLEAN     NOT NULL,
    -- Counts reports, so a claim cannot clear a boundary reported after it.
    report_seq       BIGINT      NOT NULL CHECK (report_seq > 0),
    -- An idle quiescence claim and its lease. A claim fences new execution.
    executor_id      UUID,
    claim_expires_at TIMESTAMPTZ,
    claimed_seq      BIGINT,
    CHECK ((executor_id IS NULL) = (claim_expires_at IS NULL)),
    CHECK ((executor_id IS NULL) = (claimed_seq IS NULL)),
    CHECK (executor_id IS NULL OR pending)
);
CREATE INDEX session_activity_due_idx ON session_activity (idle_due_at) WHERE pending;

-- +goose Down
DROP TABLE session_activity;
