-- +goose Up

-- Native revisions that use the Git proxy profile, or no Git at all, must not
-- gain GitHub or Git proxy access from EgressPolicy grants applied after
-- compilation. The flag is fixed with the rest of the revision inputs; existing
-- revisions keep their current behavior.
ALTER TABLE runtime_revision ADD COLUMN git_egress_restricted BOOLEAN NOT NULL DEFAULT false;

-- +goose Down

ALTER TABLE runtime_revision DROP COLUMN git_egress_restricted;
