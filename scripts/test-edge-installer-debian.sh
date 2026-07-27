#!/usr/bin/env bash
set -euo pipefail

readonly DEFAULT_ARTIFACT="dist/cdn-nginx-linux-amd64.tar.gz"
readonly TEST_IMAGES=("debian:12-slim" "debian:13-slim")

install_mock_commands() {
  install -d -m 0755 /mock-bin /run/cdn-edge-installer-test

  cat >/mock-bin/systemctl <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
state_dir=/run/cdn-edge-installer-test
command="${1:-}"
service="${*: -1}"
case "$command" in
  is-active)
    [[ -f "$state_dir/$service.active" ]]
    ;;
  is-enabled)
    [[ -f "$state_dir/$service.enabled" ]]
    ;;
  enable)
    touch "$state_dir/$service.enabled"
    ;;
  disable)
    rm -f "$state_dir/$service.enabled"
    ;;
  stop)
    if [[ "$service" == "nginx" || "$service" == "nginx.service" ]]; then
      if [[ -x /opt/cdn-edge/nginx/sbin/nginx && -s /opt/cdn-edge/nginx/run/nginx.pid ]]; then
        /opt/cdn-edge/nginx/sbin/nginx -s quit >/dev/null 2>&1 || true
      fi
    fi
    rm -f "$state_dir/$service.active"
    ;;
  start|restart)
    if [[ "$service" == "nginx" || "$service" == "nginx.service" ]]; then
      /opt/cdn-edge/nginx/sbin/nginx
    else
      if [[ "${MOCK_INSTALL_FAILURE:-}" == "agent" ]]; then
        exit 1
      fi
      install -d -m 0750 /opt/cdn-edge/data
      for name in edge-client.key edge-client.crt edge-ca.crt; do
        printf '%s\n' "$name" >"/opt/cdn-edge/data/$name"
        chmod 0600 "/opt/cdn-edge/data/$name"
      done
    fi
    touch "$state_dir/$service.active"
    ;;
  daemon-reload|reset-failed)
    ;;
  *)
    echo "unexpected systemctl invocation: $*" >&2
    exit 1
    ;;
esac
EOF

  cat >/mock-bin/sysctl <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
while [[ "${1:-}" == "-q" ]]; do shift; done
case "${1:-}" in
  -n)
    case "$2" in
      net.core.default_qdisc) echo fq_codel ;;
      net.ipv4.tcp_congestion_control) echo cubic ;;
      net.ipv4.tcp_mtu_probing) echo 0 ;;
      net.core.rmem_max|net.core.wmem_max) echo 4194304 ;;
      net.ipv4.tcp_rmem) echo '4096 131072 6291456' ;;
      net.ipv4.tcp_wmem) echo '4096 16384 4194304' ;;
      *) exit 1 ;;
    esac
    ;;
  -w|-p|--system) exit 0 ;;
  *) exit 1 ;;
esac
EOF

  printf '%s\n' '#!/usr/bin/env bash' 'exit 0' >/mock-bin/modprobe
  printf '%s\n' '#!/usr/bin/env bash' 'exit 0' >/mock-bin/sleep
  chmod 0755 /mock-bin/systemctl /mock-bin/sysctl /mock-bin/modprobe /mock-bin/sleep
}

debian_nginx_packages_present() {
  (dpkg-query -W -f='${binary:Package}\n' 2>/dev/null || true) |
    awk '$1 ~ /^nginx($|-)/ || $1 ~ /^libnginx-mod-/ { found=1 } END { exit found ? 0 : 1 }'
}

