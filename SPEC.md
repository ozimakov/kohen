# KOHEN — Specification

> Status: **Draft v0.2** · Owner: Kohen maintainers · Last updated: 2026-07-04
>
> This document is the single source of truth for **what** Kohen is, **why** it
> exists, and **which requirements** any implementation must satisfy. It is
> intentionally implementation-light: it describes behavior, contracts, and
> constraints rather than prescribing internal code structure. Where a concrete
> technology is named, it is a requirement; where an approach is named, it is a
> recommendation and is marked as such.

---

## 1. Overview

**Kohen** delivers **configuration *file trees*** from a dedicated `git`
repository into Kubernetes workloads that **read config from the filesystem and
hot-reload it** — Envoy, NGINX, HAProxy, Fluent Bit / Fluentd / Vector,
Prometheus / Alertmanager, Loki, Tempo, CoreDNS, Telegraf, and similar
file-driven daemons. For those workloads Kohen delivers each new version
**atomically**, drives a **reload**, and guarantees the **whole fleet converges
to one committed config version** with per-replica proof of what is served.

Kohen is deliberately **narrow**. It is *not* a general-purpose Kubernetes config
manager, *not* a GitOps engine, and *not* a feature-flag system. It does one
thing that today requires hand-built glue: get a directory of config files from
git onto disk, consistently and atomically, and make a file-driven process pick
it up — without a full redeploy and without the ~1 MiB / flat-key limits of a
`ConfigMap`.

> If your app reads config from **environment variables** or a single small
> key/value map, and a pod restart on change is acceptable, **you do not need
> Kohen** — use a `ConfigMap` plus [Stakater Reloader], or feature flags. See
> §2.4 (*When NOT to use Kohen*). Kohen earns its place only for **file-tree +
> hot-reload** workloads where atomicity and fleet consistency actually matter.

### 1.1 One-sentence definition

> Kohen atomically delivers a versioned directory of configuration files from a
> dedicated git repository into file-driven Kubernetes workloads, guarantees the
> whole fleet converges to the same committed version, and drives an in-place
> reload — no redeploy, no `ConfigMap` size/shape limits.

### 1.2 The concrete scenario Kohen is built for

A team runs a fleet of **Envoy** (or NGINX / Fluent Bit / Prometheus) pods. Their
config is a **tree of files** — `envoy.yaml`, route tables, filter snippets, TLS
material references — that is too large and too structured for a `ConfigMap`, and
the process **hot-reloads** on `SIGHUP` or an admin endpoint. They keep this
config in a **dedicated git repo** with `dev`/`staging`/`prod` variants so changes
are reviewed and audited. Today they bolt together `git-sync` + a reload
script + a ConfigMap hash annotation, and they still cannot answer *"is every
replica actually on commit `abc123` right now?"* or guarantee a half-written tree
is never read. **Kohen is the purpose-built answer to exactly this scenario.**

Outside this scenario, Kohen is intentionally not the right tool.

[Stakater Reloader]: https://github.com/stakater/Reloader

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
are a poor fit for delivering a *file tree of application config to a file-driven
daemon that hot-reloads* because:

- Every config edit becomes a reconcile of Kubernetes objects and, typically, a
  workload rollout — there is no in-place hot reload of the running process.
- The application still cannot pull a specific config directory tree from a
  dedicated repo into its own filesystem with atomic swaps.
- GitOps does not provide an application-facing reload contract, and config
  larger than a `ConfigMap` still has nowhere to go.

Kohen is **complementary**: use GitOps for what runs, use Kohen for what running
things read.

### 2.3 Prior art / positioning

