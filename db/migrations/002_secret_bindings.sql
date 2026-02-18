CREATE TABLE IF NOT EXISTS deployment_secret_bindings (
  id TEXT PRIMARY KEY,
  deployment_id TEXT NOT NULL REFERENCES deployments(id) ON DELETE CASCADE,
  secret_name TEXT NOT NULL,
  target_key TEXT,
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  UNIQUE(deployment_id, secret_name, target_key)
);

CREATE INDEX IF NOT EXISTS idx_secret_bindings_deployment ON deployment_secret_bindings(deployment_id);
