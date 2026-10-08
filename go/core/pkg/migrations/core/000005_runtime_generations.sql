-- +goose Up
ALTER TABLE session ADD COLUMN runtime_generation_eligible boolean NOT NULL DEFAULT false;

-- No Session foreign key: retired names and credential URIs outlive deletion.
CREATE TABLE runtime_generation (
    id uuid PRIMARY KEY,
    session_id uuid NOT NULL,
    atespace text NOT NULL CHECK (atespace <> ''),
    actor_name text NOT NULL UNIQUE CHECK (actor_name ~ '^session-[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}-[0-9a-f]{16}$' AND left(actor_name, 44) = 'session-' || session_id::text),
    credential_uri text NOT NULL UNIQUE,
    token_digest bytea NOT NULL UNIQUE CHECK (octet_length(token_digest) = 32),
    actor_uid text NOT NULL DEFAULT '',
    phase text NOT NULL CHECK (phase IN ('allocated', 'secret-issued', 'actor-issued', 'bound', 'active', 'revoked')),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    CHECK (phase NOT IN ('bound', 'active') OR actor_uid <> '')
);
CREATE UNIQUE INDEX runtime_generation_current ON runtime_generation(session_id) WHERE phase <> 'revoked';

-- +goose Down
DROP TABLE runtime_generation;
ALTER TABLE session DROP COLUMN runtime_generation_eligible;
