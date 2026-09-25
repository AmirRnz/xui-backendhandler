# Agent instructions

## Product

- This repository is the **single** Go home for the backend, installer, CLI, and Telegram bots. Do not restore the old three-repository design. `README.md` is the project overview.
- The **end-user bot** serves retail customers buying and managing their own VPN services. The **reseller bot** serves resellers buying and managing services for their customers; approval and trial rules differ. Reseller-owned end-user bots and a reseller web panel are future products. Each current bot type can have multiple isolated instances with distinct tokens, admins, panels, and commercial data.

## Evidence before changes

- Follow the user's latest decisions. Read the relevant parts of `README.md`, `docs/`, callers, schema, migrations, and tests. Check Git status and preserve user edits. If documentation and code disagree, investigate and surface the mismatch. Never invent missing behavior or claim unverified results.
- For any 3x-ui endpoint or field changed, read its request, response, description, and schema in `3x-ui-openapi-docs.json`; compare `internal/xui` and contract tests. Retain the tested 3.8.5 write gate until other compatibility is verified.

## System invariants

- PostgreSQL owns commerce, ownership, and durable work; 3x-ui owns infrastructure state. The backend alone writes commerce data and calls 3x-ui. Bot adapters use scoped credentials. Enforce instance, role, approval, ownership, and plan access server-side; Telegram input and remote panel IDs do not prove ownership.
- Retail trials use per-user/per-plan `test_reset_days` (nonpositive means once ever). Reseller trials use approval-dependent daily quotas reset at 00:00 UTC; batch reseller trials stay disabled.
- Money is integer Toman; discounts use integer basis points. Check bounds, keep purchase terms and financial history immutable, and make concurrent/repeated money and trial actions have one effect. Reject an idempotency key reused with different input.
- Persist remote intent before 3x-ui writes. Read back ambiguous or partial outcomes; preserve unresolved work for reconciliation. Never blindly retry provisioning or refund a possibly created service. Full-row client updates must preserve unowned fields and secrets.
- An instance backup moves the **complete instance**, including commercial state, into an installed backend; a global backup moves the **whole system**. Follow `docs/BACKUP.md` for collision, panel, port, source-stop, restore, and rollback checks. Never run both copies as writers.

## Code, tests, operations

- Write small, idiomatic Go changes with explicit dependencies, handled errors, bounded inputs/timeouts, parameterized SQL, versioned migrations, and graceful shutdown. Avoid hidden globals, ignored errors, normal-flow panics, and unrelated rewrites.
- Test behavior at relevant boundaries: isolation, authorization, concurrency/idempotency, failure recovery, and uncertain panel outcomes. Use a disposable PostgreSQL 16 database and mocked Telegram/3x-ui; skipped DB tests are not passes. For changed Go code run `gofmt`, `go vet ./...`, `go test -count=1 -p 1 ./...`, and `go build ./...`; add race tests where supported.
- Touch live systems only when requested. Verify current state first; migrations/restores need a validated backup, dry run, collision report, cutover sequence, and rollback. Protect secrets and backup archives. Report changes, tests run/skipped, and remaining risks concisely.
