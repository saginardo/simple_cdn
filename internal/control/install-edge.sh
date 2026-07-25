#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

CONTROL_URL=""
ENROLLMENT_TOKEN=""
BINARY_URL=""
BINARY_FILE=""
BINARY_SHA256=""
SERVICE_FILE=""
SERVICE_SHA256=""
UPDATER_SERVICE_FILE=""
UPDATER_SERVICE_SHA256=""
READINESS_FILE=""
ROOT_PREFIX="${CDN_EDGE_INSTALL_ROOT:-}"
LAYOUT_VERSION=1

while [[ $# -gt 0 ]]; do
  case "$1" in
    --control-url) CONTROL_URL="$2"; shift 2 ;;
    --enrollment-token) ENROLLMENT_TOKEN="$2"; shift 2 ;;
    --binary-url) BINARY_URL="$2"; shift 2 ;;
    --binary-file) BINARY_FILE="$2"; shift 2 ;;
    --binary-sha256) BINARY_SHA256="$2"; shift 2 ;;
    --service-file) SERVICE_FILE="$2"; shift 2 ;;
    --service-sha256) SERVICE_SHA256="$2"; shift 2 ;;
    --updater-service-file) UPDATER_SERVICE_FILE="$2"; shift 2 ;;
    --updater-service-sha256) UPDATER_SERVICE_SHA256="$2"; shift 2 ;;
    --readiness-file) READINESS_FILE="$2"; shift 2 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

