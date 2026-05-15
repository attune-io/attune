# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- Helm values schema validation (`values.schema.json`) for catching typos at install time
- Pending workloads column in `kubectl rightsize status` output
- Secret name and key context in Prometheus auth failure messages
- Nightly E2E tests for bearer-token Secret rotation and recommendations without live pods
- Structured-output test coverage for kubectl plugin (`-o json`, `-o yaml`)
- Documentation for running nightly-only Go E2E scenarios locally

### Changed

- Recommendations no longer require live pods; historical Prometheus data is sufficient for recommend-only flows
- Secret-backed bearer tokens are refreshed on every reconcile instead of being cached until TTL expiry
- Collector cache identity uses hashed token values instead of plain presence markers
- Extracted `buildCollectorOptions` helper from the main `Reconcile` method

### Fixed

- Bearer-token cache prefix collision when one Prometheus address is a prefix of another
- `make test-local` now cleans up the k3d cluster even on mid-run failures
- Gitleaks PATH resolution on self-hosted runners
- `prometheus-unreachable` E2E test assertion updated to match `PrometheusUnavailable` reason

## [0.1.0] - Unreleased

Initial release.
