#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

CONTROL_URL=""
TOKEN=""
TUNNEL_ID=""
UNINSTALL=0
STATE_ROOT="/var/lib/simple-cdn-origin-wireguard"
CONFIG_ROOT="/etc/wireguard"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --control-url) CONTROL_URL="$2"; shift 2 ;;
    --token) TOKEN="$2"; shift 2 ;;
    --tunnel-id) TUNNEL_ID="$2"; shift 2 ;;
    --uninstall) UNINSTALL=1; shift ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

if [[ $EUID -ne 0 ]]; then
  echo "WireGuard origin setup must run as root" >&2
  exit 2
fi
if [[ ! "$TUNNEL_ID" =~ ^[0-9a-fA-F-]{36}$ ]]; then
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

export DEBIAN_FRONTEND=noninteractive
if command -v apt-get >/dev/null 2>&1; then
  apt-get update
  apt-get install -y --no-install-recommends ca-certificates curl jq wireguard-tools iperf3 nftables
elif command -v dnf >/dev/null 2>&1; then
  dnf install -y ca-certificates curl jq wireguard-tools iperf3 nftables
elif command -v yum >/dev/null 2>&1; then
  yum install -y ca-certificates curl jq wireguard-tools iperf3 nftables
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
mtu=$(jq -r '.mtu // 0' "$response_file")
revision=$(jq -r '.revision // 0' "$response_file")
origin_cidr_valid=0
if valid_private_cidr "$origin_cidr"; then origin_cidr_valid=1; fi

if [[ "$response_tunnel_id" != "$TUNNEL_ID" || "$response_interface" != "$interface" ||
      "$origin_cidr_valid" != "1" ||
      ! "$listen_port" =~ ^[0-9]+$ || "$listen_port" -lt 1 || "$listen_port" -gt 65535 ||
      ! "$performance_port" =~ ^[0-9]+$ || "$performance_port" -lt 1 || "$performance_port" -gt 65535 ||
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
trap 'rm -f "$request_file" "$response_file" "$temporary_config" "$temporary_nft"' EXIT

{
  echo "# Managed by simple_cdn. Re-run the generated command to update peers."
  echo "[Interface]"
  echo "Address = $origin_cidr"
  echo "ListenPort = $listen_port"
  echo "PrivateKey = $private_key"
  echo "MTU = $mtu"
  echo "PostUp = nft -f $nft_file"
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

wg-quick down "$interface" >/dev/null 2>&1 || true
nft delete table inet "$table_name" >/dev/null 2>&1 || true
mv "$temporary_config" "$config_file"
mv "$temporary_nft" "$nft_file"

cat >"$service_file" <<EOF
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
chmod 0644 "$service_file"

jq 'del(.peers[].public_key)' "$response_file" >"$state_file"
chmod 0600 "$state_file"
systemctl daemon-reload
systemctl enable --now "wg-quick@$interface.service"
systemctl enable --now "simple-cdn-origin-iperf-$interface.service"

echo "WireGuard origin tunnel $TUNNEL_ID applied at revision $revision"
echo "Interface: $interface ($origin_cidr), performance port: $performance_port"