| Approach | What it does | Why it is not enough *for the file-tree + hot-reload case* |
| --- | --- | --- |
| `ConfigMap`/`Secret` volumes | Native k8s config units | ~1 MiB + flat-key limits; awkward for a directory tree; no git source of truth; no reload signal; kubelet-sync updates are not atomic per-tree and give no cross-replica version |
| Stakater **Reloader** | Restarts pods when a `ConfigMap`/`Secret` changes | Restart-based only (no *hot* reload); still bounded by ConfigMap limits; no git source of truth; no atomic file-tree swap |
| **`git-sync`** sidecar | Syncs a git repo into a volume | No environment model, no fleet consistency guarantee, no reload contract, minimal k8s integration — you still build the reload + version-tracking glue yourself |
| GitOps (Argo CD, Flux) | Reconcile cluster desired state from git | Heavyweight for app config; no in-container file-tree delivery + hot-reload; a config edit becomes an object reconcile/rollout |
| Feature-flag platforms (LaunchDarkly, Unleash, Flagsmith, OpenFeature) | Runtime toggles/values via SDK | Requires app code changes + a client library; not for delivering *config files* to off-the-shelf daemons (Envoy/NGINX/Fluent Bit) |
| Spring Cloud Config / Consul / Vault | Config server apps query at runtime | Requires a running service + client library; not git-file-native or k8s-file-native by default |
| Bespoke init scripts | Clone config at startup | One-shot only; no live updates; no atomicity/consistency; per-team reinvention |

Kohen's distinguishing combination, **scoped to file-driven, hot-reloadable
workloads**: git-native delivery of a **whole directory tree** + **atomic swap** +
an **application reload contract** + **fleet-wide version consistency** with
per-replica proof + declarative Kubernetes integration. No single tool above
provides that combination; Kohen exists to avoid gluing three of them together.

### 2.4 When NOT to use Kohen (and what to use instead)

Kohen is intentionally narrow. Reach for something else when:

- **Your app reads config from environment variables.** Env vars cannot change in
  a running process. Use a `ConfigMap`/`Secret` + **Reloader** (restart on
  change), or move the value behind a feature flag. Kohen will **not** inject env
  vars in v1 (N6).
- **Your config is a handful of small key/value settings and a restart on change
  is fine.** A `ConfigMap` (optionally + Reloader) is simpler and has no per-pod
  footprint. Don't add a sidecar for this.
- **You want to toggle product behavior / run experiments.** Use a feature-flag
  platform (OpenFeature-compatible). That space is solved and SDK-driven.
- **You want to deploy/reconcile Kubernetes objects from git.** That's GitOps
  (Argo CD / Flux). Kohen delivers what running things *read*, not what runs.
- **You need a secrets manager.** Use External Secrets Operator / Vault / Sealed
  Secrets. Kohen *references* secrets those tools manage (see §9A) but does not
  store or manage secret material itself.

Kohen is the right tool when **all** of these hold:

1. The workload reads configuration from the **filesystem** (a file or a
   directory tree), not solely from env vars.
2. The workload can **reload in place** (signal, admin endpoint, or built-in file
   watch) — or you explicitly accept the rollout contract (C4) for it.
3. The config is genuinely **file-tree shaped** and/or **exceeds `ConfigMap`
   practicality** (size, nesting, many files), and/or you need **atomic swaps**
   and **provable fleet-wide version consistency**.
4. The source of truth is a **dedicated git repo**, ideally spanning multiple
   environments.

If two or more of these are false, Kohen is probably the wrong tool, and the
README/docs MUST say so plainly (NFR6).

---

## 3. Goals & Non-Goals

### 3.1 Goals

- **G1** Deliver a **directory tree** of configuration from a dedicated git
  repository onto the filesystem of a **file-driven, reload-capable** Kubernetes
  workload, free of `ConfigMap` size/shape limits.
- **G2** Support a **multi-environment** repository model (dev/staging/prod, and
  optionally region/tenant) selected declaratively per workload.
- **G3** Keep configuration **current**: detect committed changes and apply them
  to running workloads without requiring a manual redeploy.
- **G4** Guarantee **consistency**: updates are atomic per replica (no partially
  written config tree is ever visible) and the fleet converges to a single named
  config version with per-replica proof of the served version.