if [[ -n "$ROOT_PREFIX" ]]; then
  ROOT_PREFIX="${ROOT_PREFIX%/}"
  if [[ "$ROOT_PREFIX" != /* || "$ROOT_PREFIX" == "/" ]]; then
    echo "CDN_EDGE_INSTALL_ROOT must be an absolute non-root path" >&2
    exit 2
  fi
elif [[ $EUID -ne 0 ]]; then
  echo "edge installation must run as root" >&2
  exit 2
fi
if [[ "$CONTROL_URL" != https://* || "$CONTROL_URL" == *[[:space:]]* ||
      ( -n "$ENROLLMENT_TOKEN" && "$ENROLLMENT_TOKEN" == *[[:space:]]* ) ||
      ( -n "$BINARY_URL" && ( "$BINARY_URL" != https://* || "$BINARY_URL" == *[[:space:]]* ) ) ||
      ( -n "$BINARY_FILE" && "$BINARY_FILE" != /* ) ||
      ( -n "$SERVICE_FILE" && "$SERVICE_FILE" != /* ) ||
      ( -n "$UPDATER_SERVICE_FILE" && "$UPDATER_SERVICE_FILE" != /* ) ||
      ( -n "$READINESS_FILE" && "$READINESS_FILE" != /* ) ||
      ! "$BINARY_SHA256" =~ ^[0-9a-fA-F]{64}$ ||
      ! "$SERVICE_SHA256" =~ ^[0-9a-fA-F]{64}$ ||
      ! "$UPDATER_SERVICE_SHA256" =~ ^[0-9a-fA-F]{64}$ ||
      ( -z "$BINARY_URL" && -z "$BINARY_FILE" ) ||
      ( -n "$BINARY_URL" && -n "$BINARY_FILE" ) ]]; then
  echo "usage: install-edge.sh --control-url HTTPS_URL [--enrollment-token TOKEN] (--binary-url HTTPS_URL | --binary-file PATH) --binary-sha256 SHA256 --service-sha256 SHA256 --updater-service-sha256 SHA256" >&2
  exit 2
fi
for file in "$BINARY_FILE" "$SERVICE_FILE" "$UPDATER_SERVICE_FILE"; do
  if [[ -n "$file" && ! -f "$file" ]]; then
    echo "staged upgrade artifact is missing: $file" >&2
    exit 2
  fi
done
CONTROL_URL="${CONTROL_URL%/}"

root_path() {
  printf '%s%s' "$ROOT_PREFIX" "$1"
}
path_exists() {
  [[ -e "$1" || -L "$1" ]]
}
link_points_to() {
  [[ -L "$1" && "$(readlink "$1")" == "$2" ]]
}
copy_tree() {
  local source="$1" destination="$2"
  if [[ -d "$source" ]]; then
    cp -a "$source/." "$destination/"
  fi
}
read_environment_value() {
  local file="$1" wanted="$2" key value
  [[ -f "$file" ]] || return 1
  while IFS='=' read -r key value; do
    if [[ "$key" == "$wanted" ]]; then
      printf '%s' "$value"
      return 0
    fi
  done <"$file"
  return 1
}

ensure_nginx_temp_directories() {
  local nginx_temp_root directory name expected_uid expected_gid actual
  nginx_temp_root=$(root_path /var/lib/nginx)
  if path_exists "$nginx_temp_root" && [[ ! -d "$nginx_temp_root" || -L "$nginx_temp_root" ]]; then
    echo "Nginx temp root is not a safe directory: $nginx_temp_root" >&2
    return 1
  fi
  install -d -m 0755 "$nginx_temp_root"
  for name in body fastcgi proxy scgi uwsgi; do
    directory="$nginx_temp_root/$name"
    if path_exists "$directory" && [[ ! -d "$directory" || -L "$directory" ]]; then
      echo "Nginx temp path is not a safe directory: $directory" >&2
      return 1
    fi
    install -d -m 0700 "$directory"
    chown www-data:root "$directory"
    chmod 0700 "$directory"
  done

  if [[ -z "$ROOT_PREFIX" ]]; then
    expected_uid=$(id -u www-data)
    expected_gid=$(id -g root)
    for name in body fastcgi proxy scgi uwsgi; do
      directory="$nginx_temp_root/$name"
      actual=$(stat -c '%u:%g:%a' "$directory")
      if [[ "$actual" != "$expected_uid:$expected_gid:700" ]]; then
        echo "could not repair Nginx temp path $directory: got $actual, want $expected_uid:$expected_gid:700" >&2
        return 1
      fi
    done
  fi
}

configure_nginx_capacity_includes() {
  local source="$1" temporary
  if [[ ! -f "$source" ]]; then
    echo "Nginx main configuration is missing: $source" >&2
    return 1
  fi
  temporary=$(mktemp "${source}.cdn-platform.XXXXXX")
  if ! awk \
    -v main_include='/opt/cdn-edge/config/nginx/cdn-platform-main.conf' \
    -v events_include='/opt/cdn-edge/config/nginx/cdn-platform-events.conf' '
      function restore_managed(line) {
        sub(/# simple_cdn nginx capacity managed (worker_processes|worker_rlimit_nofile|worker_connections): /, "", line)
        return line
      }
      {
        line = $0
        if (line ~ /^[ \t]*# simple_cdn nginx capacity main include begin[ \t]*$/) { skip_main = 1; next }
        if (skip_main) {
          if (line ~ /^[ \t]*# simple_cdn nginx capacity main include end[ \t]*$/) { skip_main = 0 }
          next
        }
        if (line ~ /^[ \t]*# simple_cdn nginx capacity events include begin[ \t]*$/) { skip_events = 1; next }
        if (skip_events) {
          if (line ~ /^[ \t]*# simple_cdn nginx capacity events include end[ \t]*$/) { skip_events = 0 }
          next
        }
        line = restore_managed(line)
        if (!events_found && line ~ /^[ \t]*worker_processes[ \t]+/) {
          match(line, /^[ \t]*/)
          indent = substr(line, 1, RLENGTH)
          body = substr(line, RLENGTH + 1)
          print indent "# simple_cdn nginx capacity managed worker_processes: " body
          next
        }
        if (!events_found && line ~ /^[ \t]*worker_rlimit_nofile[ \t]+/) {
          match(line, /^[ \t]*/)
          indent = substr(line, 1, RLENGTH)
          body = substr(line, RLENGTH + 1)
          print indent "# simple_cdn nginx capacity managed worker_rlimit_nofile: " body
          next
        }
        if (!events_found && line ~ /^[ \t]*events[ \t]*\{/) {
          print "# simple_cdn nginx capacity main include begin"
          print "include " main_include ";"
          print "# simple_cdn nginx capacity main include end"
          print line
          print "    # simple_cdn nginx capacity events include begin"
          print "    include " events_include ";"
          print "    # simple_cdn nginx capacity events include end"
          events_found = 1
          events_depth = 1
          next
        }
        if (events_found && events_depth > 0 && line ~ /^[ \t]*worker_connections[ \t]+/) {
          match(line, /^[ \t]*/)
          indent = substr(line, 1, RLENGTH)
          body = substr(line, RLENGTH + 1)
          print indent "# simple_cdn nginx capacity managed worker_connections: " body
          next
        }
        print line
        if (events_found && events_depth > 0) {
          opens = gsub(/\{/, "{", line)
          closes = gsub(/\}/, "}", line)
          events_depth += opens - closes
        }
      }
      END {
        if (skip_main || skip_events || !events_found) exit 1
      }
    ' "$source" >"$temporary"; then
    rm -f "$temporary"
    echo "could not add simple_cdn Nginx capacity includes to $source" >&2
    return 1
  fi
  chmod --reference="$source" "$temporary" 2>/dev/null || chmod 0644 "$temporary"
  mv "$temporary" "$source"
}

normalize_sysctl_value() {
  awk '{$1=$1; print}' <<<"$1"
}

