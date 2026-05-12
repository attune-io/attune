# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [Unreleased]

### Dependencies

- Bump `sigs.k8s.io/controller-runtime` from v0.24.0 to v0.24.1 (patch release).

### Changed

- **BREAKING**: Default `minimumDataPoints` lowered from 168 (7 days) to 48
  (2 days). Existing policies with explicit values are unaffected. New policies
  will start producing recommendations after ~2 days instead of ~7. Set
  `metricsSource.minimumDataPoints: 168` to restore the previous behavior.
- **BREAKING**: `kube_rightsize_prometheus_query_errors_total` metric changed
  from a bare Counter to a CounterVec with `namespace` and `query_type` labels.
  Update Grafana dashboards and alerting rules that reference this metric.
  The bundled Grafana dashboards are already updated.
- `kube_rightsize_prometheus_query_duration_seconds` histogram now includes a
  `query_type` label (e.g., `cpu_range`, `memory_range`, `cpu_fallback`,
  `memory_fallback`).

### Added

- Selector-based policy conflict detection: `CheckPolicyConflict` now evaluates
  `spec.targetRef.selector` (both `matchLabels` and `matchExpressions`) when
  detecting overlapping policies. Previously, selector-based policies were
  invisible to conflict detection.
- SSRF-safe HTTP transport for Prometheus queries with DNS rebinding protection.
  Blocks loopback, link-local, AWS IMDSv2 IPv6 (`fd00:ec2::254`), and cloud
  metadata hostnames.
- Controller-level defense-in-depth: `historyWindow` clamped to [1h, 720h],
  `parseFloat64` rejects NaN/Inf/negative/zero, `scaleLimits` detects int64
  overflow from extreme limit/request ratios.
- Container-level `seccompProfile: RuntimeDefault` in Helm chart defaults.
- Optional `NetworkPolicy` Helm template (enable with
  `networkPolicy.enabled=true`).
- `govulncheck` added to `make verify`.
- Controller benchmarks for `buildPrometheusQuery` and
  `computeRecommendations`.
- Helm template check that fails with a clear message when
  `webhooks.enabled=true` but cert-manager CRDs are not installed.
- License boilerplate check in CI lint job.
- E2E test results now output JUnit XML via Chainsaw.

### Fixed

- `quantityFromFloat` heuristic replaced with explicit `isCPU` flag, fixing
  incorrect resource formatting for workloads with >100 CPU cores.
- Prometheus addresses standardized to `http://prometheus-server.monitoring:80`
  across all docs, examples, and samples.
- ResourceQuota memory error messages now use human-readable units (e.g.,
  `256Mi`) instead of raw bytes.
- Safety observation: `CheckPod` errors no longer fall through to act on a
  zero-value `SafetyVerdict` (which would trigger false-positive reverts).
- `checkPendingSafetyObservations` no longer calls `discoverWorkloads`
  redundantly; workloads are passed from the caller.
- SPEC.md status `ResourceValues` structure corrected from nested
  `cpu.request` to flat `cpuRequest` fields.
- `historyWindow` minimum corrected from 24h to 1h in SPEC.md.
- `PartialFailure` reason removed from SPEC.md (phantom); `WorkloadDiscoveryFailed`
  added to docs.
- Grafana dashboards updated for `query_type` label on both duration and
  error metrics.
- Safety throttle check errors promoted from V(1) debug to Error level.

### Improved

- Conflict detector DRY: `CheckVPAConflict` and `CheckPolicyConflict` now
  delegate to their `InMemory` counterparts, eliminating ~40 duplicated lines.
- `ValidatePrometheusAddress` extracted to shared `internal/validation/`
  package, removing the controller's dependency on the webhook package.
- `computePercentiles` sorts in place instead of copying, reducing GC pressure
  at scale (~96MB saved per reconcile at 500 workloads).
- CI tool versions centralized into env block: `CERT_MANAGER_VERSION`,
  `K3S_IMAGE`, `ENVTEST_K8S_VERSION`, `HELM_UNITTEST_VERSION`.
- `yamllint` auto-installed in Makefile instead of failing with a terse message.
- `make verify` widened to check deepcopy and RBAC freshness (matching CI).
