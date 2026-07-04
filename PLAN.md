# KOHEN — Implementation Plan (to v1.0)

> Companion to [`SPEC.md`](./SPEC.md) (Draft v0.5). This plan sequences the work
> from an empty repo to a **v1.0** operator. It is written so that **each step
> can be picked up and delivered independently by a separate agent**: every step
> lists its dependencies, scope, deliverables, tests, and a self-contained
> Definition of Done, and references the SPEC requirements it satisfies.
>
> Usability testing (real-cluster, end-to-end, `kind`-based) is called out as its
> **own milestones** (`U1`–`U3`), separate from the unit/integration testing that
> lives inside each build step.

---

## How to use this plan

- **One step = one deliverable = one PR** (branch `cursor/<short-name>-…`). An
  agent should be able to complete a step knowing only: this plan entry, the
  referenced SPEC sections, and the artifacts produced by the step's declared
  dependencies.
- **Dependencies are explicit.** A step MUST NOT start until its `Depends on`
  steps are merged. Steps with no ordering relationship MAY be built in parallel
  (noted as *Parallelizable*).
- **Every step ships tests.** No step is "done" without the test tier(s) listed
  in its entry passing in CI (see *Testing tiers*).
- **Every step updates docs it touches** (CRD reference, flags, backend guide) so
  documentation is never a separate catch-up effort (NFR7).
- **Interfaces over implementations.** Where a step produces a package other steps
  consume, it MUST define a small, stable Go interface and document it, so
  downstream agents can depend on the contract, not the internals.

### Definition of Done (applies to every step unless overridden)

1. Code + tests merged; CI green (lint, unit, and any integration/e2e tiers the
   step declares).
2. Public interfaces documented; SPEC/README/CRD docs updated as needed.
3. No secret material in logs/events/status/artifacts (SPEC R8.3) where relevant.
4. Backward-compatible with already-merged steps, or the break is called out and
   the affected steps' entries updated.

---

## Testing tiers

The plan uses three tiers. Each step declares which it needs; usability
milestones are Tier 3 focused on user journeys.

- **Tier 1 — Unit.** Pure Go, no cluster. Deterministic; fast; run on every PR.
- **Tier 2 — Integration.**
  - *envtest* (controller-runtime: real API server + etcd, **no kubelet**) for
    reconcile logic, CRD validation, Server-Side Apply ownership/merge, and status.
  - *git server fixture* (a temporary bare repo or an in-process/containerized git
    server) for the git source library.
  - Runs on PRs touching the relevant packages.
- **Tier 3 — E2E / Usability (`kind`).** A real cluster with kubelet: install
  Kohen via Helm, run real workloads, real controllers (ESO, Sealed Secrets, a
  GitOps controller), assert **user-visible** outcomes (ConfigMap present, rollout
  happened, secret available in the pod, no flapping). This tier backs the `U*`
  milestones and the final acceptance suite. Runs on merges to the main branch
  and nightly (kind is too heavy for every PR, but each `U*` milestone wires its
  scenarios into CI).

> Rationale for `kind`: it gives a genuine kubelet + API server locally and in
> CI, so mounted-volume atomic updates, rollouts, CSI drivers, and admission
> behavior are exercised for real — which envtest cannot do. Alternatives
> (k3d/minikube) are acceptable but `kind` is the default target.

---

## Dependency graph (high level)

```
Phase 0  Foundations        S0.1 → S0.2
Phase 1  Config-only        S1.1 ─┐
         operator (MVP)     S1.2 ─┤
                            S1.3 ─┼→ S1.4 → S1.5 → S1.6 → S1.7 → S1.8  ──►  [U1]
Phase 2  Secret core        S2.1 → S2.2 → S2.3 → S2.4                    (needs S1.8)
Phase 3  Secret backends    S3.1 (ESO) ─┬─ S3.2 (Sealed)                 (needs S2.4)
                            S3.3 (Vault) ┴─ S3.4 (CSI)          ──────►  [U2]
Phase 4  Ergonomics         S4.1 … S4.6                          (needs S2.4 / S1.8)
Phase 5  Hardening          S5.1 … S5.6                          (needs Phase 1–3)
Final    Acceptance                                             ──────►  [U3] → v1.0
```

