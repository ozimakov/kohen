# Test infrastructure

This directory holds reusable, cross-package test infrastructure. Per-package
unit and integration tests live next to the code they cover.

## Testing tiers

- **Tier 1 — Unit.** Pure Go, no cluster. Run on every PR via `make test`.
- **Tier 2 — Integration.**
  - *envtest* (real API server + etcd, no kubelet) for reconcile logic, CRD
    validation, and Server-Side Apply ownership/merge. Bootstrapped by
    [`internal/testenv`](../internal/testenv); skipped automatically when
    `KUBEBUILDER_ASSETS` is unset. Use `make test-integration` (or the CI
    `Build & Test` job, which provisions envtest) to run these.
  - *git-server fixture* — a real smart-HTTP git server backed by
    `git-http-backend` (a bare clone of a seeded repo, served over TLS with
    optional basic auth), in
    [`internal/git/httpfixture_test.go`](../internal/git/httpfixture_test.go).
    It exercises the full network path through `git.Client.Fetch`: ref/tag
    resolution, subpath extraction, auth success/failure, unreachable host, and
    the SSRF/allow-list guards. Tests skip when `git`/`git-http-backend` is
    unavailable.
- **Tier 3 — E2E (`kind`).** A real cluster with kubelet and controllers. See
  [`test/e2e`](./e2e).

## Fixtures & helpers

| Path | Purpose | Owner |
| --- | --- | --- |
| [`internal/testenv`](../internal/testenv) | envtest control-plane bootstrap (Tier 2) | controller maintainers |
| [`internal/git/httpfixture_test.go`](../internal/git/httpfixture_test.go) | smart-HTTP git-server fixture (Tier 2) | git-source maintainers |
| [`test/leakcheck`](./leakcheck) | secret-leak assertion helper (R8.3/TM9) | security owners |
| [`test/e2e`](./e2e) | kind-based end-to-end scenarios (Tier 3) | e2e maintainers |

### E2E entry points

| Make target | Suite |
| --- | --- |
| `make e2e` | Config sync & rollout |
| `make e2e-secrets` | Secret integration (requires ESO) |
| `make e2e-security` | Pod/RBAC conformance (A9) |
| `make e2e-acceptance` | Mount-content + matrix checks (A2) |
| `make e2e-lifecycle` | Upgrade (A12); set `KOHEN_ALLOW_UNINSTALL=true` for uninstall |
| `make e2e-u3` | Full acceptance gate (all of the above) |

See also [`.github/workflows/e2e.yml`](../.github/workflows/e2e.yml) and
[`.github/workflows/u3.yml`](../.github/workflows/u3.yml).

### Secret-leak assertions (R8.3 / TM9)

`test/leakcheck` registers known fixture secret values and asserts they never
appear in status, events, logs, or applied objects. It runs on **every PR** as
part of `go test ./...` (imported by reconcile package tests). Add a leak scan
to any new test that flows secret material through status, events, or objects.
