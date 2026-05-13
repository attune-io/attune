# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Breaking Changes

- Webhook validation now **rejects** `cpu.percentile` and `memory.percentile`
  values outside `{50, 90, 95, 99}`. Previously, unsupported values (e.g., 75)
  were accepted with a warning and silently fell back to P95. Existing policies
  with unsupported percentile values must be updated before they can be modified.

### Added

- **Observe mode** is now a distinct mode from Recommend. Observe collects
  metrics and tracks data-point progress but does not surface recommendations
  or savings estimates. Use it as a zero-footprint warm-up phase before
  switching to Recommend.
- **RightSizeNamespaceDefaults** CRD (namespace-scoped, short name `rsnd`).
  Provides namespace-level defaults that override cluster-scoped
  `RightSizeDefaults`. Precedence: policy spec > namespace defaults >
  cluster defaults. Useful for multi-tenant clusters with different
  environments (e.g., production vs staging).
- **Job and CronJob support.** The `targetRef.kind` field now accepts `Job`
  and `CronJob`. Batch workloads are recommend-only: the operator computes
  recommendations from historical Prometheus usage but does not attempt
  in-place resizes (batch pods complete and are not available for resize).
- **Concurrent pod processing** via `maxConcurrentResizes` on
  `updateStrategy`. Resizes multiple pods in parallel within a reconcile
  cycle using a semaphore-bounded worker pool. Default: 1 (serial,
  preserving current behavior). Budget caps are thread-safe under
  concurrent access.
- **Multi-data-source support** for Thanos, VictoriaMetrics, Grafana Mimir,
  and managed Prometheus services (AMP, GMP, Azure). PrometheusConfig now
  supports custom headers (e.g., `X-Scope-OrgID` for Mimir), bearer token
  auth from a Kubernetes Secret, and TLS settings. The PromQL queries work
  unchanged since these backends implement the Prometheus HTTP API.
- **Scheduled resize windows** via `updateStrategy.schedule`. Restrict
  when resizes can occur using time-of-day windows, day-of-week constraints,
  and configurable timezone. Recommendations are computed continuously;
  only resize execution is gated. Supports overnight windows that wrap
  past midnight (e.g., 22:00-06:00). Default: no schedule (anytime).
- **Per-cycle budget caps** via `maxTotalCpuIncrease` and
  `maxTotalMemoryIncrease` on `updateStrategy`. Limits the total aggregate
  resource increase across all pods in a single reconcile cycle. Prevents
  sudden cluster-wide resource spikes when many pods need increases.
  Decreases do not consume budget. Default: unlimited (current behavior).
- **Eviction fallback** via `resizeMethod: InPlaceOrEvict`. When in-place
  resize fails (node full, kubelet marks Infeasible), the operator falls
  back to pod eviction via the Eviction API. Safety: respects PDBs, never
  evicts the last running replica, honors cooldown and canary percentages.
  Default is `InPlaceOnly` (current behavior, no disruption).
- Go module path updated from `github.com/SebTardif/kube-rightsize` to
  `github.com/SebTardifLabs/kube-rightsize` to match the repo location.

### Changed

- **Observe mode behavior** (breaking for users relying on Observe writing
  recommendations): Observe mode no longer writes recommendations or
  savings to `status`. Previously it was an alias for Recommend. Switch
  to Recommend mode to restore the old behavior.
- RBAC: Added `list` and `watch` verbs to `nodes`, `resourcequotas`, and
  `limitranges` ClusterRole rules. Previously only `get` (and `list` for
  quotas/limitranges) was granted, which prevented controller-runtime's
  informer cache from functioning. These permissions are required for node
  allocatable pre-checks and quota/LimitRange compatibility validation
  before resize operations. The operator continues to function (with
  degraded pre-checks) if these permissions are denied.

### Fixed

- Resize engine: `buildResizeTarget` no longer includes zero-valued limits
  when `controlledValues` is `RequestsOnly`, preventing Kubernetes from
  rejecting resize operations with "limits cannot be added" errors.
- Resize engine: `mergeResources` preserves existing pod limits when the
  resize target has none, and clamps memory limits to prevent forbidden
  in-place memory limit decreases.
- Status race condition: concurrent reconciles no longer overwrite the
  `status.workloads.resized` count with a stale zero value.
- `scaleLimits` no longer sets limit equal to request when the current limit
  is zero. Returns zero instead, preventing unintended QoS class changes.
- `RevertPod` now searches both init containers and regular containers,
  fixing silent revert failures for native sidecars.
- Trivy image scan in CI now uses `docker save` tar instead of image-ref,
  fixing "UNAUTHORIZED" errors when scanning locally-built images.
