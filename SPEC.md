# KOHEN — Specification

> Status: **Draft v0.1** · Owner: Kohen maintainers · Last updated: 2026-07-03
>
> This document is the single source of truth for **what** Kohen is, **why** it
> exists, and **which requirements** any implementation must satisfy. It is
> intentionally implementation-light: it describes behavior, contracts, and
> constraints rather than prescribing internal code structure. Where a concrete
> technology is named, it is a requirement; where an approach is named, it is a
> recommendation and is marked as such.

---

## 1. Overview

**Kohen** is a Kubernetes-native configuration management tool. It lets an
application consume its runtime configuration from a **dedicated `git`
repository** and keeps that configuration **consistently up to date** inside the
running workload after any change is committed.

Kohen is **not** an alternative to GitOps solutions (Argo CD, Flux, etc.). GitOps
tools reconcile *cluster and workload desired state* (Deployments, Services,
CRDs). Kohen operates one level lower and to the side: it delivers the
*application-level configuration files/values* that a running process reads,
sourced from a config repository that may span **multiple environments**. Kohen
can run alongside a GitOps stack, a Helm-only stack, or a plain
`kubectl apply` stack without conflict.

### 1.1 One-sentence definition

> Kohen mounts configuration from a dedicated git repository into a Kubernetes
> workload and guarantees that every replica converges to the same committed
> configuration version, atomically, and reloads or restarts the application
> when it changes.

### 1.2 Elevator pitch

Teams that operate many services across many environments frequently keep their
"knobs" — feature flags, tuning parameters, routing tables, allow-lists,
per-environment endpoints — in a dedicated configuration repository so that
changes are reviewable, auditable, and decoupled from application release
cycles. Getting that configuration *into* the workload, *consistently across
replicas*, and *refreshed without a full redeploy* is today a bespoke,
error-prone effort (init scripts, sidecars glued together, ConfigMap churn,
manual restarts). Kohen makes this a first-class, declarative capability.

---

## 2. Background & Motivation

### 2.1 The problem

Kubernetes offers `ConfigMap` and `Secret` as the native units of
configuration, but they create friction when the source of truth is git and the
configuration spans environments:

1. **Sync burden.** Something must translate git commits into `ConfigMap`
   updates. This is usually a hand-rolled CI job or a GitOps pipeline that
   re-renders and re-applies manifests. It couples config changes to deployment
   tooling.
2. **Update propagation is weak.** A mounted `ConfigMap` update is eventually
   reflected in the volume (with kubelet sync delay), but the application is
   **not** notified and most apps do not re-read files. Subscribers must build
   their own watch/reload logic, and environment variables sourced from
   `ConfigMap`/`Secret` never update without a pod restart.
3. **No cross-replica consistency guarantee.** During a rolling change,
   different replicas can observe different configuration versions for an
   unbounded window, and there is no built-in notion of "the whole fleet is now
   on config commit `abc123`".
4. **Multi-environment sprawl.** Representing dev/staging/prod (and per-region,
   per-tenant) variants inside cluster objects leads to duplicated manifests and
   drift. A dedicated repository with a clear directory/branch model is easier to
   review and audit, but nothing consumes it directly.
5. **Size and shape limits.** `ConfigMap` has a ~1 MiB limit and stores flat
   key/value data; delivering a directory tree of config files (e.g. nested
   YAML, templates, certificates bundles) is awkward.

### 2.2 Why not just GitOps?

GitOps controllers reconcile Kubernetes API objects toward a git-declared
desired state. They are excellent at *infrastructure and deployment* state. They
are a poor fit for *fast-moving, fine-grained, application-owned configuration*
because:

- Every config edit becomes a reconcile of Kubernetes objects and, typically, a
  workload rollout — heavyweight for a feature-flag flip.
- The application still cannot pull a specific config directory tree from a
  dedicated repo into its own filesystem with atomic swaps.
- GitOps does not provide an application-facing reload contract.

Kohen is **complementary**: use GitOps for what runs, use Kohen for what running
things read.

### 2.3 Prior art / positioning

| Approach | What it does | Gap Kohen fills |
| --- | --- | --- |
| `ConfigMap`/`Secret` volumes | Native k8s config units | No git source of truth, no reload signal, weak consistency, size limits |
| GitOps (Argo CD, Flux) | Reconcile cluster desired state from git | Heavyweight for app config; no in-container file delivery + reload |
| Spring Cloud Config / Consul / Vault | Config server apps query at runtime | Requires a running service + client libraries; not git-file-native or k8s-native by default |
| `git-sync` sidecar | Syncs a git repo into a volume | No environment model, no consistency guarantee across replicas, no reload contract, minimal k8s integration |
| Bespoke init scripts | Clone config at startup | One-shot only; no live updates; per-team reinvention |