sysctl_baseline_has_key() {
  local file="$1" wanted="$2"
  awk -F= -v wanted="$wanted" '
    {
      key = $1
      sub(/^-/, "", key)
      gsub(/[[:space:]]/, "", key)
      if (key == wanted) found = 1
    }
    END { exit found ? 0 : 1 }
  ' "$file"
}

add_managed_sysctl() {
  local key="$1" value="$2"
  printf -- '-%s = %s\n' "$key" "$value" >>"$temporary_sysctl_config"
  managed_sysctl_keys+=("$key")
  managed_sysctl_values+=("$value")
}

try_sysctl_setting() {
  local key="$1" value="$2"
  [[ -n "${sysctl_before[$key]+present}" ]] || return 1
  sysctl -q -w "$key=$value" >/dev/null 2>&1
}

configure_default_sysctl() {
  local meminfo mem_total_kib buffer_max key value index actual desired
  local buffer_supported=1 baseline_staged config_staged
  local -a candidate_keys=(
    net.core.default_qdisc
    net.ipv4.tcp_congestion_control
    net.ipv4.tcp_mtu_probing
    net.core.rmem_max
    net.core.wmem_max
    net.ipv4.tcp_rmem
    net.ipv4.tcp_wmem
  )

  buffer_max=16777216
  meminfo=$(root_path /proc/meminfo)
  mem_total_kib=""
  if [[ -r "$meminfo" ]]; then
    mem_total_kib=$(awk '$1 == "MemTotal:" { print $2; exit }' "$meminfo")
  fi
  if [[ "$mem_total_kib" =~ ^[0-9]+$ ]]; then
    if ((mem_total_kib > 4194304)); then buffer_max=33554432; fi
  else
    echo "warning: could not read total memory; using the conservative 16 MiB TCP buffer limit" >&2
  fi

  sysctl_transaction_started=1
  : >"$sysctl_runtime_backup"
  for key in "${candidate_keys[@]}"; do
    if value=$(sysctl -n "$key" 2>/dev/null); then
      value=$(normalize_sysctl_value "$value")
      sysctl_before["$key"]="$value"
      printf '%s = %s\n' "$key" "$value" >>"$sysctl_runtime_backup"
    fi
  done

  cat >"$temporary_sysctl_config" <<EOF
# Managed by simple_cdn. Local administrators may override these defaults in /etc/sysctl.d.
# TCP buffer ceiling selected from MemTotal: ${buffer_max} bytes.
EOF

  modprobe sch_fq >/dev/null 2>&1 || true
  if try_sysctl_setting net.core.default_qdisc fq; then
    add_managed_sysctl net.core.default_qdisc fq
  else
    echo "warning: fq is unavailable; leaving net.core.default_qdisc unchanged" >&2
  fi

  modprobe tcp_bbr >/dev/null 2>&1 || true
  if try_sysctl_setting net.ipv4.tcp_congestion_control bbr; then
    add_managed_sysctl net.ipv4.tcp_congestion_control bbr
  else
    echo "warning: BBR is unavailable; leaving net.ipv4.tcp_congestion_control unchanged" >&2
  fi

  if try_sysctl_setting net.ipv4.tcp_mtu_probing 1; then
    add_managed_sysctl net.ipv4.tcp_mtu_probing 1
  else
    echo "warning: TCP MTU probing is unavailable; leaving it unchanged" >&2
  fi

  try_sysctl_setting net.core.rmem_max "$buffer_max" || buffer_supported=0
  try_sysctl_setting net.core.wmem_max "$buffer_max" || buffer_supported=0
  try_sysctl_setting net.ipv4.tcp_rmem "4096 131072 $buffer_max" || buffer_supported=0
  try_sysctl_setting net.ipv4.tcp_wmem "4096 16384 $buffer_max" || buffer_supported=0
  if ((buffer_supported == 1)); then
    add_managed_sysctl net.core.rmem_max "$buffer_max"
    add_managed_sysctl net.core.wmem_max "$buffer_max"
    add_managed_sysctl net.ipv4.tcp_rmem "4096 131072 $buffer_max"
    add_managed_sysctl net.ipv4.tcp_wmem "4096 16384 $buffer_max"
  else
    echo "warning: TCP buffer tuning is not fully supported; leaving the buffer group unchanged" >&2
  fi

  if [[ -s "$sysctl_runtime_backup" ]] && ! sysctl -q -p "$sysctl_runtime_backup" >/dev/null 2>&1; then
    echo "warning: could not completely restore sysctl values after capability probing" >&2
  fi

  if path_exists "$sysctl_baseline"; then
    cp -a "$sysctl_baseline" "$temporary_sysctl_baseline"
  else
    printf '%s\n' '# Values that preceded simple_cdn sysctl management. Used during uninstall.' >"$temporary_sysctl_baseline"
  fi
  for key in "${managed_sysctl_keys[@]}"; do
    if ! sysctl_baseline_has_key "$temporary_sysctl_baseline" "$key"; then
      printf '%s = %s\n' "$key" "${sysctl_before[$key]}" >>"$temporary_sysctl_baseline"
    fi
  done

  install -d -m 0755 "$(dirname "$sysctl_config")"
  config_staged=$(mktemp "${sysctl_config}.XXXXXX")
  install -m 0644 "$temporary_sysctl_config" "$config_staged"
  mv "$config_staged" "$sysctl_config"

  baseline_staged=$(mktemp "${sysctl_baseline}.XXXXXX")
  install -m 0600 "$temporary_sysctl_baseline" "$baseline_staged"
  mv "$baseline_staged" "$sysctl_baseline"

  if ! sysctl --system >/dev/null; then
    echo "warning: one or more system sysctl files could not be applied" >&2
  fi
  for index in "${!managed_sysctl_keys[@]}"; do
    key="${managed_sysctl_keys[$index]}"
    desired=$(normalize_sysctl_value "${managed_sysctl_values[$index]}")
    if actual=$(sysctl -n "$key" 2>/dev/null); then
      actual=$(normalize_sysctl_value "$actual")
    else
      actual="unavailable"
    fi
    if [[ "$actual" != "$desired" ]]; then
      echo "warning: effective sysctl $key=$actual differs from the simple_cdn default $desired; a later administrator setting may override it" >&2
    fi
  done
}

