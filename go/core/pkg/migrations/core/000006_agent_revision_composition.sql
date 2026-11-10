-- +goose Up

-- A composed revision pairs a Session-selected development image with the
-- operator's runtime payload. It is derived from a base Agent revision and is
-- never an Agent's desired or latest revision. base_revision is not a foreign
-- key: Sessions and checkpoints keep a variant after its base is collected.
ALTER TABLE agent_revision
    ADD COLUMN base_revision TEXT CHECK (base_revision IS NULL OR base_revision <> revision),
    ADD COLUMN composition JSONB CHECK (composition IS NULL OR jsonb_typeof(composition) = 'object'),
    ADD CONSTRAINT agent_revision_composition_base CHECK ((base_revision IS NULL) = (composition IS NULL));
CREATE INDEX agent_revision_base_idx ON agent_revision (base_revision) WHERE base_revision IS NOT NULL;

-- Variants stay reachable while their base is selected by an active Agent,
-- including the interval between preparation and the first reservation.
CREATE OR REPLACE VIEW unreferenced_runtime_revision AS
SELECT r.revision FROM runtime_revision r
WHERE NOT EXISTS (SELECT 1 FROM agent_definition p WHERE p.retired_at IS NULL
    AND (p.desired_revision = r.revision OR p.latest_successful_revision = r.revision))
AND NOT EXISTS (SELECT 1 FROM agent_revision a JOIN agent_definition p
    ON p.retired_at IS NULL AND p.latest_successful_revision = a.base_revision
    WHERE a.revision = r.revision)
AND NOT EXISTS (SELECT 1 FROM sandbox_template_definition p WHERE p.retired_at IS NULL
    AND (p.desired_revision = r.revision OR p.latest_successful_revision = r.revision))
AND NOT EXISTS (SELECT 1 FROM runtime_instance i WHERE i.prepared_revision = r.revision)
AND NOT EXISTS (SELECT 1 FROM session_checkpoint c WHERE c.prepared_revision = r.revision);

-- +goose Down

CREATE OR REPLACE VIEW unreferenced_runtime_revision AS
SELECT r.revision FROM runtime_revision r
WHERE NOT EXISTS (SELECT 1 FROM agent_definition p WHERE p.retired_at IS NULL
    AND (p.desired_revision = r.revision OR p.latest_successful_revision = r.revision))
AND NOT EXISTS (SELECT 1 FROM sandbox_template_definition p WHERE p.retired_at IS NULL
    AND (p.desired_revision = r.revision OR p.latest_successful_revision = r.revision))
AND NOT EXISTS (SELECT 1 FROM runtime_instance i WHERE i.prepared_revision = r.revision)
AND NOT EXISTS (SELECT 1 FROM session_checkpoint c WHERE c.prepared_revision = r.revision);

DROP INDEX agent_revision_base_idx;
ALTER TABLE agent_revision
    DROP CONSTRAINT agent_revision_composition_base,
    DROP COLUMN composition,
    DROP COLUMN base_revision;
