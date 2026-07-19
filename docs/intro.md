# What is Kohen

Kohen is a Kubernetes operator for one pattern: **an application that consumes
domain-specific configuration from a dedicated git repository**.

Config (and its secret wiring) lives in a reviewable, multi-environment config
repo — separate from the deployment repo. Kohen syncs a path from that repo into
a `ConfigMap`, mounts it into the workload, and rolls the workload when the
config version changes.

It is **not** a GitOps control plane. Deploy **what** runs with Argo CD / Flux;
keep **config** in sync with Kohen.

## The main pattern

```text
Config repo                     Deploy / GitOps
(domain + env config)           (app manifests)
        │                              │
        │ path                         │ Deployment
        ▼                              ▼
     ┌──────┐                   ┌──────────────┐
     │Kohen │── ConfigMap ─────▶│   Workload   │
     └──────┘── mount+rollout ─▶└──────────────┘
```

One `ConfigSync` declares **repo + path + workload**. Kohen renders, wires,
stamps a version, and rolls once when that version changes.

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

## When to use Kohen — and when not

| Scenario | Use Kohen? | Notes |
| --- | --- | --- |
| Dedicated config repo drives a workload's `ConfigMap` + secret wiring + rollouts | **Yes** | Core use case |
| GitOps deploys the app; config lives in a **separate** config repo | **Yes** | Apply [GitOps ignore rules](./getting-started-and-gitops.md#gitops-coexistence) |
| GitOps already renders the app **and** its `ConfigMap` from the same repo | No | A second reconciler adds no value |
| Config exceeds `ConfigMap` size (~1 MiB) or is a large file tree | No | Prefer a `git-sync`-to-volume pattern |
| You only need secrets from an external store | No | Use [External Secrets Operator](https://external-secrets.io/) directly |
| You hand-author a `ConfigMap` and only want restart-on-change | No | Reloader (or similar) is enough |
| You want product feature toggles / experiments in code | No | Use a feature-flag platform |

## Next steps

1. [Install](./install.md)
2. [Getting started & GitOps](./getting-started-and-gitops.md)
3. [Concepts](./concepts.md)
