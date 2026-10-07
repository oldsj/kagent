-- +goose Up
-- Prepared environment variants remain reachable while their base is selected
-- by an active Agent, including the interval before the first Session reserves
-- them. Session/checkpoint references retain old variants after base changes.
CREATE OR REPLACE VIEW unreferenced_runtime_revision AS
SELECT r.revision FROM runtime_revision r
WHERE NOT EXISTS (SELECT 1 FROM agent_definition p WHERE p.retired_at IS NULL
    AND (p.desired_revision = r.revision OR p.latest_successful_revision = r.revision
        OR p.desired_revision = (r.source_snapshot ->> 'baseRevision')
        OR p.latest_successful_revision = (r.source_snapshot ->> 'baseRevision')))
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
