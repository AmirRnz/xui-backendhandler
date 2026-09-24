#!/usr/bin/env bash
set -Eeuo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
SOURCE_DIR="${XUI_BACKEND_SOURCE_DIR:-$SCRIPT_DIR}"
BIN_DIR="/usr/local/bin"
CONFIG_DIR="/etc/xui-backend"
INSTANCE_DIR="$CONFIG_DIR/instances"

if [[ "${EUID}" -ne 0 ]]; then echo "Run this installer with sudo or as root." >&2; exit 1; fi
for tool in go install systemctl systemd-run runuser openssl; do
  if ! command -v "$tool" >/dev/null 2>&1; then echo "Required command is missing: $tool" >&2; exit 1; fi
done
if ! go version | awk '{print $3}' | grep -Eq '^go1\.(2[5-9]|[3-9][0-9])'; then
  echo "Go 1.25 or newer is required." >&2; exit 1
fi

if [[ ! -f "$SOURCE_DIR/go.mod" || ! -d "$SOURCE_DIR/cmd/xui-backend" ]]; then
  echo "Source checkout is incomplete: $SOURCE_DIR" >&2
  echo "Run this installer from a reviewed xui-backend source checkout." >&2
  exit 1
fi
if ! command -v psql >/dev/null 2>&1 || ! command -v pg_dump >/dev/null 2>&1; then
  if command -v apt-get >/dev/null 2>&1 && apt-cache show postgresql-16 >/dev/null 2>&1; then
    read -r -p "PostgreSQL 16 is missing. Install PostgreSQL 16 and its client tools now? [y/N] " install_pg
    [[ "$install_pg" == "y" || "$install_pg" == "Y" ]] || { echo "PostgreSQL 16 is required; no database changes were made." >&2; exit 1; }
    apt-get update
    DEBIAN_FRONTEND=noninteractive apt-get install -y postgresql-16 postgresql-client-16
  else
    echo "PostgreSQL 16 and its client tools are required. Install PostgreSQL 16 for this OS, then rerun." >&2
    exit 1
  fi
fi
psql_major="$(psql --version | sed -E 's/.* ([0-9]+)\..*/\1/')"
dump_major="$(pg_dump --version | sed -E 's/.* ([0-9]+)\..*/\1/')"
if [[ "$psql_major" != "16" || "$dump_major" != "16" ]]; then
  echo "PostgreSQL client tools must both be major version 16 (psql=$psql_major, pg_dump=$dump_major)." >&2
  exit 1
fi
if ! id xui-backend >/dev/null 2>&1; then useradd --system --home-dir /var/lib/xui-backend --shell /usr/sbin/nologin xui-backend; fi
for managed_path in "$BIN_DIR" "$CONFIG_DIR" "$INSTANCE_DIR" /var/lib/xui-backend /var/backups/xui-backend; do
  if [[ -L "$managed_path" ]]; then echo "Refusing symlinked managed path: $managed_path" >&2; exit 1; fi
done
install -d -m 0755 "$BIN_DIR"
install -d -o xui-backend -g xui-backend -m 0700 /var/lib/xui-backend /var/backups/xui-backend
install -d -o root -g xui-backend -m 0750 "$CONFIG_DIR"
install -d -o root -g xui-backend -m 0710 "$INSTANCE_DIR"

stage_dir="$(mktemp -d /var/lib/xui-backend/.installer.XXXXXX)"
chown xui-backend:xui-backend "$stage_dir"
chmod 0700 "$stage_dir"
trap 'rm -rf -- "$stage_dir"' EXIT
stage_binary="$stage_dir/xui-backend"
(cd "$SOURCE_DIR" && go build -trimpath -o "$stage_binary" ./cmd/xui-backend)
chown xui-backend:xui-backend "$stage_binary"
chmod 0755 "$stage_binary"

migration_binary="$stage_binary"
if [[ -e "$BIN_DIR/xui-backend" ]]; then
  if [[ -L "$BIN_DIR/xui-backend" ]]; then echo "Refusing symlinked xui-backend executable." >&2; exit 1; fi
  if ! cmp -s "$stage_binary" "$BIN_DIR/xui-backend"; then
    echo "An xui-backend executable exists and differs from this source; refusing to overwrite it." >&2
    exit 1
  fi
  migration_binary="$BIN_DIR/xui-backend"
fi

cat > "$stage_dir/xui-backend.service" <<'EOF'
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
unit_path=/etc/systemd/system/xui-backend.service
install_unit=1
if [[ -e "$unit_path" ]]; then
  if [[ -L "$unit_path" ]]; then echo "Refusing symlinked xui-backend systemd unit." >&2; exit 1; fi
  if ! cmp -s "$stage_dir/xui-backend.service" "$unit_path"; then
    echo "An xui-backend systemd unit exists and differs from this installer; refusing to overwrite it." >&2
    exit 1
  fi
  install_unit=0