- **G5** Provide a clear **application reload contract** (in-place file watch,
  signal, admin endpoint/exec hook, or controlled rollout) so apps actually pick
  up changes — with in-place reload as the primary, first-class path.
- **G6** Be **Kubernetes-native**: declarative API (CRDs), optional automatic
  injection, standard RBAC, metrics, and events.
- **G7** Be **secure by default**: least-privilege git access, first-class
  **secret *references*** resolved from native `Secret`s or External Secrets
  (§9A) so plaintext secrets never live in the config repo, and optional commit
  verification.
- **G8** Be **operable**: observable, debuggable, and safe to fail (a config
  backend outage must not take down healthy workloads).
- **G9** Keep the **adoption cost low**: an operator-driven path with **no
  per-pod sidecar** MUST exist so Kohen can compete with sidecar-free tools; the
  sidecar is opt-in for the hot-reload case (§6, §10).

### 3.2 Non-Goals

- **N1** Kohen is **not** a GitOps/CD engine and will not reconcile arbitrary
  Kubernetes manifests or perform application deployments.
- **N2** Kohen is **not** a general-purpose secrets manager and does **not** store
  or manage secret material. It **resolves references** to secrets owned by
  native `Secret`s or External Secrets Operator / Vault / Sealed Secrets (§9A); it
  does not replace them.
- **N3** Kohen does **not** own the *schema* or *validation semantics* of an
  application's configuration beyond optional structural checks; the application
  remains responsible for interpreting its config.
- **N4** Kohen is **not** a templating/rendering platform competitor to
  Helm/Kustomize. Light templating MAY be supported (§7.4) but complex
  rendering pipelines are out of scope for v1.
- **N5** Kohen does not provide a UI in v1 (CLI + Kubernetes API only).
- **N6** Kohen does **not** deliver configuration as **environment variables** in
  v1. Env vars cannot update in a running process and are the domain of
  `ConfigMap`/`Secret` + restart tooling. Kohen targets filesystem config only
  (§2.4). Env-var use cases are steered to the rollout contract (C4) or out of
  scope.
- **N7** Kohen is **not** a general-purpose config manager for arbitrary apps.
  Apps that cannot read config from files or cannot reload are explicitly out of
  Kohen's sweet spot and are served (if at all) only by the coarse rollout
  contract (C4).

---

## 4. Personas & Use Cases

### 4.1 Personas

- **Data-plane / infra service owner (Dana).** Owns a fleet of file-driven
  daemons (e.g. Envoy edge proxies, NGINX gateways, Fluent Bit log shippers,
  Prometheus/Alertmanager). Wants to change a routing table, filter, scrape
  config, or pipeline by opening a PR against the config repo and have every
  replica hot-reload the new version without a redeploy — and be able to prove
  the whole fleet converged.
- **Platform / SRE engineer (Sam).** Operates the cluster. Wants a standard,
  auditable, secure, low-footprint mechanism for delivering config file trees,
  with metrics and safe failure modes, offered as a self-service capability —
  without adding a heavy sidecar to every pod in the cluster.
- **Security / compliance reviewer (Riley).** Wants config changes to be
  reviewed, attributable to a commit/author, verifiable (signed), access to the
  config repo to be least-privilege, and **no plaintext secrets in the config
  repo** — secrets must be *references* resolved from an approved secret store.

### 4.2 Primary use cases

- **UC1 — Startup delivery.** On pod start, the correct environment's config tree
  is present on disk before the application's main container starts.
- **UC2 — Live update, hot reload (primary).** A merged config change is delivered
  to running pods and the file-driven application reloads the new tree in place
  without a restart.
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
- **UC7 — Secret references.** A config file references a secret (e.g. an upstream
  credential or TLS key) by name; Kohen resolves it from a native `Secret` or via
  External Secrets and materializes the secret material into the workload —
  without the plaintext ever being committed to the config repo (§9A).

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
- **Secret reference:** A pointer, stored in the config (never the value), to
  secret material owned by a native `Secret` or an external secret tool, which
  Kohen resolves inside the cluster at materialization time (§9A).
