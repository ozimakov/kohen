# KOHEN

`Kohen` is a Kubernetes-native configuration management tool that allows
applications consume configuration from dedicated `git` repository and consistently update after any change.

`Kohen` is not an alternative to GitOps solutions. It adds additional capabilities for the applications that prefer running against a dedicated configuration repository that covers multiple environments.

## Specification

The project is being revived starting from a clean specification. See
[`SPEC.md`](./SPEC.md) for the full set of technical and non-technical
requirements, the architecture, the consistency model, and the phased roadmap.