Kohen's distinguishing combination: **git-native + multi-environment model +
atomic file delivery + application reload contract + fleet-wide consistency +
declarative Kubernetes integration.**

---

## 3. Goals & Non-Goals

### 3.1 Goals

- **G1** Deliver configuration from a dedicated git repository into a Kubernetes
  workload as files on a filesystem the application already reads.
- **G2** Support a **multi-environment** repository model (dev/staging/prod, and
  optionally region/tenant) selected declaratively per workload.
- **G3** Keep configuration **current**: detect committed changes and apply them
  to running workloads without requiring a manual redeploy.
- **G4** Guarantee **consistency**: updates are atomic per replica (no partially
  written config is ever visible) and the fleet converges to a single named
  config version.
- **G5** Provide a clear **application reload contract** (file watch, signal, or
  controlled rollout) so apps actually pick up changes.
- **G6** Be **Kubernetes-native**: declarative API (CRDs), optional automatic
  injection, standard RBAC, metrics, and events.
- **G7** Be **secure by default**: least-privilege git access, secret handling,
  and optional commit verification.
- **G8** Be **operable**: observable, debuggable, and safe to fail (a config
  backend outage must not take down healthy workloads).

### 3.2 Non-Goals

- **N1** Kohen is **not** a GitOps/CD engine and will not reconcile arbitrary
  Kubernetes manifests or perform application deployments.
- **N2** Kohen is **not** a general-purpose secrets manager. It integrates with
  existing secret stores but does not aim to replace Vault/Sealed
  Secrets/External Secrets. (It MAY deliver secret *material* referenced from the
  config repo; see §9.)
- **N3** Kohen does **not** own the *schema* or *validation semantics* of an
  application's configuration beyond optional structural checks; the application
  remains responsible for interpreting its config.
- **N4** Kohen is **not** a templating/rendering platform competitor to
  Helm/Kustomize. Light templating MAY be supported (§7.4) but complex
  rendering pipelines are out of scope for v1.
- **N5** Kohen does not provide a UI in v1 (CLI + Kubernetes API only).

---

## 4. Personas & Use Cases

### 4.1 Personas

- **Application developer (Dana).** Owns a service. Wants to change a feature
  flag or tuning value by opening a PR against the config repo and have it take
  effect in the target environment without cutting a new release.
- **Platform / SRE engineer (Sam).** Operates the cluster. Wants a standard,
  auditable, secure mechanism for config delivery, with metrics and safe failure
  modes, that they can offer as a self-service capability.
- **Security / compliance reviewer (Riley).** Wants config changes to be
  reviewed, attributable to a commit/author, verifiable (signed), and access to
  the config repo to be least-privilege.

### 4.2 Primary use cases

- **UC1 — Startup delivery.** On pod start, the correct environment's config is
  present on disk before the application's main container starts.
- **UC2 — Live update, hot reload.** A merged config change is delivered to
  running pods and the application reloads it without a restart.
- **UC3 — Live update, controlled rollout.** For applications that cannot hot
  reload, a config change triggers a safe rolling restart pinned to a single
  config version.
- **UC4 — Multi-environment.** The same workload spec deployed to dev and prod
  clusters selects different config paths/refs with no per-environment manifest
  duplication of the config itself.
- **UC5 — Rollback.** Reverting the config repo (or pinning to a prior commit)
  rolls the fleet back to a known-good configuration version.
- **UC6 — Audit.** Given a running workload, an operator can determine exactly
  which config commit each replica is serving.

---

## 5. Terminology (Glossary)

- **Config repository (config repo):** A dedicated git repository that is the
  source of truth for application configuration, distinct from application source
  repos.
- **Environment:** A named deployment context (e.g. `dev`, `staging`, `prod`),
  represented in the config repo by a path, branch, or tag (§7.2).
- **Config path:** The subdirectory within the config repo that a given workload
  consumes (e.g. `services/checkout/prod`).
- **Config version:** An immutable identifier for a delivered configuration —
  in practice a git commit SHA (optionally plus the resolved path digest).
- **Agent (`kohen-agent`):** The workload-side component that fetches and
  materializes config into a volume and drives the reload contract. Runs as an
  init container and/or sidecar.
- **Operator (`kohen-operator`):** The cluster-side controller that reconciles
  Kohen CRDs and (optionally) injects the agent into workloads.
