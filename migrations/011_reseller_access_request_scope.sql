ALTER TABLE actors
  ADD CONSTRAINT actors_deployment_id_id_unique UNIQUE (deployment_id, id);

ALTER TABLE reseller_access_requests
  ADD CONSTRAINT reseller_access_requests_actor_deployment_fk
  FOREIGN KEY (deployment_id, actor_id) REFERENCES actors(deployment_id, id);
