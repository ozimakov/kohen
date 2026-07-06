#!/usr/bin/env bash
# Verify SPEC requirement IDs cited in PLAN.md exist in SPEC.md (PLAN "How to use").
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PLAN="${ROOT}/PLAN.md"
SPEC="${ROOT}/SPEC.md"

if [[ ! -f "$PLAN" || ! -f "$SPEC" ]]; then
  echo "PLAN.md and SPEC.md are required" >&2
  exit 1
fi

missing=0
declare -A seen=()

check_id() {
  local id="$1"
  [[ -z "$id" ]] && return
  [[ -n "${seen[$id]:-}" ]] && return
  seen["$id"]=1

  if [[ "$id" == §* ]]; then
    local num="${id#§}"
    if ! grep -qE "(§${num}[^0-9]|^#+ .*${num})" "$SPEC"; then
      echo "missing SPEC section: $id" >&2
      missing=$((missing + 1))
    fi
    return
  fi

  if ! grep -qF "$id" "$SPEC"; then
    echo "missing SPEC ID: $id" >&2
    missing=$((missing + 1))
  fi
}

# Collect tokens from "SPEC refs:" lines only.
while IFS= read -r line; do
  body="${line#*SPEC refs:}"
  # Normalize en-dash ranges: R7.1–R7.2 -> R7.1 R7.2
  body="${body//–/ }"
  body="${body//—/ }"
  # Extract requirement-like tokens.
  while IFS= read -r tok; do
    tok="${tok%.}"
    tok="${tok%,}"
    tok="${tok%;}"
    tok="${tok%)}"
    tok="${tok#(}"
    check_id "$tok"
  done < <(echo "$body" | grep -oE \
    '§[0-9]+(\.[0-9]+)?|R-[A-Z]+|R[A-Z0-9]+(\.[0-9]+)?|NFR[0-9]+|TM[0-9]+|UC[0-9]+|A[0-9]+|T[0-9]+|T-[A-Z]+' || true)
done < <(grep -E '^\s*- \*\*SPEC refs:' "$PLAN")

if [[ "$missing" -gt 0 ]]; then
  echo "$missing SPEC reference(s) could not be resolved" >&2
  exit 1
fi

echo "All PLAN.md SPEC references resolve in SPEC.md"
