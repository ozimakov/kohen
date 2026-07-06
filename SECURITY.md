# Security Policy

## Supported versions

| Version | Supported |
| --- | --- |
| 0.1.x | Yes |
| < 0.1 | No |

## Reporting a vulnerability

Please **do not** report security vulnerabilities through public GitHub issues.

Email the maintainers with:

- A description of the issue
- Steps to reproduce
- Impact assessment
- Suggested fix (if any)

We aim to acknowledge reports within 5 business days.

## Security architecture

See [`docs/security.md`](./docs/security.md) and
[`docs/adr/000-threat-model.md`](./docs/adr/000-threat-model.md).

## Automated assurance

- Secret-leak scanners on reconcile/logging packages (R8.3, NFR9)
- RBAC and pod-security conformance tests (A9)
- Abuse-case regression suite (A11)
