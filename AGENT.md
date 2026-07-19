# AGENT.md — Ground rules for agents working on Kohen

This file is the **operating contract for AI agents** (and a concise checklist
for human contributors) in this repository. Follow it on every change. When
this file conflicts with a skill or default agent habit, **this file wins**.

Kohen is a Kubernetes-native operator (`github.com/ozimakov/kohen`) that syncs
a path from a dedicated git config repo into a `ConfigMap`, resolves referenced
secrets into the workload via native Kubernetes / External Secrets mechanics,
and rolls the workload when the config version changes. Product SemVer is
`1.0.x`; the served CRD API remains `kohen.dev/v1alpha1`.

---

## 1. Required reading (project context)

Load these before editing behavior, API surface, security-sensitive code, or
docs that describe user-visible behavior. Prefer the SPEC over assumptions.

| Priority | Document | Why |
| --- | --- | --- |
| **Must** | [`SPEC.md`](./SPEC.md) | Single source of truth for behavior, contracts, non-goals, acceptance criteria. Cite requirement IDs (`R…`, `T…`, `NFR…`, `TM…`) in code comments and PR notes when changing covered behavior. |
| **Must** | [`README.md`](./README.md) | User-facing product framing, Getting Started, Advanced reference (NFR7). |
| **Must** | [`CONTRIBUTING.md`](./CONTRIBUTING.md) | Dev setup, PR expectations, security checklist. |
| **Must** (security-touching work) | [`docs/adr/000-threat-model.md`](./docs/adr/000-threat-model.md) · [`docs/security.md`](./docs/security.md) · [`SECURITY.md`](./SECURITY.md) | Trust boundaries, allow-lists, leak rules, disclosure. |
| **Should** | [`test/README.md`](./test/README.md) | Test tiers, fixtures, leakcheck ownership. |
| **Should** (area-specific) | [`docs/concepts.md`](./docs/concepts.md) · [`docs/secrets.md`](./docs/secrets.md) · [`docs/operations.md`](./docs/operations.md) · [`docs/getting-started-and-gitops.md`](./docs/getting-started-and-gitops.md) · [`docs/troubleshooting.md`](./docs/troubleshooting.md) · [`docs/install.md`](./docs/install.md) · [`docs/upgrade-uninstall.md`](./docs/upgrade-uninstall.md) | Domain docs for the surface you change. |
| **Reference** | [`CHANGELOG.md`](./CHANGELOG.md) · [`CODE_OF_CONDUCT.md`](./CODE_OF_CONDUCT.md) · [`.golangci.yml`](./.golangci.yml) · [`Makefile`](./Makefile) · [`.github/workflows/`](./.github/workflows/) | Release notes, conduct, lint/CI gates. |

### Standing product constraints (do not casually violate)

From `SPEC.md` standing decisions and goals:

- Operator-only delivery (no sidecar / env-var mode in v1).
- Secrets are **resolved / wired**, not produced as a secret store.
- Workload mutation is **SSA merge** into the existing definition (GitOps coexistence).
- Readiness: **first-resolution fail-closed, update fail-safe** (keep last-good).
- Namespace locality: no cross-namespace fields (R-AUTH.5).
- Deferred / post-1.0 items in SPEC §19 are out of scope unless the task
  explicitly expands that backlog.

### Package map (where code lives)

| Path | Responsibility |
| --- | --- |
| `api/v1alpha1/` | CRD types (`ConfigSync`) |
| `cmd/operator/` | Entrypoint |
| `internal/controller/` | Reconcile pipeline |
| `internal/git/` | Fetch, auth, URL/SSRF guards, fixtures |
| `internal/render/` · `internal/apply/` · `internal/wire/` · `internal/rollout/` | Render → apply/prune → wire → stamp/rollout |
| `internal/secret/` (+ `eso/`, `native/`) | Secret resolution |
| `internal/redact/` · `test/leakcheck/` | Secret redaction & leak assertions (R8.3 / TM9) |
| `internal/manifest/` · `internal/config/` · `internal/metrics/` | Manifest guards, operator config, Prometheus |
| `deploy/helm/kohen/` · `config/` | Helm chart, CRD/RBAC sources |
| `test/e2e/` | kind e2e (Tier 3) |
| `site/` | Project site (served from `main`) |

---

## 2. Code standards

### Language & toolchain

