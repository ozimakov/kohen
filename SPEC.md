# KOHEN — Specification

> Status: **Draft v0.5** · Owner: Kohen maintainers · Last updated: 2026-07-04
>
> This document is the single source of truth for **what** Kohen is, **why** it
> exists, and **which requirements** any implementation must satisfy. It is
> intentionally implementation-light: it describes behavior, contracts, and
> constraints rather than prescribing internal code structure. Where a concrete
> technology is named, it is a requirement; where an approach is named, it is a
> recommendation and is marked as such.
>
> **Scope decisions.** (1) Kohen ships as a cluster **operator** only; a
> per-workload sidecar/env-var mode is explicitly **deferred** (§16 M6, §18). (2)
> Secrets are handled by **integration/resolution, not production**: if config
> references a secret, Kohen assumes it exists (or reconciles until it does) and
> **resolves it into the pod using existing Kubernetes mechanics** — it never
> stores, encrypts, or rotates secret material (§8).
>
> **v0.5 decisions.** (a) Kohen **mutates the target workload by merging** into
> the existing definition via Server-Side Apply with a dedicated field manager, to
> **coexist with GitOps without race conditions** (§6.2). (b) If the config path
> contains an `ExternalSecret`/`SealedSecret` manifest, Kohen **applies it** (owns
> its lifecycle) and awaits readiness; otherwise it **awaits** an
> externally-managed secret (§8.2). (c) **ESO/backend readiness:** fail-closed on
> first resolution, fail-safe (keep last-good) on subsequent updates (§8.5).

---

## 1. Overview

**Kohen** is a Kubernetes **operator** that keeps native cluster objects in sync
with a path in a dedicated `git` configuration repository. Given a repo, a
branch, and a path, Kohen continuously:

- renders non-secret configuration into a **`ConfigMap`**;
- **resolves** any secret the config references — assuming the backing secret
  already exists or will be created — and makes it available to the workload
  using **existing Kubernetes secret mechanics** (a `Secret` volume/`envFrom`, an
  External Secrets `ExternalSecret`, a CSI-mounted secret, etc.); and
- records the resolved **config git SHA** on the target workload and drives a
  **rolling update** when — and only when — it changes.

The application consumes the resulting `ConfigMap` and secret exactly as it would
any hand-authored object. Kohen decouples config/secret delivery from CI
pipelines and app release cycles, sourcing them from one reviewed, multi-
environment git repository.

### 1.1 One-sentence definition

> Kohen is a Kubernetes operator that syncs a dedicated git repo's config path
> into a `ConfigMap`, resolves the secrets that config references into the
> workload via existing Kubernetes/External-Secrets mechanics, and rolls the
> workload out by matching the config git SHA on its metadata.

### 1.2 Minimum viable usage

Point a `ConfigSync` at a repo/branch/path and a workload:

```yaml
apiVersion: kohen.dev/v1alpha1
kind: ConfigSync
metadata: { name: checkout-prod, namespace: checkout }
spec:
  source:  { url: https://github.com/acme/platform-config.git, branch: main }
  path:    services/checkout/prod
  workloadRef: { apiVersion: apps/v1, kind: Deployment, name: checkout }
```

Kohen renders `services/checkout/prod@main` into a `ConfigMap`, wires any
secrets the config references into the `checkout` Deployment, stamps the config
git SHA, and rolls it out on change. That is the entire bare-minimum contract.

### 1.3 Why native objects (not a private volume)

Targeting `ConfigMap` + native secret mechanics rather than a private file tree
is deliberate:

- **Native, atomic delivery for free.** Kubelet delivers mounted
  `ConfigMap`/`Secret` updates **atomically** (`..data` symlink swap) and
  consistently to every pod. Kohen does not reinvent atomic file swaps.
- **Fleet consistency for free.** A single `ConfigMap` is one source of truth for
  the fleet, and the SHA stamp on the workload template makes every rolled pod
  carry the same version (§9).
- **Ecosystem compatibility.** Composes with External Secrets Operator, Sealed
  Secrets, Vault, the Secrets Store CSI driver, and mounted-volume reload.
- **Known constraint, chosen deliberately.** A `ConfigMap` is limited to ~1 MiB
  and flat keys; Kohen accepts this (§12 T-LIMIT, §7.5 splitting) in exchange for
  native integration. Very large file-tree configs are out of scope for v1.

[Stakater Reloader]: https://github.com/stakater/Reloader

---

## 2. Background & Motivation

### 2.1 The problem

Teams increasingly want a **dedicated git repository** as the source of truth for
application configuration and its secret wiring — reviewable, auditable,
multi-environment, decoupled from release cycles. Turning that git content into
the `ConfigMap` a workload consumes, plus wiring in the secrets it references,
plus rolling the workload when either changes, is bespoke today:

1. **Sync burden.** Something must translate git commits into `ConfigMap`
   updates — usually a hand-rolled CI job or a full GitOps pipeline, coupling
   config to deployment tooling and releases.
2. **Config and secrets are wired separately.** Config comes from one path (CI →
   `ConfigMap`) and secrets from another (ESO/Vault/Sealed Secrets), with no
   single git-sourced reconciler that produces the config and ensures the
   referenced secrets are resolved into the same workload.
3. **No automatic, consistent rollout on change.** Making the whole fleet move to
   "config commit `abc123`" — and only when it actually changed — is
   hand-rolled (annotation hashes, manual `kubectl rollout restart`).
4. **Multi-environment sprawl.** dev/staging/prod variants in cluster objects or
   CI templates duplicate and drift; a dedicated repo is easier to review, but
   nothing turns it into live objects + rollouts on its own.

### 2.2 Why not just GitOps?

GitOps controllers (Argo CD, Flux) reconcile Kubernetes objects toward a
git-declared desired state and *can* manage `ConfigMap`s. Kohen is complementary
and narrower:

- It is purpose-built for **config repo path → `ConfigMap` + resolved secret
  references + SHA-matched rollout** of a specific workload, not for reconciling
  arbitrary manifests.
- It keeps a **dedicated config repository** as the source of truth, distinct from
  a deployment repo, and owns the **secret-resolution + rollout** loop.

Use GitOps to deploy what runs; use Kohen to keep a running workload's config
object, resolved secrets, and rollouts in sync with a dedicated config repo.

