# Unified VPN commerce backend

This Go service is the single authority for commerce, identity, trials, subscriptions, 3x-ui access, reconciliation work, and durable notifications. Telegram adapters authenticate with one deployment-scoped bearer token and send the Telegram actor ID separately. Only this repository connects to PostgreSQL or 3x-ui.

Deployment/client-service, commercial-account, and actor/role are separate records. Actors have a scoped identity provider and external subject; Telegram is the only currently authenticated provider, while the schema can represent future web/service actors. Reseller accounts can own child `reseller_customer` accounts for future reseller-owned end-user clients. Those later clients and their business rules are not implemented here.

The schema seeds three isolated deployments: `retail-finland`, `retail-germany`, and `reseller-turk1`. Configure each deployment's real panel URL and each panel's API token before any provisioning. Seeded `.invalid` URLs are intentionally nonfunctional placeholders. Set the exact tested panel API version to `3.8.5`; writes are denied for other versions until compatibility is verified.

## Local run

Use Go 1.25+ and PostgreSQL 16. Create a local database, copy `config.example.env` to an ignored `.env`, and replace every placeholder with local test credentials. Example with Docker:

```powershell
docker run --name xui-commerce-dev -e POSTGRES_DB=xui_commerce -e POSTGRES_USER=xui_app -e POSTGRES_PASSWORD=local-only -p 5432:5432 -d postgres:16
$env:DATABASE_URL = 'postgres://xui_app:local-only@127.0.0.1:5432/xui_commerce?sslmode=disable'
go run ./cmd/backend migrate
```

PowerShell does not automatically load `.env`; export its entries into the current process before serving:

```powershell
Get-Content .env | Where-Object { $_ -and -not $_.StartsWith('#') } | ForEach-Object { $name, $value = $_.Split('=', 2); [Environment]::SetEnvironmentVariable($name, $value, 'Process') }
go run ./cmd/backend serve
```

Run each Telegram adapter separately with the token for its deployment. The two retail deployments must use separate bot processes and scoped credentials even though they share the same adapter code.

Local plans and actors can be added through the SQL console. Resolve a user's actor once through `/v1/actors/resolve`, then set any required approval/role in the database as an operator. Create plans with explicit `deployment_id`, `panel_id`, `inbound_ids`, integer Toman price fields, expiry/traffic limits, and `is_global`; grant account-specific plans through `plan_access`. Payment card details live on each deployment row. Never put real credentials in this repository.

## Importing legacy data

`go run ./cmd/import-legacy --source-instance retail-finland --target-panel-id panel-retail-finland` runs a read-only dry-run report. Set `LEGACY_DATABASE_URL` to a read-only connection for exactly one legacy deployment and `DATABASE_URL` to the migrated backend database. Review the exact-source-unit financial totals, source ID collisions, remote panel identity collisions, and open obligations. After the reviewed mapping is approved, add `--apply` to archive that source. Repeat independently for `retail-germany` and `reseller-turk1`.

The importer is deliberately a lossless raw archive and obligation ledger; it does not convert legacy accounts, catalog prices, wallet openings, or subscriptions into live backend records. Currency/unit normalization, subscription ownership mapping, and remote panel reconciliation require an explicit operator-reviewed conversion before cutover. The runbook therefore blocks production cutover until that canonical conversion exists and passes its checks; running the archive alone is not cutover-ready.

## API contract and runbook

- [API contract](docs/API.md)
- [Migration and cutover runbook](docs/CUTOVER.md)

## Verification

```powershell
$env:TEST_DATABASE_URL = 'postgres://xui_test:xui_test_password@127.0.0.1:55432/xui_test?sslmode=disable'
$env:DATABASE_URL = $env:TEST_DATABASE_URL
gofmt -w .
go vet ./...
go test -count=1 -p 1 ./...
go test -race -count=1 -p 1 ./internal/integration ./internal/xui
go build ./...
```

The integration tests create private temporary schemas in the disposable database named by `TEST_DATABASE_URL`; they never use production systems.
