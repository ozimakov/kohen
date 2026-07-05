# KOHEN — Specification

> Status: **Draft v0.6** · Owner: Kohen maintainers · Last updated: 2026-07-04
>
> This document is the single source of truth for **what** Kohen is, **why** it
> exists, and **which requirements** any implementation must satisfy. It is
> intentionally implementation-light: it describes behavior, contracts, and
> constraints rather than prescribing internal code structure.
>
> **Standing scope decisions.**
> - Kohen ships as a cluster **operator** only; a per-workload sidecar/env-var
>   mode is deferred (§19).
> - Secrets are handled by **integration/resolution, not production**: config
>   references a secret; Kohen assumes it exists (or reconciles until it does)
>   and resolves it into the pod using existing Kubernetes mechanics (§8).
> - Kohen **mutates the target workload by merging** into the existing definition
>   via Server-Side Apply, engineered to coexist with GitOps (§6.2).
> - Secret manifests committed to git (`ExternalSecret`) are **applied** by Kohen
>   (owned lifecycle); otherwise Kohen **awaits** an externally-managed secret
>   (§8.2). Readiness: **first-resolution fail-closed, update fail-safe** (R8.9).
>
> **v0.6 (multi-role review fixes).** Narrowed v1 secret backends to
> `externalSecret` + `nativeSecret` (Sealed Secrets = v1.1; Vault via ESO; CSI
> deferred). Single config surface (`ConfigSync` only; in-repo `kohen.yaml`
> deferred). Added: threat model (§3.3), authorization requirements (R-AUTH.\*),
> concrete defaults (§11.2), container targeting, conditions contract (§11.4),
> secret-version-token spec (R8.10), rotation/rollout split (env-only triggers),
> `envFrom` removed (atomic list, not SSA-mergeable), `OnDelete`/`subPath`
> caveats, SSRF/path-normalization guards, force-sync, git-auth secret schema,
> GitOps compatibility matrix. `kohenctl`, webhooks, signed commits, overlay,
> splitting, DaemonSet moved to post-1.0 (§19).

---

## 1. Overview

**Kohen** is a Kubernetes **operator** that keeps native cluster objects in sync
with a path in a dedicated `git` configuration repository. Given a repo, a
branch, and a path, Kohen continuously:

- renders non-secret configuration into a **`ConfigMap`**;
- **resolves** any secret the config references — assuming the backing secret
  already exists or will be created — and makes it available to the workload
  using **existing Kubernetes secret mechanics**; and
- records the resolved **config version** on the target workload and drives a
  **rolling update** when — and only when — it changes.

The application consumes the resulting `ConfigMap` and `Secret` exactly as it
would any hand-authored object. Kohen decouples config/secret delivery from CI
pipelines and app release cycles, sourcing them from one reviewed,
multi-environment git repository.

### 1.1 One-sentence definition

> Kohen is a Kubernetes operator that syncs a dedicated git repo's config path
> into a `ConfigMap`, resolves the secrets that config references into the
> workload via existing Kubernetes/External-Secrets mechanics, and rolls the
> workload out by matching the config version stamped on its metadata.

### 1.2 Minimum viable usage

Prerequisites: the Kohen operator is installed (Helm), and a `Deployment`
named `checkout` exists in namespace `checkout`.

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

With only these fields, Kohen (defaults per §11.2):

1. renders `services/checkout/prod@main` into ConfigMap **`checkout-config`**;
2. merges a volume + mount at **`/etc/kohen/config`** into the **first
   container** of the `checkout` Deployment (Server-Side Apply, Kohen-owned
   fields only);
3. stamps the config version as annotation **`kohen.dev/config-sha`** on the pod
   template; and
4. on every future change, updates the ConfigMap and triggers exactly one
   rolling update.

Verify:

```bash
kubectl get configsync checkout-prod        # READY, CONFIG VERSION columns
kubectl get configmap checkout-config
kubectl get deploy checkout -o jsonpath='{.spec.template.metadata.annotations.kohen\.dev/config-sha}'
```

Secrets (§8) and private-repo auth (§7.5) are additive on top of this minimum.

### 1.3 Why native objects (not a private volume)

- **Native, atomic delivery.** Kubelet delivers mounted `ConfigMap`/`Secret`
  updates atomically (`..data` symlink swap) — eventually consistent within the
  kubelet sync period. Kohen does not reinvent file delivery.
  - *Caveat (normative):* `subPath` mounts do **not** receive updates, and env
    vars never update in a running process; both are served by the rollout
    (§9). Kohen-managed mounts MUST NOT use `subPath`.
- **Fleet consistency.** A single `ConfigMap` is one source of truth, and the
  version stamp on the pod template makes every rolled pod carry the same
  version (§9).
- **Ecosystem compatibility.** Composes with External Secrets Operator and
  GitOps controllers rather than replacing them.
- **Known constraint, chosen deliberately.** A `ConfigMap` is limited to ~1 MiB
  **total object size** (`data` + `binaryData`, minus metadata overhead — use a
  safety margin). Kohen fails closed on oversize (T-LIMIT). Very large
  file-tree configs are out of scope (§2.4).

---

## 2. Background & Motivation

### 2.1 The problem

Teams want a **dedicated git repository** as the source of truth for application
configuration and its secret wiring — reviewable, auditable, multi-environment,
decoupled from release cycles. Turning that git content into the `ConfigMap` a
workload consumes, wiring in the secrets it references, and rolling the workload
when either changes is bespoke today: hand-rolled CI jobs, separate config and
secret pipelines, annotation-hash tricks, and manual `kubectl rollout restart`.

### 2.2 Why not just GitOps?

GitOps controllers (Argo CD, Flux) reconcile Kubernetes objects toward a
git-declared desired state and *can* manage `ConfigMap`s. Kohen is complementary
and narrower: it is purpose-built for **config repo path → `ConfigMap` +
resolved secret references + version-matched rollout** of a specific workload,
over a **dedicated config repository** distinct from the deployment repo.

Use GitOps to deploy what runs; use Kohen to keep a running workload's config
object, resolved secrets, and rollouts in sync with a dedicated config repo.
Coexistence requirements are normative in §6.2.

### 2.3 Prior art / positioning

