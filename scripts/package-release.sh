#!/usr/bin/env bash
# Package Helm chart + pinned manifest bundles + checksums for a release.
# Usage: package-release.sh <version> [out-dir]
#   VERSION=1.0.0 ./scripts/package-release.sh 1.0.0 dist
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
VERSION="${1:-}"
OUT="${2:-${ROOT}/dist}"

if [[ -z "${VERSION}" ]]; then
  echo "usage: $0 <version> [out-dir]" >&2
  exit 2
fi
VERSION="${VERSION#v}"

CHART_DIR="${ROOT}/deploy/helm/kohen"
IMAGE_REPO="${IMAGE_REPO:-ghcr.io/ozimakov/kohen}"

rm -rf "${OUT}"
mkdir -p "${OUT}/manifests"

# Bump chart metadata in a temp copy so the working tree stays clean for dry-runs.
STAGE="$(mktemp -d)"
trap 'rm -rf "${STAGE}"' EXIT
cp -a "${CHART_DIR}/." "${STAGE}/chart"
sed -i "s/^version:.*/version: ${VERSION}/" "${STAGE}/chart/Chart.yaml"
sed -i "s/^appVersion:.*/appVersion: \"${VERSION}\"/" "${STAGE}/chart/Chart.yaml"

helm package "${STAGE}/chart" -d "${OUT}"

# Pin the operator image tag in rendered bundles so plain-manifest installs
# pull the release under test, not a stale Chart.yaml default.
helm template kohen "${STAGE}/chart" --include-crds \
  --namespace kohen-system \
  --set "image.repository=${IMAGE_REPO}" \
  --set "image.tag=${VERSION}" \
  > "${OUT}/manifests/kohen.yaml"

helm template kohen "${STAGE}/chart" --include-crds \
  --namespace kohen-system \
  --set scope=namespaced \
  --set "image.repository=${IMAGE_REPO}" \
  --set "image.tag=${VERSION}" \
  > "${OUT}/manifests/kohen-namespaced.yaml"

# Checksums cover every artifact that ships on the GitHub Release.
(
  cd "${OUT}"
  find . -type f \( -name '*.tgz' -o -path './manifests/*' \) -print0 \
    | sort -z \
    | xargs -0 sha256sum \
    > SHA256SUMS
)

# Optional CHANGELOG excerpt for the GitHub Release body.
NOTES="${OUT}/release-notes.md"
{
  echo "## Kohen v${VERSION}"
  echo
  if [[ -f "${ROOT}/CHANGELOG.md" ]]; then
    # Extract the section for this version if present; otherwise a short pointer.
    awk -v ver="${VERSION}" '
      BEGIN { want="## [" ver "]" }
      $0 ~ "^## \\[" ver "\\]" { grab=1; next }
      grab && /^## \[/ { exit }
      grab { print }
    ' "${ROOT}/CHANGELOG.md" | sed '/./,$!d' > "${STAGE}/excerpt"
    if [[ -s "${STAGE}/excerpt" ]]; then
      cat "${STAGE}/excerpt"
    else
      echo "See [CHANGELOG.md](https://github.com/ozimakov/kohen/blob/main/CHANGELOG.md) for details."
    fi
  else
    echo "See the repository CHANGELOG for details."
  fi
  echo
  echo "### Install"
  echo
  echo '```bash'
  echo "helm install kohen oci://${IMAGE_REPO}/charts/kohen --version ${VERSION} \\"
  echo "  --namespace kohen-system --create-namespace --wait"
  echo '```'
} > "${NOTES}"

echo "packaged version=${VERSION} out=${OUT}"
ls -la "${OUT}" "${OUT}/manifests"
