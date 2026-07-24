#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

usage() {
  echo "usage: restore-control-compose.sh [--verify-only] [snapshot]" >&2
}

verify_only=0
snapshot="latest"
if [[ "${1-}" == "--verify-only" ]]; then
  verify_only=1
  shift
fi
if (($# > 0)); then
  snapshot="$1"
  shift
fi
if (($# != 0)); then
  usage
  exit 2
fi
if [[ ! "$snapshot" =~ ^[A-Za-z0-9._:-]+$ ]]; then
  echo "snapshot must be 'latest' or a Restic snapshot identifier" >&2
  exit 2
fi
if [[ $EUID -ne 0 ]]; then
  echo "restore-control-compose.sh must run as root" >&2
  exit 2
fi

root="${SIMPLE_CDN_ROOT:-${CDN_PLATFORM_ROOT:-/opt/cdn-platform}}"
ready_timeout="${RESTORE_CLICKHOUSE_READY_TIMEOUT_SECONDS:-120}"
operation_timeout="${RESTORE_CLICKHOUSE_OPERATION_TIMEOUT_SECONDS:-1800}"
download_timeout="${RESTORE_DOWNLOAD_TIMEOUT_SECONDS:-3600}"
for setting in ready_timeout operation_timeout download_timeout; do
  value="${!setting}"
  if [[ ! "$value" =~ ^[1-9][0-9]*$ ]] || ((value > 86400)); then
    echo "$setting must be between 1 and 86400 seconds" >&2
    exit 2
  fi
done

run_id="$(date -u +%Y%m%dT%H%M%SZ)-$$"
database_suffix="${run_id//[-T]/_}"
temporary_database=""
rollback_database=""
previous_clickhouse_database=""
clickhouse_backup_name="simple-cdn-restore-$run_id"
restore_dir=$(mktemp -d /tmp/cdn-platform-restore.XXXXXX)
prepared_root="$root/.restore-$run_id"
staged_clickhouse="$root/backup/staging/clickhouse/$clickhouse_backup_name"
old_control="$root/data/control.before-restore-$run_id"
old_tls="$root/data/control-tls.before-restore-$run_id"
old_control_env="$root/config/control.env.before-restore-$run_id"

temporary_database_created=0
production_database_renamed=0
temporary_database_promoted=0
services_stopped=0
control_swapped=0
tls_swapped=0
control_env_swapped=0
control_had_live=0
tls_had_live=0
control_env_had_live=0
cutover_complete=0
rollback_incomplete=0
maintenance_owned=0
operation_lock_held=0
control_was_running=0
renew_was_running=0
backup_was_running=0

cd "$root"
set -a
# shellcheck source=/dev/null
source config/backup.env
set +a
: "${RESTIC_REPOSITORY:?RESTIC_REPOSITORY is required}"

clickhouse_database=$(sed -nE 's/^[[:space:]]*CLICKHOUSE_DATABASE[[:space:]]*=[[:space:]]*([^[:space:]#]+)[[:space:]]*$/\1/p' config/control.env | tail -n 1)
if [[ -z "$clickhouse_database" || "$clickhouse_database" == cdn_platform ]]; then
  clickhouse_database=simple_cdn
fi
if [[ ! "$clickhouse_database" =~ ^[A-Za-z_][A-Za-z0-9_]{0,127}$ ]]; then
  echo "CLICKHOUSE_DATABASE must be a valid ClickHouse identifier" >&2
  exit 2
fi
temporary_database="${clickhouse_database}_restore_${database_suffix}"
rollback_database="${clickhouse_database}_before_restore_${database_suffix}"
if [[ ! -s "$root/config/restic-password" ]]; then
  echo "restic password file is empty: $root/config/restic-password" >&2
  exit 2
fi
if [[ $verify_only -eq 0 && -e data/control/control.db && "${ALLOW_NONEMPTY_RESTORE:-0}" != "1" ]]; then
  echo "data/control/control.db already exists; set ALLOW_NONEMPTY_RESTORE=1 for an intentional replacement" >&2
  exit 2
fi

clickhouse_query() {
  timeout --foreground "${operation_timeout}s" docker compose exec -T clickhouse clickhouse-client --query "$1"
}

database_exists() {
  local name="${1:?database name is required}"
  [[ "$(clickhouse_query "SELECT count() FROM system.databases WHERE name = '$name' FORMAT TSVRaw")" == "1" ]]
}

restart_previous_services() {
	local restart_failed=0
  if ((control_was_running)); then
		docker compose up -d control || restart_failed=1
  fi
  if ((renew_was_running)); then
		docker compose up -d control-cert-renew || restart_failed=1
  fi
  if ((backup_was_running)); then
		docker compose --profile backup up -d backup || restart_failed=1
  fi
	return "$restart_failed"
}

release_operation_lock() {
  if ((operation_lock_held)); then
    flock --unlock 9 || true
    exec 9>&-
    operation_lock_held=0
  fi
}

rollback_cutover() {
  echo "restore cutover failed; attempting rollback" >&2
  docker compose --profile backup stop control control-cert-renew backup >/dev/null 2>&1 || true

  local rollback_failed=0
  local live_removed=1

  if ((control_env_swapped)); then
    live_removed=1
    if [[ -e config/control.env || -L config/control.env ]]; then
      if ! mv config/control.env "$prepared_root/failed-control.env" 2>/dev/null; then
        echo "failed to move the restored control.env out of service" >&2
        rollback_failed=1
        live_removed=0
      fi
    fi
    if ((control_env_had_live && live_removed)) && ! mv "$old_control_env" config/control.env 2>/dev/null; then
      echo "failed to restore the previous control.env" >&2
      rollback_failed=1
    fi
  fi
  if ((tls_swapped)); then
    live_removed=1
    if [[ -e data/control-tls || -L data/control-tls ]]; then
      if ! mv data/control-tls "$prepared_root/failed-control-tls" 2>/dev/null; then
        echo "failed to move the restored control TLS directory out of service" >&2
        rollback_failed=1
        live_removed=0
      fi
    fi
    if ((tls_had_live && live_removed)) && ! mv "$old_tls" data/control-tls 2>/dev/null; then
      echo "failed to restore the previous control TLS directory" >&2
      rollback_failed=1
    fi
  fi
  if ((control_swapped)); then
    live_removed=1
    if [[ -e data/control || -L data/control ]]; then
      if ! mv data/control "$prepared_root/failed-control" 2>/dev/null; then
        echo "failed to move the restored control data directory out of service" >&2
        rollback_failed=1
        live_removed=0
      fi
    fi
    if ((control_had_live && live_removed)) && ! mv "$old_control" data/control 2>/dev/null; then
      echo "failed to restore the previous control data directory" >&2
      rollback_failed=1
    fi
  fi

  if ((temporary_database_promoted)); then
    local current_exists=0
    local temporary_exists=0
    if database_exists "$clickhouse_database"; then current_exists=1; fi
    if database_exists "$temporary_database"; then temporary_exists=1; fi
    if ((current_exists && !temporary_exists)) && clickhouse_query "RENAME DATABASE $clickhouse_database TO $temporary_database" >/dev/null 2>&1; then
      temporary_database_created=1
      temporary_database_promoted=0
    elif ((!current_exists && temporary_exists)); then
      temporary_database_created=1
      temporary_database_promoted=0
    else
      echo "failed to demote the restored ClickHouse database" >&2
      rollback_failed=1
    fi
  fi
  if ((production_database_renamed)); then
    local current_exists=0
    local rollback_exists=0
    if database_exists "$clickhouse_database"; then current_exists=1; fi
    if database_exists "$rollback_database"; then rollback_exists=1; fi
    if ((rollback_exists && !current_exists)); then
      if ! clickhouse_query "RENAME DATABASE $rollback_database TO $previous_clickhouse_database" >/dev/null 2>&1; then
        echo "failed to restore the previous ClickHouse database" >&2
        rollback_failed=1
      fi
    elif ((!rollback_exists && current_exists)); then
      production_database_renamed=0
    else
      echo "cannot determine the previous ClickHouse database state" >&2
      rollback_failed=1
    fi
  fi
  if ((rollback_failed)); then
    rollback_incomplete=1
    release_operation_lock
    echo "automatic rollback is incomplete; control, certificate, and backup writers remain stopped" >&2
    return 1
  fi
  release_operation_lock
  if ! restart_previous_services; then
    rollback_incomplete=1
    echo "rollback completed but one or more previous services could not be restarted" >&2
    return 1
  fi
  echo "automatic rollback completed" >&2
}

cleanup() {
  exit_code=$?
  trap - EXIT
  set +e
  if ((exit_code != 0 && services_stopped && !cutover_complete)); then
    rollback_cutover || true
  fi
  if ((temporary_database_created && !rollback_incomplete)); then
    clickhouse_query "DROP DATABASE IF EXISTS $temporary_database SYNC" >/dev/null 2>&1 || true
  fi
  rm -rf "$restore_dir"
  release_operation_lock
  if ((rollback_incomplete)); then
    echo "rollback evidence retained at $prepared_root and $staged_clickhouse" >&2
  else
    if ((maintenance_owned)); then
      rm -f "$root/backup/online-restore/maintenance.lock"
    fi
    rm -rf "$prepared_root" "$staged_clickhouse"
  fi
  exit "$exit_code"
}
trap cleanup EXIT

service_is_running() {
  docker compose --profile backup ps --status running --services 2>/dev/null | grep -Fxq "$1"
}
if service_is_running control; then control_was_running=1; fi
if service_is_running control-cert-renew; then renew_was_running=1; fi
if service_is_running backup; then backup_was_running=1; fi

echo "Downloading Restic snapshot $snapshot while the control plane remains online"
timeout --foreground "${download_timeout}s" docker compose --profile backup run --rm \
  -v "$restore_dir:/restore" --entrypoint restic backup \
  restore "$snapshot" --target /restore

restored_staging="$restore_dir/backup/staging"
restored_database="$restored_staging/control/control.db"
restored_secrets="$restored_staging/control/control-secrets.tar.gz"
restored_tls="$restored_staging/control/control-tls.tar.gz"
restored_control_env="$restore_dir/deployment/config/control.env"
for required in "$restored_database" "$restored_secrets" "$restored_tls" "$restored_control_env"; do
  if [[ ! -r "$required" ]]; then
    echo "snapshot is missing required file: $required" >&2
    exit 1
  fi
done
restored_clickhouse=""
restored_source_database=""
shopt -s nullglob
restored_clickhouse_candidates=("$restored_staging/clickhouse/"*-current)
shopt -u nullglob
for candidate in "${restored_clickhouse_candidates[@]}"; do
  [[ -d "$candidate" ]] || continue
  if [[ -n "$restored_clickhouse" ]]; then
    echo "snapshot contains multiple ClickHouse native backups" >&2
    exit 1
  fi
  restored_clickhouse="$candidate"
  backup_directory=$(basename "$candidate")
  restored_source_database="${backup_directory%-current}"
  if [[ "$restored_source_database" == cdn-platform ]]; then
    restored_source_database=cdn_platform
  fi
done
if [[ -z "$restored_clickhouse" ]]; then
  echo "snapshot is missing the ClickHouse native backup" >&2
  exit 1
fi
if [[ ! "$restored_source_database" =~ ^[A-Za-z_][A-Za-z0-9_]{0,127}$ ]]; then
  echo "snapshot ClickHouse database name is invalid" >&2
  exit 1
fi

validate_archive() {
  local archive="${1:?archive is required}"
  local listing="$restore_dir/$(basename "$archive").list"
  tar --list --gzip --file "$archive" >"$listing"
  while IFS= read -r entry; do
    entry="${entry#./}"
    case "$entry" in
      "" ) ;;
      /* | .. | ../* | */../* | */..) echo "unsafe archive member: $entry" >&2; return 1 ;;
    esac
  done <"$listing"
}
validate_archive "$restored_secrets"
validate_archive "$restored_tls"

sqlite_result=$(docker compose --profile backup run --rm \
  -v "$restore_dir:/restore:ro" --entrypoint sqlite3 backup \
  /restore/backup/staging/control/control.db "PRAGMA quick_check;")
if [[ "${sqlite_result//$'\r'/}" != "ok" ]]; then
  echo "restored SQLite quick_check failed: $sqlite_result" >&2
  exit 1
fi
schema_version=$(docker compose --profile backup run --rm \
  -v "$restore_dir:/restore:ro" --entrypoint sqlite3 backup \
  /restore/backup/staging/control/control.db "SELECT COALESCE(MAX(version), 0) FROM schema_migrations;")
if [[ ! "$schema_version" =~ ^[1-9][0-9]*$ ]]; then
  echo "restored SQLite database has no migration history" >&2
  exit 1
fi

install -d -o 101 -g 101 -m 0750 backup/staging/clickhouse
rm -rf "$staged_clickhouse"
cp -a "$restored_clickhouse" "$staged_clickhouse"
chown -R 101:101 "$staged_clickhouse"

docker compose up -d clickhouse
deadline=$((SECONDS + ready_timeout))
until docker compose exec -T clickhouse clickhouse-client --query 'SELECT 1' >/dev/null 2>&1; do
  if ((SECONDS >= deadline)); then
    echo "ClickHouse did not become ready within ${ready_timeout}s" >&2
    exit 1
  fi
  sleep 2 & wait $!
done

clickhouse_query "DROP DATABASE IF EXISTS $temporary_database SYNC"
temporary_database_created=1
clickhouse_query "RESTORE DATABASE $restored_source_database AS $temporary_database FROM Disk('backups', '$clickhouse_backup_name')"

database_engine=$(clickhouse_query "SELECT engine FROM system.databases WHERE name = '$temporary_database' FORMAT TSVRaw")
if [[ "$database_engine" != "Atomic" ]]; then
  echo "restored ClickHouse database engine is $database_engine; Atomic is required for controlled cutover" >&2
  exit 1
fi
for table in cdn_access_logs cdn_site_minute cdn_access_to_minute; do
  if [[ "$(clickhouse_query "SELECT count() FROM system.tables WHERE database = '$temporary_database' AND name = '$table' FORMAT TSVRaw")" != "1" ]]; then
    echo "restored ClickHouse database is missing table $table" >&2
    exit 1
  fi
done
for table in cdn_access_logs cdn_site_minute; do
  check_result=$(clickhouse_query "CHECK TABLE $temporary_database.$table SETTINGS check_query_single_value_result=1 FORMAT TSVRaw")
  if [[ "$check_result" != 1$'\t'* && "$check_result" != "1" ]]; then
    echo "ClickHouse CHECK TABLE failed for $table: $check_result" >&2
    exit 1
  fi
done

if ((verify_only)); then
  clickhouse_query "DROP DATABASE $temporary_database SYNC"
  temporary_database_created=0
  echo "Restore drill succeeded for snapshot $snapshot; live data was not changed"
  exit 0
fi

mkdir -p "$prepared_root/control" "$prepared_root/control-tls"
install -o 10001 -g 10001 -m 0600 "$restored_database" "$prepared_root/control/control.db"
tar --extract --gzip --no-same-owner --no-same-permissions --file "$restored_secrets" --directory "$prepared_root/control"
tar --extract --gzip --no-same-owner --no-same-permissions --file "$restored_tls" --directory "$prepared_root/control-tls"
chown -R 10001:10001 "$prepared_root/control" "$prepared_root/control-tls"
chmod 0750 "$prepared_root/control" "$prepared_root/control-tls"
install -m 0600 "$restored_control_env" "$prepared_root/control.env"
sed -i \
  -e '/^[[:space:]]*EDGE_BINARY_SHA256[[:space:]]*=/d' \
  -e '/^[[:space:]]*CLICKHOUSE_DATABASE[[:space:]]*=/d' \
  "$prepared_root/control.env"
printf '\nCLICKHOUSE_DATABASE=%s\n' "$clickhouse_database" >>"$prepared_root/control.env"

previous_clickhouse_database="$clickhouse_database"
if [[ "$clickhouse_database" == simple_cdn ]]; then
  current_database_exists=0
  legacy_database_exists=0
  if database_exists simple_cdn; then current_database_exists=1; fi
  if database_exists cdn_platform; then legacy_database_exists=1; fi
  if ((current_database_exists && legacy_database_exists)); then
    echo "both ClickHouse databases simple_cdn and cdn_platform exist; refusing an ambiguous restore" >&2
    exit 1
  fi
  if ((!current_database_exists && legacy_database_exists)); then
    previous_clickhouse_database=cdn_platform
  fi
fi
if database_exists "$previous_clickhouse_database"; then
  current_engine=$(clickhouse_query "SELECT engine FROM system.databases WHERE name = '$previous_clickhouse_database' FORMAT TSVRaw")
  if [[ "$current_engine" != "Atomic" ]]; then
    echo "current ClickHouse database engine is $current_engine; Atomic is required for controlled cutover" >&2
    exit 1
  fi
fi

online_restore_root="$root/backup/online-restore"
maintenance_path="$online_restore_root/maintenance.lock"
operation_lock="$online_restore_root/operations.lock"
install -d -o 10001 -g 101 -m 2750 "$online_restore_root"
touch "$operation_lock"
chown 10001:10001 "$operation_lock"
chmod 0660 "$operation_lock"
if ! (set -o noclobber; printf '{"source":"offline-restore","run_id":"%s"}\n' "$run_id" >"$maintenance_path") 2>/dev/null; then
  echo "another online or offline restore cutover is already pending" >&2
  exit 1
fi
maintenance_owned=1
exec 9<>"$operation_lock"
if ! flock --exclusive --timeout "$ready_timeout" 9; then
  echo "backup or certificate operations did not quiesce within ${ready_timeout}s" >&2
  exit 1
fi
operation_lock_held=1

echo "Validation complete; stopping writers for the cutover"
services_stopped=1
docker compose --profile backup stop control control-cert-renew backup

if database_exists "$rollback_database"; then
  echo "rollback database already exists: $rollback_database" >&2
  exit 1
fi
if database_exists "$previous_clickhouse_database"; then
  production_database_renamed=1
  clickhouse_query "RENAME DATABASE $previous_clickhouse_database TO $rollback_database"
fi
temporary_database_promoted=1
clickhouse_query "RENAME DATABASE $temporary_database TO $clickhouse_database"
temporary_database_created=0

if [[ -e data/control ]]; then
  control_had_live=1
  mv data/control "$old_control"
fi
control_swapped=1
mv "$prepared_root/control" data/control
if [[ -e data/control-tls ]]; then
  tls_had_live=1
  mv data/control-tls "$old_tls"
fi
tls_swapped=1
mv "$prepared_root/control-tls" data/control-tls
if [[ -e config/control.env || -L config/control.env ]]; then
  control_env_had_live=1
  mv config/control.env "$old_control_env"
fi
control_env_swapped=1
mv "$prepared_root/control.env" config/control.env

release_operation_lock

docker compose up -d control-cert-bootstrap control control-cert-renew
deadline=$((SECONDS + ready_timeout))
until docker compose exec -T control curl --fail --silent --insecure https://127.0.0.1:8443/healthz >/dev/null 2>&1; do
  if ((SECONDS >= deadline)); then
    echo "restored control plane did not become healthy within ${ready_timeout}s" >&2
    exit 1
  fi
  sleep 2 & wait $!
done
docker compose --profile backup up -d backup

cutover_complete=1
services_stopped=0
echo "Restore completed from Restic snapshot $snapshot"
if ((production_database_renamed)); then
  echo "Rollback ClickHouse database retained as $rollback_database"
fi
echo "Rollback filesystem retained at $old_control, $old_tls, and $old_control_env"
