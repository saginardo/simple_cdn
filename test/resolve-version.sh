#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
resolver="$script_dir/../scripts/resolve-version.sh"
revision=0123456789abcdef0123456789abcdef01234567

assert_version() {
  local expected="$1" ref_type="$2" ref_name="$3" sha="$4" actual
  actual=$(GITHUB_REF_TYPE="$ref_type" GITHUB_REF_NAME="$ref_name" GITHUB_SHA="$sha" "$resolver")
  if [[ "$actual" != "$expected" ]]; then
    echo "version = $actual, want $expected" >&2
    exit 1
  fi
}

assert_invalid_tag() {
  local tag="$1"
  if GITHUB_REF_TYPE=tag GITHUB_REF_NAME="$tag" GITHUB_SHA="$revision" "$resolver" >/dev/null 2>&1; then
    echo "invalid release tag was accepted: $tag" >&2
    exit 1
  fi
}

assert_version 1.2.3 tag v1.2.3 "$revision"
assert_version 1.2.3-rc.1 tag v1.2.3-rc.1 "$revision"
assert_version 0.0.0-dev+0123456789ab branch main "$revision"
assert_invalid_tag 1.2.3
assert_invalid_tag v1.2
assert_invalid_tag v01.2.3
assert_invalid_tag v1.2.3-01
assert_invalid_tag v1.2.3+build.5

temporary_root=$(mktemp -d "${TMPDIR:-/tmp}/simple-cdn-version-test.XXXXXX")
trap 'rm -rf -- "$temporary_root"' EXIT
mkdir -p "$temporary_root/scripts"
cp "$resolver" "$temporary_root/scripts/resolve-version.sh"
printf 'test\n' >"$temporary_root/tracked"
git -C "$temporary_root" init --quiet
git -C "$temporary_root" config user.name test
git -C "$temporary_root" config user.email test@example.com
git -C "$temporary_root" add tracked scripts/resolve-version.sh
git -C "$temporary_root" commit --quiet -m initial
local_revision=$(git -C "$temporary_root" rev-parse HEAD)
local_resolver="$temporary_root/scripts/resolve-version.sh"

assert_local_version() {
  local expected="$1" actual
  actual=$(GITHUB_REF_TYPE= GITHUB_REF_NAME= GITHUB_SHA= "$local_resolver")
  if [[ "$actual" != "$expected" ]]; then
    echo "local version = $actual, want $expected" >&2
    exit 1
  fi
}

assert_local_version "0.0.0-dev+${local_revision:0:12}"
git -C "$temporary_root" tag -a v2.3.4 -m "test release"
assert_local_version 2.3.4
printf 'dirty\n' >>"$temporary_root/tracked"
assert_local_version "0.0.0-dev+${local_revision:0:12}.dirty"

echo "Build version resolver tests passed"
