CREATE TABLE IF NOT EXISTS deployment_runtime_variables (
    id TEXT PRIMARY KEY,
    deployment_id TEXT NOT NULL REFERENCES deployments(id) ON DELETE CASCADE,
    key TEXT NOT NULL,
    value_ciphertext TEXT NOT NULL,
    is_secret BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (deployment_id, key)
);

CREATE INDEX IF NOT EXISTS idx_deployment_runtime_variables_deployment_updated
    ON deployment_runtime_variables (deployment_id, updated_at DESC);