edge_root=$(root_path /opt/cdn-edge)
marker="$edge_root/.layout-version"
agent_unit=$(root_path /etc/systemd/system/cdn-edge-agent.service)
updater_unit=$(root_path /etc/systemd/system/cdn-edge-updater@.service)
nginx_entry=$(root_path /etc/nginx/conf.d/cdn-platform.conf)
nginx_stream_entry=$(root_path /etc/nginx/modules-enabled/99-cdn-platform-stream.conf)
nginx_default=$(root_path /etc/nginx/sites-enabled/default)
nginx_root_config=$(root_path /etc/nginx/nginx.conf)
new_unit="$edge_root/systemd/cdn-edge-agent.service"
new_updater_unit="$edge_root/systemd/cdn-edge-updater@.service"
new_nginx_config="$edge_root/config/nginx/cdn-platform.conf"
new_nginx_stream_config="$edge_root/config/nginx/cdn-platform-stream.conf"
new_nginx_main_config="$edge_root/config/nginx/cdn-platform-main.conf"
new_nginx_events_config="$edge_root/config/nginx/cdn-platform-events.conf"
logrotate_config=$(root_path /etc/logrotate.d/cdn-edge-platform)
sysctl_config=$(root_path /usr/local/lib/sysctl.d/40-simple-cdn-edge.conf)
sysctl_baseline="$edge_root/data/sysctl-baseline.conf"
old_binary=$(root_path /usr/local/bin/cdn-edge-agent)
old_config_dir=$(root_path /etc/cdn-platform)
old_state_dir=$(root_path /var/lib/cdn-platform)
old_log_dir=$(root_path /var/log/cdn-platform)
old_cache_dir=$(root_path /var/cache/cdn-platform)
expected_unit_target="$new_unit"
expected_updater_unit_target="$new_updater_unit"
expected_nginx_target="$new_nginx_config"

new_layout=0
if [[ -f "$marker" ]]; then
  if [[ "$(tr -d '[:space:]' <"$marker")" != "$LAYOUT_VERSION" ]]; then
    echo "unsupported /opt/cdn-edge layout version" >&2
    exit 1
  fi
  new_layout=1
elif [[ -d "$edge_root" && -n "$(find "$edge_root" -mindepth 1 -maxdepth 1 -print -quit)" ]]; then
  echo "$edge_root exists without a recognized layout marker; refusing to merge it" >&2
  exit 1
fi
legacy_layout=0
for path in "$old_binary" "$old_config_dir" "$old_state_dir" "$old_log_dir" "$old_cache_dir"; do
  if path_exists "$path"; then legacy_layout=1; fi
done
if path_exists "$agent_unit" && ! link_points_to "$agent_unit" "$expected_unit_target"; then legacy_layout=1; fi
if path_exists "$nginx_entry" && ! link_points_to "$nginx_entry" "$expected_nginx_target"; then legacy_layout=1; fi
if ((new_layout == 1 && legacy_layout == 1)); then
  echo "both /opt/cdn-edge and legacy CDN paths exist; refusing to guess which layout is authoritative" >&2
  exit 1
fi
if ((new_layout == 1)); then
  for required in "$new_nginx_config" "$edge_root/data/edge-client.key" "$edge_root/data/edge-client.crt" "$edge_root/data/edge-ca.crt"; do
    if [[ ! -s "$required" ]]; then
      echo "incomplete /opt/cdn-edge layout: missing ${required#"$edge_root/"}" >&2
      exit 1
    fi
  done
