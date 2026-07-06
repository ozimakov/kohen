# Contributing to Kohen

Thank you for contributing. This project follows the requirements in
[`SPEC.md`](./SPEC.md) and the implementation sequence in [`PLAN.md`](./PLAN.md).

## Development setup

```bash
make verify          # tidy, vet, build, unit+envtest
make test-integration
```

For end-to-end tests you need `kind`, `kubectl`, `helm`, and Docker:

```bash
make docker-build kind-load e2e
```

See [`test/README.md`](./test/README.md) for tier documentation.

## Pull requests

1. One plan step per PR when possible (branch `cursor/<name>-…`).
2. Every change ships tests for the tier(s) declared in the plan step.
3. Update docs when adding user-visible fields (README Getting Started +
   Advanced reference, plus relevant `docs/` pages).
4. Run `make verify` before pushing.

## Security review checklist

From [`docs/adr/000-threat-model.md`](./docs/adr/000-threat-model.md):

- [ ] No secret values in logs, events, status, or metrics (R8.3)
- [ ] New network fetches enforce R-AUTH.7 URL guards
- [ ] New object writes use SSA field manager `kohen`
- [ ] README/docs updated for user-visible changes
- [ ] Abuse-case or leak tests updated when touching reconcile/logging

## Code of conduct

See [`CODE_OF_CONDUCT.md`](./CODE_OF_CONDUCT.md).

## Reporting security issues

See [`SECURITY.md`](./SECURITY.md). Do **not** open public issues for
vulnerabilities.
