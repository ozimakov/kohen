# KOHEN — Specification

> Status: **Draft v0.3** · Owner: Kohen maintainers · Last updated: 2026-07-04
>
> This document is the single source of truth for **what** Kohen is, **why** it
> exists, and **which requirements** any implementation must satisfy. It is
> intentionally implementation-light: it describes behavior, contracts, and
> constraints rather than prescribing internal code structure. Where a concrete
> technology is named, it is a requirement; where an approach is named, it is a
> recommendation and is marked as such.
>
> **v0.3 changes the core model.** Kohen now synchronizes a git config path into
> **native Kubernetes objects** — a `ConfigMap` for non-secret config and a
> `Secret` / `ExternalSecret` for sensitive material — rather than materializing
> files directly onto a shared volume. Workloads consume those native objects the
> normal Kubernetes way. Kohen runs either as a cluster **operator** or as a
> per-workload **sidecar** configured by pod environment variables.

---

## 1. Overview

**Kohen** keeps Kubernetes-native configuration objects **in sync with a path in
a dedicated `git` repository**. Given a repo, a branch, and a path, Kohen
continuously renders:

- non-secret configuration → a **`ConfigMap`**, and
- sensitive material → a **`Secret`** and/or an **`ExternalSecret`** (and other
  supported secret manifests such as `SealedSecret`),

into the target namespace, and keeps them updated after every commit. The
workload then consumes those objects exactly as it would any hand-written
`ConfigMap`/`Secret` — mounted as a volume or projected as environment variables.

Kohen runs in one of two modes:

- **Operator mode** — a cluster controller, driven by a `ConfigSync` custom
  resource, that manages many syncs centrally with strong fleet consistency and
  no per-pod footprint.
- **Sidecar mode** — a container added to a workload's pod, configured entirely by
  **pod environment variables**, that syncs that workload's objects with no
  operator and no CRDs. The simplistic setup requires only three variables: the
  repo, the branch, and the path.

### 1.1 One-sentence definition

> Kohen synchronizes a dedicated git repository's config path into native
> Kubernetes `ConfigMap` and `Secret`/`ExternalSecret` objects — as either a
> cluster operator or a per-workload, env-var-configured sidecar — so
> applications get their configuration and secrets from git without bespoke CI
> glue or manual `kubectl apply`.

### 1.2 The minimum viable usage (sidecar mode)

Add the Kohen sidecar to a Deployment and set three environment variables:

```yaml
env:
  - name: KOHEN_REPO
    value: https://github.com/acme/platform-config.git
  - name: KOHEN_BRANCH
    value: main
  - name: KOHEN_PATH
    value: services/checkout/prod
```

Kohen renders `services/checkout/prod` from `main` into a `ConfigMap` (and any
declared `Secret`/`ExternalSecret`) in the pod's namespace, keeps it current, and
the application mounts it as usual. That is the entire bare-minimum contract.

### 1.3 Why native objects (not a private volume)

Targeting `ConfigMap`/`Secret` rather than an `emptyDir` file tree is a
deliberate design choice:

- **Native, atomic delivery for free.** Kubelet already delivers mounted
  `ConfigMap`/`Secret` updates **atomically** (via the `..data` symlink swap) and
  consistently to every pod that mounts the object. Kohen does not reinvent
  atomic file swaps.
- **Fleet consistency for free.** A single `ConfigMap` object is a single source
  of truth for the whole fleet; every replica converges to it as kubelet syncs.
- **Ecosystem compatibility.** Works with existing patterns: mounted-volume hot
  reload, [Stakater Reloader] for restart-on-change, `envFrom`, and External
  Secrets Operator for secrets. Kohen composes with these rather than replacing
  them.
- **Known constraint, chosen deliberately.** A `ConfigMap`/`Secret` is limited to
  ~1 MiB and flat keys. Kohen accepts this (see §13 T-LIMIT and §7.4 for
  splitting) in exchange for native integration; very large file-tree configs are
  out of Kohen's target scope in v1.

[Stakater Reloader]: https://github.com/stakater/Reloader

---

## 2. Background & Motivation

### 2.1 The problem

Teams increasingly want a **dedicated git repository** as the source of truth for
their application configuration and secret wiring — reviewable, auditable,
multi-environment, decoupled from application release cycles. But getting that
git content into the `ConfigMap`/`Secret` objects a workload actually consumes is
bespoke today:

1. **Sync burden.** Something must translate git commits into `ConfigMap`/`Secret`
   updates — usually a hand-rolled CI job or a full GitOps pipeline. It couples
   config changes to deployment tooling and app releases.
2. **Config and secrets are handled by different tools.** Config goes through one
   path (CI → `ConfigMap`) and secrets through another (ESO/Vault/Sealed
   Secrets). There is no single, git-sourced sync that produces both for a
   workload from one reviewed path.
3. **No lightweight, self-service option.** A team that just wants "this pod's
   config comes from that repo/branch/path" has to adopt heavyweight machinery.
   There is no drop-in, env-var-configured syncer.
4. **Multi-environment sprawl.** Representing dev/staging/prod variants inside
   cluster objects or CI templates leads to duplication and drift; a dedicated
   repo with a clear directory/branch model is easier to review — but nothing
   turns it into live cluster objects on its own.

### 2.2 Why not just GitOps?

GitOps controllers (Argo CD, Flux) reconcile Kubernetes API objects toward a
git-declared desired state, and they *can* manage `ConfigMap`s. Kohen is
**complementary and narrower**, and is useful even alongside GitOps because:

- It is purpose-built for the **config repo → `ConfigMap`+`Secret`/`ExternalSecret`**
  transformation of a single path, including the config/secret split, rather than
  reconciling arbitrary manifests.
- It offers a **zero-CRD sidecar mode** configured by pod env vars, so a single
  workload can adopt it without cluster-wide GitOps tooling or pipeline changes.
- It keeps a **dedicated config repository** as the source of truth, distinct from
  the deployment repo GitOps typically reconciles.

Use GitOps to deploy what runs; use Kohen to keep a running workload's
config/secret objects in sync with a dedicated config repo.

### 2.3 Prior art / positioning

