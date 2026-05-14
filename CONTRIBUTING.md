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
| Helm | 3.16+ or 4.x | [helm.sh](https://helm.sh/docs/intro/install/) |
| k3d **or** Kind | k3d 5.8+ / Kind 0.24+ | [k3d.io](https://k3d.io/#installation) or `go install sigs.k8s.io/kind@latest` |
| Python 3 + pip | 3.8+ | For `yamllint` (YAML linting in `make verify`) |

The Makefile auto-installs these Go tools on first use (to `$GOPATH/bin`):
golangci-lint, gotestsum, controller-gen, setup-envtest, chainsaw, kustomize, helm-docs.

`yamllint` (Python) is auto-installed via pip if missing when running `make yaml-lint`.

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
# Unit tests (502+ tests, 80% coverage threshold enforced)
make test

# Integration tests (uses envtest, no cluster needed)
make test-integration

# E2E tests (requires a local cluster with operator deployed)
# Option A: k3d (fast startup, uses k3s)
make k3d-create                           # create k3d cluster
make k3d-deploy IMG=kube-rightsize:e2e    # build, load, deploy
make test-e2e                             # run Chainsaw E2E scenarios
make k3d-delete                           # clean up

# Option B: Kind (upstream K8s, production-accurate)
make kind-create                          # create Kind cluster
make kind-deploy IMG=kube-rightsize:e2e   # build, load, deploy
make test-e2e                             # run Chainsaw E2E scenarios
make kind-delete                          # clean up

# All tests in sequence (unit + integration + E2E)
# NOTE: E2E requires a cluster with the operator deployed (see above).
# Unit and integration tests run without any cluster.
make test-all

# Single command: auto-provisions k3d cluster, deploys, runs everything, cleans up
make test-local
```

**Important:** `make k3d-deploy` and `make kind-deploy` mutate
`config/manager/kustomization.yaml`. Before committing, always restore it:
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
- Unit and integration tests with coverage threshold (80%)
- Helm lint + template validation
- Helm chart docs freshness and unit tests
- CRD manifest freshness (`make manifests` output matches committed files)
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

CI runs on self-hosted runners (3 org-level runners in `SebTardifLabs`).
Runners are background processes that must be restarted after a machine reboot:

```bash
for i in 1 2 3; do
  cd ~/actions-runner-pool/runner-$i && nohup ./run.sh &
done
```

Check runner health:
```bash
gh api orgs/SebTardifLabs/actions/runners \
  --jq '.runners[] | "\(.name): \(.status)"'
```

If runners are offline, CI jobs will queue indefinitely.

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
