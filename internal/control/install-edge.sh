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
NGINX_BUNDLE_URL_DEFAULT=""
NGINX_BUNDLE_SHA256_DEFAULT=""
NGINX_SERVICE_URL_DEFAULT=""
NGINX_SERVICE_SHA256_DEFAULT=""
NGINX_BUNDLE_URL="$NGINX_BUNDLE_URL_DEFAULT"
NGINX_BUNDLE_FILE=""
NGINX_BUNDLE_SHA256="$NGINX_BUNDLE_SHA256_DEFAULT"
NGINX_SERVICE_URL="$NGINX_SERVICE_URL_DEFAULT"
NGINX_SERVICE_FILE=""
NGINX_SERVICE_SHA256="$NGINX_SERVICE_SHA256_DEFAULT"
ROOT_PREFIX="${CDN_EDGE_INSTALL_ROOT:-}"
LAYOUT_VERSION=2
NGINX_BUNDLE_MAX_BYTES=$((128 * 1024 * 1024))
NGINX_BUNDLE_MAX_UNPACKED_BYTES=$((512 * 1024 * 1024))
NGINX_BUNDLE_MAX_ENTRIES=4096

while [[ $# -gt 0 ]]; do
  case "$1" in
    --control-url) CONTROL_URL="$2"; shift 2 ;;
    --enrollment-token) ENROLLMENT_TOKEN="$2"; shift 2 ;;
    --binary-url) BINARY_URL="$2"; BINARY_FILE=""; shift 2 ;;
    --binary-file) BINARY_FILE="$2"; BINARY_URL=""; shift 2 ;;
    --binary-sha256) BINARY_SHA256="$2"; shift 2 ;;
    --service-file) SERVICE_FILE="$2"; shift 2 ;;
    --service-sha256) SERVICE_SHA256="$2"; shift 2 ;;
    --updater-service-file) UPDATER_SERVICE_FILE="$2"; shift 2 ;;
    --updater-service-sha256) UPDATER_SERVICE_SHA256="$2"; shift 2 ;;
    --nginx-bundle-url) NGINX_BUNDLE_URL="$2"; NGINX_BUNDLE_FILE=""; shift 2 ;;
    --nginx-bundle-file) NGINX_BUNDLE_FILE="$2"; NGINX_BUNDLE_URL=""; shift 2 ;;
    --nginx-bundle-sha256) NGINX_BUNDLE_SHA256="$2"; shift 2 ;;
    --nginx-service-url) NGINX_SERVICE_URL="$2"; NGINX_SERVICE_FILE=""; shift 2 ;;
    --nginx-service-file) NGINX_SERVICE_FILE="$2"; NGINX_SERVICE_URL=""; shift 2 ;;
    --nginx-service-sha256) NGINX_SERVICE_SHA256="$2"; shift 2 ;;
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

