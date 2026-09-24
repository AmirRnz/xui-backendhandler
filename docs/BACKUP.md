# Backups and restore

`xui-backend backup global` writes one owner-only archive containing a plain SQL `pg_dump` of the configured PostgreSQL database, `backend.env`, and every registered instance's `instance.env` and optional `metadata.json`. Archives contain credentials, so keep them in a protected location and transfer them over a secure channel. The archive file is created with mode `0600`; parent directories are `0700`.

Before a global restore, stop all backend and bot instance services and take a separate copy of the current database. Restore is allowed only to an empty database. Use `restore global <archive> --dry-run` first to validate the archive, checksum, PostgreSQL connectivity, and empty target. A real restore executes the SQL in one PostgreSQL transaction. Configuration is staged separately into a new inactive review directory; this contains `backend.env` and instance configs with strict permissions, but does not register or start instances. Preserve the target `DATABASE_URL`, review credentials and panel targets, then move selected configs into the live registry and restart only after review. This avoids silently overwriting active secrets or redirecting the service to the source database.

`backup instance-config <slug>` contains only that instance's `instance.env` and optional `metadata.json`. It does not contain any customer, order, wallet, subscription, trial, work item, or other PostgreSQL data. `restore instance-config <archive> <new-slug> --dry-run` validates the archive; without `--dry-run`, it stages credentials under a new inactive review directory outside `/etc/xui-backend/instances`. It never creates a runnable service or overwrites active configs. Review and manually register an instance if appropriate. A complete isolated instance database restore is unsupported: panels and client services are shared, accounts and foreign keys cross instance boundaries, and remote panel identities have global uniqueness constraints. This remains unresolved schema and restore tooling work.

The database archive uses plain SQL to support PostgreSQL's `psql` restore path. Treat every backup archive as sensitive data. The manifest checksum detects accidental database dump corruption; it is not an authenticity signature.

## Source installer

There is no published release artifact for the new CLI yet, so avoid a `curl | sh` command that would point at a moving or stale artifact. Clone or check out the reviewed backend source on the server, then run from the repository root:

```sh
sudo ./install.sh
```

After this installer is merged to the default branch, the source-based pasteable command will be:

```sh
git clone https://github.com/AmirRnz/xui-backendhandler.git && cd xui-backendhandler && sudo ./install.sh
```

The installer builds the checked-out source's `cmd/xui-backend`, stages the binary outside `/usr/local/bin`, creates protected configuration and backup directories, and writes a systemd unit. On apt based hosts with the PostgreSQL 16 package available, it offers to install PostgreSQL 16 when the client tools are missing. It creates a dedicated `xui_backend` role and database only when neither name already exists, persists the generated database URL before creating either object, and applies migrations using the staged binary. It generates `BACKEND_PANEL_SECRETS_KEY` only for a confirmed fresh installer-managed database before any migration has applied. For an external database or a previously migrated database, a missing key stops setup with instructions to restore the original key; it never rotates a key that may decrypt stored tokens. Only after migrations succeed does it install the final executable and unit. A rerun resumes from the saved installer-managed database URL, verifies credentials and ownership, reapplies idempotent migrations, and finishes installation without replacing an existing binary or unit. Existing binaries and units are reused only when they exactly match the checked-out build and generated service definition; differing or symlinked files are left untouched and cause a clear stop. Backend and bot services stay stopped until an instance is configured. Required host tools are Go 1.25+, systemd, and PostgreSQL 16 tools; the installer is written for Linux hosts using systemd and apt when PostgreSQL needs installation.

The installer has been syntax checked with `bash -n`. It has not been run against a server, and no production host or database was touched. Review the script for the target OS before running it.
