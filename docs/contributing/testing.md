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
- `k3d 5.8+ / Kind 0.24+` (see [k3d installation](https://k3d.io/#installation) and [Kind installation](https://kind.sigs.k8s.io/docs/user/quick-start/#installation))
- Chainsaw (auto-installed by the Makefile)

### Running E2E tests from scratch

```bash
# Recommended: k3d, because CI and nightly workflows run on k3d/K3S
make k3d-create
make k3d-deploy IMG=kube-rightsize:e2e
make test-e2e
make test-e2e-go
make k3d-delete

# Alternative: Kind (supported, but local-only and not the default CI path)
make kind-create
make kind-deploy IMG=kube-rightsize:e2e
make test-e2e
make test-e2e-go
make kind-delete
```

Before running the E2E suites, verify that your current kubeconfig context points at the cluster you just created and that the API server is reachable:

```bash
kubectl config current-context
kubectl cluster-info
```

If `kubectl cluster-info` fails or still points at an old context, switch contexts before running `make test-e2e` or `make test-e2e-go`.

### Fast smoke check

Use this when you want to verify that the local end-to-end flow basically works
without running the full E2E suites:

```bash
make test-local-smoke
```

This target provisions a disposable k3d cluster, deploys cert-manager,
Prometheus, and the operator, then runs:
- `test/e2e/oneshot-resize` in Chainsaw
- `TestE2E_OneShotMode_ResizesOnePod` in `test/e2e-go/`

For a pre-provisioned cluster, the equivalent minimal smoke suite is:

```bash
make test-e2e-smoke
```

### Test scenarios

| Directory | Mode | What it verifies |
|-----------|-------------|------------------|
| `test/e2e/recommend-mode/` | Recommend | Discovers workloads, reaches InsufficientData |
| `test/e2e/observe-mode/` | Observe | Reaches InsufficientData without resizing pods |
| `test/e2e/oneshot-resize/` | OneShot | Discovers a workload and performs a one-shot resize |
| `test/e2e/canary-rollout/` | Canary | Performs a canary resize on a rollout-managed deployment |
| `test/e2e/auto-mode/` | Auto | Discovers workloads and performs automatic resizes |
| `test/e2e/bootstrap-progress/` | Recommend | Reports InsufficientData progress and ETA while metrics bootstrap |
| `test/e2e/statefulset-target/` | StatefulSet | Discovers a StatefulSet workload |
| `test/e2e/daemonset-target/` | DaemonSet | Discovers a DaemonSet workload |
| `test/e2e/cronjob-target/` | CronJob | Discovers a CronJob workload (recommend-only) |
| `test/e2e/job-target/` | Job | Discovers a standalone Job workload (recommend-only) |
| `test/e2e/opt-out/` | (cross-cutting) | `rightsize.io/skip` annotation is respected |
| `test/e2e/exclude-containers/` | (cross-cutting) | `excludedContainers` skips sidecars |
| `test/e2e/multi-selector/` | (cross-cutting) | Label selector matches multiple deployments |
| `test/e2e/eviction-fallback/` | (cross-cutting) | InPlaceOrRecreate is accepted and still resizes workloads (in-place path) |
| `test/e2e/schedule-window/` | (cross-cutting) | Schedule windows block resizes outside the allowed time |
| `test/e2e/budget-caps/` | (cross-cutting) | Budget caps are accepted and the policy still resizes workloads |
| `test/e2e/concurrent-resize/` | (cross-cutting) | `maxConcurrentResizes` is accepted and workloads still resize |
| `test/e2e/namespace-defaults/` | (cross-cutting) | RightSizeNamespaceDefaults overrides cluster defaults |
| `test/e2e/defaults-merge/` | (cross-cutting) | RightSizeDefaults values are inherited by a policy that omits them |
| `test/e2e/hpa-conflict/` | (cross-cutting) | HPA conflict is warning-only, policy still reconciles |
| `test/e2e/vpa-conflict/` | (cross-cutting) | VPA conflict is warning-only, policy still reconciles |
| `test/e2e/hpa-auto-tune/` | (cross-cutting) | Auto-tunes HPA CPU target utilization when annotated |
| `test/e2e/policy-weight/` | (cross-cutting) | Higher-weight policy outranks lower-weight on the same workload |
| `test/e2e/requests-only/` | (cross-cutting) | `controlledValues: RequestsOnly` is accepted and discovers workloads |
| `test/e2e/query-parameters/` | (cross-cutting) | Prometheus query parameters are accepted without breaking queries |
| `test/e2e/startup-boost/` | (cross-cutting) | CPU startup boost is applied to new pods |
| `test/e2e/configmap-export/` | (cross-cutting) | Recommendations are exported to a ConfigMap |
| `test/e2e/prometheus-unreachable/` | (cross-cutting) | Handles unreachable Prometheus gracefully without crashing |
| `test/e2e/grafana-dashboard/` | (helm) | Dashboard ConfigMap renders with `grafanaDashboard.enabled` |
| `test/e2e/health-probes/` | (infra) | Liveness and readiness probes pass |
| `test/e2e/metrics-endpoint/` | (infra) | Prometheus metrics endpoint is exposed |
| `test/e2e/webhook-defaulting/` | (webhook) | Mutating webhook applies defaults |
| `test/e2e/webhook-validation/` | (webhook) | Rejects invalid overhead and negative cooldown |
| `test/e2e/webhook-schedule-validation/` | (webhook) | Rejects invalid timezone, day, and window time |
| `test/e2e/defaults-validation/` | (webhook) | Rejects invalid RightSizeDefaults |

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

This runs each fuzz target with a fixed execution budget, which keeps the
CI run deterministic instead of relying on a wall-clock fuzz deadline:

```bash
go test ./internal/recommendation/... -run='^$' -fuzz=FuzzPercentileEstimator -fuzztime=5000000x
go test ./internal/recommendation/... -run='^$' -fuzz=FuzzRecommendationEngine -fuzztime=5000000x
```

Fuzz targets are defined in `internal/recommendation/fuzz_test.go`.

## Running all tests

Run everything in one command:

```bash
make test-all         # all tiers against a pre-provisioned cluster with operator + Prometheus
make test-local       # provisions k3d, deploys the stack, then runs all tiers
make test-local-smoke # provisions k3d, deploys the stack, then runs the smoke suite only
```

Or run each tier separately:

```bash
make test              # unit tests only
make test-integration  # integration tests (envtest)
make test-e2e          # Chainsaw E2E (requires local k3d or Kind cluster)
make test-e2e-go       # Go E2E (requires local k3d or Kind cluster with Prometheus)
make test-e2e-smoke    # one Chainsaw scenario + one Go E2E smoke test
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
| `internal/**/*_benchmark_test.go` | Benchmark | Go testing (`make test-bench`) |
| `test/integration/` | Integration | envtest |
| `test/e2e/` | E2E (Chainsaw) | Chainsaw (`make test-e2e`) |
| `test/e2e-go/` | E2E (Go) | Go testing + real cluster (`make test-e2e-go`) |
| `internal/recommendation/fuzz_test.go` | Fuzz | Go native fuzzing (`make test-fuzz`) |

### Full Go E2E suite

`make test-e2e-go` now runs the full Go E2E suite, including the longer
Prometheus warm-up scenarios that cover budget caps, schedule windows,
bearer-token auth, eviction fallback, realistic overprovisioned
workloads, secret rotation, recommendation retention without live pods,
and OOM-triggered safety reverts.

Expect 5-10 minutes of total runtime for the Go E2E portion because these
scenarios wait for real Prometheus samples and operator reconciles.
The nightly workflow still runs the same suite across the full Kubernetes
version matrix.

## Writing new tests

- Place unit tests next to the code they test (`foo_test.go` alongside
  `foo.go`).
- Use `testify/assert` and `testify/require` for assertions.
- Use table-driven tests for functions with multiple input/output scenarios.
- Mock the `MetricsCollector` interface for tests that need Prometheus data.
