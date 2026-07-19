# Install Kohen

Kohen ships as a Helm chart and as plain Kubernetes manifests generated from
that chart. Both paths install the same operator.

**Requirements:** Kubernetes 1.28+ (N-2 per SPEC T4; CI gates latest + one older
minor), Helm 3.13+ (for Helm install).

## Helm (recommended)

### Cluster scope (default)

Watches `ConfigSync` resources in **all** namespaces.

```bash
# From a release (recommended):
helm install kohen oci://ghcr.io/ozimakov/kohen/charts/kohen \
  --version 1.0.0 \
  --namespace kohen-system --create-namespace \
  --wait

# Or from a git checkout:
helm install kohen deploy/helm/kohen \
  --namespace kohen-system --create-namespace \
  --wait
```

### Namespace scope

Watches only the release namespace. Recommended to bound blast radius (TM8).

```bash
helm install kohen deploy/helm/kohen \
  --namespace team-a --create-namespace \
  --set scope=namespaced \
  --wait
```

Create `ConfigSync` resources in the **same** namespace as the operator when
using namespaced scope.

### Production hardening values

```yaml
scope: namespaced   # or cluster, with tight RBAC on who can create ConfigSyncs
operatorConfig:
  sourceAllowList:
    - https://github.com/acme/
  secretStoreAllowList:
    - vault-prod
  maxDegradedDuration: 15m
  allowInsecureGitTLS: false
```

See [`deploy/helm/kohen/values.yaml`](../deploy/helm/kohen/values.yaml) for all
options and [`docs/security.md`](./security.md) for guidance.

## Plain manifests

Generate a static bundle from the chart (includes CRDs):

```bash
make manifests-bundle
# Output: deploy/manifests/kohen.yaml
kubectl apply -f deploy/manifests/kohen.yaml
```

For namespaced scope, render with:

```bash
helm template kohen deploy/helm/kohen --include-crds \
  --set scope=namespaced \
  --namespace kohen-system > deploy/manifests/kohen-namespaced.yaml
```

Verify the operator is ready:

```bash
kubectl -n kohen-system rollout status deploy/kohen
kubectl -n kohen-system get pods
```

## CRDs

CRDs are installed automatically by Helm (`crds/` in the chart) or included in
the plain manifest bundle. They are cluster-scoped.

## Next steps

Follow the [Getting Started runbook](./getting-started-and-gitops.md) to create
your first `ConfigSync`.
