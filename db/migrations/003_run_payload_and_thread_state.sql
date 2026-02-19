ALTER TABLE runs
  ADD COLUMN IF NOT EXISTS input_json JSONB NOT NULL DEFAULT '{}'::jsonb,
  ADD COLUMN IF NOT EXISTS output_json JSONB,
  ADD COLUMN IF NOT EXISTS error_json JSONB,
  ADD COLUMN IF NOT EXISTS metadata_json JSONB NOT NULL DEFAULT '{}'::jsonb,
  ADD COLUMN IF NOT EXISTS started_at TIMESTAMPTZ,
  ADD COLUMN IF NOT EXISTS completed_at TIMESTAMPTZ,
  ADD COLUMN IF NOT EXISTS checkpoint_id TEXT;

CREATE INDEX IF NOT EXISTS idx_runs_checkpoint_id ON runs(checkpoint_id);

CREATE TABLE IF NOT EXISTS thread_states (
  id TEXT PRIMARY KEY,
  thread_id TEXT NOT NULL REFERENCES threads(id) ON DELETE CASCADE,
  checkpoint_id TEXT,
  parent_state_id TEXT REFERENCES thread_states(id) ON DELETE SET NULL,
  values_json JSONB NOT NULL DEFAULT '{}'::jsonb,
  metadata_json JSONB NOT NULL DEFAULT '{}'::jsonb,
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS thread_state_transitions (
  id TEXT PRIMARY KEY,
  thread_id TEXT NOT NULL REFERENCES threads(id) ON DELETE CASCADE,
  from_state_id TEXT REFERENCES thread_states(id) ON DELETE SET NULL,
  to_state_id TEXT NOT NULL REFERENCES thread_states(id) ON DELETE CASCADE,
  reason TEXT,
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_thread_states_thread_created_at ON thread_states(thread_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_thread_state_transitions_thread_created_at ON thread_state_transitions(thread_id, created_at DESC);
