# Changelog

All notable changes to kube-rightsize will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added
- CodeQL analysis in security workflow (PRs and weekly)
- NetworkPolicy Helm template (disabled by default)
- PodDisruptionBudget Helm template (when replicaCount > 1)
- Topology spread constraints Helm value
- helm-docs auto-generated README with CI freshness check
- Artifact Hub metadata for chart discoverability
- OCI labels in Dockerfile for local builds
- `make verify` target (runs all CI checks locally)
- `make clean` target (removes build artifacts)
- Makefile help section header rendering
- kubectl plugin `--all-namespaces/-A`, `--kubeconfig`, `--namespace` flags
- Leader election RBAC in kustomize config (coordination.k8s.io/leases)
- Webhook validation: safetyMargin format, negative cooldown rejection
- Webhook defaults: controlledValues, historyWindow, cooldown
- Default resource bound constants (DefaultCPUBoundsMin/Max, DefaultMemoryBoundsMin/Max)
- Condition and annotation key constants (ConditionReady, ReasonMonitoring, etc.)
- RightSizeDefaults printcolumn markers (Prometheus, Mode, Age)
- Kubebuilder validation markers: CanaryConfig.Percentage [1,100], MaxChangePercent [1,100]
- Prometheus query duration and error metrics instrumentation
- AllowDecrease enforcement (memory decreases blocked by default)
- HPA conflict detection wired into reconcile loop
- PromQL value escaping for defense-in-depth
- 25 Helm unit tests (was 6)
- Tests for safetyMargin validation, cooldown validation, new defaults, AllowDecrease clamping, escapePromQL

### Fixed
- Tracking annotations persisted to API server (deferred safety observation was non-functional)
- Deferred safety metrics use workload name from annotation instead of pod.Labels["app"]
- Inline safety observation uses configured period instead of hardcoded 30s
- Consistent timestamps within executeResizes (single time.Now())
- E2E continue-on-error removed (was silently making E2E tests decorative)
- Trivy release scan blocks on HIGH/CRITICAL vulnerabilities (was exit-code 0)
- Removed unused Scheme var from groupversion_info.go
- Removed duplicate Grafana dashboard from chart (deploy/grafana/ is canonical)
- Removed phantom prometheus.address from docs, SPEC.md, and installation guide
- Removed phantom apiresource and ctrlclient import aliases from AGENTS.md and linter config
- Fixed README: bounds are optional with safe defaults, not required fields
- Fixed examples/README.md: 06-multi-workload-selector uses Recommend, not Canary
- Fixed coverage threshold inconsistency: docs now match CI (75%)
- Fixed stale make test description in docs/contributing/testing.md
- Fixed CONTRIBUTING.md: use make targets instead of raw go test commands
- Fixed integration test time.Sleep replaced with assert.Eventually
- Added missing Helm values to docs/reference/configuration.md
- Improved Helm NOTES.txt example policy with complete cpu/memory/bounds config
- goimports comment-preceded blank import conflict resolved

### Changed
- Upgraded formatters: gofumpt + gci + goimports (community standard)
- Branch protection on main requiring Lint status check
- Coverage upload now runs on PRs (was main-only)
- Prometheus collector logs query warnings instead of discarding them
- kubectl plugin uses flag package instead of raw os.Args parsing

### Added (initial release)
- Core operator with RightSizePolicy and RightSizeDefaults CRDs
- Composable recommendation engine: percentile, margin, confidence, bounds, change filter
- In-place pod resize via `/resize` subresource (K8s 1.33+ In-Place Pod Resize)
- 5-mode graduated rollout: Observe, Recommend, OneShot, Canary, Auto
- Safety monitor with OOM detection, restart tracking, and auto-revert
- Canary pod selection (percentage-based subset resizing)
- Cooldown enforcement between resize cycles
- HPA conflict detection and annotation opt-out (`rightsize.io/skip`)
- Prometheus metrics collection with pod-level query fallback
- 10 operator Prometheus metrics (`kube_rightsize_*`)
- Cluster-scoped RightSizeDefaults merging for global configuration
- Defaulting and validation webhooks (requires cert-manager)
- kubectl-rightsize plugin with status, savings, and recommendations subcommands
- Helm chart with configurable replicas, resources, RBAC, metrics, and logging
- Grafana dashboard with 12 panels
- Unit, integration, and E2E tests, benchmarks, fuzz targets
- 19 MkDocs documentation pages
- CI/CD pipeline: lint, unit tests, integration tests (envtest), E2E tests (Chainsaw/Kind), CRD freshness, Helm lint, security scan (CodeQL, govulncheck, Trivy), docs build
- Release pipeline: multi-arch container images (GHCR), cosign signing, SBOM, GoReleaser binaries, Helm OCI chart, install manifest
- CONTRIBUTING.md, SECURITY.md, ADOPTERS.md, issue/PR templates

### Fixed
- Safety observation now tracks resized container by name (not Containers[0])
- Malformed resize annotations handled gracefully (no panic)
- Memory quantities preserve BinarySI format through estimator chain
- Status update ordering: status subresource before metadata annotations
- Compare against actual pod resources, not Deployment template spec
- Invalid Go duration format in samples and docs (7d -> 168h)
- build-installer target uses correct container image via kustomize
- Helm webhooks.enabled defaults correctly
- E2E tests assert InsufficientData without Prometheus
- 28 golangci-lint issues resolved