`[U1]`, `[U2]`, `[U3]` are the usability-testing milestones.

---

## Phase 0 — Foundations

### S0.1 — Repository scaffolding & CI
- **Depends on:** none.
- **Goal:** A clean Go project layout, build, lint, unit-test harness, multi-arch
  container image, and licensing/governance files.
- **Scope (in):** Restructure the repo (operator module under e.g. `cmd/`,
  `internal/`, `api/`); `Makefile` (`build`, `test`, `lint`, `image`, `manifests`);
  golangci-lint config; GitHub Actions (or equivalent) for lint + unit; multi-arch
  (`amd64`/`arm64`) distroless image build; `LICENSE` (Apache-2.0), `CONTRIBUTING`,
  `CODE_OF_CONDUCT`, `SECURITY.md`.
- **Scope (out):** Any Kohen behavior.
- **Deliverables:** Building repo skeleton; green CI; publishable image target.
- **Testing:** Tier 1 (a trivial unit test proves the harness); CI builds the
  image.
- **Definition of Done:** `make build test lint image` succeed locally and in CI.
- **SPEC refs:** T1, T5, NFR8, M0.

### S0.2 — Test harness & fixtures
- **Depends on:** S0.1. *Parallelizable with S1.1 after interfaces agreed.*
- **Goal:** Reusable test infrastructure every later step relies on.
- **Scope (in):** envtest bootstrap helper; git-server fixture (spin up a temp
  bare repo / lightweight git server, seed commits, return `repo@branch:path`);
  a `kind` e2e harness (cluster up/down, load locally-built image, install CRDs +
  Helm chart, teardown) with a documented `make e2e` entrypoint; golden-file
  helpers; secret-leak assertion helper (scan logs/objects for known secret
  values).
- **Deliverables:** `test/` packages + scripts; docs on running each tier.
- **Testing:** Harness self-tests (Tier 1/2); a smoke `kind` job that just
  creates/destroys a cluster (Tier 3) to prove the harness in CI.
- **Definition of Done:** Other steps can call the fixtures; smoke kind job green.
- **SPEC refs:** NFR9.

---

## Phase 1 — Config-only operator (MVP)

Delivers UC1, UC4, UC5, UC7 for **config** (no secrets yet): git → `ConfigMap`,
workload wiring via SSA merge, and SHA-matched rollout.

### S1.1 — Git source library
- **Depends on:** S0.2.
- **Goal:** Fetch and resolve config content from git deterministically and
  securely.
- **Scope (in):** `repo@branch:path` fetch; resolve branch/tag → concrete commit;
  shallow fetch / fetch-by-commit; HTTPS token & SSH key auth read from a
  `Secret`; TLS + SSH host-key verification on by default; return the file set at
  `path` + the resolved commit SHA. Define a `Source` interface.
- **Scope (out):** Rendering; k8s objects; webhooks; signature verification (S5.2).
- **Deliverables:** `internal/git` package + interface.
- **Testing:** Tier 1 (URL/ref parsing, auth selection); **Tier 2** against the
  git-server fixture (branch resolution, subpath extraction, auth failure,
  unreachable-host behavior).
- **Definition of Done:** Given a fixture repo, returns the exact file tree +
  commit SHA; verification defaults enforced.
- **SPEC refs:** R7.1–R7.2, R9-adjacent (auth), T2, T8.

### S1.2 — Config renderer (files → ConfigMap data)
- **Depends on:** S0.2. *Parallelizable with S1.1.*
- **Goal:** Turn a file tree into deterministic `ConfigMap` data.
- **Scope (in):** File tree → keys (`data`/`binaryData`), deterministic flattening
  & ordering; **oversize detection** (fail closed near the ~1 MiB limit) with an
  actionable error; verbatim mapping only.
