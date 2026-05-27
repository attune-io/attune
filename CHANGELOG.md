# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.1.4](https://github.com/attune-io/attune/compare/v0.1.3...v0.1.4) (2026-05-27)


### Features

* add arm/v7, ppc64le, and s390x architecture support ([bc3f814](https://github.com/attune-io/attune/commit/bc3f814283533990e4377c94c0f43750bd554aac))


### Bug Fixes

* use stable checksums filename for SLSA provenance ([6e5c151](https://github.com/attune-io/attune/commit/6e5c151989e8f6c99118dac5a29ef72126fbdd3d))

## [0.1.3](https://github.com/attune-io/attune/compare/v0.1.2...v0.1.3) (2026-05-27)


### Bug Fixes

* use tag refs for SLSA provenance reusable workflows ([d1ac09a](https://github.com/attune-io/attune/commit/d1ac09a6f604cfc51318d0d73a14d4947b657bc4))

## [0.1.2](https://github.com/attune-io/attune/compare/v0.1.1...v0.1.2) (2026-05-27)


### Features

* publish container image to Docker Hub for discoverability ([#102](https://github.com/attune-io/attune/issues/102)) ([9f8ffd5](https://github.com/attune-io/attune/commit/9f8ffd5fd1a1baa5e6d59b49ea46505c0bad94dd))

## [0.1.1](https://github.com/attune-io/attune/compare/v0.1.0...v0.1.1) (2026-05-27)


### Bug Fixes

* **ci:** pin all transitive pip dependencies with hashes ([#85](https://github.com/attune-io/attune/issues/85)) ([9cafc42](https://github.com/attune-io/attune/commit/9cafc4221dd6b3fa14ea1a15479e70f14a9d0611))
* convert logo from JPG to PNG with transparent corners ([#43](https://github.com/attune-io/attune/issues/43)) ([fcb3b23](https://github.com/attune-io/attune/commit/fcb3b23355ee934f7e6e9b9cee89cef89c5ca209))
* correct hallucinated email in artifacthub-repo.yml ([#56](https://github.com/attune-io/attune/issues/56)) ([4a50290](https://github.com/attune-io/attune/commit/4a50290e156723339fea0a7cf91e591faebc5aea))
* e2e nightly RealisticLoad timeout + safe cache keys for secrets (no SHA256) ([#44](https://github.com/attune-io/attune/issues/44)) ([2bed71a](https://github.com/attune-io/attune/commit/2bed71a0fcb58a241b17385d463ffd61070f183a))
* **e2e:** replace stress-ng with busybox CPU burn and update SECURITY.md ([#86](https://github.com/attune-io/attune/issues/86)) ([de76adc](https://github.com/attune-io/attune/commit/de76adc97cb994096f4ca6779b48b7dfd8c5da7f))
* **e2e:** resolve recommend-mode Chainsaw intermittent timeout ([#92](https://github.com/attune-io/attune/issues/92)) ([c50e2ac](https://github.com/attune-io/attune/commit/c50e2acdf288d2040d86d2bb653311db6c4d53a8))
* **e2e:** use explicit Command for stress-ng and add deployment diagnostics ([#83](https://github.com/attune-io/attune/issues/83)) ([c269cdf](https://github.com/attune-io/attune/commit/c269cdf908adc125bb0190bce5f2192bf666cb24))
* remove stress-ng --vm stressor from RealisticLoad E2E test ([#59](https://github.com/attune-io/attune/issues/59)) ([6b7efa9](https://github.com/attune-io/attune/commit/6b7efa9a39682680c4d8ff5d7d5cc17f55d35aca))
* scope workflow token permissions to job level for Scorecard ([#42](https://github.com/attune-io/attune/issues/42)) ([5251468](https://github.com/attune-io/attune/commit/52514682e36a307e1a0c4a235d0a76255888f79a))
* stabilize Chainsaw tests and add govulncheck to CI gate ([#95](https://github.com/attune-io/attune/issues/95)) ([011c8ad](https://github.com/attune-io/attune/commit/011c8adc56ee9c6a0e43cf2c13dfec27f18862f4))

## [Unreleased]

## [0.1.0] - 2025-05-26

### Added

- Support for Kubernetes 1.32 with `InPlacePodVerticalScaling` alpha feature gate; the operator now falls back to the deprecated `pod.Status.Resize` field for resize status on clusters without the 1.33+ pod conditions
- Top-level `safetyObservationPeriod` field on `UpdateStrategy` for configuring post-resize safety watch duration (default 5m, minimum 1m); takes precedence over `canary.observationPeriod` and works in all modes
- Early OOMKill and crash loop detection during safety observation period: critical events trigger immediate revert without waiting for the full observation period
- `kubectl attune explain` now displays the effective observation period with source tracking
- Configurable `rateWindow` field for CPU PromQL queries; no longer hardcoded to `[5m]`, now tracks `queryStep` by default
- Effective cooldown with backoff multiplier exposed in policy status
- Recommendation staleness detection with `LastDataTime` and `Stale` fields; stale recommendations block resize execution
- `StaleRecommendationsTotal` metric for tracking Prometheus degradation
- `ScheduleBlocked` status condition when outside the configured resize window
- `SCHEDULE` column in `kubectl attune status` output
- Per-policy namespace/name labels on `ReconcileDuration` metric
- Per-policy reconcile duration panel in Grafana dashboard (p99/p50 by namespace and policy)
- ReplicaSet as a supported target workload kind with adapter, RBAC, and Helm clusterrole
- Cross-namespace Secret reference rejection in webhook validation
- `AttuneHighRevertRate` PrometheusRule alert in Helm chart
- Configurable `burstSensitivity` per resource: controls how much burst detection inflates recommendations (default 0.1, set 0 to disable)
- Canary auto-promotion resets on spec change: editing a policy restarts the observation cycle so new configuration is re-validated
- `attune_burst_factor` Prometheus metric and Grafana dashboard panel showing burst detection multiplier per workload
- Burst detection now influences recommendations via logarithmic safety-margin boost
- Canary auto-promotion: when `autoPromote: true`, the operator automatically promotes to full fleet resize after the observation period passes without safety violations
- VPA conflict detection E2E test (Chainsaw scenario with inline CRD)
- OOMKill safety revert Go E2E test (uses stress-ng for reliable OOMKill trigger)
- Helm values schema validation (`values.schema.json`) for catching typos at install time
- Pending workloads column in `kubectl attune status` output
- Secret name and key context in Prometheus auth failure messages
- Go E2E tests for bearer-token Secret rotation and recommendations without live pods
- Structured-output test coverage for kubectl plugin (`-o json`, `-o yaml`)
- Documentation for running the full Go E2E suite locally
- V(1) debug log when a resize is skipped because the container is already at the target resources
- **Initial sizing webhook**: Mutating admission webhook sets pod resource requests/limits at creation time based on existing AttunePolicy recommendations, eliminating the "deploy with bad defaults" gap. Requires namespace label `attune.io/initial-sizing=enabled` and `initialSizing: true` on the policy. Safety: `failurePolicy: Ignore`, confidence threshold 0.5, stale check.
- **Directional change caps**: `maxIncreasePercent` (default 50%) and `maxDecreasePercent` (default 30%) in ResourceConfig for asymmetric per-step caps (memory decreases are riskier than CPU increases)
- **Memory-from-CPU derivation**: `memoryFromCpuRatio` in ResourceConfig derives memory recommendation from CPU (e.g., `"2.0"` for JVM heap-bound workloads), skipping Prometheus memory queries
- Wizard `create` and `promote` flows now prompt for initial sizing when mode is Auto, OneShot, or Canary
- **SLO-based guardrails**: `updateStrategy.sloGuardrails[]` defines application-level PromQL checks (latency, error rate) evaluated after each resize during the safety observation period. Breaching a threshold triggers automatic revert. Supports template variables for namespace, workload, and pod name.
- **VPA recommendation consumption**: `metricsSource.vpa` consumes existing VerticalPodAutoscaler recommendations as an alternative to Prometheus queries, bridging VPA-only clusters into Attune's in-place resize engine
- **GitOps diff command**: `kubectl attune diff` outputs resource change recommendations in YAML diff format for GitOps workflows (ArgoCD, Flux). Supports `-o yaml` structured output.
- **spec.paused**: Boolean field on `AttunePolicySpec` that halts all reconciliation (metrics collection, recommendations, resizes) without reverting existing resizes. The operator sets `Ready=False` with `reason=Paused`. Modeled after Prometheus Operator and Flux `spec.suspend`.
- **Webhook warnings for nonsensical config**: 13 admission-time warnings detect ineffective settings (e.g., canary config in non-canary mode, SLO guardrails with VPA source, resize-only settings in Observe/Recommend mode)
- **Runtime K8s events**: 31 warning/event types (up from 3) for silent controller behaviors: `StaleRecommendation`, `CooldownActive`, `HPAConflict`, `VPAConflict`, `ConfigClamped`, `ExportFailed`, `ResizeSkipped`, `BudgetExhausted`, and more. All recurring events use 1-hour deduplication to prevent log spam.
- **Warning suppression**: `attune.io/suppress-warnings` annotation accepts a comma-separated list of event reasons to suppress (e.g., `HPAConflict,ConfigClamped`)

### Changed

- **BREAKING**: `safetyMargin` field renamed to `overhead` with percentage semantics. Old multiplier values must be converted: `(old - 1) * 100` (e.g., `safetyMargin: "1.2"` becomes `overhead: "20"`). Defaults changed from `"1.2"`/`"1.3"` to `"20"`/`"30"`. Validation bounds changed from `(0, 10.0]` to `[0, 900]`.
- **BREAKING**: `maxCpuChangePercent` and `maxMemoryChangePercent` moved from `updateStrategy` to `cpu`/`memory` as `maxChangePercent`. Groups all per-resource recommendation parameters in one place.
- **BREAKING**: `updateStrategy.mode` field renamed to `updateStrategy.type` to align with Kubernetes core conventions
- **BREAKING**: `bounds.min`/`bounds.max` renamed to `minAllowed`/`maxAllowed`, `InPlaceOrEvict` renamed to `InPlaceOrRecreate`, `excludeContainers` renamed to `excludedContainers`
- Shorter requeue interval during data collection phase for faster initial recommendation generation
- `canary.percentage` CRD minimum changed from 0 to 1 (a 0% canary is meaningless)
- `rateWindow` is inheritable via `AttuneDefaults` and `AttuneNamespaceDefaults`
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
- `kubectl attune explain` was missing `safetyObservationPeriod` merge from namespace/cluster defaults, showing wrong effective value
- `StaleRecommendationsTotal` metric label mismatch between registration and increment
- E2E test flakes: OOMKill timeout, GuaranteedQoS queryStep, ScaleUp timeout, Chainsaw poll intervals, rateWindow regression with short queryStep
- Status race condition where concurrent reconciles could reset `status.workloads.resized` to 0 after a successful resize; Resized count is now derived from resize history entries which survive optimistic concurrency conflicts
- `attune_throttle_deferred_total` metric now appears in the Grafana dashboard (was the only unvisualized operator metric)
- `AttuneNamespaceDefaults` CRD missing from `config/crd/kustomization.yaml`; kustomize deployments now include it
- Bearer-token cache prefix collision when one Prometheus address is a prefix of another
- `make test-local` now cleans up the k3d cluster even on mid-run failures
- Gitleaks PATH resolution on self-hosted runners
- `prometheus-unreachable` E2E test now accepts either `InsufficientData` or `PrometheusUnavailable` reason, fixing a flake where the first reconcile sets one reason and subsequent reconciles set another
- RevertPod now retries on 409 Conflict (matching ResizePod); previously a conflict during revert left the pod at unsafe resource levels until the next reconcile
- Datadog and CloudWatch collector caches now share the same TTL eviction, capacity bounds, and race-safe LoadOrStore as the Prometheus collector cache; previously they could leak memory and create duplicate collectors
- Startup boost expiry pre-check now includes memory values, preventing node allocatable safety check bypass when namespaces have memory LimitRange constraints
- Annotation cleanup in safety observation now retries on 409 Conflict (up to 3 attempts), matching the persistResizeAnnotations retry pattern
- Multi-container sequential resize: annotation persist now retries on 409 Conflict instead of reverting the second container
- Memory limit clamp for K8s v1.33: in-place memory limit decreases are skipped when the container's resize policy is `NotRequired`, preventing API server rejection
- Guaranteed QoS preservation with memory limit clamp: the clamp is applied before the QoS check so that Guaranteed pods are not incorrectly resized into Burstable
- `helm-unittest` download now uses dynamic OS/arch detection instead of hardcoded `linux-amd64`
- OOMKill E2E test: `RestartContainer` memory resize policy hides OOM evidence by overwriting `LastTerminationState` on resize-induced restarts; test now uses `NotRequired` policy
- Safety revert path now applies K8s v1.33 memory limit clamp (`ClampMemoryLimitForPolicy`), preventing revert failures when memory limits would decrease with `NotRequired` resize policy
- CI image builds switched from Docker/BuildKit to `ko`, eliminating Docker daemon dependency and containerd storage race conditions on macOS self-hosted runners
- k3d image import retry loops with pre-cleanup for macOS containerd storage flakes
- Confidence factor formula `(1+M/C)^E` produced a 4x multiplier at maximum confidence (7 days of data), inflating all recommendations well beyond the user's configured overhead. A workload with P95=200m and `overhead: "20"` converged to ~960m instead of the expected ~240m. Replaced with `1 + M*(1-C)^E` which gives factor=1.0 at full confidence and up to 1.8x at minimum confidence.
- `memoryFromCpuRatio` values above 10.0 (e.g., `"16.0"` for in-memory databases) were silently rejected by the shared `parseFloat64` parser, disabling the feature without any error or warning. The ratio now uses a dedicated parser with a 1000.0 ceiling.