fi
existing_identity=0
if ((new_layout == 1)) ||
   [[ -s "$old_state_dir/edge-client.key" && -s "$old_state_dir/edge-client.crt" && -s "$old_state_dir/edge-ca.crt" ]]; then
  existing_identity=1
fi
if [[ -z "$ENROLLMENT_TOKEN" && "$existing_identity" == "0" ]]; then
  echo "an enrollment token is required because this host has no complete edge mTLS identity" >&2
  exit 1
fi

old_nginx_active=0
if systemctl is-active --quiet nginx.service 2>/dev/null; then old_nginx_active=1; fi
if [[ -z "$ROOT_PREFIX" ]]; then
  export DEBIAN_FRONTEND=noninteractive
  apt-get update
  apt-get install -y --no-install-recommends nginx libnginx-mod-stream libnginx-mod-http-lua ca-certificates curl iproute2 nftables logrotate lz4 procps kmod
fi
ensure_nginx_temp_directories

transaction_dir=$(mktemp -d "$(root_path /tmp/cdn-edge-install.XXXXXX)")
trap 'rm -rf "$transaction_dir"' EXIT
temporary_binary="$transaction_dir/cdn-edge-agent"
temporary_unit="$transaction_dir/cdn-edge-agent.service"
temporary_updater_unit="$transaction_dir/cdn-edge-updater@.service"
temporary_sysctl_config="$transaction_dir/40-simple-cdn-edge.conf"
temporary_sysctl_baseline="$transaction_dir/sysctl-baseline.next"
sysctl_runtime_backup="$transaction_dir/sysctl-runtime.backup"
if [[ -n "$BINARY_FILE" ]]; then
  cp "$BINARY_FILE" "$temporary_binary"
else
  curl --fail --location --silent --show-error "$BINARY_URL" --output "$temporary_binary"
fi
echo "$BINARY_SHA256  $temporary_binary" | sha256sum --check --status
if [[ -n "$SERVICE_FILE" ]]; then
  cp "$SERVICE_FILE" "$temporary_unit"
else
  curl --fail --location --silent --show-error "${CONTROL_URL}/install-edge.service" --output "$temporary_unit"
fi
echo "$SERVICE_SHA256  $temporary_unit" | sha256sum --check --status
if [[ -n "$UPDATER_SERVICE_FILE" ]]; then
  cp "$UPDATER_SERVICE_FILE" "$temporary_updater_unit"
else
  curl --fail --location --silent --show-error "${CONTROL_URL}/install-edge-updater.service" --output "$temporary_updater_unit"
fi
echo "$UPDATER_SERVICE_SHA256  $temporary_updater_unit" | sha256sum --check --status
if ! grep -Fqx 'ExecStart=/opt/cdn-edge/bin/cdn-edge-agent' "$temporary_unit" ||
   ! grep -Fqx 'EnvironmentFile=/opt/cdn-edge/config/edge.env' "$temporary_unit"; then
  echo "downloaded edge service does not match the /opt/cdn-edge layout" >&2
  exit 1
fi
if ! grep -Fqx 'ExecStart=/opt/cdn-edge/bin/cdn-edge-agent upgrade-helper %i' "$temporary_updater_unit" ||
   ! grep -Fqx 'EnvironmentFile=/opt/cdn-edge/config/edge.env' "$temporary_updater_unit"; then
  echo "downloaded edge updater service does not match the /opt/cdn-edge layout" >&2
  exit 1
fi

old_agent_active=0
old_agent_enabled=0
if systemctl is-active --quiet cdn-edge-agent.service 2>/dev/null; then old_agent_active=1; fi
if systemctl is-enabled --quiet cdn-edge-agent.service 2>/dev/null; then old_agent_enabled=1; fi
for item in unit updater-unit nginx stream-entry stream-config main-config events-config nginx-root logrotate default-site; do
  case "$item" in
    unit) source="$agent_unit" ;;
    updater-unit) source="$updater_unit" ;;
    nginx) source="$nginx_entry" ;;
    stream-entry) source="$nginx_stream_entry" ;;
    stream-config) source="$new_nginx_stream_config" ;;
    main-config) source="$new_nginx_main_config" ;;
    events-config) source="$new_nginx_events_config" ;;
    nginx-root) source="$nginx_root_config" ;;
    logrotate) source="$logrotate_config" ;;
    default-site) source="$nginx_default" ;;
  esac
  if path_exists "$source"; then cp -a "$source" "$transaction_dir/$item.backup"; fi
