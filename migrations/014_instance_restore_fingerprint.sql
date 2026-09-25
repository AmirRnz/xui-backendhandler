ALTER TABLE deployments
  ADD COLUMN IF NOT EXISTS restore_fingerprint TEXT NOT NULL DEFAULT '';
