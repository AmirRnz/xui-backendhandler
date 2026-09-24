ALTER TABLE admin_configuration_audit
  ADD COLUMN IF NOT EXISTS subject_ref TEXT NOT NULL DEFAULT '';
