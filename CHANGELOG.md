# Changelog

All notable changes to Kohen are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project
adheres to [Semantic Versioning](https://semver.org/).

## [1.0.0] — 2026-07-16

First public product release. API group/version remains `kohen.dev/v1alpha1`
(see `docs/upgrade-uninstall.md`).

### Added

- Config-only and secret-aware `ConfigSync` operator (ESO + native Secret)
- Helm chart + cluster-scoped and namespaced plain-manifest bundles
- Multi-arch release pipeline (amd64/arm64 image, Helm OCI chart, SBOM, cosign)
- Docs suite + project site (`site/`) for GitHub Pages
- U3 acceptance gate automating A1–A12 on kind

### Fixed (pre-tag gap review)

- Retract `kohen-stamp` object annotation on `ConfigSync` deletion
- Reset stale `status.rolloutInProgress` on `rollout: none` and wiring failures
- Skip workload Wire/Stamp writes when the version stamp already matches (A3)
- Observability: secret resolution transition events; apply/prune/wire metrics;
  stuck rollout (`ProgressDeadlineExceeded`) maps to Ready=Degraded

### Documentation

- SPEC promoted from Draft v0.6 to **v1.0**
- Closed nested-path key separator (`__`); documented poll-bounded rotation
- Kubernetes floor aligned to **1.28+**; SECURITY supports **1.0.x**

## [Unreleased]

### Changed

- Project site deploy: publish `site/` from **`main`** via GitHub Actions Pages
  (`actions/deploy-pages`); stop publishing an orphan `gh-pages` branch
- Site and docs: position Kohen as a **Kubernetes-native** operator (native
  ConfigMaps/Secrets, no sidecars or private volumes)
- Release publishing automation: SemVer validation, dry-run packaging,
  digest-based cosign, OCI chart under `ghcr.io/ozimakov/kohen/charts`,
  pinned manifest image tags, `SHA256SUMS`, prerelease-aware `:latest`
- Release workflow triggers on **GitHub Release `published`** and attaches
  artifacts to that release (replacing tag-push → create-release)

[1.0.0]: https://github.com/ozimakov/kohen/releases/tag/v1.0.0
