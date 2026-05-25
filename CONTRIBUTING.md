# Contributing to attune

Thank you for your interest in contributing! This document provides
guidelines and instructions for contributing.

## Development Setup

### Prerequisites

| Tool | Version | Install |
|------|---------|---------|
| Go | 1.26+ | [golang.org/dl](https://golang.org/dl/) |
| Docker | 24+ | [docs.docker.com](https://docs.docker.com/engine/install/) |
| kubectl | 1.32+ | [kubernetes.io](https://kubernetes.io/docs/tasks/tools/) |
| Helm | 3.16+ or 4.x | [helm.sh](https://helm.sh/docs/intro/install/) |
| k3d **or** Kind | k3d 5.8+ / Kind 0.24+ | [k3d.io](https://k3d.io/#installation) or [kind.sigs.k8s.io](https://kind.sigs.k8s.io/docs/user/quick-start/#installation) |
| Python 3 + pip | 3.8+ | For `yamllint` (YAML linting in `make verify`) |

The Makefile auto-installs these Go tools on first use (to `$GOPATH/bin`):
golangci-lint, gotestsum, controller-gen, setup-envtest, chainsaw, kustomize, helm-docs.
It also installs the Helm unittest plugin automatically when needed for
`make helm-unittest` or `make verify`.

`yamllint` (Python) is auto-installed via pip if missing when running `make yaml-lint`.

### Local Development

```bash
# Clone the repo
git clone https://github.com/attune-io/attune.git
cd attune

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
# Unit tests (1000+ tests, 80% coverage threshold enforced, currently >90%)
make test

# Integration tests (uses envtest, no cluster needed)
make test-integration

# E2E tests (requires a local cluster with operator deployed)
# Recommended: k3d, because CI and nightly workflows run on k3d/K3S
make k3d-create                           # create k3d cluster
make k3d-deploy IMG=attune:e2e    # build, load, deploy
make test-e2e                             # run Chainsaw E2E scenarios
make test-e2e-go                          # run full Go E2E suite
make k3d-delete                           # clean up

# Alternative: Kind (supported, but local-only and not the default CI path)
make kind-create                          # create Kind cluster
make kind-deploy IMG=attune:e2e   # build, load, deploy
make test-e2e                             # run Chainsaw E2E scenarios
make test-e2e-go                          # run full Go E2E suite
make kind-delete                          # clean up

# All tests in sequence (unit + integration + Chainsaw + Go E2E)
# NOTE: E2E requires a cluster with the operator deployed (see above).
# Unit and integration tests run without any cluster.
make test-all

# Single command: auto-provisions k3d, deploys, runs unit + integration +
# Chainsaw E2E + full Go E2E suite, then cleans up
make test-local

# Fast end-to-end smoke check: auto-provisions k3d, deploys, runs one
# Chainsaw scenario + one Go E2E test, then cleans up
make test-local-smoke
```

`make test-e2e`, `make test-e2e-go`, and `make test-e2e-smoke` work with
an already deployed k3d or Kind cluster.

`make test-local` includes the full Go E2E suite. Expect longer runtime than
`make test-local-smoke`, because the longer Prometheus warm-up scenarios now run
in the standard `make test-e2e-go` target and regular CI.

**Important:** `make k3d-deploy` and `make kind-deploy` mutate
`config/manager/kustomization.yaml`. Before committing, always restore it:
```bash
git checkout config/manager/kustomization.yaml
```

### Building the Container Image

```bash
make docker-build IMG=attune:dev
```

### Pre-commit Checklist

Run `make verify` before every commit. It covers:
- golangci-lint (code quality + import alias enforcement)
- Unit and integration tests with coverage threshold (80%)
- Helm lint + template validation
- Helm chart docs freshness and unit tests
- CRD manifest freshness (`make manifests` output matches committed files)
- Grafana dashboard sync (`deploy/grafana/dashboard.json` source and generated Helm dashboard stay aligned)
- Documentation defaults consistency check
- govulncheck for known vulnerabilities

For faster feedback on docs-only or YAML-only changes, use `make verify-quick`
(skips integration tests and govulncheck).

If you changed CRD types (`api/v1alpha1/`), also run:
```bash
make manifests   # regenerate CRDs and RBAC
make generate    # regenerate deepcopy methods
```
Commit the generated output.

## Documentation Site

The `docs/` directory is configured as an [MkDocs](https://www.mkdocs.org/)
site with the [Material](https://squidfun.github.io/mkdocs-material/) theme.

### Local preview

```bash
pip install mkdocs-material
mkdocs serve
```

Then open `http://127.0.0.1:8000`. Changes to markdown files reload
automatically.

### Editing docs

- Every markdown file under `docs/` must start with a `# Title` heading.
- Navigation order is controlled by `mkdocs.yml` (the `nav:` key).
- Admonitions (`!!! note`, `!!! tip`, `!!! warning`) are supported.

## Helm CI parity notes

If you are reproducing CI failures locally, prefer `make verify` over hand-built
Helm commands. The CI workflow includes a couple of details that are easy to
miss when replaying the Helm jobs manually:

- template validation simulates cert-manager CRDs with
  `--api-versions cert-manager.io/v1`
- the Helm unittest plugin is installed from the exact release asset filename,
  which may not match the tag string one-to-one

If a local manual replay disagrees with CI, check the exact commands in
[`.github/workflows/ci.yaml`](.github/workflows/ci.yaml) before assuming the
chart or workflow is wrong.

## CI Runners

CI runs on GitHub-hosted `ubuntu-latest` runners by default. To switch to
self-hosted runners, set the repository variable `RUNNER` to `self-hosted`
in Settings > Secrets and variables > Actions > Variables.

To check queued or in-progress CI runs:

```bash
make ci-runner-status
```

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
