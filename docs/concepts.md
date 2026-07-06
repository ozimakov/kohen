# Concepts

Kohen keeps a workload's **configuration** and **secret wiring** in sync with a
path in a dedicated git repository — then rolls the workload when the version
changes.

## Core objects

| Concept | Description |
| --- | --- |
| **Config repo** | A git repository holding environment-specific config files |
| **ConfigSync** | The CRD that binds `repo@ref:path` → workload |
| **Source commit** | The resolved git SHA (`status.sourceCommit`) |
| **Config version** | Rollout identity stamped as `kohen.dev/config-sha` |
| **Secret reference** | A pointer in `spec.secretRefs` — never a value |

## Reconcile flow

1. **Fetch** git at `spec.source.ref` + `spec.path`
2. **Render** files → `ConfigMap` (manifests and `kohen.*` excluded)
3. **Resolve** `spec.secretRefs` (ESO or native `Secret`)
4. **Apply** owned objects via Server-Side Apply (field manager `kohen`)
5. **Wire** volumes/mounts/env into the target workload
6. **Stamp** config version; trigger rollout only on change
7. **Report** status conditions (§11.4)

## Consistency model

- One resolved commit per reconcile cycle
- Config version = `git:<short-sha>` plus `-sec:<hash>` when env-surfaced secrets exist
- File-surfaced secret rotation updates in place (no rollout); env rotation rolls once

## Namespace locality (R-AUTH.5)

`workloadRef`, `configMap`, credential Secrets, and resolved Secrets must all
live in the **same namespace** as the `ConfigSync`. There is no cross-namespace
mode in v1.

## GitOps coexistence

Kohen merges only its owned fields. Argo CD / Flux must use **Server-Side
Apply** and the documented ignore rules — see
[Getting Started & GitOps](./getting-started-and-gitops.md#gitops-coexistence).

## When to use Kohen

See the decision table in [`SPEC.md`](../SPEC.md) §2.4 and the README summary.
