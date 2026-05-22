# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- Support for Kubernetes 1.32 with `InPlacePodVerticalScaling` alpha feature gate; the operator now falls back to the deprecated `pod.Status.Resize` field for resize status on clusters without the 1.33+ pod conditions
- Top-level `safetyObservationPeriod` field on `UpdateStrategy` for configuring post-resize safety watch duration (default 5m, minimum 1m); takes precedence over `canary.observationPeriod` and works in all modes
- Early OOMKill and crash loop detection during safety observation period: critical events trigger immediate revert without waiting for the full observation period
- `kubectl rightsize explain` now displays the effective observation period with source tracking
- Configurable `rateWindow` field for CPU PromQL queries; no longer hardcoded to `[5m]`, now tracks `queryStep` by default
- Effective cooldown with backoff multiplier exposed in policy status
- Recommendation staleness detection with `LastDataTime` and `Stale` fields; stale recommendations block resize execution
- `StaleRecommendationsTotal` metric for tracking Prometheus degradation
- `ScheduleBlocked` status condition when outside the configured resize window
- `SCHEDULE` column in `kubectl rightsize status` output
- Per-policy namespace/name labels on `ReconcileDuration` metric
- Per-policy reconcile duration panel in Grafana dashboard (p99/p50 by namespace and policy)
- ReplicaSet as a supported target workload kind with adapter, RBAC, and Helm clusterrole
- Cross-namespace Secret reference rejection in webhook validation
- `KubeRightsizeHighRevertRate` PrometheusRule alert in Helm chart
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

- Shorter requeue interval during data collection phase for faster initial recommendation generation
- `canary.percentage` CRD minimum changed from 0 to 1 (a 0% canary is meaningless)
- `rateWindow` is inheritable via `RightSizeDefaults` and `RightSizeNamespaceDefaults`
- Deployment-owned ReplicaSets are filtered from target discovery to prevent double-resizing
- Reconcile predicate filters out self-triggered status and metadata updates, reducing kube-apiserver load by eliminating 2-3x reconcile amplification per cycle
- Recommendations no longer require live pods; historical Prometheus data is sufficient for recommend-only flows
- Secret-backed bearer tokens are refreshed on every reconcile instead of being cached until TTL expiry
- Collector cache identity uses hashed token values instead of plain presence markers
- Extracted `buildCollectorOptions` helper from the main `Reconcile` method
- Documentation now clarifies that `minimumDataPoints` counts Prometheus range-query samples, so wall-clock recommendation timing depends on `queryStep`
- Reserved Prometheus query parameters (`query`, `start`, `end`, `step`, `time`, `timeout`) are now rejected so operator-managed request keys cannot be overridden

### Fixed

- `golang.org/x/net` updated to v0.55.0 to fix GO-2026-5026 (Punycode validation vulnerability in `idna`)
- Trivy image scan CI failure on runners without BuildKit/buildx; the step now strips BuildKit-only Dockerfile directives and builds natively with the legacy builder
- `make docker-build` now sets `DOCKER_BUILDKIT=1` so the Dockerfile's `--platform=$BUILDPLATFORM` resolves on legacy Docker CLIs
- `kubectl rightsize explain` was missing `safetyObservationPeriod` merge from namespace/cluster defaults, showing wrong effective value
- `StaleRecommendationsTotal` metric label mismatch between registration and increment
- E2E test flakes: OOMKill timeout, GuaranteedQoS queryStep, ScaleUp timeout, Chainsaw poll intervals, rateWindow regression with short queryStep
- Status race condition where concurrent reconciles could reset `status.workloads.resized` to 0 after a successful resize; Resized count is now derived from resize history entries which survive optimistic concurrency conflicts
- `kube_rightsize_throttle_deferred_total` metric now appears in the Grafana dashboard (was the only unvisualized operator metric)
- `RightSizeNamespaceDefaults` CRD missing from `config/crd/kustomization.yaml`; kustomize deployments now include it
- Bearer-token cache prefix collision when one Prometheus address is a prefix of another
- `make test-local` now cleans up the k3d cluster even on mid-run failures
- Gitleaks PATH resolution on self-hosted runners
- `prometheus-unreachable` E2E test now accepts either `InsufficientData` or `PrometheusUnavailable` reason, fixing a flake where the first reconcile sets one reason and subsequent reconciles set another
- Multi-container sequential resize: annotation persist now retries on 409 Conflict instead of reverting the second container
- Memory limit clamp for K8s v1.33: in-place memory limit decreases are skipped when the container's resize policy is `NotRequired`, preventing API server rejection
- Guaranteed QoS preservation with memory limit clamp: the clamp is applied before the QoS check so that Guaranteed pods are not incorrectly resized into Burstable
- `helm-unittest` download now uses dynamic OS/arch detection instead of hardcoded `linux-amd64`

## [0.1.0] - Unreleased

Initial release.
