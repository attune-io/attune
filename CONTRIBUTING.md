# Contributing to kube-rightsize

Thank you for your interest in contributing! This document provides
guidelines and instructions for contributing.

## Development Setup

### Prerequisites

| Tool | Version | Install |
|------|---------|---------|
| Go | 1.26+ | [golang.org/dl](https://golang.org/dl/) |
| Docker | 24+ | [docs.docker.com](https://docs.docker.com/engine/install/) |
| kubectl | 1.33+ | [kubernetes.io](https://kubernetes.io/docs/tasks/tools/) |
| Helm | 3.16+ | [helm.sh](https://helm.sh/docs/intro/install/) |
| k3d | 5.8+ | [k3d.io](https://k3d.io/#installation) |

The Makefile auto-installs these Go tools on first use (to `$GOPATH/bin`):
golangci-lint, gotestsum, controller-gen, setup-envtest, chainsaw, kustomize, helm-docs.

### Local Development

```bash
# Clone the repo
git clone https://github.com/SebTardifLabs/kube-rightsize.git
cd kube-rightsize

# Install Go dependencies
go mod download

# Build the operator and kubectl plugin
make build
make build-plugin

# Run all CI checks locally (lint, test, helm-docs, CRD freshness)
make verify
```

### Running Tests

```bash
# Unit tests (370+ tests, 75% coverage threshold enforced)
make test

# Integration tests (uses envtest, no cluster needed)
make test-integration

# E2E tests (requires k3d cluster with operator deployed)
make k3d-create                           # create k3d cluster (K8s 1.33)
make k3d-deploy IMG=kube-rightsize:e2e    # build, load, install cert-manager + Prometheus + operator
make test-e2e                             # run Chainsaw E2E scenarios

# All tests in sequence
make test-all

# Clean up the k3d cluster when done
make k3d-delete
```

**Important:** `make k3d-deploy` mutates `config/manager/kustomization.yaml`.
Before committing, always restore it:
```bash
git checkout config/manager/kustomization.yaml
```

### Building the Container Image

```bash
make docker-build IMG=kube-rightsize:dev
```

### Pre-commit Checklist

Run `make verify` before every commit. It covers:
- golangci-lint (code quality + import alias enforcement)
- Unit tests with coverage threshold (75%)
- Helm chart docs freshness
- Helm chart unit tests (44 tests)
- CRD manifest freshness (`make manifests` output matches committed files)

If you changed CRD types (`api/v1alpha1/`), also run:
```bash
make manifests   # regenerate CRDs and RBAC
make generate    # regenerate deepcopy methods
```
Commit the generated output.

## Pull Request Process

1. Fork the repository and create a branch from `main`
2. Make your changes with tests
3. Run `make verify` to run all CI checks locally
4. Submit a pull request

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
