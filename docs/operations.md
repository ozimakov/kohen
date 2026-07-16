# Operations (kubectl-first)

Kohen v1 ships without a dedicated CLI (SPEC §15, N7). Use `kubectl` for day-2
operations.

## Status

Printer columns:

```bash
kubectl get configsync -A
# READY  SOURCE-COMMIT  CONFIG-VERSION  WORKLOAD-VERSION  AGE
```

Detailed conditions:

```bash
kubectl describe configsync <name> -n <namespace>
```

Key status fields:

| Field | Meaning |
| --- | --- |
| `status.observedGeneration` | Last reconciled `.metadata.generation` |
| `status.sourceCommit` | Plain git SHA |
| `status.configVersion` | Desired rollout identity |
| `status.workloadVersion` | Stamped version on the workload |
| `status.rolloutInProgress` | Rolling update in flight |
| `status.secretRefs[].established` | Sticky: reference was resolved as part of a wired version (fail-safe eligibility) |

Correlate with the workload annotation:

```bash
kubectl get deploy <workload> -n <ns> \
  -o jsonpath='{.spec.template.metadata.annotations.kohen\.dev/config-sha}'
```

## Force sync

Trigger an immediate reconcile (Kohen clears the annotation):

```bash
kubectl annotate configsync/<name> -n <ns> \
  kohen.dev/sync-now="$(date +%s)" --overwrite
```

## Pin / rollback

Point `spec.source.ref` at a tag or commit SHA:

```bash
kubectl patch configsync/<name> -n <ns> --type=merge \
  -p '{"spec":{"source":{"ref":"<tag-or-sha>"}}}'
```

Or revert the branch in git and wait for the next poll / force-sync.

## Verify from git

The config repository is the source of truth. Diff locally:

```bash
git diff <old-sha> <new-sha> -- path/to/config
```

## Multi-workload pattern (R-SINGLETON)

**One `ConfigSync` per workload.** Multiple workloads sharing the same git path
each get their own `ConfigSync` (and their own `ConfigMap` name). Kohen dedupes
git fetches by repo+commit, but each sync owns its objects independently.

A second `ConfigSync` targeting the same `workloadRef` degrades with
`SingletonViolation`.

## Metrics & health

The operator exposes Prometheus metrics on port 8080 and health endpoints on
8081 (`/healthz`, `/readyz`). See the Helm `metrics.service` values.

## Operator footprint (T6 / NFR3)

Default chart requests/limits (single replica):

| Resource | Request | Limit |
| --- | --- | --- |
| CPU | `50m` | `500m` |
| Memory | `64Mi` | `256Mi` |

Scale envelope (NFR3): **hundreds** of `ConfigSync`es per operator instance is
the design target. Secret informers are label-restricted to git credentials to
keep memory bounded; secret rotation for referenced Secrets is poll-bounded
(≤ `sync.interval`). Size the operator up if you run many syncs with large
rendered ConfigMaps or frequent git fetches.

## Troubleshooting

See the [troubleshooting guide](./troubleshooting.md) for the full symptom →
condition → action matrix.
