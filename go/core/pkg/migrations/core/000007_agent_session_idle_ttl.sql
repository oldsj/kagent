-- +goose Up
ALTER TABLE agent_definition ADD COLUMN session_idle_ttl_seconds BIGINT
    CHECK (session_idle_ttl_seconds >= 0);

-- +goose Down
ALTER TABLE agent_definition DROP COLUMN session_idle_ttl_seconds;