done
if path_exists "$sysctl_config"; then cp -a "$sysctl_config" "$transaction_dir/sysctl-config.backup"; fi
if path_exists "$sysctl_baseline"; then cp -a "$sysctl_baseline" "$transaction_dir/sysctl-baseline.backup"; fi
if ((new_layout == 1)); then
  for item in bin/cdn-edge-agent config/edge.env config/nginx/cdn-platform-main.conf config/nginx/cdn-platform-events.conf systemd/cdn-edge-agent.service systemd/cdn-edge-updater@.service; do
    if path_exists "$edge_root/$item"; then
      mkdir -p "$transaction_dir/new-layout/$(dirname "$item")"
      cp -a "$edge_root/$item" "$transaction_dir/new-layout/$item"
    fi
  done
fi

log_moved=0
committed=0
sysctl_transaction_started=0
declare -A sysctl_before=()
managed_sysctl_keys=()
managed_sysctl_values=()
rollback() {
  local code=$?
  trap - ERR
  if ((committed == 0)); then
    systemctl stop cdn-edge-agent.service >/dev/null 2>&1 || true
    systemctl disable cdn-edge-agent.service >/dev/null 2>&1 || true
    rm -f "$agent_unit" "$updater_unit" "$nginx_entry" "$nginx_stream_entry" "$nginx_default"
    if path_exists "$transaction_dir/unit.backup"; then cp -a "$transaction_dir/unit.backup" "$agent_unit"; fi
    if path_exists "$transaction_dir/updater-unit.backup"; then cp -a "$transaction_dir/updater-unit.backup" "$updater_unit"; fi
    if path_exists "$transaction_dir/nginx.backup"; then cp -a "$transaction_dir/nginx.backup" "$nginx_entry"; fi
    if path_exists "$transaction_dir/stream-entry.backup"; then cp -a "$transaction_dir/stream-entry.backup" "$nginx_stream_entry"; fi
    if path_exists "$transaction_dir/stream-config.backup"; then
      cp -a "$transaction_dir/stream-config.backup" "$new_nginx_stream_config"
    else
      rm -f "$new_nginx_stream_config"
    fi
    if path_exists "$transaction_dir/main-config.backup"; then
      cp -a "$transaction_dir/main-config.backup" "$new_nginx_main_config"
    else
      rm -f "$new_nginx_main_config"
    fi
    if path_exists "$transaction_dir/events-config.backup"; then
      cp -a "$transaction_dir/events-config.backup" "$new_nginx_events_config"
    else
      rm -f "$new_nginx_events_config"
    fi
    if path_exists "$transaction_dir/nginx-root.backup"; then cp -a "$transaction_dir/nginx-root.backup" "$nginx_root_config"; fi
    if path_exists "$transaction_dir/logrotate.backup"; then
      mkdir -p "$(dirname "$logrotate_config")"
      cp -a "$transaction_dir/logrotate.backup" "$logrotate_config"
    else
      rm -f "$logrotate_config"
    fi
    if ((sysctl_transaction_started == 1)); then
      if path_exists "$transaction_dir/sysctl-config.backup"; then
        mkdir -p "$(dirname "$sysctl_config")"
        cp -a "$transaction_dir/sysctl-config.backup" "$sysctl_config"
      else
        rm -f "$sysctl_config"
      fi
      if path_exists "$transaction_dir/sysctl-baseline.backup"; then
        mkdir -p "$(dirname "$sysctl_baseline")"
        cp -a "$transaction_dir/sysctl-baseline.backup" "$sysctl_baseline"
      else
        rm -f "$sysctl_baseline"
      fi
      if [[ -s "$sysctl_runtime_backup" ]]; then
        sysctl -q -p "$sysctl_runtime_backup" >/dev/null 2>&1 || true
      fi
    fi
    if path_exists "$transaction_dir/default-site.backup"; then cp -a "$transaction_dir/default-site.backup" "$nginx_default"; fi
    if ((log_moved == 1)) && path_exists "$edge_root/logs/access.json"; then
      mkdir -p "$old_log_dir"
      mv "$edge_root/logs/access.json" "$old_log_dir/access.json" || true
    fi
    if ((new_layout == 0)); then
      rm -rf "$edge_root"
    else
      for item in bin/cdn-edge-agent config/edge.env config/nginx/cdn-platform-main.conf config/nginx/cdn-platform-events.conf systemd/cdn-edge-agent.service systemd/cdn-edge-updater@.service; do
        rm -f "$edge_root/$item"
        if path_exists "$transaction_dir/new-layout/$item"; then
          mkdir -p "$edge_root/$(dirname "$item")"
          cp -a "$transaction_dir/new-layout/$item" "$edge_root/$item"
        fi
      done
    fi
    systemctl daemon-reload >/dev/null 2>&1 || true
    if ((old_nginx_active == 1)); then
      if ((legacy_layout == 1)); then
        nginx -t >/dev/null 2>&1 && systemctl restart nginx.service >/dev/null 2>&1 || true
      elif systemctl is-active --quiet nginx.service 2>/dev/null; then
        nginx -t >/dev/null 2>&1 && systemctl reload nginx.service >/dev/null 2>&1 || true
      else
        nginx -t >/dev/null 2>&1 && systemctl start nginx.service >/dev/null 2>&1 || true
      fi
    else
      systemctl stop nginx.service >/dev/null 2>&1 || true
    fi
    if ((old_agent_enabled == 1)); then systemctl enable cdn-edge-agent.service >/dev/null 2>&1 || true; fi
    if ((old_agent_active == 1)); then systemctl start cdn-edge-agent.service >/dev/null 2>&1 || true; fi
  fi
  rm -rf "$transaction_dir"
  exit "$code"
}
trap rollback ERR

