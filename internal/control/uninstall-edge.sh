#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

CONTROL_URL=""
TOKEN=""
NODE_ID=""
NODE_NAME=""
NODE_IPV4=""
ROOT_PREFIX="${SIMPLE_CDN_UNINSTALL_ROOT:-${CDN_PLATFORM_UNINSTALL_ROOT:-}}"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --control-url) CONTROL_URL="$2"; shift 2 ;;
    --token) TOKEN="$2"; shift 2 ;;
    --node-id) NODE_ID="$2"; shift 2 ;;
    --node-name) NODE_NAME="$2"; shift 2 ;;
    --node-ipv4) NODE_IPV4="$2"; shift 2 ;;
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
if [[ "$CONTROL_URL" != https://* || "$CONTROL_URL" == *[[:space:]]* || -z "$TOKEN" || "$TOKEN" == *[[:space:]]* ]] ||
  [[ ! "$NODE_ID" =~ ^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$ ]] ||
  [[ -z "$NODE_NAME" || -z "$NODE_IPV4" || "$NODE_IPV4" == *[[:space:]]* ]]; then
  echo "usage: uninstall-edge.sh --control-url HTTPS_URL --token TOKEN --node-id UUID --node-name NAME --node-ipv4 ADDRESS" >&2
  exit 2
fi
CONTROL_URL="${CONTROL_URL%/}"

confirm_uninstall() {
  local expected="UNINSTALL $NODE_ID" confirmation
  printf '\nDANGER: this command permanently removes the managed edge installation.\n' >&2
  printf 'Target edge node: %q\n' "$NODE_NAME" >&2
  printf 'Target node ID: %s\n' "$NODE_ID" >&2
  printf 'Target public IPv4: %s\n' "$NODE_IPV4" >&2
  printf 'Type exactly "%s" to continue:\n> ' "$expected" >&2
  if ! IFS= read -r confirmation </dev/tty; then
    echo "interactive confirmation requires a controlling terminal; nothing was removed" >&2
    exit 2
  fi
  if [[ "$confirmation" != "$expected" ]]; then
    echo "confirmation did not match; nothing was removed" >&2
    exit 1
  fi
}

confirm_uninstall

root_path() {
  printf '%s%s' "$ROOT_PREFIX" "$1"
}
link_points_to() {
  [[ -L "$1" && "$(readlink "$1")" == "$2" ]]
}
callback() {
  local action="$1"
  shift
  curl --fail --silent --show-error --connect-timeout 10 --max-time 30 --request POST \
    --header "Authorization: Bearer $TOKEN" \
    "$@" "$CONTROL_URL/api/edge/v1/uninstall/$action"
}

lock_dir=$(root_path /run/cdn-edge-agent-uninstall.lock)
if ! mkdir "$lock_dir" 2>/dev/null; then
  echo "another edge uninstall is already running" >&2
  exit 1
fi
legacy_backup=""
cleanup_temporary() {
  if [[ -n "$legacy_backup" ]]; then rm -rf "$legacy_backup"; fi
  rmdir "$lock_dir" 2>/dev/null || true
}
trap cleanup_temporary EXIT

edge_root=$(root_path /opt/cdn-edge)
marker="$edge_root/.layout-version"
layout_version=""
if [[ -f "$marker" ]]; then layout_version=$(tr -d '[:space:]' <"$marker"); fi
if [[ -n "$layout_version" && "$layout_version" != "1" && "$layout_version" != "2" ]]; then
  echo "unsupported /opt/cdn-edge layout version" >&2
  exit 1
fi

agent_unit=$(root_path /etc/systemd/system/cdn-edge-agent.service)
updater_unit=$(root_path /etc/systemd/system/cdn-edge-updater@.service)
nginx_unit=$(root_path /etc/systemd/system/nginx.service)
expected_agent_unit="$edge_root/systemd/cdn-edge-agent.service"
expected_updater_unit="$edge_root/systemd/cdn-edge-updater@.service"
expected_nginx_unit="$edge_root/systemd/nginx.service"

started=0
cleanup_committed=0
agent_was_active=0
agent_was_enabled=0
nginx_was_active=0
nginx_was_enabled=0
legacy_changed=0
report_failure() {
  local code=$?
  trap - ERR
  set +e
  if ((started == 1 && cleanup_committed == 0)); then
    if ((legacy_changed == 1)) && [[ -d "$legacy_backup" ]]; then
      legacy_http=$(root_path /etc/nginx/conf.d/cdn-platform.conf)
      legacy_stream=$(root_path /etc/nginx/modules-enabled/99-cdn-platform-stream.conf)
      legacy_root=$(root_path /etc/nginx/nginx.conf)
      rm -f "$legacy_http" "$legacy_stream" "$legacy_root"
      if [[ -e "$legacy_backup/http" ]]; then install -d -m 0755 "$(dirname "$legacy_http")"; cp -a "$legacy_backup/http" "$legacy_http"; fi
      if [[ -e "$legacy_backup/stream" ]]; then install -d -m 0755 "$(dirname "$legacy_stream")"; cp -a "$legacy_backup/stream" "$legacy_stream"; fi
      if [[ -e "$legacy_backup/root" ]]; then install -d -m 0755 "$(dirname "$legacy_root")"; cp -a "$legacy_backup/root" "$legacy_root"; fi
      nginx -t >/dev/null 2>&1 && systemctl reload nginx.service >/dev/null 2>&1 || true
    fi
    systemctl daemon-reload >/dev/null 2>&1 || true
    if ((nginx_was_enabled == 1)); then systemctl enable nginx.service >/dev/null 2>&1 || true; fi
    if ((nginx_was_active == 1)); then systemctl start nginx.service >/dev/null 2>&1 || true; fi
    if ((agent_was_enabled == 1)); then systemctl enable cdn-edge-agent.service >/dev/null 2>&1 || true; fi
    if ((agent_was_active == 1)); then systemctl start cdn-edge-agent.service >/dev/null 2>&1 || true; fi
    callback fail --header 'Content-Type: text/plain' \
      --data-binary "edge uninstall failed before local cleanup completed (exit $code)" >/dev/null 2>&1 || true
  fi
  exit "$code"
}
trap report_failure ERR

callback start >/dev/null
started=1
if systemctl is-active --quiet cdn-edge-agent.service 2>/dev/null; then agent_was_active=1; fi
if systemctl is-enabled --quiet cdn-edge-agent.service 2>/dev/null; then agent_was_enabled=1; fi
if systemctl is-active --quiet nginx.service 2>/dev/null; then nginx_was_active=1; fi
if systemctl is-enabled --quiet nginx.service 2>/dev/null; then nginx_was_enabled=1; fi

if ((agent_was_enabled == 1)); then systemctl disable cdn-edge-agent.service >/dev/null; fi
if ((agent_was_active == 1)); then systemctl stop cdn-edge-agent.service >/dev/null; fi
if [[ "$layout_version" == "2" ]]; then
  if ((nginx_was_enabled == 1)); then systemctl disable nginx.service >/dev/null; fi
  if ((nginx_was_active == 1)); then systemctl stop nginx.service >/dev/null; fi
fi

# The edge owns only these dedicated nftables tables.
if command -v nft >/dev/null 2>&1; then
  for table in simple_cdn cdn_platform; do
    if nft list table inet "$table" >/dev/null 2>&1; then nft delete table inet "$table" >/dev/null; fi
  done
fi

# Layout 1 used Debian Nginx and injected includes under /etc/nginx. Remove
# only those known files and marked lines so an operator-owned installation is
# left intact during a compatibility uninstall.
if [[ "$layout_version" == "1" ]]; then
  nginx_root=$(root_path /etc/nginx/nginx.conf)
  nginx_http=$(root_path /etc/nginx/conf.d/cdn-platform.conf)
  nginx_stream=$(root_path /etc/nginx/modules-enabled/99-cdn-platform-stream.conf)
  mkdir -p "$(root_path /tmp)"
  legacy_backup=$(mktemp -d "$(root_path /tmp/cdn-edge-uninstall.XXXXXX)")
  if [[ -e "$nginx_http" ]]; then cp -a "$nginx_http" "$legacy_backup/http"; fi
  if [[ -e "$nginx_stream" ]]; then cp -a "$nginx_stream" "$legacy_backup/stream"; fi
  if [[ -e "$nginx_root" ]]; then cp -a "$nginx_root" "$legacy_backup/root"; fi
  legacy_changed=1
  rm -f "$nginx_http" "$nginx_stream"
  if [[ -f "$nginx_root" ]] && {
    grep -Fq '# simple_cdn nginx capacity main include begin' "$nginx_root" ||
      grep -Fq '# simple_cdn nginx capacity events include begin' "$nginx_root"
  }; then
    nginx_root_next=$(mktemp "${nginx_root}.cdn-edge.XXXXXX")
    awk '
      /^[ \t]*# simple_cdn nginx capacity main include begin[ \t]*$/ { skip_main=1; next }
      skip_main { if ($0 ~ /^[ \t]*# simple_cdn nginx capacity main include end[ \t]*$/) skip_main=0; next }
      /^[ \t]*# simple_cdn nginx capacity events include begin[ \t]*$/ { skip_events=1; next }
      skip_events { if ($0 ~ /^[ \t]*# simple_cdn nginx capacity events include end[ \t]*$/) skip_events=0; next }
      { sub(/# simple_cdn nginx capacity managed (worker_processes|worker_rlimit_nofile|worker_connections): /, ""); print }
      END { if (skip_main || skip_events) exit 1 }
    ' "$nginx_root" >"$nginx_root_next"
    chmod --reference="$nginx_root" "$nginx_root_next" 2>/dev/null || chmod 0644 "$nginx_root_next"
    mv "$nginx_root_next" "$nginx_root"
  fi
  if command -v nginx >/dev/null 2>&1; then
    nginx -t
    systemctl reload nginx.service
  fi
fi

# From this point local cleanup is intentionally idempotent. If the final
# callback fails, rerunning the command only has to complete the callback.
cleanup_committed=1
if link_points_to "$agent_unit" "$expected_agent_unit"; then rm -f "$agent_unit"; fi
if link_points_to "$updater_unit" "$expected_updater_unit"; then rm -f "$updater_unit"; fi
if [[ "$layout_version" == "2" ]] && link_points_to "$nginx_unit" "$expected_nginx_unit"; then rm -f "$nginx_unit"; fi
rm -f "$(root_path /etc/logrotate.d/cdn-edge-platform)" \
  "$(root_path /usr/local/bin/cdn-edge-agent)"

sysctl_config=$(root_path /usr/local/lib/sysctl.d/40-simple-cdn-edge.conf)
sysctl_baseline="$edge_root/data/sysctl-baseline.conf"
rm -f "$sysctl_config"
if command -v sysctl >/dev/null 2>&1; then
  if [[ -s "$sysctl_baseline" ]] && ! sysctl -q -p "$sysctl_baseline" >/dev/null; then
    echo "warning: could not completely restore the pre-install sysctl baseline" >&2
  fi
  sysctl --system >/dev/null || echo "warning: one or more remaining system sysctl files could not be applied" >&2
else
  echo "warning: sysctl is unavailable; runtime values will remain until reboot" >&2
fi

rm -rf "$edge_root" \
  "$(root_path /etc/cdn-platform)" "$(root_path /var/lib/cdn-platform)" \
  "$(root_path /var/log/cdn-platform)" "$(root_path /var/cache/cdn-platform)"
systemctl daemon-reload
systemctl reset-failed cdn-edge-agent.service >/dev/null 2>&1 || true
systemctl reset-failed 'cdn-edge-updater@*.service' >/dev/null 2>&1 || true
if [[ "$layout_version" == "2" ]]; then systemctl reset-failed nginx.service >/dev/null 2>&1 || true; fi

if ! callback complete >/dev/null; then
  echo "local cleanup completed, but the control-plane callback failed; rerun this command" >&2
  exit 1
fi
trap - ERR
if [[ "$layout_version" == "2" ]]; then
  echo "simple_cdn edge agent and managed Nginx were removed."
else
  echo "simple_cdn edge components were removed; an external Nginx installation was left untouched."
fi
