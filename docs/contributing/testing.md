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
    enforces this threshold and fails if coverage drops below it. Typical
    totals on `main` are about 92% of statements in `./internal/...`.

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
AttunePolicy, injecting mock metrics, and asserting that status is
updated correctly.

`test/integration/api_fault_test.go` adds scripted API faults via a
controller-runtime client interceptor (timeout after a committed status
write, two status 409s then success, a manager restart mid-reconcile,
timeout after a committed pod annotation persist, and two pod-Update
409s then success). Those cases use a dedicated envtest control plane
so they do not race the shared TestMain manager.

Envtest has no kubelet and no cAdvisor. It cannot exercise in-place
`pods/resize`, MemoryPressure, or Prometheus scrape. Those stay in
`test/e2e` and `test/e2e-go`. The interceptor tests cover status
writes and annotation persist retry only. The persist cases call
`persistResizeAnnotations` through an integration-tag helper; they
do not go through `UpdateResize`. After a committed persist plus a
client timeout, persist confirms the tracking annotations with a
Clientset Get and treats the write as success so `resizeContainer`
does not revert.

## E2E tests (Chainsaw)

End-to-end tests run against a real Kubernetes cluster using
[Chainsaw](https://kyverno.github.io/chainsaw/). They deploy actual
Deployments and AttunePolicy resources and verify the operator
behaves correctly.

### Prerequisites

- Docker
- `k3d 5.8+ / Kind 0.24+` (see [k3d installation](https://k3d.io/#installation) and [Kind installation](https://kind.sigs.k8s.io/docs/user/quick-start/#installation))
- Chainsaw (auto-installed by the Makefile)

### Running E2E tests from scratch

```bash
# Recommended: k3d, because CI and nightly workflows run on k3d/K3S
make k3d-create
make k3d-deploy IMG=attune:e2e
make test-e2e
make test-e2e-go
make k3d-delete

# Alternative: Kind (supported, but local-only and not the default CI path)
make kind-create
make kind-deploy IMG=attune:e2e
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
| `test/e2e/opt-out/` | (cross-cutting) | `attune.io/skip` annotation is respected |
| `test/e2e/exclude-containers/` | (cross-cutting) | `excludedContainers` skips sidecars |
| `test/e2e/multi-selector/` | (cross-cutting) | Label selector matches multiple deployments |
| `test/e2e/eviction-fallback/` | (cross-cutting) | InPlaceOrRecreate is accepted and still resizes workloads (in-place path) |
| `test/e2e/schedule-window/` | (cross-cutting) | Schedule windows block resizes outside the allowed time |
| `test/e2e/budget-caps/` | (cross-cutting) | Budget caps are accepted and the policy still resizes workloads |
| `test/e2e/concurrent-resize/` | (cross-cutting) | `maxConcurrentResizes` is accepted and workloads still resize |
| `test/e2e/namespace-defaults/` | (cross-cutting) | AttuneNamespaceDefaults overrides cluster defaults |
| `test/e2e/defaults-merge/` | (cross-cutting) | AttuneDefaults values are inherited by a policy that omits them |
| `test/e2e/hpa-conflict/` | (cross-cutting) | HPA conflict is warning-only, policy still reconciles |
| `test/e2e/vpa-conflict/` | (cross-cutting) | VPA conflict is warning-only, policy still reconciles |
| `test/e2e/hpa-auto-tune/` | (cross-cutting) | Auto-tunes HPA CPU target utilization when annotated |
| `test/e2e/policy-weight/` | (cross-cutting) | Higher-weight policy outranks lower-weight on the same workload |
| `test/e2e/requests-only/` | (cross-cutting) | `controlledValues: RequestsOnly` is accepted and discovers workloads |
| `test/e2e/query-parameters/` | (cross-cutting) | Prometheus query parameters are accepted without breaking queries |
| `test/e2e/startup-boost/` | (cross-cutting) | CPU startup boost is applied to new pods |
| `test/e2e/configmap-export/` | (cross-cutting) | Recommendations are exported to a ConfigMap (`schema-version` + export-schema label) |
| `test/e2e/fleet-report/` | (infra) | Fleet report ConfigMap is written when `fleetReport` is enabled in E2E Helm |
| `test/e2e/gitops-pr-dry-run/` | (cross-cutting) | GitOps PR dry-run sets `PullRequestDryRun`/`PullRequestUnchanged`/`PullRequestCooldown` without forge credentials |

GitOps PR unit tests must cover the **second cycle** after merge, not only
the first open. `TestReconcileGitOpsPullRequest_LivePathDoesNotRecreateAfterMerge`
injects a fake `PullRequestClient` and asserts `CreateOrUpdate` is not
called again when the drift table is unchanged after the head branch is
gone. `TestGitHubClient_Create_AgainAfterHeadDeleted` documents that the
GitHub client *will* bootstrap another empty PR if the reconciler asks.
| `test/e2e/runtime-profile-defaults/` | (webhook + API) | `runtimeProfile: java` stored and accepted; java+allowDecrease warns at admission |
| `test/e2e/runtime-profile-java-no-mem-decrease/` | Recommend | java profile applies memory overhead=40 in explanation; CR overhead/allowDecrease stay unset |
| `test/e2e/resize-blocked-status/` | Recommend | Injected Deferred+Infeasible pod conditions surface `workloads.deferred`/`infeasible` and `ResizeBlocked` |
| `test/e2e/prometheus-unreachable/` | (cross-cutting) | Handles unreachable Prometheus gracefully without crashing |
| `test/e2e/grafana-dashboard/` | (helm) | Dashboard ConfigMap renders with `grafanaDashboard.enabled` |
| `test/e2e/health-probes/` | (infra) | Liveness and readiness probes pass |
| `test/e2e/metrics-endpoint/` | (infra) | Prometheus metrics endpoint is exposed |
| `test/e2e/webhook-defaulting/` | (webhook) | Mutating webhook applies defaults |
| `test/e2e/webhook-validation/` | (webhook) | Rejects invalid overhead and negative cooldown |
| `test/e2e/webhook-schedule-validation/` | (webhook) | Rejects invalid timezone, day, and window time |
| `test/e2e/defaults-validation/` | (webhook) | Rejects invalid AttuneDefaults |

### Paths covered primarily by unit tests (not full-cluster e2e)

Some safety and capacity paths are **intentionally unit-tested** rather
than full Chainsaw/Go e2e, because a faithful cluster simulation is
flake-prone or environment-specific:

| Path | Why unit tests are the primary gate | Where tests live |
|------|-------------------------------------|------------------|
| **Memory limit usage floor** | Needs controllable “recent usage” (recommendation raw percentile) while decreasing limits; real cgroup usage on pause pods is near zero and does not exercise the floor. | `internal/resize/usage_floor_test.go`, `internal/controller/memory_usage_floor_test.go` |
| **Version-aware memory limit decrease (clamp vs apply)** | Covered in **Go E2E** `TestE2E_MemoryLimitDecrease_VersionAware` (nightly 1.32–1.35 matrix): Guaranteed pod with oversized limit, `RequestsAndLimits` + `allowDecrease`; asserts limit drops on 1.35+ and stays clamped on 1.33–1.34. Unit tests still own clamp math and version parsing. | `test/e2e-go/e2e_test.go`, `internal/resize/engine_test.go` |
| **Node pressure / capacity skip** | Unit tests own reason strings, metric labels, **resize-path** event/metric emission, Clientset vs stale cache preference, and **live re-check** after a stale per-cycle nodeCache. **Go E2E** `TestE2E_NodeMemoryPressure_SkipsMemoryIncrease` patches `MemoryPressure` on the pod’s node (not `t.Parallel`; restores on cleanup; 100ms re-inject + sample log). Hard-fails only when samples show pressure **solidly True** across a window around a memory increase; kubelet-clear races recreate pods and continue (issue #481). Real allocatable exhaustion is also covered by a unit `executeResizes` path test. | `internal/controller/resize_pressure_test.go`, `capacity_skip_test.go`, `test/e2e-go/` |
| **Deferred / Infeasible UX** | Real kubelet Deferred/Infeasible needs capacity races (flake-prone). Cluster tests **inject** `PodResizePending` status (Chainsaw + Go E2E) and assert `workloads.deferred`/`infeasible` + `ResizeBlocked`. Helpers stay unit-tested. | `test/e2e/resize-blocked-status/`, `TestE2E_ResizeBlocked_*`, `internal/resize/engine_test.go`, `conditions_test.go` |
| **GitOps PR live HTTP** | Must not call GitHub/GitLab from CI. Client create/update paths use fake HTTP. Cluster e2e covers **dry-run** and missing-secret only (`test/e2e/gitops-pr-dry-run/`). | `internal/gitops/*_test.go`, `internal/controller/gitops_pr_test.go` |
| **Runtime profile in-memory defaults** | Unit: `ApplyRuntimeProfileDefaults`. Chainsaw admission + stored field: `runtime-profile-defaults`. Java-unique overhead=40 in explanation: Chainsaw + Go `TestE2E_RuntimeProfileJava_BlocksMemoryDecrease`. | `pkg/defaults/defaults_test.go`, `test/e2e/runtime-profile-*`, `test/e2e-go/` |

When adding behavior in these areas, extend the unit tables first. Prefer
deterministic injection (pod/node status patches) over waiting for real
resource pressure or external SaaS tokens.

### Chainsaw vs Go E2E (when to use which)

Both run against a real cluster in CI/nightly. Prefer:

| Prefer | Strengths | Weak spots |
|--------|-----------|------------|
| **Chainsaw** (`test/e2e/`) | Declarative multi-resource apply, webhook dry-run, pure kubectl/script flows, easy to read in PRs, no compile step | `/bin/sh` (dash) only; weak typed waits; yamllint max 200 cols; awkward for concurrent client-go loops |
| **Go E2E** (`test/e2e-go/`) | Typed clients, `retry.RetryOnConflict`, status subresource loops, `t.Parallel`, shared helpers, version gates | Heavier to write; must use `t.Parallel()` only when tests do not mutate cluster-scoped state (nodes) |

**Neither** can make the real kubelet emit Deferred/Infeasible without capacity races; inject status and assert operator UX instead. **Node** conditions (MemoryPressure) are injectable but **cluster-scoped**: keep those Go tests non-parallel and always restore.

### Writing new E2E tests

Create a directory under `test/e2e/<scenario-name>/` with a
`chainsaw-test.yaml` file. Follow the existing pattern: create a
namespace, deploy a workload, create a policy, assert on status.

Chainsaw configuration is in `.chainsaw.yaml` (timeouts, parallelism).

!!! warning
    E2E tests modify cluster state. Always run them against a disposable
    local cluster (k3d or Kind), not a shared environment.

## Fuzz testing

Fuzz tests exercise the recommendation engine and webhook validation with
random inputs to catch panics and edge cases:

```bash
make test-fuzz
# faster local loop:
FUZZTIME=5s make test-fuzz
```

`make test-fuzz` runs `scripts/run-fuzz.sh`, which executes each
coverage-guided target for 30 seconds by default (`FUZZTIME`,
overridable). Targets:

- `FuzzPercentileEstimator` (`./internal/recommendation/...`)
- `FuzzRecommendationEngine` (`./internal/recommendation/...`)
- `FuzzValidateFloatFields` (`./internal/webhook/...`)

The runner continues across targets so one failure does not hide others,
and retries once only when the log is a pure Go fuzz deadline flake
(`context deadline exceeded` with no crash corpus or assert; see
[golang/go#75804](https://github.com/golang/go/issues/75804)). Real
failures (panic, `failing input written`, `t.Error`) are never retried.

Fuzz targets are defined in `internal/recommendation/fuzz_test.go`
(estimator and engine) and `internal/webhook/validation_test.go`
(float-field parsing via `strconv.ParseFloat`). Classifier unit tests:
`bash scripts/test_run_fuzz.sh` (also via `make python-test`).

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
| `internal/webhook/validation_test.go` (`FuzzValidateFloatFields`) | Fuzz | Go native fuzzing (`make test-fuzz`) |

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
