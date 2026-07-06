# Test infrastructure

This directory holds reusable, cross-package test infrastructure. Per-package
unit and integration tests live next to the code they cover.

## Testing tiers (see [PLAN.md](../PLAN.md) “Testing tiers”)

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
- **Tier 3 — E2E / Usability (`kind`).** A real cluster with kubelet and
  controllers. See [`test/e2e`](./e2e) and milestones U1, U2, U3.

## Fixtures & helpers

| Path | Purpose | Owner |
| --- | --- | --- |
| [`internal/testenv`](../internal/testenv) | envtest control-plane bootstrap (Tier 2) | controller maintainers |
| [`internal/git/httpfixture_test.go`](../internal/git/httpfixture_test.go) | smart-HTTP git-server fixture (Tier 2) | git-source maintainers |
| [`test/leakcheck`](./leakcheck) | secret-leak assertion helper (R8.3/TM9) | security owners |
| [`test/e2e`](./e2e) | kind-based end-to-end scenarios (Tier 3, U1–U3) | e2e maintainers |

### E2E entry points

| Make target | Suite |
| --- | --- |
| `make e2e` | U1 config sync & rollout |
| `make e2e-secrets` | U2 secret integration (requires ESO) |
| `make e2e-security` | S3.1 pod/RBAC conformance (A9) |
| `make e2e-acceptance` | U3 mount-content + matrix (A2) |
| `make e2e-lifecycle` | S3.3 upgrade (A12); set `KOHEN_ALLOW_UNINSTALL=true` for uninstall |
| `make e2e-u3` | Full U3 gate (all of the above) |

### Secret-leak assertions (R8.3 / TM9)

`test/leakcheck` registers known fixture secret values and asserts they never
appear in status, events, logs, or applied objects. It runs on **every PR** as
part of `go test ./...` (it is imported by the reconcile package tests, e.g.
`TestPipelineNoSecretLeakAcrossSinks`), not only in Tier 3. Add a leak scan to
any new test that flows secret material through status, events, or objects.