| Approach | What it does | How Kohen differs |
| --- | --- | --- |
| CI job rendering `ConfigMap`s | Pipeline turns git into objects | Kohen is a running, declarative sync (operator or sidecar), decoupled from CI and app releases; also handles the secret split |
| GitOps (Argo CD, Flux) | Reconcile cluster desired state from git | Kohen is a narrow git-path→(ConfigMap+Secret) sync with a zero-CRD sidecar mode and a dedicated config repo model; composes with GitOps |
| `git-sync` sidecar | Syncs a git repo into a **volume** | Kohen syncs into **native objects** (ConfigMap/Secret/ExternalSecret), gaining native atomic delivery, fleet consistency, and the config/secret split |
| External Secrets Operator (ESO) | Pulls secrets from external stores into `Secret`s | Kohen handles **config** and **orchestrates** secrets by applying `ExternalSecret`s from git; ESO remains the secret authority |
| Stakater Reloader | Restarts pods when a `ConfigMap`/`Secret` changes | Complementary: Kohen produces the objects from git; Reloader (or file-watch) handles reload |
| Bespoke init scripts | Clone config at startup | One-shot, no live updates, no object model, per-team reinvention |

Kohen's distinguishing combination: **a running git→native-objects sync that
produces both config (`ConfigMap`) and secrets (`Secret`/`ExternalSecret`) from
one reviewed config-repo path, available either as a central operator or as a
drop-in, env-var-configured sidecar, across multiple environments.**

### 2.4 When NOT to use Kohen (and what to use instead)

Kohen is intentionally narrow. Reach for something else when:

- **Your config genuinely exceeds `ConfigMap` limits** (~1 MiB, flat keys, huge
  file trees). Kohen targets object-sized config; a direct-volume `git-sync`
  pattern fits better (a Kohen file-volume mode is a possible future, not v1).
- **You already render `ConfigMap`s from git via GitOps** and do not want a
  separate config repo or a second reconciler. Keep doing that.
- **You only need secrets from an external store.** Use External Secrets Operator
  directly; Kohen adds value when you also want git-sourced config and/or the
  git-declared `ExternalSecret` workflow in the same sync.
- **You only need "restart on `ConfigMap` change" and already author the
  `ConfigMap` by hand.** A `ConfigMap` + Reloader is enough; Kohen adds the git
  source of truth on top.
- **You want to toggle product behavior / run experiments in code.** Use a
  feature-flag platform (OpenFeature-compatible).

Kohen is the right tool when you want a **dedicated git repo (multi-environment)
to be the source of truth for a workload's `ConfigMap` and `Secret`/`ExternalSecret`
objects**, delivered by a running sync that is either centrally operated or
dropped in per-workload via env vars.

---

## 3. Goals & Non-Goals

### 3.1 Goals

- **G1** Continuously synchronize a git `repo@branch:path` into a target
  **`ConfigMap`** (non-secret config) in the workload's namespace.
- **G2** Synchronize sensitive material into a **`Secret`** and/or
  **`ExternalSecret`** (and support other secret manifests, e.g. `SealedSecret`)
  from the same git path, **without plaintext secrets in git** (§8).
- **G3** Offer **two modes** with the same output contract:
  **operator** (CRD-driven, central, multi-workload) and **sidecar**
  (per-workload, configured by pod env vars, no CRDs).
- **G4** Make the **simplistic path trivial**: a sidecar needs only `repo`,
  `branch`, `path` as env vars to work.
- **G5** Support a **multi-environment** repository model (dev/staging/prod, and
  optionally region/tenant) selected by branch and/or path.
- **G6** Keep objects **current** after each commit and, in operator mode,
  guarantee **fleet consistency** (all consumers converge to one resolved
  version).
- **G7** Compose with the ecosystem for **reload** (native mounted-volume updates,
  Reloader, or an optional Kohen-driven rollout) rather than reinventing it.
- **G8** Be **secure by default**: least-privilege git access and least-privilege
  RBAC for writing `ConfigMap`/`Secret` objects; secrets never logged.
- **G9** Be **operable and fail-safe**: a git or Kohen outage must never delete or
  corrupt existing objects or take down healthy workloads.

### 3.2 Non-Goals

- **N1** Kohen is **not** a GitOps/CD engine; it does not reconcile arbitrary
  Kubernetes manifests or deploy applications. It syncs a config path into a
  bounded set of config/secret objects.
- **N2** Kohen is **not** a secrets manager and does **not** store, encrypt, or
  rotate secret material. It **orchestrates** secrets by applying
  `ExternalSecret`/`SealedSecret` manifests from git and by composing native
  `Secret`s from existing references; the external tool remains the authority
  (§8.3).
- **N3** Kohen does **not** own the *schema/validation semantics* of an
  application's config beyond optional structural checks.
- **N4** Kohen is **not** a templating platform competitor to Helm/Kustomize.
  Light, deterministic overlay/substitution MAY be supported (§7.4); complex
  rendering is out of scope for v1.
- **N5** Kohen does **not** deliver config as a large private file tree on a
  volume in v1; the target is `ConfigMap`/`Secret` objects, subject to their size
  limits (§1.3, §13 T-LIMIT). A direct-file-volume mode is a possible future
  (§20).
- **N6** Kohen does **not** provide a UI in v1 (CLI + Kubernetes API only).

---

## 4. Personas & Use Cases

### 4.1 Personas

- **Application developer (Dana).** Owns a service. Wants its `ConfigMap` and
  secret wiring to come from a reviewed PR in a dedicated config repo, and to
  adopt this by adding a sidecar and three env vars — no platform ticket.
- **Platform / SRE engineer (Sam).** Operates the cluster. Wants a central,
  auditable operator that syncs many workloads' config/secret objects from git
  with least-privilege RBAC, strong fleet consistency, metrics, and safe failure
  modes.
- **Security / compliance reviewer (Riley).** Wants config changes reviewed and
  attributable to commits, **no plaintext secrets in git**, secrets sourced via
  ESO/Sealed Secrets, and least-privilege access to write cluster objects.

### 4.2 Primary use cases