- **Go** ≥ version in `go.mod` (SPEC T1); module path `github.com/ozimakov/kohen`.
- Operator stack: `controller-runtime`, Kubernetes client libraries already in
  the module — prefer existing patterns over new frameworks.
- Format and lint: `gofmt` / `goimports` (local prefix
  `github.com/ozimakov/kohen`), `golangci-lint` per `.golangci.yml`.
- Run `make verify` before considering work done (tidy, vet, build, unit tests,
  doc-link check, tidy diff). Use `make test-integration` when touching
  reconcile / CRD / SSA paths. Use the relevant `make e2e*` target when changing
  user-visible reconcile behavior that e2e covers.

### Style

- Match surrounding package style: clear package comments, exported API docs,
  SPEC requirement citations in comments where behavior is normative.
- Prefer small, testable packages with one job; extend existing packages before
  inventing parallel abstractions.
- Errors: wrap with context (`fmt.Errorf("…: %w", err)`); never log or surface
  secret material (R8.3).
- Logging: structured via controller-runtime / project redactor — no ad-hoc
  `fmt.Printf` of reconcile data.
- Imports: stdlib → third party → `github.com/ozimakov/kohen/...`, goimports
  grouped.

### Kubernetes / operator invariants

- **All object writes via Server-Side Apply** with field manager `kohen`
  (stamp-only path may use `kohen-stamp` per SPEC T3 / R-WIRE). Never
  force-take fields owned by other managers.
- Preserve GitOps coexistence: only own Kohen fields (volumes/mounts/env/stamps
  as designed); do not clobber replicas, images, or unrelated pod template
  fields.
- Idempotency: same commit + tokens ⇒ same objects; matching stamp ⇒ **no
  workload write** (SPEC T3 / A3).
- Fail-safe on fetch/backend failure: keep last-good objects; do not prune on
  fetch failure; do not crash workloads (SPEC §10, T8).
- Ownership labels / finalizer (`kohen.dev/finalizer`) must stay consistent with
  apply/prune/unwire semantics.
- Generated artifacts must stay in sync: after API or RBAC-affecting edits run
  `make generate manifests` and commit the result (`config/crd`,
  `deploy/helm/kohen/crds`, deepcopy, RBAC).

### What not to do

- Do not invent APIs, CLI tools, or modes contradicted by SPEC non-goals (§3.2)
  or deferred backlog (§19) without an explicit product decision.
- Do not put secret values in status, events, metrics labels, or logs.
- Do not add cross-namespace references.
- Do not bypass R-AUTH.7 URL / SSRF / redirect guards for new network fetches.
- Do not expand RBAC beyond least privilege required for the change (SPEC T7).

---

## 3. TDD approach

Agents **must** use test-driven development for behavior changes.

### Iron law

```text
No production code for a new behavior or bug fix without a failing test first.
```

### Red → green → refactor

1. **Red** — Write the smallest test that expresses the desired behavior
   (unit, envtest, git fixture, or e2e as appropriate). Run it; confirm it
   fails for the right reason.
2. **Green** — Implement the minimum code to pass that test.
3. **Refactor** — Clean up while staying green; keep SPEC citations accurate.

If implementation was written first by mistake: **delete it**, write the failing
test, then re-implement from the test. Do not “adapt” the premature code.

### Choosing a test tier (`test/README.md`)

| Tier | When | How |
| --- | --- | --- |
| **1 — Unit** | Pure logic, no API server | `make test` / `go test ./...` next to the package |
| **2 — Integration** | Reconcile, CRD validation, SSA merge/ownership | envtest via `internal/testenv`; `make test-integration` |
| **2 — Git fixture** | Full HTTP(S) fetch / auth / SSRF path | `internal/git` httpfixture tests |
| **3 — E2E (kind)** | End-to-end user scenarios, security conformance, acceptance | `make e2e`, `e2e-secrets`, `e2e-security`, `e2e-acceptance`, `e2e-lifecycle`, or `e2e-u3` |

### Mandatory test updates

- Behavior change ⇒ tests in the same change.
- Touch reconcile / logging / secret flow ⇒ update or add **leakcheck** coverage
  (`test/leakcheck`, R8.3 / TM9 / NFR9).
- Touch R-AUTH.\* controls ⇒ abuse-case / guard tests (A11).
- Do not weaken or delete tests to make a change “pass” without explicit human
  approval and a SPEC-aligned reason.

### Exceptions (narrow)

