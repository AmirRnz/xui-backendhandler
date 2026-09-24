CREATE TABLE IF NOT EXISTS refund_requests (
  id BIGSERIAL PRIMARY KEY,
  deployment_id TEXT NOT NULL REFERENCES deployments(id),
  account_id BIGINT NOT NULL REFERENCES commercial_accounts(id),
  actor_id BIGINT NOT NULL REFERENCES actors(id),
  subscription_id BIGINT NOT NULL UNIQUE REFERENCES subscriptions(id),
  operation_key TEXT NOT NULL,
  input_hash TEXT NOT NULL,
  suggested_amount_toman BIGINT NOT NULL DEFAULT 0 CHECK (suggested_amount_toman >= 0),
  refundable_cap_toman BIGINT NOT NULL DEFAULT 0 CHECK (refundable_cap_toman >= 0),
  reason TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','approved','rejected')),
  approved_amount_toman BIGINT,
  approved_by BIGINT REFERENCES actors(id),
  audit_note TEXT NOT NULL DEFAULT '',
  manual_override BOOLEAN NOT NULL DEFAULT false,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE(deployment_id,account_id,operation_key)
);
CREATE TABLE IF NOT EXISTS refund_approvals (
  id BIGSERIAL PRIMARY KEY,
  request_id BIGINT NOT NULL UNIQUE REFERENCES refund_requests(id),
  deployment_id TEXT NOT NULL REFERENCES deployments(id),
  account_id BIGINT NOT NULL REFERENCES commercial_accounts(id),
  amount_toman BIGINT NOT NULL CHECK (amount_toman > 0),
  approved_by BIGINT NOT NULL REFERENCES actors(id),
  audit_note TEXT NOT NULL,
  manual_override BOOLEAN NOT NULL DEFAULT false,
  operation_key TEXT NOT NULL,
  input_hash TEXT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE(deployment_id,account_id,operation_key)
);
