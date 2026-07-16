# Security hardening guide

Kohen's threat model is defined in [`SPEC.md`](../SPEC.md) §3.3 and expanded in
[`docs/adr/000-threat-model.md`](./adr/000-threat-model.md). This guide is for
platform and security reviewers installing Kohen in production.

## ConfigSync create ≈ Pod create (R-AUTH.1 / TM1)

Anyone who can create a `ConfigSync` in a namespace can wire **any** `Secret`
in that namespace into a pod they control — the same trust Kubernetes already
grants to pod creators. Grant `configsyncs` create/update permissions with the
same care as `pods` create.

Optional tightening:

- Restrict who receives `configsyncs` RBAC via your standard namespace RBAC.
- Configure operator-level allow-lists (below) so only approved git sources and
  ESO stores can be used.

**R-AUTH.2 (referable-secret policy)** — an operator-level policy restricting
which Secrets a `ConfigSync` may wire — is **not implemented in v1.0** and is
tracked as post-1.0. Until then, namespace RBAC for `configsyncs` is the control
(R-AUTH.1).

## Install scope & blast radius (TM8)

| Scope | Operator watches | Compromised operator can |
| --- | --- | --- |
| `cluster` (default) | All namespaces | Read Secrets + patch workloads in any namespace where a `ConfigSync` exists |
| `namespaced` | Release namespace only | Same, but **only** in the release namespace |

**Recommendation:** use `scope: namespaced` when Kohen serves a single tenant or
team namespace. Install one operator per namespace if you need isolation.

```bash
helm install kohen deploy/helm/kohen \
  --namespace team-a-kohen --create-namespace \
  --set scope=namespaced
```

The operator pod runs **non-root** with a **read-only root filesystem**
(distroless image, UID 65532). Conformance tests assert this posture (A9).

## Allow-lists (R-AUTH.3 / R-AUTH.4)

### Git source allow-list

Restrict which repository URLs syncs may use. Empty = all HTTPS/SSH URLs that
pass SSRF guards.

```yaml
operatorConfig:
  sourceAllowList:
    - https://github.com/acme/
    - ssh://git@git.acme.corp/
```

Syncs to other URLs fail closed with `Fetched=False/SourceNotAllowed`.

### Secret store allow-list

When ESO applies `ExternalSecret` manifests from git, restrict
`secretStoreRef.name`:

```yaml
operatorConfig:
  secretStoreAllowList:
    - vault-prod
    - aws-secrets-prod
```

Committed manifests referencing other stores are rejected
(`ManifestsApplied=False/StoreNotAllowed`).

## SSRF & git safety (R-AUTH.7 / TM5)

Regardless of allow-lists, Kohen blocks:

- Non-HTTPS/SSH schemes
- Loopback, link-local (including `169.254.169.254`), unspecified, multicast
- HTTP redirects to those blocked schemes/IPs (re-screened every hop)

**v1.0 note:** redirect hops are **not** re-checked against the optional
`sourceAllowList` (R-AUTH.3) — only scheme/blocked-IP guards apply on redirect.
Prefer pinning sources that do not redirect across hosts, or leave the
allow-list empty until post-1.0 hardens this.

Git credentials must reference Secrets labeled `kohen.dev/git-credential=true`
(R-AUTH.6). Unlabeled Secrets are rejected at reconcile time.

## Secret handling (R8.3 / TM9)

Kohen **never** stores, generates, or logs secret values. Rotation detection
uses metadata tokens only (`resourceVersion`, ESO synced revision).

CI runs leak scanners on every PR touching reconcile/logging packages and in the
U2/U3 e2e suites.

## RBAC reference (T7)

The Helm chart installs least-privilege rules (see
[`deploy/helm/kohen/templates/_helpers.tpl`](../deploy/helm/kohen/templates/_helpers.tpl)):

- `configsyncs` — full reconcile verbs
- `configmaps` — create/update owned maps
- `secrets` — get/list/watch (referenced material)
- `externalsecrets` — apply-if-present lifecycle
- `deployments`, `statefulsets` — get/list/watch/patch (SSA merge only)
- `events` — emit user-visible events

RBAC conformance tests verify reconcile fails when required rules are removed
and recovers when restored (A9).

## Abuse cases (A11)

The following must fail closed (automated in CI):

| Case | Expected condition |
| --- | --- |
| Disallowed `source.url` | `SourceNotAllowed` |
| Unlabeled `authSecretRef` | `AuthFailed` |
| Non-allow-listed manifest kind | `ManifestKindNotAllowed` |
| Manifest targeting foreign namespace | `ManifestNamespaceViolation` |
| Disallowed ESO store | `StoreNotAllowed` |
| Second `ConfigSync` on same workload | `SingletonViolation` |

Cross-namespace references are prevented by API design (no namespace fields on
refs) plus manifest guards.

## Reporting vulnerabilities

See [`SECURITY.md`](../SECURITY.md) at the repository root.