- **UC1 — Simplistic sidecar sync.** A workload adds the Kohen sidecar with
  `repo`/`branch`/`path` env vars; Kohen creates and maintains its `ConfigMap`.
- **UC2 — Central operator sync.** A `ConfigSync` resource makes the operator keep
  a workload's `ConfigMap`/`Secret` in sync, with fleet-consistent versions.
- **UC3 — Config + secrets together.** From one git path, Kohen produces a
  `ConfigMap` for config and applies an `ExternalSecret` (populated by ESO) for
  credentials — no plaintext in git.
- **UC4 — Multi-environment.** dev/staging/prod are selected by branch and/or
  path with no duplication of the config payload in manifests.
- **UC5 — Live update + reload.** A merged commit updates the `ConfigMap`; the app
  hot-reloads the mounted volume, or Reloader/an optional Kohen rollout restarts
  it.
- **UC6 — Rollback.** Reverting the repo (or pinning a prior commit) restores the
  previous objects.
- **UC7 — Audit.** For any workload, one can determine which git commit produced
  the currently applied objects.

---

## 5. Terminology (Glossary)

- **Config repository (config repo):** A dedicated git repository that is the
  source of truth for application configuration and secret wiring, distinct from
  application source/deploy repos.
- **Config path:** The subdirectory within the config repo a sync consumes
  (e.g. `services/checkout/prod`).
- **Sync target objects:** The Kubernetes objects Kohen produces and maintains —
  a `ConfigMap` and zero or more `Secret`/`ExternalSecret`/`SealedSecret`.
- **Operator mode:** Kohen running as a cluster controller reconciling
  `ConfigSync` (and `ConfigSource`) resources.
- **Sidecar mode:** Kohen running as a container in a workload's pod, configured
  by pod environment variables, syncing that workload's objects.
- **Config version / config git SHA:** An immutable identifier for a produced set
  of objects — the source git commit SHA (extended with a content/secret digest
  when overlays/secrets contribute). This is the value Kohen stamps on and matches
  against workload metadata.
- **SHA stamp:** The config git SHA recorded on the target workload's pod-template
  metadata (annotation `kohen.dev/config-sha`), which is the version of record and
  the rollout trigger (§11.1, R-VERSION).
- **Reload contract:** How the application picks up updated objects (native
  mounted-volume update, Reloader restart, in-place signal/HTTP, or the default
  SHA-annotation rollout).
- **Fleet consistency:** All consumers of a sync converge to objects produced from
  a single resolved commit; with the rollout contract, guaranteed by construction
  because every rolled pod carries the same SHA stamp.

---

## 6. System Architecture

Kohen has one binary that can run in two modes, plus a repository model and a
git→objects transformation (§7, §8). Both modes produce the **same output
contract**: a `ConfigMap` and optional `Secret`/`ExternalSecret` in the target
namespace, labeled and owned by Kohen.

```
            ┌──────────────────────────────────────────────┐
            │            Config git repository              │
            │   services/checkout/{dev,staging,prod}/...    │
            └───────────────┬──────────────────────────────┘
                            │ fetch repo@branch:path (poll/webhook, read-only)
        ┌───────────────────┴────────────────────┐
        ▼                                         ▼
┌───────────────────────┐              ┌────────────────────────────────┐
│  OPERATOR MODE        │              │  SIDECAR MODE                   │
│  kohen-operator       │              │  kohen sidecar in the pod       │
│  (Deployment)         │              │  configured via pod env vars    │
│  reconciles ConfigSync│              │  (KOHEN_REPO/BRANCH/PATH)       │
└──────────┬────────────┘              └───────────────┬────────────────┘
           │ apply/update (server-side apply, owner refs)                │
           ▼                                                             ▼
   ┌───────────────────────────────────────────────────────────────────────┐
   │ Target namespace                                                        │
   │   ConfigMap  <name>            (non-secret config)                      │
   │   Secret / ExternalSecret      (sensitive material; ESO populates)      │
   └───────────────────────────────┬───────────────────────────────────────┘
                                    │ mounted as volume / envFrom
                                    ▼
                        ┌───────────────────────────┐
                        │  Application workload     │
                        │  reads ConfigMap/Secret;  │
                        │  reload via volume update, │
                        │  Reloader, or rollout      │
                        └───────────────────────────┘
```

### 6.1 The sync (common to both modes)

A single sync cycle: **fetch** `repo@branch:path` → **resolve** the commit →
**transform** into desired objects (config→`ConfigMap`, secrets→`Secret`/
`ExternalSecret`, §8) → **apply** them to the target namespace (server-side
apply, with Kohen ownership labels/annotations and, where applicable, owner
references) → **prune** objects Kohen previously created that no longer exist in
git → **signal reload** per the configured contract (§11).

### 6.2 Operator mode

- A cluster `Deployment` reconciling `ConfigSource` (repo + auth) and `ConfigSync`
  (branch, path, targets, policy) resources (§12).
- **Resolves the branch to a concrete commit once** and applies objects derived
  from that commit, giving **fleet consistency** (R-CONS).
- No per-pod footprint; suited to production and many workloads.
- Least-privilege, namespaced RBAC to write only the objects it manages.
- OPTIONALLY runs a mutating webhook to inject the sidecar (for teams that prefer
  the sidecar UX but centrally managed) and OPTIONALLY receives git webhooks to
  accelerate syncs.

### 6.3 Sidecar mode

- A `kohen` container added to the workload's pod (recommended: an **init**
  invocation to create the objects before the app starts, plus a **sidecar**
  invocation to keep them current).
- Configured **entirely by pod environment variables** (§10). The simplistic
  setup is `KOHEN_REPO` + `KOHEN_BRANCH` + `KOHEN_PATH`.
- No CRDs, no operator required — a drop-in for a single workload.
- Writes the objects via the pod's `ServiceAccount`, which therefore needs RBAC
  to manage the target `ConfigMap`/`Secret` in its namespace (§10.3). This is a
  deliberate trade-off documented for reviewers; strict environments SHOULD use
  operator mode.
- Consistency is **eventual** across replicas (each sidecar resolves the branch
  independently); writes MUST be deterministic and idempotent so concurrent
  replicas converge (R-SIDE-IDEM). For strong fleet consistency, use operator
  mode.