| Approach | What it does | How Kohen differs |
| --- | --- | --- |
| CI job rendering `ConfigMap`s | Pipeline turns git into objects | Kohen is a running operator, decoupled from CI/releases; also resolves referenced secrets and drives the rollout |
| GitOps (Argo CD, Flux) | Reconcile cluster desired state from git | Kohen is a narrow config-path→object + secret-resolution + rollout loop over a dedicated config repo; composes with GitOps |
| External Secrets Operator | Pulls secrets from external stores into `Secret`s | Kohen **consumes** ESO: applies/references `ExternalSecret`s and wires the resulting `Secret` into the pod; ESO remains the authority |
| Stakater Reloader | Restarts pods when a `ConfigMap`/`Secret` changes | Kohen owns a version-matched rollout keyed on the git commit rather than a content hash; no third-party restart tool needed |
| Bespoke init scripts | Clone config at startup | One-shot, no live updates, no object model, no rollout |

### 2.4 When to use Kohen — and when not

| Scenario | Use Kohen? | Use instead / notes |
| --- | --- | --- |
| Dedicated config repo should drive a workload's `ConfigMap` + secrets + rollouts | **Yes** | Kohen's core case |
| GitOps deploys the app; config lives in a **separate** config repo | **Yes** | Apply the GitOps ignore rules first (§6.2) |
| GitOps already renders the app **and** its `ConfigMap` from the same repo | No | Keep your GitOps pipeline; a second reconciler adds no value |
| Config exceeds `ConfigMap` limits (~1 MiB, big file trees) | No | `git-sync`-to-volume pattern; Kohen volume mode is deferred (§19) |
| You only need secrets from an external store | No | Use External Secrets Operator directly |
| You hand-author a `ConfigMap` and want restart-on-change | No | Stakater Reloader alone is enough |
| You want product feature toggles/experiments in code | No | Feature-flag platform (OpenFeature) |

---

## 3. Goals, Non-Goals & Threat Model

### 3.1 Goals

- **G1** Continuously synchronize a git `repo@ref:path` into a target
  **`ConfigMap`** in the workload's namespace, with safe defaults so that
  source + path + workloadRef is a complete configuration (§1.2, §11.2).
- **G2 — Secret resolution (not production).** For each declared secret
  reference, assume the backing secret exists or reconcile until it does, then
  make it available to the workload via existing Kubernetes mechanics (§8).
  Kohen MUST NOT store, encrypt, generate, or rotate secret material.
- **G3 — v1 secret backends.** v1 MUST support **External Secrets Operator**
  (primary; also the documented path for Vault/AWS/GCP/Azure) and **native
  `Secret`**. Sealed Secrets SHOULD follow in v1.1. Direct Vault
  (injector/CSI) and Secrets Store CSI integration are deferred (§19) — their
  mechanics cannot satisfy the v1 readiness contract (R8.9).
- **G4** Support a **multi-environment** repository model (dev/staging/prod)
  selected by ref and/or path.
- **G5** Keep target objects **current** and **fleet-consistent**: resolve the
  ref to a single commit, produce objects from it, converge all pods to that
  version.
- **G6 — Version-matched rollout.** Record the resolved config version on the
  workload and trigger a rolling update **only when it changes** (§9).
- **G7 — Secure by default.** Least-privilege git access and RBAC; namespace
  locality; an explicit threat model (§3.3) with authorization requirements
  (R-AUTH.\*); secret values never exposed.
- **G8 — Fail-safe.** A git/backend/Kohen outage must never delete or corrupt
  existing objects, wire an unresolved secret, or take down healthy workloads.

### 3.2 Non-Goals

- **N1** Not a GitOps/CD engine; does not reconcile arbitrary manifests or
  deploy applications.
- **N2** Not a secrets manager (see G2).
- **N3** Does not own the schema/validation semantics of an app's config beyond
  structural checks.
- **N4** Not a templating platform. v1 renders files **verbatim**;
  overlay/templating is deferred (§19).
- **N5** Does not deliver config as a large file tree/volume; bounded by
  `ConfigMap` limits (T-LIMIT).
- **N6** Operator-only in v1; sidecar/env-var mode deferred (§19).
- **N7** No UI; v1 ships **without a dedicated CLI** — `kubectl` is the
  interface (§15). `kohenctl` is deferred (§19).
- **N8** v1 does not support `envFrom` surfacing (atomic list — not
  SSA-mergeable, §6.2), `subPath` mounts (§1.3), `OnDelete` update strategies
  (§9), or `DaemonSet` targets (deferred, §19).

### 3.3 Threat model & trust boundaries

Actors: **namespace developer** (can create `ConfigSync`), **git committer**
(can merge to the config repo), **platform admin** (installs/configures the
operator), **external attacker** (network), **compromised operator pod**.

| # | Threat | Control |
| --- | --- | --- |
| TM1 | A `ConfigSync` creator wires namespace `Secret`s into a pod they control (confused deputy) | **R-AUTH.1**: creating a `ConfigSync` is security-equivalent to creating a Pod (pods can already mount any namespace Secret). RBAC for `configsyncs` MUST be granted accordingly and this MUST be documented. Optional tightening: operator-level policy restricting referable `Secret`s per namespace (R-AUTH.2). |
| TM2 | A `ConfigSync` points at an attacker-controlled git repo; committed `ExternalSecret`s pull from permissive stores | **R-AUTH.3**: optional (recommended-on) operator-level **git source allow-list**; syncs to non-allowed URLs fail closed. **R-AUTH.4**: applied manifests are restricted to namespaced, allow-listed kinds (`ExternalSecret`; `SealedSecret` in v1.1) and, when configured, an allow-list of `secretStoreRef` names. Cluster-scoped secret CRs MUST never be applied from a namespaced path. |
| TM3 | Cross-namespace reach (wire ns-A secrets into ns-B workloads) | **R-AUTH.5**: `workloadRef`, target `ConfigMap`, and all resolved `Secret`s MUST be in the `ConfigSync`'s namespace. No cross-namespace mode in v1 (CEL-enforced). |
| TM4 | Git-credential theft via `authSecretRef` pointing at arbitrary Secrets | **R-AUTH.6**: `authSecretRef` MUST reference a `Secret` labeled `kohen.dev/git-credential: "true"` (CEL/webhook enforced). |
| TM5 | SSRF via `source.url` (metadata endpoints, internal hosts) | **R-AUTH.7**: scheme allow-list (`https`, `ssh`); link-local/metadata IP ranges blocked; redirects to disallowed hosts fail closed. |
| TM6 | Malicious repo tree (path traversal, symlink escape) | **R7.5**: rendered keys MUST be relative and normalized; `..`, absolute paths, and symlinks escaping `path` MUST be rejected. |
| TM7 | Git write access ≈ config-delivery authority | Documented explicitly: whoever can merge to a bound path controls the delivered config. Branch protection/review is the control; commit-signature verification is deferred hardening (§19). |
| TM8 | Compromised operator pod | Blast radius = operator SA: read referenced `Secret`s and patch bound workloads **in served namespaces only** (T7). Namespace-scoped installation MUST be supported to bound this; the blast radius MUST be documented in the hardening guide. |
| TM9 | Secret leakage through logs/status/events/metrics | **R8.3** + centralized redacting logger; leak tests in CI (NFR9). |

