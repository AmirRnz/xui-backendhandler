ALTER TABLE commercial_accounts
  ADD CONSTRAINT commercial_accounts_deployment_account_uq UNIQUE(home_deployment_id,id);

CREATE TABLE IF NOT EXISTS plan_access (
  deployment_id TEXT NOT NULL,
  plan_id BIGINT NOT NULL,
  account_id BIGINT NOT NULL,
  granted_by BIGINT REFERENCES actors(id),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY(deployment_id,plan_id,account_id),
  FOREIGN KEY(deployment_id,plan_id) REFERENCES plans(deployment_id,id) ON DELETE CASCADE,
  FOREIGN KEY(deployment_id,account_id) REFERENCES commercial_accounts(home_deployment_id,id) ON DELETE CASCADE
);
