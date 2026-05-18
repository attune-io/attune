# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- Configurable `burstSensitivity` per resource: controls how much burst detection inflates recommendations (default 0.1, set 0 to disable)
- Canary auto-promotion resets on spec change: editing a policy restarts the observation cycle so new configuration is re-validated
- `kube_rightsize_burst_factor` Prometheus metric and Grafana dashboard panel showing burst detection multiplier per workload
- Burst detection now influences recommendations via logarithmic safety-margin boost
- Canary auto-promotion: when `autoPromote: true`, the operator automatically promotes to full fleet resize after the observation period passes without safety violations
- VPA conflict detection E2E test (Chainsaw scenario with inline CRD)
- OOMKill safety revert Go E2E test (uses stress-ng for reliable OOMKill trigger)
- Helm values schema validation (`values.schema.json`) for catching typos at install time
- Pending workloads column in `kubectl rightsize status` output
- Secret name and key context in Prometheus auth failure messages
- Go E2E tests for bearer-token Secret rotation and recommendations without live pods
- Structured-output test coverage for kubectl plugin (`-o json`, `-o yaml`)
- Documentation for running the full Go E2E suite locally

### Changed

- Reconcile predicate filters out self-triggered status and metadata updates, reducing kube-apiserver load by eliminating 2-3x reconcile amplification per cycle
- Recommendations no longer require live pods; historical Prometheus data is sufficient for recommend-only flows
- Secret-backed bearer tokens are refreshed on every reconcile instead of being cached until TTL expiry
- Collector cache identity uses hashed token values instead of plain presence markers
- Extracted `buildCollectorOptions` helper from the main `Reconcile` method

### Fixed

- Status race condition where concurrent reconciles could reset `status.workloads.resized` to 0 after a successful resize; Resized count is now derived from resize history entries which survive optimistic concurrency conflicts
- `kube_rightsize_throttle_deferred_total` metric now appears in the Grafana dashboard (was the only unvisualized operator metric)
- `RightSizeNamespaceDefaults` CRD missing from `config/crd/kustomization.yaml`; kustomize deployments now include it
- Bearer-token cache prefix collision when one Prometheus address is a prefix of another
- `make test-local` now cleans up the k3d cluster even on mid-run failures
- Gitleaks PATH resolution on self-hosted runners
- `prometheus-unreachable` E2E test now accepts either `InsufficientData` or `PrometheusUnavailable` reason, fixing a flake where the first reconcile sets one reason and subsequent reconciles set another

## [0.1.0] - Unreleased

Initial release.