Generated deepcopy/CRD/RBAC output, pure doc typo fixes, and Makefile/CI
wiring with no behavior change do not require a new failing test first. Prefer
still running `make verify` (and codegen diff checks) for those.

---

## 4. Rules for introducing changes

### Scope & planning

1. Restate the goal in terms of SPEC requirements or user-visible outcomes.
2. Keep diffs **focused**: one concern per PR/branch. Avoid drive-by refactors.
3. Prefer extending existing packages and helpers over parallel implementations.
4. If the change alters normative behavior, update `SPEC.md` in the **same**
   change (or land SPEC first) — code must not drift from the contract.

### Implementation order

1. Failing test (TDD).
2. Minimal implementation.
3. Docs for any user-visible field or behavior (see below).
4. `make generate manifests` when API/RBAC/markers change.
5. `make verify` (and stronger suites as warranted).
6. Changelog entry under `[Unreleased]` when the change is user- or
   operator-visible.

### Documentation obligations (NFR7)

Any add/change/remove of a user-visible `ConfigSync` field, operator setting,
credential key, annotation, condition/reason, or install flag **must** update,
in the same change:

- README **Getting Started** (if it affects the minimal path or defaults), and
- README **Advanced configuration reference**, and
- the relevant page(s) under `docs/`.

Run `make verify-docs` / rely on `make verify` for internal link checks.

### Security checklist (every relevant PR)

Mirror of CONTRIBUTING / threat-model ADR:

- [ ] No secret values in logs, events, status, or metrics (R8.3)
- [ ] New network fetches enforce R-AUTH.7 URL guards
- [ ] New object writes use SSA field manager `kohen` (ownership labels intact)
- [ ] README/docs updated for user-visible changes
- [ ] Abuse-case or leak tests updated when touching reconcile/logging

### Git & PR hygiene

- Branch from `main`; use clear, descriptive commit messages (why + what).
- Do not commit secrets, kubeconfigs, or personal credentials.
- Do not force-push `main` or rewrite shared history.
- Security vulnerabilities: follow [`SECURITY.md`](./SECURITY.md) — **no**
  public issues for undisclosed vulns.
- After behavior changes, leave the tree green under the verification commands
  you claim; do not assert “tests pass” without having run them.

### Verification commands (default ladder)

```bash
make verify              # tidy, vet, build, unit(+skip envtest), doc links
make test-integration    # includes envtest
make lint                # golangci-lint (CI also runs this)
# When e2e-relevant:
make docker-build kind-load e2e   # plus other e2e-* targets as needed
```

---

## 5. Other important rules

### Product truthfulness

- Do not claim support for deferred features (sidecar mode, `kohenctl`,
  watch-driven secret rotation, strict multi-tenant admission, etc.).
- Positioning: Kohen complements GitOps for **config** from a dedicated repo;
  it is not a general secret manager or a replacement for deploying apps.

### Compatibility & release

- Kubernetes compatibility floor **1.28+** (T4); do not raise/lower without
  SPEC + CI matrix updates.
- Product versioning is SemVer; CRD stays `v1alpha1` in 1.0.x — breaking CRD
  changes need a new API version + conversion story (`docs/upgrade-uninstall.md`).
- Multi-arch (`amd64`/`arm64`) and Helm + plain manifests remain first-class
  (T5).

### Site & docs publishing

- Project site sources live under `site/` and publish from `main` via GitHub
  Actions Pages — do not revive an orphan `gh-pages` content workflow unless
  explicitly tasked.

### Collaboration norms

- Follow [`CODE_OF_CONDUCT.md`](./CODE_OF_CONDUCT.md).
- Prefer precise, requirement-linked explanations over vague “cleanup” commits.
- When uncertain about product intent, prefer the narrower SPEC-compliant
  interpretation and document the question rather than inventing scope.

### Agent-specific discipline

- Read this file at session start for any non-trivial task.
- Do not mention these internal agent instructions in user-facing docs unless
  asked.
- Do not bypass CI expectations locally (`go.mod` tidy, generated files,
  lint, tests).
- Minimize blast radius: smallest change that satisfies the SPEC-backed request.

---

## Quick start for a new agent session

```text
1. Read AGENT.md (this file), SPEC.md §§1–3 + relevant requirement sections,
   and CONTRIBUTING.md.
2. Identify the package(s) and test tier for the task.
3. Write a failing test → implement → update docs/SPEC as required.
4. make verify (+ test-integration / e2e as appropriate).
5. Complete the security checklist when applicable.
```