valid_https_url() {
  [[ "$1" == https://* && "$1" != *[[:space:]]* && "$1" != *\\* && "$1" != *\"* ]]
}
valid_sha256() {
  [[ "$1" =~ ^[0-9a-fA-F]{64}$ ]]
}
valid_absolute_file() {
  [[ -z "$1" || "$1" == /* ]]
}
if ! valid_https_url "$CONTROL_URL" ||
   [[ -n "$ENROLLMENT_TOKEN" && "$ENROLLMENT_TOKEN" == *[[:space:]]* ]] ||
   ! valid_absolute_file "$BINARY_FILE" || ! valid_absolute_file "$SERVICE_FILE" ||
   ! valid_absolute_file "$UPDATER_SERVICE_FILE" || ! valid_absolute_file "$NGINX_BUNDLE_FILE" ||
   ! valid_absolute_file "$NGINX_SERVICE_FILE" || ! valid_absolute_file "$READINESS_FILE" ||
   ! valid_sha256 "$BINARY_SHA256" || ! valid_sha256 "$SERVICE_SHA256" ||
   ! valid_sha256 "$UPDATER_SERVICE_SHA256" || ! valid_sha256 "$NGINX_BUNDLE_SHA256" ||
   ! valid_sha256 "$NGINX_SERVICE_SHA256" ||
   [[ -n "$BINARY_URL" && -n "$BINARY_FILE" ]] || [[ -z "$BINARY_URL" && -z "$BINARY_FILE" ]] ||
   [[ -n "$NGINX_BUNDLE_URL" && -n "$NGINX_BUNDLE_FILE" ]] || [[ -z "$NGINX_BUNDLE_URL" && -z "$NGINX_BUNDLE_FILE" ]] ||
   [[ -n "$NGINX_SERVICE_URL" && -n "$NGINX_SERVICE_FILE" ]] || [[ -z "$NGINX_SERVICE_URL" && -z "$NGINX_SERVICE_FILE" ]]; then
  echo "usage: install-edge.sh --control-url HTTPS_URL [--enrollment-token TOKEN] (--binary-url HTTPS_URL | --binary-file PATH) --binary-sha256 SHA256 --service-sha256 SHA256 --updater-service-sha256 SHA256 (--nginx-bundle-url HTTPS_URL | --nginx-bundle-file PATH) --nginx-bundle-sha256 SHA256 (--nginx-service-url HTTPS_URL | --nginx-service-file PATH) --nginx-service-sha256 SHA256" >&2
  exit 2
fi
for url in "$BINARY_URL" "$NGINX_BUNDLE_URL" "$NGINX_SERVICE_URL"; do
  if [[ -n "$url" ]] && ! valid_https_url "$url"; then
    echo "artifact URLs must use absolute HTTPS URLs" >&2
    exit 2
  fi
done
for file in "$BINARY_FILE" "$SERVICE_FILE" "$UPDATER_SERVICE_FILE" "$NGINX_BUNDLE_FILE" "$NGINX_SERVICE_FILE"; do
  if [[ -n "$file" && ! -f "$file" ]]; then
    echo "staged upgrade artifact is missing: $file" >&2
    exit 2
  fi
done
CONTROL_URL="${CONTROL_URL%/}"
NGINX_BUNDLE_SHA256="${NGINX_BUNDLE_SHA256,,}"

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
fetch_artifact() {
  local source_file="$1" source_url="$2" destination="$3"
  if [[ -n "$source_file" ]]; then
    cp "$source_file" "$destination"
  else
    curl --fail --location --silent --show-error --proto '=https' --tlsv1.2 "$source_url" --output "$destination"
  fi
}
verify_artifact() {
  local digest="$1" pathname="$2"
  echo "$digest  $pathname" | sha256sum --check --status
}
normalize_managed_nginx_paths() {
  local pathname="$1" temporary
  if [[ ! -f "$pathname" || -L "$pathname" ]]; then
    echo "managed Nginx configuration is not a regular file: $pathname" >&2
    return 1
  fi
  temporary=$(mktemp "$(dirname "$pathname")/.cdn-edge-normalize.XXXXXX")
  sed \
    -e 's#/var/cache/cdn-platform#/opt/cdn-edge/cache#g' \
    -e 's#/etc/cdn-platform/certs#/opt/cdn-edge/config/certs#g' \
    -e 's#/var/log/cdn-platform/access.json#/opt/cdn-edge/logs/access.json#g' \
    -e 's#/var/log/nginx/cdn-platform-tcp-access.log#/opt/cdn-edge/logs/tcp-access.json#g' \
    -e 's#/var/log/nginx/cdn-platform-tcp-error.log#/opt/cdn-edge/logs/tcp-error.log#g' \
    "$pathname" >"$temporary"
  chmod --reference="$pathname" "$temporary" 2>/dev/null || chmod 0640 "$temporary"
  mv "$temporary" "$pathname"
}
normalize_managed_nginx_tree() {
  local directory="$1" file_list unsafe pathname
  if [[ ! -d "$directory" || -L "$directory" ]]; then
    echo "managed Nginx configuration root is not a regular directory: $directory" >&2
    return 1
  fi
  unsafe=$(find -P "$directory" ! -type d ! -type f -print -quit)
  if [[ -n "$unsafe" ]]; then
    echo "managed Nginx configuration contains an unsafe entry: $unsafe" >&2
    return 1
  fi
  file_list="$transaction_dir/nginx-config-files"
  find -P "$directory" -type f -name '*.conf' -print0 >"$file_list"
  while IFS= read -r -d '' pathname; do
    normalize_managed_nginx_paths "$pathname"
  done <"$file_list"
}

edge_root=$(root_path /opt/cdn-edge)
marker="$edge_root/.layout-version"
agent_unit=$(root_path /etc/systemd/system/cdn-edge-agent.service)
updater_unit=$(root_path /etc/systemd/system/cdn-edge-updater@.service)
nginx_unit=$(root_path /etc/systemd/system/nginx.service)
new_agent_unit="$edge_root/systemd/cdn-edge-agent.service"
new_updater_unit="$edge_root/systemd/cdn-edge-updater@.service"
new_nginx_unit="$edge_root/systemd/nginx.service"
nginx_binary="$edge_root/nginx/sbin/nginx"
nginx_config="$edge_root/config/nginx/cdn-platform.conf"
nginx_stream_config="$edge_root/config/nginx/cdn-platform-stream.conf"
nginx_main_config="$edge_root/config/nginx/cdn-platform-main.conf"
nginx_events_config="$edge_root/config/nginx/cdn-platform-events.conf"
logrotate_config=$(root_path /etc/logrotate.d/cdn-edge-platform)
sysctl_config=$(root_path /usr/local/lib/sysctl.d/40-simple-cdn-edge.conf)
sysctl_baseline="$edge_root/data/sysctl-baseline.conf"
old_binary=$(root_path /usr/local/bin/cdn-edge-agent)
old_config_dir=$(root_path /etc/cdn-platform)
old_state_dir=$(root_path /var/lib/cdn-platform)
old_log_dir=$(root_path /var/log/cdn-platform)
old_cache_dir=$(root_path /var/cache/cdn-platform)

layout_before="none"
if [[ -f "$marker" ]]; then
  layout_before=$(tr -d '[:space:]' <"$marker")
  if [[ "$layout_before" != "1" && "$layout_before" != "$LAYOUT_VERSION" ]]; then
    echo "unsupported /opt/cdn-edge layout version" >&2
    exit 1
  fi
elif [[ -d "$edge_root" && -n "$(find "$edge_root" -mindepth 1 -maxdepth 1 -print -quit)" ]]; then
  echo "$edge_root exists without a recognized layout marker; refusing to merge it" >&2
  exit 1
fi
legacy_layout=0
for pathname in "$old_binary" "$old_config_dir" "$old_state_dir" "$old_log_dir" "$old_cache_dir"; do
  if path_exists "$pathname"; then legacy_layout=1; fi
done
if [[ "$layout_before" != "none" && "$legacy_layout" == "1" ]]; then
  if [[ "$layout_before" == "$LAYOUT_VERSION" ]]; then
    echo "warning: stale legacy CDN paths found; /opt/cdn-edge layout ${LAYOUT_VERSION} remains authoritative" >&2
    legacy_layout=0
  else
    echo "both /opt/cdn-edge and legacy CDN paths exist; refusing to guess which layout is authoritative" >&2
    exit 1
  fi
fi
if [[ "$layout_before" != "none" ]]; then
  for required in "$nginx_config" "$edge_root/data/edge-client.key" "$edge_root/data/edge-client.crt" "$edge_root/data/edge-ca.crt"; do
    if [[ ! -s "$required" ]]; then
      echo "incomplete /opt/cdn-edge layout: missing ${required#"$edge_root/"}" >&2
      exit 1
    fi
  done
fi
existing_identity=0
if [[ "$layout_before" != "none" ]] ||
   [[ -s "$old_state_dir/edge-client.key" && -s "$old_state_dir/edge-client.crt" && -s "$old_state_dir/edge-ca.crt" ]]; then
  existing_identity=1
fi
if [[ -z "$ENROLLMENT_TOKEN" && "$existing_identity" == "0" ]]; then
  echo "an enrollment token is required because this host has no complete edge mTLS identity" >&2
  exit 1
fi

if [[ -z "$ROOT_PREFIX" ]]; then
  export DEBIAN_FRONTEND=noninteractive
  apt-get update
  apt-get install -y --no-install-recommends \
    ca-certificates curl iproute2 nftables logrotate lz4 procps kmod \
    libpcre2-8-0 zlib1g libcrypt1
fi

install -d -m 0755 "$(root_path /opt)"
transaction_dir=$(mktemp -d "$(root_path /opt/.cdn-edge-install.XXXXXX)")
trap 'rm -rf "$transaction_dir"' EXIT
backup_root="$transaction_dir/backup"
install -d -m 0700 "$backup_root"
temporary_binary="$transaction_dir/cdn-edge-agent"
temporary_agent_unit="$transaction_dir/cdn-edge-agent.service"
temporary_updater_unit="$transaction_dir/cdn-edge-updater@.service"
temporary_nginx_bundle="$transaction_dir/cdn-nginx.tar.gz"
temporary_nginx_unit="$transaction_dir/nginx.service"

fetch_artifact "$BINARY_FILE" "$BINARY_URL" "$temporary_binary"
verify_artifact "$BINARY_SHA256" "$temporary_binary"
if [[ -n "$SERVICE_FILE" ]]; then
  cp "$SERVICE_FILE" "$temporary_agent_unit"
else
  curl --fail --location --silent --show-error --proto '=https' --tlsv1.2 "${CONTROL_URL}/install-edge.service" --output "$temporary_agent_unit"
fi
verify_artifact "$SERVICE_SHA256" "$temporary_agent_unit"
if [[ -n "$UPDATER_SERVICE_FILE" ]]; then
  cp "$UPDATER_SERVICE_FILE" "$temporary_updater_unit"
else
  curl --fail --location --silent --show-error --proto '=https' --tlsv1.2 "${CONTROL_URL}/install-edge-updater.service" --output "$temporary_updater_unit"
fi
verify_artifact "$UPDATER_SERVICE_SHA256" "$temporary_updater_unit"
fetch_artifact "$NGINX_BUNDLE_FILE" "$NGINX_BUNDLE_URL" "$temporary_nginx_bundle"
verify_artifact "$NGINX_BUNDLE_SHA256" "$temporary_nginx_bundle"
nginx_bundle_size=$(stat -c '%s' "$temporary_nginx_bundle")
if [[ ! "$nginx_bundle_size" =~ ^[0-9]+$ ]] || ((nginx_bundle_size <= 0 || nginx_bundle_size > NGINX_BUNDLE_MAX_BYTES)); then
  echo "Nginx bundle must be non-empty and no larger than ${NGINX_BUNDLE_MAX_BYTES} bytes" >&2
  exit 1
fi
fetch_artifact "$NGINX_SERVICE_FILE" "$NGINX_SERVICE_URL" "$temporary_nginx_unit"
verify_artifact "$NGINX_SERVICE_SHA256" "$temporary_nginx_unit"

if ! grep -Fqx 'ExecStart=/opt/cdn-edge/bin/cdn-edge-agent' "$temporary_agent_unit" ||
   ! grep -Fqx 'EnvironmentFile=/opt/cdn-edge/config/edge.env' "$temporary_agent_unit"; then
  echo "downloaded edge service does not match the /opt/cdn-edge layout" >&2
  exit 1
fi
if ! grep -Fqx 'ExecStart=/opt/cdn-edge/bin/cdn-edge-agent upgrade-helper %i' "$temporary_updater_unit" ||
   ! grep -Fqx 'EnvironmentFile=/opt/cdn-edge/config/edge.env' "$temporary_updater_unit"; then
  echo "downloaded edge updater service does not match the /opt/cdn-edge layout" >&2
  exit 1
fi
for expected in \
  'PIDFile=/opt/cdn-edge/nginx/run/nginx.pid' \
  'ExecStartPre=-/usr/bin/rm -f /opt/cdn-edge/nginx/run/status.sock' \
  'ExecStartPre=/opt/cdn-edge/nginx/sbin/nginx -t -q' \
  'ExecStart=/opt/cdn-edge/nginx/sbin/nginx' \
  'ExecReload=/opt/cdn-edge/nginx/sbin/nginx -s reload'; do
  if ! grep -Fqx "$expected" "$temporary_nginx_unit"; then
    echo "downloaded Nginx service does not match the /opt/cdn-edge layout" >&2
    exit 1
  fi
done

entries_file="$transaction_dir/nginx.entries"
verbose_entries_file="$transaction_dir/nginx.entries.verbose"
tar -tzf "$temporary_nginx_bundle" >"$entries_file"
tar -tvzf "$temporary_nginx_bundle" >"$verbose_entries_file"
nginx_entry_count=$(wc -l <"$entries_file")
if ((nginx_entry_count == 0 || nginx_entry_count > NGINX_BUNDLE_MAX_ENTRIES)); then
  echo "Nginx bundle contains an invalid number of entries" >&2
  exit 1
fi
if [[ -s "$entries_file" ]] && awk 'substr($0,1,1) != "-" && substr($0,1,1) != "d" { exit 1 }' "$verbose_entries_file"; then
  :
else
  echo "Nginx bundle contains unsupported archive entry types" >&2
  exit 1
fi
if LC_ALL=C sort "$entries_file" | awk 'seen[$0]++ { duplicate=1 } END { exit duplicate ? 0 : 1 }'; then
  echo "Nginx bundle contains duplicate paths" >&2
  exit 1
fi
if ! awk -v limit="$NGINX_BUNDLE_MAX_UNPACKED_BYTES" '
  $3 !~ /^[0-9]+$/ { invalid=1; next }
  { total += $3; if (total > limit) oversized=1 }
  END { exit (invalid || oversized) ? 1 : 0 }
' "$verbose_entries_file"; then
  echo "Nginx bundle has invalid sizes or expands beyond ${NGINX_BUNDLE_MAX_UNPACKED_BYTES} bytes" >&2
  exit 1
fi
while IFS= read -r entry; do
  clean="${entry%/}"
  if [[ -z "$clean" || "$clean" == /* || "$clean" == ".." || "$clean" == ../* ||
        "$clean" == */../* || "$clean" == */.. || "$clean" == *[[:space:]]* ||
        ( "$clean" != "nginx" && "$clean" != nginx/* ) ]]; then
    echo "Nginx bundle contains unsafe path: $entry" >&2
    exit 1
  fi
done <"$entries_file"
for required in \
  nginx/sbin/nginx nginx/conf/nginx.conf nginx/conf/mime.types \
  nginx/licenses/nginx.txt nginx/licenses/ngx_devel_kit.txt \
  nginx/licenses/openresty-luajit.txt nginx/licenses/lua-nginx-module.txt \
  nginx/licenses/lua-resty-core.txt nginx/licenses/lua-resty-lrucache.txt \
  nginx/VERSION nginx/BUILD.json; do
  if ! grep -Fxq "$required" "$entries_file"; then
    echo "Nginx bundle is missing $required" >&2
    exit 1
  fi
done
extract_root="$transaction_dir/extracted"
install -d -m 0700 "$extract_root"
tar -xzf "$temporary_nginx_bundle" --no-same-owner --no-same-permissions -C "$extract_root"
candidate_nginx="$extract_root/nginx"
if find "$candidate_nginx" \( -type l -o -type b -o -type c -o -type p -o -type s \) -print -quit | grep -q .; then
  echo "Nginx bundle extraction contains an unsupported filesystem object" >&2
  exit 1
fi
for required in sbin/nginx conf/nginx.conf conf/mime.types VERSION BUILD.json; do
  if [[ ! -f "$candidate_nginx/$required" ]]; then
    echo "Nginx bundle extraction is incomplete: $required" >&2
    exit 1
  fi
done
find "$candidate_nginx" -type d -exec chmod 0755 {} +
find "$candidate_nginx" -type f -exec chmod 0644 {} +
nginx_version=$(tr -d '[:space:]' <"$candidate_nginx/VERSION")
if [[ ! "$nginx_version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  echo "Nginx bundle VERSION is invalid" >&2
  exit 1
fi
chmod 0755 "$candidate_nginx/sbin/nginx"
nginx_version_output=$(LD_LIBRARY_PATH="$candidate_nginx/lib" "$candidate_nginx/sbin/nginx" -V 2>&1)
if ! grep -Fq "nginx/$nginx_version" <<<"$nginx_version_output"; then
  echo "Nginx binary version does not match bundle VERSION" >&2
  exit 1
fi
nginx_build_metadata=$(tr -d '[:space:]' <"$candidate_nginx/BUILD.json")
if ! grep -Fq "\"nginx_version\":\"$nginx_version\"" <<<"$nginx_build_metadata" ||
   ! grep -Fq '"architecture":"amd64"' <<<"$nginx_build_metadata"; then
  echo "Nginx bundle BUILD.json does not match VERSION or amd64 architecture" >&2
  exit 1
fi

backup_path() {
  local source="$1" name="$2"
  if path_exists "$source"; then
    cp -a "$source" "$backup_root/$name"
    : >"$backup_root/$name.present"
  fi
}
restore_path() {
  local destination="$1" name="$2"
  rm -rf "$destination"
  if [[ -f "$backup_root/$name.present" ]]; then
    install -d -m 0755 "$(dirname "$destination")"
    cp -a "$backup_root/$name" "$destination"
  fi
}

backup_path "$agent_unit" agent-unit
backup_path "$updater_unit" updater-unit
backup_path "$nginx_unit" nginx-unit
backup_path "$(root_path /etc/nginx)" etc-nginx
backup_path "$logrotate_config" logrotate
backup_path "$sysctl_config" sysctl-config
backup_path "$sysctl_baseline" sysctl-baseline
backup_path "$edge_root/bin/cdn-edge-agent" edge-binary
backup_path "$edge_root/config/edge.env" edge-env
backup_path "$edge_root/config/nginx" edge-nginx-config
backup_path "$new_agent_unit" edge-agent-unit
backup_path "$new_updater_unit" edge-updater-unit
backup_path "$new_nginx_unit" edge-nginx-unit

old_agent_active=0
old_agent_enabled=0
old_nginx_active=0
old_nginx_enabled=0
if systemctl is-active --quiet cdn-edge-agent.service 2>/dev/null; then old_agent_active=1; fi
if systemctl is-enabled --quiet cdn-edge-agent.service 2>/dev/null; then old_agent_enabled=1; fi
if systemctl is-active --quiet nginx.service 2>/dev/null; then old_nginx_active=1; fi
if systemctl is-enabled --quiet nginx.service 2>/dev/null; then old_nginx_enabled=1; fi

nginx_packages_file="$transaction_dir/nginx-packages.tsv"
: >"$nginx_packages_file"
declare -a nginx_packages=()
if [[ -z "$ROOT_PREFIX" ]]; then
  (dpkg-query -W -f='${binary:Package}\t${Version}\n' 2>/dev/null || true) |
    awk '$1 ~ /^nginx($|-)/ || $1 ~ /^libnginx-mod-/ { print $1 "\t" $2 }' >"$nginx_packages_file"
  while IFS=$'\t' read -r package package_version; do
    if [[ -n "$package" && -n "$package_version" ]]; then nginx_packages+=("$package"); fi
  done <"$nginx_packages_file"
fi
if [[ "$layout_before" == "none" && "$legacy_layout" == "0" && -z "$ROOT_PREFIX" &&
      ${#nginx_packages[@]} -eq 0 && -d "$(root_path /etc/nginx)" ]]; then
  echo "/etc/nginx exists but is not owned by Debian Nginx packages; refusing to remove an unmanaged installation" >&2
  exit 1
fi

sysctl_runtime_backup="$transaction_dir/sysctl-runtime.backup"
: >"$sysctl_runtime_backup"
configure_default_sysctl() {
  local meminfo mem_total_kib buffer_max key value profile_next baseline_next
  local -a keys=(
    net.core.default_qdisc net.ipv4.tcp_congestion_control net.ipv4.tcp_mtu_probing
    net.core.rmem_max net.core.wmem_max net.ipv4.tcp_rmem net.ipv4.tcp_wmem
  )
  declare -A before=()
  for key in "${keys[@]}"; do
    if value=$(sysctl -n "$key" 2>/dev/null); then
      value=$(awk '{$1=$1; print}' <<<"$value")
      before["$key"]="$value"
      printf '%s = %s\n' "$key" "$value" >>"$sysctl_runtime_backup"
    fi
  done
  buffer_max=16777216
  meminfo=$(root_path /proc/meminfo)
  mem_total_kib=""
  if [[ -r "$meminfo" ]]; then mem_total_kib=$(awk '$1 == "MemTotal:" { print $2; exit }' "$meminfo"); fi
  if [[ "$mem_total_kib" =~ ^[0-9]+$ ]] && ((mem_total_kib > 4194304)); then buffer_max=33554432; fi

  profile_next="$transaction_dir/40-simple-cdn-edge.conf"
  printf '%s\n' '# Managed by simple_cdn. Later files in /etc/sysctl.d may override these defaults.' >"$profile_next"
  modprobe sch_fq >/dev/null 2>&1 || true
  if [[ -n "${before[net.core.default_qdisc]+set}" ]] && sysctl -q -w net.core.default_qdisc=fq >/dev/null 2>&1; then
    printf '%s\n' '-net.core.default_qdisc = fq' >>"$profile_next"
  else
    echo "warning: fq is unavailable; leaving net.core.default_qdisc unchanged" >&2
  fi
  modprobe tcp_bbr >/dev/null 2>&1 || true
  if [[ -n "${before[net.ipv4.tcp_congestion_control]+set}" ]] && sysctl -q -w net.ipv4.tcp_congestion_control=bbr >/dev/null 2>&1; then
    printf '%s\n' '-net.ipv4.tcp_congestion_control = bbr' >>"$profile_next"
  else
    echo "warning: BBR is unavailable; leaving net.ipv4.tcp_congestion_control unchanged" >&2
  fi
  if [[ -n "${before[net.ipv4.tcp_mtu_probing]+set}" ]] && sysctl -q -w net.ipv4.tcp_mtu_probing=1 >/dev/null 2>&1; then
    printf '%s\n' '-net.ipv4.tcp_mtu_probing = 1' >>"$profile_next"
  fi
  buffer_supported=1
  for assignment in \
    "net.core.rmem_max=$buffer_max" "net.core.wmem_max=$buffer_max" \
    "net.ipv4.tcp_rmem=4096 131072 $buffer_max" "net.ipv4.tcp_wmem=4096 16384 $buffer_max"; do
    key="${assignment%%=*}"
    if [[ -z "${before[$key]+set}" ]] || ! sysctl -q -w "$assignment" >/dev/null 2>&1; then buffer_supported=0; fi
  done
  if ((buffer_supported == 1)); then
    printf -- '-net.core.rmem_max = %s\n' "$buffer_max" >>"$profile_next"
    printf -- '-net.core.wmem_max = %s\n' "$buffer_max" >>"$profile_next"
    printf -- '-net.ipv4.tcp_rmem = 4096 131072 %s\n' "$buffer_max" >>"$profile_next"
    printf -- '-net.ipv4.tcp_wmem = 4096 16384 %s\n' "$buffer_max" >>"$profile_next"
  else
    echo "warning: TCP buffer tuning is not fully supported; leaving the buffer group unchanged" >&2
    for key in net.core.rmem_max net.core.wmem_max net.ipv4.tcp_rmem net.ipv4.tcp_wmem; do
      if [[ -n "${before[$key]+set}" ]]; then sysctl -q -w "$key=${before[$key]}" >/dev/null 2>&1 || true; fi
    done
  fi
  if [[ -s "$sysctl_runtime_backup" ]]; then sysctl -q -p "$sysctl_runtime_backup" >/dev/null 2>&1 || true; fi

  baseline_next="$transaction_dir/sysctl-baseline.next"
  if [[ -f "$sysctl_baseline" ]]; then
    cp "$sysctl_baseline" "$baseline_next"
  else
    printf '%s\n' '# Values that preceded simple_cdn sysctl management. Used during uninstall.' >"$baseline_next"
  fi
  for key in "${keys[@]}"; do
    if [[ -n "${before[$key]+set}" ]] && ! awk -F= -v wanted="$key" '{ key=$1; sub(/^-/, "", key); gsub(/[[:space:]]/, "", key); if (key == wanted) found=1 } END { exit found ? 0 : 1 }' "$baseline_next"; then
      printf '%s = %s\n' "$key" "${before[$key]}" >>"$baseline_next"
    fi
  done
  install -d -m 0755 "$(dirname "$sysctl_config")"
  install -m 0644 "$profile_next" "$sysctl_config"
  install -d -m 0750 "$(dirname "$sysctl_baseline")"
  install -m 0600 "$baseline_next" "$sysctl_baseline"
  sysctl --system >/dev/null || echo "warning: one or more system sysctl files could not be applied" >&2
}

nginx_packages_purged=0
install_committed=0
rollback() {
  local code=$?
  trap - ERR
  set +e
  if ((install_committed == 0)); then
    systemctl stop cdn-edge-agent.service >/dev/null 2>&1
    systemctl stop nginx.service >/dev/null 2>&1
    if [[ -d "$edge_root/nginx" ]]; then rm -rf "$edge_root/nginx"; fi
    if [[ -d "$transaction_dir/nginx.previous" ]]; then mv "$transaction_dir/nginx.previous" "$edge_root/nginx"; fi

    if [[ "$layout_before" == "none" ]]; then
      rm -rf "$edge_root"
    else
      restore_path "$edge_root/bin/cdn-edge-agent" edge-binary
      restore_path "$edge_root/config/edge.env" edge-env
      restore_path "$edge_root/config/nginx" edge-nginx-config
      restore_path "$new_agent_unit" edge-agent-unit
      restore_path "$new_updater_unit" edge-updater-unit
      restore_path "$new_nginx_unit" edge-nginx-unit
    fi
    restore_path "$agent_unit" agent-unit
    restore_path "$updater_unit" updater-unit
    restore_path "$nginx_unit" nginx-unit
    restore_path "$logrotate_config" logrotate
    restore_path "$sysctl_config" sysctl-config
    if [[ "$layout_before" != "none" ]]; then restore_path "$sysctl_baseline" sysctl-baseline; fi

    if ((nginx_packages_purged == 1)) && [[ -z "$ROOT_PREFIX" ]]; then
      declare -a package_specs=() package_names=()
      while IFS=$'\t' read -r package package_version; do
        if [[ -n "$package" && -n "$package_version" ]]; then
          package_specs+=("$package=$package_version")
          package_names+=("$package")
        fi
      done <"$nginx_packages_file"
      if ((${#package_specs[@]} > 0)); then
        apt-get install -y --no-install-recommends -- "${package_specs[@]}" >/dev/null 2>&1 ||
          apt-get install -y --no-install-recommends -- "${package_names[@]}" >/dev/null 2>&1 || true
      fi
    fi
    restore_path "$(root_path /etc/nginx)" etc-nginx
    if [[ -s "$sysctl_runtime_backup" ]]; then sysctl -q -p "$sysctl_runtime_backup" >/dev/null 2>&1 || true; fi
    sysctl --system >/dev/null 2>&1 || true
    systemctl daemon-reload >/dev/null 2>&1 || true
    if ((old_nginx_enabled == 1)); then systemctl enable nginx.service >/dev/null 2>&1; else systemctl disable nginx.service >/dev/null 2>&1; fi
    if ((old_nginx_active == 1)); then systemctl start nginx.service >/dev/null 2>&1; fi
    if ((old_agent_enabled == 1)); then systemctl enable cdn-edge-agent.service >/dev/null 2>&1; else systemctl disable cdn-edge-agent.service >/dev/null 2>&1; fi
    if ((old_agent_active == 1)); then systemctl start cdn-edge-agent.service >/dev/null 2>&1; fi
  fi
  exit "$code"
}
trap rollback ERR

if ((old_agent_active == 1)); then systemctl stop cdn-edge-agent.service; fi
install -d -m 0755 "$edge_root" "$edge_root/bin" "$edge_root/systemd"
install -d -m 0750 "$edge_root/config" "$edge_root/config/nginx" "$edge_root/data" "$edge_root/logs"
install -d -m 0700 "$edge_root/config/certs"
install -d -m 0750 "$edge_root/cache"
chown www-data:www-data "$edge_root/cache"

poll_seconds=30
if ((legacy_layout == 1)); then
  if value=$(read_environment_value "$old_config_dir/edge.env" EDGE_POLL_SECONDS) && [[ "$value" =~ ^[0-9]+$ ]] && ((value >= 5 && value <= 300)); then poll_seconds="$value"; fi
  copy_tree "$old_state_dir" "$edge_root/data"
  copy_tree "$old_config_dir/certs" "$edge_root/config/certs"
  copy_tree "$old_log_dir" "$edge_root/logs"
elif [[ "$layout_before" != "none" ]]; then
  if value=$(read_environment_value "$edge_root/config/edge.env" EDGE_POLL_SECONDS) && [[ "$value" =~ ^[0-9]+$ ]] && ((value >= 5 && value <= 300)); then poll_seconds="$value"; fi
fi

if ((legacy_layout == 1)) && [[ -f "$(root_path /etc/nginx/conf.d/cdn-platform.conf)" ]]; then
  sed \
    -e 's#/var/cache/cdn-platform#/opt/cdn-edge/cache#g' \
    -e 's#/etc/cdn-platform/certs#/opt/cdn-edge/config/certs#g' \
    -e 's#/var/log/cdn-platform/access.json#/opt/cdn-edge/logs/access.json#g' \
    "$(root_path /etc/nginx/conf.d/cdn-platform.conf)" >"$nginx_config"
elif [[ ! -f "$nginx_config" ]]; then
  cat >"$nginx_config" <<'EOF'
# Generated by cdn-edge-agent. Do not edit.
server {
    listen 80 default_server;
    server_name _;
    location = /__cdn_health { access_log off; add_header Content-Type text/plain; return 200 "ok\n"; }
    location / { return 404; }
}
EOF
fi
if [[ ! -f "$nginx_stream_config" ]]; then printf '%s\n' '# Generated by cdn-edge-agent. Do not edit.' >"$nginx_stream_config"; fi
if [[ ! -f "$nginx_main_config" ]]; then
  printf '%s\n' '# Generated by cdn-edge-agent. Do not edit.' 'pcre_jit on;' 'worker_processes auto;' 'worker_rlimit_nofile 65536;' 'worker_shutdown_timeout 1h;' >"$nginx_main_config"
fi
if [[ ! -f "$nginx_events_config" ]]; then
  printf '%s\n' '# Generated by cdn-edge-agent. Do not edit.' 'worker_connections 4096;' >"$nginx_events_config"
fi
normalize_managed_nginx_tree "$edge_root/config/nginx"
chmod 0640 "$nginx_config" "$nginx_stream_config" "$nginx_main_config" "$nginx_events_config"
quic_host_key="$edge_root/config/nginx/quic-host.key"
if path_exists "$quic_host_key" && [[ ! -f "$quic_host_key" || -L "$quic_host_key" ]]; then
  echo "QUIC host key path is not a safe regular file: $quic_host_key" >&2
  exit 1
fi
if [[ ! -f "$quic_host_key" ]]; then
  (umask 077; head -c 32 /dev/urandom >"$quic_host_key")
fi
if [[ $(stat -c '%s' "$quic_host_key") -lt 32 ]]; then
  echo "QUIC host key must contain at least 32 bytes" >&2
  exit 1
fi
chmod 0600 "$quic_host_key"

configure_default_sysctl
install -m 0755 "$temporary_binary" "$edge_root/bin/cdn-edge-agent"
install -m 0644 "$temporary_agent_unit" "$new_agent_unit"
install -m 0644 "$temporary_updater_unit" "$new_updater_unit"

if ((old_nginx_active == 1)); then systemctl stop nginx.service; fi
if ((${#nginx_packages[@]} > 0)); then
  nginx_packages_purged=1
  apt-get purge -y -- "${nginx_packages[@]}"
fi
if [[ "$layout_before" != "$LAYOUT_VERSION" || ${#nginx_packages[@]} -gt 0 ]]; then rm -rf "$(root_path /etc/nginx)"; fi
if [[ -d "$edge_root/nginx" ]]; then mv "$edge_root/nginx" "$transaction_dir/nginx.previous"; fi
mv "$candidate_nginx" "$edge_root/nginx"
printf '%s\n' "$NGINX_BUNDLE_SHA256" >"$edge_root/nginx/.bundle-sha256"
chmod 0644 "$edge_root/nginx/.bundle-sha256" "$edge_root/nginx/VERSION" "$edge_root/nginx/BUILD.json"
install -d -m 0755 "$edge_root/nginx/run"
for name in body fastcgi proxy scgi uwsgi; do
  directory="$edge_root/nginx/tmp/$name"
  if path_exists "$directory" && [[ ! -d "$directory" || -L "$directory" ]]; then
    echo "Nginx temp path is not a safe directory: $directory" >&2
    false
  fi
  install -d -m 0700 "$directory"
  chown www-data:www-data "$directory"
done
install -m 0644 "$temporary_nginx_unit" "$new_nginx_unit"
rm -f "$nginx_unit"
ln -s "$new_nginx_unit" "$nginx_unit"
systemctl daemon-reload
"$nginx_binary" -t
systemctl enable nginx.service
systemctl start nginx.service
systemctl is-active --quiet nginx.service

edge_capabilities="tcp_stream_v1,edge_rate_limit_v1,nginx_capacity_v1,nginx_bundle_v1"
nginx_version_output=$("$nginx_binary" -V 2>&1)
if grep -Fq -- '--with-http_v3_module' <<<"$nginx_version_output"; then edge_capabilities+=",http3_v1"; fi
cat >"$edge_root/config/edge.env" <<EOF
CONTROL_URL=${CONTROL_URL}
ENROLLMENT_TOKEN=${ENROLLMENT_TOKEN}
EDGE_POLL_SECONDS=${poll_seconds}
EDGE_STATE_DIR=/opt/cdn-edge/data
NGINX_CONFIG_PATH=/opt/cdn-edge/config/nginx/cdn-platform.conf
NGINX_STREAM_CONFIG_PATH=/opt/cdn-edge/config/nginx/cdn-platform-stream.conf
NGINX_MAIN_CONFIG_PATH=/opt/cdn-edge/config/nginx/cdn-platform-main.conf
NGINX_EVENTS_CONFIG_PATH=/opt/cdn-edge/config/nginx/cdn-platform-events.conf
NGINX_BINARY_PATH=/opt/cdn-edge/nginx/sbin/nginx
NGINX_PID_PATH=/opt/cdn-edge/nginx/run/nginx.pid
NGINX_STATUS_SOCKET_PATH=/opt/cdn-edge/nginx/run/status.sock
NGINX_VERSION_PATH=/opt/cdn-edge/nginx/VERSION
NGINX_SHA256_PATH=/opt/cdn-edge/nginx/.bundle-sha256
EDGE_CERT_DIR=/opt/cdn-edge/config/certs
EDGE_ACCESS_LOG=/opt/cdn-edge/logs/access.json
EDGE_SECURITY_LOG=/opt/cdn-edge/logs/security.json
EDGE_CAPABILITIES=${edge_capabilities}
EOF
chmod 0600 "$edge_root/config/edge.env"

install -d -m 0755 "$(dirname "$logrotate_config")"
cat >"$logrotate_config" <<'EOF'
/opt/cdn-edge/logs/access.json /opt/cdn-edge/logs/security.json /opt/cdn-edge/logs/nginx-error.log /opt/cdn-edge/logs/tcp-access.json /opt/cdn-edge/logs/tcp-error.log {
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

rm -f "$agent_unit" "$updater_unit"
ln -s "$new_agent_unit" "$agent_unit"
ln -s "$new_updater_unit" "$updater_unit"
systemctl daemon-reload
systemctl enable cdn-edge-agent.service
systemctl restart cdn-edge-agent.service
agent_active=0
for _ in 1 2 3 4 5; do
  sleep 1
  if systemctl is-active --quiet cdn-edge-agent.service; then agent_active=1; break; fi
done
if ((agent_active == 0)); then
  echo "cdn-edge-agent did not become active after installation" >&2
  false
fi
identity_ready=0
for _ in $(seq 1 30); do
  if [[ -s "$edge_root/data/edge-client.key" && -s "$edge_root/data/edge-client.crt" && -s "$edge_root/data/edge-ca.crt" ]]; then identity_ready=1; break; fi
  sleep 1
done
if ((identity_ready == 0)); then
  echo "cdn-edge-agent did not establish its mTLS identity after installation" >&2
  false
fi
sed -i 's/^ENROLLMENT_TOKEN=.*/ENROLLMENT_TOKEN=/' "$edge_root/config/edge.env"
if [[ -n "$READINESS_FILE" ]]; then
  upgrade_ready=0
  for _ in $(seq 1 120); do
    if [[ -f "$READINESS_FILE" && "$(tr -d '[:space:]' <"$READINESS_FILE")" == "${BINARY_SHA256,,}" ]]; then upgrade_ready=1; break; fi
    sleep 1
  done
  if ((upgrade_ready == 0)); then
    echo "upgraded edge agent did not confirm a control-plane heartbeat" >&2
    false
  fi
fi
if grep -Fq 'location = /__cdn_health' "$nginx_config"; then
  curl --fail --silent --show-error --max-time 5 http://127.0.0.1/__cdn_health >/dev/null
fi

printf '%s\n' "$LAYOUT_VERSION" >"$marker"
install_committed=1
trap - ERR

# The new layout becomes authoritative only after both services and the mTLS
# identity are ready. Cleanup is then best-effort so it cannot trigger a partial
# rollback that destroys the legacy source after the new installation is live.
legacy_cleanup_failed=0
for pathname in "$old_config_dir" "$old_state_dir" "$old_log_dir" "$old_cache_dir"; do
  if ! rm -rf -- "$pathname"; then
    echo "warning: could not remove stale legacy path $pathname" >&2
    legacy_cleanup_failed=1
  fi
done
if ! rm -f -- "$old_binary"; then
  echo "warning: could not remove stale legacy path $old_binary" >&2
  legacy_cleanup_failed=1
fi
if ((legacy_cleanup_failed == 1)); then
  echo "warning: rerun this installer after correcting permissions to finish legacy cleanup" >&2
fi
echo "simple_cdn edge agent and managed Nginx ${nginx_version} are installed under /opt/cdn-edge."
