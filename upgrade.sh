#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
SOURCE_DIR="${XUI_BACKEND_SOURCE_DIR:-$SCRIPT_DIR}"
TESTING="${XUI_BACKEND_UPGRADE_TESTING:-0}"
ROOT_PREFIX=""
if [[ "$TESTING" == "1" ]]; then
  ROOT_PREFIX="${XUI_BACKEND_UPGRADE_TEST_ROOT:?test root required}"
elif [[ "${EUID}" -ne 0 ]]; then
  echo "Run the upgrade with sudo or as root." >&2
  exit 1
fi

BIN_DIR="${ROOT_PREFIX}/usr/local/bin"
CONFIG_DIR="${ROOT_PREFIX}/etc/xui-backend"
INSTANCE_DIR="$CONFIG_DIR/instances"
UNIT_DIR="${ROOT_PREFIX}/etc/systemd/system"
STATE_DIR="${ROOT_PREFIX}/var/lib/xui-backend"
BACKEND_BIN="$BIN_DIR/xui-backend"
BACKEND_ENV="$CONFIG_DIR/backend.env"
BACKEND_UNIT="$UNIT_DIR/xui-backend.service"
BACKUP_ROOT=""
DRYRUN_ENV_FILE=""

usage() {
  cat <<'EOF'
Usage: sudo ./upgrade.sh --backup-dir <remote-mount-directory> --dry-run-db-env-file <protected-env-file> [--source-dir <checkout>]

The env file must contain DRY_RUN_DATABASE_URL pointing at the PostgreSQL
maintenance database `postgres`. Its role must be able to create/drop databases.
The script tests migrations on an isolated clone and drops that clone before
stopping any service.
EOF
}
while (($#)); do
  case "$1" in
    --backup-dir) (($# >= 2)) || { echo "--backup-dir needs a path" >&2; exit 2; }; BACKUP_ROOT="$2"; shift 2 ;;
    --dry-run-db-env-file) (($# >= 2)) || { echo "--dry-run-db-env-file needs a protected env file" >&2; exit 2; }; DRYRUN_ENV_FILE="$2"; shift 2 ;;
    --source-dir) (($# >= 2)) || { echo "--source-dir needs a path" >&2; exit 2; }; SOURCE_DIR="$2"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) echo "Unknown option." >&2; exit 2 ;;
  esac
done
if [[ -z "$BACKUP_ROOT" || -z "$DRYRUN_ENV_FILE" ]]; then usage >&2; exit 2; fi
if [[ ! -f "$SOURCE_DIR/go.mod" || ! -d "$SOURCE_DIR/cmd/xui-backend" ]]; then echo "Reviewed source checkout is incomplete." >&2; exit 1; fi
for tool in go systemctl systemd-run psql pg_dump pg_restore createdb dropdb curl python3 findmnt install cmp mv stat; do
  command -v "$tool" >/dev/null 2>&1 || { echo "A required upgrade tool is missing." >&2; exit 1; }
done
for p in "$BACKEND_BIN" "$BACKEND_ENV" "$BACKEND_UNIT" "$DRYRUN_ENV_FILE"; do
  [[ -f "$p" && ! -L "$p" ]] || { echo "A required managed file is missing or unsafe." >&2; exit 1; }
done
[[ $((8#$(stat -c '%a' "$DRYRUN_ENV_FILE") & 077)) -eq 0 ]] || { echo "Dry-run database env file must not be accessible by group or other users." >&2; exit 1; }
[[ -d "$INSTANCE_DIR" && ! -L "$INSTANCE_DIR" ]] || { echo "Managed instance directory is missing or unsafe." >&2; exit 1; }
[[ -d "$BACKUP_ROOT" && ! -L "$BACKUP_ROOT" ]] || { echo "Backup destination must be an existing directory on a remote mount." >&2; exit 1; }
backend_env_owner="$(stat -c '%U' "$BACKEND_ENV")"
backend_env_group="$(stat -c '%G' "$BACKEND_ENV")"
backend_env_mode="$(stat -c '%a' "$BACKEND_ENV")"
[[ "$TESTING" == "1" || "$backend_env_owner" == root ]] || { echo "backend.env must be root-owned." >&2; exit 1; }
[[ "$backend_env_mode" == 600 || ( "$backend_env_mode" == 640 && "$backend_env_group" == xui-backend ) ]] || { echo "backend.env must be mode 0600 or root:xui-backend mode 0640." >&2; exit 1; }
[[ "$TESTING" == "1" || "$(stat -c '%U' "$DRYRUN_ENV_FILE")" == root ]] || { echo "Dry-run database env file must be root-owned." >&2; exit 1; }
if [[ "$TESTING" != "1" ]]; then
  grep -q '^# XUI_BACKEND_INSTALLER_MANAGED_DATABASE=1$' "$BACKEND_ENV" || { echo "This is not an installer-managed backend installation." >&2; exit 1; }
  case "$(findmnt -T "$BACKUP_ROOT" -n -o FSTYPE 2>/dev/null || true)" in nfs|nfs4|cifs|smb3|sshfs|fuse.sshfs) ;; *) echo "Backup directory must be on an NFS/CIFS/SSHFS remote mount." >&2; exit 1 ;; esac
fi
for p in "$BIN_DIR" "$CONFIG_DIR" "$UNIT_DIR" "$STATE_DIR"; do [[ ! -L "$p" ]] || { echo "Refusing symlinked managed path." >&2; exit 1; }; done

mkdir -p "$STATE_DIR"
stage_dir="$(mktemp -d "$STATE_DIR/.upgrade.XXXXXX")"
chmod 0700 "$stage_dir"
cleanup_upgrade() {
  local status=0
  if [[ "${clone_created:-0}" == 1 ]]; then
    DB_HOST="$TEST_HOST"; DB_PORT="$TEST_PORT"; DB_USER="$TEST_USER"; DB_PASSWORD="$TEST_PASSWORD"; DB_NAME="$TEST_ADMIN_DB"; DB_SSLMODE="$TEST_SSLMODE"; PASSFILE="$stage_dir/test-pgpass"; set_pg
    if dropdb --if-exists --maintenance-db=postgres --force "$clone_db" >/dev/null 2>&1; then
      clone_created=0
    else
      echo "ERROR: could not drop disposable rehearsal database '$clone_db'; it may remain and requires manual cleanup." >&2
      status=1
    fi
  fi
  rm -rf -- "$stage_dir"
  return "$status"
}
trap cleanup_upgrade EXIT

expected_backend_unit="$stage_dir/xui-backend.service.expected"
cat > "$expected_backend_unit" <<'EOF'
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
EOF
cmp -s "$expected_backend_unit" "$BACKEND_UNIT" || { echo "Backend unit differs from the managed installer definition." >&2; exit 1; }
shopt -s nullglob
for unit in "$UNIT_DIR"/xui-backend-instance-*.service; do
  [[ -f "$unit" && ! -L "$unit" ]] || { echo "Unsafe instance unit found." >&2; exit 1; }
  slug="$(basename "$unit" .service)"; slug="${slug#xui-backend-instance-}"
  [[ "$slug" =~ ^[a-z0-9][a-z0-9-]{0,47}$ && -d "$INSTANCE_DIR/$slug" && ! -L "$INSTANCE_DIR/$slug" && -f "$INSTANCE_DIR/$slug/instance.env" && ! -L "$INSTANCE_DIR/$slug/instance.env" ]] || { echo "Instance unit/configuration mapping is not managed safely." >&2; exit 1; }
  grep -Fxq "ExecStart=/usr/local/bin/xui-backend run-instance $slug" "$unit" || { echo "Instance unit command does not match its managed slug." >&2; exit 1; }
done
for config in "$INSTANCE_DIR"/*/instance.env; do
  [[ -f "$config" && ! -L "$config" ]] || { echo "Unsafe instance configuration found." >&2; exit 1; }
  slug="$(basename "$(dirname "$config")")"
  [[ -f "$UNIT_DIR/xui-backend-instance-$slug.service" ]] || { echo "Instance configuration has no matching managed unit." >&2; exit 1; }
done
shopt -u nullglob

read_env_value() {
  python3 - "$1" "$2" <<'PY'
import json, pathlib, sys
for line in pathlib.Path(sys.argv[1]).read_text().splitlines():
    if not line or line.lstrip().startswith('#') or '=' not in line: continue
    key, value = line.split('=', 1)
    if key.strip() != sys.argv[2]: continue
    value = value.strip()
    if value.startswith('"'): value = json.loads(value)
    sys.stdout.write(value)
    raise SystemExit(0)
raise SystemExit(2)
PY
}
decode_b64() { printf '%s' "$1" | base64 -d; }
parse_db_url() {
  mapfile -t DB_PARTS < <(printf '%s' "$1" | python3 -c '
import base64,sys,urllib.parse
u=urllib.parse.urlparse(sys.stdin.read().strip())
if u.scheme not in ("postgres","postgresql") or not u.path.lstrip("/"): raise SystemExit(2)
v=[u.hostname or "127.0.0.1",str(u.port or 5432),urllib.parse.unquote(u.username or ""),urllib.parse.unquote(u.password or ""),urllib.parse.unquote(u.path.lstrip("/")),urllib.parse.parse_qs(u.query).get("sslmode",["prefer"])[0]]
if any(any(ord(c)<32 for c in x) for x in v): raise SystemExit(2)
for x in v: print(base64.b64encode(x.encode()).decode())
')
  [[ "${#DB_PARTS[@]}" == 6 ]] || return 1
  DB_HOST="$(decode_b64 "${DB_PARTS[0]}")"; DB_PORT="$(decode_b64 "${DB_PARTS[1]}")"
  DB_USER="$(decode_b64 "${DB_PARTS[2]}")"; DB_PASSWORD="$(decode_b64 "${DB_PARTS[3]}")"
  DB_NAME="$(decode_b64 "${DB_PARTS[4]}")"; DB_SSLMODE="$(decode_b64 "${DB_PARTS[5]}")"
  [[ -n "$DB_USER" && -n "$DB_NAME" ]]
}
append_pgpass() {
  local escape
  escape() { local s="$1"; s="${s//\\/\\\\}"; s="${s//:/\\:}"; printf '%s' "$s"; }
  printf '%s:%s:%s:%s:%s\n' "$(escape "$DB_HOST")" "$DB_PORT" "$(escape "$DB_NAME")" "$(escape "$DB_USER")" "$(escape "$DB_PASSWORD")" >> "$PASSFILE"
}
set_pg() { export PGHOST="$DB_HOST" PGPORT="$DB_PORT" PGUSER="$DB_USER" PGDATABASE="$DB_NAME" PGSSLMODE="$DB_SSLMODE" PGPASSFILE="$PASSFILE"; append_pgpass; }

db_url="$(read_env_value "$BACKEND_ENV" DATABASE_URL)" || { echo "Managed DATABASE_URL is missing." >&2; exit 1; }
test_url="$(read_env_value "$DRYRUN_ENV_FILE" DRY_RUN_DATABASE_URL)" || { echo "Dry-run maintenance URL is missing." >&2; exit 1; }
parse_db_url "$db_url" || { echo "Managed database URL is invalid." >&2; exit 1; }
SRC_HOST="$DB_HOST" SRC_PORT="$DB_PORT" SRC_USER="$DB_USER" SRC_PASSWORD="$DB_PASSWORD" SRC_NAME="$DB_NAME" SRC_SSLMODE="$DB_SSLMODE"
parse_db_url "$test_url" || { echo "Dry-run database URL is invalid." >&2; exit 1; }
[[ "$DB_NAME" == postgres && "$DB_HOST:$DB_PORT/$DB_NAME" != "$SRC_HOST:$SRC_PORT/$SRC_NAME" ]] || { echo "Dry-run URL must use a separate postgres maintenance database." >&2; exit 1; }
TEST_HOST="$DB_HOST" TEST_PORT="$DB_PORT" TEST_USER="$DB_USER" TEST_PASSWORD="$DB_PASSWORD" TEST_ADMIN_DB="$DB_NAME" TEST_SSLMODE="$DB_SSLMODE"

PASSFILE="$stage_dir/pgpass"; : > "$PASSFILE"; chmod 0600 "$PASSFILE"
DB_HOST="$SRC_HOST" DB_PORT="$SRC_PORT" DB_USER="$SRC_USER" DB_PASSWORD="$SRC_PASSWORD" DB_NAME="$SRC_NAME" DB_SSLMODE="$SRC_SSLMODE"
set_pg
src_versions="$(psql -X -qAt -v ON_ERROR_STOP=1 -c "SELECT string_agg(version, ',' ORDER BY version) FROM schema_migrations")" || { echo "Could not inspect managed schema." >&2; exit 1; }
[[ "$src_versions" == "001_core.sql,002_deployments.sql,003_refunds.sql,004_legacy_obligations.sql,005_plan_access.sql,006_actor_scope.sql,007_actor_subject.sql,008_admin_configuration.sql,009_admin_audit_subject_ref.sql,010_reseller_access_requests.sql,011_reseller_access_request_scope.sql,012_runtime_instances.sql" ]] || { echo "Upgrade requires the exact migration 012 source schema; no services were changed." >&2; exit 1; }

echo "Preflight accepted schema 012; staging protected backup and migration rehearsal."
backup_dir="$BACKUP_ROOT/xui-backend-upgrade-$(date -u +%Y%m%dT%H%M%SZ)"
mkdir -m 0700 -- "$backup_dir" || { echo "Could not create unique backup directory." >&2; exit 1; }
mkdir -m 0700 "$backup_dir/config" "$backup_dir/units" "$backup_dir/instances"
pg_dump -Fc --no-owner --no-acl -f "$backup_dir/database.dump"
chmod 0600 "$backup_dir/database.dump"
pg_restore --list "$backup_dir/database.dump" >/dev/null || { echo "Off-host database dump could not be read back; no live services were changed." >&2; exit 1; }
install -m 0600 "$BACKEND_ENV" "$backup_dir/config/backend.env"
install -m 0755 "$BACKEND_BIN" "$backup_dir/xui-backend.previous"
install -m 0644 "$BACKEND_UNIT" "$backup_dir/units/xui-backend.service"
cp -a "$INSTANCE_DIR/." "$backup_dir/instances/"
shopt -s nullglob
for unit in "$UNIT_DIR"/xui-backend-instance-*.service; do
  [[ -f "$unit" && ! -L "$unit" ]] || { echo "Unexpected instance unit file; stop." >&2; exit 1; }
  install -m 0644 "$unit" "$backup_dir/units/$(basename "$unit")"
done
shopt -u nullglob
(cd "$SOURCE_DIR" && go build -trimpath -o "$stage_dir/xui-backend.new" ./cmd/xui-backend)
chmod 0755 "$stage_dir/xui-backend.new"

# Create an isolated DB clone on the explicitly supplied disposable PG server.
test_pass="$stage_dir/test-pgpass"
PASSFILE="$test_pass"; : > "$PASSFILE"; chmod 0600 "$PASSFILE"
DB_HOST="$TEST_HOST"; DB_PORT="$TEST_PORT"; DB_USER="$TEST_USER"; DB_PASSWORD="$TEST_PASSWORD"; DB_NAME="$TEST_ADMIN_DB"; DB_SSLMODE="$TEST_SSLMODE"; set_pg
clone_db="xui_upgrade_${EUID}_$(date -u +%Y%m%d%H%M%S)_${RANDOM}"
createdb --maintenance-db=postgres "$clone_db"
clone_created=1
DB_HOST="$TEST_HOST"; DB_PORT="$TEST_PORT"; DB_USER="$TEST_USER"; DB_PASSWORD="$TEST_PASSWORD"; DB_NAME="$TEST_ADMIN_DB"; DB_SSLMODE="$TEST_SSLMODE"; PASSFILE="$test_pass"; set_pg
pg_restore --no-owner --no-acl --dbname="$clone_db" "$backup_dir/database.dump"
printf '%s\0' "$TEST_HOST" "$TEST_PORT" "$TEST_USER" "$TEST_PASSWORD" "$clone_db" "$TEST_SSLMODE" | python3 -c '
import pathlib,sys,urllib.parse
host,port,user,password,db,ssl=sys.stdin.buffer.read().decode().split("\0")[:-1]
url="postgresql://"+urllib.parse.quote(user,safe="")+":"+urllib.parse.quote(password,safe="")+"@"+host+":"+port+"/"+db+"?sslmode="+urllib.parse.quote(ssl,safe="")
env_path,out_path=sys.argv[1:]
lines=pathlib.Path(env_path).read_text().splitlines()
out=[]; found=False
for line in lines:
    if line.startswith("DATABASE_URL="):
        out.append("DATABASE_URL="+url); found=True
    else: out.append(line)
if not found: raise SystemExit("DATABASE_URL missing")
pathlib.Path(out_path).write_text("\n".join(out)+"\n")
' "$BACKEND_ENV" "$stage_dir/clone.env"
chmod 0600 "$stage_dir/clone.env"
systemd-run --quiet --wait --collect --unit="xui-backend-upgrade-dryrun-$$" --property=User=xui-backend --property=Group=xui-backend --property="EnvironmentFile=$stage_dir/clone.env" "$stage_dir/xui-backend.new" migrate
clone_versions="$(psql -X -qAt -v ON_ERROR_STOP=1 --dbname="$clone_db" -c "SELECT string_agg(version, ',' ORDER BY version) FROM schema_migrations")"
[[ "$clone_versions" == "$src_versions,013_restore_quarantine.sql,014_instance_restore_fingerprint.sql" ]] || { echo "Staged migration did not reach schema 014; live installation was untouched." >&2; exit 1; }
install -m 0755 "$stage_dir/xui-backend.new" "$backup_dir/xui-backend.new"
printf '%s\n' "$clone_versions" > "$backup_dir/verified-schema.txt"
chmod 0600 "$backup_dir/verified-schema.txt"
cleanup_upgrade
trap - EXIT
cat <<EOF
Preflight passed. No live service, database, binary, or config was changed.
Protected backup and staged binary: $backup_dir
Migration 012 -> 014 was tested on a disposable clone; the clone was removed.

This tool intentionally stops before cutover because database restore and
filesystem/systemd changes cannot be committed atomically. In a maintenance
window, record which managed units are active, stop active instance units,
then stop xui-backend.service. Atomically install xui-backend.new as
/usr/local/bin/xui-backend, run 'migrate' with the protected backend.env,
start the backend, verify /healthz and authenticated /v1/admin/config, then
start only instance units that were active before maintenance and verify each.
If any step fails, stop managed units, restore database.dump and the previous
binary/config/units from the backup, and restart only previously active units.
If database restore fails, keep all services stopped and preserve the backup.
EOF