- **Sync:** One cycle of fetch → resolve → materialize → signal.
- **Reload contract:** The agreed mechanism by which the application picks up a
  new config version (file watch, signal, endpoint, or rollout).
- **Consistency target:** The named config version the fleet should converge to.

---

## 6. System Architecture

Kohen has two cooperating components plus a well-defined repository model.

```
                    ┌──────────────────────────────────────────────┐
                    │              Config git repository            │
                    │   services/checkout/{dev,staging,prod}/...    │
                    └───────────────┬──────────────────────────────┘
                                    │ fetch (poll or webhook), read-only
                                    ▼
┌───────────────────────────────────────────────────────────────────────────┐
│ Kubernetes cluster                                                          │
│                                                                             │
│   ┌───────────────────────┐        watches CRDs / injects agent            │
│   │   kohen-operator      │◄──────────────────────────────────────────┐    │
│   │  (Deployment)         │                                            │    │
│   │  - reconciles CRDs    │        publishes ConfigRelease / status    │    │
│   └──────────┬────────────┘                                            │    │
│              │                                                          │    │
│              ▼ (optional) mutating webhook injects init + sidecar       │    │
│   ┌─────────────────────────────────────────────────────────────┐      │    │
│   │  Application Pod                                              │      │    │
│   │                                                              │      │    │
│   │   [init] kohen-agent  ── materialize vN ──► emptyDir volume  │      │    │
│   │                                                    ▲          │      │    │
│   │   [sidecar] kohen-agent ── watch/poll ── atomic swap         │      │    │
│   │                    │  signal (SIGHUP / file / http)          │      │    │
│   │                    ▼                                         │      │    │
│   │   [app] main container  ◄── reads config volume ─────────────┘      │    │
│   └─────────────────────────────────────────────────────────────┘           │
└───────────────────────────────────────────────────────────────────────────┘
```

### 6.1 `kohen-agent` (workload-side)

- Runs **as an init container** to guarantee config presence before app start
  (UC1), and/or **as a sidecar** to keep it fresh (UC2/UC3).
- Fetches the configured path from the config repo at the configured ref.
- Materializes config into a shared volume (recommended: `emptyDir`) using an
  **atomic swap** (§8).
- Executes the **reload contract** (§10).
- Exposes health and metrics endpoints.
- MUST be able to operate **standalone** (configured purely via flags/env and a
  Kubernetes `Secret` for credentials) so that Kohen is usable without the
  operator for simple cases.

### 6.2 `kohen-operator` (cluster-side)

- Reconciles Kohen **CRDs** (§11).
- Resolves refs to concrete commit SHAs and publishes the **consistency target**
  so all replicas of a workload converge to the same version.
- OPTIONALLY runs a **mutating admission webhook** that injects the agent
  (init + sidecar) and volumes into workloads based on annotations/label
  selectors, so application authors do not edit pod specs by hand.
- Records Kubernetes **Events** and updates CRD **status** with the observed
  config version per workload for auditability (UC6).
- OPTIONALLY receives git provider **webhooks** to trigger near-immediate syncs
  instead of relying solely on polling.

> The operator is OPTIONAL for basic file-delivery use cases but REQUIRED for
> fleet-wide consistency coordination, auto-injection, and webhook-driven syncs.

---

## 7. Configuration Repository Model

### 7.1 Requirements

- **R7.1** A config repo MUST be a standard git repository reachable over HTTPS
  or SSH.
- **R7.2** A workload MUST be able to select a **subdirectory** (config path) so
  a single repo can serve many services.
- **R7.3** The model MUST support multiple environments without duplicating the
  config payload across manifests.

### 7.2 Environment selection strategies (all MUST be supported)

1. **Path-based (default, recommended):** environments are subdirectories, e.g.
   `services/checkout/dev`, `.../prod`. Single branch (`main`), simplest review.
2. **Branch-based:** each environment is a branch (`env/dev`, `env/prod`).
   Enables environment-specific promotion flows.
3. **Tag/ref-based pinning:** a workload MAY pin to a tag or explicit commit SHA
   for immutable, auditable releases and rollbacks.

The selected strategy MUST be explicit per workload (no implicit magic).

### 7.3 Layout conventions (recommended, not enforced)

```
config-repo/
├── services/
│   └── checkout/
│       ├── base/                 # shared defaults across environments
│       │   └── app.yaml
│       ├── dev/
│       │   └── app.yaml          # overrides / full values for dev
│       ├── staging/
│       └── prod/
└── kohen.yaml                    # optional repo-level metadata/policy
```

### 7.4 Optional light templating / overlay (v1 = optional, may defer)