fi

if [[ -L "$CONFIG_DIR/backend.env" || ( -e "$CONFIG_DIR/backend.env" && ! -f "$CONFIG_DIR/backend.env" ) ]]; then
  echo "backend.env must be a regular file; refusing to follow or replace it." >&2
  exit 1
fi
if [[ ! -e "$CONFIG_DIR/backend.env" ]]; then
  install -o root -g xui-backend -m 0640 /dev/null "$CONFIG_DIR/backend.env"
  cat > "$CONFIG_DIR/backend.env" <<'EOF'
# Configure DATABASE_URL and scoped bot credentials through the xui-backend menu.
EOF
  chown root:xui-backend "$CONFIG_DIR/backend.env"
  chmod 0640 "$CONFIG_DIR/backend.env"
fi

db_url="$(awk -F= '/^DATABASE_URL=/{sub(/^[^=]*=/, ""); print; exit}' "$CONFIG_DIR/backend.env")"
managed_db=0
if grep -q '^# XUI_BACKEND_INSTALLER_MANAGED_DATABASE=1$' "$CONFIG_DIR/backend.env"; then managed_db=1; fi
if [[ -z "$db_url" ]]; then
  db_user=xui_backend
  db_name=xui_backend
  if ! systemctl start postgresql; then echo "Could not start PostgreSQL." >&2; exit 1; fi
  role_exists="$(runuser -u postgres -- psql -X -A -t -q -c "SELECT 1 FROM pg_roles WHERE rolname='${db_user}'")"
  db_exists="$(runuser -u postgres -- psql -X -A -t -q -c "SELECT 1 FROM pg_database WHERE datname='${db_name}'")"
  if [[ "$role_exists" == "1" || "$db_exists" == "1" ]]; then
    echo "Refusing to reuse existing PostgreSQL role or database named xui_backend." >&2
    echo "No existing database or role was changed. Configure DATABASE_URL manually, then rerun." >&2
    exit 1
  fi
  db_password="$(openssl rand -hex 32)"
  db_url="postgres://${db_user}:${db_password}@127.0.0.1:5432/${db_name}?sslmode=disable"
  # Persist the generated credential before creating PostgreSQL objects. A
  # rerun can then safely finish whichever create step was interrupted.
  printf '# XUI_BACKEND_INSTALLER_MANAGED_DATABASE=1\nDATABASE_URL=%s\n' "$db_url" >> "$CONFIG_DIR/backend.env"
  managed_db=1
fi