- **Scope (out):** Overlay/templating (S4.3); splitting (S4.4).
- **Deliverables:** `internal/render` package + interface producing a desired
  `ConfigMap` spec + a content digest.
- **Testing:** Tier 1 golden files; determinism (same input ⇒ byte-identical);
  oversize triggers a clear error.
- **Definition of Done:** Deterministic, oversize-safe rendering with a stable
  digest.
- **SPEC refs:** R7.4, R7.5, T9, T-LIMIT.

### S1.3 — `ConfigSync` CRD & API types
- **Depends on:** S0.1. *Parallelizable with S1.1/S1.2.*
- **Goal:** The primary API surface.
- **Scope (in):** `api/v1alpha1` types for `ConfigSync` (source, path, configMap,
  `secretRefs` shape reserved, workloadRef, reload, sync), deepcopy, CRD manifests,
  OpenAPI + CEL validation, printer columns, status subresource + conditions.
- **Scope (out):** Reconcile behavior (S1.4+).
- **Deliverables:** Generated CRD YAML + Go types.
- **Testing:** **Tier 2** envtest: CRD installs; valid CRs accepted; invalid CRs
  rejected by CEL; defaulting applied.
- **Definition of Done:** CRD round-trips through the API server with validation.
- **SPEC refs:** §11, R11.1–R11.2.

### S1.4 — Operator skeleton
- **Depends on:** S1.3.
- **Goal:** A runnable controller with no business logic yet.
- **Scope (in):** controller-runtime manager; `ConfigSync` reconciler stub
  (records `observedGeneration`, sets a `Ready` condition); leader election;
  metrics + health endpoints; structured JSON logging; least-privilege RBAC
  manifests; Helm chart (operator Deployment, RBAC, CRDs).
- **Deliverables:** `cmd/operator`, `internal/controller`, `deploy/helm`.
- **Testing:** **Tier 2** envtest: manager starts, reconciles a CR to `Ready`,
  metrics/health serve.
- **Definition of Done:** Operator installs via Helm and reconciles a no-op CR.
- **SPEC refs:** §6.3, T1, T7, R14.1/R14.3, G9.

### S1.5 — Object apply/prune engine
- **Depends on:** S1.4.
- **Goal:** Safe, idempotent creation/update/pruning of Kohen-owned objects.
- **Scope (in):** Server-Side Apply with a dedicated field manager + ownership
  labels; create/update `ConfigMap`; **prune** owned objects that disappear;
  **never** adopt/overwrite un-owned objects without explicit opt-in.
- **Scope (out):** Workload mutation (S1.6); applying secret manifests (S2.4).
- **Deliverables:** `internal/apply` package + interface.
- **Testing:** **Tier 2** envtest: apply/update idempotency; prune-on-disappear;
  refusal to touch un-owned objects.
- **Definition of Done:** ConfigMaps converge idempotently; pruning is owner-safe.
- **SPEC refs:** T3, R8.3-adjacent, R11.3–R11.4, R-ATOM.

### S1.6 — Workload SSA merge & GitOps coexistence
- **Depends on:** S1.5.
- **Goal:** Wire owned fields into a workload without fighting other managers.
- **Scope (in):** SSA merge into `Deployment`/`StatefulSet` pod template using the
  Kohen field manager; inject **only** owned fields (start with a `ConfigMap`
  volume/mount + the SHA annotation placeholder); keyed/granular list merges;
  conflict handling (re-read + re-apply owned fields only); ownership labels;
  authored **GitOps ignore-rule docs** (Argo CD `ignoreDifferences`, Flux
  exclusions).
- **Deliverables:** `internal/wire` package; docs section.
- **Testing:** **Tier 2** envtest: field-ownership assertions; simulate a
  competing field manager and assert no clobber; injected fields are pruned on
  removal.