- Kohen MAY support a `base` + `<env>` **overlay/merge** (deep-merge of YAML/JSON)
  so shared defaults live once. If implemented, merge semantics MUST be
  deterministic and documented.
- Kohen MAY support simple variable substitution from a bounded, explicit set of
  inputs (e.g. cluster name, environment name, pod metadata). Turing-complete
  templating is a **non-goal** (§3.2 N4).
- If neither is enabled, Kohen delivers files **verbatim**.

### 7.5 Repository-level policy (`kohen.yaml`, optional)

The repo MAY declare policy the operator enforces, e.g. required commit signing,
allowed consumer namespaces, or a schema reference for validation. Enforcement
of repo policy is a v2 goal; v1 MUST at least ignore unknown fields gracefully.

---

## 8. Consistency & Atomicity Model

This is the core differentiator and MUST be specified precisely.

### 8.1 Atomic materialization (per replica)

- **R8.1** A sync MUST never expose a partially written config tree to the
  application. Implementations MUST write the new version to a staging location
  and switch atomically (recommended: write to `.../vN` and flip a `current`
  symlink; the app reads through `current`).
- **R8.2** A failed or interrupted sync MUST leave the previously good version in
  place (no truncation, no empty directory).
- **R8.3** The delivered tree MUST be identifiable by an immutable **config
  version** (git commit SHA; if overlays/templating are used, the version MUST
  also incorporate a digest of the resolved output).

### 8.2 Fleet consistency (across replicas)

- **R8.4** For a given workload, Kohen MUST provide a mechanism to converge all
  replicas to a single **consistency target** version rather than letting each
  replica independently resolve a moving ref.
  - The operator resolves the tracked ref (e.g. `main`) to a concrete commit and
    publishes it as the target (e.g. via a `ConfigRelease` object / CRD status /
    a projected value). Agents fetch **that pinned commit**, not the moving ref.
- **R8.5** The system MUST expose, per workload, the **currently targeted**
  version and the **observed** version per replica, so convergence is
  measurable (UC6).
- **R8.6** Rollout of a new consistency target SHOULD be **controllable**: at
  minimum immediate; SHOULD support a documented ordering so that hot-reload and
  restart-based workloads converge predictably. Advanced strategies (canary
  percentage, soak windows) are v2 goals.

### 8.3 Rollback

- **R8.7** Setting the consistency target to a previous version MUST cause the
  fleet to converge back to that version using the same delivery + reload
  contract as a forward change (UC5).

---

## 9. Security Requirements

- **R9.1 — Least privilege git access.** The agent MUST require only read access
  to the config repo. Credentials (SSH key, HTTPS token, or app installation
  token) MUST be sourced from Kubernetes `Secret`s, never baked into images or
  CRDs in plaintext.
- **R9.2 — Credential scope.** Per-workload or per-namespace credential scoping
  MUST be supported so one team's workload cannot read another team's config repo
  path via shared credentials, subject to git provider capabilities.
- **R9.3 — Host key verification.** For SSH, known-hosts / host key verification
  MUST be supported and enabled by default (no blind trust-on-first-use in prod).
- **R9.4 — TLS verification.** For HTTPS, server certificate verification MUST be
  on by default; disabling it MUST require an explicit, logged opt-in.
- **R9.5 — Commit verification (SHOULD).** Kohen SHOULD support verifying signed
  commits/tags (GPG/SSH signatures) against an allow-list of keys before a
  version becomes a consistency target, to defend against tampering.
- **R9.6 — Secret material handling.** If Kohen delivers secret material, it MUST
  set restrictive file permissions, prefer `tmpfs`/`emptyDir` with `medium:
  Memory` for secret volumes, and MUST NOT log secret contents. Kohen SHOULD
  integrate with existing secret stores rather than storing plaintext secrets in
  the config repo.
- **R9.7 — RBAC.** The operator and agent MUST ship with least-privilege RBAC.
  The agent MUST NOT require cluster-admin. Namespaced installation MUST be
  supported.
- **R9.8 — Supply chain.** Released container images MUST be built reproducibly
  in CI, published with an immutable digest, and SHOULD be signed (e.g. cosign)
  with an SBOM attached.
- **R9.9 — Multi-tenancy isolation.** In shared clusters, a workload MUST only be
  able to consume config paths/repos it is authorized for; the operator MUST
  enforce namespace/selector allow-lists where configured.
- **R9.10 — Webhook security.** Any inbound git provider webhook endpoint MUST
  verify request signatures/secrets and MUST be resilient to spoofed/replayed
  events (a webhook only *triggers* a fetch; trust derives from the fetched,
  pinned commit, never from webhook payload contents).

