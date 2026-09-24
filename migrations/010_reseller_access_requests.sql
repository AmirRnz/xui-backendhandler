CREATE TABLE IF NOT EXISTS reseller_access_requests (
  id BIGSERIAL PRIMARY KEY,
  deployment_id TEXT NOT NULL REFERENCES deployments(id),
  actor_id BIGINT NOT NULL REFERENCES actors(id),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS reseller_access_requests_actor_created_idx
  ON reseller_access_requests(deployment_id, actor_id, created_at DESC, id DESC);