if ((old_agent_active == 1)); then systemctl stop cdn-edge-agent.service; fi
install -d -m 0755 "$edge_root" "$edge_root/bin" "$edge_root/systemd"
install -d -m 0750 "$edge_root/config" "$edge_root/config/nginx" "$edge_root/data" "$edge_root/logs"
install -d -m 0755 "$(dirname "$nginx_stream_entry")"
install -d -m 0700 "$edge_root/config/certs"
install -d -m 0750 "$edge_root/cache"
if [[ -z "$ROOT_PREFIX" ]]; then chown www-data:www-data "$edge_root/cache"; fi

poll_seconds=30
if ((legacy_layout == 1)); then
  if value=$(read_environment_value "$old_config_dir/edge.env" EDGE_POLL_SECONDS) && [[ "$value" =~ ^[0-9]+$ ]] && ((value >= 5 && value <= 300)); then
    poll_seconds="$value"
  fi
  copy_tree "$old_state_dir" "$edge_root/data"
  copy_tree "$old_config_dir/certs" "$edge_root/config/certs"
  copy_tree "$old_log_dir" "$edge_root/logs"
  rm -f "$edge_root/logs/access.json"
elif ((new_layout == 1)); then
  if value=$(read_environment_value "$edge_root/config/edge.env" EDGE_POLL_SECONDS) && [[ "$value" =~ ^[0-9]+$ ]] && ((value >= 5 && value <= 300)); then
    poll_seconds="$value"
  fi
fi

configure_default_sysctl

install -m 0755 "$temporary_binary" "$edge_root/bin/cdn-edge-agent"
install -m 0644 "$temporary_unit" "$new_unit"
install -m 0644 "$temporary_updater_unit" "$new_updater_unit"
cat >"$edge_root/config/edge.env" <<EOF
CONTROL_URL=${CONTROL_URL}
ENROLLMENT_TOKEN=${ENROLLMENT_TOKEN}
EDGE_POLL_SECONDS=${poll_seconds}
EDGE_STATE_DIR=/opt/cdn-edge/data
NGINX_CONFIG_PATH=/opt/cdn-edge/config/nginx/cdn-platform.conf
NGINX_STREAM_CONFIG_PATH=/opt/cdn-edge/config/nginx/cdn-platform-stream.conf
NGINX_MAIN_CONFIG_PATH=/opt/cdn-edge/config/nginx/cdn-platform-main.conf
NGINX_EVENTS_CONFIG_PATH=/opt/cdn-edge/config/nginx/cdn-platform-events.conf
EDGE_CERT_DIR=/opt/cdn-edge/config/certs
EDGE_ACCESS_LOG=/opt/cdn-edge/logs/access.json
EDGE_SECURITY_LOG=/opt/cdn-edge/logs/security.json
EDGE_CAPABILITIES=tcp_stream_v1,edge_rate_limit_v1,nginx_capacity_v1
EOF
chmod 0600 "$edge_root/config/edge.env"

if ((legacy_layout == 1)) && path_exists "$nginx_entry"; then
  sed \
    -e 's#/var/cache/cdn-platform#/opt/cdn-edge/cache#g' \
    -e 's#/etc/cdn-platform/certs#/opt/cdn-edge/config/certs#g' \
    -e 's#/var/log/cdn-platform/access.json#/opt/cdn-edge/logs/access.json#g' \
    "$nginx_entry" >"$new_nginx_config"
  chmod 0640 "$new_nginx_config"
elif ! path_exists "$new_nginx_config"; then
  cat >"$new_nginx_config" <<'EOF'
# Generated by cdn-edge-agent. Do not edit.
server {
    listen 80 default_server;
    server_name _;
    location = /__cdn_health { access_log off; add_header Content-Type text/plain; return 200 "ok\n"; }
    location / { return 404; }
}
EOF
  chmod 0640 "$new_nginx_config"
fi

if ! path_exists "$new_nginx_stream_config"; then
  cat >"$new_nginx_stream_config" <<'EOF'
