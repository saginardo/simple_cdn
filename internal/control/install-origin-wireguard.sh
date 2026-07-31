#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

CONTROL_URL=""
TOKEN=""
TUNNEL_ID=""
TUNNEL_NAME=""
ORIGIN_ADDRESS=""
UNINSTALL=0
STATE_ROOT="/var/lib/simple-cdn-origin-wireguard"
CONFIG_ROOT="/etc/wireguard"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --control-url) CONTROL_URL="$2"; shift 2 ;;
    --token) TOKEN="$2"; shift 2 ;;
    --tunnel-id) TUNNEL_ID="$2"; shift 2 ;;
    --tunnel-name) TUNNEL_NAME="$2"; shift 2 ;;
    --origin-address) ORIGIN_ADDRESS="$2"; shift 2 ;;
    --uninstall) UNINSTALL=1; shift ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

if [[ $EUID -ne 0 ]]; then
  echo "WireGuard origin setup must run as root" >&2
  exit 2
fi
if [[ ! "$TUNNEL_ID" =~ ^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$ ]]; then
  echo "--tunnel-id must be a UUID" >&2
  exit 2
fi

compact_id=${TUNNEL_ID//-/}
compact_id=${compact_id,,}
interface="scwg${compact_id:0:10}"
table_name="scwg_${compact_id:0:10}"
config_file="$CONFIG_ROOT/$interface.conf"
nft_file="$CONFIG_ROOT/$interface.nft"
key_file="$STATE_ROOT/$TUNNEL_ID.key"
state_file="$STATE_ROOT/$TUNNEL_ID.json"
service_file="/etc/systemd/system/simple-cdn-origin-iperf-$interface.service"

uninstall_tunnel() {
  systemctl disable --now "simple-cdn-origin-iperf-$interface.service" >/dev/null 2>&1 || true
  wg-quick down "$interface" >/dev/null 2>&1 || true
  systemctl disable "wg-quick@$interface.service" >/dev/null 2>&1 || true
  nft delete table inet "$table_name" >/dev/null 2>&1 || true
  rm -f "$service_file" "$config_file" "$nft_file" "$key_file" "$state_file"
  systemctl daemon-reload
  echo "WireGuard origin tunnel $TUNNEL_ID removed"
}

if [[ $UNINSTALL == 1 ]]; then
  if [[ -z "$TUNNEL_NAME" || -z "$ORIGIN_ADDRESS" || "$ORIGIN_ADDRESS" == *[[:space:]]* ]]; then
    echo "usage: install-origin-wireguard.sh --tunnel-id UUID --tunnel-name NAME --origin-address ADDRESS --uninstall" >&2
    exit 2
  fi
  expected="UNINSTALL $TUNNEL_ID"
  printf '\nDANGER: this command permanently removes the managed origin tunnel.\n' >&2
  printf 'Target tunnel: %q\n' "$TUNNEL_NAME" >&2
  printf 'Target tunnel ID: %s\n' "$TUNNEL_ID" >&2
  printf 'Target origin address: %s\n' "$ORIGIN_ADDRESS" >&2
  printf 'Target interface: %s\n' "$interface" >&2
  printf 'Type exactly "%s" to continue:\n> ' "$expected" >&2
  if ! IFS= read -r confirmation </dev/tty; then
    echo "interactive confirmation requires a controlling terminal; nothing was removed" >&2
    exit 2
  fi
  if [[ "$confirmation" != "$expected" ]]; then
    echo "confirmation did not match; nothing was removed" >&2
    exit 1
  fi
  uninstall_tunnel
  exit 0
fi

if [[ ! "$CONTROL_URL" =~ ^https://[^[:space:]]+$ ]] || [[ -z "$TOKEN" || "$TOKEN" == *[[:space:]]* ]]; then
  echo "usage: install-origin-wireguard.sh --control-url HTTPS_URL --token TOKEN --tunnel-id UUID" >&2
  exit 2
fi
CONTROL_URL=${CONTROL_URL%/}

valid_ipv4() {
  local value="$1" first second third fourth extra octet
  IFS=. read -r first second third fourth extra <<<"$value"
  [[ -z "$extra" && -n "$first" && -n "$second" && -n "$third" && -n "$fourth" ]] || return 1
  for octet in "$first" "$second" "$third" "$fourth"; do
    [[ "$octet" =~ ^[0-9]{1,3}$ && $((10#$octet)) -le 255 ]] || return 1
  done
}

valid_private_ipv4() {
  local value="$1" first second third fourth
  valid_ipv4 "$value" || return 1
  IFS=. read -r first second third fourth <<<"$value"
  [[ "$first" == "10" ]] ||
    [[ "$first" == "172" && $((10#$second)) -ge 16 && $((10#$second)) -le 31 ]] ||
    [[ "$first" == "192" && "$second" == "168" ]]
}

valid_private_cidr() {
  local value="$1" address prefix
  [[ "$value" == */* ]] || return 1
  address=${value%/*}
  prefix=${value#*/}
  valid_private_ipv4 "$address" && [[ "$prefix" =~ ^[0-9]{1,2}$ && "$prefix" -ge 16 && "$prefix" -le 32 ]]
}

strip_wg_quick_config() {
  awk '
    /^\[Interface\][[:space:]]*$/ { section = "interface"; print; next }
    /^\[[^]]+\][[:space:]]*$/ { section = "other"; print; next }
    {
      key = $0
      sub(/[[:space:]]*=.*$/, "", key)
      gsub(/^[[:space:]]+|[[:space:]]+$/, "", key)
      if (section == "interface" && (key == "Address" || key == "DNS" || key == "MTU" || key == "Table" ||
          key == "PreUp" || key == "PostUp" || key == "PreDown" || key == "PostDown" || key == "SaveConfig")) next
      print
    }
  ' "$1"
}

export DEBIAN_FRONTEND=noninteractive
if command -v apt-get >/dev/null 2>&1; then
  apt-get update
  apt-get install -y --no-install-recommends ca-certificates curl jq wireguard-tools iperf3 nftables iproute2
elif command -v dnf >/dev/null 2>&1; then
  dnf install -y ca-certificates curl jq wireguard-tools iperf3 nftables iproute
elif command -v yum >/dev/null 2>&1; then
  yum install -y ca-certificates curl jq wireguard-tools iperf3 nftables iproute
else
  echo "supported package manager not found (apt-get, dnf, or yum required)" >&2
  exit 1
fi

install -d -m 0700 "$STATE_ROOT" "$CONFIG_ROOT"
if [[ ! -s "$key_file" ]]; then
  wg genkey >"$key_file"
  chmod 0600 "$key_file"
fi
private_key=$(tr -d '[:space:]' <"$key_file")
public_key=$(printf '%s' "$private_key" | wg pubkey)

request_file=$(mktemp)
response_file=$(mktemp)
trap 'rm -f "$request_file" "$response_file"' EXIT
jq -cn --arg token "$TOKEN" --arg public_key "$public_key" '{token:$token, public_key:$public_key}' >"$request_file"

for attempt in $(seq 1 30); do
  status=$(curl --silent --show-error --output "$response_file" --write-out '%{http_code}' \
    --proto '=https' --tlsv1.2 --request POST \
    --header 'Content-Type: application/json' --data-binary "@$request_file" \
    "$CONTROL_URL/api/wireguard/v1/configure")
  if [[ "$status" == "200" ]]; then
    break
  fi
  if [[ "$status" == "409" && $attempt -lt 30 ]]; then
    echo "waiting for edge WireGuard public keys ($attempt/30)"
    sleep 3
    continue
  fi
  detail=$(jq -r '.error // "configuration request failed"' "$response_file" 2>/dev/null || true)
  echo "WireGuard configuration request failed (HTTP $status): $detail" >&2
  exit 1
done

response_tunnel_id=$(jq -r '.tunnel_id // empty' "$response_file")
response_interface=$(jq -r '.interface_name // empty' "$response_file")
origin_cidr=$(jq -r '.origin_address_cidr // empty' "$response_file")
listen_port=$(jq -r '.listen_port // 0' "$response_file")
performance_port=$(jq -r '.performance_port // 0' "$response_file")
origin_egress_limit_mbps=$(jq -r '.origin_egress_limit_mbps // 0' "$response_file")
mtu=$(jq -r '.mtu // 0' "$response_file")
revision=$(jq -r '.revision // 0' "$response_file")
origin_cidr_valid=0
if valid_private_cidr "$origin_cidr"; then origin_cidr_valid=1; fi

if [[ "$response_tunnel_id" != "$TUNNEL_ID" || "$response_interface" != "$interface" ||
      "$origin_cidr_valid" != "1" ||
      ! "$listen_port" =~ ^[0-9]+$ || "$listen_port" -lt 1 || "$listen_port" -gt 65535 ||
      ! "$performance_port" =~ ^[0-9]+$ || "$performance_port" -lt 1 || "$performance_port" -gt 65535 ||
      ! "$origin_egress_limit_mbps" =~ ^[0-9]+$ || "$origin_egress_limit_mbps" -gt 10000 ||
      ! "$mtu" =~ ^[0-9]+$ || "$mtu" -lt 1280 || "$mtu" -gt 1500 ||
      ! "$revision" =~ ^[0-9]+$ || "$revision" -lt 1 ]]; then
  echo "control plane returned an invalid WireGuard configuration" >&2
  exit 1
fi

peer_count=$(jq '.peers | length' "$response_file")
if [[ ! "$peer_count" =~ ^[0-9]+$ || "$peer_count" -lt 1 || "$peer_count" -gt 253 ]]; then
  echo "control plane returned an invalid WireGuard peer set" >&2
  exit 1
fi

temporary_config=$(mktemp "$CONFIG_ROOT/.${interface}.conf.XXXXXX")
temporary_nft=$(mktemp "$CONFIG_ROOT/.${interface}.nft.XXXXXX")
temporary_runtime=$(mktemp "$CONFIG_ROOT/.${interface}.runtime.XXXXXX")
temporary_old_runtime=$(mktemp "$CONFIG_ROOT/.${interface}.old-runtime.XXXXXX")
temporary_nft_update=$(mktemp "$CONFIG_ROOT/.${interface}.nft-update.XXXXXX")
temporary_service=$(mktemp "/etc/systemd/system/.simple-cdn-origin-iperf-${interface}.XXXXXX")
temporary_state=$(mktemp "$STATE_ROOT/.${TUNNEL_ID}.state.XXXXXX")
trap 'rm -f "$request_file" "$response_file" "$temporary_config" "$temporary_nft" "$temporary_runtime" "$temporary_old_runtime" "$temporary_nft_update" "$temporary_service" "$temporary_state"' EXIT
tc_binary=$(command -v tc)
if [[ "$tc_binary" != /* || "$tc_binary" == *[[:space:]]* ]]; then
  echo "tc executable path is invalid" >&2
  exit 1
fi

{
  echo "# Managed by simple_cdn. Re-run the generated command to update peers."
  echo "[Interface]"
  echo "Address = $origin_cidr"
  echo "ListenPort = $listen_port"
  echo "PrivateKey = $private_key"
  echo "MTU = $mtu"
  echo "PostUp = nft -f $nft_file"
  if [[ "$origin_egress_limit_mbps" -gt 0 ]]; then
    echo "PostUp = $tc_binary qdisc replace dev %i root handle 1: htb default 10"
    echo "PostUp = $tc_binary class replace dev %i parent 1: classid 1:10 htb rate ${origin_egress_limit_mbps}mbit ceil ${origin_egress_limit_mbps}mbit"
    echo "PostUp = $tc_binary qdisc replace dev %i parent 1:10 handle 10: fq_codel"
  fi
  echo "PreDown = $tc_binary qdisc delete dev %i root 2>/dev/null || true"
  echo "PreDown = nft delete table inet $table_name 2>/dev/null || true"
  while IFS= read -r peer; do
    peer_key=$(jq -r '.public_key' <<<"$peer")
    allowed_ip=$(jq -r '.allowed_ip' <<<"$peer")
    peer_address=${allowed_ip%/32}
    peer_address_valid=0
    if valid_private_ipv4 "$peer_address"; then peer_address_valid=1; fi
    if [[ ! "$peer_key" =~ ^[A-Za-z0-9+/]{43}=$ || "$allowed_ip" != */32 || "$peer_address_valid" != "1" ]]; then
      echo "control plane returned an invalid WireGuard peer" >&2
      exit 1
    fi
    echo
    echo "[Peer]"
    echo "PublicKey = $peer_key"
    echo "AllowedIPs = $allowed_ip"
  done < <(jq -c '.peers[]' "$response_file")
} >"$temporary_config"
chmod 0600 "$temporary_config"
strip_wg_quick_config "$temporary_config" >"$temporary_runtime"
chmod 0600 "$temporary_runtime"

while IFS= read -r edge_ip; do
  if ! valid_ipv4 "$edge_ip"; then
    echo "control plane returned an invalid edge IP allowlist" >&2
    exit 1
  fi
done < <(jq -r '.peers[].public_ipv4' "$response_file")
edge_ips=$(jq -r '[.peers[].public_ipv4] | join(", ")' "$response_file")
iperf3_binary=$(command -v iperf3)
if [[ "$iperf3_binary" != /* || "$iperf3_binary" == *[[:space:]]* ]]; then
  echo "iperf3 executable path is invalid" >&2
  exit 1
fi
cat >"$temporary_nft" <<EOF
table inet $table_name {
  chain input {
    type filter hook input priority -5; policy accept;
    iifname "$interface" tcp dport $performance_port accept
    ip saddr { $edge_ips } tcp dport $performance_port accept
    tcp dport $performance_port drop
    ip saddr { $edge_ips } udp dport $listen_port accept
    udp dport $listen_port drop
  }
}
EOF
chmod 0600 "$temporary_nft"

cat >"$temporary_service" <<EOF
[Unit]
Description=simple_cdn origin performance server ($interface)
After=network-online.target wg-quick@$interface.service
Requires=wg-quick@$interface.service

[Service]
Type=simple
ExecStart=$iperf3_binary -s -p $performance_port
Restart=on-failure
RestartSec=2s

[Install]
WantedBy=multi-user.target
EOF
chmod 0644 "$temporary_service"
jq 'del(.peers[].public_key)' "$response_file" >"$temporary_state"
chmod 0600 "$temporary_state"

cat >"$temporary_nft_update" <<EOF
flush chain inet $table_name input
add rule inet $table_name input iifname "$interface" tcp dport $performance_port accept
add rule inet $table_name input ip saddr { $edge_ips } tcp dport $performance_port accept
add rule inet $table_name input tcp dport $performance_port drop
add rule inet $table_name input ip saddr { $edge_ips } udp dport $listen_port accept
add rule inet $table_name input udp dport $listen_port drop
EOF
chmod 0600 "$temporary_nft_update"

apply_nft_rules() {
  local full_config="$1"
  if nft list chain inet "$table_name" input >/dev/null 2>&1; then
    nft -f "$temporary_nft_update"
    return
  fi
  nft delete table inet "$table_name" >/dev/null 2>&1 || true
  nft -f "$full_config"
}

restore_nft_rules() {
  nft delete table inet "$table_name" >/dev/null 2>&1 || true
  if [[ -s "$nft_file" ]]; then
    nft -f "$nft_file"
  fi
}

managed_qdisc_active() {
  "$tc_binary" qdisc show dev "$interface" 2>/dev/null | grep -q 'qdisc htb 1: root'
}

apply_egress_limit() {
  local limit="$1" rate
  if [[ "$limit" -eq 0 ]]; then
    if managed_qdisc_active; then
      "$tc_binary" qdisc delete dev "$interface" root
    fi
    return
  fi
  rate="${limit}mbit"
  "$tc_binary" qdisc replace dev "$interface" root handle 1: htb default 10
  "$tc_binary" class replace dev "$interface" parent 1: classid 1:10 htb rate "$rate" ceil "$rate"
  "$tc_binary" qdisc replace dev "$interface" parent 1:10 handle 10: fq_codel
}

config_changed=1
nft_changed=1
service_changed=1
if [[ -s "$config_file" ]] && cmp -s "$config_file" "$temporary_config"; then config_changed=0; fi
if [[ -s "$nft_file" ]] && cmp -s "$nft_file" "$temporary_nft"; then nft_changed=0; fi
if [[ -s "$service_file" ]] && cmp -s "$service_file" "$temporary_service"; then service_changed=0; fi

interface_active=0
if ip link show dev "$interface" >/dev/null 2>&1; then
  if wg show "$interface" >/dev/null 2>&1; then
    interface_active=1
  else
    echo "managed interface $interface exists but is not an active WireGuard interface" >&2
    exit 1
  fi
fi

if [[ $interface_active == 1 ]]; then
  if [[ ! -s "$config_file" ]]; then
    echo "active WireGuard interface $interface has no managed configuration file" >&2
    exit 1
  fi
  old_origin_cidr=$(awk -F ' *= *' '$1 == "Address" { print $2; exit }' "$config_file")
  old_mtu=$(awk -F ' *= *' '$1 == "MTU" { print $2; exit }' "$config_file")
  old_egress_limit_mbps=$(sed -n 's/.* htb rate \([0-9][0-9]*\)mbit ceil .*/\1/p' "$config_file" | head -n 1)
  old_egress_limit_mbps=${old_egress_limit_mbps:-0}
  old_origin_cidr_valid=0
  if valid_private_cidr "$old_origin_cidr"; then old_origin_cidr_valid=1; fi
  if [[ "$old_origin_cidr_valid" != "1" || ! "$old_mtu" =~ ^[0-9]+$ || "$old_mtu" -lt 1280 || "$old_mtu" -gt 1500 ||
        ! "$old_egress_limit_mbps" =~ ^[0-9]+$ || "$old_egress_limit_mbps" -gt 10000 ]]; then
    echo "existing managed WireGuard configuration is invalid; refusing a disruptive replacement" >&2
    exit 1
  fi
  strip_wg_quick_config "$config_file" >"$temporary_old_runtime"
  chmod 0600 "$temporary_old_runtime"

  nft_updated=0
  wireguard_updated=0
  address_updated=0
  mtu_updated=0
  egress_updated=0
  rollback_live_update() {
    local rollback_failed=0
    set +e
    if [[ $egress_updated == 1 ]]; then apply_egress_limit "$old_egress_limit_mbps" || rollback_failed=1; fi
    if [[ $mtu_updated == 1 ]]; then ip link set dev "$interface" mtu "$old_mtu" || rollback_failed=1; fi
    if [[ $address_updated == 1 ]]; then
      ip -4 address replace "$old_origin_cidr" dev "$interface" || rollback_failed=1
      ip -4 address delete "$origin_cidr" dev "$interface" || rollback_failed=1
    fi
    if [[ $wireguard_updated == 1 ]]; then wg syncconf "$interface" "$temporary_old_runtime" || rollback_failed=1; fi
    if [[ $nft_updated == 1 ]]; then restore_nft_rules || rollback_failed=1; fi
    set -e
    if [[ $rollback_failed == 1 ]]; then
      echo "warning: failed to fully restore the previous WireGuard runtime configuration" >&2
    fi
  }

  if [[ $nft_changed == 1 ]] || ! nft list chain inet "$table_name" input >/dev/null 2>&1; then
    if ! apply_nft_rules "$temporary_nft"; then
      echo "failed to update the managed WireGuard firewall rules" >&2
      exit 1
    fi
    nft_updated=1
  fi
  if [[ $config_changed == 1 ]]; then
    if ! wg syncconf "$interface" "$temporary_runtime"; then
      rollback_live_update
      echo "failed to synchronize the active WireGuard interface; the previous configuration remains active" >&2
      exit 1
    fi
    wireguard_updated=1
    if [[ "$old_origin_cidr" != "$origin_cidr" ]]; then
      if ! ip -4 address replace "$origin_cidr" dev "$interface"; then
        rollback_live_update
        echo "failed to add the updated WireGuard source address" >&2
        exit 1
      fi
      address_updated=1
      if ! ip -4 address delete "$old_origin_cidr" dev "$interface"; then
        rollback_live_update
        echo "failed to remove the previous WireGuard source address" >&2
        exit 1
      fi
    fi
    if [[ "$old_mtu" != "$mtu" ]]; then
      if ! ip link set dev "$interface" mtu "$mtu"; then
        rollback_live_update
        echo "failed to update the WireGuard MTU" >&2
        exit 1
      fi
      mtu_updated=1
    fi
  fi
  egress_needs_apply=$config_changed
  if [[ $egress_needs_apply == 0 ]]; then
    if [[ "$origin_egress_limit_mbps" -gt 0 ]]; then
      if ! managed_qdisc_active; then egress_needs_apply=1; fi
    elif managed_qdisc_active; then
      egress_needs_apply=1
    fi
  fi
  if [[ $egress_needs_apply == 1 ]]; then
    egress_updated=1
    if ! apply_egress_limit "$origin_egress_limit_mbps"; then
      rollback_live_update
      echo "failed to update the WireGuard source egress limit" >&2
      exit 1
    fi
  fi
  mv "$temporary_config" "$config_file"
  mv "$temporary_nft" "$nft_file"
else
  mv "$temporary_config" "$config_file"
  mv "$temporary_nft" "$nft_file"
  nft delete table inet "$table_name" >/dev/null 2>&1 || true
  systemctl enable "wg-quick@$interface.service"
  systemctl restart "wg-quick@$interface.service"
fi

mv "$temporary_service" "$service_file"
mv "$temporary_state" "$state_file"
systemctl daemon-reload
systemctl enable "wg-quick@$interface.service"
systemctl enable "simple-cdn-origin-iperf-$interface.service"
if [[ $service_changed == 1 ]] || ! systemctl is-active --quiet "simple-cdn-origin-iperf-$interface.service"; then
  systemctl restart "simple-cdn-origin-iperf-$interface.service"
fi

echo "WireGuard origin tunnel $TUNNEL_ID applied at revision $revision"
echo "Interface: $interface ($origin_cidr), performance port: $performance_port, egress limit: ${origin_egress_limit_mbps} Mbps (0 = unlimited)"
