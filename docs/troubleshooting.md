# Troubleshooting

Map symptoms to `ConfigSync` conditions (SPEC §11.4, R10.2). Run
`kubectl describe configsync <name>` to see conditions and events.

## Config fetch & render

| Symptom | Condition / reason | First action |
| --- | --- | --- |
| Sync stuck, old config still served | `Fetched=False/FetchFailed` | Check URL, ref, network; Kohen keeps last-good |
| Private repo auth errors | `Fetched=False/AuthFailed` | Verify credential Secret exists, is labeled `kohen.dev/git-credential=true`, has correct keys |
| URL blocked by policy | `Fetched=False/SourceNotAllowed` | Fix URL or `operatorConfig.sourceAllowList` |
| Wrong path | `Fetched=False/PathNotFound` | Correct `spec.path` for the ref |
| Config too large | `Rendered=False/Oversize` | Reduce config (~1 MiB `ConfigMap` limit) or split path |
| Unsafe file in tree | `Rendered=False/TreeSafetyViolation` | Remove `..`, symlinks, absolute paths |
| Bad file names | `Rendered=False/InvalidKey` or `KeyConflict` | Fix nested path key collisions |

## Secret resolution

| Symptom | Condition / reason | First action |
| --- | --- | --- |
| Never wired, waiting for secret | `SecretsReady=False/AwaitingFirstResolution` | Create `Secret` or make `ExternalSecret` Ready |
| Secret/key missing | `SecretsReady=False/SecretNotFound` or `KeyMissing` | Create backing object with required keys |
| ESO not ready | `SecretsReady=False/BackendNotReady` | Fix ESO store / provider; check `ExternalSecret` status |
| Was good, backend blipped | `SecretsReady=False/DegradedServingLastGood` | Wait for recovery; workload keeps last-good |
| Degraded too long | `SecretsReady=False/MaxDegradedExceeded` | Investigate underlying secret backend; check `maxDegradedDuration` |

## Manifest apply (git-committed ExternalSecrets)

| Symptom | Condition / reason | First action |
| --- | --- | --- |
| Wrong kind committed | `ManifestsApplied=False/ManifestKindNotAllowed` | Only `ExternalSecret` may be applied from git |
| Foreign namespace in manifest | `ManifestsApplied=False/ManifestNamespaceViolation` | Remove `metadata.namespace` or match ConfigSync ns |
| Store not allowed | `ManifestsApplied=False/StoreNotAllowed` | Fix `secretStoreRef` or operator allow-list |
| Malformed manifest | `ManifestsApplied=False/ManifestInvalid` | Fix YAML shape |

## Workload wiring & rollout

| Symptom | Condition / reason | First action |
| --- | --- | --- |
| Target missing | `WorkloadWired=False/WorkloadNotFound` | Create/fix `workloadRef` |
| OnDelete / Recreate strategy | `WorkloadWired=False/UnsupportedStrategy` | Use rolling strategy or `rollout: none` |
| GitOps stripped fields | `WorkloadWired=False/ApplyConflict` | Apply [GitOps ignore rules](./getting-started-and-gitops.md#gitops-coexistence) |
| Duplicate sync | `WorkloadWired=False/SingletonViolation` | Remove extra `ConfigSync` on same workload |
| Rollout stuck | `RolloutComplete=False/RollingOut` or `ProgressDeadlineExceeded` | Check pod crash loop; pin prior ref to roll back |

## Overall readiness

| `Ready` reason | Meaning |
| --- | --- |
| `Synced` | Desired version applied, wired, converged |
| `Progressing` | Rollout in flight |
| `Degraded` | One or more steps failed; see sub-conditions |

## Operator health

If the operator pod is down, existing objects and stamps persist but no new
versions apply. Check:

```bash
kubectl -n kohen-system get pods
kubectl -n kohen-system logs deploy/kohen
```

## Security / abuse

Disallowed URLs, unlabeled credentials, and allow-list violations are covered in
[`docs/security.md`](./security.md#abuse-cases-a11).