- **Definition of Done:** Kohen-owned fields merge in and survive a competing
  manager without clobbering it (foundation for A10).
- **SPEC refs:** §6.2, R-WIRE.1–R-WIRE.5.

### S1.7 — SHA-matched rollout
- **Depends on:** S1.6.
- **Goal:** Roll the workload only when the config version changes.
- **Scope (in):** Compute config version (commit SHA + content digest); stamp on
  pod-template metadata via S1.6; **match** desired vs. stamped; trigger rollout
  only on mismatch (no-op on match); populate status (`configVersion`,
  `workloadVersion`, `rolloutInProgress`); emit events; support `Deployment` +
  `StatefulSet`.
- **Scope (out):** Secret-hash contribution (S2.2); `DaemonSet` (S4.5).
- **Deliverables:** rollout logic in `internal/controller`.
- **Testing:** **Tier 2** envtest: change ⇒ exactly one stamp update; no change ⇒
  no write; status reflects versions.
- **Definition of Done:** Idempotent, SHA-driven rollout with accurate status.
- **SPEC refs:** §9 R-VERSION, R-ROLLOUT.1–.4, R-ROLLBACK.

### S1.8 — Reconcile loop integration & fail-safe
- **Depends on:** S1.1, S1.2, S1.7.
- **Goal:** Assemble the full config-only reconcile and make it robust.
- **Scope (in):** Wire fetch → render → apply → wire → stamp/rollout → status;
  bounded backoff + jitter; keep-last-good on git outage; `Degraded`/`Pending`
  conditions; requeue policy; dedup git fetches by repo+commit.
- **Deliverables:** Complete config-only operator.
- **Testing:** **Tier 2** envtest + git fixture, incl. fault injection (git
  unreachable, malformed path, oversize) asserting last-good is preserved.
- **Definition of Done:** End-to-end config sync works in envtest; failures never
  corrupt/delete objects.
- **SPEC refs:** §6.1, §13, T8, T10, R13.1–R13.2.

---

## 🧪 Usability Milestone U1 — Config sync & rollout on `kind`

- **Depends on:** S1.8.
- **Goal:** Prove the **config-only** user journeys on a real cluster, including
  GitOps coexistence — this is integration testing of the *experience*, not units.
- **Environment:** `kind` cluster; Kohen installed via Helm; an in-cluster git
  server seeded with a config repo; a sample `Deployment` consuming the ConfigMap.
- **Scenarios (each automated in CI + captured in a runbook):**
  1. Create a `ConfigSync` → `ConfigMap` appears, workload is wired, pod mounts it.
  2. Commit a config change → mounted volume updates (C0) **and** exactly one
     rolling update occurs; a follow-up reconcile with no change ⇒ **no** rollout.
  3. The applied config git SHA is readable from workload metadata + CR status.
  4. Rollback (revert commit / pin prior SHA) → prior version restored + rolled.
  5. Git outage → CR `Degraded`, workload stays healthy, auto-recovers.
  6. Delete `ConfigSync` → only Kohen-owned objects/fields pruned; workload intact.
  7. **GitOps coexistence:** install Argo CD **or** Flux managing the same
     `Deployment`; apply the documented ignore rules; assert **no flapping** and
     both controllers converge (A10).
- **Deliverables:** `test/e2e` suite + CI job (kind) + a "Getting Started &
  GitOps" runbook.
- **Definition of Done:** All scenarios pass in CI on kind; runbook verified.
- **SPEC refs:** A1, A2, A3, A7, A10, UC1, UC4, UC5, UC7, NFR6.

---

## Phase 2 — Secret resolution core

Delivers UC2/UC3 mechanics (backend-agnostic parts).

### S2.1 — Secret reference model & API
- **Depends on:** S1.3.
- **Goal:** Declare secret references and surfacing.
- **Scope (in):** Extend `ConfigSync.spec.secretRefs` + in-repo `kohen.yaml`
  parsing; backend enum (`externalSecret|nativeSecret|sealedSecret|vault|csi`);
  `surface` (`file`/`env`) schema; validation (reject conflicts/ambiguity);
  status shape for per-ref resolution state (no values).
