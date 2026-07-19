# Kohen

Kohen is a Kubernetes operator for one pattern: **an application that consumes
domain-specific configuration from a dedicated git repository**.

Point a `ConfigSync` at a git repo path and a workload. Kohen renders that path
into a `ConfigMap`, mounts it, and rolls the workload when the config version
changes — version-matched across the fleet.

Kohen is **not** a GitOps replacement. Use Argo CD / Flux to deploy **what**
runs; use Kohen to keep a running workload's **config** in sync with a dedicated
config repo. See **[What is Kohen](./docs/intro.md)** for the pattern, a
diagram, and when (not) to use it.

```text
  config repo                         cluster
  (domain / env config)               ┌─────────────────────────────────┐
                                      │                                 │
  services/checkout/prod/  ──fetch──▶ │  Kohen                          │
    app.yaml                          │    │                            │
    feature.toml                      │    ├─▶ ConfigMap (rendered)     │
                                      │    ├─▶ mount into Deployment    │
                                      │    └─▶ stamp + rolling update   │
                                      │                                 │
  deploy repo / GitOps ──deploy──▶    │  Deployment (checkout)          │
                                      └─────────────────────────────────┘
```

### When to use Kohen — and when not

| Scenario | Use Kohen? | Notes |
| --- | --- | --- |
| Dedicated config repo drives a workload's `ConfigMap` + secret wiring + rollouts | **Yes** | Core use case |
| GitOps deploys the app; config lives in a **separate** config repo | **Yes** | Apply [GitOps ignore rules](#gitops-coexistence) |
| GitOps already renders the app **and** its `ConfigMap` from the same repo | No | A second reconciler adds no value |
| Config exceeds `ConfigMap` size (~1 MiB) or is a large file tree | No | Prefer a `git-sync`-to-volume pattern |
| You only need secrets from an external store | No | Use External Secrets Operator directly |
| You hand-author a `ConfigMap` and only want restart-on-change | No | Reloader (or similar) is enough |
| You want product feature toggles / experiments in code | No | Use a feature-flag platform |

## Documentation

- **[What is Kohen](./docs/intro.md)** — positioning, pattern, when to use
- [Install](./docs/install.md) — Helm and plain manifests, both RBAC scopes
- [Getting Started & GitOps](./docs/getting-started-and-gitops.md) — Day-1 walkthrough on `kind`
- [Concepts](./docs/concepts.md) — architecture, reconcile flow, consistency
- [Secrets (ESO + native)](./docs/secrets.md)
- [Operations](./docs/operations.md) · [Troubleshooting](./docs/troubleshooting.md)
- [Security](./docs/security.md) · [Upgrade & uninstall](./docs/upgrade-uninstall.md)
- [Specification](./SPEC.md) — behavioral contract and acceptance criteria
- [Project site](https://ozimakov.github.io/kohen/)

> **Status:** **v1.0** — acceptance suite A1–A12 on `kind` (see
> [`.github/workflows/u3.yml`](./.github/workflows/u3.yml)). API group remains
> `kohen.dev/v1alpha1` ([upgrade notes](./docs/upgrade-uninstall.md)).
> `spec.secretRefs` supports `externalSecret` and `nativeSecret`.

---

## Getting started

Minimal config, maximum value out of the box: with three fields Kohen renders
your config, wires it into the workload, and gives you version-matched rollouts.

### Prerequisites

- A Kubernetes cluster (v1.28+) and `kubectl`.
- `helm` v3.13+.
- A git repository containing your config files under some path, reachable from
  the cluster.
- The target workload (a `Deployment` or `StatefulSet`) already running in the
  namespace where you'll create the `ConfigSync`.

### 1. Install Kohen (Helm)

```bash
# From a published release:
helm install kohen oci://ghcr.io/ozimakov/kohen/charts/kohen \
  --version 1.0.0 \
  --namespace kohen-system --create-namespace \
  --wait

# Or from this checkout:
helm install kohen deploy/helm/kohen \
  --namespace kohen-system --create-namespace \
  --wait
```

### 2. Create a `ConfigSync` (minimal)

Prerequisite for this example: a `Deployment` named `checkout` in namespace
`checkout`.

```yaml
apiVersion: kohen.dev/v1alpha1
kind: ConfigSync
metadata:
  name: checkout-prod
  namespace: checkout
spec:
  source:
    url: https://github.com/acme/platform-config.git
    ref: main
  path: services/checkout/prod
  workloadRef:
    kind: Deployment
    name: checkout
```

```bash
kubectl apply -f checkout-prod.yaml
```

### 3. What you get out of the box

With only the fields above, Kohen applies these defaults and does the rest
automatically:

| Concern | Default (out of the box) |
| --- | --- |
| ConfigMap name | `<workloadRef.name>-config` → `checkout-config` |
| Target container | the **first** container in the pod spec |
| Mount path | `/etc/kohen/config` |
| Rollout mode | `auto` (config change ⇒ exactly one rolling update) |
| Sync interval | `30s` polling (plus a force-sync annotation) |
| Version stamp | `kohen.dev/config-sha` on the pod template |

Concretely, Kohen: (1) renders `services/checkout/prod@main` into the
`checkout-config` `ConfigMap`; (2) merges a volume + mount at `/etc/kohen/config`
into the first container via Server-Side Apply (Kohen-owned fields only);
(3) stamps the config version on the pod template; and (4) on every future
change, updates the `ConfigMap` and triggers exactly one rolling update.

### 4. Verify

```bash
kubectl -n checkout get configsync checkout-prod   # READY, CONFIG VERSION
kubectl -n checkout get configmap checkout-config
kubectl -n checkout get deploy checkout \
  -o jsonpath='{.spec.template.metadata.annotations.kohen\.dev/config-sha}'
```

For the full walkthrough — including private-repo auth, rollback, outage
behavior, troubleshooting, and GitOps coexistence — follow the
[runbook](./docs/getting-started-and-gitops.md).

---

## Advanced configuration reference

Every feature currently shipped.

### `ConfigSync` spec

| Field | Type | Default | Description |
| --- | --- | --- | --- |
| `source.url` | string (required) | — | Repository URL over HTTPS or SSH. |
| `source.ref` | string | `main` | Branch, tag, or commit SHA. Branch = moving; tag/commit = immutable pin (use to roll back). |
| `source.authSecretRef.name` | string | — | Name of a git-credential `Secret` in the same namespace (see [authentication](#private-repository-authentication)). |
| `path` | string (required) | — | Repository-relative path whose files are rendered. Must not start with `/` or contain `..`. |
| `workloadRef.kind` | `Deployment` \| `StatefulSet` | — | Target workload kind. |
| `workloadRef.name` | string (required) | — | Target workload name (same namespace). |
| `configMap.name` | string | `<workloadRef.name>-config` | Name of the rendered `ConfigMap`. |
| `wiring.container` | string | first container | Container to wire the volume/mount into. |
| `wiring.mountPath` | string | `/etc/kohen/config` | Mount path for the config volume. Never uses `subPath` (so live updates work). |
| `rollout` | `auto` \| `none` | `auto` | See [rollout modes](#rollout-modes). |
| `sync.interval` | duration | `30s` | Polling interval between reconciles. |
| `secretRefs[]` | list (max 32) | — | Secrets the config references, surfaced as files or env vars. See [secret references](#secret-references). |
| `secretRefs[].name` | DNS-1123 ≤50 | — | Stable reference name (status/events key). |
| `secretRefs[].surface.rolloutOnRotate` | bool | `true` | When `surface.as=env`, whether secret rotation advances the config version and rolls the workload. Ignored for `as: file` (kubelet delivers in place). |

The rendered file tree maps to `ConfigMap` keys; `/` in a nested file name maps
to `__` in the key. Rendering fails closed if the result exceeds the ~1 MiB
`ConfigMap` limit (`Rendered=False/Oversize`).

### Secret references

`spec.secretRefs[]` lets a workload consume secrets alongside its config. Two
backends are supported: `externalSecret` (an External Secrets Operator
`ExternalSecret`, applied from git and awaited) and `nativeSecret` (a
pre-existing `Secret`). Each is surfaced as a `file` (mounted volume, live
in-place updates) or `env` (a `valueFrom.secretKeyRef` entry, rotation rolls the
workload unless `rolloutOnRotate: false`). Kohen never reads the secret value;
the readiness policy fails closed on first resolution and fails safe (last-good)
on a transient outage. Rotation is detected on the sync poll interval (≤
`sync.interval`). See the full [secret integration guide](./docs/secrets.md) for
schema, rotation, guard rails, and the Vault-via-ESO decision tree.

```yaml
spec:
  secretRefs:
    - name: db                # native Secret, as an env var
      backend: nativeSecret
      nativeSecret: { name: checkout-db }
      surface:
        as: env
        envVar: DB_PASSWORD
        key: password
        rolloutOnRotate: true   # default; set false to skip rollout on env rotation
    - name: api               # ESO ExternalSecret (committed to git), as a file
      backend: externalSecret
      externalSecret: { name: checkout-api }
      surface: { as: file, mountPath: /etc/checkout/api }
```

### Rollout modes

| Mode | Behavior on a config change |
| --- | --- |
| `auto` (default) | Updates the `ConfigMap`, stamps `kohen.dev/config-sha` on the **pod template**, and triggers **exactly one** rolling update. A no-op reconcile causes no rollout. Requires a rolling strategy — `OnDelete` StatefulSets / `Recreate` Deployments surface `UnsupportedStrategy`. |
| `none` | Updates the `ConfigMap` (mounted files update in place) and records the version on the **workload object** annotation, with **no rollout**. Use for apps that reload config without restarting. |

### Private repository authentication

Create a `Secret` in the **same namespace** as the `ConfigSync`, labeled
`kohen.dev/git-credential=true` (enforced at reconcile time), and reference it
from `spec.source.authSecretRef`. Recognized keys:

| Key | Purpose |
| --- | --- |
| `username` | HTTPS username. |
| `password` | HTTPS password. |
| `token` | HTTPS token (used as the password when `password` is unset). |
| `ssh-privatekey` | PEM-encoded SSH private key (use an `ssh://…` / `git@…` URL). |
| `ssh-passphrase` | Passphrase for the SSH key, if any. |
| `known_hosts` | SSH `known_hosts` for host-key verification. |
| `insecure-skip-tls-verify` | `"true"` to skip TLS verification — only honored when the operator is installed with `allowInsecureGitTLS=true`. |
| `insecure-ignore-host-key` | `"true"` to skip SSH host-key checks — same gating. |

Credentials are never written to logs, events, or status; on failure the
`ConfigSync` goes `Degraded` with a redacted `AuthFailed` event.

### Operator / Helm configuration

Install-time values (see [`deploy/helm/kohen/values.yaml`](./deploy/helm/kohen/values.yaml)):

| Value | Default | Purpose |
| --- | --- | --- |
| `scope` | `cluster` | `cluster` (watch all namespaces; installs a `ClusterRole`) or `namespaced` (watch the release namespace only; installs a `Role`). |
| `image.repository` / `image.tag` | `ghcr.io/ozimakov/kohen` / chart appVersion | Operator image. |
| `replicaCount` / `leaderElection.enabled` | `1` / `true` | HA-safe defaults. |
| `operatorConfig.sourceAllowList` | `[]` (all allowed) | Restrict git hosts / URL prefixes usable as sources (`R-AUTH.3`). Recommended in production. |
| `operatorConfig.secretStoreAllowList` | `[]` (no restriction) | Names of secret stores an applied `ExternalSecret` may reference (`R-AUTH.4`). Recommended in production. See the [secret guide](./docs/secrets.md#7-guard-rails-operatorplatform-team). |
| `operatorConfig.maxDegradedDuration` | `15m` | How long to serve last-good before surfacing `MaxDegradedExceeded`. |
| `operatorConfig.allowInsecureGitTLS` | `false` | Permit per-source `insecure-skip-tls-verify` / `insecure-ignore-host-key`. Keep `false` in production. |
| `metrics.service.enabled` / `.port` | `true` / `8080` | Prometheus metrics endpoint. |
| `resources`, `podSecurityContext`, `securityContext` | hardened | Non-root, read-only rootfs, dropped caps, seccomp `RuntimeDefault`. |

Regardless of the allow-list, Kohen always blocks source hosts that resolve to
loopback, link-local (incl. the `169.254.169.254` metadata endpoint),
unspecified, or multicast addresses, and re-screens every HTTP redirect hop
(see [security hardening](./docs/security.md)).

### Annotations

| Annotation | On | Purpose |
| --- | --- | --- |
| `kohen.dev/sync-now` | `ConfigSync` | Set to any value to force an immediate reconcile; Kohen clears it. |
| `kohen.dev/config-sha` | pod template (auto) or workload object (none) | The stamped config version. |
| `kohen.dev/git-credential` | `Secret` (label) | Must be `true` for a Secret to be usable as git credentials. |

### Status & conditions

`status` exposes `observedGeneration`, `sourceCommit`, `configVersion`,
`workloadVersion`, `rolloutInProgress`, per-reference `secretRefs` (including
sticky `established`), and per-step conditions: `Fetched`, `Rendered`,
`ManifestsApplied`, `SecretsReady`, `WorkloadWired`, `RolloutComplete`, and the
overall `Ready`. Short name: `kubectl get cs`. Common failure reasons and the
first action to take:

| Reason | Condition | First action |
| --- | --- | --- |
| `FetchFailed` | `Fetched=False` | Check URL/ref/network; Kohen serves last-good and auto-recovers. |
| `AuthFailed` | `Fetched=False` | Verify the credential Secret exists, is labeled, and has the right keys. |
| `SourceNotAllowed` | `Fetched=False` | Fix `operatorConfig.sourceAllowList` or the URL (or a blocked-IP source). |
| `PathNotFound` | `Fetched=False` | Correct `spec.path` for the ref. |
| `Oversize` | `Rendered=False` | Reduce/split config (~1 MiB `ConfigMap` limit). |
| `TreeSafetyViolation` / `InvalidKey` / `KeyConflict` | `Rendered=False` | Remove unsafe files / fix file names/collisions. |
| `StoreNotAllowed` / `ManifestKindNotAllowed` / `ManifestNamespaceViolation` | `ManifestsApplied=False` | Fix the committed `ExternalSecret` or the store allow-list (see the [secret guide](./docs/secrets.md#9-troubleshooting)). |
| `AwaitingFirstResolution` | `SecretsReady=False` | A never-wired secret isn't resolvable yet; no rollout until it resolves. Create the `Secret` / make the `ExternalSecret` Ready. |
| `DegradedServingLastGood` | `SecretsReady=False` | An established secret went transiently not-ready; workload keeps running last-good and auto-recovers. |
| `WorkloadNotFound` | `WorkloadWired=False` | Create the target workload or fix `workloadRef`. |
| `UnsupportedStrategy` | `WorkloadWired=False` | Use a rolling strategy, or `rollout: none`. |
| `ApplyConflict` | `WorkloadWired=False` | Apply the [GitOps ignore rules](#gitops-coexistence). |
| `SingletonViolation` | `WorkloadWired=False` | Only one `ConfigSync` may target a workload; remove the duplicate. |
| `MaxDegradedExceeded` | `SecretsReady=False` | Degraded past `maxDegradedDuration`; investigate the underlying `SecretsReady` failure. |

### GitOps coexistence

Kohen merges only its owned fields (the `kohen-config` volume/mount and the
`kohen.dev/config-sha` annotation) via Server-Side Apply. Coexistence with Argo
CD / Flux is guaranteed when the other controller uses SSA **and** applies the
documented ignore rules; client-side / whole-object appliers will strip Kohen's
fields. Copy-paste Argo CD `ignoreDifferences` and Flux `Merge` SSA snippets are
in the [runbook](./docs/getting-started-and-gitops.md#gitops-coexistence).
