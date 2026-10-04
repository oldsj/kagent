-- +goose Up

-- Git hosts a revision lets Sessions clone from. Session creation validates a
-- requested workspace against the pinned revision, so the list is immutable
-- with the rest of the revision inputs.
ALTER TABLE runtime_revision ADD COLUMN git_origins TEXT[] NOT NULL DEFAULT '{}';

-- +goose Down

ALTER TABLE runtime_revision DROP COLUMN git_origins;
