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
    The project requires 80%+ line coverage for `internal/` packages. CI
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
- k3d (`curl -s https://raw.githubusercontent.com/k3d-io/k3d/main/install.sh | bash`) **or** Kind (`go install sigs.k8s.io/kind@latest`)
- Chainsaw (auto-installed by the Makefile)

### Running E2E tests from scratch

```bash
# Option A: k3d (fast startup, uses k3s)
make k3d-create
make k3d-deploy IMG=kube-rightsize:e2e
make test-e2e
make k3d-delete

# Option B: Kind (upstream K8s, production-accurate)
make kind-create
make kind-deploy IMG=kube-rightsize:e2e
make test-e2e
make kind-delete
```

### Test scenarios

| Directory | Mode | What it verifies |
|-----------|-------------|------------------|
| `test/e2e/recommend-mode/` | Recommend | Discovers workloads, reaches InsufficientData |
| `test/e2e/observe-mode/` | Observe | Reaches condition, no resizes performed |
| `test/e2e/oneshot-resize/` | OneShot | Reaches InsufficientData (no Prometheus) |
| `test/e2e/canary-rollout/` | Canary | Canary config accepted, reaches InsufficientData |
| `test/e2e/auto-mode/` | Auto | Discovers workloads, reaches InsufficientData |
| `test/e2e/statefulset-target/` | StatefulSet | Discovers StatefulSet workload |
| `test/e2e/daemonset-target/` | DaemonSet | Discovers DaemonSet workload |
| `test/e2e/cronjob-target/` | CronJob | Discovers CronJob workload (recommend-only) |
| `test/e2e/opt-out/` | (cross-cutting) | `rightsize.io/skip` annotation respected |
| `test/e2e/eviction-fallback/` | (cross-cutting) | InPlaceOrEvict policy accepted, discovers workloads |
| `test/e2e/schedule-window/` | (cross-cutting) | Schedule windows, daysOfWeek, timezone accepted |
| `test/e2e/budget-caps/` | (cross-cutting) | maxTotalCpuIncrease/maxTotalMemoryIncrease accepted |
| `test/e2e/concurrent-resize/` | (cross-cutting) | maxConcurrentResizes accepted, discovers workloads |
| `test/e2e/namespace-defaults/` | (cross-cutting) | RightSizeNamespaceDefaults overrides cluster defaults |
| `test/e2e/webhook-schedule-validation/` | (webhook) | Rejects invalid timezone, day, window time |

### Writing new E2E tests

Create a directory under `test/e2e/<scenario-name>/` with a
`chainsaw-test.yaml` file. Follow the existing pattern: create a
namespace, deploy a workload, create a policy, assert on status.

Chainsaw configuration is in `.chainsaw.yaml` (timeouts, parallelism).

!!! warning
    E2E tests modify cluster state. Always run them against a disposable
    local cluster (k3d or Kind), not a shared environment.

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

Run everything in one command:

```bash
make test-all      # unit + integration + E2E (requires local cluster)
```

Or run each tier separately:

```bash
make test              # unit tests only
make test-integration  # integration tests (envtest)
make test-e2e          # E2E tests (requires local k3d or Kind cluster)
```

For a full local validation including lint, helm, and CRD freshness:

```bash
make verify        # all CI checks locally
```

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
