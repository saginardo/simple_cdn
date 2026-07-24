#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

usage() {
  echo "usage: deploy-control-compose.sh <ghcr-image-tag-or-digest> [root]" >&2
}

if (($# < 1 || $# > 2)); then
  usage
  exit 2
fi
if [[ $EUID -ne 0 ]]; then
  echo "deploy-control-compose.sh must run as root" >&2
  exit 2
fi

image_ref="$1"
root="${2:-/opt/cdn-platform}"
health_timeout="${DEPLOY_HEALTH_TIMEOUT_SECONDS:-120}"
image_pattern='^ghcr\.io/saginardo/simple_cdn(:[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}|@sha256:[a-f0-9]{64})$'
if [[ ! "$image_ref" =~ $image_pattern ]]; then
  echo "image must be a ghcr.io/saginardo/simple_cdn tag or digest" >&2
  exit 2
fi
if [[ "$root" != /* || "$root" == "/" ]]; then
  echo "root must be an absolute path below /" >&2
  exit 2
fi
root=$(realpath -m -- "$root")
if [[ "$root" == "/" ]]; then
  echo "root must resolve below /" >&2
  exit 2
fi
if [[ ! "$health_timeout" =~ ^[1-9][0-9]*$ ]] || ((health_timeout > 1800)); then
  echo "DEPLOY_HEALTH_TIMEOUT_SECONDS must be between 1 and 1800" >&2
  exit 2
fi

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
source_root=$(cd -- "$script_dir/.." && pwd)
for required in "$source_root/deploy/docker-compose.yaml" "$source_root/scripts/install-control-compose.sh"; do
  if [[ ! -e "$required" ]]; then
    echo "deployment bundle is missing $required" >&2
    exit 2
  fi
done
for required in "$root/compose.yaml" "$root/.env" "$root/app" "$root/config/control.env" "$root/config/backup.env"; do
  if [[ ! -e "$required" ]]; then
    echo "existing Compose deployment is missing $required; run install-control-compose.sh first" >&2
    exit 2
  fi
done

compose_project_name() {
  local file="$1" project
  project=$(sed -nE 's/^name:[[:space:]]*"?([a-z0-9][a-z0-9_-]*)"?[[:space:]]*$/\1/p' "$file" | head -n 1)
  if [[ -z "$project" ]]; then
    echo "Compose definition has no valid top-level project name: $file" >&2
    return 1
  fi
  printf '%s\n' "$project"
}

active_project=$(compose_project_name "$root/compose.yaml")
target_project=$(compose_project_name "$source_root/deploy/docker-compose.yaml")
project_changed=0
if [[ "$active_project" != "$target_project" ]]; then
  project_changed=1
fi

service_is_running() {
  local project="$1" service="$2" running_services
  running_services=$(docker compose -p "$project" --profile backup ps --status running --services 2>/dev/null) || return 1
  grep -Fxq "$service" <<<"$running_services"
}

clickhouse_database_count() {
  local project="$1" database="$2" count
  if ! count=$(docker compose -p "$project" exec -T clickhouse clickhouse-client \
    --query "SELECT count() FROM system.databases WHERE name = '$database' FORMAT TSVRaw"); then
    echo "failed to inspect ClickHouse database $database" >&2
    return 1
  fi
  if [[ "$count" != "0" && "$count" != "1" ]]; then
    echo "unexpected ClickHouse database count for $database: $count" >&2
    return 1
  fi
  printf '%s\n' "$count"
}

rename_clickhouse_database() {
  local project="$1" source="$2" target="$3"
  docker compose -p "$project" exec -T clickhouse clickhouse-client \
    --query "RENAME DATABASE $source TO $target"
}

wait_for_control() {
  local project="$1" deadline=$((SECONDS + health_timeout))
  until docker compose -p "$project" exec -T control curl --fail --silent --insecure https://127.0.0.1:8443/healthz >/dev/null 2>&1; do
    if ((SECONDS >= deadline)); then
      docker compose -p "$project" logs --tail 50 control >&2 || true
      return 1
    fi
    sleep 2 & wait $!
  done
}

prune_obsolete_control_images() {
  local current_image="$1"
  local repository tag digest image_id reference
  local -A obsolete_image_ids=()
  while IFS='|' read -r repository tag digest image_id; do
    [[ -n "$image_id" && "$image_id" != "$current_image" ]] || continue
    case "$repository" in
      ghcr.io/saginardo/simple_cdn|ghcr.io/saginardo/cdn_platform|cdn-platform-control) ;;
      *) continue ;;
    esac
    obsolete_image_ids["$image_id"]=1
    if [[ "$tag" != "<none>" ]]; then
      reference="$repository:$tag"
      docker image rm "$reference" >/dev/null 2>&1 || true
    fi
    if [[ ("$repository" == ghcr.io/saginardo/simple_cdn || "$repository" == ghcr.io/saginardo/cdn_platform) && "$digest" != "<none>" ]]; then
      reference="$repository@$digest"
      docker image rm "$reference" >/dev/null 2>&1 || true
    fi
  done < <(docker image ls --digests --no-trunc --format '{{.Repository}}|{{.Tag}}|{{.Digest}}|{{.ID}}')
  for image_id in "${!obsolete_image_ids[@]}"; do
    docker image rm "$image_id" >/dev/null 2>&1 || true
  done
}

cd "$root"
if ! service_is_running "$active_project" control ||
  ! service_is_running "$active_project" clickhouse ||
  ! service_is_running "$active_project" backup; then
  echo "control, clickhouse, and backup must all be running before an automated deployment" >&2
  exit 1
fi
control_renew_was_running=0
if service_is_running "$active_project" control-cert-renew; then control_renew_was_running=1; fi

configured_clickhouse_database=$(sed -nE 's/^[[:space:]]*CLICKHOUSE_DATABASE[[:space:]]*=[[:space:]]*([^[:space:]#]+)[[:space:]]*$/\1/p' "$root/config/control.env" | tail -n 1)
legacy_database_migration_expected=0
if [[ -z "$configured_clickhouse_database" || "$configured_clickhouse_database" == cdn_platform ]]; then
  legacy_database_exists=$(clickhouse_database_count "$active_project" cdn_platform)
  current_database_exists=$(clickhouse_database_count "$active_project" simple_cdn)
  if ((legacy_database_exists && current_database_exists)); then
    echo "both ClickHouse databases simple_cdn and cdn_platform exist; refusing an ambiguous deployment" >&2
    exit 1
  fi
  if ((legacy_database_exists && !current_database_exists)); then
    legacy_database_migration_expected=1
  fi
fi

echo "Pulling verified control image $image_ref"
docker pull "$image_ref"

rollback_root=$(mktemp -d "$root/.deploy-rollback.XXXXXX")
chmod 0700 "$rollback_root"
deployment_changed=0
deployment_committed=0
rollback_incomplete=0

restore_deployment_files() {
  rm -rf "$root/app" || return 1
  cp -a "$rollback_root/app" "$root/app" || return 1
  install -m 0644 "$rollback_root/compose.yaml" "$root/compose.yaml" || return 1
  install -m 0644 "$rollback_root/.env" "$root/.env" || return 1
  cp -a "$rollback_root/control.env" "$root/config/control.env" || return 1
  cp -a "$rollback_root/backup.env" "$root/config/backup.env" || return 1
}

rollback_clickhouse_database() {
  local project="$1" legacy_exists current_exists
  ((legacy_database_migration_expected)) || return 0
  legacy_exists=$(clickhouse_database_count "$project" cdn_platform) || return 1
  current_exists=$(clickhouse_database_count "$project" simple_cdn) || return 1
  if ((current_exists && !legacy_exists)); then
    if ! rename_clickhouse_database "$project" simple_cdn cdn_platform; then
      echo "failed to restore the legacy ClickHouse database name" >&2
      return 1
    fi
    return 0
  fi
  if ((legacy_exists && !current_exists)); then
    return 0
  fi
  echo "cannot determine how to restore the legacy ClickHouse database name" >&2
  return 1
}

rollback_deployment() {
  local database_project
  echo "Deployment failed; restoring the previous Compose definition and image" >&2
  set +e
  cd "$root" || return 1
  database_project="$target_project"
  if ! service_is_running "$database_project" clickhouse; then
    database_project="$active_project"
  fi
  docker compose -p "$target_project" stop control >/dev/null 2>&1 || true
  rollback_clickhouse_database "$database_project" || return 1
  if ((project_changed)); then
    docker compose -p "$target_project" down || return 1
  fi
  restore_deployment_files || return 1
  cd "$root" || return 1
  docker compose -p "$active_project" config --quiet || return 1
  if ((project_changed)); then
    docker compose -p "$active_project" up -d --no-build control || return 1
  else
    docker compose -p "$active_project" up -d --no-build --no-deps control || return 1
  fi
  wait_for_control "$active_project" || return 1
  if ((control_renew_was_running)); then
    docker compose -p "$active_project" up -d --no-build --no-deps control-cert-renew || return 1
  fi
  docker compose -p "$active_project" --profile backup up -d --no-build --no-deps backup || return 1
  echo "Previous control deployment restored" >&2
  return 0
}

cleanup() {
  local exit_code=$?
  trap - EXIT
  if ((exit_code != 0 && deployment_changed && !deployment_committed)); then
    if ! rollback_deployment; then
      rollback_incomplete=1
      echo "automatic deployment rollback is incomplete; evidence retained at $rollback_root" >&2
    fi
  fi
  if ((!rollback_incomplete)); then
    rm -rf "$rollback_root"
  fi
  exit "$exit_code"
}
trap cleanup EXIT

cp -a "$root/compose.yaml" "$rollback_root/compose.yaml"
cp -a "$root/.env" "$rollback_root/.env"
cp -a "$root/app" "$rollback_root/app"
cp -a "$root/config/control.env" "$rollback_root/control.env"
cp -a "$root/config/backup.env" "$rollback_root/backup.env"
deployment_changed=1
docker compose -p "$active_project" --profile backup stop backup
(cd "$source_root" && ./scripts/install-control-compose.sh "$root" "$image_ref")
cd "$root"
docker compose -p "$target_project" config --quiet

if ((project_changed)); then
  docker compose -p "$active_project" -f "$rollback_root/compose.yaml" --project-directory "$root" down
  docker compose -p "$target_project" up -d --no-build control
else
  docker compose -p "$target_project" up -d --no-build --no-deps control
fi
wait_for_control "$target_project"

expected_image=$(docker image inspect --format '{{.Id}}' "$image_ref")
control_container=$(docker compose -p "$target_project" ps -q control)
if [[ -z "$control_container" ]]; then
  echo "control container was not created" >&2
  exit 1
fi
running_image=$(docker inspect --format '{{.Image}}' "$control_container")
if [[ "$running_image" != "$expected_image" ]]; then
  echo "control container is not running the requested image" >&2
  exit 1
fi

if ((control_renew_was_running)); then
  docker compose -p "$target_project" up -d --no-build --no-deps control-cert-renew
  service_is_running "$target_project" control-cert-renew
fi
docker compose -p "$target_project" up -d --no-build --no-deps backup
service_is_running "$target_project" backup

# A completed one-shot bootstrap container would otherwise pin an obsolete image.
docker compose -p "$target_project" rm -f control-cert-bootstrap >/dev/null 2>&1 || true
deployment_committed=1
prune_obsolete_control_images "$expected_image"
echo "Deployed $image_ref and verified control-plane health"