---

## 10. Application Reload Contract

Applications differ in how they can absorb config changes; Kohen MUST support a
menu of contracts, selectable per workload. Exactly one primary contract applies
per workload.

- **C1 — Volume + file watch (recommended for reload-capable apps).** Kohen
  atomically swaps the config volume; the app watches files and re-reads. Kohen's
  responsibility ends at the atomic swap.
- **C2 — Signal.** After a swap, Kohen sends a configured signal (default
  `SIGHUP`) to the application process so it re-reads config. Requires the agent
  to be able to signal the app (shared process namespace or a documented
  mechanism).
- **C3 — HTTP/exec hook.** After a swap, Kohen calls a configured local endpoint
  (e.g. `POST /-/reload`) or executes a configured command. A hook failure MUST
  be reported and MUST NOT corrupt the on-disk version.
- **C4 — Rollout (for apps that cannot reload).** The operator triggers a
  controlled rolling restart of the workload pinned to the new consistency
  target (e.g. by updating a pod-template annotation carrying the config
  version). New pods start (via the init agent) already on the new version.

Requirements:

- **R10.1** The chosen contract MUST be explicit and documented per workload.
- **R10.2** For C1–C3, the config version on disk MUST be updated atomically
  *before* the reload signal/hook fires.
- **R10.3** Reload success/failure MUST be observable (metric + event/log).
- **R10.4** A reload/hook failure MUST NOT roll back the on-disk version silently;
  behavior on failure (retry, hold, alert) MUST be documented and configurable.

---

## 11. Kubernetes API (CRDs)

The operator MUST expose declarative resources. Field names below are indicative;
the API MUST be versioned (`v1alpha1` → `v1beta1` → `v1`) and follow Kubernetes
API conventions (status subresource, `observedGeneration`, printer columns).

### 11.1 `ConfigSource` (cluster- or namespace-scoped)

Describes a config repo and how to reach it.

```yaml
apiVersion: kohen.dev/v1alpha1
kind: ConfigSource
metadata:
  name: platform-config
  namespace: checkout
spec:
  url: https://github.com/acme/platform-config.git
  auth:
    secretRef: { name: kohen-git-creds }     # ssh key or https token
  interface: https                            # https | ssh
  verification:                               # optional (R9.5)
    requireSignedCommits: true
    allowedSignersSecretRef: { name: kohen-allowed-signers }
status:
  conditions: [ ... ]
  lastPolledCommit: 9f1c2ab
```

### 11.2 `ConfigBinding` (namespaced)

Binds a workload to a config path/environment and defines the reload contract.

```yaml
apiVersion: kohen.dev/v1alpha1
kind: ConfigBinding
metadata:
  name: checkout-prod
  namespace: checkout
spec:
  sourceRef: { name: platform-config }
  selectMode: path                            # path | branch | ref
  path: services/checkout                     # base config path
  environment: prod                           # resolves to path/branch/ref per selectMode
  ref: main                                   # tracked ref (path/branch modes); or pinned SHA/tag
  target:
    workloadRef:                              # which workload consumes this
      apiVersion: apps/v1
      kind: Deployment
      name: checkout
    mountPath: /etc/appconfig
    volume: { type: emptyDir }
  sync:
    interval: 30s                             # poll cadence; webhooks may accelerate
  reload:
    contract: signal                          # fileWatch | signal | httpHook | execHook | rollout
    signal: SIGHUP
  overlay:                                    # optional (§7.4)
    enabled: true
    baseDir: services/checkout/base
status:
  targetVersion: 9f1c2ab                      # consistency target (R8.4)
  observedReplicas:                           # per-replica observed version (R8.5)
    - pod: checkout-abc  version: 9f1c2ab
    - pod: checkout-def  version: 9f1c2ab
  conditions: [ ... ]
```

### 11.3 `ConfigRelease` (namespaced, operator-managed, optional)

Represents an immutable, resolved consistency target for a binding (the pinned
commit + resolved output digest). Useful for audit history and rollback (UC5/UC6).

Requirements:

- **R11.1** CRDs MUST validate inputs via OpenAPI schema / CEL where possible.
- **R11.2** Status MUST reflect the current consistency target and per-replica
  observed versions.
- **R11.3** Auto-injection MUST be opt-in per workload (annotation or selector),
  never global-by-default.
- **R11.4** Deleting a `ConfigBinding` MUST NOT delete application workloads; it
  stops syncing and (for injected setups) reverts injection on next rollout.

---

## 12. Technical Requirements