### 6.4 Choosing a mode

| | Operator mode | Sidecar mode |
| --- | --- | --- |
| Setup | `ConfigSource` + `ConfigSync` CRs | Pod env vars only |
| Footprint | One central Deployment | One container per pod |
| Fleet consistency | Strong (single resolved commit) | Eventual (per-replica resolve) |
| RBAC surface | Central, scoped to operator | Workload `ServiceAccount` can write objects |
| Best for | Production, many workloads, strict security | Simplicity, single/low-replica workloads, self-service |

---

## 7. Configuration Repository Model

### 7.1 Requirements

- **R7.1** A config repo MUST be a standard git repository reachable over HTTPS or
  SSH.
- **R7.2** A sync MUST select a **branch** and a **path** (subdirectory) so a
  single repo can serve many workloads and environments.
- **R7.3** The model MUST support multiple environments without duplicating the
  config payload across manifests.

### 7.2 Environment selection

- **Path-based (default):** environments are subdirectories, e.g.
  `services/checkout/{dev,staging,prod}`. Single branch (`main`), simplest review.
- **Branch-based:** each environment is a branch (`env/dev`, `env/prod`).
- **Pinning:** a sync MAY pin to a tag or explicit commit SHA for immutable,
  auditable releases and rollbacks.

Selection MUST be explicit (branch + path, or a pinned ref); no implicit magic.

### 7.3 Layout conventions (recommended, not enforced)

```
config-repo/
└── services/
    └── checkout/
        └── prod/
            ├── app.yaml              # -> ConfigMap key app.yaml
            ├── logging.conf          # -> ConfigMap key logging.conf
            └── kohen.yaml            # optional per-path Kohen config (targets, secrets, split)
```

### 7.4 Config → `ConfigMap` mapping

- **R7.4** By default, each regular file under `path` becomes one **key** in the
  target `ConfigMap` (key = file's path relative to `path`; binary content uses
  `binaryData`). Directory structure is flattened into keys deterministically.
- **R7.5** When the rendered content would exceed `ConfigMap` limits (§13
  T-LIMIT), Kohen MUST fail closed with an actionable error and MAY support
  splitting across multiple named `ConfigMap`s (documented, deterministic).
- **R7.6** Optional light overlay/templating (deep-merge of a `base` + `<env>`,
  bounded variable substitution) MAY be supported per §3.2 N4; if enabled it MUST
  be deterministic. Otherwise content is mapped **verbatim**.

### 7.5 Per-path Kohen config (`kohen.yaml`, optional)

The path MAY contain a `kohen.yaml` that declares target object names, the
config/secret split, secret mappings (§8), and reload policy. In sidecar mode this
lets the repo own the details while the pod supplies only `repo`/`branch`/`path`.
Fields set by env vars or a `ConfigSync` take precedence over `kohen.yaml` per a
documented order; conflicts that cannot be ordered MUST be rejected.

---

## 8. Git → Objects Transformation & Secrets

Kohen never expects **plaintext secrets in git**. It splits a config path into a
non-secret `ConfigMap` and secret objects produced through tools that own the
secret material.

### 8.1 Config vs. secret classification

- **R8.1** Files/keys are classified as **config** (→ `ConfigMap`) unless declared
  **secret**. Secret declaration is explicit via one of:
  - files under a conventional `secrets/` subdirectory of the path, and/or
  - a `secrets:` mapping in `kohen.yaml` / `ConfigSync` (§8.2), and/or
  - recognized secret manifests committed in git (`ExternalSecret`,
    `SealedSecret`) which Kohen applies as-is.
- **R8.2** A file classified as secret MUST NOT be written into the `ConfigMap`.

### 8.2 Secret sources (all consumed via, or producing, a native `Secret`)

- **S1 — `ExternalSecret` (recommended).** The config path contains (or Kohen
  generates from a mapping) an `ExternalSecret`. Kohen **applies** it; External
  Secrets Operator populates the resulting `Secret`. No secret value is in git.
- **S2 — `SealedSecret`.** The path contains an encrypted `SealedSecret`. Kohen
  applies it; the Sealed Secrets controller decrypts it into a `Secret`.
- **S3 — Reference to an existing `Secret`.** A mapping references keys of
  `Secret`s already in the namespace to compose/rename into the target `Secret`.
- **S4 — Native `Secret` created by Kohen (constrained).** Kohen MAY create a
  `Secret` only from material that is itself sourced securely (S1–S3 outputs or an
  explicitly referenced source); it MUST NOT read plaintext secret values from
  git.

Kohen is the **applier/orchestrator**; ESO / Sealed Secrets / the referenced
`Secret` remain the source of truth and rotation authority (N2).

### 8.3 Ownership, pruning & rotation

- **R8.3** Objects Kohen creates MUST carry Kohen ownership labels/annotations and
  (for namespaced consumers) appropriate owner references, so they are
  identifiable, auditable, and safely prunable.
- **R8.4** When a file/secret disappears from git, Kohen MUST prune the object/key
  **it created**; it MUST NOT delete objects it does not own.
- **R8.5** Kohen MUST NOT delete or overwrite a pre-existing object it does not own
  without explicit adopt-on-apply opt-in.
- **R8.6** A referenced/backing secret rotating MUST be reflected: Kohen re-applies
  as needed and, where a rollout/reload is configured, folds a **hash of the
  produced object content** (never the secret value) into the config version so
  rotation triggers reload (R-CONS, §9).

### 8.4 Secret handling requirements

- **R8.7** Secret values MUST NEVER appear in logs, events, metrics, CR status, or
  CLI output; only presence/absence and hashed markers are exposed.
- **R8.8** Kohen MUST NOT persist secret material to disk except as required by
  the applied tool; any transient secret handling MUST use memory only.
- **R8.9** Conflicting/ambiguous secret mappings MUST be rejected at validation
  time, not silently merged.

---

## 9. Consistency & Atomicity Model

Kohen leans on native Kubernetes semantics and adds fleet-level version
resolution.

