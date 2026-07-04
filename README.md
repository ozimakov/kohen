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