**v1 multi-tenancy stance (normative):** Kohen v1 targets clusters where a
namespace's `ConfigSync` creators are trusted with that namespace's secrets
(the same trust Kubernetes grants pod creators). Stricter multi-tenant
authorization (admission-webhook policy binding git repos to namespaces and
secrets to consumers) is deferred hardening (§19) and MUST be documented as
such.

---

## 4. Personas & Use Cases

### 4.1 Personas

- **Application developer (Dana).** Wants a service's `ConfigMap` and secret
  wiring to come from a reviewed PR in a config repo, declared once via a
  `ConfigSync`, rolling out automatically on change.
- **Platform / SRE engineer (Sam).** Wants a central, auditable,
  least-privilege reconciler that integrates with the cluster's secret backend
  and drives consistent rollouts — installable per-namespace to bound blast
  radius.
- **Security reviewer (Riley).** Wants config changes attributable to commits,
  no plaintext secrets in git, an explicit threat model, and RBAC guidance.

### 4.2 Primary use cases

- **UC1 — Config sync.** A `ConfigSync` keeps a workload's `ConfigMap` in sync
  with a git path, fleet-consistently.
- **UC2 — Secret resolution.** Config references a secret; Kohen resolves the
  backing `Secret` and makes it available to the pod — no plaintext in git.
- **UC3 — ESO integration.** The secret is backed by an `ExternalSecret`
  (committed to git or pre-existing); Kohen applies/awaits it and wires the
  resulting `Secret`.
- **UC4 — Multi-environment.** dev/staging/prod selected by ref and/or path.
- **UC5 — Version-matched rollout.** A merged commit (or env-surfaced secret
  rotation) advances the config version and triggers exactly one rolling
  update; an unchanged version triggers none.
- **UC6 — Rollback / pin.** Reverting the repo or setting `spec.source.ref` to
  a prior tag/commit restores the objects and rolls back.
- **UC7 — Audit.** The applied config version and source commit are readable
  from workload metadata and `ConfigSync` status.

---

## 5. Terminology

- **Config repo / config path:** the dedicated git repository and the
  subdirectory a sync consumes.
- **`ConfigSync`:** the custom resource describing one git-path→objects
  synchronization and its target workload (§11).
- **Source commit:** the git commit SHA the ref resolved to (status field
  `sourceCommit`).
- **Config version:** the rollout-trigger identity:
  `git:<source-commit>` extended with `sec:<secret-version-hash>` **only when
  env-surfaced secrets exist** (§9). Stamped on the workload and shown as
  `configVersion` in status.
- **Version stamp:** annotation `kohen.dev/config-sha` (fixed name, not
  configurable in v1) on the target workload's pod-template metadata — or on
  the workload object's metadata when `rollout: none` (R-VERSION).
- **Secret reference:** a pointer in `ConfigSync.spec.secretRefs` (never a
  value) to secret material owned by a supported backend; Kohen **resolves** it
  and wires it into the pod.
- **Secret version token:** a non-secret identifier of a resolved secret's
  current state (R8.10) used for rotation detection — never derived from secret
  values.
- **Fleet consistency:** all pods of the workload run objects produced from one
  resolved commit, guaranteed because every rolled pod carries the same stamp.

---

## 6. System Architecture

Kohen is a single operator `Deployment` reconciling `ConfigSync` resources.

```
            ┌──────────────────────────────────────────────┐
            │            Config git repository              │
            │   services/checkout/{dev,staging,prod}/...    │
            └───────────────┬──────────────────────────────┘
                            │ fetch repo@ref:path (poll + force-sync, read-only)
                            ▼
                 ┌───────────────────────┐   watches: ConfigSync, referenced
                 │   kohen-operator      │   Secrets, ExternalSecrets, owned
                 │   (Deployment)        │   objects, target workloads
                 └──────────┬────────────┘
        render config        │ resolve secret refs         stamp version + rollout
        ─────────────┐       │       ┌──────────────────┐        │
                     ▼       ▼       ▼                  │        ▼
   ┌───────────────────────────────────────────────────────────────────────┐
   │ ConfigSync namespace (locality enforced, R-AUTH.5)                      │
   │   ConfigMap <workload>-config     (non-secret config)                   │
   │   ExternalSecret (applied from git) ──ESO──► Secret                     │
   │   Deployment/StatefulSet: volume+mount / env wired; version annotation  │
   └───────────────────────────────┬───────────────────────────────────────┘
                                    │ built-in controller rolls on stamp change
                                    ▼
                        ┌───────────────────────────┐
                        │  Application workload     │
                        └───────────────────────────┘
```

### 6.1 The reconcile loop

Triggers: poll interval (`spec.sync.interval`, default 30s), watch events on the
`ConfigSync`, referenced `Secret`s/`ExternalSecret`s, owned objects, and the
target workload, and the **force-sync annotation** (`kohen.dev/sync-now: <any>`
on the `ConfigSync` triggers an immediate reconcile; the operator clears it).

One cycle:

1. **Fetch** `repo@ref:path`; resolve the ref to a concrete commit
   (`sourceCommit`). Enforce R-AUTH.3/R-AUTH.7.
2. **Render** non-secret files → desired `ConfigMap` (§7.4). Manifest files
   Kohen applies (§8.2) and reserved `kohen.*` files are **excluded** from
   `ConfigMap` keys (R7.6).
3. **Resolve secrets** per `spec.secretRefs` (§8): apply-if-present or await;
   enforce readiness policy (R8.9). Unresolved ⇒ fail closed for this version.
4. **Apply** the `ConfigMap` and Kohen-owned secret manifests via SSA with
   ownership labels; **prune** owned objects that vanished from git.
5. **Wire & stamp** — merge Kohen-owned volume/mount/env fields and the version
   annotation into the workload (§6.2); compare desired config version to the
   stamp; on mismatch update the annotation (one rolling update); on match, do
   nothing.