- **Deliverables:** API additions + parser.
- **Testing:** Tier 1 parse/validate; **Tier 2** envtest CEL rejection of bad
  refs.
- **Definition of Done:** References validate; conflicts rejected (R8.7).
- **SPEC refs:** §8.1, §8.5 R8.7, §11.

### S2.2 — Resolution framework (readiness policy, fail-closed, redaction)
- **Depends on:** S2.1, S1.8.
- **Goal:** The backend-independent resolution engine and its safety semantics.
- **Scope (in):** `Resolver` interface (input: reference; output: resolved
  material identity + readiness); **assume-exists/reconcile**; **fail-closed** on
  unresolved (R8.4); **asymmetric readiness policy** (first-resolution fail-closed,
  update fail-safe, R8.9); fold a resolved-secret **hash** (never the value) into
  the config version (extends S1.7); redaction guarantees everywhere.
- **Deliverables:** `internal/secret` framework + a fake backend for tests.
- **Testing:** Tier 1 state-machine tests for the readiness policy; **Tier 2**
  envtest with fake Secrets (first-resolution blocks rollout; prior-good survives
  a transient not-ready; rotation advances version); secret-leak assertion.
- **Definition of Done:** Readiness/fail-closed behavior provably correct with a
  fake backend; no leaks.
- **SPEC refs:** §8.1 P3, §8.5 R8.3–R8.5, R8.9, NFR1.

### S2.3 — Surfacing mechanics (file/env wiring)
- **Depends on:** S2.2, S1.6.
- **Goal:** Make a resolved secret available in the pod via existing mechanics.
- **Scope (in):** Extend the SSA merge (S1.6) to inject `Secret`/CSI volumes +
  mounts (file) and `secretKeyRef`/`envFrom` (env); owned-field markers; feed the
  resolved-secret hash into the rollout for env-surfaced secrets.
- **Deliverables:** surfacing in `internal/wire`.
- **Testing:** **Tier 2** envtest wiring assertions (file + env), ownership, and
  no clobber of un-owned pod fields.
- **Definition of Done:** Resolved secrets appear in the pod spec via both
  mechanics, owner-safe.
- **SPEC refs:** §8.4, R-WIRE.\*, R8.5.

### S2.4 — Native backend + apply-if-present manifest engine
- **Depends on:** S2.3, S1.5.
- **Goal:** First concrete backend + the "apply owned secret manifests from git"
  capability.
- **Scope (in):** **Native `Secret`** backend (reference by name/keys, await
  existence, wire). Generalize S1.5 to **apply** `ExternalSecret`/`SealedSecret`
  manifests found in git (owned + pruned, R8.8) — the shared engine used by S3.x;
  when absent, await (R8.2).
- **Deliverables:** `internal/secret/native`, apply-manifest support.
- **Testing:** **Tier 2** envtest: native ref resolves and wires; a committed
  generic secret manifest is applied/pruned; absent ⇒ awaits.
- **Definition of Done:** Native path works end-to-end in envtest; manifest engine
  ready for real backends.
- **SPEC refs:** §8.2, §8.3 (native row), R8.2, R8.8.

---

## Phase 3 — Secret backends

Each backend is an independent step over the S2.4 engine. **Parallelizable** with
each other once S2.4 is merged. Each requires a Tier-3 (`kind`) integration
because it depends on a real external controller/driver.

### S3.1 — External Secrets Operator (primary)
- **Depends on:** S2.4.
- **Goal:** First-class ESO integration.
- **Scope (in):** Reference an `ExternalSecret`/its target `Secret`; **apply** the
  `ExternalSecret` from git if present; **await `Ready`** per R8.9; wire the
  resulting `Secret`.
