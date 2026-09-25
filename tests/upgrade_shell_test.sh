#!/usr/bin/env bash
set -Eeuo pipefail
repo="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
bash -n "$repo/upgrade.sh"
if output="$(bash "$repo/upgrade.sh" 2>&1)"; then
  echo "upgrade.sh accepted an unprivileged invocation" >&2
  exit 1
fi
[[ "$output" == *"sudo or as root"* ]]

tmp="$(mktemp -d)"
trap 'rm -rf -- "$tmp"' EXIT
root="$tmp/root"
bin="$tmp/bin"
db_dir="$tmp/databases"
mkdir -p "$root/usr/local/bin" "$root/etc/xui-backend/instances" "$root/etc/systemd/system" "$root/var/lib/xui-backend" "$root/backup-success" "$root/backup-failure" "$root/backup-drop-failure" "$root/backup-mode-0600" "$root/source/cmd/xui-backend" "$bin" "$db_dir"
printf '#!/bin/sh\nexit 0\n' > "$root/usr/local/bin/xui-backend"
chmod 0755 "$root/usr/local/bin/xui-backend"
cat > "$root/etc/xui-backend/backend.env" <<'ENV'
DATABASE_URL=postgres://source:src-secret@127.0.0.1:5432/source?sslmode=disable
ENV
chmod 0640 "$root/etc/xui-backend/backend.env"
cat > "$root/etc/systemd/system/xui-backend.service" <<'UNIT'
[Unit]
Description=XUI Backend commerce authority
After=network-online.target postgresql.service
Wants=network-online.target

[Service]
Type=simple
User=xui-backend
Group=xui-backend
EnvironmentFile=/etc/xui-backend/backend.env
ExecStart=/usr/local/bin/xui-backend serve
Restart=on-failure
RestartSec=5
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=/var/lib/xui-backend /etc/xui-backend/instances

[Install]
WantedBy=multi-user.target
UNIT
cat > "$root/source/go.mod" <<'MOD'
module example.invalid/mock
MOD
printf 'DRY_RUN_DATABASE_URL=postgres://test:dry-secret@127.0.0.1:5432/postgres?sslmode=disable\n' > "$tmp/dry.env"
chmod 0600 "$tmp/dry.env"
python_real="$(command -v python3)"
stat_real="$(command -v stat)"