6. **Report** status, conditions (§11.4), events, metrics.

### 6.2 Workload wiring (merge into the existing definition)

Kohen mutates the target workload — but only by **merging** Kohen-owned fields,
engineered to coexist with GitOps:

- **R-WIRE.1 (SSA, dedicated field manager).** All workload mutation MUST use
  Server-Side Apply with field manager `kohen`, claiming ownership of **only**
  the fields Kohen injects: its named volume, its `volumeMounts` entry, its
  `env` entries, and the `kohen.dev/config-sha` annotation.
- **R-WIRE.2 (SSA-mergeable constructs only).** Injected list entries MUST be
  keyed map-list members (`volumes[name]`, `containers[name].volumeMounts[mountPath]`,
  `containers[name].env[name]`). **`envFrom` MUST NOT be used** — it is an
  atomic list and cannot be co-owned. Env surfacing uses discrete
  `env[]` entries with `valueFrom.secretKeyRef`/`configMapKeyRef`.
- **R-WIRE.3 (container targeting).** Wiring targets an explicit container:
  `spec.wiring.container` (default: the **first container** in the pod spec).
  Init containers are not targeted in v1.
- **R-WIRE.4 (conflict behavior).** Kohen never force-takes fields owned by
  another manager. On SSA conflict it re-reads and re-applies only its own
  fields. Kohen MUST NOT modify any pod-spec field it does not own.
- **R-WIRE.5 (GitOps compatibility matrix — normative docs).** Coexistence is
  guaranteed only when the other manager (a) uses SSA (Argo CD
  `ServerSideApply=true`, Flux SSA) **and** (b) applies the documented ignore
  rules (Argo CD `ignoreDifferences`, Flux drift exclusions) for
  `kohen.dev/config-sha` and Kohen-labeled volumes/mounts/env. Client-side /
  whole-object appliers **will strip Kohen's fields**; this MUST be documented
  as unsupported-without-ignores, and the docs MUST ship copy-paste snippets
  for Argo CD and Flux. Users MUST apply these **before** creating the
  `ConfigSync`.
- **R-WIRE.6 (unwire on delete).** On `ConfigSync` deletion (finalizer), Kohen
  MUST remove exactly its owned fields (SSA apply of an empty owned set) and
  its version annotation, leaving the rest of the workload untouched.

### 6.3 Installation model

- One operator `Deployment` (namespace `kohen-system` by convention), installed
  via **Helm** (also plain manifests). CRDs cluster-scoped as usual.
- **Two supported RBAC scopes** (T7): **cluster-wide** (watch `ConfigSync` in
  all namespaces) or **namespace-scoped** (per-namespace `Role`s only;
  recommended where blast radius matters, TM8).
- Optional operator-level configuration: git source allow-list (R-AUTH.3),
  secret-store allow-list (R-AUTH.4), referable-secret policy (R-AUTH.2),
  `maxDegradedDuration` (R8.11).

---

## 7. Configuration Repository Model

### 7.1 Requirements

- **R7.1** A config repo MUST be a standard git repository reachable over HTTPS
  or SSH.
- **R7.2** A sync selects a **ref** (branch, tag, or commit — `spec.source.ref`)
  and a **path**. Branch tracking follows the moving branch; tag/commit pins are
  immutable (UC6 rollback = set a prior tag/commit or revert the branch).
- **R7.3** Multiple environments are modeled by **path** (default;
  `services/x/{dev,prod}` on one branch) and/or **ref** (env branches). The
  selection MUST be explicit.

### 7.2 Layout convention (recommended, not enforced)

```
config-repo/
└── services/
    └── checkout/
        └── prod/
            ├── app.yaml               # -> ConfigMap key "app.yaml"
            ├── logging.conf           # -> ConfigMap key "logging.conf"
            └── external-secret.yaml   # ExternalSecret manifest: applied, NOT a ConfigMap key
```

### 7.3 Single configuration surface

The **`ConfigSync` resource is the only control surface in v1**. An in-repo
`kohen.yaml` (repo-owned targets/refs/policy) is deferred (§19) to avoid a
second config surface and precedence rules.

### 7.4 Config → `ConfigMap` mapping

- **R7.4** Each regular file under `path` becomes one key in the target
  `ConfigMap` (key = path relative to `path`, with `/` replaced by a documented
  safe separator since ConfigMap keys cannot contain `/`; binary content →
  `binaryData`). Mapping is deterministic and **verbatim** (no templating).
- **R7.5 (tree safety).** Keys MUST be relative and normalized; `..`, absolute
  paths, and symlinks resolving outside `path` MUST be rejected (fail closed).
- **R7.6 (exclusions).** Files that Kohen consumes as apply-if-present
  manifests (recognized `ExternalSecret`, and `SealedSecret` in v1.1) and
  reserved `kohen.*` files are excluded from `ConfigMap` keys.
- **R7.7 (oversize).** If rendered content would exceed the `ConfigMap` object
  size limit (T-LIMIT, with safety margin), the sync MUST fail closed with an
  actionable error (condition + event naming the offending size). Splitting is
  deferred (§19); the documented remedy is "reduce the config or split the
  path".

### 7.5 Git authentication (`authSecretRef`)

- **R7.8** Credentials come from a `Secret` in the `ConfigSync` namespace,
  labeled `kohen.dev/git-credential: "true"` (R-AUTH.6), with a documented
  schema:
  - HTTPS: `username` (optional), `password` (token).
  - SSH: `ssh-privatekey`, plus `known_hosts` (host-key verification is ON by
    default; TLS verification is ON by default; disabling either requires an
    explicit, logged opt-in).

---

## 8. Secret Integration & Resolution

Kohen's secret responsibility is **integration, not production**. Config in git
never contains secret values; the `ConfigSync` declares **references**; Kohen
resolves each reference and surfaces the material via standard constructs.

### 8.1 Principles

- **P1 — No plaintext secrets in git.**
- **P2 — Kohen never owns the material.** The backend (ESO / native `Secret`)
  is the authority.
- **P3 — Assume-exists, else reconcile.** Missing material ⇒ wait/requeue with
  backoff, `Pending`/`Degraded`, fail closed per R8.9. Never wire an
  unresolved reference.
- **P4 — Existing mechanics only.** `Secret` volume + mount (file) or `env[]`
  with `secretKeyRef` (env). No bespoke delivery. No `envFrom` (R-WIRE.2).