- **Testing:** **Tier 3** kind with ESO installed and a dev/fake provider (e.g.
  the ESO fake or a local provider): apply → Ready → wired; not-ready path honors
  the readiness policy.
- **Definition of Done:** ESO journey green on kind; no plaintext leakage.
- **SPEC refs:** §8.3 (ESO), R8.2, R8.9, G3, UC3.

### S3.2 — Sealed Secrets
- **Depends on:** S2.4. *Parallelizable with S3.1/S3.3/S3.4.*
- **Goal:** Support committing encrypted secrets in git.
- **Scope (in):** Apply a `SealedSecret` from git if present; await the decrypted
  `Secret`; wire it.
- **Testing:** **Tier 3** kind with the sealed-secrets controller: sealed → Secret
  → wired.
- **Definition of Done:** Sealed Secrets journey green on kind.
- **SPEC refs:** §8.3 (Sealed), R8.2.

### S3.3 — HashiCorp Vault (Injector / CSI paths)
- **Depends on:** S2.4. *(ESO→Vault is already covered by S3.1.)*
- **Goal:** Support Vault where ESO is not used.
- **Scope (in):** For Agent Injector: ensure required pod annotations are present
  (owned). For CSI: ensure the CSI volume/mount + `SecretProviderClass` reference
  are wired.
- **Testing:** **Tier 3** kind with Vault (dev) + injector and/or CSI provider.
- **Definition of Done:** At least one Vault path (injector or CSI) green on kind.
- **SPEC refs:** §8.3 (Vault).

### S3.4 — Secrets Store CSI Driver
- **Depends on:** S2.4. *Parallelizable.*
- **Goal:** Mount-based secrets via the CSI driver.
- **Scope (in):** Wire the `SecretProviderClass` reference + CSI volume/mount into
  the pod; optional sync-to-`Secret` handling.
- **Testing:** **Tier 3** kind with the CSI driver + a mock/dev provider.
- **Definition of Done:** CSI journey green on kind.
- **SPEC refs:** §8.3 (CSI).

---

## 🧪 Usability Milestone U2 — Secret integration on `kind`

- **Depends on:** S3.1 (minimum); extend as S3.2–S3.4 land.
- **Goal:** Prove the **secret** user journeys end-to-end on a real cluster.
- **Scenarios (automated + runbook):**
  1. Config references a secret backed by an `ExternalSecret` committed to git →
     Kohen applies it, awaits `Ready`, wires it as **file** and as **env**; the
     value is present in the pod; **no plaintext** anywhere (git/logs/events/
     status/CLI).
  2. **First-resolution fail-closed:** reference a not-yet-existing secret → CR
     `Pending`, **no rollout**; create it → resolves and rolls out.
  3. **Update fail-safe:** with a running good version, make the backend
     transiently not-ready → workload keeps running, CR `Degraded`, recovers.
  4. **Rotation:** rotate an env-surfaced secret → config version advances →
     rollout; volume-surfaced secret updates in place.
  5. Repeat (1) for native + Sealed Secrets backends.
- **Deliverables:** `test/e2e/secrets` + CI job; secret-backend integration guide.
- **Definition of Done:** All scenarios pass on kind; leak scanner clean.
- **SPEC refs:** A4, A5, A6, UC2, UC3.

---

## Phase 4 — Ergonomics & ecosystem

*Parallelizable* once Phase 1 (and, for S4.x touching secrets, Phase 2) is merged.

### S4.1 — `kohenctl`
- **Depends on:** S1.8 (status/pin/rollback), S2.2 (redaction for diff).
- **Goal:** Operator-facing CLI.
- **Scope (in):** `status`, `diff` (secrets redacted), `pin`, `rollback`,
  `verify`.
- **Testing:** Tier 1 command logic; **Tier 3** smoke against a kind cluster.
- **DoD:** Commands work against a live `ConfigSync`; diff never prints secrets.
- **SPEC refs:** §15, R8.3, UC6, UC7.

