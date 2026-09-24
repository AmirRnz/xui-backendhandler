ALTER TABLE actors ALTER COLUMN telegram_id DROP NOT NULL;
ALTER TABLE actors ADD COLUMN identity_provider TEXT NOT NULL DEFAULT 'telegram'
  CHECK(identity_provider IN ('telegram','web','service'));
ALTER TABLE actors ADD COLUMN external_subject TEXT NOT NULL DEFAULT '';
UPDATE actors SET external_subject=telegram_id::text WHERE identity_provider='telegram' AND external_subject='';
ALTER TABLE actors ADD CONSTRAINT actors_external_subject_nonempty CHECK(length(external_subject)>0);
ALTER TABLE actors ADD CONSTRAINT actors_deployment_external_subject_uq
  UNIQUE(deployment_id,identity_provider,external_subject);

ALTER TABLE commercial_accounts
  ADD CONSTRAINT commercial_accounts_parent_same_deployment_fk
    FOREIGN KEY(home_deployment_id,parent_account_id)
    REFERENCES commercial_accounts(home_deployment_id,id);
