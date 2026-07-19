#!/usr/bin/env bash
# Lightweight self-test for validate-release-tag.sh (run via make release-validate-test).
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
V="${ROOT}/scripts/validate-release-tag.sh"

check() {
  local input="$1" want_ver="$2" want_pre="$3"
  eval "$("$V" "$input")"
  [[ "${version}" == "${want_ver}" ]] || { echo "version mismatch for ${input}: got ${version}"; exit 1; }
  [[ "${prerelease}" == "${want_pre}" ]] || { echo "prerelease mismatch for ${input}: got ${prerelease}"; exit 1; }
}

check v1.2.3 1.2.3 false
check 1.0.0-rc.1 1.0.0-rc.1 true
check 1.0.0+build.5 1.0.0+build.5 false
check 1.0.0-rc.1+meta 1.0.0-rc.1+meta true

! "$V" nope >/dev/null 2>&1
! "$V" 01.0.0 >/dev/null 2>&1
! "$V" v >/dev/null 2>&1

echo "validate-release-tag_test.sh: ok"
