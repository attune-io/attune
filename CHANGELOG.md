# Changelog

All notable changes to kube-rightsize will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added
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
- Grafana dashboard with 13 panels
- 156 tests (unit, integration, E2E), 7 benchmarks, 2 fuzz targets
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
- Helm webhooks.enabled defaults to false (no webhook templates in chart)
- E2E tests assert InsufficientData without Prometheus
- 28 golangci-lint issues resolved
