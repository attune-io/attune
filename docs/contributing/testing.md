## Unit tests

Run all unit tests with race detection and coverage:

```bash
make test
```

This uses `gotestsum` with auto-retry for flaky tests:

```bash
gotestsum --format pkgname \
  --rerun-fails --rerun-fails-max-failures=5 \
  --packages="./api/... ./cmd/... ./internal/..." \
  -- -race -timeout=10m \
  -coverpkg=./internal/... \
  -coverprofile=coverage.out \
  -covermode=atomic
```

View the coverage report:

```bash
go tool cover -html=coverage.out
```

!!! note "Coverage requirements"
    The project requires 75%+ line coverage for `internal/` packages. CI
    enforces this threshold and fails if coverage drops below it.

## Integration tests (envtest)

Integration tests use controller-runtime's `envtest` to run a real API server
and etcd locally without a full cluster:

```bash
make test-integration
```

This installs the `setup-envtest` tool if needed, downloads the Kubernetes
binaries, and runs:

```bash
KUBEBUILDER_ASSETS="$(setup-envtest use -p path)" \
  go test ./test/integration/... -race -count=1 -timeout=15m -tags=integration
```

Integration tests verify the full reconciliation loop: creating a
RightSizePolicy, injecting mock metrics, and asserting that status is
updated correctly.

## E2E tests (Chainsaw)

End-to-end tests run against a real Kubernetes cluster using
[Chainsaw](https://kyverno.github.io/chainsaw/). They deploy actual
Deployments and RightSizePolicy resources and verify the operator
behaves correctly.

### Prerequisites

- Docker
- Kind (`go install sigs.k8s.io/kind@latest`)
- Chainsaw (`go install github.com/kyverno/chainsaw@v0.2.15`)

### Running E2E tests from scratch

```bash
# 1. Create a Kind cluster
make kind-create

# 2. Build the operator image, load into Kind, install CRDs, deploy
make kind-deploy IMG=kube-rightsize:e2e

# 3. Wait for the operator to be ready
kubectl wait --for=condition=Available deployment/kube-rightsize-controller-manager \
  -n kube-rightsize-system --timeout=120s

# 4. Run all E2E tests
make test-e2e

# 5. Clean up
make kind-delete
```

### Test scenarios

| Directory | Mode | What it verifies |
|-----------|------|------------------|
| `test/e2e/recommend-mode/` | Recommend | Discovers workloads, reaches InsufficientData |
| `test/e2e/observe-mode/` | Observe | Reaches condition, no resizes performed |
| `test/e2e/oneshot-resize/` | OneShot | Reaches InsufficientData (no Prometheus) |
| `test/e2e/canary-rollout/` | Canary | Canary config accepted, reaches InsufficientData |
| `test/e2e/auto-mode/` | Auto | Discovers workloads, reaches InsufficientData |
| `test/e2e/opt-out/` | (cross-cutting) | `rightsize.io/skip` annotation respected |

### Writing new E2E tests

Create a directory under `test/e2e/<scenario-name>/` with a
`chainsaw-test.yaml` file. Follow the existing pattern: create a
namespace, deploy a workload, create a policy, assert on status.

Chainsaw configuration is in `.chainsaw.yaml` (timeouts, parallelism).

!!! warning
    E2E tests modify cluster state. Always run them against a disposable
    Kind cluster, not a shared environment.

## Fuzz testing

Fuzz tests exercise the recommendation engine with random inputs to catch
panics and edge cases:

```bash
make test-fuzz
```

This runs each fuzz target for 30 seconds:

```bash
go test ./internal/recommendation/... -fuzz=. -fuzztime=30s
```

Fuzz targets are defined in `internal/recommendation/fuzz_test.go`.

## Running all tests

```bash
make test-all
```

This runs unit tests, integration tests, and E2E tests in sequence.

## Test organization

| Directory | Type | Framework |
|-----------|------|-----------|
| `api/v1alpha1/*_test.go` | Unit | Go testing |
| `internal/**/*_test.go` | Unit | Go testing + testify |
| `test/integration/` | Integration | envtest |
| `test/e2e/` | E2E | Chainsaw |
| `internal/recommendation/fuzz_test.go` | Fuzz | Go native fuzzing |

## Writing new tests

- Place unit tests next to the code they test (`foo_test.go` alongside
  `foo.go`).
- Use `testify/assert` and `testify/require` for assertions.
- Use table-driven tests for functions with multiple input/output scenarios.
- Mock the `MetricsCollector` interface for tests that need Prometheus data.
