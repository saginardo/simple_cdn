#!/usr/bin/env bash
set -euo pipefail
umask 077

status_file="${BACKUP_STATUS_FILE:-/var/lib/cdn-platform-operations/backup.json}"
max_attempts="${BACKUP_MAX_ATTEMPTS:-3}"
retry_delays_csv="${BACKUP_RETRY_DELAYS_SECONDS:-30,120}"
backup_command="${BACKUP_COMMAND:-/usr/local/lib/cdn-platform/compose-backup.sh}"
restore_root="${ONLINE_RESTORE_ROOT:-/var/lib/cdn-platform-restore}"

mkdir -p "$restore_root"
backup_lock="$restore_root/backup.lock"
touch "$backup_lock"
if [[ $EUID -eq 0 ]]; then chown 10001:10001 "$backup_lock"; fi
chmod 0660 "$backup_lock"
exec 8<>"$backup_lock"
flock --exclusive 8

if [[ ! "$max_attempts" =~ ^[1-9][0-9]*$ ]] || ((max_attempts > 10)); then
  echo "BACKUP_MAX_ATTEMPTS must be between 1 and 10" >&2
  exit 2
fi
IFS=',' read -r -a retry_delays <<<"$retry_delays_csv"
for delay in "${retry_delays[@]}"; do
  if [[ ! "$delay" =~ ^[0-9]+$ ]] || ((delay > 86400)); then
    echo "BACKUP_RETRY_DELAYS_SECONDS must be a comma-separated list of 0-86400 second delays" >&2
    exit 2
  fi
done

started_at=$(date --iso-8601=seconds)
error_file=$(mktemp /tmp/cdn-backup-error.XXXXXX)
trap 'rm -f "$error_file"' EXIT
retention_only=0

record_status() {
  local state="${1:?backup state is required}"
  local attempt="${2:?backup attempt is required}"
  local detail="${3-}"
  if ! cdn-control backup-status "$status_file" "$state" "$attempt" "$max_attempts" "$started_at" "$detail"; then
    echo "warning: could not record backup state '$state'" >&2
  fi
  return 0
}

for ((attempt = 1; attempt <= max_attempts; attempt++)); do
  : >"$error_file"
  record_status running "$attempt"
  command=("$backup_command")
  if ((retention_only)); then
    command+=(retention)
  fi
  exec {error_writer_fd}> >(tee "$error_file" >&2)
  error_writer_pid=$!
  if "${command[@]}" 2>&"$error_writer_fd"; then
    exit_code=0
  else
    exit_code=$?
  fi
  exec {error_writer_fd}>&-
  unset error_writer_fd
  if ! wait "$error_writer_pid"; then
    echo "warning: could not capture backup command error output" >&2
  fi
  if ((exit_code == 0)); then
    record_status succeeded "$attempt"
    exit 0
  fi
	if ((exit_code == 75)); then
		record_status skipped "$attempt"
		echo "backup skipped while an online restore cutover is pending" >&2
		exit 75
	fi
	if ((exit_code == 76)); then
		retention_only=1
	fi

  detail=$(tail -n 20 "$error_file")
  if [[ -z "$detail" ]]; then
    detail="backup command exited with status $exit_code"
  fi
  if ((attempt == max_attempts)); then
    record_status failed "$attempt" "$detail"
    exit "$exit_code"
  fi

  delay_index=$((attempt - 1))
  if ((delay_index >= ${#retry_delays[@]})); then
    delay_index=$((${#retry_delays[@]} - 1))
  fi
  delay="${retry_delays[$delay_index]}"
  record_status retrying "$attempt" "$detail"
  if ((retention_only)); then
    echo "backup snapshot succeeded; retention attempt $attempt/$max_attempts failed; retrying in ${delay}s" >&2
  else
    echo "backup attempt $attempt/$max_attempts failed; retrying in ${delay}s" >&2
  fi
  sleep "$delay" & wait $!
done
