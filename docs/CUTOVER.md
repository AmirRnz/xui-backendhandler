# Migration and cutover runbook

This runbook has a tested raw archive step and a blocked canonical conversion step. The archive preserves source material and review obligations; it does not make subscriptions or balances live in the new system. Do not cut over a deployment until the conversion blockers below are cleared in a separately reviewed implementation.

## Preserve three independent sources

Use the source identifiers exactly as follows:

| Source | Legacy deployment | New deployment | Expected channel |
|---|---|---|---|
| `retail-finland` | `/opt/xui-end-bot` | `retail-finland` | retail |
| `retail-germany` | `/opt/xui-end-bot-germany` | `retail-germany` | retail |
| `reseller-turk1` | `/opt/xui-reseller-bot` | `reseller-turk1` | reseller |

The importer stores `(source_instance, entity_type, legacy_id)` as the source key. Equal user, plan, transaction, or subscription IDs remain distinct. It never infers ownership from Telegram ID, email, username, UUID, group, or remote client alone. If two sources map to one target panel, it reports matching subscription email/UUID/sub-ID identities for operator review.

## Stage 0: freeze and backup

1. Schedule a maintenance window and record the exact source DB names, panel URLs, panel versions, bot configs, service versions, and current active workers for all three deployments.
2. Stop changes to plans, wallets, approvals, subscriptions, and panel clients for the source being captured. Take and verify a PostgreSQL backup and a separate panel/config backup. Keep it outside these repositories.
3. Record pre-import table counts, exact monetary column sums, pending payment/top-up/refund counts, and reconciliation record counts. Confirm the two retail sources still refer to separate data sets.
4. Keep all production bot and worker processes unchanged until a separately approved cutover. Do not run this tool against production without a specific operational request.

## Stage 1: backend preparation

1. Provision a separate PostgreSQL 16 database and apply `go run ./cmd/backend migrate`.
2. Verify `client_services`, the three deployment rows, and panel rows. Change seeded `.invalid` panel URLs only after matching each source deployment to the correct panel. Configure backend bearer credentials, panel API tokens, and Telegram bot tokens outside source control.
3. Bootstrap plans, scoped actors/roles, deployment payment instructions, panel identity maps, and admin accounts only from reviewed source data. The current archive command does not do this canonical data conversion.
4. Verify the exact 3x-ui version. Write access is gated to 3.8.5. The supplied spec says 3.x; no broader version support is assumed.

## Stage 2: dry-run source archive

Run one source at a time from the repository root, with an account that can only read the legacy database. The source connection is forced read-only by the CLI. The target database must already have the migrations applied.

```powershell
$env:LEGACY_DATABASE_URL = 'postgres://legacy_reader:...@legacy-host:5432/legacy_db?sslmode=verify-full'
$env:DATABASE_URL = 'postgres://xui_app:...@backend-host:5432/xui_commerce?sslmode=verify-full'
go run ./cmd/import-legacy --source-instance retail-finland --target-panel-id panel-retail-finland
```

Repeat separately with `retail-germany` and `reseller-turk1`, using the verified target panel mapping. Review the JSON report:

- every public source table count;
- exact decimal sums for numeric columns whose names contain amount, balance, price, cost, wallet, refund, or total (these are reported in original source units, not normalized);
- source ID collisions against already imported instances;
- remote subscription email/UUID/sub-ID collisions only when two sources map to the same target panel;
- pending payment/top-up/refund and reconciliation record counts.

Any collision requires explicit ownership/panel investigation. Never resolve one by merging on matching IDs. Confirm old Toman/Rial or other source units from authoritative records; the importer does not normalize money.

## Stage 3: archive and validate

After review, rerun with `--apply`. The command holds a repeatable-read, read-only snapshot of the source and writes exact JSON source rows, content hashes, source ID maps, and open obligation rows in bounded target chunks. A source row, its identity map, and its obligation are committed together. If the process stops, rerunning the unchanged source skips committed rows and continues from the last committed chunk. It rejects changed row contents or changed source counts/totals rather than overwriting them; a changed source requires a newly reviewed migration plan.

For each source, independently compare:

1. all source table counts against `legacy_import_batches.source_counts` and archived target counts;
2. all exact-unit monetary totals against `source_financial_totals` and `target_financial_totals`;
3. count of archived rows and source-scoped ID maps;
4. open obligations by type and each payload against the original row;
5. collision report and target panel assignment;
6. source data has not changed since the report.

Resolve each pending payment, wallet top-up, refund, and reconciliation row from authoritative history. Do not automatically replay any legacy approval, credit, refund, or 3x-ui operation. Determine whether its external effect already occurred, and record an operator-approved conversion decision before making a new backend record.

## Canonical conversion gate — currently not implemented

The raw archive is not sufficient for a safe bot cutover. Before cutover, implement and test a source-specific conversion that maps accounts/actors, approved plans and access grants, Toman balances with opening ledger entries, subscription ownership, and remote panel identity to backend records. This must include explicit price/money unit resolution; preserve historical ledgers and quotes; keep old subscription terms unavailable rather than reconstructing them; and place unverified or colliding subscriptions into manual review. Then repeat count/financial checks after canonical conversion. The current system does not claim those checks have passed.

## Cutover sequence after that gate is satisfied

1. Announce a source-specific freeze and take final verified DB and panel backups.
2. Stop that deployment's legacy bot service and every legacy reconciliation/provisioning/notification worker. Confirm no process can write after the recorded shutdown time.
3. Apply the final archive and canonical conversion, resolve pending obligations, verify all blockers and exact totals, and check panel identity mappings.
4. Start only the backend worker and the one v2 bot scoped to that deployment. Run read-only health, actor ownership, plan visibility, wallet, subscription, admin approval, and a panel mock/sandbox smoke test first.
5. Observe durable work, outbox, and reconciliation queues before enabling customer writes. Never run legacy and new writers concurrently for the same operation.
6. Repeat independently for Finland retail, Germany retail, then reseller Turkey. Do not combine customer accounts, balances, subscriptions, or approval policies across these deployments.

## Rollback

If no new customer write has happened, stop the v2 bot and backend worker for that deployment, restore the recorded legacy bot/worker configuration, and resume the unchanged legacy database/panel state. If any v2 purchase, credit, refund, trial, cancellation, or panel write happened, do not simply restart legacy writers: freeze both sides, reconcile every ledger entry and remote client, export new-backend obligations, and obtain an operator-reviewed reverse/conversion plan. Backups are recovery inputs, not permission to overwrite newer financial or panel state.
