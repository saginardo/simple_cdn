#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
repository_root=$(cd -- "$script_dir/.." && pwd)
ref_type="${GITHUB_REF_TYPE:-}"
ref_name="${GITHUB_REF_NAME:-}"
revision="${GITHUB_SHA:-}"
dirty_suffix=""

if [[ -z "$ref_type" ]] && git -C "$repository_root" rev-parse --is-inside-work-tree >/dev/null 2>&1; then
  if [[ -z "$(git -C "$repository_root" status --porcelain)" ]]; then
    ref_name=$(git -C "$repository_root" describe --tags --exact-match --match 'v*' HEAD 2>/dev/null || true)
    if [[ -n "$ref_name" ]]; then
      ref_type="tag"
    fi
  else
    dirty_suffix=".dirty"
  fi
  if [[ -z "$revision" ]]; then
    revision=$(git -C "$repository_root" rev-parse HEAD)
  fi
fi

if [[ "$ref_type" == "tag" ]]; then
  semver_pattern='^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$'
  if [[ ! "$ref_name" =~ $semver_pattern ]]; then
    echo "release tag must match vMAJOR.MINOR.PATCH, optionally with a prerelease suffix: $ref_name" >&2
    exit 2
  fi
  release_version=${ref_name#v}
  if [[ "$release_version" == *-* ]]; then
    IFS='.' read -r -a prerelease_identifiers <<<"${release_version#*-}"
    for identifier in "${prerelease_identifiers[@]}"; do
      if [[ "$identifier" =~ ^[0-9]+$ && "$identifier" == 0?* && "$identifier" != "0" ]]; then
        echo "numeric prerelease identifiers must not contain leading zeroes: $ref_name" >&2
        exit 2
      fi
    done
  fi
  printf '%s\n' "$release_version"
  exit 0
fi

if [[ -z "$revision" ]]; then
  printf 'dev\n'
  exit 0
fi
if [[ ! "$revision" =~ ^[0-9a-fA-F]{7,64}$ ]]; then
  echo "build revision must be a 7-64 character hexadecimal Git object ID" >&2
  exit 2
fi

revision=${revision,,}
printf '0.0.0-dev+%s%s\n' "${revision:0:12}" "$dirty_suffix"