- **T1 — Language & runtime.** Components MUST be implemented in **Go**
  (consistent with the existing `kohen-agent` prototype and the Kubernetes
  ecosystem). Target Go version MUST be current stable (≥ 1.23 at time of
  writing).
- **T2 — Git access.** Git operations MUST use a maintained Go git
  implementation (the prototype uses `go-git`). Shallow fetches and
  fetch-by-commit MUST be used to minimize bandwidth and enforce pinning (R8.4).
- **T3 — Kubernetes compatibility.** MUST support the **N-2** range of currently
  supported upstream Kubernetes minor versions. The operator SHOULD be built with
  `controller-runtime`.
- **T4 — Distribution.**
  - Components MUST be published as minimal, multi-arch (`amd64`, `arm64`)
    container images (distroless/static base preferred).
  - Installation MUST be available via a **Helm chart** and MUST also be possible
    via plain manifests.
  - The agent MUST also be runnable **standalone** (flags/env only) for the
    no-operator path (§6.1).
- **T5 — Configuration surface.** The agent MUST accept configuration via CLI
  flags and environment variables (the prototype already uses `--gitUrl`,
  `--gitPath`, `--targetDir` / `KOHEN_GIT_URL`, `KOHEN_GIT_PATH`,
  `KOHEN_TARGET_DIR`); the operator via CRDs and a controller config file.
- **T6 — Resource footprint.** Agent memory/CPU footprint MUST be small enough to
  run as a sidecar in every pod (target: idle < 32 MiB RAM, negligible idle CPU;
  MUST be documented and tested).
- **T7 — Volumes.** Recommended shared volume is `emptyDir`; secret material MUST
  use memory-backed volumes (R9.6). The agent MUST tolerate read-only root
  filesystems and run as non-root.
- **T8 — Failure isolation.** A config backend (git) outage MUST NOT crash or
  restart the application container. The agent MUST keep serving the
  last-good version and surface degraded status (see §14).
- **T9 — Idempotency & determinism.** Given the same input commit + overlay
  inputs, materialized output MUST be byte-identical (deterministic ordering,
  stable merges) so config versions are reproducible.
- **T10 — Concurrency & scale.** The operator MUST reconcile many
  `ConfigBinding`s efficiently (work queues, rate limiting, shared informers) and
  MUST bound git fetch concurrency to protect the git backend.

---

## 13. Failure Modes & Resilience

| Failure | Required behavior |
| --- | --- |
| Git unreachable at **init** | Retry with backoff up to a deadline. If a previously cached version exists (via a persistent cache), MAY start from it; otherwise fail init and let the pod's restart policy retry. MUST log clearly and emit an event. |
| Git unreachable at **sidecar** runtime | Keep serving last-good version (T8). Mark binding/agent **degraded**. Never delete or truncate current config. |
| Auth failure | Fail fast with actionable error; emit event; MUST NOT retry so aggressively as to lock the account. Redact secrets from logs. |
| Malformed / missing config path | Do not swap to a bad version. Keep last-good, mark degraded, emit event. |
| Overlay/template error | Same as malformed path: no swap, degraded, event. |
| Reload hook/signal failure | Config on disk stays at new version (already swapped); report failure per R10.3/R10.4; apply configured failure policy. |
| Signature verification failure (R9.5) | Version MUST NOT become a consistency target; emit security event. |
| Operator down | Existing pinned targets keep being served by agents; no new targets are published until operator recovers. Agents MUST degrade gracefully, not thrash. |
| Partial fleet convergence | Status MUST show divergence (R8.5); MUST NOT silently claim success. |

- **R13.1** Retries MUST use bounded exponential backoff with jitter.
- **R13.2** All failure states MUST be represented in status/conditions and
  metrics, not only in logs.

---

## 14. Observability

- **R14.1 — Metrics.** Both components MUST expose Prometheus metrics, including
  at minimum: sync attempts/successes/failures, sync duration, current served
  config version (as a labeled gauge/info metric), reload attempts/outcomes,
  fleet convergence (targeted vs observed), git fetch latency/errors, and
  degraded-state gauges.
- **R14.2 — Logging.** Structured (JSON) logging with configurable levels.
  Secrets MUST never be logged (R9.6).
- **R14.3 — Health.** The agent MUST expose readiness/liveness endpoints.
  Readiness MUST reflect "has a valid config version materialized"; liveness MUST
  NOT flap due to git backend outages (T8).
- **R14.4 — Kubernetes Events.** The operator MUST emit events for target
  changes, convergence, and failures.
- **R14.5 — Auditability.** For any workload it MUST be possible to answer "which
  config commit is each replica currently serving, and when did it change" from
  status + metrics + events (UC6).
