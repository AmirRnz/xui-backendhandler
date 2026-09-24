ALTER TABLE deployments
  ADD COLUMN IF NOT EXISTS configuration JSONB NOT NULL DEFAULT '{"features":{},"text":{}}'::jsonb;

ALTER TABLE panels
  ADD COLUMN IF NOT EXISTS encrypted_api_token BYTEA;
ALTER TABLE commercial_accounts
  ADD COLUMN IF NOT EXISTS system_key TEXT;
CREATE UNIQUE INDEX IF NOT EXISTS commercial_accounts_system_key_uq
  ON commercial_accounts(system_key) WHERE system_key IS NOT NULL;

CREATE TABLE IF NOT EXISTS admin_configuration_audit (
  id BIGSERIAL PRIMARY KEY,
  deployment_id TEXT NOT NULL REFERENCES deployments(id),
  actor_id BIGINT NOT NULL REFERENCES actors(id),
  action TEXT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS admin_configuration_audit_recent_idx
  ON admin_configuration_audit(deployment_id,created_at DESC);

-- These are the sole Telegram identities authorized to administer the two v2 deployments.
-- Germany intentionally has no corresponding admin actor.
INSERT INTO commercial_accounts(home_deployment_id,kind,status,system_key)
SELECT id, CASE WHEN channel='reseller' THEN 'reseller' ELSE 'retail_customer' END,'active','telegram-admin:96937669:'||id
FROM deployments d WHERE id IN ('retail-finland','reseller-turk1')
  AND NOT EXISTS (SELECT 1 FROM commercial_accounts a WHERE a.system_key='telegram-admin:96937669:'||d.id);

INSERT INTO actors(deployment_id,account_id,telegram_id,identity_provider,external_subject,role,approval_status,enabled)
SELECT d.id,a.id,96937669,'telegram','96937669','admin','approved',true
FROM deployments d JOIN commercial_accounts a ON a.home_deployment_id=d.id AND a.system_key='telegram-admin:96937669:'||d.id
WHERE d.id IN ('retail-finland','reseller-turk1')
ON CONFLICT(deployment_id,telegram_id)
DO UPDATE SET identity_provider='telegram',external_subject='96937669',role='admin',approval_status='approved',enabled=true,updated_at=now();
