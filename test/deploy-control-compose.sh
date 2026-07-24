#!/usr/bin/env bash
set -Eeuo pipefail

if [[ $EUID -ne 0 ]]; then
  echo "deploy-control-compose test must run as root" >&2
  exit 2
fi

repository_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
image_ref="ghcr.io/saginardo/simple_cdn@sha256:$(printf 'a%.0s' {1..64})"

run_case() (
  local mode="$1"
  local case_root deployment_root fake_bin log_file
  case_root=$(mktemp -d /tmp/cdn-platform-deploy-test.XXXXXX)
  deployment_root="$case_root/deployment"
  fake_bin="$case_root/bin"
  log_file="$case_root/docker.log"
  cleanup_case() {
    local exit_code=$?
    if ((exit_code != 0)) && [[ -r "$log_file" ]]; then
      echo "fake Docker log for failed $mode deployment:" >&2
      sed 's/^/  /' "$log_file" >&2
    fi
    rm -rf "$case_root"
    exit "$exit_code"
  }
  trap cleanup_case EXIT

  install -d "$deployment_root/app" "$deployment_root/config" "$fake_bin"
  printf 'name: cdn-platform\n' >"$deployment_root/compose.yaml"
  printf 'CDN_SOURCE_DIR=./app\n' >"$deployment_root/.env"
  printf '%s\n' \
    'EDGE_BINARY_PATH=/usr/local/lib/cdn-platform/cdn-edge-agent-linux-amd64' \
    'EDGE_BINARY_SHA256=legacy-digest' \
    'CLICKHOUSE_DATABASE=cdn_platform' >"$deployment_root/config/control.env"
  printf 'CLICKHOUSE_DATABASE=cdn_platform\n' >"$deployment_root/config/backup.env"
  printf 'old support files\n' >"$deployment_root/app/marker"

  apply_fake_docker "$fake_bin/docker"
  if [[ "$mode" == success ]]; then
    PATH="$fake_bin:$PATH" \
      FAKE_DEPLOY_ROOT="$deployment_root" FAKE_DOCKER_LOG="$log_file" FAKE_MODE="$mode" \
      DEPLOY_HEALTH_TIMEOUT_SECONDS=1 \
      "$repository_root/scripts/deploy-control-compose.sh" "$image_ref" "$deployment_root"

    grep -Fxq "CDN_CONTROL_IMAGE=$image_ref" "$deployment_root/.env"
    cmp "$repository_root/deploy/docker-compose.yaml" "$deployment_root/compose.yaml"
    [[ -d "$deployment_root/app/deploy" && ! -e "$deployment_root/app/marker" ]]
    ! grep -q '^EDGE_BINARY_SHA256=' "$deployment_root/config/control.env"
    grep -Fxq 'CLICKHOUSE_DATABASE=simple_cdn' "$deployment_root/config/control.env"
    grep -Fxq 'CLICKHOUSE_DATABASE=simple_cdn' "$deployment_root/config/backup.env"
    grep -Fxq 'simple_cdn' "$deployment_root/fake-clickhouse-database"
    grep -Fq -- '--no-build' "$log_file"
    grep -Fq 'compose -p cdn-platform -f' "$log_file"
    grep -Fq 'compose -p simple_cdn up -d --no-build control' "$log_file"
    grep -Fq 'compose -p simple_cdn up -d --no-build --no-deps backup' "$log_file"
    for obsolete in \
      'ghcr.io/saginardo/simple_cdn:sha-old' \
      'ghcr.io/saginardo/simple_cdn@sha256:old-digest' \
      'sha256:old-ghcr' \
      'ghcr.io/saginardo/cdn_platform:main' \
      'ghcr.io/saginardo/cdn_platform@sha256:legacy-digest' \
      'sha256:legacy-ghcr' \
      'cdn-platform-control:local' \
      'sha256:old-local'; do
      grep -Fq "image rm $obsolete" "$log_file"
    done
    if grep -Eq 'image rm (ghcr\.io/saginardo/simple_cdn:main|sha256:requested-image|unrelated/service|sha256:unrelated-image)' "$log_file"; then
      echo "deployment attempted to remove the current or an unrelated image" >&2
      return 1
    fi
  else
    if PATH="$fake_bin:$PATH" \
      FAKE_DEPLOY_ROOT="$deployment_root" FAKE_DOCKER_LOG="$log_file" FAKE_MODE="$mode" \
      DEPLOY_HEALTH_TIMEOUT_SECONDS=1 \
      "$repository_root/scripts/deploy-control-compose.sh" "$image_ref" "$deployment_root"; then
      echo "unhealthy deployment unexpectedly succeeded" >&2
      return 1
    fi

    grep -Fxq 'name: cdn-platform' "$deployment_root/compose.yaml"
    grep -Fxq 'CDN_SOURCE_DIR=./app' "$deployment_root/.env"
    grep -Fxq 'old support files' "$deployment_root/app/marker"
    grep -Fxq 'EDGE_BINARY_SHA256=legacy-digest' "$deployment_root/config/control.env"
    grep -Fxq 'CLICKHOUSE_DATABASE=cdn_platform' "$deployment_root/config/control.env"
    grep -Fxq 'CLICKHOUSE_DATABASE=cdn_platform' "$deployment_root/config/backup.env"
    grep -Fxq 'cdn_platform' "$deployment_root/fake-clickhouse-database"
    grep -Fq "pull $image_ref" "$log_file"
    if [[ $(grep -Fc 'up -d --no-build control ' "$log_file") -lt 2 ]]; then
      echo "failure case did not exercise both cutover and rollback" >&2
      return 1
    fi
  fi

  if compgen -G "$deployment_root/.deploy-rollback.*" >/dev/null; then
    echo "completed deployment left rollback files behind" >&2
    return 1
  fi
  if grep -Eq '(^| )build( |$)' "$log_file"; then
    echo "deployment attempted a host-side image build" >&2
    return 1
  fi
  if grep -Fq 'image prune' "$log_file"; then
    echo "deployment attempted to prune unrelated Docker images" >&2
    return 1
  fi
)

apply_fake_docker() {
  local target="$1"
  install -m 0755 "$repository_root/test/fake-docker" "$target"
}

run_case success
run_case failure

if (cd "$repository_root" && ./scripts/install-control-compose.sh /tmp/../.. "$image_ref"); then
  echo "installer accepted a path that resolves to /" >&2
  exit 1
fi

echo "deployment success and rollback paths passed"
