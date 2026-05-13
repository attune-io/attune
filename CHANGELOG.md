# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Changed

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