### 8.2 What "resolve" means

For each reference:

1. **Identify** the backing `Secret` (name, keys).
2. **Apply-if-present, else await.** If the config path contains a recognized
   `ExternalSecret` manifest, Kohen **applies** it (SSA + ownership + prune —
   R8.8) subject to R-AUTH.4; otherwise Kohen awaits the externally-managed
   object. Reconcile until the `Secret` exists with the required keys and (for
   ESO) the `ExternalSecret` reports `Ready=True` (R8.9 governs when this
   gates).
3. **Wire** it into the workload per `surface` (§8.4).
4. **Track rotation** via the secret version token (R8.10).

### 8.3 v1 backends

| Backend | How the `Secret` exists | Kohen's role |
| --- | --- | --- |
| `externalSecret` (**primary**) | ESO syncs from Vault/AWS/GCP/Azure/… via an `ExternalSecret` | Apply the `ExternalSecret` from git if present (else await it); gate on `Ready` per R8.9; wire the target `Secret` |
| `nativeSecret` | Created out-of-band (another controller, cert-manager, kubectl) | Await existence + required keys; wire it |

**Vault** is supported **via ESO** (documented decision tree). Sealed Secrets
(v1.1), Vault injector/CSI, and Secrets Store CSI are deferred (§19): the
injector/CSI paths have no operator-visible readiness signal and cannot honor
the v1 fail-closed contract (R8.9).

### 8.4 Surfacing

- **File:** Kohen adds a `Secret` volume + `volumeMount` at
  `surface.mountPath` in the target container. Kubelet updates the files on
  rotation (no restart, no rollout).
- **Env:** Kohen adds `env[]` entries with `valueFrom.secretKeyRef`. Env values
  change only on pod restart ⇒ env-surfaced rotation participates in the
  rollout (§9).

### 8.5 Requirements

> Requirement IDs are stable across spec versions; gaps (R8.1, R8.2, R8.6,
> R8.7) are retired v0.5 IDs whose content moved into §8.2/§8.3, R-AUTH.\*, or
> R8.12.

- **R8.3 (redaction).** No secret value may ever appear in git, logs, events,
  metrics, status, or CLI output. Implementations MUST use a centralized
  redacting logger, MUST NOT log raw `Secret` objects or rendered env/volume
  specs, and MUST keep metric label cardinality bounded (names/keys only).
  Leak tests run in CI on every PR touching reconcile/logging (NFR9).
- **R8.4 (fail closed on unresolved).** A missing `Secret`/key blocks
  stamping/rollout for that version; keep last-good; `Pending`/`Degraded`;
  event; requeue.
- **R8.5 (rotation).** File-surfaced rotation is delivered by kubelet and MUST
  NOT trigger a rollout. Env-surfaced rotation advances the config version
  (§9) and rolls the workload. Whether env-rotation auto-rollout can be
  disabled per-reference is a documented option (`surface.rolloutOnRotate`,
  default `true`).
- **R8.8 (owned-manifest lifecycle).** Manifests applied from git use SSA +
  ownership labels and are pruned when removed from git; Kohen MUST NOT
  adopt/overwrite pre-existing objects it does not own (no adoption mode in
  v1).
- **R8.9 (readiness policy — asymmetric).**
  - *First resolution* (no prior wired-and-rolled version for this reference):
    **fail closed** — do not wire/stamp/roll until the `Secret` exists and (ESO)
    `Ready=True`. Prevents rolling pods that would crash on a missing secret.
  - *Subsequent updates* (a prior good version is running): **fail safe** —
    keep last-good wiring/version, mark `Degraded`, requeue, reconcile forward
    when ready. Never tear down a healthy workload for a transient backend
    outage.
- **R8.10 (secret version token — never values).** Rotation detection uses
  metadata only: for `nativeSecret`, the `Secret`'s `resourceVersion` +
  observed key set; for `externalSecret`, the `ExternalSecret`'s synced
  revision/status (falling back to target `Secret` `resourceVersion`).
  Implementations MUST NOT hash or otherwise derive tokens from secret
  **values**.
- **R8.11 (bounded degradation).** Fail-safe (R8.9) MUST NOT mask staleness
  forever: after a configurable `maxDegradedDuration`, Kohen keeps the
  workload running but stops advancing to new versions and emits a
  security-visible event/metric.
- **R8.12** Conflicting/ambiguous references (duplicate names, overlapping
  mounts) MUST be rejected at validation time.

---

## 9. Consistency, Atomicity & Rollout

- **R-ATOM.** Object updates are applied atomically (SSA of fully-formed
  objects). In-pod file delivery relies on kubelet's atomic symlink swap
  (eventually consistent within the kubelet sync period; `subPath` excluded,
  §1.3).
- **R-CONS.** The operator resolves the ref to a **single commit** per
  reconcile and produces all objects from it. The **config version** is the
  string `git:<short-sourceCommit>`, extended to
  `git:<short-sourceCommit>-sec:<hash of env-surfaced secret version tokens>`
  when any secret is env-surfaced (normative format; `-` joins the
  components). File-surfaced secret rotation does **not** change the config
  version (R8.5) — kubelet delivers it in place.
- **R-VERSION.** The authoritative record of "which config version this
  workload runs" is the stamp `kohen.dev/config-sha`: on the **pod-template**
  metadata when `rollout: auto`, or on the **workload object's** metadata when
  `rollout: none` (recorded without touching the pod template, so no restart
  is triggered). Matching desired-vs-stamped makes reconciliation idempotent
  and the version auditable from workload metadata in both modes (UC7). Status
  additionally exposes `sourceCommit` (plain git SHA) so users can correlate
  with git history (the stamp holds the composite version, not necessarily the
  bare SHA).
- **R-ROLLOUT.**
  - **.1 (stamp).** After successful apply + resolution, write the config
    version to the location R-VERSION prescribes for the sync's `rollout`
    mode, via §6.2 merge semantics.
  - **.2 (match).** Trigger a rollout **only** on desired ≠ stamped; on match,
    make no write (no spurious rollouts).
  - **.3 (trigger).** Only the annotation update triggers the rollout; the
    built-in controller executes it with the workload's own strategy. Kohen
    MUST NOT delete pods.
  - **.4 (order).** Stamp only after the `ConfigMap` is applied and all
    references resolved (R8.4/R8.9).
  - **.5 (supported targets).** `Deployment` and `StatefulSet` with
    `RollingUpdate` strategy. `OnDelete` is **unsupported**: Kohen MUST detect
    it, set `Degraded` with an explanatory reason, and skip stamping.
    `DaemonSet` is deferred (§19).
  - **.6 (concurrency).** If a new version arrives during an in-progress
    rollout, the latest desired version wins (stamp again); intermediate
    versions are coalesced.
