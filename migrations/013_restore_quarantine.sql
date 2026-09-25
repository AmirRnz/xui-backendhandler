-- Restored work must not be replayed until remote or delivery uncertainty is reviewed.
ALTER TABLE work_items ADD COLUMN IF NOT EXISTS restore_quarantined BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE outbox ADD COLUMN IF NOT EXISTS restore_quarantined BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE deployments ADD COLUMN IF NOT EXISTS transfer_frozen BOOLEAN NOT NULL DEFAULT FALSE;