# Generated by cdn-edge-agent. Do not edit.
EOF
  chmod 0640 "$new_nginx_stream_config"
fi

if ! path_exists "$new_nginx_main_config"; then
  cat >"$new_nginx_main_config" <<'EOF'
# Generated by cdn-edge-agent. Do not edit.
worker_processes auto;
worker_rlimit_nofile 65536;
EOF
  chmod 0640 "$new_nginx_main_config"
fi

if ! path_exists "$new_nginx_events_config"; then
  cat >"$new_nginx_events_config" <<'EOF'
# Generated by cdn-edge-agent. Do not edit.
worker_connections 4096;
EOF
  chmod 0640 "$new_nginx_events_config"
fi

cat >"$nginx_stream_entry" <<'EOF'
# Managed by simple_cdn. Do not edit.
stream {
    include /opt/cdn-edge/config/nginx/cdn-platform-stream.conf;
}
EOF
chmod 0644 "$nginx_stream_entry"

configure_nginx_capacity_includes "$nginx_root_config"

mkdir -p "$(dirname "$logrotate_config")"
cat >"$logrotate_config" <<'EOF'
/opt/cdn-edge/logs/access.json /opt/cdn-edge/logs/security.json {
    size 32M
    rotate 16
    missingok
    notifempty
    compress
    compresscmd /usr/bin/lz4
    uncompresscmd /usr/bin/unlz4
    compressoptions -q
    compressext .lz4
    copytruncate
}
EOF
chmod 0644 "$logrotate_config"

if ((legacy_layout == 1)) && [[ -f "$old_log_dir/access.json" ]]; then
  if [[ "$(stat -c %d "$old_log_dir/access.json")" != "$(stat -c %d "$edge_root/logs")" ]]; then
    echo "legacy access log and /opt/cdn-edge are on different filesystems; refusing a lossy live migration" >&2
    false
  fi
  mv "$old_log_dir/access.json" "$edge_root/logs/access.json"
  log_moved=1
fi

rm -f "$nginx_entry"
ln -s "$expected_nginx_target" "$nginx_entry"
rm -f "$nginx_default"
nginx -t
if ((legacy_layout == 1)); then
  if systemctl is-active --quiet nginx.service; then
    # The legacy cache zone keeps its path in the running master. Replacing
    # that path requires a cold start; a reload is accepted by the signaling
    # process but rejected asynchronously by the Nginx master.
    systemctl restart nginx.service
  else
    systemctl start nginx.service
  fi
elif systemctl is-active --quiet nginx.service; then
  systemctl reload nginx.service
else
  systemctl start nginx.service
fi
systemctl is-active --quiet nginx.service

rm -f "$agent_unit"
ln -s "$expected_unit_target" "$agent_unit"
rm -f "$updater_unit"
ln -s "$expected_updater_unit_target" "$updater_unit"
systemctl daemon-reload
systemctl enable cdn-edge-agent.service
systemctl restart cdn-edge-agent.service
agent_active=0
for _ in 1 2 3 4 5; do
  sleep 1
  if systemctl is-active --quiet cdn-edge-agent.service; then
    agent_active=1
    break
  fi
done
if ((agent_active == 0)); then
  echo "cdn-edge-agent did not become active after installation" >&2
  false
fi
identity_ready=0
for _ in $(seq 1 30); do
  if [[ -s "$edge_root/data/edge-client.key" && -s "$edge_root/data/edge-client.crt" && -s "$edge_root/data/edge-ca.crt" ]]; then
    identity_ready=1
    break
  fi
  sleep 1
done
if ((identity_ready == 0)); then
  echo "cdn-edge-agent did not establish its mTLS identity after installation" >&2
  false
fi
if [[ -n "$READINESS_FILE" ]]; then
  upgrade_ready=0
  for _ in $(seq 1 120); do
    if [[ -f "$READINESS_FILE" && "$(tr -d '[:space:]' <"$READINESS_FILE")" == "${BINARY_SHA256,,}" ]]; then
      upgrade_ready=1
      break
    fi
    sleep 1
  done
  if ((upgrade_ready == 0)); then
    echo "upgraded edge agent did not confirm a control-plane heartbeat" >&2
    false
  fi
fi
if grep -Fq 'location = /__cdn_health' "$new_nginx_config"; then
  curl --fail --silent --show-error --max-time 5 http://127.0.0.1/__cdn_health >/dev/null
fi

printf '%s\n' "$LAYOUT_VERSION" >"$marker"
chmod 0644 "$marker"
committed=1
trap - ERR
if ((legacy_layout == 1)); then
  rm -f "$old_binary"
  rm -rf "$old_config_dir" "$old_state_dir" "$old_log_dir" "$old_cache_dir"
fi
rm -rf "$transaction_dir"
trap - EXIT
echo "CDN edge deployment is active under /opt/cdn-edge."
