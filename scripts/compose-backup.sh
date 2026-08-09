#!/usr/bin/env bash
set -euo pipefail
umask 077

phase="${1:-all}"
if [[ "$phase" != "all" && "$phase" != "retention" ]]; then
  echo "usage: compose-backup.sh [retention]" >&2
  exit 2
fi

restore_root="${ONLINE_RESTORE_ROOT:-/var/lib/cdn-platform-restore}"
mkdir -p "$restore_root"
operation_lock="$restore_root/operations.lock"
touch "$operation_lock"
if [[ $EUID -eq 0 ]]; then chown 10001:10001 "$operation_lock"; fi
chmod 0660 "$operation_lock"
exec 9<>"$operation_lock"
flock --shared 9
if [[ -e "$restore_root/maintenance.lock" ]]; then
  echo "backup skipped while an online restore cutover is pending" >&2
  exit 75
fi

runtime_dir=$(mktemp -d /tmp/cdn-backup-runtime.XXXXXX)
trap 'rm -rf "$runtime_dir"' EXIT
# shellcheck source=/dev/null
source /usr/local/lib/cdn-platform/compose-backup-common.sh
load_backup_runtime "$runtime_dir"

data_dir="${CONTROL_DATA_DIR:-/var/lib/cdn-platform}"
tls_dir="${CONTROL_TLS_DIR:-/var/lib/cdn-control-tls}"
staging_dir="${BACKUP_STAGING_DIR:-/backup/staging}"
clickhouse_url="${CLICKHOUSE_URL:-http://127.0.0.1:8123}"
clickhouse_database="${CLICKHOUSE_DATABASE:-simple_cdn}"
if [[ "$clickhouse_database" == cdn_platform ]]; then
  clickhouse_database=simple_cdn
fi
snapshot_name="${clickhouse_database}-current"
control_staging="$staging_dir/control"
clickhouse_staging="$staging_dir/clickhouse/$snapshot_name"
cleanup() {
  rm -rf "$runtime_dir" "$control_staging" "$clickhouse_staging"
}
trap cleanup EXIT

required_inputs=("$RESTIC_PASSWORD_FILE")
if [[ "$phase" == "all" ]]; then
  required_inputs+=("$data_dir/control.db")
fi
for required in "${required_inputs[@]}"; do
  if [[ ! -r "$required" ]]; then
    echo "required backup input is not readable: $required" >&2
    exit 2
  fi
done

if [[ "$phase" == "all" ]]; then
  rm -rf "$control_staging" "$clickhouse_staging"
  mkdir -p "$control_staging"
  sqlite3 "$data_dir/control.db" ".backup '$control_staging/control.db'"
  chmod 0600 "$control_staging/control.db"

  archive_inputs=()
  for directory in pki letsencrypt nginx-artifacts; do
    if [[ -d "$data_dir/$directory" ]]; then
      archive_inputs+=("$directory")
    fi
  done
  if ((${#archive_inputs[@]})); then
    tar --create --gzip --file "$control_staging/control-secrets.tar.gz" \
      --exclude='.nginx-download-*.tmp' --directory "$data_dir" "${archive_inputs[@]}"
  else
    tar --create --gzip --file "$control_staging/control-secrets.tar.gz" --files-from /dev/null
  fi
  tar --create --gzip --file "$control_staging/control-tls.tar.gz" \
    --exclude='./before-restore-*' --exclude='./.online-restore-*' \
    --directory "$tls_dir" .

  backup_query="BACKUP DATABASE ${clickhouse_database} TO Disk('backups', '${snapshot_name}')"
  curl --fail-with-body --silent --show-error --data-binary "$backup_query" "$clickhouse_url/"

  restic backup \
    "$control_staging" \
    "$clickhouse_staging" \
    /deployment/config/control.env \
    /deployment/config/backup.env \
    /deployment/compose.yaml \
    /deployment/.env \
    --tag cdn-control-compose
fi

if restic forget --keep-daily 7 --keep-weekly 4 --keep-monthly 6 --prune; then
  exit 0
else
  exit_code=$?
fi
if [[ "$phase" == "all" ]]; then
  echo "backup snapshot succeeded but retention failed" >&2
  exit 76
fi
exit "$exit_code"
