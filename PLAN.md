# KOHEN — Implementation Plan (to v1.0)

> Companion to [`SPEC.md`](./SPEC.md) (Draft v0.6). This plan sequences the work
> from an empty repo to a **v1.0** operator. It is written so that **each step
> can be picked up and delivered independently by a separate agent**: every step
> lists its dependencies, scope, deliverables, tests, and a self-contained
> Definition of Done, and references the SPEC requirements it satisfies.
>
> Usability testing (real-cluster, end-to-end, `kind`-based) is called out as its
> **own milestones** (`U1`–`U3`), separate from the unit/integration testing that
> lives inside each build step.
>
> **v1 scope (per SPEC v0.6):** secret backends are **ESO + native `Secret`**
> only; `ConfigSync` is the only config surface; no CLI/webhooks/overlay/
> splitting/DaemonSet in v1 — see the [post-1.0 backlog](#post-10-backlog).

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
- **SPEC references must resolve.** A CI docs check verifies that every SPEC ID
  cited here exists in `SPEC.md` (Appendix A).

**For human readers** — milestone → user-visible capability:

| After | You can |
| --- | --- |
| U1 | Sync a git path to a ConfigMap, wire a Deployment, get version-matched rollouts (config only) |
| U2 | Reference ESO-backed and native secrets from the same `ConfigSync`, safely |
| U3 / v1.0 | Install/upgrade via Helm with docs, hardening guide, and a full acceptance suite behind it |

### Definition of Done (applies to every step unless overridden)

1. Code + tests merged; CI green (lint, unit, and any integration/e2e tiers the
   step declares).
2. Public interfaces documented; SPEC/README/CRD docs updated as needed.
3. No secret material in logs/events/status/artifacts (SPEC R8.3) where relevant.
4. Backward-compatible with already-merged steps, or the break is called out and
   the affected steps' entries updated.

---

## Testing tiers

- **Tier 1 — Unit.** Pure Go, no cluster. Deterministic; fast; run on every PR.
- **Tier 2 — Integration.**
  - *envtest* (controller-runtime: real API server + etcd, **no kubelet, no
    built-in workload controllers**) for reconcile logic, CRD validation,
    Server-Side Apply ownership/merge, and status. envtest can assert **stamps
    and object state**, never actual pod rollouts or volume updates — those are
    Tier 3 only.
  - *git server fixture* (a temporary bare repo or an in-process/containerized
    git server) for the git source library.
  - Runs on PRs touching the relevant packages.
- **Tier 3 — E2E / Usability (`kind`).** A real cluster with kubelet and
  controllers: install Kohen via Helm, run real workloads and real ESO, assert
  **user-visible** outcomes (ConfigMap present, rollout happened, secret in the
  pod, no flapping). Backs the `U*` milestones and the release gate. Runs on
  merges to main and nightly; each `U*` milestone wires its scenarios into CI.

> Rationale for `kind`: a genuine kubelet + controllers locally and in CI, so
> mounted-volume atomic updates, real rollouts, and coexistence behavior are
> exercised for real — which envtest cannot do.

---

## Dependency graph (high level)

```
Phase 0  Foundations        S0.1 → (S0.2 ∥ S0.3)
Phase 1  Config-only        S1.1 ─┐
         operator (MVP)     S1.2 ─┤
                            S1.3 ─┼→ S1.4 → S1.5 → S1.6 → S1.7 → S1.8  ──►  [U1]
Phase 2  Secrets            S2.1 → S2.2 → S2.3 → S2.4                  ──►  [U2]
Phase 3  Ship readiness     S3.1 ∥ S3.2 ∥ S3.3  (needs U2)             ──►  [U3] → v1.0
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
  `CODE_OF_CONDUCT`, `SECURITY.md`; the SPEC-reference docs check (see *How to
  use this plan*).
- **Scope (out):** Any Kohen behavior.
- **Deliverables:** Building repo skeleton; green CI; publishable image target.
- **Testing:** Tier 1 (a trivial unit test proves the harness); CI builds the
  image.
- **Definition of Done:** `make build test lint image` succeed locally and in CI.
- **SPEC refs:** T1, T5, NFR8.

### S0.2 — Test harness & fixtures
- **Depends on:** S0.1.
- **Goal:** Reusable test infrastructure every later step relies on.
- **Scope (in):** envtest bootstrap helper; git-server fixture (spin up a temp
  bare repo / lightweight git server, seed commits, return `repo@ref:path`);
  a `kind` e2e harness (cluster up/down, load locally-built image, install CRDs +
  Helm chart, teardown) with a documented `make e2e` entrypoint; golden-file
  helpers; **secret-leak assertion helper** (scan logs/events/status/objects for
  known fixture secret values) wired to run on **every PR** touching
  reconcile/logging packages, not only in Tier 3; `test/README.md` documenting
  fixture ownership/maintenance.
- **Deliverables:** `test/` packages + scripts; docs on running each tier.
- **Testing:** Harness self-tests (Tier 1/2); a smoke `kind` job that
  creates/destroys a cluster (Tier 3) to prove the harness in CI.
- **Definition of Done:** Other steps can call the fixtures; smoke kind job green.
- **SPEC refs:** NFR9, R8.3.

### S0.3 — Threat model & security baseline (ADR)
- **Depends on:** S0.1. *Parallelizable with S0.2.*
- **Goal:** Lock the security architecture **before** the operator skeleton, so
  RBAC, validation, and allow-lists are designed in, not bolted on.
- **Scope (in):** Turn SPEC §3.3 into a repo ADR: actors, trust boundaries,
  TM1–TM9 → control mapping (R-AUTH.1–.7); decide operator-config schema for
  the git source allow-list, secret-store allow-list, referable-secret policy,
  and `maxDegradedDuration`; the RBAC shape for **both** install scopes (T7);
  a security review checklist used by later steps' PRs.
- **Deliverables:** `docs/adr/000-threat-model.md`; security checklist in
  `CONTRIBUTING`.
- **Testing:** n/a (design artifact); reviewed like code.
- **Definition of Done:** Every TM row maps to a control and an owning step in
  this plan; later steps cite it.
- **SPEC refs:** §3.3, R-AUTH.1–.7, T7, TM1–TM9.

---

## Phase 1 — Config-only operator (MVP)

Delivers UC1, UC4, UC5, UC7 for **config** (no secrets yet): git → `ConfigMap`,
workload wiring via SSA merge, and version-matched rollout.

### S1.1 — Git source library
- **Depends on:** S0.2, S0.3.
- **Goal:** Fetch and resolve config content from git deterministically and
  securely.
- **Scope (in):** `repo@ref:path` fetch; resolve branch/tag → concrete commit
  (commit pins verbatim); shallow fetch / fetch-by-commit; HTTPS token & SSH key
  auth from the documented credential-`Secret` schema (R7.8) incl. the
  `kohen.dev/git-credential` label check (R-AUTH.6); TLS + SSH host-key
  verification on by default; **URL guards** — scheme allow-list, link-local/
  metadata IP blocking, redirect policy (R-AUTH.7); operator-level **source
  allow-list** enforcement hook (R-AUTH.3); return the file set at `path` + the
  resolved commit SHA. Define a `Source` interface.
- **Scope (out):** Rendering; k8s objects; webhooks / signed commits (post-1.0).
- **Deliverables:** `internal/git` package + interface.
- **Testing:** Tier 1 (URL/ref parsing, auth selection, URL-guard cases);
  **Tier 2** against the git-server fixture (ref resolution, subpath extraction,
  auth failure, unreachable host, disallowed URL fails closed).
- **Definition of Done:** Given a fixture repo, returns the exact file tree +
  commit SHA; verification defaults and URL guards enforced.
- **SPEC refs:** R7.1–R7.2, R7.8, R-AUTH.3, R-AUTH.6, R-AUTH.7, T2, T8.

### S1.2 — Config renderer (files → ConfigMap data)
- **Depends on:** S0.2. *Parallelizable with S1.1.*
- **Goal:** Turn a file tree into deterministic `ConfigMap` data.
- **Scope (in):** File tree → keys (`data`/`binaryData`) with the documented
  nested-path key separator (SPEC §18 Q1 — resolve here); deterministic
  flattening & ordering; **tree safety** — reject `..`, absolute paths, symlink
  escapes (R7.5); **exclusions** — recognized secret-manifest files and
  reserved `kohen.*` files are not ConfigMap keys (R7.6); **oversize detection**
  (fail closed with safety margin + actionable error, R7.7); verbatim mapping
  only.
- **Scope (out):** Overlay/templating and splitting (post-1.0).
- **Deliverables:** `internal/render` package + interface producing a desired
  `ConfigMap` spec.
- **Testing:** Tier 1 golden files; determinism (same input ⇒ byte-identical);
  tree-safety and oversize cases produce the documented errors.
- **Definition of Done:** Deterministic, safe, oversize-guarded rendering.
- **SPEC refs:** R7.4–R7.7, T9, T-LIMIT.

### S1.3 — `ConfigSync` CRD & API types
- **Depends on:** S0.1, S0.3. *Parallelizable with S1.1/S1.2.*
- **Goal:** The primary API surface, with defaults and validation per SPEC.
- **Scope (in):** `api/v1alpha1` types (`source{url,ref,authSecretRef}`, `path`,
  `workloadRef{kind,name}`, `configMap`, `wiring{container,mountPath}`,
  `rollout`, `sync`, `secretRefs` shape reserved for S2.1); **defaults exactly
  per SPEC §11.2**; deepcopy; CRD manifests; OpenAPI + CEL validation incl.
  namespace locality (R-AUTH.5), supported `workloadRef.kind`, `rollout` enum;
  printer columns (R11.4); status subresource with the **§11.4 condition
  types/reasons** as typed constants.
- **Scope (out):** Reconcile behavior (S1.4+); R-SINGLETON enforcement (S1.7,
  needs workload knowledge).
- **Deliverables:** Generated CRD YAML + Go types + condition/reason constants.
- **Testing:** **Tier 2** envtest: CRD installs; valid CRs accepted; invalid CRs
  (cross-namespace ref shapes, bad enums) rejected by CEL; defaults applied and
  asserted field-by-field against §11.2.
- **Definition of Done:** CRD round-trips through the API server with validation
  and documented defaults.
- **SPEC refs:** §11.1–§11.4, R-AUTH.5, R11.1–R11.4.

### S1.4 — Operator skeleton (Helm, RBAC scopes, observability floor)
- **Depends on:** S1.3.
- **Goal:** A runnable controller with no business logic yet — but with the
  final security posture.
- **Scope (in):** controller-runtime manager; `ConfigSync` reconciler stub
  (records `observedGeneration`, sets `Ready`); leader election; metrics +
  health endpoints; structured JSON logging through the **centralized redacting
  logger** (R8.3); operator config file (allow-lists, `maxDegradedDuration` —
  schema from S0.3); Helm chart with **both install scopes** (cluster-wide and
  namespace-scoped Roles) and non-root/read-only-rootfs pod security; plain
  manifests output.
- **Deliverables:** `cmd/operator`, `internal/controller`, `deploy/helm`.
- **Testing:** **Tier 2** envtest: manager starts, reconciles a CR to `Ready`,
  metrics/health serve; unit tests for the redacting logger; Helm template
  tests for both scopes.
- **Definition of Done:** Operator installs via Helm in either scope and
  reconciles a no-op CR.
- **SPEC refs:** §6.3, T1, T7, R8.3, R13.1–R13.3.

### S1.5 — Object apply/prune engine
- **Depends on:** S1.4.
- **Goal:** Safe, idempotent creation/update/pruning of Kohen-owned objects.
- **Scope (in):** Server-Side Apply with field manager `kohen` + ownership
  labels; create/update `ConfigMap`; **prune** owned objects that disappear;
  **never** adopt/overwrite un-owned objects (no adoption mode, R8.8).
- **Scope (out):** Workload mutation (S1.6); applying secret manifests (S2.4).
- **Deliverables:** `internal/apply` package + interface.
- **Testing:** **Tier 2** envtest: apply/update idempotency; prune-on-disappear;
  refusal to touch un-owned objects.
- **Definition of Done:** ConfigMaps converge idempotently; pruning is owner-safe.
- **SPEC refs:** T3, R8.8, R11.3, R-ATOM.

### S1.6 — Workload SSA merge & GitOps coexistence
- **Depends on:** S1.5.
- **Goal:** Wire owned fields into a workload without fighting other managers.
- **Scope (in):** SSA merge into `Deployment`/`StatefulSet` pod template with
  field manager `kohen`; **container targeting** (`wiring.container`, default
  first container — R-WIRE.3); inject only owned, **keyed** list entries
  (volume, volumeMount, env — **no `envFrom`**, R-WIRE.2) + the version
  annotation placeholder; conflict handling (re-read + re-apply owned fields
  only, R-WIRE.4); **unwire on delete** via finalizer (R-WIRE.6); reject
  `subPath` in Kohen-managed mounts; authored **GitOps compatibility matrix +
  ignore-rule snippets** (Argo CD `ignoreDifferences`, Flux exclusions;
  SSA-based appliers only — R-WIRE.5).
- **Deliverables:** `internal/wire` package (backend-agnostic; surface-specific
  builders, so secret surfacing in S2.3 extends rather than forks it); docs
  section with copy-paste snippets.
- **Testing:** **Tier 2** envtest: field-ownership assertions; simulate a
  competing SSA field manager and assert no clobber; multi-container targeting;
  owned fields fully removed on unwire; `envFrom`/`subPath` rejected.
- **Definition of Done:** Kohen-owned fields merge in, survive a competing SSA
  manager, and unwire cleanly (foundation for A10).
- **SPEC refs:** §6.2, R-WIRE.1–R-WIRE.6.

### S1.7 — Version stamp & rollout
- **Depends on:** S1.6.
- **Goal:** Roll the workload only when the config version changes.
- **Scope (in):** Compute the config version (`git:<sourceCommit>`; secret
  component arrives in S2.2); stamp `kohen.dev/config-sha` via S1.6; **match**
  desired vs. stamped; stamp only on mismatch and only after ConfigMap apply
  (R-ROLLOUT.2/.4); coalesce concurrent versions (R-ROLLOUT.6); `rollout: none`
  mode; **strategy guard** — detect `OnDelete` and set `Degraded/
  UnsupportedStrategy` without stamping (R-ROLLOUT.5); **R-SINGLETON**
  enforcement (second `ConfigSync` on the same workload →
  `Degraded/SingletonViolation`); status (`sourceCommit`, `configVersion`,
  `workloadVersion`, `rolloutInProgress` derived from workload
  generation/updatedReplicas); events.
- **Scope (out):** Actual pod rollouts (Tier 3 — envtest asserts **stamping and
  status only**, per Testing tiers).
- **Deliverables:** rollout logic in `internal/controller`.
- **Testing:** **Tier 2** envtest: change ⇒ exactly one stamp update; no change
  ⇒ no workload write; `OnDelete` degrades; singleton violation degrades;
  status reflects versions.
- **Definition of Done:** Idempotent, version-driven stamping with accurate
  status.
- **SPEC refs:** §9 R-VERSION, R-CONS, R-ROLLOUT.1–.6, R-SINGLETON, R-ROLLBACK.

### S1.8 — Reconcile loop integration & fail-safe
- **Depends on:** S1.1, S1.2, S1.7.
- **Goal:** Assemble the full config-only reconcile and make it robust.
- **Scope (in):** Wire fetch → render → apply → wire → stamp → status; **watch
  wiring** (ConfigSync, owned objects, target workload) + poll interval +
  **force-sync annotation** (`kohen.dev/sync-now`, §6.1); bounded backoff +
  jitter (R10.1); keep-last-good on git outage; every §10 failure row surfaces
  its named §11.4 condition reason + metric (R10.2); dedup git fetches by
  repo+commit (T10).
- **Deliverables:** Complete config-only operator.
- **Testing:** **Tier 2** envtest + git fixture, incl. fault injection (git
  unreachable, auth failure, disallowed URL, malformed path, tree-safety
  violation, oversize, workload missing) asserting last-good preserved **and**
  the documented condition reason set for each.
- **Definition of Done:** End-to-end config sync works in envtest; every failure
  mode maps to its documented reason; failures never corrupt/delete objects.
- **SPEC refs:** §6.1, §10, §11.4, R10.1–R10.2, T8, T10, R13.1–R13.4.

---

## 🧪 Usability Milestone U1 — Config sync & rollout on `kind`

- **Depends on:** S1.8.
- **Goal:** Prove the **config-only** user journeys on a real cluster, including
  GitOps coexistence — integration testing of the *experience*, not units.
- **Environment:** `kind`; Kohen via Helm; an in-cluster git server seeded with
  a config repo; a sample `Deployment`.
- **Scenarios (each automated in CI + captured in a runbook):**
  1. **Day-1 runbook:** apply the SPEC §1.2 YAML **verbatim** → default-named
     `ConfigMap` appears, workload wired at the default mount path, stamped
     (A1).
  2. Commit a config change → mounted volume updates in running pods **and**
     exactly one rolling update occurs (`rollout: auto`); a follow-up reconcile
     with no change ⇒ no rollout (A2, A3). Repeat with `rollout: none` ⇒ volume
     updates, no rollout.
  3. `sourceCommit` + stamped version readable from CR status and workload
     metadata (UC7).
  4. **Private repo auth:** HTTPS-token and SSH-key credential Secrets (R7.8);
     auth failure ⇒ `Degraded/AuthFailed` with a redacted, actionable event.
  5. Rollback: pin `spec.source.ref` to a prior tag/commit → prior version
     restored + rolled (UC6).
  6. Git outage → `Degraded`, workload stays healthy, auto-recovers.
  7. **Error UX:** oversize config ⇒ `Rendered=False/Oversize` with the
     documented message (A8); missing workload ⇒ `WorkloadNotFound`; `OnDelete`
     StatefulSet ⇒ `UnsupportedStrategy`.
  8. Delete `ConfigSync` → owned objects pruned, workload unwired and intact
     (A7).
  9. **GitOps coexistence:** Argo CD **or** Flux in **SSA mode** managing the
     same `Deployment`, documented ignore rules applied → no flapping, both
     converge (A10).
  10. **Force sync:** `kohen.dev/sync-now` annotation triggers an immediate
      reconcile.
- **Deliverables:** `test/e2e` suite + CI job (kind) + the "Getting Started &
  GitOps" runbook (the runbook **is** scenario 1 + 9's docs).
- **Definition of Done:** All scenarios pass in CI on kind; runbook verified by
  following it literally.
- **SPEC refs:** A1–A3, A7, A8, A10, UC1, UC4–UC7, NFR6.

---

## Phase 2 — Secrets

Delivers UC2/UC3 with the two v1 backends (ESO primary, native `Secret`).

### S2.1 — Secret reference model & API
- **Depends on:** S1.3, S0.3.
- **Goal:** Declare secret references and surfacing on the `ConfigSync`.
- **Scope (in):** `spec.secretRefs`: backend enum (**`externalSecret |
  nativeSecret`** only), `surface` (`file` with `mountPath` / `env` with
  `envVar`+`key`, `rolloutOnRotate`); validation — reject duplicate names,
  overlapping mounts, env collisions (R8.12); same-namespace locality
  (R-AUTH.5); per-ref resolution status shape (no values).
- **Scope (out):** Resolution behavior (S2.2+); in-repo `kohen.yaml`
  (post-1.0).
- **Deliverables:** API additions + validation.
- **Testing:** Tier 1 validate; **Tier 2** envtest CEL rejection of conflicts
  and cross-namespace shapes.
- **Definition of Done:** References validate; conflicts rejected.
- **SPEC refs:** §8.1, §8.4, R8.12, R-AUTH.5, §11.1.

### S2.2 — Resolution framework (readiness, version tokens, redaction)
- **Depends on:** S2.1, S1.8.
- **Goal:** The backend-independent resolution engine and its safety semantics.
- **Scope (in):** `Resolver` interface (reference in; identity + readiness +
  **version token** out); assume-exists/reconcile (P3); fail-closed on
  unresolved (R8.4); **asymmetric readiness** (first-resolution fail-closed,
  update fail-safe — R8.9) as an explicit state machine;
  **`maxDegradedDuration`** enforcement (R8.11); **secret version tokens from
  metadata only** — `resourceVersion` + key set for native, ESO synced
  revision/status for ESO — never values (R8.10); fold **env-surfaced** tokens
  into the config version; file-surfaced rotation does **not** advance it
  (R8.5, R-CONS); **watches on referenced `Secret`s/`ExternalSecret`s** as
  reconcile triggers (§6.1, T10); redaction everywhere.
- **Deliverables:** `internal/secret` framework + a fake backend for tests.
- **Testing:** Tier 1 state-machine tests (readiness policy incl.
  `MaxDegradedExceeded`); **Tier 2** envtest with fake Secrets: first
  resolution blocks stamping; prior-good survives transient not-ready;
  env-surfaced rotation advances the version, file-surfaced does not; leak
  assertions.
- **Definition of Done:** Readiness/fail-closed/rotation behavior provably
  correct with a fake backend; no leaks.
- **SPEC refs:** §8.1–§8.2, R8.4–R8.5, R8.9–R8.11, R-CONS, NFR1.

### S2.3 — Secret surfacing + native backend
- **Depends on:** S2.2, S1.6.
- **Goal:** Wire resolved secrets into the pod; ship the first backend.
- **Scope (in):** Extend `internal/wire` surface builders: `Secret` volume +
  mount (file) and discrete `env[]` `secretKeyRef` entries (env) — owned,
  keyed, no `envFrom`, container-targeted; **native `Secret` backend** (await
  existence + required keys, wire, track token).
- **Deliverables:** surfacing in `internal/wire`; `internal/secret/native`.
- **Testing:** **Tier 2** envtest: file + env wiring assertions, ownership, no
  clobber; native ref resolves/awaits/wires end-to-end with S2.2 semantics.
- **Definition of Done:** Native-backed secrets appear in the pod spec via both
  mechanics, owner-safe (A6 foundations).
- **SPEC refs:** §8.3 (native), §8.4, R-WIRE.2–R-WIRE.4, R8.5.

### S2.4 — Apply-if-present manifest engine + ESO backend
- **Depends on:** S2.3, S1.5.
- **Goal:** Apply owned secret manifests from git; first-class ESO integration.
- **Scope (in):** Generalize S1.5 to **apply** recognized `ExternalSecret`
  manifests found in git (owned + pruned, R8.8; excluded from ConfigMap keys
  per R7.6); **guard rails** — namespaced allow-listed kinds only, optional
  `secretStoreRef` allow-list, never cluster-scoped secret CRs (R-AUTH.4);
  when absent, await the externally-managed object (§8.2); **ESO backend** —
  gate first resolution on `ExternalSecret` `Ready=True` (R8.9), wire the
  target `Secret`, token from synced revision (R8.10).
- **Deliverables:** manifest-apply support in `internal/apply`;
  `internal/secret/eso`.
- **Testing:** **Tier 2** envtest (ESO CRDs installed, status set by the test):
  apply/prune/await; kind/store guard-rail rejections; **Tier 3** kind with
  real ESO + fake provider: apply → Ready → wired; not-ready honors the
  readiness policy.
- **Definition of Done:** ESO journey green on kind; guard rails enforced; no
  plaintext leakage.
- **SPEC refs:** §8.2, §8.3 (ESO), R8.8–R8.10, R-AUTH.4, G3, UC3.

---

## 🧪 Usability Milestone U2 — Secret integration on `kind`

- **Depends on:** S2.4.
- **Goal:** Prove the **secret** user journeys — including abuse cases — on a
  real cluster.
- **Scenarios (automated + runbook):**
  1. Config references a secret backed by a git-committed `ExternalSecret` →
     applied, awaited (`Ready`), wired as **file** and as **env**; value present
     in the pod; **no plaintext** anywhere (git/logs/events/status) (A4).
  2. **First-resolution fail-closed:** reference a not-yet-existing secret →
     `Pending/AwaitingFirstResolution`, no rollout; create it → resolves and
     rolls (A5).
  3. **Update fail-safe:** with a running good version, make ESO transiently
     not-ready → workload keeps running, `Degraded/DegradedServingLastGood`,
     recovers; exceed `maxDegradedDuration` → `MaxDegradedExceeded` event
     (A5).
  4. **Rotation:** rotate an env-surfaced secret → version advances → exactly
     one rollout; rotate a file-surfaced secret → in-place update, **no**
     rollout (A6).
  5. **Native backend:** pre-existing `Secret` referenced and wired (Tier 2
     covered the mechanics — one kind smoke here).
  6. **Abuse cases (A11):** `ConfigSync` with a disallowed `source.url` fails
     closed; unlabeled `authSecretRef` rejected; committed manifest with a
     non-allow-listed kind or store rejected; second `ConfigSync` on the same
     workload degrades.
- **Deliverables:** `test/e2e/secrets` + CI job; ESO + native integration
  guide (incl. the Vault-via-ESO decision tree).
- **Definition of Done:** All scenarios pass on kind; leak scanner clean.
- **SPEC refs:** A4–A6, A11, UC2, UC3, R-AUTH.3–.4, R-AUTH.6.

---

## Phase 3 — Ship readiness

*Parallelizable* once U2 is green.

### S3.1 — Security conformance & hardening guide
- **Depends on:** U2.
- **Goal:** Verify the security posture end-to-end; document it.
- **Scope (in):** **RBAC conformance tests** for both install scopes (operator
  fails without the exact permissions, succeeds with them — A9); pod security
  context checks (non-root, read-only rootfs); abuse-case regression suite
  promoted to the release gate; **hardening guide**: threat model summary,
  compromise blast radius per scope (TM8), allow-list configuration, the
  "`ConfigSync` create ≈ Pod create" RBAC guidance (R-AUTH.1).
- **Deliverables:** conformance tests; `docs/security.md`.
- **Testing:** **Tier 2/3** as above.
- **Definition of Done:** A9 + A11 automated; guide published.
- **SPEC refs:** T7, R-AUTH.1–.7, TM8, A9, A11.

### S3.2 — Documentation suite
- **Depends on:** U2. *Parallelizable with S3.1.*
- **Goal:** The complete NFR7 set, verified against the running system.
- **Scope (in):** Concepts; install (Helm + manifests, both scopes); Day-1
  runbook (from U1); git auth guide; ESO/native secret guide; **GitOps
  coexistence quickstart** with tested snippets (R-WIRE.5); **troubleshooting**
  — the §11.4 symptom → condition/reason → action table; **kubectl operations**
  (§15: status, force-sync, pin/rollback); multi-workload pattern (one
  `ConfigSync` per workload, R-SINGLETON); README refresh (§2.4 decision table
  prominent).
- **Deliverables:** `docs/` tree; README.
- **Testing:** Runbook steps executed literally against kind (can reuse U1/U2
  jobs); docs CI (links, SPEC refs).
- **Definition of Done:** A newcomer can go install → first sync → debug a
  failure using only the docs.
- **SPEC refs:** NFR6, NFR7, §15, §11.4.

### S3.3 — Release engineering
- **Depends on:** S0.1 (pipeline scaffolding may start early); **U2** for the
  A12 upgrade/uninstall verification; final gate needs S3.1/S3.2.
  *Parallelizable.*
- **Goal:** Repeatable, trustworthy releases.
- **Scope (in):** Versioned Helm chart + manifest bundles; SemVer release
  automation (tags → images + chart publish); CRD upgrade policy documented
  (NFR10); **operator upgrade + uninstall behavior** implemented/verified
  (upgrade keeps syncs converging; uninstall leaves workloads running —
  A12); image signing + SBOM in the release pipeline (kept lightweight;
  scope per S0.3 checklist).
- **Deliverables:** release workflow; upgrade/uninstall docs.
- **Testing:** **Tier 3** kind: Helm upgrade from previous tag; uninstall
  leaves workload running and objects present.
- **Definition of Done:** A12 automated; releases reproducible from a tag.
- **SPEC refs:** T5, NFR10, A12.

---

## 🧪 Usability Milestone U3 — v1.0 acceptance suite on `kind`

- **Depends on:** Phase 3.
- **Goal:** A single real-cluster suite that gates v1.0.
- **Scope (in):**
  - Automate the full **acceptance matrix A1–A12** (SPEC §16) on `kind`.
  - Run on **two Kubernetes minor versions** (latest + one older kind node
    image); `amd64` required, `arm64` where CI allows. (Full N-2 matrix is a
    post-1.0 CI expansion; T4 remains the declared support statement.)
  - Install via **Helm and plain manifests**.
  - ESO + a GitOps controller (SSA mode) coexistence in one combined scenario.
  - Operator **upgrade + uninstall** journeys (from S3.3).
- **Testing:** Tier 3 only; wired as the release gate.
- **Definition of Done:** A1–A12 green on both versions and both install
  methods; leak scanner clean.
- **SPEC refs:** §16 (A1–A12), NFR4, NFR5, NFR9, NFR10.

---

## v1.0 exit criteria

v1.0 is declared when **all** of the following hold:

1. Phases 0–3 complete (S0.1–S3.3).
2. Usability milestones **U1, U2, U3 green in CI** on `kind`.
3. Documentation complete per S3.2 (NFR7).
4. Release process in place per S3.3 (SemVer, upgrade/uninstall verified,
   NFR10).

Everything else is post-1.0 (below) — deliberately, per SPEC §19.

### Suggested delivery order (critical path)

```
S0.1 → (S0.2 ∥ S0.3) → (S1.1 ∥ S1.2 ∥ S1.3) → S1.4 → S1.5 → S1.6 → S1.7 → S1.8 → U1
     → S2.1 → S2.2 → S2.3 → S2.4 → U2
     → (S3.1 ∥ S3.2 ∥ S3.3) → U3 → v1.0
```

Steps joined by `∥` are parallelizable across independent agents.

---

## Post-1.0 backlog

Sequenced roughly by expected demand; each becomes a plan entry when scheduled.

| Item | Notes |
| --- | --- |
| Sealed Secrets backend (v1.1) | Reuses the S2.4 manifest engine; add issuer/key-scope policy first (SPEC §18 Q2) |
| `kohenctl` | status/diff UX on top of the stable status API |
| Git webhooks | Disabled by default; HMAC verification, trust the fetched pinned commit — not the payload |
| Signed-commit verification | GPG/SSH allow-list; block acting on unverified commits |
| Overlay / light templating | Deterministic base+env merge; bounded substitution |
| `ConfigMap` splitting | For configs near T-LIMIT |
| `DaemonSet` targets | Extend R-ROLLOUT.5 |
| Reloader interop | Content-hash annotation mode |
| In-repo `kohen.yaml` | Second config surface + precedence rules |
| Strict multi-tenant authorization | Policy CR / admission webhook binding repos↔namespaces↔secrets; v1 namespace-trust stance confirmed (SPEC RD8) |
| Vault injector / Secrets Store CSI | Needs a readiness contract compatible with R8.9 |
| Progressive rollout strategies | Only if native strategies prove insufficient |
| Scale/performance validation harness | Publish NFR2/NFR3 numbers at scale |
| Sidecar / env-var mode; file-volume mode | SPEC §19 |
| Sync history CR (`ConfigRelease`) | Only if status history proves insufficient |