- **`SecretMapping`:** The declarative mapping of secret references to sources and
  projection targets, declared in-repo or in the `ConfigBinding` (§9A.3).
- **File-driven workload:** An application/daemon that reads its configuration
  from files on disk and can reload it in place (Kohen's primary target).

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

### 6.3 Deployment topologies

Kohen MUST support two topologies so adoption cost matches the use case (G9):

- **Topology A — In-pod agent (primary; for hot-reload + large trees).** Init +
  sidecar `kohen-agent` deliver the config tree to a shared volume and drive an
  **in-place reload** (C1–C3). This is the only topology that can hot-reload a
  file-driven daemon without a restart, and it is unconstrained by `ConfigMap`
  size/shape limits. It carries a per-pod footprint (T6), justified precisely
  because no sidecar-free tool can do in-place file-tree reload.
- **Topology B — Operator-projected, sidecar-free (low footprint; for the
  rollout contract).** The operator fetches, resolves (including secret
  references, §9A), and **projects** the resolved config into the pod via a
  Kubernetes object (e.g. a generated `ConfigMap`/`Secret` mounted as a volume),
  then drives a version-pinned rolling restart (C4). No per-pod agent runs. This
  competes with `ConfigMap` + Reloader on adoption cost, adds the git source of
  truth + multi-environment model + fleet consistency, but inherits `ConfigMap`
  limits and cannot hot-reload.

Selection MUST be explicit per binding. The reload contract (§10) determines the
required topology: C1–C3 imply Topology A; C4 MAY use either but defaults to
Topology B.

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

## 9A. Secret References

The config repo is plaintext and reviewable, so it MUST NOT contain secret
values. Yet real config (Envoy upstreams, NGINX TLS, Fluent Bit outputs,
Prometheus remote-write) needs credentials and keys. Kohen therefore supports
**secret *references***: the git-tracked config points at a secret **by name**,
and Kohen resolves the actual material **inside the cluster** at materialization
time from a secret source Kohen does not own.

### 9A.1 Principles

- **P1 — No plaintext secrets in git.** Kohen MUST NOT require, expect, or read
  secret *values* from the config repo. Only *references* live in git.
- **P2 — Kohen is a consumer, not a store (N2).** Kohen resolves references
  against secret material that already exists in (or is provisioned into) the
  cluster. It does not encrypt, decrypt, rotate, or persist secret material of its
  own.
- **P3 — Native `Secret` is the common substrate.** Whatever the upstream store
  (native `Secret`, External Secrets Operator, Vault, Sealed Secrets, CSI
  drivers), Kohen resolves through the **Kubernetes `Secret`** those tools
  materialize. This gives one integration seam and inherits Kubernetes RBAC and
  encryption-at-rest. External Secrets support is therefore "resolve the `Secret`
  that ESO syncs", not a re-implementation of ESO.

### 9A.2 Supported secret sources

- **S1 — Native `Secret`.** Reference an existing `Secret` (name + key) in the
  workload's namespace.
- **S2 — External Secrets Operator (ESO).** Reference an `ExternalSecret` (or its
  target `Secret`). Kohen resolves the target `Secret` and MAY wait for the
  `ExternalSecret` to report `Ready` before materializing (R9A.6). Kohen does not
  talk to the external provider directly.
- **S3 — Any tool that produces a native `Secret`.** Sealed Secrets, Vault
  agent/CSI (already materialized), cert-manager-issued TLS `Secret`s, etc. are
  all consumed via S1 semantics.

Cross-namespace references are **disallowed by default** and MUST require explicit
operator-level allow-listing when enabled (multi-tenancy, R9.9).

### 9A.3 How a reference is declared

References are declared **declaratively**, not by scanning arbitrary config file
formats (format-agnostic and robust). Two equivalent declaration sites are
supported; both produce the same behavior:

1. **In-repo (`kohen.secrets.yaml`, recommended)** — lives beside the config in
   the git path, so the config author owns it and it is reviewed with the config:

   ```yaml
   # services/edge-proxy/prod/kohen.secrets.yaml
   apiVersion: kohen.dev/v1alpha1
   kind: SecretMapping
   secrets:
     - name: upstream-api-token          # logical name used by the config
       from:                             # exactly one source
         nativeSecret: { name: edge-upstream, key: token }
       project:
         mode: file                      # file | inline
         path: secrets/upstream_token    # relative to the mount root
         fileMode: "0400"
     - name: tls-key
       from:
         externalSecret: { name: edge-tls }   # ESO-managed; key defaults by convention
       project:
         mode: file
         path: secrets/tls/server.key
         fileMode: "0400"
   ```

2. **In the `ConfigBinding`** — the same list under `spec.secretRefs` (see §11.2),
   for cases where the config repo is read-only to the consumer or the platform
   team wants to own the mapping. If both are present, the operator MUST reject
   conflicting definitions rather than silently merge (R9A.7).

### 9A.4 Projection modes

- **M-file (default, preferred).** Each referenced secret is materialized as a
  **file** at the declared path within the config mount, on a **memory-backed**
  volume. The git-tracked config references it **by path** (e.g. Envoy SDS from
  file, NGINX `ssl_certificate_key /etc/appconfig/secrets/tls/server.key`). This
  keeps secret values out of every config file and is the least-exposure option.
- **M-inline (opt-in).** When light templating (§7.4) is enabled, a config file
  MAY contain a reference such as `{{ secret "upstream-api-token" }}` that Kohen
  substitutes during rendering. The rendered output containing the value MUST be
  written **only** to a memory-backed volume, MUST be excluded from any on-disk
  cache, and MUST NOT be logged or included in diffs (§16.2 `kohenctl diff` MUST
  redact). Inline mode is more convenient for daemons that only accept inline
  secrets but has a larger exposure surface; it is therefore explicit opt-in.

### 9A.5 Delivery per topology

- **Topology A (in-pod agent).** Referenced `Secret`s are mounted into the agent
  via **standard Kubernetes secret volumes** (declared by injection/the
  operator). The agent composes them into the config tree as part of the same
  **atomic swap** (§8) — it does not need Kubernetes API `get` on Secrets, so the
  sidecar keeps minimal RBAC. Kubelet remains the secret delivery mechanism.
- **Topology B (operator projection).** The operator resolves references and
  emits a generated, memory-mountable `Secret`/projected volume for the workload,
  then drives a version-pinned rollout (C4) on change.

### 9A.6 Consistency, rotation & versioning

- **R9A.1** A referenced secret that is missing/incomplete/not-`Ready` MUST cause
  the sync to **fail closed**: do not swap to a config version with unresolved
  references; keep last-good; mark `Degraded`; emit an event (§13).
- **R9A.2** The **config version** (§8.3) MUST incorporate a **salted hash** of
  the resolved secret material (never the material itself) so that a **secret
  rotation triggers the reload/rollout contract** just like a git change. This
  gives file-driven daemons automatic reload on cert/credential rotation.
- **R9A.3** Materialization of secrets MUST be **atomic** together with the
  config tree (no window where config points at a not-yet-written secret file).

### 9A.7 Security requirements (secret references)

- **R9A.4** Secret material MUST only ever be written to **memory-backed** volumes
  (`emptyDir` `medium: Memory` / `tmpfs`), with restrictive file permissions
  (default `0400`) and correct ownership for the app UID/`fsGroup`.
- **R9A.5** Secret values MUST NEVER appear in logs, events, metrics, CRD status,
  traces, or `kohenctl` output; only presence/absence and hashed version markers
  are exposed.
- **R9A.6** RBAC for reading `Secret`s MUST be least-privilege and namespaced. In
  Topology A the sidecar SHOULD avoid Secret API access entirely by relying on
  kubelet-mounted secret volumes; in Topology B the operator's Secret access MUST
  be scoped (ideally per-namespace, resourceNames-limited where feasible).
- **R9A.7** Conflicting or ambiguous secret mappings (duplicate logical names,
  both in-repo and binding definitions disagreeing) MUST be rejected at
  reconcile/validation time, not resolved silently.
- **R9A.8** ESO/Vault remain the source of truth and rotation authority; Kohen
  MUST NOT extend secret lifetime beyond what the source provides and MUST
  re-resolve on the source `Secret`'s change (watch-driven).

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
  secretRefs:                                 # optional (§9A); may instead live in-repo as SecretMapping
    - name: upstream-api-token
      from: { nativeSecret: { name: edge-upstream, key: token } }
      project: { mode: file, path: secrets/upstream_token, fileMode: "0400" }
    - name: tls-key
      from: { externalSecret: { name: edge-tls } }   # ESO-managed target Secret
      project: { mode: file, path: secrets/tls/server.key, fileMode: "0400" }
status:
  targetVersion: 9f1c2ab                      # consistency target (R8.4); includes resolved-secret hash (R9A.2)
  secretRefs:                                 # resolution status only — never values (R9A.5)
    - name: upstream-api-token  resolved: true  source: nativeSecret
    - name: tls-key             resolved: true  source: externalSecret
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
- **R11.5** Secret references MAY be declared in the `ConfigBinding`
  (`spec.secretRefs`) or in-repo (`SecretMapping`, §9A.3); the CRD schema for both
  MUST be identical, and conflicts MUST be rejected (R9A.7). CRD status MUST
  report per-reference resolution state without exposing values (R9A.5).

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
| Secret reference unresolved (missing `Secret`/key, ESO not `Ready`) | **Fail closed** (R9A.1): do not swap; keep last-good; mark `Degraded`; emit event; never log the value. |
| Referenced secret rotates | Re-resolve (watch-driven, R9A.8); resolved-secret hash changes the config version (R9A.2) and drives the reload/rollout contract like a git change. |
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
  fleet convergence (targeted vs observed), git fetch latency/errors,
  secret-reference resolution successes/failures (count only, never values;
  R9A.5), and degraded-state gauges.
- **R14.2 — Logging.** Structured (JSON) logging with configurable levels.
  Secret values MUST never be logged, and secret-reference material MUST be
  redacted everywhere (R9.6, R9A.5).
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
| `--secret-map` | `KOHEN_SECRET_MAP` | `""` | Path to the resolved `SecretMapping` (§9A) the agent composes into the tree. |
| `--secret-mount` | `KOHEN_SECRET_MOUNT` | `""` | Root of kubelet-mounted referenced `Secret`s (Topology A source, §9A.5). |
| `--metrics-addr` | `KOHEN_METRICS_ADDR` | `:9095` | Metrics/health bind address. |
| `--verify-signed` | `KOHEN_VERIFY_SIGNED` | `false` | Require verified signed commits. |

> The three existing prototype flags (`--gitUrl`, `--gitPath`, `--targetDir`) map
> onto `--git-url`, `--git-path`, `--target-dir`. The spec standardizes on
> kebab-case; the implementation SHOULD accept the legacy names as aliases for a
> deprecation window.

### 16.2 `kohenctl` (optional helper CLI, SHOULD)

- `kohenctl status <binding>` — show target vs observed versions (UC6).
- `kohenctl diff <binding>` — show config diff between served and latest version
  (secret-referenced material MUST be redacted, R9A.5).
- `kohenctl pin <binding> <sha>` / `kohenctl rollback <binding>` — set target
  (UC5).
- `kohenctl verify <repo>` — validate signatures / layout locally.

---

## 17. Milestones / Phased Roadmap

Ordered by dependency, not by calendar. Each milestone is independently
shippable and testable.

- **M0 — Spec & foundations (this document).** Agreed requirements, glossary,
  architecture. Repo scaffolding, license, CI skeleton.
- **M1 — Agent MVP (standalone), file-tree focus.** `kohen-agent` init + sidecar
  modes, atomic swap of a **directory tree** (R8.1–R8.3), HTTPS/SSH auth from
  secrets (R9.1–R9.4), file-watch + signal reload contracts (C1, C2),
  metrics/health (§14), fail-safe behavior (T8, §13). Delivers UC1, UC2 (Topology
  A) without the operator — validated against a real file-driven daemon
  (Envoy/NGINX/Fluent Bit).
- **M2 — Operator & CRDs.** `ConfigSource` + `ConfigBinding`, ref→commit
  resolution and consistency target publication (R8.4–R8.5), status/events,
  Helm chart, and the sidecar-free **Topology B** projection path (G9). Delivers
  fleet consistency, UC4, UC6.
- **M3 — Secret references (§9A).** `SecretMapping` (in-repo + `spec.secretRefs`),
  native `Secret` (S1) and External Secrets (S2) resolution, file projection mode
  (M-file) on memory-backed volumes, fail-closed + rotation-triggered reload
  (R9A.1–R9A.8), redaction everywhere. Delivers UC7. (Inline mode M-inline
  deferred to M6 with templating.)
- **M4 — Reload completeness & rollout.** HTTP/exec hooks (C3), rollout contract
  (C4) with version-pinned restarts, rollback (UC5, R8.7).
- **M5 — Injection & webhooks.** Mutating webhook auto-injection (R11.3),
  git-provider webhook-triggered syncs (R9.10), `kohenctl`.
- **M6 — Advanced.** Overlays/light templating + inline secret substitution
  (§7.4, M-inline), signed-commit verification (R9.5), repo policy enforcement
  (§7.5), progressive rollout strategies (R8.6), tracing (R14.6), image
  signing/SBOM (R9.8).

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
- **A9** A config file that references a secret (native `Secret` and an
  ESO-managed target `Secret`) has the material projected as a file on a
  memory-backed volume with mode `0400`, and the plaintext never appears in git,
  logs, events, status, or `kohenctl` output (§9A, R9A.4–R9A.5).
- **A10** An unresolved secret reference fails closed — the workload keeps its
  last-good config, is marked `Degraded`, and emits an event (R9A.1); rotating the
  referenced secret changes the config version and drives the reload/rollout
  contract automatically (R9A.2), verified in an e2e test.

---

## 19. Open Questions

1. **Persistent cache for init resilience.** Should agents share a
   node-local/persistent cache so an init container can start from last-good
   config during a total git outage? Trade-off: resilience vs. staleness/attack
   surface. (Affects §13 init row.)
2. **Consistency coordination transport.** Publish the consistency target via a
   dedicated `ConfigRelease` CRD, `ConfigBinding` status, or a projected
   in-cluster artifact the agent reads? Impacts operator↔agent coupling.
3. **Overlay engine scope.** How much merge/templating is "light enough" to stay
   within non-goal N4 while being genuinely useful? (Also gates inline secret
   substitution M-inline, §9A.4.)
4. **Multi-repo bindings.** Should a single workload be able to compose config
   from more than one `ConfigSource`? If so, define precedence/merge order.
5. **Webhook ingress topology.** For webhook-driven syncs, what is the supported
   ingress/relay model in restricted-egress clusters?
6. **Secret access path in Topology A.** §9A.5 prefers kubelet-mounted secret
   volumes (no sidecar Secret API access), but that requires the operator/webhook
   to know the referenced `Secret`s at injection time. Is a fallback where the
   agent reads Secrets via a tightly-scoped (`resourceNames`) RBAC role
   acceptable for dynamically-discovered references, or must all references be
   resolvable at admission time?
7. **ESO readiness coupling.** Should Kohen hard-block materialization until an
   `ExternalSecret` reports `Ready` (safer, but couples startup to ESO health), or
   proceed with the last-known target `Secret` if present (more available)?
   (Relates to R9A.1 vs. NFR1.)
8. **Env-var-sourced config (resolved as N6).** Confirmed out of scope for v1;
   such apps are steered to the rollout contract (C4). Revisit only if strong
   demand emerges.

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
