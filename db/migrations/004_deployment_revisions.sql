CREATE TABLE IF NOT EXISTS deployment_revisions (
  id TEXT PRIMARY KEY,
  deployment_id TEXT NOT NULL REFERENCES deployments(id) ON DELETE CASCADE,
  image_digest TEXT,
  repo_url TEXT NOT NULL,
  git_ref TEXT NOT NULL,
  repo_path TEXT NOT NULL,
  runtime_profile TEXT NOT NULL,
  mode TEXT NOT NULL,
  reason TEXT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_deployment_revisions_deployment_created_at
  ON deployment_revisions(deployment_id, created_at DESC);
