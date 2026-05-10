# Contributing to kube-rightsize

Thank you for your interest in contributing! This document provides
guidelines and instructions for contributing.

## Development Setup

### Prerequisites

- Go 1.26+
- Docker
- kubectl
- Kind (for local cluster testing)
- Helm 3.16+

### Local Development

```bash
# Clone the repo
git clone https://github.com/SebTardif/kube-rightsize.git
cd kube-rightsize

# Install dependencies
go mod download

# Generate CRDs and RBAC
make manifests

# Generate deep copy methods
make generate

# Run unit tests
make test

# Run linter
make lint

# Build the operator binary
make build

# Build the container image
make docker-build IMG=kube-rightsize:dev

# Create a local Kind cluster and deploy
make kind-create
make kind-deploy IMG=kube-rightsize:dev
```

### Running Tests

```bash
# Unit tests (with gotestsum, coverage, flaky retry)
make test

# Integration tests (requires envtest binaries)
make test-integration

# E2E tests (requires Kind cluster)
make kind-create
make test-e2e

# All tests
make test-all

# All CI checks locally (lint, test, helm-docs, CRD freshness)
make verify
```

## Pull Request Process

1. Fork the repository and create a branch from `main`
2. Make your changes with tests
3. Run `make manifests` if you changed CRD types
4. Run `make verify` to run all CI checks locally
5. Submit a pull request

### Commit Messages

Follow [Conventional Commits](https://www.conventionalcommits.org/):

```
feat: add time-of-day-aware algorithm
fix: handle nil Prometheus response gracefully
docs: update quickstart guide
test: add fuzz tests for estimator chain
chore: update controller-runtime to v0.24.1
```

### Code Style

- Use structured logging (`logr`) exclusively; never `fmt.Printf` or `log.Printf`
- Follow controller-runtime patterns for reconciliation
- Use `resource.Quantity` for all CPU/memory values; never parse strings manually
- Add table-driven tests for new logic
- Use `meta.SetStatusCondition()` for condition management

## Code of Conduct

This project follows the [CNCF Code of Conduct](https://github.com/cncf/foundation/blob/main/code-of-conduct.md).
