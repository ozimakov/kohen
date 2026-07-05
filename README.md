# KOHEN

`Kohen` is a Kubernetes-native configuration management tool that allows
applications consume configuration from dedicated `git` repository and consistently update after any change.

`Kohen` is not an alternative to GitOps solutions. It adds additional capabilities for the applications that prefer running against a dedicated configuration repository that covers multiple environments.

## Specification

The project is being revived starting from a clean specification. See
[`SPEC.md`](./SPEC.md) for the full set of technical and non-technical
requirements, the architecture, the consistency model, the threat model, and
the acceptance criteria. Start with the minimal example (SPEC §1.2) and the
"when to use Kohen — and when not" decision table (SPEC §2.4).

The implementation sequence toward **v1.0** — broken into independently
buildable steps (each with its own tests) plus dedicated `kind`-based usability
milestones — is in [`PLAN.md`](./PLAN.md).

## Getting started

The [Getting Started & GitOps Coexistence runbook](./docs/getting-started-and-gitops.md)
is the verified, copy-pasteable path for the config-only journey: install via
Helm, sync a git path to a workload, roll out on config changes, wire private
repo auth, and coexist with Argo CD / Flux. It is exercised end-to-end in CI on
`kind` (see [`test/e2e`](./test/e2e)).