- **R14.6 — Tracing (SHOULD).** Sync operations SHOULD support OpenTelemetry
  tracing.

---

## 15. Non-Functional Requirements

- **NFR1 — Reliability.** Kohen MUST fail safe: its own unavailability degrades
  *freshness of config*, never the *availability of healthy application pods*.
- **NFR2 — Performance.** Steady-state sync SHOULD detect and deliver a change
  within a bounded, configurable window (default target: ≤ 60 s via polling;
  faster with webhooks). Atomic swap of a typical config tree SHOULD complete in
  well under 1 s.
- **NFR3 — Scalability.** MUST scale to hundreds of `ConfigBinding`s and hundreds
  of workloads per operator instance without overloading the git backend
  (dedup fetches by repo+commit, cache).
- **NFR4 — Compatibility.** MUST work with mainstream git providers (GitHub,
  GitLab, Bitbucket, self-hosted) over HTTPS and SSH. MUST NOT require a specific
  provider's proprietary API for core functionality (webhooks are an optional
  accelerator).
- **NFR5 — Portability.** MUST run on any conformant Kubernetes distribution and
  on both `amd64` and `arm64`.
- **NFR6 — Usability.** A developer MUST be able to onboard a service (basic path
  mode, file-watch reload) with a single `ConfigBinding` and a credentials
  `Secret`. Getting-started docs MUST demonstrate this end to end.
- **NFR7 — Documentation.** MUST include: concepts, install (Helm + manifests),
  security hardening guide, CRD reference, agent flag/env reference, reload
  contract cookbook, and troubleshooting.
- **NFR8 — Licensing.** Project MUST ship under a permissive OSI-approved license
  (recommended: Apache-2.0) with a clear contribution guide and code of conduct.
- **NFR9 — Testing.** MUST include unit tests, integration tests against a real
  git server, and end-to-end tests on an ephemeral cluster (e.g. `kind`),
  including the atomic-swap and consistency guarantees and the failure modes in
  §13. CI MUST gate merges on these.
- **NFR10 — Versioning & compatibility policy.** MUST follow SemVer for released
  artifacts and a documented CRD deprecation policy. Breaking CRD changes require
  a new API version with conversion.

---

## 16. Configuration & CLI Reference (indicative)

### 16.1 `kohen-agent` (standalone / injected)

| Flag | Env | Default | Description |
| --- | --- | --- | --- |
| `--git-url` | `KOHEN_GIT_URL` | — (required) | Config repo URL (HTTPS/SSH). |
| `--git-path` | `KOHEN_GIT_PATH` | `""` | Subdirectory / config path within the repo. |
| `--git-ref` | `KOHEN_GIT_REF` | `main` | Tracked ref, or pinned commit/tag. |
| `--target-dir` | `KOHEN_TARGET_DIR` | — (required) | Directory to materialize config into (via `current` symlink). |
| `--mode` | `KOHEN_MODE` | `init` | `init` (one-shot) or `sidecar` (continuous). |
| `--interval` | `KOHEN_INTERVAL` | `30s` | Sidecar poll interval. |
| `--reload` | `KOHEN_RELOAD` | `none` | `none｜signal｜http｜exec`. |
| `--reload-signal` | `KOHEN_RELOAD_SIGNAL` | `SIGHUP` | Signal for `signal` mode. |
| `--reload-target` | `KOHEN_RELOAD_TARGET` | — | Endpoint/command for `http`/`exec` mode. |
| `--auth-*` | `KOHEN_AUTH_*` | — | Credential source (mounted secret path/token). |
| `--metrics-addr` | `KOHEN_METRICS_ADDR` | `:9095` | Metrics/health bind address. |
| `--verify-signed` | `KOHEN_VERIFY_SIGNED` | `false` | Require verified signed commits. |

> The three existing prototype flags (`--gitUrl`, `--gitPath`, `--targetDir`) map
> onto `--git-url`, `--git-path`, `--target-dir`. The spec standardizes on
> kebab-case; the implementation SHOULD accept the legacy names as aliases for a
> deprecation window.

### 16.2 `kohenctl` (optional helper CLI, SHOULD)

- `kohenctl status <binding>` — show target vs observed versions (UC6).
- `kohenctl diff <binding>` — show config diff between served and latest version.
- `kohenctl pin <binding> <sha>` / `kohenctl rollback <binding>` — set target
  (UC5).
- `kohenctl verify <repo>` — validate signatures / layout locally.

---

## 17. Milestones / Phased Roadmap

Ordered by dependency, not by calendar. Each milestone is independently
shippable and testable.

