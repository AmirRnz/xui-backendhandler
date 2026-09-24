ALTER TABLE actors
  ADD CONSTRAINT actors_deployment_account_fk
    FOREIGN KEY(deployment_id,account_id)
    REFERENCES commercial_accounts(home_deployment_id,id);

ALTER TABLE actors
  ADD CONSTRAINT actors_deployment_account_actor_uq
    UNIQUE(deployment_id,account_id,id);