if [[ "$managed_db" == "1" ]]; then
  if [[ ! "$db_url" =~ ^postgres://xui_backend:([a-f0-9]{64})@127\.0\.0\.1:5432/xui_backend\?sslmode=disable$ ]]; then
    echo "Installer-managed DATABASE_URL was edited and cannot be safely resumed automatically." >&2
    echo "Preserve the current environment file and configure PostgreSQL manually before rerunning." >&2
    exit 1
  fi
  db_password="${BASH_REMATCH[1]}"
  db_user=xui_backend
  db_name=xui_backend
  if ! systemctl start postgresql; then echo "Could not start PostgreSQL." >&2; exit 1; fi
  role_exists="$(runuser -u postgres -- psql -X -A -t -q -c "SELECT 1 FROM pg_roles WHERE rolname='${db_user}'")"
  if [[ "$role_exists" == "1" ]]; then
    if ! PGPASSWORD="$db_password" psql -X -h 127.0.0.1 -U "$db_user" -d postgres -Atqc 'SELECT 1' >/dev/null 2>&1; then
      echo "Existing xui_backend role credentials do not match the saved installer URL; no database objects were changed." >&2
      exit 1
    fi
  else
    runuser -u postgres -- psql -X -q -v ON_ERROR_STOP=1 <<SQL
CREATE ROLE ${db_user} LOGIN PASSWORD '${db_password}';
SQL
  fi
  db_owned="$(runuser -u postgres -- psql -X -A -t -q -c "SELECT 1 FROM pg_database WHERE datname='${db_name}' AND pg_get_userbyid(datdba)='${db_user}'")"
  db_exists="$(runuser -u postgres -- psql -X -A -t -q -c "SELECT 1 FROM pg_database WHERE datname='${db_name}'")"
  if [[ "$db_owned" == "1" ]]; then
    if ! PGPASSWORD="$db_password" psql -X -h 127.0.0.1 -U "$db_user" -d "$db_name" -Atqc 'SELECT 1' >/dev/null 2>&1; then
      echo "Saved xui_backend credentials cannot access the owned database; no existing data was changed." >&2
      exit 1
    fi
  elif [[ "$db_exists" == "1" ]]; then
    echo "Database xui_backend exists but is not owned by the installer role; refusing to change it." >&2
    exit 1
  else
    runuser -u postgres -- psql -X -q -v ON_ERROR_STOP=1 <<SQL
CREATE DATABASE ${db_name} OWNER ${db_user};
SQL
  fi
  unset db_password
fi
if ! grep -q '^BACKEND_PANEL_SECRETS_KEY=' "$CONFIG_DIR/backend.env"; then
  if [[ "$managed_db" != "1" ]]; then
    echo "BACKEND_PANEL_SECRETS_KEY is missing for the configured external database." >&2
    echo "Restore the original key from protected backup; generating a new key can make stored encrypted panel or Telegram tokens unreadable." >&2
    exit 1
  fi
  if grep -q '^# XUI_BACKEND_INSTALLER_MANAGED_PANEL_KEY=1$' "$CONFIG_DIR/backend.env"; then
    echo "The previously generated BACKEND_PANEL_SECRETS_KEY is missing." >&2
    echo "Restore the original key from protected backup; the installer will not replace it." >&2
    exit 1
  fi
  if [[ ! "$db_url" =~ ^postgres://xui_backend:([a-f0-9]{64})@127\.0\.0\.1:5432/xui_backend\?sslmode=disable$ ]]; then
    echo "Cannot verify that the installer-managed database is fresh; refusing to generate an encryption key." >&2
    exit 1
  fi
  db_password="${BASH_REMATCH[1]}"
  migration_table="$(PGPASSWORD="$db_password" psql -X -h 127.0.0.1 -U xui_backend -d xui_backend -Atqc "SELECT EXISTS(SELECT 1 FROM information_schema.tables WHERE table_schema='public' AND table_name='schema_migrations')")"
  if [[ "$migration_table" == "t" ]]; then
    migration_count="$(PGPASSWORD="$db_password" psql -X -h 127.0.0.1 -U xui_backend -d xui_backend -Atqc 'SELECT count(*) FROM schema_migrations')"
    if [[ "$migration_count" != "0" ]]; then
      echo "The installer-managed database already has migrations; refusing to generate a replacement encryption key." >&2
      echo "Restore the original BACKEND_PANEL_SECRETS_KEY from protected backup." >&2
      exit 1
    fi
  fi
  other_table_count="$(PGPASSWORD="$db_password" psql -X -h 127.0.0.1 -U xui_backend -d xui_backend -Atqc "SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE c.relkind='r' AND n.nspname NOT IN ('pg_catalog','information_schema') AND n.nspname NOT LIKE 'pg_toast%' AND c.relname <> 'schema_migrations'")"
  if [[ "$other_table_count" != "0" ]]; then
    echo "The installer-managed database contains application tables; refusing to generate a replacement encryption key." >&2
    echo "Restore the original BACKEND_PANEL_SECRETS_KEY from protected backup." >&2
    exit 1
  fi
  unset db_password
  printf '# XUI_BACKEND_INSTALLER_MANAGED_PANEL_KEY=1\nBACKEND_PANEL_SECRETS_KEY=%s\n' "$(openssl rand -base64 32)" >> "$CONFIG_DIR/backend.env"
fi
if ! grep -q '^BACKEND_CLIENTS_JSON=' "$CONFIG_DIR/backend.env"; then
  printf 'BACKEND_CLIENTS_JSON=[]\n' >> "$CONFIG_DIR/backend.env"
fi
chown root:xui-backend "$CONFIG_DIR/backend.env"
chmod 0640 "$CONFIG_DIR/backend.env"

systemd-run --quiet --wait --collect --unit="xui-backend-initial-migrate-$$" \
  --property=User=xui-backend --property=Group=xui-backend \
  --property="EnvironmentFile=$CONFIG_DIR/backend.env" \
  "$migration_binary" migrate
if [[ ! -e "$BIN_DIR/xui-backend" ]]; then
  install -o root -g root -m 0755 "$stage_binary" "$BIN_DIR/xui-backend"
fi
if [[ "$install_unit" == "1" ]]; then
  install -o root -g root -m 0644 "$stage_dir/xui-backend.service" "$unit_path"
fi
systemctl daemon-reload
echo "Installed xui-backend. Run 'sudo xui-backend' to configure instances and credentials."
echo "PostgreSQL 16 is initialized, the backend encryption key is stored, and migrations are applied."
echo "Backend and bot services remain stopped until you add an instance in the CLI."
