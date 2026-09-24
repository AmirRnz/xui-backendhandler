CREATE TABLE IF NOT EXISTS client_services (
  id TEXT PRIMARY KEY,
  display_name TEXT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS panels (
  id TEXT PRIMARY KEY,
  base_url TEXT NOT NULL,
  enabled BOOLEAN NOT NULL DEFAULT true,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS deployments (
  id TEXT PRIMARY KEY,
  client_service_id TEXT NOT NULL REFERENCES client_services(id),
  channel TEXT NOT NULL CHECK (channel IN ('retail','reseller','web')),
  legacy_source_instance TEXT NOT NULL UNIQUE,
  default_panel_id TEXT REFERENCES panels(id),
  payment_card_number TEXT NOT NULL DEFAULT '',
  payment_card_owner TEXT NOT NULL DEFAULT '',
  payment_instructions TEXT NOT NULL DEFAULT '',
  retail_trial_reset_days INT NOT NULL DEFAULT 30,
  unapproved_trial_daily_limit INT NOT NULL DEFAULT 1,
  enabled BOOLEAN NOT NULL DEFAULT true,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS commercial_accounts (
  id BIGSERIAL PRIMARY KEY,
  home_deployment_id TEXT NOT NULL REFERENCES deployments(id),
  kind TEXT NOT NULL CHECK (kind IN ('retail_customer','reseller','reseller_customer','organization')),
  parent_account_id BIGINT REFERENCES commercial_accounts(id),
  status TEXT NOT NULL DEFAULT 'active',
  wallet_balance_toman BIGINT NOT NULL DEFAULT 0 CHECK (wallet_balance_toman >= 0),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS actors (
  id BIGSERIAL PRIMARY KEY,
  deployment_id TEXT NOT NULL REFERENCES deployments(id),
  account_id BIGINT NOT NULL REFERENCES commercial_accounts(id),
  telegram_id BIGINT NOT NULL,
  role TEXT NOT NULL CHECK (role IN ('customer','reseller','reseller_customer','admin','operator')),
  approval_status TEXT NOT NULL DEFAULT 'pending' CHECK (approval_status IN ('pending','approved','rejected')),
  enabled BOOLEAN NOT NULL DEFAULT true,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE(deployment_id, telegram_id)
);
CREATE TABLE IF NOT EXISTS plans (
  id BIGSERIAL PRIMARY KEY,
  deployment_id TEXT NOT NULL REFERENCES deployments(id),
  panel_id TEXT NOT NULL REFERENCES panels(id),
  kind TEXT NOT NULL CHECK (kind IN ('paid','test')),
  name TEXT NOT NULL,
  enabled BOOLEAN NOT NULL DEFAULT true,
  is_limited BOOLEAN NOT NULL DEFAULT false,
  is_global BOOLEAN NOT NULL DEFAULT true,
  description TEXT NOT NULL DEFAULT '',
  base_price_toman BIGINT NOT NULL DEFAULT 0 CHECK (base_price_toman >= 0),
  price_per_extra_ip_toman BIGINT NOT NULL DEFAULT 0 CHECK (price_per_extra_ip_toman >= 0),
  price_per_gb_toman BIGINT NOT NULL DEFAULT 0 CHECK (price_per_gb_toman >= 0),
  price_per_extra_month_toman BIGINT NOT NULL DEFAULT 0 CHECK (price_per_extra_month_toman >= 0),
  base_ip_limit INT NOT NULL DEFAULT 1 CHECK (base_ip_limit >= 0),
  max_ip_limit INT NOT NULL DEFAULT 1 CHECK (max_ip_limit >= base_ip_limit),
  min_data_gb INT NOT NULL DEFAULT 0 CHECK (min_data_gb >= 0),
  max_data_bytes BIGINT NOT NULL DEFAULT 0 CHECK (max_data_bytes >= 0),
  expire_seconds BIGINT NOT NULL DEFAULT 0 CHECK (expire_seconds >= 0),
  test_ip_limit INT NOT NULL DEFAULT 1 CHECK (test_ip_limit >= 0),
  max_per_day INT NOT NULL DEFAULT 0 CHECK (max_per_day >= 0),
  flow TEXT NOT NULL DEFAULT '',
  inbound_ids INT[] NOT NULL DEFAULT '{}',
  discount_tiers JSONB NOT NULL DEFAULT '[]'::jsonb,
  usage_description TEXT NOT NULL DEFAULT '',
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE(deployment_id, id)
);
CREATE TABLE IF NOT EXISTS purchase_quotes (
  id BIGSERIAL PRIMARY KEY,
  deployment_id TEXT NOT NULL REFERENCES deployments(id),
  account_id BIGINT NOT NULL REFERENCES commercial_accounts(id),
  actor_id BIGINT NOT NULL REFERENCES actors(id),
  quote_key TEXT NOT NULL,
  plan_id BIGINT NOT NULL REFERENCES plans(id),
  plan_snapshot JSONB NOT NULL,
  terms JSONB NOT NULL,
  months INT NOT NULL CHECK (months > 0),
  ip_limit INT NOT NULL CHECK (ip_limit >= 0),
  data_gb INT NOT NULL CHECK (data_gb >= 0),
  final_price_toman BIGINT NOT NULL CHECK (final_price_toman > 0),
  currency TEXT NOT NULL DEFAULT 'تومان',
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE(deployment_id, account_id, quote_key)
);
CREATE TABLE IF NOT EXISTS wallet_ledger (
  id BIGSERIAL PRIMARY KEY,
  deployment_id TEXT NOT NULL REFERENCES deployments(id),
  account_id BIGINT NOT NULL REFERENCES commercial_accounts(id),
  actor_id BIGINT REFERENCES actors(id),
  amount_toman BIGINT NOT NULL CHECK (amount_toman <> 0),
  balance_after_toman BIGINT NOT NULL CHECK (balance_after_toman >= 0),
  entry_type TEXT NOT NULL CHECK (entry_type IN ('credit','debit','adjustment')),
  description TEXT NOT NULL,
  operation_key TEXT NOT NULL,
  input_hash TEXT NOT NULL,
  reference_type TEXT NOT NULL DEFAULT '',
  reference_id BIGINT,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE(deployment_id, account_id, operation_key)
);
CREATE TABLE IF NOT EXISTS subscriptions (
  id BIGSERIAL PRIMARY KEY,
  deployment_id TEXT NOT NULL REFERENCES deployments(id),
  account_id BIGINT NOT NULL REFERENCES commercial_accounts(id),
  subscriber_account_id BIGINT REFERENCES commercial_accounts(id),
  actor_id BIGINT REFERENCES actors(id),
  panel_id TEXT NOT NULL REFERENCES panels(id),
  plan_id BIGINT REFERENCES plans(id),
  source_kind TEXT NOT NULL CHECK (source_kind IN ('paid','test','legacy')),
  status TEXT NOT NULL CHECK (status IN ('provisioning','active','expired','cancel_requested','deprovisioning','reconciliation','cancelled','deleted','manual_review')),
  client_email TEXT NOT NULL,
  client_uuid TEXT NOT NULL,
  sub_id TEXT NOT NULL,
  display_name TEXT NOT NULL DEFAULT '',
  ip_limit INT NOT NULL DEFAULT 1,
  traffic_limit_bytes BIGINT NOT NULL DEFAULT 0,
  expiry_time_ms BIGINT NOT NULL DEFAULT 0,
  flow TEXT NOT NULL DEFAULT '',
  inbound_ids INT[] NOT NULL DEFAULT '{}',
  subscription_links JSONB NOT NULL DEFAULT '[]'::jsonb,
  panel_link TEXT NOT NULL DEFAULT '',
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE(panel_id, client_email),
  UNIQUE(panel_id, client_uuid),
  UNIQUE(panel_id, sub_id)
);
CREATE INDEX IF NOT EXISTS subscriptions_owner_idx ON subscriptions(deployment_id, account_id, status);
CREATE TABLE IF NOT EXISTS payment_intents (
  id BIGSERIAL PRIMARY KEY,
  deployment_id TEXT NOT NULL REFERENCES deployments(id),
  account_id BIGINT NOT NULL REFERENCES commercial_accounts(id),
  actor_id BIGINT NOT NULL REFERENCES actors(id),
  quote_id BIGINT NOT NULL REFERENCES purchase_quotes(id),
  intent_key TEXT NOT NULL,
  input_hash TEXT NOT NULL,
  amount_toman BIGINT NOT NULL CHECK (amount_toman > 0),
  terms JSONB NOT NULL,
  status TEXT NOT NULL CHECK (status IN ('awaiting_receipt','receipt_submitted','approved','rejected','cancelled','expired')),
  telegram_file_id TEXT NOT NULL DEFAULT '',
  reviewed_by BIGINT REFERENCES actors(id),
  review_note TEXT NOT NULL DEFAULT '',
  approved_at TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE(deployment_id, account_id, intent_key)
);
CREATE UNIQUE INDEX IF NOT EXISTS one_active_payment_intent_per_account ON payment_intents(deployment_id, account_id) WHERE status IN ('awaiting_receipt','receipt_submitted');
CREATE TABLE IF NOT EXISTS orders (
  id BIGSERIAL PRIMARY KEY,
  deployment_id TEXT NOT NULL REFERENCES deployments(id),
  account_id BIGINT NOT NULL REFERENCES commercial_accounts(id),
  actor_id BIGINT NOT NULL REFERENCES actors(id),
  quote_id BIGINT NOT NULL REFERENCES purchase_quotes(id),
  operation_key TEXT NOT NULL,
  input_hash TEXT NOT NULL,
  payment_method TEXT NOT NULL CHECK (payment_method IN ('wallet','direct')),
  status TEXT NOT NULL CHECK (status IN ('awaiting_payment','provisioning','completed','manual_review','cancelled')),
  subscription_id BIGINT REFERENCES subscriptions(id),
  payment_intent_id BIGINT REFERENCES payment_intents(id),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE(deployment_id, account_id, operation_key)
);
CREATE TABLE IF NOT EXISTS payment_settlements (
  id BIGSERIAL PRIMARY KEY,
  deployment_id TEXT NOT NULL REFERENCES deployments(id),
  account_id BIGINT NOT NULL REFERENCES commercial_accounts(id),
  intent_id BIGINT NOT NULL UNIQUE REFERENCES payment_intents(id),
  amount_toman BIGINT NOT NULL CHECK (amount_toman > 0),
  status TEXT NOT NULL CHECK (status IN ('approved','reversed')),
  approved_by BIGINT NOT NULL REFERENCES actors(id),
  operation_key TEXT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE(deployment_id, account_id, operation_key)
);
CREATE TABLE IF NOT EXISTS work_items (
  id BIGSERIAL PRIMARY KEY,
  deployment_id TEXT NOT NULL REFERENCES deployments(id),
  account_id BIGINT REFERENCES commercial_accounts(id),
  actor_id BIGINT REFERENCES actors(id),
  panel_id TEXT NOT NULL REFERENCES panels(id),
  subscription_id BIGINT REFERENCES subscriptions(id),
  payment_intent_id BIGINT REFERENCES payment_intents(id),
  operation_key TEXT NOT NULL,
  input_hash TEXT NOT NULL,
  kind TEXT NOT NULL CHECK (kind IN ('provision_add','subscription_update','subscription_delete')),
  desired_state JSONB NOT NULL,
  observed_state JSONB NOT NULL DEFAULT '{}'::jsonb,
  phase TEXT NOT NULL DEFAULT 'ready' CHECK (phase IN ('ready','create_attempted','manual_review')),
  status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','running','succeeded','failed','manual_review')),
  attempts INT NOT NULL DEFAULT 0,
  last_error TEXT NOT NULL DEFAULT '',
  lease_until TIMESTAMPTZ,
  next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE(deployment_id, operation_key)
);
CREATE INDEX IF NOT EXISTS work_items_claim_idx ON work_items(status, lease_until, id);
CREATE TABLE IF NOT EXISTS trial_usage (
  deployment_id TEXT NOT NULL REFERENCES deployments(id),
  account_id BIGINT NOT NULL REFERENCES commercial_accounts(id),
  plan_id BIGINT NOT NULL REFERENCES plans(id),
  policy TEXT NOT NULL CHECK (policy IN ('retail_cooldown','reseller_daily')),
  period_date DATE NOT NULL,
  used_count INT NOT NULL CHECK (used_count >= 0),
  last_claimed_at TIMESTAMPTZ NOT NULL,
  last_work_item_id BIGINT,
  PRIMARY KEY(deployment_id, account_id, plan_id, policy, period_date)
);
CREATE TABLE IF NOT EXISTS trial_claims (
  work_item_id BIGINT PRIMARY KEY REFERENCES work_items(id) ON DELETE CASCADE,
  deployment_id TEXT NOT NULL REFERENCES deployments(id),
  account_id BIGINT NOT NULL REFERENCES commercial_accounts(id),
  plan_id BIGINT NOT NULL REFERENCES plans(id),
  policy TEXT NOT NULL,
  period_date DATE NOT NULL,
  status TEXT NOT NULL CHECK (status IN ('reserved','committed','released')),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS outbox (
  id BIGSERIAL PRIMARY KEY,
  deployment_id TEXT NOT NULL REFERENCES deployments(id),
  account_id BIGINT REFERENCES commercial_accounts(id),
  actor_id BIGINT REFERENCES actors(id),
  dedupe_key TEXT NOT NULL,
  topic TEXT NOT NULL,
  payload JSONB NOT NULL,
  status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','sending','sent','failed')),
  attempts INT NOT NULL DEFAULT 0,
  next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  lease_until TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE(deployment_id, dedupe_key)
);
CREATE TABLE IF NOT EXISTS topup_requests (
  id BIGSERIAL PRIMARY KEY,
  deployment_id TEXT NOT NULL REFERENCES deployments(id),
  account_id BIGINT NOT NULL REFERENCES commercial_accounts(id),
  actor_id BIGINT NOT NULL REFERENCES actors(id),
  amount_toman BIGINT NOT NULL CHECK (amount_toman > 0),
  operation_key TEXT NOT NULL,
  input_hash TEXT NOT NULL,
  status TEXT NOT NULL CHECK (status IN ('awaiting_receipt','receipt_submitted','approved','rejected','cancelled')),
  telegram_file_id TEXT NOT NULL DEFAULT '',
  reviewed_by BIGINT REFERENCES actors(id),
  review_note TEXT NOT NULL DEFAULT '',
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE(deployment_id, account_id, operation_key)
);
CREATE TABLE IF NOT EXISTS wallet_credit_approvals (
  id BIGSERIAL PRIMARY KEY,
  deployment_id TEXT NOT NULL REFERENCES deployments(id),
  account_id BIGINT NOT NULL REFERENCES commercial_accounts(id),
  topup_request_id BIGINT UNIQUE REFERENCES topup_requests(id),
  amount_toman BIGINT NOT NULL CHECK (amount_toman > 0),
  approved_by BIGINT NOT NULL REFERENCES actors(id),
  operation_key TEXT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE(deployment_id, account_id, operation_key)
);
CREATE TABLE IF NOT EXISTS legacy_import_batches (
  source_instance TEXT PRIMARY KEY,
  channel TEXT NOT NULL,
  source_fingerprint TEXT NOT NULL,
  status TEXT NOT NULL CHECK (status IN ('started','complete','blocked')),
  source_counts JSONB NOT NULL DEFAULT '{}'::jsonb,
  source_financial_totals JSONB NOT NULL DEFAULT '{}'::jsonb,
  target_counts JSONB NOT NULL DEFAULT '{}'::jsonb,
  target_financial_totals JSONB NOT NULL DEFAULT '{}'::jsonb,
  collision_report JSONB NOT NULL DEFAULT '[]'::jsonb,
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS legacy_id_map (
  source_instance TEXT NOT NULL,
  entity_type TEXT NOT NULL,
  legacy_id TEXT NOT NULL,
  target_entity_type TEXT NOT NULL,
  target_id TEXT NOT NULL,
  content_hash TEXT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY(source_instance, entity_type, legacy_id)
);
CREATE TABLE IF NOT EXISTS legacy_records (
  source_instance TEXT NOT NULL,
  entity_type TEXT NOT NULL,
  legacy_id TEXT NOT NULL,
  payload JSONB NOT NULL,
  content_hash TEXT NOT NULL,
  imported_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY(source_instance, entity_type, legacy_id),
  FOREIGN KEY(source_instance, entity_type, legacy_id) REFERENCES legacy_id_map(source_instance, entity_type, legacy_id)
);
CREATE TABLE IF NOT EXISTS migration_audit (
  id BIGSERIAL PRIMARY KEY,
  source_instance TEXT NOT NULL,
  action TEXT NOT NULL,
  details JSONB NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
