CREATE TABLE IF NOT EXISTS legacy_panel_assignments (
  source_instance TEXT PRIMARY KEY,
  panel_id TEXT NOT NULL REFERENCES panels(id),
  assigned_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS legacy_obligations (
  source_instance TEXT NOT NULL,
  entity_type TEXT NOT NULL,
  legacy_id TEXT NOT NULL,
  kind TEXT NOT NULL CHECK (kind IN ('pending_payment','pending_topup','pending_refund','reconciliation')),
  payload JSONB NOT NULL,
  status TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open','resolved','waived')),
  review_note TEXT NOT NULL DEFAULT '',
  reviewed_by TEXT NOT NULL DEFAULT '',
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY(source_instance, entity_type, legacy_id, kind),
  FOREIGN KEY(source_instance, entity_type, legacy_id)
    REFERENCES legacy_records(source_instance, entity_type, legacy_id)
);

CREATE INDEX IF NOT EXISTS legacy_obligations_open_idx
  ON legacy_obligations(source_instance, status, kind);