- **R-ATOM (delivery atomicity).** Mounted `ConfigMap`/`Secret` updates are made
  atomically by kubelet (`..data` symlink swap); Kohen relies on this and MUST
  apply object updates atomically (server-side apply of a fully-formed object,
  never partial writes).
- **R-CONS (fleet consistency, operator mode).** The operator MUST resolve the
  tracked branch to a **single commit** and produce objects from that commit, so
  every consumer of the sync converges to one **config version**. The config
  version MUST incorporate a content digest (including produced-secret hashes,
  R8.6) so any change — git or backing-secret — advances it.
- **R-CONS-SIDE (sidecar mode).** Consistency is eventual; each sidecar MUST
  produce **deterministic, idempotent** object content for a given commit so
  concurrent replicas converge without churn (R-SIDE-IDEM). Strong consistency
  requires operator mode.
- **R-VERSION (version of record).** The authoritative "which config version is
  this workload running" record is the **config git SHA stamped on the workload's
  pod-template metadata** (R11.4). Kohen decides whether a reload/rollout is
  needed by **matching** the desired config version against that stamp (R11.5),
  which makes reconciliation idempotent and makes the deployed version auditable
  directly from `Deployment`/`StatefulSet` metadata (UC7).
- **R-SAFE (no destructive partial state).** A failed/interrupted sync MUST leave
  the last-good objects intact (no truncation, no deletion) (§14).
- **R-ROLLBACK.** Reverting the repo or pinning a prior commit MUST reproduce the
  earlier objects (UC6).

---

## 10. Sidecar Configuration (Environment Variables)

Sidecar mode is configured **only** by environment variables so it is a true
drop-in. The simplistic set is three variables; the rest have safe defaults.

| Env var | Required | Default | Description |
| --- | --- | --- | --- |
| `KOHEN_REPO` | yes | — | Config repo URL (HTTPS/SSH). |
| `KOHEN_BRANCH` | no | `main` | Branch to track (or a pinned tag/commit). |
| `KOHEN_PATH` | no | `""` (repo root) | Path within the repo to sync. |
| `KOHEN_CONFIGMAP` | no | derived from workload name | Target `ConfigMap` name. |
| `KOHEN_SECRET` | no | derived / disabled | Target `Secret` name (if any). |
| `KOHEN_NAMESPACE` | no | pod namespace | Target namespace (defaults to own). |
| `KOHEN_INTERVAL` | no | `30s` | Poll cadence. |
| `KOHEN_MODE` | no | `init+sidecar` | `init`, `sidecar`, or both. |
| `KOHEN_AUTH_SECRET` | no | — | Name of a `Secret` holding git credentials. |
| `KOHEN_RELOAD` | no | `none` | `none｜annotate｜signal｜http｜rollout` (§11). |
| `KOHEN_METRICS_ADDR` | no | `:9095` | Metrics/health bind address. |

- **R10.1** With only `KOHEN_REPO` (and defaults), Kohen MUST perform a valid sync
  of the repo root on `main` into a derived `ConfigMap`.
- **R10.2** The three headline variables `KOHEN_REPO`/`KOHEN_BRANCH`/`KOHEN_PATH`
  MUST be sufficient for the documented simplistic case (G4).
- **R10.3 (RBAC).** Sidecar mode requires the pod's `ServiceAccount` to have a
  namespaced `Role` permitting `create/get/update/patch` on the target
  `ConfigMap`/`Secret` (and `create/patch` on `ExternalSecret`/`SealedSecret` if
  used). When the SHA-annotation rollout contract (C4) is used, the `Role` MUST
  also permit `get/patch` on the target `Deployment`/`StatefulSet`/`DaemonSet` so
  the sidecar can stamp the SHA and trigger a rollout of its own workload. Kohen
  MUST ship this `Role`/`RoleBinding` as an installable manifest and document the
  security implication that a workload SA can then write those objects. Strict
  environments SHOULD prefer operator mode.

> The legacy prototype variables `KOHEN_GIT_URL`, `KOHEN_GIT_PATH`,
> `KOHEN_TARGET_DIR` map to `KOHEN_REPO`, `KOHEN_PATH`, and (semantically) the
> target object; they SHOULD be accepted as deprecated aliases where sensible.

---

## 11. Reload / Consumption Contract

Workloads consume the produced objects natively; Kohen offers a menu of reload
behaviors, selectable per sync (env var `KOHEN_RELOAD` or `ConfigSync.spec.reload`).

- **C0 — Native volume update (default for mounted ConfigMaps).** The app mounts
  the `ConfigMap`/`Secret` and re-reads on kubelet's atomic update. Kohen does
  nothing beyond applying the object.
- **C1 — Reloader annotation.** Kohen sets a content-hash annotation so
  [Stakater Reloader] (or equivalent) restarts the workload on change. Turnkey for
  apps that read config only at startup or via `envFrom`.
- **C2 — Signal.** Kohen (sidecar) sends a configured signal (default `SIGHUP`) to
  the app process after a successful apply.
- **C3 — HTTP hook.** Kohen calls a configured local endpoint (e.g.
  `POST /-/reload`) after a successful apply.
- **C4 — SHA-annotation rollout (default when reload is required).** Kohen stamps
  the resolved **config git SHA** onto the target `Deployment`/`StatefulSet`
  (and `DaemonSet`) metadata and drives a rolling update by mismatch. This is the
  primary, declarative reload mechanism (§11.1) and works for any workload without
  needing in-place reload support.

### 11.1 SHA-annotation rollout (C4) — the primary reload mechanism

Kohen records "which config version this workload is running" **in the workload's
own metadata**, and reconciles a rollout by matching that against the desired
config version.

