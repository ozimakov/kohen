#!/usr/bin/env bash
# Validate a SemVer release tag / version string (NFR10).
# Usage: validate-release-tag.sh <version-or-tag>
# Accepts "v1.0.0", "1.0.0", "1.0.0-rc.1", "1.0.0+build.1".
set -euo pipefail

raw="${1:-}"
if [[ -z "${raw}" ]]; then
  echo "usage: $0 <version-or-tag>" >&2
  exit 2
fi

ver="${raw#v}"
# SemVer 2.0 core + optional pre-release + optional build metadata.
semver_re='^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$'
if [[ ! "${ver}" =~ ${semver_re} ]]; then
  echo "error: ${raw@Q} is not a valid SemVer (got version=${ver@Q})" >&2
  exit 1
fi

prerelease=false
if [[ "${ver}" == *-* ]]; then
  # Build metadata alone uses '+'; a '-' indicates pre-release.
  core_and_pre="${ver%%+*}"
  if [[ "${core_and_pre}" == *-* ]]; then
    prerelease=true
  fi
fi

echo "version=${ver}"
echo "prerelease=${prerelease}"