### 2.3 Prior art / positioning

| Approach | What it does | How Kohen differs |
| --- | --- | --- |
| CI job rendering `ConfigMap`s | Pipeline turns git into objects | Kohen is a running operator, decoupled from CI/releases; also resolves referenced secrets and drives the rollout |
| GitOps (Argo CD, Flux) | Reconcile cluster desired state from git | Kohen is a narrow config-path→object + secret-resolution + SHA-rollout loop over a dedicated config repo; composes with GitOps |
| External Secrets Operator (ESO) | Pulls secrets from external stores into `Secret`s | Kohen **consumes** ESO: it references/applies `ExternalSecret`s and wires the resulting `Secret` into the pod; ESO remains the secret authority |
| Sealed Secrets / Vault / CSI | Make secret material available in-cluster | Kohen integrates with each as a resolution backend (§8) rather than replacing them |
| Stakater Reloader | Restarts pods when a `ConfigMap`/`Secret` changes | Kohen has its own SHA-matched rollout (§9); Reloader interop remains available |
| Bespoke init scripts | Clone config at startup | One-shot, no live updates, no object model, no rollout, per-team reinvention |

Kohen's distinguishing combination: **a git→`ConfigMap` operator that also
resolves the secrets the config references into the workload (via existing
mechanics), and rolls the workload out by matching the config git SHA — over a
dedicated, multi-environment config repo.**

### 2.4 When NOT to use Kohen (and what to use instead)

- **Your config exceeds `ConfigMap` limits** (~1 MiB, huge file trees). A direct
  `git-sync`-to-volume pattern fits better (a Kohen volume mode is deferred, N5).
- **You already render `ConfigMap`s from git via GitOps** and do not want a second
  reconciler or a separate config repo. Keep doing that.
- **You need a secrets manager.** Use External Secrets Operator / Vault / Sealed
  Secrets. Kohen **integrates** with these to make secrets available in the pod;
  it does not store, encrypt, or rotate them (§3.2 N2, §8).
- **You only hand-author a `ConfigMap` and want restart-on-change.** `ConfigMap` +
  Reloader is enough; Kohen adds the git source of truth and secret resolution.
- **You want to toggle product behavior in code.** Use a feature-flag platform.

Kohen fits when you want a **dedicated, multi-environment git repo to drive a
workload's `ConfigMap`, its referenced secrets, and its rollouts**, centrally and
declaratively.

---

## 3. Goals & Non-Goals

### 3.1 Goals

- **G1** As an operator, continuously synchronize a git `repo@branch:path` into a
  target **`ConfigMap`** in the workload's namespace.
- **G2 — Secret resolution (not production).** For each secret the config
  references, **assume it exists or reconcile until it does**, then make it
  available to the workload using **existing Kubernetes secret mechanics** (§8).
  Kohen MUST NOT store, encrypt, or rotate secret material.
- **G3** Support a broad menu of secret backends — External Secrets Operator
  (primary), native `Secret`, Sealed Secrets, Vault, and the Secrets Store CSI
  driver — via a common resolution model (§8.3).
- **G4** Support a **multi-environment** repository model (dev/staging/prod, and
  optionally region/tenant) selected by branch and/or path.
- **G5** Keep the target objects **current** and guarantee **fleet consistency**:
  resolve the branch to a single commit, produce objects from it, and converge all
  pods to that version.
- **G6 — SHA-matched rollout.** Record the resolved **config git SHA** on the
  workload and trigger a rolling update **only when it changes** (§9).
- **G7** Be **secure by default**: least-privilege git access; least-privilege
  RBAC scoped to the objects/workloads it manages; secrets never logged.
- **G8** Be **operable and fail-safe**: a git/secret-backend/Kohen outage must
  never delete or corrupt existing objects, wire an unresolved secret, or take
  down healthy workloads.
- **G9** Keep the declarative surface **minimal**: repo/branch/path + a workload
  reference is enough for the common case (§1.2).

### 3.2 Non-Goals

- **N1** Not a GitOps/CD engine; does not reconcile arbitrary manifests or deploy
  applications.
- **N2** Not a secrets manager. Kohen **resolves and wires** secrets that other
  tools own; it does not store, encrypt, generate, or rotate secret material
  (§8).
- **N3** Does not own the schema/validation semantics of an app's config beyond
  optional structural checks.
- **N4** Not a templating platform competitor to Helm/Kustomize; light,
  deterministic overlay/substitution MAY be supported (§7.4).
- **N5** Does not deliver config as a large private file tree/volume in v1; the
  target is `ConfigMap` + native secret mechanics, bounded by their size limits
  (§1.3, §12 T-LIMIT). A volume-target mode is a possible future (§18).
- **N6** **Operator-only in v1.** A per-workload sidecar with env-var
  configuration is explicitly deferred (§16 M6, §18); all v1 behavior is delivered by
  the operator.
- **N7** No UI in v1 (CLI + Kubernetes API only).

---

## 4. Personas & Use Cases

### 4.1 Personas

- **Application developer (Dana).** Owns a service. Wants its `ConfigMap` and the
  secrets it uses to come from a reviewed PR in a dedicated config repo, declared
  once via a `ConfigSync`, and to roll out automatically on change.
- **Platform / SRE engineer (Sam).** Operates the cluster and the Kohen operator.
  Wants a central, auditable, least-privilege reconciler that integrates with the
  cluster's chosen secret backend (ESO/Vault/etc.) and drives consistent
  rollouts.
- **Security / compliance reviewer (Riley).** Wants config changes reviewed and
  attributable to commits, **no plaintext secrets in git**, secret material owned
  by the approved backend, and least-privilege access for the operator.

### 4.2 Primary use cases

- **UC1 — Config sync.** A `ConfigSync` keeps a workload's `ConfigMap` in sync with
  a git path, fleet-consistently.
- **UC2 — Secret resolution.** Config references a secret; Kohen resolves the
  backing `Secret` (assuming it exists or reconciling until it does) and makes it
  available to the pod via existing mechanics — no plaintext in git (§8).
- **UC3 — External Secrets integration.** The referenced secret is backed by an
  `ExternalSecret`; Kohen ensures it is applied/present and wires the resulting
  `Secret` into the workload; ESO owns the material.