cat > "$bin/go" <<'MOCK'
#!/usr/bin/env bash
while (($#)); do
  if [[ "$1" == -o ]]; then
    shift
    printf '#!/bin/sh\nexit 0\n' > "$1"
    chmod 0755 "$1"
    exit 0
  fi
  shift
done
exit 2
MOCK
cat > "$bin/psql" <<'MOCK'
#!/usr/bin/env bash
for arg in "$@"; do
  if [[ "$arg" == --dbname=* ]]; then
    printf '%s\n' '001_core.sql,002_deployments.sql,003_refunds.sql,004_legacy_obligations.sql,005_plan_access.sql,006_actor_scope.sql,007_actor_subject.sql,008_admin_configuration.sql,009_admin_audit_subject_ref.sql,010_reseller_access_requests.sql,011_reseller_access_request_scope.sql,012_runtime_instances.sql,013_restore_quarantine.sql,014_instance_restore_fingerprint.sql'
    exit 0
  fi
done
printf '%s\n' '001_core.sql,002_deployments.sql,003_refunds.sql,004_legacy_obligations.sql,005_plan_access.sql,006_actor_scope.sql,007_actor_subject.sql,008_admin_configuration.sql,009_admin_audit_subject_ref.sql,010_reseller_access_requests.sql,011_reseller_access_request_scope.sql,012_runtime_instances.sql'
MOCK
cat > "$bin/pg_dump" <<'MOCK'
#!/usr/bin/env bash
while (($#)); do
  if [[ "$1" == -f ]]; then shift; printf 'fake dump' > "$1"; exit 0; fi
  shift
done
exit 2
MOCK
cat > "$bin/createdb" <<'MOCK'
#!/usr/bin/env bash
name="${@: -1}"
touch "$MOCK_DB_DIR/$name"
printf '%s' "$name" > "$MOCK_CREATED_NAME"
MOCK
cat > "$bin/dropdb" <<'MOCK'
#!/usr/bin/env bash
name="${@: -1}"
if [[ "${MOCK_DROP_FAIL:-0}" == 1 ]]; then exit 1; fi
rm -f -- "$MOCK_DB_DIR/$name"
MOCK
cat > "$bin/systemd-run" <<'MOCK'
#!/usr/bin/env bash
[[ "${MOCK_MIGRATE_FAIL:-0}" == 0 ]]
MOCK
cat > "$bin/python3" <<'MOCK'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "$MOCK_PYTHON_ARGS"
exec "$PYTHON_REAL" "$@"
MOCK
cat > "$bin/stat" <<'MOCK'
#!/usr/bin/env bash
if [[ "$1" == -c && "$2" == %G && "${@: -1}" == */etc/xui-backend/backend.env ]]; then
  printf 'xui-backend\n'
else
  exec "$STAT_REAL" "$@"
fi
MOCK
for name in systemctl pg_restore curl findmnt mv; do
  printf '#!/usr/bin/env bash\nexit 0\n' > "$bin/$name"
done
chmod 0700 "$bin"/*
export MOCK_DB_DIR="$db_dir" MOCK_CREATED_NAME="$tmp/created-name" MOCK_PYTHON_ARGS="$tmp/python-args" PYTHON_REAL="$python_real" STAT_REAL="$stat_real"

run_upgrade() {
  local backup="$1"; shift
  PATH="$bin:$PATH" XUI_BACKEND_UPGRADE_TESTING=1 XUI_BACKEND_UPGRADE_TEST_ROOT="$root" \
    bash "$repo/upgrade.sh" --backup-dir "$backup" --dry-run-db-env-file "$tmp/dry.env" --source-dir "$root/source" "$@"
}

# Successful rehearsal must remove its disposable database before reporting success.
if ! output="$(run_upgrade "$root/backup-success" 2>&1)"; then
  echo "$output" >&2
  exit 1
fi
clone="$(cat "$MOCK_CREATED_NAME")"
[[ ! -e "$db_dir/$clone" && "$output" == *"Preflight passed"* ]]
! grep -q 'dry-secret' "$MOCK_PYTHON_ARGS"

# A root-owned installer environment can use strict 0600 too.
chmod 0600 "$root/etc/xui-backend/backend.env"
if ! output="$(run_upgrade "$root/backup-mode-0600" 2>&1)"; then
  echo "$output" >&2
  exit 1
fi
clone="$(cat "$MOCK_CREATED_NAME")"
[[ ! -e "$db_dir/$clone" && "$output" == *"Preflight passed"* ]]

# A migration failure still drops the clone through the EXIT cleanup trap.
set +e
output="$(MOCK_MIGRATE_FAIL=1 run_upgrade "$root/backup-failure" 2>&1)"
status=$?
set -e
clone="$(cat "$MOCK_CREATED_NAME")"
[[ $status -ne 0 && ! -e "$db_dir/$clone" && "$output" != *"could not drop disposable"* ]]

# A failed drop is surfaced and the test confirms the clone really remains.
set +e
output="$(MOCK_DROP_FAIL=1 run_upgrade "$root/backup-drop-failure" 2>&1)"
status=$?
set -e
clone="$(cat "$MOCK_CREATED_NAME")"
[[ $status -ne 0 && -e "$db_dir/$clone" && "$output" == *"could not drop disposable rehearsal database"* ]]

chmod 0644 "$tmp/dry.env"
set +e
output="$(run_upgrade "$root/backup-failure" 2>&1)"
status=$?
set -e
[[ $status -ne 0 && "$output" == *"must not be accessible by group or other users"* ]]

chmod 0600 "$tmp/dry.env"
chmod 0644 "$root/etc/xui-backend/backend.env"
set +e
output="$(run_upgrade "$root/backup-failure" 2>&1)"
status=$?
set -e
[[ $status -ne 0 && "$output" == *"backend.env must be mode 0600 or root:xui-backend mode 0640"* ]]
echo "upgrade shell integration checks passed"