### S4.2 — Reloader interop
- **Depends on:** S1.7.
- **Goal:** Optional Stakater Reloader annotation for hot-reload teams.
- **Scope (in):** Content-hash annotation option; docs on choosing C0/C1/rollout.
- **Testing:** **Tier 3** kind with Reloader installed.
- **DoD:** Reloader restarts on change when enabled.
- **SPEC refs:** §10 C1.

### S4.3 — Overlay / light templating
- **Depends on:** S1.2.
- **Goal:** Deterministic `base` + `<env>` deep-merge and bounded substitution.
- **Scope (in):** Opt-in overlay; documented, deterministic merge; bounded
  variable set; verbatim remains the default.
- **Testing:** Tier 1 golden + determinism; envtest for digest stability.
- **DoD:** Overlay output is deterministic and folds into the config version.
- **SPEC refs:** §7.4 R7.6, T9, N4.

### S4.4 — ConfigMap splitting for large config
- **Depends on:** S1.2, S1.5.
- **Goal:** Handle configs near/over the object-size limit.
- **Scope (in):** Deterministic split across multiple named `ConfigMap`s; wiring
  of all parts; oversize-per-part safety.
- **Testing:** Tier 1 split determinism; **Tier 2** envtest multi-ConfigMap apply.
- **DoD:** Large config splits deterministically and mounts correctly.
- **SPEC refs:** §7.5 R7.5, T-LIMIT.

### S4.5 — `DaemonSet` rollout support
- **Depends on:** S1.7.
- **Goal:** Extend SHA rollout to `DaemonSet`.
- **Testing:** **Tier 2** envtest + **Tier 3** kind (DaemonSet rollout).
- **DoD:** DaemonSet rolls on SHA change.
- **SPEC refs:** R-ROLLOUT.1, R11.9-adjacent.

### S4.6 — Audit & rollback polish
- **Depends on:** S1.7.
- **Goal:** Make version history and rollback first-class.
- **Scope (in):** Status history / optional `ConfigRelease` record; events for
  version transitions; `kohenctl rollback` UX.
- **Testing:** **Tier 2** envtest; **Tier 3** kind rollback journey.
- **DoD:** Operators can audit and roll back with confidence.
- **SPEC refs:** UC6, UC7, R-ROLLBACK, R14.4–R14.5.

---

## Phase 5 — Hardening & acceleration

### S5.1 — Git webhooks
- **Depends on:** S1.8.
- **Goal:** Near-immediate syncs beyond polling.
- **Scope (in):** Webhook receiver; signature/secret verification; trigger a sync
  (trust the fetched pinned commit, not the payload); restricted-egress guidance.
- **Testing:** Tier 1 signature verification; **Tier 3** kind with a simulated
  provider webhook.
- **DoD:** A verified webhook accelerates sync; spoofed/replayed events are safe.
- **SPEC refs:** §6.3, NFR2, NFR4.

### S5.2 — Signed-commit verification
- **Depends on:** S1.1.
- **Goal:** Refuse to act on untrusted commits.
- **Scope (in):** GPG/SSH signature verification against an allow-list; block
  acting on unverified commits; security events.
- **Testing:** Tier 1/2 with signed & unsigned fixture commits.
- **DoD:** Unsigned/untrusted commits are not acted on.
- **SPEC refs:** §6.3, §13 (signature row).

### S5.3 — Progressive rollout strategies (optional for v1)
- **Depends on:** S1.7.
- **Goal:** Canary/soak beyond the workload's native strategy, if warranted.
- **Scope (in):** Optional staged version propagation; **may be deferred past
  v1.0** if native strategies suffice (decide during planning review).
- **Testing:** **Tier 3** kind, if built.
- **DoD:** Either shipped-with-tests or explicitly deferred with rationale.
- **SPEC refs:** §9 (rollout), roadmap M5.

