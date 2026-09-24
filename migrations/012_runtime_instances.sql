ALTER TABLE deployments
  ADD COLUMN IF NOT EXISTS encrypted_telegram_token BYTEA,
  ADD COLUMN IF NOT EXISTS admin_telegram_id BIGINT,
  ADD COLUMN IF NOT EXISTS bot_instance_active BOOLEAN NOT NULL DEFAULT TRUE,
  ADD COLUMN IF NOT EXISTS telegram_notifications_enabled BOOLEAN NOT NULL DEFAULT TRUE;

UPDATE deployments SET admin_telegram_id=96937669
  WHERE id IN ('retail-finland','reseller-turk1') AND admin_telegram_id IS NULL;

CREATE TABLE IF NOT EXISTS backend_client_credentials (
  token_hash BYTEA PRIMARY KEY,
  deployment_id TEXT NOT NULL REFERENCES deployments(id) ON DELETE CASCADE,
  enabled BOOLEAN NOT NULL DEFAULT TRUE,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS backend_client_credentials_deployment_idx
  ON backend_client_credentials(deployment_id) WHERE enabled;
