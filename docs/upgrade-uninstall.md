# Upgrade & uninstall

## Versioning (NFR10)

Kohen follows [Semantic Versioning](https://semver.org/):

- **MAJOR** — breaking API or behavior changes (new API version with conversion)
- **MINOR** — backward-compatible features
- **PATCH** — backward-compatible fixes

The Helm chart `version` tracks chart packaging changes; `appVersion` tracks the
operator image.

### Product SemVer vs API version

- **Product** releases follow SemVer (`v1.0.0`, chart `version` / `appVersion`).
- The served CRD API in v1.0.x is **`kohen.dev/v1alpha1`**. That is intentional:
  the alpha API version signals that a future `v1beta1`/`v1` may introduce
  conversion. Within `1.0.x`, compatible field additions are preferred over
  breaks.

### CRD upgrade policy

- CRD changes ship in the Helm chart `crds/` directory and the plain manifest
  bundles (`kohen.yaml` cluster-scope, `kohen-namespaced.yaml` namespaced).
- **Compatible changes** (new optional fields, additional validation) apply with
  `helm upgrade` or `kubectl apply`.
- **Breaking changes** require a new API version (`v1beta1`, etc.) with a
  conversion webhook — not expected in v1.0.x.
- Always review the [CHANGELOG](../CHANGELOG.md) / release notes before upgrading
  across minor versions.

## Helm upgrade

Upgrades roll the operator Deployment when the image or config changes. Running
`ConfigSync` resources continue to reconcile.

```bash
helm upgrade kohen deploy/helm/kohen \
  --namespace kohen-system \
  --reuse-values \
  --wait
```

To pin a specific image:

```bash
helm upgrade kohen deploy/helm/kohen \
  --namespace kohen-system \
  --reuse-values \
  --set image.tag=1.0.0 \
  --wait
```

**Verified behavior (A12):** after upgrade, existing syncs converge on new git
commits without manual intervention. See `TestU3OperatorUpgrade` in
[`test/e2e/lifecycle_test.go`](../test/e2e/lifecycle_test.go).

## Plain manifest upgrade

Re-apply the rendered bundle:

```bash
make manifests-bundle
kubectl apply -f deploy/manifests/kohen.yaml
kubectl -n kohen-system rollout status deploy/kohen
```

## Uninstall

```bash
helm uninstall kohen --namespace kohen-system
```

**Verified behavior (A12):**

- The operator Deployment, RBAC, and operator ConfigMap are removed.
- **User workloads keep running** with their current wiring, `ConfigMap`s, and
  version stamps.
- `ConfigSync` CRs remain in the cluster but are no longer reconciled.
- Kohen-owned `ConfigMap`s and applied `ExternalSecret`s are **not** deleted
  automatically on operator uninstall — only deleting a `ConfigSync` triggers
  prune/unwire (R11.3).

To fully remove Kohen's effect on a workload, delete the `ConfigSync` **before**
uninstalling the operator:

```bash
kubectl delete configsync <name> -n <namespace>
```

## Releases

Tagged releases (`v*`) publish via
[`.github/workflows/release.yml`](../.github/workflows/release.yml):

- Multi-arch operator image (`linux/amd64`, `linux/arm64`) to GHCR, with SBOM
- Cosign keyless signature (hard-fail — unsigned images do not ship)
- Helm chart pushed to `oci://ghcr.io/<owner>/charts`
- GitHub Release artifacts: chart `.tgz`, `kohen.yaml`, `kohen-namespaced.yaml`

The project site is published from [`site/`](../site/) to the `gh-pages` branch
via [`.github/workflows/pages.yml`](../.github/workflows/pages.yml).

**One-time setup:** Settings → Pages → Source **Deploy from a branch** →
branch **`gh-pages`** / root. After that, every `main` change under `site/`
updates https://ozimakov.github.io/kohen/.
Release artifacts include an SPDX SBOM and cosign signatures when signing
credentials are configured.
