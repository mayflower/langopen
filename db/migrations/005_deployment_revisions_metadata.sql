ALTER TABLE deployment_revisions
  ADD COLUMN IF NOT EXISTS revision_id TEXT;

UPDATE deployment_revisions
SET revision_id = id
WHERE revision_id IS NULL OR revision_id = '';

ALTER TABLE deployment_revisions
  ALTER COLUMN revision_id SET NOT NULL;

ALTER TABLE deployment_revisions
  ADD COLUMN IF NOT EXISTS metadata JSONB NOT NULL DEFAULT '{}'::jsonb;

CREATE INDEX IF NOT EXISTS idx_deployment_revisions_revision_id
  ON deployment_revisions(deployment_id, revision_id);
