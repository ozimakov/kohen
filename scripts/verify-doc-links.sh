#!/usr/bin/env bash
# Check internal markdown links in docs/ and README.md resolve to files.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
missing=0

check_file() {
  local file="$1"
  local dir
  dir="$(dirname "$file")"
  while IFS= read -r link; do
    # Strip anchor
    local target="${link%%#*}"
    [[ -z "$target" || "$target" == http* ]] && continue
    local resolved="${dir}/${target}"
    if [[ ! -e "$resolved" ]]; then
      echo "$file: broken link -> $target" >&2
      missing=$((missing + 1))
    fi
  done < <(grep -oE '\]\([^)]+\)' "$file" | sed 's/](\(.*\))/\1/' || true)
}

for f in "$ROOT"/README.md "$ROOT"/docs/*.md "$ROOT"/docs/adr/*.md; do
  [[ -f "$f" ]] || continue
  check_file "$f"
done

if [[ "$missing" -gt 0 ]]; then
  echo "$missing broken doc link(s)" >&2
  exit 1
fi

echo "Doc links OK"