container_test() {
  local scenario="$1"
  export DEBIAN_FRONTEND=noninteractive
  apt-get update
  apt-get install -y --no-install-recommends nginx
  printf '%s\n' 'operator configuration must survive rollback' >/etc/nginx/operator-marker.conf
  install_mock_commands
  export PATH="/mock-bin:/usr/sbin:/usr/bin:/sbin:/bin"

  printf '%s\n' '#!/usr/bin/env bash' 'exit 0' >/tmp/cdn-edge-agent
  chmod 0755 /tmp/cdn-edge-agent
  local binary_sha agent_service_sha updater_service_sha nginx_bundle_sha nginx_service_sha
  binary_sha=$(sha256sum /tmp/cdn-edge-agent | awk '{print $1}')
  agent_service_sha=$(sha256sum /source/internal/control/install-edge.service | awk '{print $1}')
  updater_service_sha=$(sha256sum /source/internal/control/install-edge-updater.service | awk '{print $1}')
  nginx_bundle_sha=$(sha256sum /fixtures/cdn-nginx-linux-amd64.tar.gz | awk '{print $1}')
  nginx_service_sha=$(sha256sum /source/internal/control/install-edge-nginx.service | awk '{print $1}')

  local -a arguments=(
    /source/internal/control/install-edge.sh
    --control-url https://control.example.test
    --enrollment-token integration-token
    --binary-file /tmp/cdn-edge-agent
    --binary-sha256 "$binary_sha"
    --service-file /source/internal/control/install-edge.service
    --service-sha256 "$agent_service_sha"
    --updater-service-file /source/internal/control/install-edge-updater.service
    --updater-service-sha256 "$updater_service_sha"
    --nginx-bundle-file /fixtures/cdn-nginx-linux-amd64.tar.gz
    --nginx-bundle-sha256 "$nginx_bundle_sha"
    --nginx-service-file /source/internal/control/install-edge-nginx.service
    --nginx-service-sha256 "$nginx_service_sha"
  )
  local -a bash_options=()
  if [[ "${DEBUG_INSTALLER:-}" == "1" ]]; then bash_options=(-x); fi

  if [[ "$scenario" == "success" ]]; then
    bash "${bash_options[@]}" "${arguments[@]}"
    if debian_nginx_packages_present; then
      echo "Debian Nginx packages remain installed" >&2
      return 1
    fi
    [[ ! -e /etc/nginx ]]
    [[ "$(cat /opt/cdn-edge/.layout-version)" == "2" ]]
    [[ "$(cat /opt/cdn-edge/nginx/VERSION)" == "1.30.4" ]]
    [[ "$(stat -c '%a' /opt/cdn-edge/nginx)" == "755" ]]
    [[ "$(stat -c '%a' /opt/cdn-edge/nginx/lib/lua/5.1/resty/core.lua)" == "644" ]]
    curl --fail --silent --show-error http://127.0.0.1/__cdn_health >/dev/null
    /opt/cdn-edge/nginx/sbin/nginx -s quit
    return
  fi

  export MOCK_INSTALL_FAILURE=agent
  set +e
  bash "${bash_options[@]}" "${arguments[@]}"
  local status=$?
  set -e
  if ((status == 0)); then
    echo "failure scenario unexpectedly succeeded" >&2
    return 1
  fi
  debian_nginx_packages_present
  grep -Fqx 'operator configuration must survive rollback' /etc/nginx/operator-marker.conf
  [[ ! -e /opt/cdn-edge ]]
}

main() {
  if [[ "${1:-}" == "--container" ]]; then
    [[ $# -eq 2 && ( "$2" == "success" || "$2" == "rollback" ) ]] || {
      echo "usage: $0 --container success|rollback" >&2
      return 2
    }
    container_test "$2"
    return
  fi

  local artifact="${1:-$DEFAULT_ARTIFACT}"
  [[ -f "$artifact" ]] || {
    echo "Nginx artifact not found: $artifact" >&2
    return 2
  }
  command -v docker >/dev/null 2>&1 || {
    echo "docker is required to test Debian Nginx migration" >&2
    return 2
  }
  local repository artifact_path script_path image scenario
  repository=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
  artifact_path=$(cd -- "$(dirname -- "$artifact")" && pwd)/$(basename -- "$artifact")
  script_path="$repository/scripts/$(basename -- "${BASH_SOURCE[0]}")"
  for image in "${TEST_IMAGES[@]}"; do
    for scenario in success rollback; do
      echo "Testing $scenario migration on $image"
      docker run --rm \
        --mount "type=bind,src=$repository,dst=/source,readonly" \
        --mount "type=bind,src=$artifact_path,dst=/fixtures/cdn-nginx-linux-amd64.tar.gz,readonly" \
        --mount "type=bind,src=$script_path,dst=/test-edge-installer-debian.sh,readonly" \
        "$image" bash /test-edge-installer-debian.sh --container "$scenario"
    done
  done
}

main "$@"
