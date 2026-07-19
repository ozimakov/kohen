# Contributing to Kohen

Thanks for contributing. Behavioral requirements live in [`SPEC.md`](./SPEC.md);
start with the [README](./README.md) for usage docs. AI agents and automation
must follow [`AGENT.md`](./AGENT.md) (ground rules, TDD, change checklist).

## Development setup

```bash
make verify          # tidy, vet, build, unit+envtest
make test-integration
```

For end-to-end tests you need `kind`, `kubectl`, `helm`, and Docker:

```bash
make docker-build kind-load e2e
```

See [`test/README.md`](./test/README.md) for how tests are organized.

## Pull requests

1. Keep changes focused; prefer one concern per PR.
2. Ship tests with behavior changes.
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