- **UC4 — Multi-environment.** dev/staging/prod selected by branch and/or path,
  no payload duplication.
- **UC5 — SHA-matched rollout.** A merged commit (or a resolved-secret change)
  stamps a new config git SHA and triggers exactly one rolling update; an
  unchanged SHA triggers none.
- **UC6 — Rollback.** Reverting the repo or pinning a prior commit restores the
  objects and stamps the prior SHA.
- **UC7 — Audit.** For any workload, the applied config git SHA is readable from
  its metadata and the `ConfigSync` status.

---

## 5. Terminology (Glossary)

- **Config repository (config repo):** A dedicated git repository that is the
  source of truth for application configuration and its secret references,
  distinct from application source/deploy repos.
- **Config path:** The subdirectory within the config repo a sync consumes.
- **`ConfigSync`:** The custom resource describing one git-path→objects
  synchronization and its target workload (§11).
- **Config version / config git SHA:** An immutable identifier for a produced set
  of objects — the source git commit SHA, extended with a content digest and a
  hash of resolved-secret identities/versions (never values). Kohen stamps and
  matches this on the workload.
- **SHA stamp:** The config git SHA recorded on the target workload's pod-template
  metadata (annotation `kohen.dev/config-sha`); the version of record and rollout
  trigger (§9).
- **Secret reference:** A pointer in the config (never the value) to secret
  material owned by a supported backend, which Kohen **resolves** and makes
  available in the pod (§8).
- **Resolution:** Determining the concrete in-cluster `Secret` (or CSI/injected
  material) for a reference — waiting/reconciling until it exists — and wiring it
  into the workload via existing mechanics.
- **Fleet consistency:** All pods of the workload run objects produced from one
  resolved commit; guaranteed because every rolled pod carries the same SHA stamp.

---

## 6. System Architecture

Kohen is a single cluster operator (a `Deployment`) reconciling `ConfigSync`
resources. Its output is native objects in the workload's namespace plus a
SHA-matched rollout of the workload.

```
            ┌──────────────────────────────────────────────┐
            │            Config git repository              │
            │   services/checkout/{dev,staging,prod}/...    │
            └───────────────┬──────────────────────────────┘
                            │ fetch repo@branch:path (poll/webhook, read-only)
                            ▼
                 ┌───────────────────────┐
                 │   kohen-operator      │  reconciles ConfigSync
                 │   (Deployment)        │
                 └──────────┬────────────┘
        render config        │ resolve secret refs        stamp SHA + rollout
        ─────────────┐       │       ┌──────────────────┐        │
                     ▼       ▼       ▼                  │        ▼
   ┌───────────────────────────────────────────────────────────────────────┐
   │ Target namespace                                                        │
   │   ConfigMap  <name>          (non-secret config)                        │
   │   references → backing Secret via ESO / native / SealedSecret / CSI     │
   │   Deployment/StatefulSet: ConfigMap + Secret wired in; SHA annotation   │
   └───────────────────────────────┬───────────────────────────────────────┘
                                    │ built-in controller rolls out on SHA change
                                    ▼
                        ┌───────────────────────────┐
                        │  Application workload     │
                        │  reads ConfigMap + Secret │
                        └───────────────────────────┘
```

### 6.1 The reconcile loop

For each `ConfigSync`, one cycle:

1. **Fetch** `repo@branch:path` and resolve the branch to a concrete commit.
2. **Render** non-secret config → desired `ConfigMap` (§7.4).
3. **Resolve secrets** the config references (§8): ensure/await the backing
   secret, and wire it into the workload via existing mechanics. If any reference
   is unresolved, **fail closed** (do not proceed to stamp/rollout).
4. **Apply** the `ConfigMap` (and any secret manifests Kohen owns, §8.2) via
   server-side apply with ownership labels; **prune** objects it previously
   created that vanished from git.
5. **Wire & stamp** — merge Kohen-owned volumes/mounts/env and the SHA annotation
   into the workload via SSA (merge semantics, §6.2), compare the computed config
   version to the workload's stamp, and if changed, update the pod-template
   annotation to trigger a rolling update (§9). If unchanged, do nothing.
6. **Report** status/events/metrics.

### 6.2 Workload wiring (merge into the existing definition)

Kohen **does** mutate the target workload — it references the produced
`ConfigMap` and resolved secrets into the pod template using **existing
Kubernetes mechanics** (a `ConfigMap`/`Secret` volume + mount, `env`/`envFrom`
with `secretKeyRef`/`secretRef`, or a CSI volume — §8.4) and stamps the config
SHA (§9). Because a workload is frequently *also* owned by a GitOps controller
(Argo CD, Flux), Kohen MUST perform this mutation as a **merge into the existing
definition**, engineered to coexist with other managers rather than fight them.

- **R-WIRE.1 (server-side apply, dedicated field manager).** Kohen MUST mutate the
  workload via **Kubernetes Server-Side Apply** using a dedicated, stable field
  manager (e.g. `kohen`). It MUST claim ownership of **only** the fields it
  injects (its named volumes, volumeMounts, env entries, and the SHA annotation),
  never the whole pod spec or lists it did not author.
- **R-WIRE.2 (keyed, granular merge).** All injected list entries MUST use stable
  keys (named `volumes`, `volumeMounts`, `env` entries, keyed by name) so SSA
  performs a granular per-entry merge. Kohen MUST NOT replace entire lists or use
  client-side/whole-object apply on the workload.
- **R-WIRE.3 (GitOps coexistence, no flapping).** Kohen MUST be resilient to
  another manager concurrently reconciling the workload: on an SSA field-manager
  **conflict**, Kohen MUST re-read and re-apply **only its own fields** and MUST
  NOT force-override fields owned by other managers. The design goal is that
  Kohen's fields and GitOps's fields have **disjoint ownership**, so neither
  controller reverts the other (no reconcile flapping / rollout races).
