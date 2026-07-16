# ADR 000 — Threat model & security baseline

**Status:** Accepted  
**Date:** 2026-07-06  
**SPEC:** §3.3, R-AUTH.1–.7, T7, TM1–TM9

## Context

Kohen reconciles git configuration into native Kubernetes objects and merges
secret references into workloads. The operator runs with cluster or namespace
RBAC and reads referenced `Secret` material. Security must be designed in, not
bolted on.

## Actors & trust boundaries

| Actor | Trust |
| --- | --- |
| Namespace developer | Can create `ConfigSync` in namespaces they control |
| Git committer | Controls delivered config for paths they can merge to |
| Platform admin | Installs/configures the operator, allow-lists, RBAC scope |
| External attacker | Untrusted network |
| Compromised operator pod | Bounded by operator ServiceAccount |

**v1 stance (RD8):** namespace-level trust — creating a `ConfigSync` is
security-equivalent to creating a `Pod` that can mount namespace `Secret`s.

## Threat → control mapping

| ID | Threat | Control | Owning step |
| --- | --- | --- | --- |
| TM1 | Confused deputy: wire arbitrary namespace Secrets | R-AUTH.1 document Pod-equivalent RBAC; optional R-AUTH.2 referable-secret policy (**post-1.0** — see `docs/security.md`) | S3.1 / `docs/security.md` |
| TM2 | Attacker-controlled git + permissive ESO stores | R-AUTH.3 source allow-list; R-AUTH.4 manifest kind/store allow-list | S1.1, S2.4, S3.1 |
| TM3 | Cross-namespace reach | R-AUTH.5 locality (no cross-ns fields on API; manifest guard) | S1.3, S2.4 |
| TM4 | Git-credential theft via arbitrary Secret ref | R-AUTH.6 `kohen.dev/git-credential` label | S1.1, S1.8 |
| TM5 | SSRF via `source.url` | R-AUTH.7 scheme + IP + redirect guards | S1.1 |
| TM6 | Malicious repo tree | R7.5 tree safety | S1.2 |
| TM7 | Git write ≈ delivery authority | Documented; branch protection is the control | S3.2 |
| TM8 | Compromised operator pod | Namespace-scoped install; documented blast radius | S3.1 |
| TM9 | Secret leakage | R8.3 redacting logger + leak tests | S0.2, all reconcile steps |

## Operator configuration (platform admin)

| Setting | Purpose |
| --- | --- |
| `sourceAllowList` | Restrict git URLs (R-AUTH.3) |
| `secretStoreAllowList` | Restrict ESO `secretStoreRef` names (R-AUTH.4) |
| `maxDegradedDuration` | Bound fail-safe staleness (R8.11) |
| `allowInsecureGitTLS` | Gate insecure TLS/SSH opt-outs (default `false`) |

## RBAC shape (T7)

Two install scopes:

1. **Cluster** — `ClusterRole` watches all namespaces; smallest blast radius
   for multi-tenant clusters is still the operator SA across served namespaces.
2. **Namespaced** — `Role` in the release namespace only; `WATCH_NAMESPACE`
   limits reconciliation.

Rules (shared): read/write `ConfigSync`; read labeled credential Secrets;
read referenced Secrets/ExternalSecrets; write owned ConfigMaps/ExternalSecrets;
`patch` target Deployments/StatefulSets; leader-election leases.

## Security review checklist (for PRs)

- [ ] No secret values in logs, events, status, or metrics (R8.3)
- [ ] New network fetches enforce R-AUTH.7 URL guards
- [ ] New object writes use SSA field manager `kohen` with ownership labels
- [ ] New user-visible fields documented in README Getting Started + Advanced
- [ ] Abuse-case or leak tests updated when touching reconcile/logging

## Consequences

- Strict multi-tenant authorization (admission webhook binding repos to
  namespaces) is **post-1.0** (SPEC §19).
- Per-name `resourceNames` RBAC for dynamic secret refs is not generally
  possible; namespace scoping is the primary blast-radius control.
