# Upgrade & uninstall

## Versioning (NFR10)

Kohen follows [Semantic Versioning](https://semver.org/):

- **MAJOR** — breaking API or behavior changes (new API version with conversion)
- **MINOR** — backward-compatible features
- **PATCH** — backward-compatible fixes

The Helm chart `version` tracks chart packaging changes; `appVersion` tracks the
operator image.

### CRD upgrade policy

- CRD changes ship in the Helm chart `crds/` directory and the plain manifest
  bundle.
- **Compatible changes** (new optional fields, additional validation) apply with
  `helm upgrade` or `kubectl apply`.
- **Breaking changes** require a new API version (`v1beta1`, etc.) with a
  conversion webhook — not expected in v1.0.x.
- Always review the release notes before upgrading across minor versions.

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
  --set image.tag=0.1.0 \
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

Tagged releases publish container images and Helm charts via the
[`.github/workflows/release.yml`](../.github/workflows/release.yml) workflow.
Release artifacts include an SPDX SBOM and cosign signatures when signing
credentials are configured.
