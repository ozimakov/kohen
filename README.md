# KOHEN

`Kohen` is a Kubernetes-native configuration management tool that lets
applications consume configuration from a dedicated `git` repository and
consistently roll out after any change.

`Kohen` is **not** an alternative to GitOps solutions. It adds capabilities for
applications that prefer running against a dedicated configuration repository
covering multiple environments, and is engineered to **coexist** with Argo CD /
Flux rather than replace them (see [GitOps coexistence](#gitops-coexistence) and
the "when to use Kohen — and when not" decision table in [`SPEC.md`](./SPEC.md)
§2.4).

In one `ConfigSync` you point at a git repo + path and a workload; Kohen renders
the path into a `ConfigMap`, mounts it into the workload, and rolls the workload
whenever the config changes — version-matched across the fleet.

## Documentation

- [`SPEC.md`](./SPEC.md) — full technical/non-technical requirements,
  architecture, consistency model, threat model, and acceptance criteria.
- [`PLAN.md`](./PLAN.md) — the implementation sequence toward **v1.0**.
- [Concepts](./docs/concepts.md) — architecture, reconcile flow, consistency model.
- [Install](./docs/install.md) — Helm and plain manifests, both RBAC scopes.
- [Getting Started & GitOps runbook](./docs/getting-started-and-gitops.md)
  — verified Day-1 walkthrough (install → sync → rollout → auth → rollback →
  GitOps coexistence), exercised in CI on `kind`.
- [Secret integration guide (ESO + native)](./docs/secrets.md) — reference
  secrets safely; readiness, rotation, guard rails, Vault-via-ESO tree.
- [Operations](./docs/operations.md) — kubectl status, force-sync, rollback.
- [Troubleshooting](./docs/troubleshooting.md) — symptom → condition → action.
- [Security hardening](./docs/security.md) — threat model, RBAC, allow-lists.
- [Upgrade & uninstall](./docs/upgrade-uninstall.md) — SemVer, CRD policy, A12.

> **Status:** Phases 0–2 and **Phase 3 (ship readiness)** are implemented.
> The [U3 acceptance gate](./.github/workflows/u3.yml) automates criteria
> **A1–A12** on `kind` (two Kubernetes minors, Helm + plain manifests).
> `spec.secretRefs` supports `externalSecret` and `nativeSecret` backends.

---

## Getting started

Minimal config, maximum value out of the box: with three fields Kohen renders
your config, wires it into the workload, and gives you version-matched rollouts.

### Prerequisites

- A Kubernetes cluster (v1.29+) and `kubectl`.
- `helm` v3.13+.
- A git repository containing your config files under some path, reachable from
  the cluster.
- The target workload (a `Deployment` or `StatefulSet`) already running in the
  namespace where you'll create the `ConfigSync`.

### 1. Install Kohen (Helm)

```bash
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
| `secretRefs[]` | list | — | Secrets the config references, surfaced as files or env vars. See [secret references](#secret-references). |

The rendered file tree maps to `ConfigMap` keys; `/` in a nested file name maps
to `__` in the key. Rendering fails closed if the result exceeds the ~1 MiB
`ConfigMap` limit (`Rendered=False/Oversize`).

### Secret references

`spec.secretRefs[]` lets a workload consume secrets alongside its config. Two
backends are supported: `externalSecret` (an External Secrets Operator
`ExternalSecret`, applied from git and awaited) and `nativeSecret` (a
pre-existing `Secret`). Each is surfaced as a `file` (mounted volume, live
in-place updates) or `env` (a `valueFrom.secretKeyRef` entry, rotation rolls the
workload). Kohen never reads the secret value; the readiness policy fails closed
on first resolution and fails safe (last-good) on a transient outage. See the
full [secret integration guide](./docs/secrets.md) for schema, rotation, guard
rails, and the Vault-via-ESO decision tree.

```yaml
spec:
  secretRefs:
    - name: db                # native Secret, as an env var
      backend: nativeSecret
      nativeSecret: { name: checkout-db }
      surface: { as: env, envVar: DB_PASSWORD, key: password }
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
(`SPEC.md` R-AUTH.7).

### Annotations

| Annotation | On | Purpose |
| --- | --- | --- |
| `kohen.dev/sync-now` | `ConfigSync` | Set to any value to force an immediate reconcile; Kohen clears it. |
| `kohen.dev/config-sha` | pod template (auto) or workload object (none) | The stamped config version. |
| `kohen.dev/git-credential` | `Secret` (label) | Must be `true` for a Secret to be usable as git credentials. |

### Status & conditions

`status` exposes `sourceCommit`, `configVersion`, `workloadVersion`,
`rolloutInProgress`, per-reference `secretRefs`, and per-step conditions:
`Fetched`, `Rendered`, `ManifestsApplied`, `SecretsReady`, `WorkloadWired`,
`RolloutComplete`, and the overall `Ready`. Common failure reasons and the first
action to take:

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

---

## When to use Kohen — and when not

Use GitOps to deploy **what** runs; use Kohen to keep a running workload's
**config** in sync with a dedicated config repo and roll it out consistently.
See the decision table in [`SPEC.md`](./SPEC.md) §2.4 before adopting.