- **R11.4 (stamp).** On a successful apply, Kohen MUST record the resolved config
  version — the **git commit SHA**, extended with the content/secret digest
  (R-CONS, R8.6) — onto the target workload's **pod-template** metadata, as an
  annotation (recommended: `kohen.dev/config-sha` on `spec.template.metadata`).
  Writing to the pod template (not just the workload's top-level metadata) is what
  makes Kubernetes perform a **rolling update**.
- **R11.5 (match).** Each reconcile MUST compare the **desired** config version
  against the version currently stamped on the workload. Kohen triggers a rollout
  **only when they differ**; when they match, Kohen MUST make **no change** (no
  spurious rollouts — this is the idempotency guarantee, R-SIDE-IDEM).
- **R11.6 (trigger).** When a reload is needed (versions differ), Kohen MUST
  trigger the rollout by updating the pod-template annotation to the new version,
  letting the built-in `Deployment`/`StatefulSet`/`DaemonSet` controller perform
  the rolling update with the workload's own `strategy`/`updateStrategy`. Kohen
  MUST NOT implement its own pod-deletion rollout logic.
- **R11.7 (order).** The annotation update MUST happen **only after** the
  `ConfigMap`/`Secret` objects for the new version are successfully applied
  (R11.2), so new pods start against the new objects.
- **R11.8 (consistency & audit).** Because the SHA lives on the pod template, all
  pods created by the rollout carry the same config version, giving fleet
  consistency by construction; and the currently-applied version is readable
  directly from workload metadata (UC7). Reverting to a prior SHA (rollback, UC6)
  is the same mechanism in reverse.
- **R11.9 (workload kinds).** C4 MUST support `Deployment` and `StatefulSet` and
  SHOULD support `DaemonSet`. The target is named via `reload.workloadRef`
  (operator) or defaulted to the sidecar's owning workload.

> C4 is essentially "Kohen is its own Reloader, keyed on the git SHA": instead of
> hashing rendered content into an annotation for a third-party tool (C1), Kohen
> uses the authoritative git commit SHA as the version and owns the match/trigger
> loop. C1 remains available for teams standardized on Stakater Reloader.

### 11.2 Requirements (all contracts)

- **R11.1** The chosen contract MUST be explicit per sync and documented. When
  reload is required and the app cannot reload in place, C4 is the default.
- **R11.2** A reload/hook/rollout MUST fire only **after** the objects are
  successfully applied.
- **R11.3** Reload success/failure MUST be observable (metric + event/log) and
  MUST NOT corrupt applied objects on failure.

---

## 12. Kubernetes API (CRDs — operator mode)

Operator mode exposes declarative resources. Field names are indicative; the API
MUST be versioned (`v1alpha1` → `v1beta1` → `v1`) and follow Kubernetes API
conventions (status subresource, `observedGeneration`, printer columns, CEL
validation where possible).

### 12.1 `ConfigSource` (namespaced or cluster-scoped)

```yaml
apiVersion: kohen.dev/v1alpha1
kind: ConfigSource
metadata: { name: platform-config, namespace: checkout }
spec:
  url: https://github.com/acme/platform-config.git
  interface: https                       # https | ssh
  auth: { secretRef: { name: kohen-git-creds } }
  verification:                           # optional
    requireSignedCommits: true
    allowedSignersSecretRef: { name: kohen-allowed-signers }
status:
  conditions: [ ... ]
  lastResolvedCommit: 9f1c2ab
```

### 12.2 `ConfigSync` (namespaced) — the primary resource

Describes one git-path → objects synchronization.

```yaml
apiVersion: kohen.dev/v1alpha1
kind: ConfigSync
metadata: { name: checkout-prod, namespace: checkout }
spec:
  sourceRef: { name: platform-config }
  branch: main                           # or pinned tag/commit
  path: services/checkout/prod
  targets:
    configMap: { name: checkout-config }
    secret:    { name: checkout-secrets }        # optional
  secrets:                               # optional (§8); may also live in-repo kohen.yaml
    - key: db-password
      externalSecret: { name: checkout-db }      # ESO-managed; applied by Kohen
    - key: tls-key
      fromSecret: { name: checkout-tls, key: tls.key }
  sync: { interval: 30s }
  reload:
    contract: rollout                    # native | annotate | signal | http | rollout
    workloadRef: { apiVersion: apps/v1, kind: Deployment, name: checkout }  # Deployment|StatefulSet|DaemonSet
    shaAnnotation: kohen.dev/config-sha  # stamped on spec.template.metadata (R11.4)
status:
  configVersion: 9f1c2ab                  # desired version = git SHA + content/secret digest (R-CONS)
  workloadVersion: 9f1c2ab                # version currently stamped on the workload (R-VERSION); rollout fires on mismatch
  rolloutInProgress: false                # true while the built-in controller is rolling
  appliedObjects:
    - { kind: ConfigMap, name: checkout-config }
    - { kind: ExternalSecret, name: checkout-db }
  secrets:                                # resolution state only — never values (R8.7)
    - { key: db-password, resolved: true, via: externalSecret }
  conditions: [ ... ]
```

### 12.3 Requirements

- **R12.1** CRDs MUST validate inputs (OpenAPI/CEL).
- **R12.2** Status MUST report the config version, applied objects, and secret
  resolution state (no values).
- **R12.3** Deleting a `ConfigSync` MUST prune only Kohen-owned objects (R8.4) and
  MUST NOT delete workloads.
- **R12.4** Optional sidecar auto-injection MUST be opt-in per workload
  (annotation/selector), never global-by-default.

---

## 13. Technical Requirements

- **T1 — Language/runtime.** Go (consistent with the prototype and the k8s
  ecosystem); current stable Go (≥ 1.23).
- **T2 — Git access.** Use a maintained Go git library (prototype uses `go-git`);
  shallow fetch and fetch-by-commit to enforce pinning and minimize bandwidth.
- **T3 — Apply semantics.** Object writes MUST use **server-side apply** with a
  stable field manager and Kohen ownership labels; updates MUST be idempotent
  (same commit ⇒ same object, R-SIDE-IDEM).
- **T4 — Kubernetes compatibility.** Support the **N-2** range of supported
  upstream minor versions; operator SHOULD use `controller-runtime`.
- **T-LIMIT — Object size.** Produced `ConfigMap`/`Secret` are bounded by the
  Kubernetes ~1 MiB object limit. Kohen MUST detect oversize and fail closed with
  an actionable error (R7.5); splitting is the supported mitigation.
- **T5 — Distribution.** One multi-arch (`amd64`/`arm64`), minimal
  (distroless/static) image usable as both operator and sidecar; install via Helm
  chart and plain manifests, including the sidecar RBAC `Role` (R10.3).
- **T6 — Footprint.** Small enough to run as a sidecar in every pod (target idle
  < 32 MiB RAM, negligible idle CPU; documented and tested).
- **T7 — Security posture.** Run as non-root on a read-only root filesystem;
  least-privilege RBAC in both modes.
- **T8 — Failure isolation.** A git outage MUST NOT delete/corrupt objects or
  crash the workload; keep last-good objects and surface degraded status.
- **T9 — Determinism.** Given the same commit + inputs, produced object content
  MUST be byte-identical (stable key ordering, stable merges).
- **T10 — Scale (operator).** Reconcile many `ConfigSync`es efficiently (work
  queues, shared informers, rate limiting); dedup git fetches by repo+commit and
  bound fetch concurrency.

---

## 14. Failure Modes & Resilience

| Failure | Required behavior |
| --- | --- |
| Git unreachable (initial) | Retry with backoff to a deadline; sidecar init MAY start the app if a previously-applied object exists; else fail and let restart policy retry; log + event. |
| Git unreachable (steady state) | Keep last-good objects (T8); mark `Degraded`; never delete/prune on inability to fetch. |
| Auth failure | Fail fast with actionable error; event; avoid account-locking retry storms; redact secrets. |
| Malformed/missing path | Do not apply a bad version; keep last-good; `Degraded`; event. |
| Oversize object (T-LIMIT) | Fail closed with a clear error; suggest split; keep last-good. |
| Secret unresolved (ESO not `Ready`, missing ref) | Fail closed for that secret; do not apply a version with unresolved secrets; keep last-good; `Degraded`; event; never log value. |
| Backing secret rotates | Re-apply; content hash advances config version (R8.6) → reload/rollout per contract. |
| Reload/hook/rollout failure | Objects remain applied; report per R11.3; apply configured failure policy. |
| SHA already matches workload (C4) | No-op: MUST NOT patch the workload or trigger a rollout (R11.5) — prevents spurious/looping rollouts. |
| Target workload for rollout not found/wrong kind (C4) | Do not fail the object sync; mark `Degraded` on the reload condition; event; keep objects applied. |
| Rollout stuck/failing (app crashloops on new config) | Kohen relies on the built-in controller + `progressDeadlineSeconds`; MUST surface rollout status and MUST NOT force-delete pods; rollback = stamp the prior SHA (R11.8). |
| Concurrent sidecar writers (replicas) | Idempotent, deterministic apply + SHA match ⇒ convergence, not churn (R-SIDE-IDEM); excess API writes bounded/deduped. |
| Operator down | Applied objects persist unchanged; no new versions until recovery; degrade gracefully. |
| Signature verification failure | Do not apply that commit; security event. |

- **R14.1** Retries MUST use bounded exponential backoff with jitter.
- **R14.2** All failure states MUST appear in status/conditions and metrics, not
  only logs.

---

## 15. Observability

- **R15.1 — Metrics (Prometheus).** At minimum: sync attempts/successes/failures,
  sync duration, current config version (labeled info metric), objects
  applied/pruned, secret-resolution success/failure (counts only, never values),
  reload outcomes, git fetch latency/errors, and degraded-state gauges.
- **R15.2 — Logging.** Structured (JSON), configurable level; secret material
  redacted everywhere (R8.7).
- **R15.3 — Health.** Readiness reflects "a valid version applied"; liveness MUST
  NOT flap on git outages (T8).
- **R15.4 — Events.** Emit Kubernetes events for applies, prunes, version changes,
  and failures.
- **R15.5 — Auditability.** For any workload it MUST be possible to answer "which
  git commit produced the current objects, and when did it change" (UC7).

---

## 16. Non-Functional Requirements

- **NFR1 — Reliability.** Kohen MUST fail safe: its unavailability degrades
  *freshness of objects*, never *availability of healthy workloads* or the
  integrity of already-applied objects.
- **NFR2 — Performance.** Detect+apply a change within a bounded, configurable
  window (default target ≤ 60 s via polling; faster with webhooks).
- **NFR3 — Scalability.** Hundreds of `ConfigSync`es / workloads per operator
  without overloading the git backend (dedup by repo+commit).
- **NFR4 — Compatibility.** Work with mainstream git providers (GitHub, GitLab,
  Bitbucket, self-hosted) over HTTPS/SSH; no proprietary API required for core
  function (webhooks are an optional accelerator). Compose with ESO, Sealed
  Secrets, and Reloader.
- **NFR5 — Portability.** Any conformant Kubernetes; `amd64` + `arm64`.
- **NFR6 — Usability.** The simplistic sidecar path (three env vars) MUST be
  demonstrated end-to-end in getting-started docs; the "when NOT to use" guidance
  (§2.4) MUST be prominent.
- **NFR7 — Documentation.** Concepts, install (Helm + manifests + sidecar RBAC),
  security hardening, CRD reference, sidecar env-var reference, reload cookbook,
  troubleshooting.
- **NFR8 — Licensing.** Permissive OSI license (recommended Apache-2.0) with
  contribution guide and code of conduct.
- **NFR9 — Testing.** Unit + integration (real git server, envtest/`kind`) + e2e
  covering both modes, the config/secret split, oversize handling, and the
  failure modes in §14. CI gates merges.
- **NFR10 — Versioning.** SemVer for artifacts; documented CRD deprecation policy;
  breaking CRD changes require a new API version with conversion.

---

## 17. Configuration & CLI Reference

- **Sidecar mode** — configured by environment variables (§10).
- **Operator mode** — configured by `ConfigSource`/`ConfigSync` (§12) plus a
  controller config file (leader election, metrics, concurrency).
- **`kohenctl` (optional helper, SHOULD).**
  - `kohenctl status <sync>` — config version, applied objects, convergence (UC7).
  - `kohenctl diff <sync>` — diff between applied and latest (secrets redacted,
    R8.7).
  - `kohenctl pin <sync> <sha>` / `kohenctl rollback <sync>` — set version (UC6).
  - `kohenctl verify <repo@branch:path>` — validate layout/signatures locally.

---

## 18. Milestones / Phased Roadmap

Ordered by dependency, not calendar. Each milestone is independently shippable.

- **M0 — Spec & foundations (this document).** Requirements, glossary,
  architecture; repo scaffolding, license, CI skeleton.
- **M1 — Sidecar MVP.** `kohen` init+sidecar, env-var config
  (`KOHEN_REPO`/`BRANCH`/`PATH`, R10.1–R10.2), git fetch, config→`ConfigMap`
  (§7.4), server-side apply + ownership + prune (T3, R8.3–R8.4), sidecar RBAC
  manifest (R10.3), native volume reload (C0) **and the SHA-annotation rollout
  (C4, §11.1): stamp `kohen.dev/config-sha` on the workload pod template, match
  desired-vs-stamped, trigger a rolling update only on mismatch** (R11.4–R11.9),
  metrics/health, fail-safe (§14). Delivers UC1, UC5.
- **M2 — Secrets.** Config/secret split (§8.1), `ExternalSecret` (S1) and
  `SealedSecret` (S2) apply, existing-`Secret` references (S3), fail-closed +
  rotation-triggered rollout via the SHA stamp (R8.6, R11.4), redaction (R8.7).
  Delivers UC3.
- **M3 — Operator & CRDs.** `ConfigSource` + `ConfigSync`, branch→commit
  resolution + fleet consistency (R-CONS), workload SHA matching across the fleet,
  status/events, Helm chart. Delivers UC2, UC4, UC7 with strong consistency.
- **M4 — Reload completeness.** Reloader annotation (C1), in-place signal/HTTP
  hooks (C2, C3), rollback (UC6, R-ROLLBACK) via prior-SHA stamp, `kohenctl`.
- **M5 — Injection & webhooks.** Optional sidecar auto-injection (R12.4),
  git-webhook-triggered syncs, signed-commit verification.
- **M6 — Advanced.** Overlay/light templating (§7.4), `ConfigMap` splitting for
  large config, progressive rollout strategies, tracing, image signing/SBOM.

---

## 19. Acceptance Criteria

- **A1** A pod with the Kohen sidecar and only `KOHEN_REPO`/`KOHEN_BRANCH`/
  `KOHEN_PATH` set produces and maintains the expected `ConfigMap`, verified in an
  e2e `kind` test (R10.1–R10.2).
- **A2** Committing a change updates the `ConfigMap`; a mounted-volume consumer
  observes it (C0) and a Reloader-annotated consumer restarts (C1).
- **A2b** With the rollout contract (C4): a commit stamps the new config git SHA
  on the `Deployment`/`StatefulSet` pod template and triggers exactly one rolling
  update; a subsequent reconcile with an unchanged SHA triggers **no** rollout
  (idempotent match, R11.4–R11.6); the deployed version is readable from workload
  metadata (R-VERSION, UC7).
- **A3** A git outage does not delete/corrupt objects or crash the workload;
  status shows `Degraded`; recovery re-applies automatically (T8, §14).
- **A4** From one path, config lands in a `ConfigMap` and a secret is delivered via
  an applied `ExternalSecret` (ESO-populated) with **no plaintext in git**, logs,
  events, status, or CLI (§8, R8.7).
- **A5** An unresolved secret fails closed (last-good kept, `Degraded`, event);
  rotating the backing secret advances the config version and drives the reload
  contract (R8.6).
- **A6** In operator mode, all consumers of a `ConfigSync` converge to objects from
  a single resolved commit (R-CONS); status reports it (UC7).
- **A7** Deleting a `ConfigSync`/sidecar prunes only Kohen-owned objects and never
  deletes the workload (R8.4, R12.3).
- **A8** Producing an oversize object fails closed with an actionable error rather
  than a partial/rejected apply (T-LIMIT).
- **A9** The sidecar runs as non-root on a read-only root filesystem within the
  documented footprint (T6, T7).

---

## 20. Open Questions

1. **Sidecar multi-replica coordination.** Idempotent last-writer-wins is simple
   but produces redundant API writes across replicas. Do we add optional leader
   election among a workload's Kohen sidecars, or firmly steer multi-replica
   production to operator mode?
2. **Object naming/derivation defaults.** How should the default `ConfigMap`/
   `Secret` names be derived in sidecar mode (workload name? pod owner? a fixed
   suffix?) to be predictable and collision-safe?
3. **Config/secret split ergonomics.** Is the `secrets/` convention + `kohen.yaml`
   mapping the right default, or should Kohen only ever apply explicit secret
   manifests (`ExternalSecret`/`SealedSecret`) committed to git and never infer?
4. **Adopting pre-existing objects.** Default is to never touch un-owned objects
   (R8.5). Is opt-in adoption needed for migration from hand-authored objects?
5. **ESO readiness coupling.** Block applying the config version until the
   `ExternalSecret` is `Ready` (safer) vs. proceed if a prior `Secret` exists
   (more available)? (R8.6 vs. NFR1.)
6. **Direct-file-volume mode (deferred, N5).** Is there enough demand for very
   large config trees to justify a future volume-target mode alongside the object
   model?
7. **Webhook ingress topology.** Supported relay model for git webhooks in
   restricted-egress clusters.

---

## 21. Relationship to the Existing Prototype

The repository contains an early `kohen-agent` prototype (Go + `go-git`) that
clones a repo into a target **directory** via `--gitUrl`/`--gitPath`/
`--targetDir`, and reads matching env vars. It validates the fetch-from-git and
env-var-configuration premises and maps onto the **sidecar init** phase of **M1**
— except the target evolves from a filesystem directory to native
`ConfigMap`/`Secret` **objects** (the v0.3 core model). The prototype's env vars
are retained as deprecated aliases (§10).

---

## Appendix A — Requirement Index

Requirements are labeled inline (`Rn.n`, `R-*`, `Tn`, `Cn`, `An`, `Gn`, `Nn`,
`NFRn`, `UCn`) so implementation PRs and tests can reference them directly. An
implementation is spec-conformant for a milestone (§18) when all requirements
reachable from that milestone's capabilities and their acceptance criteria (§19)
are satisfied and demonstrated by automated tests (NFR9).
