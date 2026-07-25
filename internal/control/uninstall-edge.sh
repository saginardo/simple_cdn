#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

CONTROL_URL=""
TOKEN=""
ROOT_PREFIX="${SIMPLE_CDN_UNINSTALL_ROOT:-${CDN_PLATFORM_UNINSTALL_ROOT:-}}"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --control-url) CONTROL_URL="$2"; shift 2 ;;
    --token) TOKEN="$2"; shift 2 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

if [[ -n "$ROOT_PREFIX" ]]; then
  ROOT_PREFIX="${ROOT_PREFIX%/}"
  if [[ "$ROOT_PREFIX" != /* || "$ROOT_PREFIX" == "/" ]]; then
    echo "SIMPLE_CDN_UNINSTALL_ROOT must be an absolute non-root path" >&2
    exit 2
  fi
elif [[ $EUID -ne 0 ]]; then
  echo "edge uninstall must run as root" >&2
  exit 2
fi
if [[ "$CONTROL_URL" != https://* || -z "$TOKEN" ]]; then
  echo "usage: uninstall-edge.sh --control-url HTTPS_URL --token TOKEN" >&2
  exit 2
fi

CONTROL_URL="${CONTROL_URL%/}"
root_path() {
  printf '%s%s' "$ROOT_PREFIX" "$1"
}

lock_dir=$(root_path /run/cdn-edge-agent-uninstall.lock)
if ! mkdir "$lock_dir" 2>/dev/null; then
  echo "another edge uninstall is already running" >&2
  exit 1
fi
trap 'rmdir "$lock_dir" 2>/dev/null || true' EXIT

callback() {
  local action="$1"
  shift
  curl --fail --silent --show-error --connect-timeout 10 --max-time 30 --request POST \
    --header "Authorization: Bearer $TOKEN" \
    "$@" "$CONTROL_URL/api/edge/v1/uninstall/$action"
}

started=0
cleanup_committed=0
was_enabled=0
was_active=0
nginx_config=""
nginx_backup=""
nginx_config_present=0
nginx_stream_entry=""
nginx_stream_backup=""
nginx_stream_present=0
nginx_root_config=""
nginx_root_backup=""
nginx_root_present=0
nginx_root_next=""
config_removed=0
report_failure() {
  local code=$?
  trap - ERR
  if ((started == 1 && cleanup_committed == 0)); then
    if ((config_removed == 1)); then
      if ((nginx_config_present == 1)) && [[ -n "$nginx_backup" && -e "$nginx_backup" ]]; then
        cp -a "$nginx_backup" "$nginx_config" >/dev/null 2>&1 || true
      fi
      if ((nginx_stream_present == 1)) && [[ -n "$nginx_stream_backup" && -e "$nginx_stream_backup" ]]; then
        cp -a "$nginx_stream_backup" "$nginx_stream_entry" >/dev/null 2>&1 || true
      fi
	  if ((nginx_root_present == 1)) && [[ -n "$nginx_root_backup" && -e "$nginx_root_backup" ]]; then
		cp -a "$nginx_root_backup" "$nginx_root_config" >/dev/null 2>&1 || true
	  fi
      nginx -t >/dev/null 2>&1 && systemctl reload nginx >/dev/null 2>&1 || true
    fi
    if [[ -n "$nginx_backup" ]]; then rm -f "$nginx_backup" >/dev/null 2>&1 || true; fi
    if [[ -n "$nginx_stream_backup" ]]; then rm -f "$nginx_stream_backup" >/dev/null 2>&1 || true; fi
	if [[ -n "$nginx_root_backup" ]]; then rm -f "$nginx_root_backup" >/dev/null 2>&1 || true; fi
	if [[ -n "$nginx_root_next" ]]; then rm -f "$nginx_root_next" >/dev/null 2>&1 || true; fi
    if ((was_enabled == 1)); then systemctl enable cdn-edge-agent.service >/dev/null 2>&1 || true; fi
    if ((was_active == 1)); then systemctl start cdn-edge-agent.service >/dev/null 2>&1 || true; fi
    callback fail --header 'Content-Type: text/plain' \
      --data-binary "edge uninstall failed before local cleanup completed (exit $code)" >/dev/null 2>&1 || true
  fi
  exit "$code"
}
trap report_failure ERR

callback start >/dev/null
started=1

if systemctl is-enabled --quiet cdn-edge-agent.service 2>/dev/null; then
  was_enabled=1
fi
if systemctl is-active --quiet cdn-edge-agent.service 2>/dev/null; then
  was_active=1
fi

service_unit=$(root_path /etc/systemd/system/cdn-edge-agent.service)
updater_unit=$(root_path /etc/systemd/system/cdn-edge-updater@.service)
if [[ -e "$service_unit" ]]; then
  systemctl disable cdn-edge-agent.service >/dev/null
  systemctl stop cdn-edge-agent.service >/dev/null
else
  systemctl disable cdn-edge-agent.service >/dev/null 2>&1 || true
  systemctl stop cdn-edge-agent.service >/dev/null 2>&1 || true
fi

# The agent owns only this dedicated table. Leave all distribution and
# operator-managed firewall tables untouched.
if command -v nft >/dev/null 2>&1; then
  for table in simple_cdn cdn_platform; do
    if nft list table inet "$table" >/dev/null 2>&1; then
      nft delete table inet "$table" >/dev/null
    fi
  done
fi

nginx_config=$(root_path /etc/nginx/conf.d/cdn-platform.conf)
nginx_stream_entry=$(root_path /etc/nginx/modules-enabled/99-cdn-platform-stream.conf)
nginx_root_config=$(root_path /etc/nginx/nginx.conf)
if [[ -e "$nginx_config" ]]; then
  if ! nginx_backup=$(mktemp "$(root_path /tmp/cdn-platform-nginx.XXXXXX)"); then
    false
  fi
  cp -a "$nginx_config" "$nginx_backup"
  nginx_config_present=1
fi
if [[ -e "$nginx_stream_entry" ]]; then
  if ! nginx_stream_backup=$(mktemp "$(root_path /tmp/cdn-platform-nginx-stream.XXXXXX)"); then
    false
  fi
  cp -a "$nginx_stream_entry" "$nginx_stream_backup"
  nginx_stream_present=1
fi
if [[ -e "$nginx_root_config" ]] && {
  grep -Fq '# simple_cdn nginx capacity main include begin' "$nginx_root_config" ||
    grep -Fq '# simple_cdn nginx capacity events include begin' "$nginx_root_config"
}; then
  if ! nginx_root_backup=$(mktemp "$(root_path /tmp/cdn-platform-nginx-root.XXXXXX)"); then
    false
  fi
  cp -a "$nginx_root_config" "$nginx_root_backup"
  nginx_root_present=1
fi
if ((nginx_config_present == 1 || nginx_stream_present == 1 || nginx_root_present == 1)); then
  rm -f "$nginx_config"
  rm -f "$nginx_stream_entry"
	if ((nginx_root_present == 1)); then
	  nginx_root_next=$(mktemp "${nginx_root_config}.cdn-platform.XXXXXX")
	  if ! awk '
	    /^[ \t]*# simple_cdn nginx capacity main include begin[ \t]*$/ { skip_main = 1; next }
	    skip_main { if ($0 ~ /^[ \t]*# simple_cdn nginx capacity main include end[ \t]*$/) skip_main = 0; next }
	    /^[ \t]*# simple_cdn nginx capacity events include begin[ \t]*$/ { skip_events = 1; next }
	    skip_events { if ($0 ~ /^[ \t]*# simple_cdn nginx capacity events include end[ \t]*$/) skip_events = 0; next }
	    { sub(/# simple_cdn nginx capacity managed (worker_processes|worker_rlimit_nofile|worker_connections): /, ""); print }
	    END { if (skip_main || skip_events) exit 1 }
	  ' "$nginx_root_config" >"$nginx_root_next"; then
	    rm -f "$nginx_root_next"
	    nginx_root_next=""
	    false
	  fi
	  chmod --reference="$nginx_root_config" "$nginx_root_next" 2>/dev/null || chmod 0644 "$nginx_root_next"
	  mv "$nginx_root_next" "$nginx_root_config"
	  nginx_root_next=""
	fi
  config_removed=1
  if ! command -v nginx >/dev/null 2>&1 || ! nginx -t; then
    if ((nginx_config_present == 1)); then cp -a "$nginx_backup" "$nginx_config"; fi
    if ((nginx_stream_present == 1)); then cp -a "$nginx_stream_backup" "$nginx_stream_entry"; fi
	if ((nginx_root_present == 1)); then cp -a "$nginx_root_backup" "$nginx_root_config"; fi
    config_removed=0
    if [[ -n "$nginx_backup" ]]; then rm -f "$nginx_backup"; fi
    if [[ -n "$nginx_stream_backup" ]]; then rm -f "$nginx_stream_backup"; fi
	if [[ -n "$nginx_root_backup" ]]; then rm -f "$nginx_root_backup"; fi
    if ((was_enabled == 1)); then systemctl enable cdn-edge-agent.service >/dev/null 2>&1 || true; fi
    if ((was_active == 1)); then systemctl start cdn-edge-agent.service >/dev/null 2>&1 || true; fi
    callback fail --header 'Content-Type: text/plain' --data-binary 'Nginx validation failed after removing simple_cdn configuration' >/dev/null 2>&1 || true
    echo "Nginx validation failed; platform configuration was restored" >&2
    exit 1
  fi
  if ! systemctl reload nginx; then
    if ((nginx_config_present == 1)); then cp -a "$nginx_backup" "$nginx_config"; fi
    if ((nginx_stream_present == 1)); then cp -a "$nginx_stream_backup" "$nginx_stream_entry"; fi
	if ((nginx_root_present == 1)); then cp -a "$nginx_root_backup" "$nginx_root_config"; fi
    config_removed=0
    if [[ -n "$nginx_backup" ]]; then rm -f "$nginx_backup"; fi
    if [[ -n "$nginx_stream_backup" ]]; then rm -f "$nginx_stream_backup"; fi
	if [[ -n "$nginx_root_backup" ]]; then rm -f "$nginx_root_backup"; fi
    nginx -t >/dev/null 2>&1 && systemctl reload nginx >/dev/null 2>&1 || true
    if ((was_enabled == 1)); then systemctl enable cdn-edge-agent.service >/dev/null 2>&1 || true; fi
    if ((was_active == 1)); then systemctl start cdn-edge-agent.service >/dev/null 2>&1 || true; fi
    callback fail --header 'Content-Type: text/plain' --data-binary 'Nginx reload failed after removing simple_cdn configuration' >/dev/null 2>&1 || true
    echo "Nginx reload failed; platform configuration was restored" >&2
    exit 1
  fi
  if [[ -n "$nginx_backup" ]]; then rm -f "$nginx_backup"; fi
  if [[ -n "$nginx_stream_backup" ]]; then rm -f "$nginx_stream_backup"; fi
	if [[ -n "$nginx_root_backup" ]]; then rm -f "$nginx_root_backup"; fi
fi

# From this point the operation is intentionally idempotent. A later failure
# leaves the job running so rerunning the command can finish the callback.
cleanup_committed=1
rm -f "$service_unit" "$updater_unit"
rm -f "$(root_path /etc/logrotate.d/cdn-edge-platform)"
rm -f "$(root_path /usr/local/bin/cdn-edge-agent)"
sysctl_config=$(root_path /usr/local/lib/sysctl.d/40-simple-cdn-edge.conf)
sysctl_baseline=$(root_path /opt/cdn-edge/data/sysctl-baseline.conf)
rm -f "$sysctl_config"
if command -v sysctl >/dev/null 2>&1; then
  if [[ -s "$sysctl_baseline" ]] && ! sysctl -q -p "$sysctl_baseline" >/dev/null; then
    echo "warning: could not completely restore the pre-install sysctl baseline" >&2
  fi
  if ! sysctl --system >/dev/null; then
    echo "warning: one or more remaining system sysctl files could not be applied" >&2
  fi
else
  echo "warning: sysctl is unavailable; removed the platform profile but runtime values will remain until reboot" >&2
fi
rm -rf "$(root_path /opt/cdn-edge)" \
  "$(root_path /etc/cdn-platform)" "$(root_path /var/lib/cdn-platform)" \
  "$(root_path /var/log/cdn-platform)" "$(root_path /var/cache/cdn-platform)"
systemctl daemon-reload
systemctl reset-failed cdn-edge-agent.service >/dev/null 2>&1 || true
systemctl reset-failed 'cdn-edge-updater@*.service' >/dev/null 2>&1 || true

if ! callback complete >/dev/null; then
  echo "local cleanup completed, but the control-plane callback failed; rerun this command" >&2
  exit 1
fi
trap - ERR
echo "simple_cdn edge components were removed; Nginx remains installed."
