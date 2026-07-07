#!/usr/bin/env bash
# Retry docker build/pull commands when registry auth or pulls flake (Docker Hub 500s).
set -euo pipefail

if [[ $# -lt 1 ]]; then
  echo "usage: $0 <command> [args...]" >&2
  exit 2
fi

max_attempts="${DOCKER_RETRY_ATTEMPTS:-5}"
delay="${DOCKER_RETRY_INITIAL_DELAY_SEC:-8}"

attempt=1
while true; do
  if "$@"; then
    exit 0
  fi
  if (( attempt >= max_attempts )); then
    echo "Command failed after ${max_attempts} attempts: $*" >&2
    exit 1
  fi
  echo "Attempt ${attempt}/${max_attempts} failed; retrying in ${delay}s: $*" >&2
  sleep "$delay"
  attempt=$((attempt + 1))
  delay=$((delay * 2))
done
