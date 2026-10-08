-- +goose Up
-- The Session row remains the single operation/admission lock. These immutable
-- action bytes supplement its current lifecycle owner; they never expire.
CREATE TABLE session_native_preparation (
    session_id uuid PRIMARY KEY,
    owner_operation_id uuid NOT NULL,
    action_id text NOT NULL CHECK (length(action_id) BETWEEN 1 AND 128),
    request_digest text NOT NULL CHECK (request_digest ~ '^[0-9a-f]{64}$'),
    execution_id uuid NOT NULL UNIQUE,
    request_data bytea NOT NULL CHECK (octet_length(request_data) BETWEEN 1 AND 8192),
    assignment_data bytea NOT NULL CHECK (octet_length(assignment_data) BETWEEN 1 AND 12288),
    phase text NOT NULL CHECK (phase IN ('pending', 'uncertain', 'confirmed', 'definite-failure')),
    challenge_id uuid NOT NULL UNIQUE,
    observation_sequence bigint NOT NULL CHECK (observation_sequence BETWEEN 0 AND 65535),
    issued_at timestamptz,
    result_data bytea CHECK (octet_length(result_data) BETWEEN 1 AND 16384),
    effect_data bytea CHECK (octet_length(effect_data) BETWEEN 1 AND 16384),
    receipt_data bytea NOT NULL CHECK (octet_length(receipt_data) BETWEEN 1 AND 16384),
    current_valid boolean NOT NULL DEFAULT false,
    CHECK (NOT current_valid OR (phase = 'confirmed' AND issued_at IS NOT NULL AND result_data IS NOT NULL AND effect_data IS NOT NULL)),
    CHECK (phase <> 'uncertain' OR issued_at IS NOT NULL),
    CHECK (phase NOT IN ('confirmed', 'definite-failure') OR result_data IS NOT NULL)
);

-- +goose Down
DROP TABLE session_native_preparation;