- **R-WIRE.4 (GitOps ignore guidance).** Because many GitOps setups apply the
  workload as a whole object (and would otherwise prune Kohen's additions), Kohen
  MUST document and provide the required "ignore" configuration for common GitOps
  tools — e.g. Argo CD `ignoreDifferences` and Flux drift-detection exclusions —
  covering the SHA annotation (`kohen.dev/config-sha`) and Kohen-injected
  volumes/mounts/env. Kohen SHOULD label its injected additions to make these
  exclusions expressible.
- **R-WIRE.5 (ownership & pruning).** Injected fields MUST carry identifying
  labels/annotations so Kohen can update and, on `ConfigSync` deletion, **prune
  only its own additions** without disturbing the rest of the pod spec
  (R11.3–R11.4).
- Changes to owned wiring alter the pod template and therefore participate in the
  SHA-matched rollout (§9).

> Rationale: the mutation is intentional (it is how referenced secrets become
> available in the pod), but it is scoped and additive. SSA's per-field ownership
> is the primary mechanism that keeps Kohen and GitOps from racing; the documented
> ignore rules are the belt-and-braces for GitOps tools that apply whole objects.

### 6.3 Operator responsibilities & optional accelerators

- Least-privilege RBAC scoped to the namespaces/objects/workloads it manages
  (§12 T7).
- OPTIONALLY receives git provider **webhooks** to accelerate syncs beyond
  polling.
- OPTIONALLY verifies **signed commits** before acting on a commit.

> A per-workload sidecar mode configured by pod environment variables
> (`KOHEN_REPO`/`KOHEN_BRANCH`/`KOHEN_PATH`, matching the prototype) is **deferred**
> (N6). The operator is the only mode in v1.

---

## 7. Configuration Repository Model

### 7.1 Requirements

- **R7.1** A config repo MUST be a standard git repository reachable over HTTPS or
  SSH.
- **R7.2** A sync MUST select a **branch** and a **path** so one repo can serve
  many workloads and environments.
- **R7.3** The model MUST support multiple environments without duplicating the
  config payload across manifests.

### 7.2 Environment selection

- **Path-based (default):** environments are subdirectories, e.g.
  `services/checkout/{dev,staging,prod}` on one branch.
- **Branch-based:** each environment is a branch (`env/dev`, `env/prod`).
- **Pinning:** a sync MAY pin to a tag or explicit commit SHA for immutable,
  auditable releases and rollbacks.

Selection MUST be explicit (branch + path, or a pinned ref).

### 7.3 Layout conventions (recommended, not enforced)

```
config-repo/
└── services/
    └── checkout/
        └── prod/
            ├── app.yaml              # -> ConfigMap key app.yaml
            ├── logging.conf          # -> ConfigMap key logging.conf
            └── kohen.yaml            # optional: secret references, targets, split, overlay
```

### 7.4 Config → `ConfigMap` mapping

- **R7.4** By default each regular file under `path` becomes one **key** in the
  target `ConfigMap` (key = path relative to `path`; binary content →
  `binaryData`), flattened deterministically.
- **R7.5** If rendered content would exceed `ConfigMap` limits (§12 T-LIMIT),
  Kohen MUST fail closed with an actionable error and MAY support splitting across
  multiple named `ConfigMap`s (documented, deterministic).
- **R7.6** Optional light overlay/templating (deep-merge of `base` + `<env>`,
  bounded variable substitution) MAY be supported per N4; if enabled it MUST be
  deterministic. Otherwise content is mapped **verbatim**.

### 7.5 Per-path Kohen config (`kohen.yaml`, optional)

The path MAY contain a `kohen.yaml` declaring target object names, secret
references (§8), the config/secret split, overlay, and reload policy — so the repo
can own the details while the `ConfigSync` supplies only source/path/workload.
Fields on the `ConfigSync` take precedence per a documented order; unorderable
conflicts MUST be rejected.

---

## 8. Secret Integration & Resolution

Kohen's secret responsibility is **integration, not production**. Config in git
never contains secret values; it **references** secrets owned by a supported
backend. Kohen **resolves** each reference and makes the material available to the
workload using **existing Kubernetes mechanics**.

### 8.1 Principles

- **P1 — No plaintext secrets in git.** Only references live in git.
- **P2 — Kohen never owns the material.** It does not store, encrypt, generate, or
  rotate secret values. The backend (ESO/Vault/Sealed Secrets/native `Secret`/CSI
  provider) is the authority (N2).
- **P3 — Assume-exists, else reconcile.** A referenced secret is assumed to exist
  or to be on its way. If it is not yet present, Kohen **waits/reconciles**
  (requeue with backoff), marks the sync `Pending`/`Degraded`, and MUST NOT wire
  or roll out an unresolved reference (fail closed, R8.4).
- **P4 — Existing mechanics only.** Kohen surfaces the secret to the pod using
  standard Kubernetes constructs (`Secret` volume/`envFrom`/`secretKeyRef`, or a
  CSI `SecretProviderClass` volume), not a bespoke delivery path.

### 8.2 What "resolve" means

For each reference, Kohen:

1. **Identifies the backing secret** (a `Secret` name/keys, or a CSI provider
   class, per the backend).
2. **Applies-if-present, else awaits.** If the config path (or `kohen.yaml`)
   contains a creation manifest — an `ExternalSecret` or `SealedSecret` — Kohen
   **MUST apply it** (owning its lifecycle via SSA + ownership labels + prune,
   R8.8) so the responsible controller creates the `Secret`. If no such manifest
   is present, Kohen **awaits** an externally-managed secret and never creates one.
   Either way it reconciles until the material is present and (for ESO) `Ready`,
   subject to the readiness policy in R8.9.
3. **Wires it into the workload** via the requested mechanic (§8.4).
4. Includes a **hash of the resolved secret's identity/version** (never the value)
   in the config version, so a backing-secret change participates in the rollout
   (§9).

### 8.3 Supported backends (the menu users can choose)

| Backend | How the `Secret` comes to exist | Kohen's role | Notes |
| --- | --- | --- | --- |
| **External Secrets Operator (primary)** | ESO syncs from Vault/AWS/GCP/Azure/… into a `Secret` via an `ExternalSecret` | Apply the `ExternalSecret` from git if present (else reference/await); await `Ready` per R8.9; wire the `Secret` in | Recommended default; broad provider support |
| **Native `Secret`** | Created out-of-band (kubectl, another controller) | Reference it by name/keys; await existence; wire it in | Simplest; Kohen adds no lifecycle |
| **Sealed Secrets (Bitnami)** | Sealed Secrets controller decrypts a `SealedSecret` into a `Secret` | Apply the (encrypted) `SealedSecret` from git if present (else await); await the `Secret`; wire it in | Encrypted material can live in git safely |
| **HashiCorp Vault** | Via ESO, or Vault Agent Injector, or the CSI provider | For ESO: as above. For Injector/CSI: ensure the required annotations / CSI volume are present on the pod | Prefer ESO unless injector/CSI already standard |
| **Secrets Store CSI Driver** (AWS/GCP/Azure/Vault providers) | CSI driver mounts provider secrets; optionally syncs to a `Secret` | Ensure the `SecretProviderClass` reference + CSI volume/mount are wired into the pod | Good when secrets must be mounted, not in etcd |

- **R8.1** ESO MUST be supported first-class; the others MUST be supported through
  the same reference/resolution model where the mechanics allow.
- **R8.2** The backend is explicit per reference; Kohen MUST NOT guess a backend.

### 8.4 Surfacing mechanics (existing Kubernetes constructs)

- **File:** add a `Secret` (or CSI) volume and a `volumeMount` at a declared path.
  Auto-updates via kubelet for native/ESO `Secret`s.
- **Env:** add container `env` with `valueFrom.secretKeyRef`, or `envFrom` with a
  `secretRef`. (Env values change only on pod restart → covered by the SHA
  rollout.)
- Kohen MUST manage only the volumes/mounts/env entries it adds (owned markers)
  and MUST NOT modify unrelated pod-spec fields.

### 8.5 Requirements

- **R8.3** No secret value MUST ever appear in git, logs, events, metrics,
  `ConfigSync` status, or CLI output; only reference identity, presence, and
  hashed markers are exposed.
- **R8.4 (fail closed on unresolved).** An unresolved/incomplete reference
  (missing `Secret`, missing key) MUST prevent stamping/rollout for that version;
  keep last-good; mark `Pending`/`Degraded`; emit an event; requeue.
- **R8.5 (rotation).** When the backing secret changes, Kohen MUST reflect it: for
  volume-mounted native/ESO secrets, kubelet updates the file; where a restart is
  required (env), the resolved-secret hash advances the config version and drives
  the rollout (§9).
- **R8.6 (least privilege).** The operator MUST read only the specific `Secret`s /
  namespaces it must resolve, scoped by RBAC (ideally `resourceNames`-limited);
  cross-namespace references MUST be off by default and require explicit
  allow-listing.
- **R8.7** Conflicting/ambiguous references MUST be rejected at validation time,
  not silently merged.
- **R8.8 (owned-manifest lifecycle).** Secret manifests Kohen applies from git
  (`ExternalSecret`/`SealedSecret`, R8.2 step 2) MUST be applied via SSA with
  ownership labels and MUST be pruned when they disappear from git; Kohen MUST NOT
  adopt/overwrite a pre-existing manifest it does not own without explicit opt-in.
- **R8.9 (readiness policy — first-resolution fail-closed, update fail-safe).**
  Backend readiness (notably ESO `ExternalSecret` `Ready`) MUST be handled
  asymmetrically:
  - **First resolution** (no prior successfully-wired-and-rolled-out version for
    this reference): Kohen MUST **fail closed** — do not wire, stamp, or roll out
    until the backing secret exists and reports `Ready`; mark `Pending`. This
    prevents rolling pods that would crash on a missing/incomplete secret.
  - **Subsequent updates** (a prior good version is already wired and running):
    Kohen MUST **fail safe** — keep serving the last-good wiring/version, mark the
    reference/reload condition `Degraded`, requeue, and reconcile forward to the
    new version once the backend is `Ready` again. It MUST NOT tear down or roll a
    healthy workload because the backend is transiently not `Ready` (NFR1).

---

## 9. Consistency, Atomicity & Rollout

Kohen leans on native Kubernetes semantics and adds a single resolved version per
sync, recorded on the workload.

- **R-ATOM (delivery atomicity).** Mounted `ConfigMap`/`Secret` updates are made
  atomically by kubelet; Kohen MUST apply object updates atomically (server-side
  apply of fully-formed objects, never partial writes).
- **R-CONS (fleet consistency).** The operator MUST resolve the branch to a
  **single commit** and produce objects from it, and MUST record the resulting
  **config version** on the workload so every pod converges to one version. The
  config version MUST incorporate a content digest and a resolved-secret hash
  (R8.5) so any change advances it.
- **R-VERSION (version of record).** The authoritative record of "which config
  version this workload runs" is the **config git SHA stamped on the workload's
  pod-template metadata** (annotation `kohen.dev/config-sha`). Kohen decides
  whether a rollout is needed by **matching** the desired version against that
  stamp; this makes reconciliation idempotent and the deployed version auditable
  from workload metadata (UC7).
- **R-ROLLOUT (SHA-matched rollout — primary reload mechanism).**
  - **R-ROLLOUT.1 (stamp).** After a successful apply + secret resolution, Kohen
    MUST write the config version onto the target `Deployment`/`StatefulSet`
    (SHOULD support `DaemonSet`) **pod-template** metadata, via the merge
    semantics of §6.2 (SSA, Kohen-owned field only). Writing to the pod template
    is what makes the built-in controller perform a rolling update. RBAC per T7.
  - **R-ROLLOUT.2 (match).** Each reconcile MUST compare desired vs. stamped and
    trigger a rollout **only when they differ**; when they match, Kohen MUST make
    **no change** (no spurious rollouts).
  - **R-ROLLOUT.3 (trigger).** Kohen triggers the rollout only by updating the
    annotation, letting the built-in controller roll out with the workload's own
    `strategy`/`updateStrategy`. Kohen MUST NOT implement custom pod-deletion
    rollout logic.
  - **R-ROLLOUT.4 (order).** The stamp/rollout MUST happen only **after** the
    `ConfigMap` is applied and all secret references are resolved (R8.4), so new
    pods start against the new objects and available secrets.
- **R-SAFE (no destructive partial state).** A failed/interrupted reconcile MUST
  leave last-good objects and the current stamp intact (no truncation/deletion).
- **R-ROLLBACK.** Reverting the repo or pinning a prior commit MUST reproduce the
  earlier objects and stamp the prior SHA (UC6).

> This makes Kohen "its own Reloader keyed on the git SHA": rather than hashing
> rendered content for a third-party tool, Kohen uses the authoritative commit SHA
> as the version and owns the match/trigger loop. Native mounted-volume reload
> (C0) and Stakater Reloader interop (C1) remain available for apps that
> hot-reload without a restart (§10).

---

## 10. Reload / Consumption Options

The workload consumes the produced objects natively. Reload behavior:

- **C0 — Native volume update (default for mounted `ConfigMap`/`Secret`).** The
  app re-reads on kubelet's atomic update; no restart. Kohen does nothing beyond
  applying objects.
- **C1 — Reloader interop (optional).** Kohen MAY set a content-hash annotation so
  [Stakater Reloader] restarts the workload, for teams standardized on it.
- **C-ROLLOUT — SHA-matched rollout (default when a restart is required).** As
  specified in §9 (R-ROLLOUT). This is the primary mechanism for apps that read
  config/secrets at startup or via `envFrom`.

Requirements:

- **R10.1** The chosen behavior MUST be explicit per sync and documented; when a
  restart is required, SHA-matched rollout is the default.
- **R10.2** Any restart/rollout MUST fire only **after** objects are applied and
  secrets resolved (R-ROLLOUT.4), and success/failure MUST be observable and MUST
  NOT corrupt applied objects.

> In-pod reload options (signal, local HTTP hook) require a process-adjacent
> component and are deferred with sidecar mode (N6, §16 M6).

---

## 11. Kubernetes API (CRDs)

The operator exposes declarative resources. Field names are indicative; the API
MUST be versioned (`v1alpha1` → `v1beta1` → `v1`) and follow Kubernetes API
conventions (status subresource, `observedGeneration`, printer columns, CEL
validation where possible).

### 11.1 `ConfigSync` (namespaced) — the primary resource

```yaml
apiVersion: kohen.dev/v1alpha1
kind: ConfigSync
metadata: { name: checkout-prod, namespace: checkout }
spec:
  source:
    url: https://github.com/acme/platform-config.git
    branch: main                          # or pinned tag/commit
    interface: https                      # https | ssh
    authSecretRef: { name: kohen-git-creds }        # optional (private repos)
  path: services/checkout/prod
  configMap: { name: checkout-config }              # target; defaults derived from workload
  secretRefs:                             # optional (§8); may also live in-repo kohen.yaml
    - name: db-password
      backend: externalSecret             # externalSecret | nativeSecret | sealedSecret | vault | csi
      externalSecret: { name: checkout-db }         # applied by Kohen if present in git; ESO populates
      surface: { as: env, envVar: DB_PASSWORD, key: password }
    - name: tls
      backend: nativeSecret
      nativeSecret: { name: checkout-tls }
      surface: { as: file, mountPath: /etc/tls }
  workloadRef: { apiVersion: apps/v1, kind: Deployment, name: checkout }  # Deployment|StatefulSet|DaemonSet
  reload:
    onRestart: rollout                    # rollout (default) | reloaderAnnotation | none
    shaAnnotation: kohen.dev/config-sha   # stamped on spec.template.metadata (R-VERSION)
  sync: { interval: 30s }
status:
  configVersion: 9f1c2ab                  # desired = git SHA + content + resolved-secret hash (R-CONS)
  workloadVersion: 9f1c2ab                # currently stamped on the workload; rollout fires on mismatch
  rolloutInProgress: false
  secretRefs:                             # resolution state only — never values (R8.3)
    - { name: db-password, resolved: true,  backend: externalSecret }
    - { name: tls,         resolved: false, reason: SecretNotFound }   # -> Pending/Degraded, no rollout
  conditions: [ ... ]
```

### 11.2 Requirements

- **R11.1** CRDs MUST validate inputs (OpenAPI/CEL).
- **R11.2** Status MUST report the desired/stamped config versions, rollout state,
  and per-reference secret resolution state (no values).
- **R11.3** Deleting a `ConfigSync` MUST prune only Kohen-owned objects and
  Kohen-added workload wiring; it MUST NOT delete the workload or secrets it does
  not own.
- **R11.4** Kohen MUST NOT modify workload pod-spec fields it does not own, nor
  adopt/overwrite pre-existing objects it does not own without explicit opt-in.

---

## 12. Technical Requirements

- **T1 — Language/runtime.** Go (consistent with the prototype and ecosystem);
  current stable Go (≥ 1.23). Operator built with `controller-runtime`.
- **T2 — Git access.** Maintained Go git library (prototype uses `go-git`);
  shallow fetch and fetch-by-commit to pin and minimize bandwidth.
- **T3 — Apply semantics.** Server-side apply with a stable field manager and
  ownership labels; idempotent (same commit + resolved secrets ⇒ same objects,
  same stamp ⇒ no rollout).
- **T4 — Kubernetes compatibility.** Support the **N-2** range of supported
  upstream minor versions.
- **T-LIMIT — Object size.** Produced `ConfigMap`/`Secret` are bounded by the
  ~1 MiB object limit; Kohen MUST detect oversize and fail closed (R7.5).
- **T5 — Distribution.** Multi-arch (`amd64`/`arm64`), minimal
  (distroless/static) operator image; install via Helm chart and plain manifests,
  including scoped RBAC and CRDs.
- **T6 — Footprint.** The operator's footprint MUST be documented and bounded;
  scale is per §14 NFR3.
- **T7 — RBAC / security posture.** Least-privilege, namespace-scoped RBAC:
  read the specific git-cred and referenced `Secret`s (R8.6), manage the target
  `ConfigMap`, and `get/patch` the target workloads. Run as non-root on a
  read-only root filesystem. No cluster-admin.
- **T8 — Failure isolation.** A git or secret-backend outage MUST NOT delete/
  corrupt objects, wire unresolved secrets, or crash workloads; keep last-good and
  surface degraded status.
- **T9 — Determinism.** Same commit + resolved inputs ⇒ byte-identical objects and
  a stable config version.
- **T10 — Scale.** Reconcile many `ConfigSync`es efficiently (work queues, shared
  informers, rate limiting); dedup git fetches by repo+commit; bound fetch
  concurrency; watch referenced `Secret`s to react to rotation.

---

## 13. Failure Modes & Resilience

| Failure | Required behavior |
| --- | --- |
| Git unreachable | Retry with backoff; keep last-good objects and stamp (T8); mark `Degraded`; never prune on inability to fetch. |
| Auth failure | Fail fast with actionable error; event; avoid account-locking retry storms; redact secrets. |
| Malformed/missing path | Do not apply/stamp a bad version; keep last-good; `Degraded`; event. |
| Oversize object (T-LIMIT) | Fail closed with a clear error; suggest split; keep last-good. |
| Referenced secret missing / key absent | **Fail closed** (R8.4): do not wire, stamp, or roll out; `Pending`/`Degraded`; requeue; event; never log value. |
| Backend not `Ready` (e.g. ESO) — first resolution | **Fail closed** (R8.9): `Pending`; do not roll out pods that would crash on a missing secret. |
| Backend not `Ready` (e.g. ESO) — with a prior good version | **Fail safe** (R8.9): keep the last-good wiring/version running; `Degraded`; requeue; reconcile forward when `Ready`. Never tear down a healthy workload. |
| Backing secret rotates | Volume-mounted secrets update via kubelet; env-surfaced secrets advance the config version (R8.5) → rollout. |
| SSA conflict with another manager (GitOps) | Re-read and re-apply only Kohen-owned fields (R-WIRE.3); never force-override others' fields; rely on disjoint ownership + documented GitOps ignore rules (R-WIRE.4) to avoid flapping. |
| SHA already matches workload | No-op: MUST NOT patch the workload or roll out (R-ROLLOUT.2) — prevents spurious/looping rollouts. |
| Target workload not found / wrong kind | Do not fail the object sync; mark reload condition `Degraded`; event; keep objects applied. |
| Rollout stuck (app crashloops on new config) | Rely on the built-in controller + `progressDeadlineSeconds`; surface rollout status; MUST NOT force-delete pods; rollback = stamp prior SHA (R-ROLLBACK). |
| Operator down | Applied objects and stamps persist unchanged; no new versions until recovery; degrade gracefully. |
| Signature verification failure | Do not act on that commit; security event. |

- **R13.1** Retries MUST use bounded exponential backoff with jitter.
- **R13.2** All failure states MUST appear in status/conditions and metrics, not
  only logs.

---

## 14. Observability & Non-Functional Requirements

### 14.1 Observability

- **R14.1 — Metrics (Prometheus).** At minimum: reconcile attempts/successes/
  failures and duration; current config version (labeled info metric); objects
  applied/pruned; secret-resolution success/failure/pending (counts only, never
  values); rollouts triggered vs. skipped-on-match; git fetch latency/errors;
  degraded-state gauges.
- **R14.2 — Logging.** Structured (JSON), configurable level; secret material
  redacted everywhere (R8.3).
- **R14.3 — Health.** Standard operator readiness/liveness; liveness MUST NOT flap
  on git/secret-backend outages (T8).
- **R14.4 — Events.** Emit events for applies, prunes, secret resolution
  outcomes, stamps/rollouts, and failures.
- **R14.5 — Auditability.** For any workload, the applied config git SHA MUST be
  readable from workload metadata and `ConfigSync` status (UC7).

### 14.2 Non-Functional

- **NFR1 — Reliability.** Kohen MUST fail safe: its unavailability degrades
  *freshness*, never *availability* of healthy workloads or the integrity of
  applied objects/stamps.
- **NFR2 — Performance.** Detect+apply a change within a bounded, configurable
  window (default target ≤ 60 s via polling; faster with webhooks).
- **NFR3 — Scalability.** Hundreds of `ConfigSync`es/workloads per operator
  without overloading git or the API server (dedup by repo+commit).
- **NFR4 — Compatibility.** Mainstream git providers (GitHub, GitLab, Bitbucket,
  self-hosted) over HTTPS/SSH; compose with ESO, Sealed Secrets, Vault, the CSI
  driver, and Reloader; no proprietary API required for core function.
- **NFR5 — Portability.** Any conformant Kubernetes; `amd64` + `arm64`.
- **NFR6 — Usability.** The minimal `ConfigSync` (source + path + workloadRef)
  MUST be demonstrated end-to-end; the "when NOT to use" guidance (§2.4) MUST be
  prominent.
- **NFR7 — Documentation.** Concepts, install (Helm + manifests + RBAC), security
  hardening, CRD reference, secret-backend integration guides (ESO/native/Sealed/
  Vault/CSI), rollout/reload cookbook, troubleshooting.
- **NFR8 — Licensing.** Permissive OSI license (recommended Apache-2.0) with
  contribution guide and code of conduct.
- **NFR9 — Testing.** Unit + integration (real git server, envtest/`kind`) + e2e
  covering config sync, each secret backend (ESO first), SHA-matched rollout
  idempotency, oversize handling, and the failure modes in §13. CI gates merges.
- **NFR10 — Versioning.** SemVer for artifacts; documented CRD deprecation policy;
  breaking CRD changes require a new API version with conversion.

---

## 15. CLI Reference

- **Operator** — configured by `ConfigSync` (§11) plus a controller config file
  (leader election, metrics, concurrency, secret-backend enablement).
- **`kohenctl` (optional helper, SHOULD).**
  - `kohenctl status <sync>` — desired/stamped versions, resolution state, rollout
    (UC7).
  - `kohenctl diff <sync>` — diff between applied and latest (secrets redacted,
    R8.3).
  - `kohenctl pin <sync> <sha>` / `kohenctl rollback <sync>` — set version (UC6).
  - `kohenctl verify <repo@branch:path>` — validate layout/references/signatures.

---

## 16. Milestones / Phased Roadmap

Ordered by dependency, not calendar. Each milestone is independently shippable.

- **M0 — Spec & foundations (this document).** Requirements, glossary,
  architecture; repo scaffolding, license, CI skeleton.
- **M1 — Operator MVP.** `ConfigSync` CRD, git fetch + branch→commit resolution,
  config→`ConfigMap` (§7.4), server-side apply + ownership + prune (T3),
  **SSA merge into the workload** with a dedicated field manager + GitOps
  coexistence (R-WIRE.1–R-WIRE.5), and the **SHA-matched rollout** (R-ROLLOUT:
  stamp `kohen.dev/config-sha`, match, roll out only on change) for
  `Deployment`/`StatefulSet`, scoped RBAC (T7), metrics/health, fail-safe (§13).
  Delivers UC1, UC4, UC5, UC7.
- **M2 — Secret resolution (ESO first).** Secret references (§8), assume-exists/
  reconcile (P3), apply-if-present-else-await (R8.2/R8.8), file + env surfacing
  (§8.4), `ExternalSecret` integration with the asymmetric readiness policy
  (R8.9), fail-closed on unresolved (R8.4), rotation→rollout (R8.5), redaction
  (R8.3). Delivers UC2, UC3.
- **M3 — More secret backends.** Native `Secret`, Sealed Secrets, then Vault
  (Injector/CSI) and the Secrets Store CSI driver via the same model (§8.3).
- **M4 — Ecosystem & ergonomics.** Reloader interop (C1), `kohenctl`, rollback
  polish (R-ROLLBACK), overlay/light templating (§7.4), `DaemonSet` support.
- **M5 — Acceleration & hardening.** Git-webhook-triggered syncs, signed-commit
  verification, progressive rollout strategies, tracing, image signing/SBOM.
- **M6 (candidate) — Sidecar/env-var mode.** Revisit the deferred per-workload
  sidecar with env-var configuration (N6) if demand warrants (§18).

---

## 17. Acceptance Criteria

- **A1** A minimal `ConfigSync` (source + path + `workloadRef`) produces and
  maintains the expected `ConfigMap`, verified in an e2e `kind` test (UC1).
- **A2** Committing a change updates the `ConfigMap`; a mounted-volume consumer
  observes it (C0).
- **A3** With the rollout contract: a commit stamps the new config git SHA on the
  `Deployment`/`StatefulSet` pod template and triggers exactly one rolling update;
  a reconcile with an unchanged SHA triggers **no** rollout (R-ROLLOUT idempotency,
  R-VERSION); the deployed version is readable from workload metadata (UC7).
- **A4** Config references a secret backed by an `ExternalSecret` committed to git;
  Kohen **applies** it, awaits `Ready`, and wires the resulting `Secret` into the
  workload via the requested mechanic, with **no plaintext in
  git/logs/events/status/CLI** (§8, R8.2, R8.3), verified e2e with ESO.
- **A5** Readiness policy (R8.9): on **first** resolution an unready/missing secret
  fails closed (last-good kept, `Pending`, **no stamp/rollout**, R8.4); once it
  appears/`Ready`, Kohen resolves and rolls out. With a **prior good version**, a
  transiently unready backend fails safe — the running version is kept, condition
  is `Degraded`, and Kohen reconciles forward when `Ready`. Rotating an
  env-surfaced secret advances the version and rolls out (R8.5).
- **A6** A native `Secret` and a Sealed Secret resolve through the same model
  (§8.3) and surface correctly as file and env.
- **A7** Deleting a `ConfigSync` prunes only Kohen-owned objects and Kohen-added
  workload wiring; the workload and un-owned secrets are untouched (R11.3–R11.4).
- **A8** Producing an oversize `ConfigMap` fails closed with an actionable error
  (T-LIMIT).
- **A9** The operator runs as non-root, read-only rootfs, with least-privilege
  RBAC (only referenced secrets/namespaces, target ConfigMap, target workloads)
  (T7).
- **A10 (GitOps coexistence).** A workload also managed by a GitOps controller
  (Argo CD or Flux) does **not** flap: Kohen's SSA merge touches only its own
  fields, an SSA conflict is resolved by re-applying only Kohen-owned fields
  (R-WIRE.3), and with the documented ignore rules (R-WIRE.4) neither controller
  reverts the other. Verified e2e with at least one GitOps controller.

---

## 18. Open Questions

### 18.1 Resolved decisions (recorded for history)

- **RD1 — Workload mutation.** Kohen **does** mutate the target workload to wire in
  secrets/config, performed as an SSA **merge** into the existing definition to
  **coexist with GitOps without race conditions** (§6.2, R-WIRE.\*).
- **RD2 — Apply vs. await.** Kohen **applies** an `ExternalSecret`/`SealedSecret`
  committed to git (owning its lifecycle) and **awaits** an externally-managed
  secret otherwise (§8.2, R8.2/R8.8).
- **RD3 — Backend readiness.** **First-resolution fail-closed, update fail-safe**
  (§8.5, R8.9): never roll out pods that would crash on a missing secret, but never
  tear down a healthy workload for a transiently unready backend.

### 18.2 Still open

1. **Config/secret split ergonomics.** Explicit `secretRefs` only, or also a
   `secrets/` convention that infers references?
2. **Env-surfaced secret rotation.** Env values need a restart to update; is
   auto-rollout on env-secret rotation always desired, or opt-in per reference?
3. **Default object/annotation naming.** Derivation of the default `ConfigMap`
   name and confirmation of `kohen.dev/config-sha`.
4. **GitOps ignore automation.** Beyond documenting Argo CD `ignoreDifferences` /
   Flux exclusions (R-WIRE.4), should Kohen ship generators/snippets (or an Argo
   CD `ignoreDifferences` fragment) to make coexistence turnkey?
5. **Direct-file-volume mode (deferred, N5)** and **sidecar/env-var mode (deferred,
   N6):** demand thresholds to revisit.
6. **Webhook ingress topology** for git webhooks in restricted-egress clusters.

---

## 19. Relationship to the Existing Prototype

The repository contains an early `kohen-agent` prototype (Go + `go-git`) that
clones a repo into a target **directory** via `--gitUrl`/`--gitPath`/
`--targetDir` (and matching env vars). It validates the git-fetch premise and the
env-var configuration ergonomics. In v0.4 the **operator** is the delivery
vehicle: the prototype's fetch logic maps onto the operator's fetch/resolve step,
while the target evolves from a filesystem directory to a native `ConfigMap` plus
resolved secret wiring and a SHA-matched rollout. The prototype's env-var
interface is the seed for the **deferred** sidecar mode (N6, §18).

---

## Appendix A — Requirement Index

Requirements are labeled inline (`Rn.n`, `R-*`, `Tn`, `Gn`, `Nn`, `NFRn`, `An`,
`UCn`, `Pn`) so implementation PRs and tests can reference them directly. An
implementation is spec-conformant for a milestone (§16) when all requirements
reachable from that milestone's capabilities and their acceptance criteria (§17)
are satisfied and demonstrated by automated tests (NFR9).
