# Getting Started & GitOps Coexistence (U1 runbook)

This runbook is the verified, copy-pasteable path for the **config-only** journey
covered by usability milestone **U1** (`PLAN.md`). It is exercised end-to-end in
CI on `kind` (`test/e2e`), so every command below is expected to work verbatim.

Kohen turns a path in a **dedicated git config repository** into the `ConfigMap`
a workload consumes, wires that `ConfigMap` into the workload, and rolls the
workload when the config changes. It is **not** a GitOps/CD engine — it composes
with Argo CD and Flux rather than replacing them (see
[GitOps coexistence](#gitops-coexistence) and `SPEC.md` §2.2/§2.4).

- [1. Prerequisites](#1-prerequisites)
- [2. Install Kohen (Helm)](#2-install-kohen-helm)
- [3. Day-1: sync a path to a workload](#3-day-1-sync-a-path-to-a-workload)
- [4. Verify the wiring](#4-verify-the-wiring)
- [5. Change config → rollout](#5-change-config--rollout)
- [6. Private-repo authentication](#6-private-repo-authentication)
- [7. Force an immediate sync](#7-force-an-immediate-sync)
- [8. Rollback](#8-rollback)
- [9. Uninstall / cleanup](#9-uninstall--cleanup)
- [10. GitOps coexistence](#gitops-coexistence)
- [11. Troubleshooting](#11-troubleshooting)

---

## 1. Prerequisites

- A Kubernetes cluster (v1.29+) and `kubectl` context pointing at it.
- `helm` v3.13+.
- A **dedicated git config repository** reachable from the cluster over HTTPS or
  SSH, containing the config files you want to deliver under some path (for
  example `services/checkout/prod/`).
- The target workload (a `Deployment` or `StatefulSet`) already exists in the
  namespace where you will create the `ConfigSync`.

`ConfigSync` is **namespace-local by construction**: the git source, the
credential Secret, the target workload, and the rendered `ConfigMap` all live in
the `ConfigSync`'s own namespace (`SPEC.md` R-AUTH.5).

## 2. Install Kohen (Helm)

Kohen installs in one of two scopes (`SPEC.md` §16):

**Cluster scope** (watch all namespaces; installs a `ClusterRole`):

```bash
helm install kohen deploy/helm/kohen \
  --namespace kohen-system --create-namespace \
  --set scope=cluster \
  --wait
```

**Namespaced scope** (watch only the release namespace; installs a `Role`):

```bash
helm install kohen deploy/helm/kohen \
  --namespace my-team --create-namespace \
  --set scope=namespaced \
  --wait
```

Useful operator-config values (rendered into the operator `ConfigMap`, `SPEC.md`
§12); all are optional:

| Value | Default | Purpose |
| --- | --- | --- |
| `operatorConfig.sourceAllowList` | `[]` (all allowed) | Restrict which git hosts/URL-prefixes may be used as sources (`R-AUTH.3`). |
| `operatorConfig.maxDegradedDuration` | `15m` | How long to serve last-good before surfacing `MaxDegradedExceeded`. |
| `operatorConfig.allowInsecureGitTLS` | `false` | Permit per-source `insecure-skip-tls-verify` (test/self-signed servers only). |

> **Security note.** Leave `allowInsecureGitTLS=false` in production. Setting an
> explicit `sourceAllowList` is strongly recommended so a compromised namespace
> cannot point Kohen at an arbitrary URL. Regardless of the allow-list, Kohen
> always blocks source hosts that resolve to loopback, link-local (including the
> `169.254.169.254` metadata endpoint), unspecified, or multicast addresses, and
> re-screens every HTTP redirect hop (`SPEC.md` R-AUTH.7 / TM5).

## 3. Day-1: sync a path to a workload

This is the minimum viable usage from `SPEC.md` §1.2 — apply it **verbatim**
(adjust names/URL for your environment). Prerequisite: a `Deployment` named
`checkout` exists in namespace `checkout`.

```yaml
apiVersion: kohen.dev/v1alpha1
kind: ConfigSync
metadata:
  name: checkout-prod
  namespace: checkout
spec:
  source:
    url: https://github.com/acme/platform-config.git
    ref: main
  path: services/checkout/prod
  workloadRef:
    kind: Deployment
    name: checkout
```

```bash
kubectl apply -f checkout-prod.yaml
```

With only these fields, Kohen applies the defaults from `SPEC.md` §11.2:

| Concern | Default | Field to override |
| --- | --- | --- |
| ConfigMap name | `<workloadRef.name>-config` → `checkout-config` | `spec.configMap.name` |
| Target container | first container in the pod spec | `spec.wiring.container` |
| Mount path | `/etc/kohen/config` | `spec.wiring.mountPath` |
| Ref | `main` | `spec.source.ref` |
| Rollout mode | `auto` | `spec.rollout` (`auto` \| `none`) |
| Sync interval | `30s` | `spec.sync.interval` |

Kohen then (1) renders `services/checkout/prod@main` into ConfigMap
`checkout-config`; (2) merges a volume + mount at `/etc/kohen/config` into the
first container via Server-Side Apply (Kohen-owned fields only); (3) stamps the
config version as the pod-template annotation `kohen.dev/config-sha`; and (4) on
every future change, updates the `ConfigMap` and triggers exactly one rolling
update (`rollout: auto`).

## 4. Verify the wiring

```bash
# READY + CONFIG VERSION columns:
kubectl -n checkout get configsync checkout-prod

# The rendered ConfigMap:
kubectl -n checkout get configmap checkout-config

# The version stamp on the pod template:
kubectl -n checkout get deploy checkout \
  -o jsonpath='{.spec.template.metadata.annotations.kohen\.dev/config-sha}'
```

Inspect status and conditions (`SPEC.md` §11.4):

```bash
kubectl -n checkout get configsync checkout-prod -o yaml
```

Key status fields:

- `status.sourceCommit` — resolved git commit SHA (correlates with `git log`).
- `status.configVersion` — desired rollout-trigger identity (`git:<short-sha>`).
- `status.workloadVersion` — version currently stamped on the workload.
- `status.conditions` — `Fetched`, `Rendered`, `WorkloadWired`,
  `RolloutComplete`, and the overall `Ready`.

Kohen injects a volume named **`kohen-config`** and a matching `volumeMount` into
the target container. These field names matter for the GitOps ignore rules below.

## 5. Change config → rollout

Commit a change to the watched path in your config repo. Within one sync
interval Kohen re-renders the `ConfigMap` and, in `rollout: auto`, triggers
**exactly one** rolling update; the mounted volume in running pods is updated by
the kubelet, and the new pods carry the new `kohen.dev/config-sha`. A subsequent
reconcile with **no change** performs **no rollout** (`SPEC.md` A2/A3).

`rollout: none` updates the `ConfigMap` (and thus the mounted files in place) but
does **not** roll the workload — use it for apps that reload config without a
restart. In `none` mode Kohen still keeps the volume/mount wired and records the
version on the workload object rather than the pod template.

> `subPath` mounts and env vars do **not** receive live updates; that is exactly
> what the rollout is for. Kohen-managed mounts never use `subPath` (`SPEC.md`
> §1.3).

## 6. Private-repo authentication

Create a Secret in the **same namespace** as the `ConfigSync`, labeled
`kohen.dev/git-credential=true` (enforced at reconcile time, `SPEC.md`
R-AUTH.6), and reference it from `spec.source.authSecretRef`.

**HTTPS token:**

```bash
kubectl -n checkout create secret generic git-creds \
  --from-literal=username=git \
  --from-literal=password=<TOKEN>
kubectl -n checkout label secret git-creds kohen.dev/git-credential=true
```

**SSH key** (use an `ssh://…` or `git@…:…` source URL):

```bash
kubectl -n checkout create secret generic git-creds \
  --from-file=ssh-privatekey=./id_ed25519 \
  --from-file=known_hosts=./known_hosts
kubectl -n checkout label secret git-creds kohen.dev/git-credential=true
```

Reference it:

```yaml
spec:
  source:
    url: https://github.com/acme/platform-config.git
    ref: main
    authSecretRef:
      name: git-creds
```

On bad credentials the `ConfigSync` goes `Degraded` with reason `AuthFailed` and
emits an actionable, **redacted** event — credentials never appear in logs,
events, or status (`SPEC.md` R8.3 / TM9).

## 7. Force an immediate sync

To bypass the poll interval and reconcile now, set the force-sync annotation on
the `ConfigSync`; Kohen processes it and clears it:

```bash
kubectl -n checkout annotate configsync checkout-prod kohen.dev/sync-now=1 --overwrite
```

## 8. Rollback

Pin `spec.source.ref` to a prior tag or commit SHA. Kohen restores that
version's config and (in `auto`) rolls the workload back to it (`SPEC.md` UC6).
Tag/commit pins are immutable; a branch ref tracks the moving branch.

```bash
kubectl -n checkout patch configsync checkout-prod --type=merge \
  -p '{"spec":{"source":{"ref":"v1.4.2"}}}'
```

## 9. Uninstall / cleanup

Deleting the `ConfigSync` prunes the objects Kohen owns (the `ConfigMap`) and
**unwires** the workload — the injected volume/mount and version stamp are
retracted — while leaving the workload itself intact (`SPEC.md` A7).

```bash
kubectl -n checkout delete configsync checkout-prod
```

---

## GitOps coexistence

Kohen mutates the target workload only by **merging Kohen-owned fields** via
Server-Side Apply with the dedicated field manager `kohen` (plus `kohen-stamp`
for the version annotation). It never force-takes fields owned by another
manager (`SPEC.md` §6.2, R-WIRE.1–R-WIRE.5).

**Coexistence is guaranteed only when the other manager (a) uses Server-Side
Apply and (b) applies the ignore rules below** for the fields Kohen owns.
Client-side / whole-object appliers overwrite the whole pod template and **will
strip Kohen's volume, mount, and stamp** — that configuration is unsupported
without these ignore rules (`SPEC.md` R-WIRE.5).

Fields Kohen owns on the target workload (what the other controller must ignore):

- pod-template volume `spec.template.spec.volumes` entry named **`kohen-config`**;
- the target container's `volumeMounts` entry named **`kohen-config`**;
- the pod-template annotation **`kohen.dev/config-sha`**.

### Argo CD (Server-Side Apply + `ignoreDifferences`)

Enable SSA and ignore Kohen's fields so Argo CD does not flap or revert them.

Application-level `syncPolicy` / `ignoreDifferences`:

```yaml
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: checkout
spec:
  syncPolicy:
    syncOptions:
      - ServerSideApply=true
  ignoreDifferences:
    - group: apps
      kind: Deployment
      name: checkout
      namespace: checkout
      jsonPointers:
        - /spec/template/metadata/annotations/kohen.dev~1config-sha
      jqPathExpressions:
        - .spec.template.spec.volumes[] | select(.name == "kohen-config")
        - .spec.template.spec.containers[].volumeMounts[] | select(.name == "kohen-config")
```

> `~1` is the JSON-Pointer escape for `/` in the annotation key
> `kohen.dev/config-sha`.

### Flux (Kustomize controller, SSA + drift exclusion)

Flux applies server-side by default with the `Override` policy, which reverts
fields it does not own. Set the `Merge` apply policy on the managed `Deployment`
so Flux preserves the non-overlapping fields Kohen owns (the annotation values
are case-sensitive PascalCase: `Override` \| `Merge` \| `IfNotPresent` \|
`Ignore`):

```yaml
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: checkout
  namespace: flux-system
spec:
  # ...
  patches:
    - target:
        kind: Deployment
        name: checkout
      patch: |
        - op: add
          path: /metadata/annotations/kustomize.toolkit.fluxcd.io~1ssa
          value: Merge
```

Equivalently, annotate the `Deployment` manifest in git directly:

```yaml
metadata:
  annotations:
    kustomize.toolkit.fluxcd.io/ssa: Merge
```

Because Kohen's volume/mount are keyed map-list members (by `name`/`mountPath`)
and its stamp is a discrete annotation, `Merge` preserves them without overlap;
these are exactly the non-atomic fields the policy can retain.

With SSA + the ignore rules applied, the GitOps controller and Kohen converge on
the same `Deployment` without flapping: the GitOps controller owns the app spec,
Kohen owns its config volume/mount/stamp (`SPEC.md` A10).

---

## 11. Troubleshooting

Every failure surfaces as a condition reason (`SPEC.md` §10/§11.4). Read them
with `kubectl -n <ns> get configsync <name> -o yaml` (see `status.conditions`)
and check events with `kubectl -n <ns> describe configsync <name>`.

| Reason | Condition | Meaning & first action |
| --- | --- | --- |
| `FetchFailed` | `Fetched=False` | Git unreachable / ref missing. Check URL, ref, network; Kohen keeps serving last-good and auto-recovers. |
| `AuthFailed` | `Fetched=False` | Bad or missing credentials. Verify the Secret exists, is labeled `kohen.dev/git-credential=true`, and has the right keys. |
| `SourceNotAllowed` | `Fetched=False` | URL blocked by the allow-list, or host resolves to a blocked (loopback/link-local/metadata) address. Fix `operatorConfig.sourceAllowList` or the URL. |
| `PathNotFound` | `Fetched=False` | `spec.path` does not exist at the ref. Correct the path. |
| `Oversize` | `Rendered=False` | Rendered content exceeds the ~1 MiB `ConfigMap` limit. Split the config or reduce it (`SPEC.md` A8). |
| `TreeSafetyViolation` | `Rendered=False` | Symlink escape / unsafe tree. Remove offending files. |
| `InvalidKey` / `KeyConflict` | `Rendered=False` | File names are not valid `ConfigMap` keys or collide. Rename files. |
| `WorkloadNotFound` | `WorkloadWired=False` | The `workloadRef` target does not exist in the namespace. Create it or fix the ref. |
| `UnsupportedStrategy` | `WorkloadWired=False` | `rollout: auto` requires a rolling strategy; `OnDelete` StatefulSet / `Recreate` Deployment is unsupported for auto rollout. Switch strategy or use `rollout: none`. |
| `ApplyConflict` | `WorkloadWired=False` | Another manager owns a field Kohen needs. Apply the [GitOps ignore rules](#gitops-coexistence). |
| `SingletonViolation` | `WorkloadWired=False` | More than one `ConfigSync` targets the same workload; only one may. Delete the duplicate. |
| `MaxDegradedExceeded` | `Ready=False` | Degraded longer than `operatorConfig.maxDegradedDuration`. Investigate the underlying `Fetched`/`Rendered` failure. |

The workload always stays healthy during source outages: Kohen retains the
last-good `ConfigMap` and never prunes on fetch/render failure (`SPEC.md` §10,
fail-safe).
