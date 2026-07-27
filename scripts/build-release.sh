#!/usr/bin/env bash
set -euo pipefail

OUTPUT_DIR="${1:-dist}"
SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
BUILD_VERSION=$("$SCRIPT_DIR/resolve-version.sh")
mkdir -p "$OUTPUT_DIR"

if ! command -v npm >/dev/null 2>&1; then
  echo "npm is required to build the embedded management UI" >&2
  exit 2
fi

npm --prefix frontend ci
npm --prefix frontend run build

build() {
  local package="$1"
  local output="$2"
  CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath \
    -ldflags="-s -w -X simple_cdn/internal/version.Version=$BUILD_VERSION" \
    -o "$OUTPUT_DIR/$output" "$package"
}

build ./cmd/control cdn-control-linux-amd64
build ./cmd/edge-agent cdn-edge-agent-linux-amd64

nginx_output=$(mktemp -d)
trap 'rm -rf "$nginx_output"' EXIT
docker build --target nginx-artifact --output "type=local,dest=$nginx_output" .
install -m 0644 "$nginx_output/cdn-nginx-linux-amd64.tar.gz" "$OUTPUT_DIR/cdn-nginx-linux-amd64.tar.gz"

if command -v sha256sum >/dev/null 2>&1; then
  (cd "$OUTPUT_DIR" && sha256sum cdn-control-linux-amd64 cdn-edge-agent-linux-amd64 cdn-nginx-linux-amd64.tar.gz >SHA256SUMS)
else
  (cd "$OUTPUT_DIR" && shasum -a 256 cdn-control-linux-amd64 cdn-edge-agent-linux-amd64 cdn-nginx-linux-amd64.tar.gz >SHA256SUMS)
fi

echo "Built Linux AMD64 assets for $BUILD_VERSION in $OUTPUT_DIR"
