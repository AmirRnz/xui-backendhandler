# Unified VPN commerce backend

This Go service is the single authority for commerce, identity, trials, subscriptions, 3x-ui access, reconciliation work, and durable notifications. Telegram adapters authenticate with one deployment-scoped bearer token and send the Telegram actor ID separately. Only this repository connects to PostgreSQL or 3x-ui.

Deployment/client-service, commercial-account, and actor/role are separate records. The end-user bot serves retail customers buying and managing their own services. The reseller bot serves reseller accounts buying and managing services for their customers, with separate approval and trial rules. Multiple instances of either bot can use distinct Telegram tokens, admins, panels, and commercial data. Reseller-owned end-user bots and the reseller web panel are future products, not implemented here.

The schema seeds three isolated deployments: `retail-finland`, `retail-germany`, and `reseller-turk1`; the CLI can register additional instances. Configure each deployment's real panel URL and each panel's API token before any provisioning. Seeded `.invalid` URLs are intentionally nonfunctional placeholders. Set the exact tested panel API version to `3.8.5`; writes are denied for other versions until compatibility is verified.

## Installer and instance menu

From a reviewed checkout containing `install.sh`, on a Linux host with systemd, Go 1.25+, and PostgreSQL 16:

```sh
sudo ./install.sh
sudo xui-backend
```

`xui-backend` without arguments opens the operator menu. It can add end-user or reseller bot instances, start/stop/restart/remove their services, and create or restore instance or global backups. Adding an instance asks for its Telegram token and admin ID, 3x-ui panel URL and API token, and backend URL; a local backend URL is suggested from the configured listen port. Removing a bot revokes its API credential but retains its accounts and commerce history. See [installer and backup/restore](docs/BACKUP.md) before moving or restoring data. There is no published remote one-line installer yet.

## Reviewed GitHub → Finland delivery

For a runnable change, finish and verify it locally, push a reviewed feature branch, wait for CI, merge the reviewed commit to `main`, then use the managed `finland-mcp-server` connection to pull and deploy that exact `main` SHA. Keep the previous binary in protected rollback storage, replace the binary atomically, restart only affected units, and verify the deployed SHA, intended units, `/healthz`, and scoped access. GitHub Actions does not deploy automatically. Documentation-only changes do not require a VPS update.

At its last sync on 2026-09-25, `/root/xui-backendhandler` on Finland was locally clean on `main` at `9e77846`; GitHub `main` is now `cf3a34b`, and commits since that VPS sync are documentation-only, so `/usr/local/bin/xui-backend` remains the code-only release at `c19f8df`. `xui-backend.service` and `xui-backend-instance-kitten.service` are active, the live database has migrations 001–014, and the prior binary is retained for rollback. No off-host backup mount or protected rehearsal env file containing `DRY_RUN_DATABASE_URL` was found at the last inspection. Do not use `upgrade.sh` for this host until it supports schema 014 and those backup/rehearsal prerequisites exist.

## Local run

Use Go 1.25+ and PostgreSQL 16. Create a local database, copy `config.example.env` to an ignored `.env`, and replace every placeholder with local test credentials. Example with Docker:

```powershell
docker run --name xui-commerce-dev -e POSTGRES_DB=xui_commerce -e POSTGRES_USER=xui_app -e POSTGRES_PASSWORD=local-only -p 5432:5432 -d postgres:16
$env:DATABASE_URL = 'postgres://xui_app:local-only@127.0.0.1:5432/xui_commerce?sslmode=disable'
go run ./cmd/xui-backend migrate
```

PowerShell does not automatically load `.env`; export its entries into the current process before serving:

```powershell
Get-Content .env | Where-Object { $_ -and -not $_.StartsWith('#') } | ForEach-Object { $name, $value = $_.Split('=', 2); [Environment]::SetEnvironmentVariable($name, $value, 'Process') }
go run ./cmd/xui-backend serve
```

Each installed bot instance runs as a separate process with its own scoped credentials. The two seeded retail deployments remain isolated even though they share the same end-user adapter code.

Administrators can manage plans, payment instructions, trial settings, reseller approval policy, bot text/features, and the deployment's default panel through `/v1/admin/config`. The Telegram identity `96937669` is migrated as admin only for `retail-finland` and `reseller-turk1`; Germany remains separate. Admin writes are deployment-scoped and audited. The public `/v1/features` endpoint gives bots the corresponding deployment's feature/text configuration.

Panel tokens configured through the admin API are encrypted with AES-256-GCM. Set `BACKEND_PANEL_SECRETS_KEY` to a separately stored base64-encoded 32-byte random key (for example, generate with `openssl rand -base64 32`) in the backend service environment before using panel token updates. Keep the key out of this repository and logs, back it up securely, and retain it across restarts; losing it makes database-stored panel tokens unreadable. Admin GET responses expose only whether a token is configured. Existing environment-provided `XUI_PANEL_TOKENS_JSON` remains supported as a fallback.

## Importing legacy data

`go run ./cmd/import-legacy --source-instance retail-finland --target-panel-id panel-retail-finland` runs a read-only dry-run report. Set `LEGACY_DATABASE_URL` to a read-only connection for exactly one legacy deployment and `DATABASE_URL` to the migrated backend database. Review the exact-source-unit financial totals, source ID collisions, remote panel identity collisions, and open obligations. After the reviewed mapping is approved, add `--apply` to archive that source. Repeat independently for `retail-germany` and `reseller-turk1`.

The importer is deliberately a lossless raw archive and obligation ledger; it does not convert legacy accounts, catalog prices, wallet openings, or subscriptions into live backend records. Currency/unit normalization, subscription ownership mapping, and remote panel reconciliation require an explicit operator-reviewed conversion before cutover. The runbook therefore blocks production cutover until that canonical conversion exists and passes its checks; running the archive alone is not cutover-ready.

## API contract and runbook

- [API contract](docs/API.md)
- [Migration and cutover runbook](docs/CUTOVER.md)
- [Installer and backup/restore](docs/BACKUP.md)

## Verification

```powershell
$env:TEST_DATABASE_URL = 'postgres://xui_test:xui_test_password@127.0.0.1:55432/xui_test?sslmode=disable'
$env:DATABASE_URL = $env:TEST_DATABASE_URL
Get-ChildItem -Recurse -Filter *.go -File | ForEach-Object { gofmt -w $_.FullName }
go vet ./...
go test -count=1 -p 1 ./...
go test -race -count=1 -p 1 ./internal/integration ./internal/xui
go build ./...
```

The integration tests create private temporary schemas in the disposable database named by `TEST_DATABASE_URL`; they never use production systems.
