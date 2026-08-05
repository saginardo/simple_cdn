#!/usr/bin/env bash
set -euo pipefail

readonly DEFAULT_ARTIFACT="dist/cdn-nginx-linux-amd64.tar.gz"
readonly TEST_IMAGES=("debian:12-slim" "debian:13-slim")

container_smoke_test() {
  local artifact="$1"

  umask 077
  export DEBIAN_FRONTEND=noninteractive
  apt-get update
  apt-get install -y --no-install-recommends \
    ca-certificates curl libcrypt1 libpcre2-8-0 zlib1g
  rm -rf /var/lib/apt/lists/*

  install -d -m 0755 /opt/cdn-edge
  tar -xzf "$artifact" --no-same-owner --no-same-permissions -C /opt/cdn-edge
  find /opt/cdn-edge/nginx -type d -exec chmod 0755 {} +
  find /opt/cdn-edge/nginx -type f -exec chmod 0644 {} +
  chmod 0755 /opt/cdn-edge/nginx/sbin/nginx
  for notice in \
    nginx.txt ngx_devel_kit.txt openresty-luajit.txt lua-nginx-module.txt \
    lua-resty-core.txt lua-resty-lrucache.txt ngx_brotli.txt brotli.txt \
    zstd-nginx-module.txt zstd-library.txt; do
    test -s "/opt/cdn-edge/nginx/licenses/$notice"
  done
  install -d -m 0755 \
    /opt/cdn-edge/config/nginx \
    /opt/cdn-edge/logs \
    /opt/cdn-edge/nginx/run
  install -d -o www-data -g www-data -m 0700 \
    /opt/cdn-edge/nginx/tmp/body \
    /opt/cdn-edge/nginx/tmp/fastcgi \
    /opt/cdn-edge/nginx/tmp/proxy \
    /opt/cdn-edge/nginx/tmp/scgi \
    /opt/cdn-edge/nginx/tmp/uwsgi

  printf '%s\n' \
    'worker_processes 1;' \
    'worker_rlimit_nofile 1024;' \
    >/opt/cdn-edge/config/nginx/cdn-platform-main.conf
  printf '%s\n' \
    'worker_connections 128;' \
    >/opt/cdn-edge/config/nginx/cdn-platform-events.conf
  : >/opt/cdn-edge/config/nginx/cdn-platform-stream.conf
  printf '%s\n' \
    'server {' \
    '    listen 127.0.0.1:18080;' \
    '    gzip on;' \
    '    gzip_min_length 20;' \
    '    gzip_types text/plain;' \
    '    brotli on;' \
    '    brotli_min_length 20;' \
    '    brotli_types text/plain;' \
    '    zstd on;' \
    '    zstd_min_length 20;' \
    '    zstd_types text/plain;' \
    '    location = /__artifact_smoke {' \
    '        content_by_lua_block {' \
    '            local lrucache = require "resty.lrucache"' \
    '            local cache = lrucache.new(2)' \
    '            cache:set("result", "managed-nginx-ok")' \
    '            local result = cache:get("result")' \
    '            ngx.say(result)' \
    '        }' \
    '    }' \
    '    location = /__compression_smoke {' \
    '        default_type text/plain;' \
    '        return 200 "simple_cdn compression module smoke response simple_cdn compression module smoke response simple_cdn compression module smoke response\n";' \
    '    }' \
    '}' \
    >/opt/cdn-edge/config/nginx/cdn-platform.conf

  local nginx=/opt/cdn-edge/nginx/sbin/nginx
  local dependencies
  dependencies=$(ldd "$nginx")
  printf '%s\n' "$dependencies"
  if grep -Fq 'not found' <<<"$dependencies"; then
    echo "Nginx has unresolved runtime dependencies" >&2
    return 1
  fi
  grep -Fq \
    'libluajit-5.1.so.2 => /opt/cdn-edge/nginx/lib/libluajit-5.1.so.2' \
    <<<"$dependencies"
  local version_output
  version_output=$("$nginx" -V 2>&1)
  grep -Fq 'nginx version: nginx/1.30.4' <<<"$version_output"
  grep -Fq -- '--add-module=/build/ngx_brotli-' <<<"$version_output"
  grep -Fq -- '--add-module=/build/zstd-nginx-module-' <<<"$version_output"
  "$nginx" -t

  trap '/opt/cdn-edge/nginx/sbin/nginx -s quit >/dev/null 2>&1 || true' EXIT
  "$nginx"
  test "$(curl --fail --silent --show-error \
    http://127.0.0.1:18080/__artifact_smoke)" = 'managed-nginx-ok'
  for encoding in gzip br zstd; do
    test "$(curl --fail --silent --show-error \
      --header "Accept-Encoding: $encoding" \
      --dump-header - --output /dev/null \
      http://127.0.0.1:18080/__compression_smoke \
      | tr -d '\r' \
      | awk 'tolower($1) == "content-encoding:" { print tolower($2) }')" = "$encoding"
  done
  "$nginx" -s quit
  trap - EXIT
}

main() {
  if [[ "${1:-}" == "--container" ]]; then
    [[ $# -eq 2 ]] || {
      echo "usage: $0 --container ARTIFACT" >&2
      return 2
    }
    container_smoke_test "$2"
    return
  fi

  local artifact="${1:-$DEFAULT_ARTIFACT}"
  [[ -f "$artifact" ]] || {
    echo "Nginx artifact not found: $artifact" >&2
    return 2
  }
  command -v docker >/dev/null 2>&1 || {
    echo "docker is required to test the Nginx artifact" >&2
    return 2
  }

  local artifact_dir artifact_name image script_path
  artifact_dir=$(cd -- "$(dirname -- "$artifact")" && pwd)
  artifact_name=$(basename -- "$artifact")
  script_path=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)/$(basename -- "${BASH_SOURCE[0]}")

  for image in "${TEST_IMAGES[@]}"; do
    echo "Testing $artifact on $image"
    docker run --rm \
      --mount "type=bind,src=$artifact_dir,dst=/artifact,readonly" \
      --mount "type=bind,src=$script_path,dst=/test-nginx-artifact.sh,readonly" \
      "$image" \
      bash /test-nginx-artifact.sh --container "/artifact/$artifact_name"
  done
}

main "$@"