- **M0 — Spec & foundations (this document).** Agreed requirements, glossary,
  architecture. Repo scaffolding, license, CI skeleton.
- **M1 — Agent MVP (standalone).** `kohen-agent` init + sidecar modes, atomic
  swap (R8.1–R8.3), HTTPS/SSH auth from secrets (R9.1–R9.4), file-watch + signal
  reload contracts (C1, C2), metrics/health (§14), fail-safe behavior (T8, §13).
  Delivers UC1, UC2 without the operator.
- **M2 — Operator & CRDs.** `ConfigSource` + `ConfigBinding`, ref→commit
  resolution and consistency target publication (R8.4–R8.5), status/events,
  Helm chart. Delivers fleet consistency, UC4, UC6.
- **M3 — Reload completeness & rollout.** HTTP/exec hooks (C3), rollout contract
  (C4) with version-pinned restarts, rollback (UC5, R8.7).
- **M4 — Injection & webhooks.** Mutating webhook auto-injection (R11.3),
  git-provider webhook-triggered syncs (R9.10), `kohenctl`.
- **M5 — Advanced.** Overlays/light templating (§7.4), signed-commit verification
  (R9.5), repo policy enforcement (§7.5), progressive rollout strategies (R8.6),
  tracing (R14.6), image signing/SBOM (R9.8).

---

## 18. Acceptance Criteria (definition of done per capability)

- **A1** Deploying a `ConfigBinding` (M2) results in the referenced workload
  having the correct environment's config on disk before its main container is
  ready, verified in an e2e `kind` test.
- **A2** Committing a change to the tracked ref causes all replicas to converge
  to the new config version within the configured window, with status showing
  100% convergence, and the app observing the change via its reload contract.
- **A3** An interrupted/failed sync never exposes partial config; the previous
  version remains readable (fault-injection test).
- **A4** A git outage during sidecar operation does not restart or fail the app
  container; status shows `Degraded`; recovery re-converges automatically.
- **A5** Setting the target to a prior commit rolls the fleet back to that
  version using the same delivery path.
- **A6** For any running workload, an operator can determine the exact config
  commit each replica serves via `status`/`kohenctl status`.
- **A7** The agent runs as non-root on a read-only root filesystem within the
  documented resource footprint (T6, T7).
- **A8** No secret values appear in logs under any tested failure mode (R9.6).

---

## 19. Open Questions

1. **Persistent cache for init resilience.** Should agents share a
   node-local/persistent cache so an init container can start from last-good
   config during a total git outage? Trade-off: resilience vs. staleness/attack
   surface. (Affects §13 init row.)
2. **Consistency coordination transport.** Publish the consistency target via a
   dedicated `ConfigRelease` CRD, `ConfigBinding` status, or a projected
   in-cluster artifact the agent reads? Impacts operator↔agent coupling.
3. **Env-var-sourced config.** Some apps only read env vars, which cannot live
   update. Do we officially scope such apps to the rollout contract (C4) only, or
   provide an env-injection shim?
4. **Overlay engine scope.** How much merge/templating is "light enough" to stay
   within non-goal N4 while being genuinely useful?
5. **Multi-repo bindings.** Should a single workload be able to compose config
   from more than one `ConfigSource`? If so, define precedence/merge order.
6. **Secret delivery boundary.** Exactly where does Kohen stop and
   External-Secrets/Vault begin (N2)? Define the supported integration seam.
7. **Webhook ingress topology.** For webhook-driven syncs, what is the supported
   ingress/relay model in restricted-egress clusters?

---

## 20. Relationship to the Existing Prototype

The repository contains an early `kohen-agent` prototype (a Go program using
`go-git` that clones a repo into a target directory via `--gitUrl` / `--gitPath`
/ `--targetDir`). It validates the basic "fetch config from git into a workload
directory" premise and maps directly onto the **init-container** portion of the
**M1** agent. The revived implementation extends it toward this spec: atomic
swaps, sidecar/continuous mode, reload contracts, secure auth, observability, and
(via the operator) multi-environment selection and fleet-wide consistency. The
prototype's flags are retained as documented aliases (§16.1).

---

## Appendix A — Requirement Index

Requirements are labeled inline (`Rn.n`, `Tn`, `Cn`, `An`, `Gn`, `Nn`, `NFRn`,
`UCn`) so implementation PRs and tests can reference them directly. An
implementation is considered spec-conformant for a given milestone when all
requirements reachable from that milestone's capabilities (§17) and their
acceptance criteria (§18) are satisfied and demonstrated by automated tests
(NFR9).
