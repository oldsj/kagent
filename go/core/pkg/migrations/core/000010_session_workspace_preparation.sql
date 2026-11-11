-- +goose Up

-- One workspace preparation per Session. The request and assignment bytes are
-- immutable once recorded: a retry with other bytes is a conflict, and the
-- runtime can only complete the exact assignment it was issued.
CREATE TABLE session_workspace_preparation (
    session_id         UUID        PRIMARY KEY REFERENCES runtime_instance(id) ON DELETE CASCADE,
    action_id          TEXT        NOT NULL CHECK (length(action_id) BETWEEN 1 AND 128),
    request_digest     TEXT        NOT NULL CHECK (request_digest ~ '^[0-9a-f]{64}$'),
    request_data       BYTEA       NOT NULL CHECK (octet_length(request_data) BETWEEN 1 AND 8192),
    execution_id       UUID        NOT NULL UNIQUE,
    assignment_data    BYTEA       NOT NULL CHECK (octet_length(assignment_data) BETWEEN 1 AND 12288),
    phase              TEXT        NOT NULL CHECK (phase IN ('pending', 'confirmed', 'definite-failure')),
    -- Set when the runtime first reads the assignment; never cleared.
    issued_at          TIMESTAMPTZ,
    -- The runtime's completion report, kept byte-for-byte.
    result_data        BYTEA       CHECK (octet_length(result_data) BETWEEN 1 AND 16384),
    effect_observed_at TIMESTAMPTZ,
    settled_at         TIMESTAMPTZ,
    CONSTRAINT session_workspace_preparation_settled CHECK (
        (phase = 'pending') = (result_data IS NULL)
        AND (result_data IS NULL) = (settled_at IS NULL)
        AND (result_data IS NULL OR issued_at IS NOT NULL)
        AND (phase = 'confirmed') = (effect_observed_at IS NOT NULL))
);

-- +goose Down

DROP TABLE session_workspace_preparation;