### S5.4 — Supply chain (signing, SBOM, provenance)
- **Depends on:** S0.1.
- **Goal:** Trustworthy release artifacts.
- **Scope (in):** Reproducible builds; cosign image signing; SBOM; provenance
  attestation in CI.
- **Testing:** CI verifies signature + SBOM presence on release artifacts.
- **DoD:** Released images are signed with an attached SBOM.
- **SPEC refs:** R9.8-adjacent (SPEC security), NFR10.

### S5.5 — Security & RBAC hardening
- **Depends on:** Phases 1–3.
- **Goal:** Least privilege, verified.
- **Scope (in):** Audit RBAC (referenced secrets/namespaces only, target
  ConfigMap, target workloads); non-root + read-only rootfs; cross-namespace
  refs off by default; hardening guide.
- **Testing:** **Tier 2/3** RBAC conformance (operator fails without the exact
  perms and succeeds with them); pod security context checks.
- **DoD:** Operator runs with documented least privilege; hardening guide
  published.
- **SPEC refs:** T7, R8.6, R9.9-adjacent.

### S5.6 — Scale & performance validation
- **Depends on:** S1.8.
- **Goal:** Confidence at target scale.
- **Scope (in):** Load harness (hundreds of `ConfigSync`es); fetch dedup + fetch
  concurrency bounds; sync-latency SLO check.
- **Testing:** **Tier 3** kind (or an envtest-based benchmark) exercising scale;
  publish results.
- **DoD:** Meets NFR2/NFR3 targets with documented numbers.
- **SPEC refs:** NFR2, NFR3, T10.

---

## 🧪 Usability Milestone U3 — v1.0 acceptance suite on `kind`

- **Depends on:** Phases 1–5 (at least S3.1 + core Phase 4/5 hardening).
- **Goal:** A single, comprehensive real-cluster suite that gates v1.0.
- **Scope (in):**
  - Automate the full **acceptance matrix A1–A10** (SPEC §17) on `kind`.
  - Run against the **N-2** range of supported Kubernetes minor versions (multiple
    kind node images) and, where CI allows, `amd64` + `arm64`.
  - Install **both** via Helm and via plain manifests.
  - Include ESO + Sealed Secrets + a GitOps controller coexistence together.
  - CRD upgrade/conversion smoke (install `v1alpha1`, exercise the conversion path
    scaffolding toward `v1`).
- **Testing:** Tier 3 only; wired as the release gate.
- **Definition of Done:** Entire A1–A10 matrix green across the supported version
  matrix; both install methods pass; leak scanner clean.
- **SPEC refs:** §17 (A1–A10), NFR4, NFR5, NFR9, NFR10.

---

## v1.0 exit criteria

v1.0 is declared when **all** of the following hold:

1. Phases 0–3 complete; Phase 4 ergonomics and Phase 5 hardening items marked
   required for v1 are complete (S4.1–S4.4, S5.1/S5.2/S5.4/S5.5 at minimum;
   S5.3/S5.6 either done or explicitly deferred with rationale).
2. Usability milestones **U1, U2, U3 green in CI** on `kind`.
3. Documentation complete (NFR7): concepts, install (Helm + manifests + RBAC),
   security hardening, CRD reference, secret-backend guides, rollout/reload
   cookbook, GitOps coexistence, troubleshooting.
4. API graduated on a defined path (`v1alpha1` → … → `v1`) with a documented
   deprecation/conversion policy (NFR10); SemVer release process in place.
5. Supply-chain artifacts (signed images + SBOM) published (S5.4).

### Suggested delivery order (critical path)

```
S0.1 → S0.2 → (S1.1 ∥ S1.2 ∥ S1.3) → S1.4 → S1.5 → S1.6 → S1.7 → S1.8 → U1
     → S2.1 → S2.2 → S2.3 → S2.4 → S3.1 → U2
     → (S3.2 ∥ S3.3 ∥ S3.4 ∥ Phase 4 ∥ Phase 5) → U3 → v1.0
```

Steps joined by `∥` are parallelizable across independent agents.