- **R-SAFE.** A failed/interrupted reconcile leaves last-good objects and the
  current stamp intact.
- **R-ROLLBACK.** Reverting the repo or pinning `spec.source.ref` to a prior
  tag/commit reproduces the earlier objects and stamps the prior version (per
  R-VERSION's mode-dependent location).
- **R-SINGLETON.** **At most one `ConfigSync` may target a given workload**
  (enforced; a second sync targeting the same `workloadRef` is rejected /
  `Degraded` with reason). Multiple workloads sharing a config path use one
  `ConfigSync` each (fetches are deduped by repo+commit, T10); each sync owns
  its own `ConfigMap` (distinct names — sharing one `ConfigMap` object across
  syncs is not supported in v1).

### 9.1 Reload semantics (how changes reach the app)

- **Mounted files (default):** kubelet updates the mounted `ConfigMap`/`Secret`
  atomically. Apps that hot-reload files pick changes up without restart.
- **Rollout (default-on):** the version stamp changes on every config-version
  change, causing a rolling restart — the universal path for apps that read
  config at startup or via env.
- **`spec.rollout: auto | none`** (default `auto`): `none` keeps the pod
  template untouched — the version is recorded on the workload object's
  metadata instead (R-VERSION) — for hot-reload-capable apps that must not
  restart on config changes. Env surfacing with `rollout: none` is rejected at
  validation time (env changes require a restart to take effect). There is no
  third-party-reloader mode in v1 (Reloader interop deferred, §19).

---

## 10. Failure Modes & Resilience

| Failure | Required behavior |
| --- | --- |
| Git unreachable | Backoff retry; keep last-good objects + stamp; `Degraded`; never prune on fetch failure. |
| Auth failure | Fail fast, actionable error; event; no lockout-inducing retry storm; redacted. |
| Disallowed source URL (R-AUTH.3/.7) | Fail closed; `Degraded` with reason; security event. |
| Malformed/missing path; tree-safety violation (R7.5) | No apply/stamp; keep last-good; `Degraded`; event. |
| Oversize (T-LIMIT/R7.7) | Fail closed; condition + event with size and remedy; keep last-good. |
| Referenced secret missing / key absent | Fail closed (R8.4); `Pending`/`Degraded`; requeue; value never logged. |
| Backend not ready — first resolution | Fail closed (R8.9); `Pending`; no rollout of crash-bound pods. |
| Backend not ready — prior good version running | Fail safe (R8.9); keep last-good; `Degraded`; bounded by R8.11. |
| Secret rotates (file-surfaced) | Kubelet updates in place; no rollout (R8.5). |
| Secret rotates (env-surfaced) | Version advances → one rollout (R8.5, R-CONS). |
| Version stamp already matches | No-op; no workload write (R-ROLLOUT.2). |
| Workload not found / wrong kind / `OnDelete` | Object sync continues; `WorkloadWired=False` with reason (`WorkloadNotFound` / `UnsupportedStrategy`); no stamp. |
| Second `ConfigSync` targets same workload | Rejected / `Degraded` (R-SINGLETON). |
| Rollout stuck (crashloop on new config) | Built-in controller + `progressDeadlineSeconds`; surface status; never force-delete; rollback = pin prior ref. |
| SSA conflict with another manager | Re-apply own fields only (R-WIRE.4); documented ignore rules (R-WIRE.5) prevent flapping. |
| Operator down | Objects and stamps persist; no new versions; graceful recovery. |

- **R10.1** Retries use bounded exponential backoff with jitter.
- **R10.2** Every failure state above maps to a **named condition reason**
  (§11.4) and a metric — not only logs. (Exception: "operator down" cannot set
  conditions by definition; it is observable via the health endpoints and the
  absence of the operator's metrics, R13.3.)

---

## 11. Kubernetes API

### 11.1 `ConfigSync` (namespaced) — full shape

```yaml
apiVersion: kohen.dev/v1alpha1
kind: ConfigSync
metadata:
  name: checkout-prod
  namespace: checkout
spec:
  source:
    url: https://github.com/acme/platform-config.git
    ref: main                            # branch | tag | commit SHA (pin = rollback/UC6)
    authSecretRef: { name: kohen-git-creds }   # optional; Secret labeled kohen.dev/git-credential
  path: services/checkout/prod
  workloadRef:
    kind: Deployment                     # Deployment | StatefulSet (RollingUpdate only)
    name: checkout
  configMap:
    name: checkout-config                # default: <workloadRef.name>-config
  wiring:
    container: checkout                  # default: first container
    mountPath: /etc/kohen/config         # default
  rollout: auto                          # auto (default) | none
  sync:
    interval: 30s                        # default
  secretRefs:                            # optional (§8)
    - name: db-password
      backend: externalSecret            # externalSecret | nativeSecret (v1)
      externalSecret: { name: checkout-db }
      surface:
        as: env
        envVar: DB_PASSWORD
        key: password
        rolloutOnRotate: true            # default
    - name: tls
      backend: nativeSecret
      nativeSecret: { name: checkout-tls }
      surface:
        as: file
        mountPath: /etc/tls
status:
  sourceCommit: 9f1c2ab34…               # plain git SHA (correlate with git log)
  configVersion: git:9f1c2ab-sec:71aa02  # rollout-trigger identity (stamped value)
  workloadVersion: git:9f1c2ab-sec:71aa02
  rolloutInProgress: false
  secretRefs:                            # resolution state only — never values
    - { name: db-password, resolved: true,  backend: externalSecret }
    - { name: tls,         resolved: false, reason: SecretNotFound }
  conditions: [ … ]                      # contract in §11.4
```

### 11.2 Defaults (normative)

| Field | Default |
| --- | --- |
| `source.ref` | `main` |
| `configMap.name` | `<workloadRef.name>-config` |
| `wiring.container` | first container of the pod spec |
| `wiring.mountPath` | `/etc/kohen/config` |
| `rollout` | `auto` |
| `sync.interval` | `30s` |
| Version-stamp annotation | `kohen.dev/config-sha` (fixed, not configurable) |
| `surface.rolloutOnRotate` | `true` |

### 11.3 Validation & lifecycle requirements

- **R11.1** CRDs validate via OpenAPI + CEL, including: namespace locality
  (R-AUTH.5), credential label (R-AUTH.6), supported `workloadRef.kind`,
  `rollout` enum, env surfacing combined with `rollout: none` (rejected,
  §9.1), secret-ref conflicts (R8.12), and R-SINGLETON (webhook or
  reconcile-time rejection).
- **R11.2** Status reports `sourceCommit`, desired/stamped versions, rollout
  state, and per-reference resolution (no values).
- **R11.3** Deletion (finalizer): prune Kohen-owned objects and unwire owned
  workload fields (R-WIRE.6); never delete the workload or un-owned secrets.
- **R11.4** Printer columns: `READY`, `SOURCE-COMMIT`, `CONFIG-VERSION`,
  `WORKLOAD-VERSION`, `AGE`.

### 11.4 Conditions contract

| Type | Meaning | Example reasons |
| --- | --- | --- |
| `Ready` | Overall: desired version fully applied, wired, converged | `Synced`, `Progressing`, `Degraded` |
| `Fetched` | Git fetch/resolve of `spec.source` succeeded | `FetchFailed`, `AuthFailed`, `SourceNotAllowed`, `PathNotFound` |
| `Rendered` | Config rendered within limits | `Oversize`, `TreeSafetyViolation` |
| `SecretsReady` | All references resolved per R8.9 | `SecretNotFound`, `KeyMissing`, `AwaitingFirstResolution`, `BackendNotReady`, `DegradedServingLastGood`, `MaxDegradedExceeded` |
| `WorkloadWired` | SSA merge of owned fields succeeded | `WorkloadNotFound`, `UnsupportedStrategy`, `ApplyConflict`, `SingletonViolation` |
| `RolloutComplete` | Stamped version == workload's observed rolled-out state | `RollingOut`, `ProgressDeadlineExceeded` |

Every §10 failure row maps to one of these reasons (R10.2). Docs MUST include a
troubleshooting table: *symptom → condition/reason → action*.

---

## 12. Technical Requirements

- **T1** Go (current stable, ≥ 1.23); operator built on `controller-runtime`.
- **T2** Maintained Go git library; shallow fetch / fetch-by-commit; fetches
  deduped by repo+commit across syncs.
- **T3** All object writes via SSA with field manager `kohen`; idempotent
  (same commit + tokens ⇒ same objects; same stamp ⇒ no workload write).
- **T4** Kubernetes N-2 minor-version compatibility (declared; the v1 CI gate
  runs latest + one older — see PLAN U3).
- **T-LIMIT** `ConfigMap`/`Secret` bounded by ~1 MiB total object size; Kohen
  fails closed with margin (R7.7).
- **T5** Multi-arch (`amd64`/`arm64`) minimal image; Helm + plain manifests.
- **T6** Operator footprint documented and bounded.
- **T7** Least-privilege RBAC per install scope (§6.3): read `ConfigSync` +
  labeled credential Secrets + referenced Secrets/ExternalSecrets in served
  namespaces; write owned `ConfigMap`s/`ExternalSecret`s; `get/patch` target
  workloads. Non-root, read-only rootfs, no cluster-admin. Honest limitation
  (documented): referenced-secret names are dynamic, so per-name
  `resourceNames` RBAC is not generally possible — namespace scoping is the
  blast-radius control (TM8).
- **T8** Git/backend outages: keep last-good, degrade, never crash workloads.
- **T9** Determinism: same inputs ⇒ byte-identical objects and stable version.
- **T10** Efficient reconciliation at hundreds of syncs (work queues, shared
  informers, rate limits, bounded fetch concurrency, watch-driven secret
  rotation detection).

---

## 13. Observability

- **R13.1 Metrics (Prometheus):** reconcile counts/duration; fetch
  latency/errors; renders (incl. oversize failures); secret resolution
  success/failure/pending (counts; bounded labels); rollouts triggered vs
  skipped-on-match; degraded gauges (incl. `MaxDegradedExceeded`); current
  version info metric.
- **R13.2 Logging:** structured JSON, centralized redaction (R8.3).
- **R13.3 Health:** standard readiness/liveness; liveness never flaps on
  git/backend outage.
- **R13.4 Events:** applies, prunes, stamps/rollouts, resolution transitions,
  all failure reasons.
- **R13.5 Audit:** `sourceCommit` + config version readable from status and
  workload metadata (UC7).

---

## 14. Non-Functional Requirements

- **NFR1 Reliability:** Kohen unavailability degrades freshness, never
  availability or integrity of applied objects/stamps.
- **NFR2 Performance:** change detected + applied within a bounded window
  (default ≤ 60 s via polling; force-sync annotation for immediate reconcile).
- **NFR3 Scalability:** hundreds of `ConfigSync`es per operator without
  overloading git or the API server.
- **NFR4 Compatibility:** mainstream git providers over HTTPS/SSH; ESO;
  SSA-based GitOps controllers per R-WIRE.5.
- **NFR5 Portability:** any conformant Kubernetes; `amd64` + `arm64`.
- **NFR6 Usability:** §1.2 (with defaults §11.2) is a complete, copy-paste
  Day-1 path, demonstrated end-to-end in the getting-started runbook; §2.4
  guidance prominent in README/docs.
- **NFR7 Documentation:** install (both RBAC scopes), Day-1 runbook, git auth,
  ESO integration, GitOps coexistence quickstart (with snippets, R-WIRE.5),
  troubleshooting (condition/reason catalog §11.4), kubectl operations
  (§15), security hardening + threat model (§3.3, TM8). The **README MUST carry
  two usage sections kept current every phase**: a **Getting Started** with a
  *minimal* configuration (fewest fields, maximum value out of the box, per §1.2
  + defaults §11.2) and an **Advanced configuration reference** documenting
  *every* shipped `ConfigSync`/operator field, credential key, annotation, and
  status/condition. Each phase that adds or changes a user-visible field MUST
  update both sections in the same change.
- **NFR8 Licensing:** Apache-2.0, contribution guide, code of conduct.
- **NFR9 Testing:** unit + integration (envtest, git fixture) + e2e (`kind`);
  leak tests on every PR touching reconcile/logging; abuse-case tests for
  R-AUTH.\* (see PLAN).
- **NFR10 Versioning:** SemVer; CRD deprecation policy; breaking CRD changes
  require a new API version with conversion.

---

## 15. Operations (kubectl-first)

v1 ships no CLI (N7). Documented `kubectl` recipes cover:

- **Status:** `kubectl get configsync` (printer columns), `kubectl describe`
  (conditions per §11.4).
- **Force sync:** `kubectl annotate configsync/<name> kohen.dev/sync-now=$(date +%s)`.
- **Pin / rollback:** `kubectl patch configsync/<name> --type=merge -p '{"spec":{"source":{"ref":"<tag-or-sha>"}}}'`.
- **Diff/verify:** git-side (`git diff`) — the repo is the source of truth.
- **Debug matrix:** the §11.4 symptom → reason → action table.

`kohenctl` (status/diff UX) is deferred (§19).

---

## 16. Acceptance Criteria (v1.0)

- **A1** The §1.2 minimal `ConfigSync` (source + path + workloadRef, defaults
  only) produces the expected `ConfigMap`, wiring, and stamp on a `kind`
  cluster.
- **A2** A commit updates the `ConfigMap`; a mounted-volume consumer observes
  the new content (non-`subPath`).
- **A3** A commit advances the config version, triggering **exactly one**
  rolling update; a reconcile with an unchanged version performs **no**
  workload write; `sourceCommit` and the stamp are readable (UC7).
- **A4** A secret backed by a git-committed `ExternalSecret` is applied,
  awaited (`Ready`), and wired as **file** and as **env**; no plaintext in
  git/logs/events/status (leak scanner).
- **A5** Readiness asymmetry: first resolution of a missing/unready secret
  fails closed (no stamp/rollout) then recovers; with a prior good version a
  transient backend outage keeps the workload running (`Degraded`), bounded by
  `maxDegradedDuration`.
- **A6** Rotation: file-surfaced rotation updates in place with **no rollout**;
  env-surfaced rotation advances the version and rolls **once**.
- **A7** `ConfigSync` deletion prunes owned objects and unwires owned workload
  fields only; the workload keeps running.
- **A8** Oversize rendering fails closed with the documented condition reason
  and event.
- **A9** The operator runs non-root/read-only-rootfs with the documented RBAC
  in both install scopes; RBAC conformance tests pass (fails without exact
  perms, succeeds with them).
- **A10** GitOps coexistence: with an SSA-based GitOps controller (Argo CD or
  Flux) managing the same workload and the documented ignore rules applied,
  there is **no flapping** and both controllers converge.
- **A11** Abuse cases fail closed: disallowed `source.url` (allow-list),
  unlabeled `authSecretRef`, cross-namespace reference, second `ConfigSync` on
  the same workload, applied-manifest kind/store outside the allow-list.
- **A12** Operator upgrade (Helm) and uninstall are clean: upgrade keeps syncs
  converging; uninstall leaves workloads running and objects in place
  (documented behavior).

---

## 17. Resolved Decisions (history)

- **RD1** Workload mutation is in scope, performed as SSA **merge** to coexist
  with GitOps (§6.2).
- **RD2** Secret manifests in git are **applied** (owned); otherwise awaited
  (§8.2).
- **RD3** Readiness: first-resolution fail-closed, update fail-safe (R8.9).
- **RD4 (v0.6)** v1 backends = ESO + native `Secret`; Vault via ESO; Sealed
  Secrets v1.1; CSI/injector deferred (readiness-contract mismatch).
- **RD5 (v0.6)** Single config surface: `ConfigSync` only; `kohen.yaml`
  deferred.
- **RD6 (v0.6)** Rotation/rollout split: only env-surfaced secret rotation
  triggers rollouts; file-surfaced rotation is kubelet-delivered.
- **RD7 (v0.6)** `envFrom` unsupported (atomic list vs SSA); env surfacing via
  discrete `env[]` entries.
- **RD8 (v0.6, confirmed by owner)** v1 trust stance: **namespace-level
  trust** — `ConfigSync` create ≈ Pod create (documented); allow-lists as
  optional tightening; strict multi-tenant authorization (admission-webhook
  policy) stays in the post-1.0 backlog with no v1 commitments.
- **RD9 (v0.6)** kubectl-first operations; `kohenctl` deferred.

---

## 18. Open Questions

1. **Key separator for nested paths** (R7.4): `ConfigMap` keys cannot contain
   `/`; pick and document the separator (e.g. `__`) — cosmetic but
   user-visible.
2. **Sealed Secrets v1.1 shape:** same apply-if-present engine; confirm policy
   controls (issuer/key scope) before enabling.
3. **GitOps snippet shipping:** docs-only vs generated `ignoreDifferences`
   fragments in the Helm chart.

---

## 19. Deferred / Post-1.0 Backlog

Sealed Secrets backend (v1.1) · Secrets Store CSI & Vault injector/CSI ·
`kohenctl` · git webhooks (disabled-by-default design exists in v0.5 history) ·
signed-commit verification · overlay/light templating · `ConfigMap` splitting ·
`DaemonSet` targets · Reloader interop mode · progressive rollout strategies ·
in-repo `kohen.yaml` control surface · strict multi-tenant authorization
(policy CR / admission webhook) · sidecar/env-var mode · direct-file-volume
mode · sync history CR (`ConfigRelease`).

---

## 20. Relationship to the Existing Prototype

The repository previously contained an early `kohen-agent` prototype (Go +
`go-git`) that cloned a repo into a directory via
`--gitUrl`/`--gitPath`/`--targetDir` and matching env vars. It validated the
git-fetch premise; its fetch logic maps to the operator's fetch/resolve step
(S1.1 in PLAN) and its env-var interface is the seed for the deferred sidecar
mode (§19). The prototype has now been removed (see git history) in favour of
the clean-room operator implementation described in this SPEC and sequenced in
PLAN.md; the git-source library (`internal/git`, S1.1) supersedes it.

---

## Appendix A — Requirement Index

Requirements are labeled inline (`Gn`, `Nn`, `UCn`, `TMn`, `R-AUTH.n`,
`R7.n`, `R8.n`, `R-WIRE.n`, `R-ROLLOUT.n`, `R-ATOM/CONS/VERSION/SAFE/ROLLBACK/
SINGLETON`, `R10.n`, `R11.n`, `R13.n`, `Tn`, `T-LIMIT`, `NFRn`, `An`, `Pn`,
`RDn`). PLAN.md references these IDs; a CI docs check SHOULD verify that PLAN
references resolve to existing SPEC IDs.
