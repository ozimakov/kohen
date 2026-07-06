# Secret Integration (ESO + native) — U2 guide

This guide is the verified path for referencing **secrets** from a `ConfigSync`.
It is exercised end-to-end on `kind` with a real External Secrets Operator
(`test/e2e`, U2 in [`PLAN.md`](../PLAN.md)) and covers both v1 backends:

- **`externalSecret`** — an [External Secrets Operator](https://external-secrets.io)
  `ExternalSecret` (the recommended path; ESO fetches from Vault/AWS/GCP/…).
- **`nativeSecret`** — a pre-existing Kubernetes `Secret` (created by
  cert-manager, another controller, or `kubectl`).

Kohen **never reads or produces secret material** (design principle P2). It only
*awaits* a secret's readiness and *wires* the resulting `Secret` into the
workload as files or env vars, and — for ESO — *applies* the `ExternalSecret`
manifest committed to git. The secret value reaches the pod through the backing
`Secret` only; it never appears in git, logs, events, or `ConfigSync` status
(`SPEC.md` R8.3 / TM9).

- [1. Concepts](#1-concepts)
- [2. Declaring secret references](#2-declaring-secret-references)
- [3. Native `Secret` backend](#3-native-secret-backend)
- [4. External Secrets Operator backend](#4-external-secrets-operator-backend)
- [5. Readiness, fail-closed, and fail-safe](#5-readiness-fail-closed-and-fail-safe)
- [6. Rotation](#6-rotation)
- [7. Guard rails (operator/platform team)](#7-guard-rails-operatorplatform-team)
- [8. Choosing a backend: Vault-via-ESO decision tree](#8-choosing-a-backend-vault-via-eso-decision-tree)
- [9. Troubleshooting](#9-troubleshooting)

---

## 1. Concepts

A `ConfigSync` lists the secrets its config references under `spec.secretRefs`.
Each reference names a backend object and a **surface** — how the resolved
`Secret` is delivered into the pod:

| Surface (`surface.as`) | Delivered as | Live-updates? | Rotation triggers rollout? |
| --- | --- | --- | --- |
| `file` | `Secret` volume mounted at `mountPath` (never `subPath`) | Yes (kubelet, in place) | **No** — updated in place |
| `env` | one `env` entry via `valueFrom.secretKeyRef` (`envVar` ← `key`) | No | **Yes** by default (`rolloutOnRotate`) |

Kohen merges only its own **keyed** list entries (a volume, a volume mount, a
discrete `env` entry — never `envFrom`) into the target container via
Server-Side Apply, exactly like the config volume, so it coexists with GitOps
appliers (see the [GitOps coexistence](./getting-started-and-gitops.md#gitops-coexistence)
rules — the same ignore rules apply to the secret volume/mount and env entries).

## 2. Declaring secret references

`spec.secretRefs[]` fields (`SPEC.md` §8.1, §8.4):

| Field | Type | Description |
| --- | --- | --- |
| `name` | string (required) | Unique local name (DNS-1123 label, ≤ 50 chars). Also names the wired volume. |
| `backend` | `externalSecret` \| `nativeSecret` | Resolution mechanism. |
| `externalSecret.name` | string | `ExternalSecret` object name (`backend: externalSecret`). |
| `nativeSecret.name` | string | `Secret` name (`backend: nativeSecret`). |
| `surface.as` | `file` \| `env` | Surfacing mode. |
| `surface.mountPath` | string | Mount path for `as: file`. Must be unique and differ from `wiring.mountPath`. |
| `surface.envVar` | string | Env var name for `as: env`. |
| `surface.key` | string | `Secret` data key exposed by the env var (`as: env`). |
| `surface.rolloutOnRotate` | bool (default `true`) | Whether an **env** rotation advances the config version. Ignored for `file`. |

Validation (rejected by the API server / reconciler): duplicate `name`s,
duplicate `mountPath`s or `envVar`s, a `mountPath` equal to `wiring.mountPath`,
`rollout: none` combined with an env surface, and a surface whose fields don't
match its `as` mode. All references are **namespace-local**: the backing object
lives in the `ConfigSync`'s own namespace (R-AUTH.5).

## 3. Native `Secret` backend

Use `nativeSecret` when a `Secret` already exists in the namespace (created by
cert-manager, sealed-secrets, another operator, or `kubectl`). Kohen awaits its
existence (and, for an env surface, the referenced `key`) and wires it.

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
  secretRefs:
    - name: tls
      backend: nativeSecret
      nativeSecret:
        name: checkout-tls        # e.g. a cert-manager Certificate's Secret
      surface:
        as: file
        mountPath: /etc/checkout/tls
    - name: db
      backend: nativeSecret
      nativeSecret:
        name: checkout-db
      surface:
        as: env
        envVar: DB_PASSWORD
        key: password
```

The rotation token is **metadata-only** — the `Secret`'s `resourceVersion` and
its sorted key set (R8.10) — so Kohen detects rotation without reading values.

> Kohen does not cache arbitrary `Secret`s; referenced `Secret`s are read
> on demand via an uncached API reader (TM8). A native `Secret` rotation is
> therefore picked up on the poll interval (`spec.sync.interval`), not instantly.

## 4. External Secrets Operator backend

With ESO, the source of truth is an external store (Vault/AWS/GCP/…). You
**commit the `ExternalSecret` manifest to git** alongside your config; Kohen
applies it (owned + pruned), waits for ESO to report `Ready=True`, then wires
the target `Secret` it produces.

1. A platform team installs ESO and creates a `SecretStore`/`ClusterSecretStore`
   (this is ESO configuration, out of scope for `ConfigSync`).
2. Commit an `ExternalSecret` under your config path — it is **excluded from the
   ConfigMap** automatically (R7.6):

   ```yaml
   # services/checkout/prod/external-secret.yaml
   apiVersion: external-secrets.io/v1
   kind: ExternalSecret
   metadata:
     name: checkout-api
   spec:
     refreshInterval: 1h
     secretStoreRef:
       name: vault-backend      # must be in the operator allow-list (§7)
       kind: SecretStore
     target:
       name: checkout-api       # the Secret ESO creates (defaults to this name)
     data:
       - secretKey: token
         remoteRef:
           key: secret/data/checkout
           property: api-token
   ```

3. Reference it from the `ConfigSync`:

   ```yaml
   spec:
     secretRefs:
       - name: api
         backend: externalSecret
         externalSecret:
           name: checkout-api    # the ExternalSecret / its target Secret
         surface:
           as: env
           envVar: API_TOKEN
           key: token
   ```

Kohen applies the `ExternalSecret`, gates first resolution on its `Ready=True`
condition (R8.9), and wires the target `Secret` (`spec.target.name`, defaulting
to the `ExternalSecret` name). The rotation token is ESO's
`status.syncedResourceVersion` when present, else the target `Secret`'s
`resourceVersion` (R8.10) — metadata only.

Deleting the `ConfigSync` (or removing the manifest from git) prunes the owned
`ExternalSecret`; ESO then removes the target `Secret` it created.

## 5. Readiness, fail-closed, and fail-safe

Kohen applies an **asymmetric readiness policy** (R8.9), surfaced on the
`SecretsReady` condition and folded into `Ready`:

- **First resolution fails closed.** A reference that has *never* been wired and
  is not yet resolvable holds the workload: `SecretsReady=False /
  AwaitingFirstResolution`, no wiring, no rollout. Kohen never rolls pods that
  would crash on a missing secret. Once the secret appears, it resolves and
  rolls.
- **An established reference fails safe.** A reference that *was* wired and
  rolled, then goes transiently not-ready (store outage, ESO not Ready, Secret
  briefly deleted), keeps the workload running on last-good:
  `SecretsReady=False / DegradedServingLastGood`. It auto-recovers.
- **Bounded degradation.** If an established reference serves last-good longer
  than the operator's `maxDegradedDuration` (default `15m`, R8.11), Kohen
  surfaces the security-visible `MaxDegradedExceeded` (condition + Warning event
  + metric) and still refuses to advance until it recovers.

## 6. Rotation

Rotation behavior depends on the surface (R8.5, R-CONS):

- **Env-surfaced** secrets fold their metadata token into the config version, so
  a rotation **advances the version and triggers exactly one rollout** (the pod
  must restart to pick up a changed env var). Set `surface.rolloutOnRotate:
  false` to opt out.
- **File-surfaced** secrets update **in place** — the kubelet refreshes the
  mounted volume and **no rollout occurs**. Use this for certificates and other
  files the app reloads without restarting.

## 7. Guard rails (operator/platform team)

The apply-if-present engine only touches recognized, namespaced, allow-listed
kinds (R-AUTH.4/R-AUTH.5, TM2/TM3). Configure them at install time (`SPEC.md`
§12); see [`deploy/helm/kohen/values.yaml`](../deploy/helm/kohen/values.yaml):

| Value | Default | Purpose |
| --- | --- | --- |
| `operatorConfig.secretStoreAllowList` | `[]` (no restriction) | Names of secret stores an applied `ExternalSecret` may reference. When set, **every** `secretStoreRef` — top-level and per-`data[]`/`dataFrom[]` `sourceRef` — must be listed, `generatorRef` is refused, and a manifest with no verifiable store reference is refused. |
| `operatorConfig.maxDegradedDuration` | `15m` | How long an established reference may serve last-good before `MaxDegradedExceeded`. |

Additional guard rails, always on:

- **Only `external-secrets.io/ExternalSecret`** (namespaced) may be applied from
  git — never cluster-scoped secret CRs (e.g. `ClusterExternalSecret`).
- An applied manifest **must target the `ConfigSync`'s namespace** (an explicit
  foreign `metadata.namespace` is rejected).
- Kohen **never adopts** a pre-existing, un-owned object of the same name (R8.8).

> **Security note.** Because a `ConfigSync` can cause an `ExternalSecret` to be
> applied, "who may create a `ConfigSync`" is as sensitive as "who may create a
> Pod that mounts a Secret" (R-AUTH.1). Set `secretStoreAllowList` in production
> so a compromised config repo cannot pull from an arbitrary store.

## 8. Choosing a backend: Vault-via-ESO decision tree

```
Does the secret already live in a Kubernetes Secret you control the lifecycle of
(cert-manager, sealed-secrets, another operator)?
├─ Yes → backend: nativeSecret  (Kohen awaits + wires it; no manifest to commit)
└─ No — it lives in an external store (Vault, AWS/GCP/Azure SM, …)?
   └─ Use External Secrets Operator:
      1. Platform team installs ESO and a SecretStore/ClusterSecretStore
         pointing at the store (e.g. Vault) — once per cluster/tenant.
      2. Commit the ExternalSecret next to your config in git.
      3. backend: externalSecret, referencing that ExternalSecret.
      → Prefer this for anything sourced from Vault: ESO owns auth to Vault and
        the sync policy; Kohen only awaits Ready and wires the target Secret.
```

Rule of thumb: **native** for secrets another in-cluster controller already
materializes; **ESO** for anything that originates in an external secret manager
(Vault being the canonical case). Sealed Secrets is a post-1.0 backend
(`PLAN.md`).

## 9. Troubleshooting

Read `status.conditions` (`SecretsReady`, `ManifestsApplied`) and events with
`kubectl -n <ns> describe configsync <name>`.

| Reason | Condition | Meaning & first action |
| --- | --- | --- |
| `AwaitingFirstResolution` | `SecretsReady=False` | A never-wired reference isn't resolvable yet. Create the `Secret`, or wait for ESO to make the `ExternalSecret` Ready. No rollout until it resolves (by design). |
| `DegradedServingLastGood` | `SecretsReady=False` | An established reference went transiently not-ready; the workload keeps running on last-good and auto-recovers. |
| `MaxDegradedExceeded` | `SecretsReady=False` | Degraded past `maxDegradedDuration`. Investigate the store/ESO/Secret; security-visible. |
| `SecretNotFound` | per-`secretRefs[]` status | The native `Secret` (or ESO target) is absent. Check the name/namespace. |
| `KeyMissing` | per-`secretRefs[]` status | An env surface's `key` is not in the backing `Secret`. Fix `surface.key` or the `Secret`. |
| `BackendNotReady` | `SecretsReady=False` | ESO hasn't reported `Ready=True` (or the ESO CRD isn't installed). Check the `ExternalSecret` and ESO. |
| `StoreNotAllowed` | `ManifestsApplied=False` | The `ExternalSecret` references a store outside `secretStoreAllowList`. Fix the manifest or the allow-list. |
| `ManifestNamespaceViolation` | `ManifestsApplied=False` | An applied manifest sets a foreign `metadata.namespace`. Remove it (same-namespace only). |
| `ManifestKindNotAllowed` | `ManifestsApplied=False` | A committed manifest is a kind Kohen won't apply (only `ExternalSecret`). |
| `ManifestApplyFailed` | `ManifestsApplied=False` | Apply/prune error, or a pre-existing un-owned object of the same name (no adoption). |

No secret value ever appears in these conditions, events, or status — only the
backing `Secret` and the pod carry it (R8.3).
