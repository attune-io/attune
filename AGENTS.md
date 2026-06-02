# AGENTS.md

## Project

Attune: Kubernetes operator for in-place pod resource right-sizing (VPA replacement).
Requires Kubernetes 1.32+ (In-Place Pod Resize; 1.32 alpha with feature gate, 1.33+ beta enabled by default). Built with Go 1.26,
controller-runtime v0.24.1, Kubebuilder v4, K8s API v0.36.1.

**Naming convention:** "Attune" (capitalized) in prose and documentation.
`attune` (lowercase) in code, packages, namespaces, Prometheus metrics
(`attune_resize_total`), CLI commands (`kubectl attune`), API groups
(`attune.io`), Helm chart names, Docker images, and URLs.

## Commands

- Install deps: `go mod download`
- Build: `make build`
- Build plugin: `make build-plugin`
- Build image: `make docker-build IMG=attune:dev`
- Test (unit): `make test`
- Test (single pkg): `go test ./internal/resize/... -race -count=1`
- Test (integration): `make test-integration`
- Test (E2E Chainsaw): `NO_COLOR=1 make test-e2e` (requires k3d cluster; NO_COLOR prevents raw ANSI codes in agent output)
- Test (E2E Go): `make test-e2e-go` (requires k3d cluster with operator + Prometheus)
- Test (E2E smoke): `make test-e2e-smoke` (requires deployed k3d/Kind cluster with operator + Prometheus)
- Test (fuzz): `make test-fuzz`
- Test (bench): `make test-bench`
- Lint: `make lint`
- Lint + fix: `make lint-fix`
- Format: `make fmt`
- Generate CRDs/RBAC: `make manifests`
- Generate deepcopy: `make generate`
- Helm chart docs: `make helm-docs-gen`
- Helm chart tests: `make helm-unittest`
- Helm lint + template validation: `make helm-lint`
- Doc defaults consistency check: `make verify-doc-defaults`
- Fast pre-commit checks: `make verify-quick` (no integration tests or govulncheck)
- All CI checks locally: `make verify`
- Clean build artifacts: `make clean`
- Local cluster (k3d): `make k3d-create && make k3d-deploy IMG=attune:e2e`
- Local cluster (Kind): `make kind-create && make kind-deploy IMG=attune:e2e`
- Full local test (auto-provisions k3d): `make test-local`
- Local smoke test (auto-provisions k3d): `make test-local-smoke`
- E2E tests: `make test-e2e` (requires local cluster with operator deployed)

## Structure

- `api/v1alpha1/` - CRD type definitions (AttunePolicy, AttuneDefaults)
- `cmd/manager/` - Operator entry point
- `cmd/kubectl-attune/` - kubectl plugin
- `internal/controller/` - Reconciler (core business logic)
- `internal/metrics/` - Metrics collection (Prometheus, Datadog, CloudWatch), QueryBuilder interface, rate limiting
- `internal/recommendation/` - Composable estimator chain (percentile, margin, confidence, bounds, change filter)
- `internal/resize/` - In-place pod resize engine via /resize subresource
- `internal/safety/` - Post-resize safety observation and rollback
- `internal/conflict/` - HPA conflict detection
- `internal/webhook/` - Admission webhooks (defaulting + validation)
- `internal/operatormetrics/` - Operator-level Prometheus metrics (init-registered)
- `internal/validation/` - Shared validation (Prometheus address SSRF checks)
- `internal/throttle/` - Shared throttle checker interface (breaks import cycle)
- `internal/transform/` - Informer cache transform functions (strip unused pod fields to reduce memory)
- `pkg/defaults/` - Shared default-value and merge logic (used by controller + kubectl plugin)
- `config/` - Kustomize manifests (CRDs, RBAC, manager deployment)
- `charts/attune/` - Helm chart with cert-manager webhook support
- `test/integration/` - envtest-based integration tests
- `test/e2e/` - Chainsaw E2E test scenarios
- `docs/` - MkDocs documentation site

## Conventions

### Import aliases (enforced by golangci-lint importas)

Use these exact aliases; the linter rejects alternatives:

```go
corev1      "k8s.io/api/core/v1"
appsv1      "k8s.io/api/apps/v1"
metav1      "k8s.io/apimachinery/pkg/apis/meta/v1"
apierrors   "k8s.io/apimachinery/pkg/api/errors"
ctrl        "sigs.k8s.io/controller-runtime"
```

### Logging

Use `logr` structured logging exclusively. `fmt.Print` and `fmt.Fprint` are
forbidden by the linter (except in `cmd/kubectl-attune/`).

### resource.Quantity

Use `resource.ParseQuantity()` (returns error) instead of `resource.MustParse()`
(panics). Use DecimalSI format for CPU, BinarySI for memory. Use Go `time.Duration`
for all durations (e.g., `168h` not `7d`).

### Float parsing

`strconv.ParseFloat("NaN", 64)` and `strconv.ParseFloat("Inf", 64)` succeed
with nil error, returning `math.NaN()` and `math.Inf()`. All float comparisons
with NaN return false, silently disabling any threshold or guardrail that uses
the parsed value. Always check `math.IsNaN(v) || math.IsInf(v, 0)` after
`strconv.ParseFloat` in validation code.

The same guard applies at **runtime query boundaries**, not just parse-time.
Prometheus queries, API responses, and external computations can return NaN
(e.g., `0/0` in PromQL) or Inf without any string parsing involved. Any
`float64` received from an external system must be checked before comparison.
Example: `internal/safety/monitor.go` SLO query values (PR #167),
`internal/metrics/collector.go` `GetThrottleRatio` (line 349).

### Webhooks

controller-runtime v0.24.x uses typed generic interfaces. Register webhooks with:

```go
// AttunePolicy: defaulting + validation
ctrl.NewWebhookManagedBy(mgr, &attunev1alpha1.AttunePolicy{}).
    WithDefaulter(&webhook.AttunePolicyDefaulter{}).
    WithValidator(&webhook.AttunePolicyValidator{}).
    Complete()

// AttuneDefaults: validation only (costPricing fields)
ctrl.NewWebhookManagedBy(mgr, &attunev1alpha1.AttuneDefaults{}).
    WithValidator(&webhook.AttuneDefaultsValidator{}).
    Complete()
```

### CRD schema vs webhook validation

Kubebuilder markers like `+kubebuilder:validation:Minimum=1` generate
OpenAPI schema constraints in the CRD. These are enforced by the API
server at admission time, **before** webhooks run. A zero value that
violates a CRD-level `minimum` is rejected even if the webhook would
accept it. When writing tests that create CRs, always respect CRD-level
constraints; webhook-level logic cannot override them.

### Pod resize

The `/resize` subresource is not available via the controller-runtime client.
Use a typed `kubernetes.Clientset` and call `UpdateResize()`:

```go
clientset.CoreV1().Pods(ns).UpdateResize(ctx, name, pod, metav1.UpdateOptions{})
```

Wrap `UpdateResize` in `retry.RetryOnConflict` (kubelet and concurrent
container resizes bump `resourceVersion`).
See `internal/resize/engine.go` `ResizePod()`.

**K8s v1.33 memory limit restriction:** Kubernetes v1.33 forbids decreasing a
container's memory limit in-place when the resize policy is `NotRequired`.
The operator handles this via `ClampMemoryLimitForPolicy` in
`internal/resize/engine.go`. K8s v1.34+ relaxed this restriction.

### Code generation

Run `make manifests` after changing CRD types or RBAC markers. Run
`make generate` after changing API types. Commit the generated output.

### Reconciler construction (contributor note)

`AttunePolicyReconciler` has required internal state (e.g. `eventDedup`).
Always create instances via `NewAttunePolicyReconciler()` (then set exported
fields like `Client`, `Scheme`, `MetricsFactory` as needed). Direct struct
literals bypass initialization and are no longer used in the test suite
(tracked in #141). This applies to any new tests or extensions.

### RBAC markers and the controller-runtime cache

Any resource accessed via the controller-runtime client (`r.Get()`, `r.List()`,
`r.Update()`) goes through the informer cache by default. The cache needs
`list` and `watch` RBAC to start its reflector. If you add a new `r.Get()`
call for a resource type, check:

1. Does the RBAC marker include `list` and `watch`? If not, add them.
2. Is the resource in `DisableFor` (`cmd/manager/main.go`)? If yes,
   it bypasses the cache and only needs `get`.

**When changing a client call's verb** (e.g., `r.Update()` to `r.Patch()`,
or `r.Get()` to `r.List()`), the RBAC marker must also be updated. The
code compiles without the RBAC change; the failure only appears at runtime
as a "forbidden" error, which controller-runtime retries with exponential
backoff. This silently burns through timeouts and is hard to diagnose
without reading operator logs.

After changing RBAC markers, update **three places**:
- The kubebuilder marker in `internal/controller/`
- `config/rbac/role.yaml` (run `make manifests`)
- `charts/attune/templates/clusterrole.yaml` + its test

**Kubebuilder RBAC markers must be in `internal/controller/`.** `controller-gen`
only scans packages specified in its invocation (typically
`internal/controller/...`). Placing a `+kubebuilder:rbac` marker in a utility
package (e.g., `internal/metrics/`, `internal/safety/`) has no effect; the marker
is silently ignored because controller-gen never reads that package. If a utility
function accesses a new API resource, add the RBAC marker to
`internal/controller/attunepolicy_controller.go` (where all other RBAC markers
live), not to the file that contains the function.

Learned from PR #269/PR #270 where `DetectClusterTLSProfile()` in
`internal/metrics/tlsprofile.go` reads `apiservers.config.openshift.io/v1`, but
the RBAC marker was initially placed in `internal/metrics/tlsprofile.go`. Running
`make manifests` produced no change because controller-gen never scanned that
package. Moving the marker to `internal/controller/attunepolicy_controller.go`
fixed it.

Currently, Secrets are the only resource in `DisableFor` (get-only is safe).
All other resources accessed via the client need `list`/`watch`.

### Adding a new defaultable field

Fields that should be overridable by `AttuneDefaults` must use
pointer types (`*int32`, `*bool`, `*metav1.Duration`) so nil=unset
is distinguishable from zero/false. Update all 7 locations:

1. `api/v1alpha1/attunepolicy_types.go` - Add `*T` field with
   `json:"name,omitempty"` and `// +optional`
2. `api/v1alpha1/defaults.go` - Add `DefaultXxx` constant
3. `pkg/defaults/defaults.go` `ApplyBuiltInDefaults()` - Add
   nil check + default assignment
4. `pkg/defaults/defaults.go` `MergeDefaults()` - Add merge
   clause (covers both controller and kubectl plugin)
5. `internal/webhook/validation.go` - Add validation if needed
6. Run `make manifests && make generate` to regenerate CRD + deepcopy
7. `cmd/kubectl-attune/main.go` `printEffectiveValues()` - Add
   display line so `kubectl attune explain` shows the field

If the field also belongs in `AttuneDefaults`, add it to
`api/v1alpha1/attunedefaults_types.go` as well.

### Adding a new Prometheus operator metric

When adding a new metric to `internal/operatormetrics/metrics.go`,
update all 5 locations:

1. `internal/operatormetrics/metrics.go` - Define the metric var and
   register it in `init()`
2. The emitting call site (e.g., `internal/controller/`) - Increment
   or observe the metric at the right code path
3. `docs/reference/metrics.md` - Add the metric definition with labels
   table and an example PromQL query
4. `docs/guides/troubleshooting.md` - If the metric surfaces a user-visible
   condition (NaN data, clamped requests), add a reference and PromQL
   alert example in the relevant troubleshooting section
5. `charts/attune/files/grafana-dashboard.json` - Add a panel (or update
   an existing row) so the metric is visible in Grafana
6. `charts/attune/templates/prometheusrule.yaml` + `values.yaml` - If the
   metric indicates a condition worth alerting on, add an opt-in
   PrometheusRule alert with configurable threshold

This checklist was added after PR #177 added `attune_request_clamped_total`
and `attune_nan_inf_samples_total` to the code but missed locations 3-6.
The doc gap was caught in the cycle 18 post-cycle gate; the dashboard and
alert gaps became issues #179 and #181.

### Helm values.yaml comments (helm-docs format)

`helm-docs` reads `# --` comments from `values.yaml` to generate README
parameter tables. Multi-line descriptions must use `# --` only on the
first line; continuation lines use `#` without `--`:

```yaml
# -- First line of the description.
# Continuation text on the second line.
# More continuation text.
someValue: "default"
```

Using `# --` on every line causes helm-docs to treat each line as a
separate parameter description, producing garbled output.

### Hard process rule: Never push directly to main

**All changes — no matter how small — must go through a feature branch and Pull Request.** Direct pushes to `main` (including `git push origin main`, force-pushes, or bypassing the merge queue) are forbidden, even for the repo owner.

This applies to code, CI/workflows, Makefile, scripts, and documentation.

See the "Never Push Directly to Main (HARD RULE)" section in `~/.grok/skills/attune-contrib/SKILL.md` for the full rationale and correct workflow.

### Pull request titles and commit messages

The repository enforces semantic PR titles via [.github/workflows/pr-title.yaml](.github/workflows/pr-title.yaml) (the `amannn/action-semantic-pull-request` action).

- Allowed types: `feat`, `fix`, `docs`, `ci`, `refactor`, `test`, `chore`, `perf`, `build`, `revert`.
- The subject (text after `type: `) **must start with a lowercase letter** (`subjectPattern: ^[a-z].+$`).
  - Good: `fix: e2e nightly RealisticLoad timeout + safe cache keys for secrets (no SHA256)`
  - Bad: `fix: E2E nightly ...` (capital E fails the regex and blocks the PR immediately).
- The check runs on PR open/edit/synchronize and validates the PR title (and frequently the head commit message).
- Dependabot PRs are automatically exempted by the workflow.
- Always commit with `-s` (`git commit -s` or `git commit --amend -s`) so DCO passes. Never leave unexpanded shell like `$(git config user.name)` in the `Signed-off-by` line.

When creating branches, commits, or PRs, make the first line a valid semantic title so the gate passes on the first attempt. This avoids immediate CI failures and repeated title edits.

### MkDocs documentation links

MkDocs strict mode rejects relative links that resolve outside the `docs/`
directory. When referencing files elsewhere in the repo (e.g., `charts/`,
`scripts/`), use absolute GitHub URLs instead of relative paths:

```markdown
<!-- BAD: relative path outside docs/ — MkDocs strict mode rejects this -->
[Helm README](../../charts/attune/README.md)

<!-- GOOD: absolute GitHub URL -->
[Helm README](https://github.com/attune-io/attune/tree/main/charts/attune#prometheusrule)
```

## Testing

- Framework: `testify` (assert/require)
- Write table-driven tests for all logic
- Coverage threshold: 80% on `./internal/...` (CI enforced)
- Generated files (`zz_generated.deepcopy.go`) are excluded from coverage
- CI uses `gotestsum` with `--rerun-fails` for flaky retry and JUnit XML reports
- Run with `-race` flag
- Use `kubefake.NewSimpleClientset()` to test resize operations
- Use `fake.NewClientBuilder()` for controller-runtime client mocking
- Integration tests use envtest (build tag: `integration`)
- E2E tests use Chainsaw v0.2.15 on k3d or Kind clusters (K8s 1.32, 1.33, 1.34, 1.35 matrix in CI)
- E2E tests that modify CRs mid-test must use a refetch/retry loop to handle
  optimistic concurrency conflicts (the operator reconciles the same object
  concurrently, causing `the object has been modified` errors on update)
- Go E2E tests (`test/e2e-go/`) must include `t.Parallel()` as the first line.
  Every test creates a unique namespace via `uniqueNS()`, so they are fully
  isolated. Without `t.Parallel()`, 13 tests run sequentially (~12 min);
  with it, they run concurrently (~2 min, bounded by OOMKill at 127s).
- E2E test policies must use `Cooldown: 1m` (the minimum) to avoid long requeue
  delays during data collection.
- E2E test pods should use Burstable QoS (requests only, no CPU/memory limits)
  unless testing QoS behavior specifically. Guaranteed QoS pods are harder to
  schedule when 13 parallel tests compete for ~4 allocatable CPUs on the k3d
  node. Keep CPU requests at or below 300m per test pod.
- E2E wait helpers (`waitForDeploymentReady`, `waitForResize`, etc.) must
  log diagnostic state on timeout (pod phase, container state, events).
  Silent timeouts make CI failures undiagnosable.
- Chainsaw assertions must target **stable** operator states, not transient
  ones. With `minimumDataPoints: 1`, the operator can transition from
  `InsufficientData` to `Monitoring` within seconds. A static assert on
  the transient state races with the reconcile loop and intermittently
  times out. Use script-based assertions that accept multiple valid states
  when the assertion target can change during the poll window.
- When an E2E test fails intermittently in the nightly K8s version matrix,
  check the failure pattern across multiple runs before blaming a specific
  version. If the failure rotates randomly across versions, the root cause
  is NOT version-specific; look for test setup differences instead.

## CI

- Runs on **GitHub-hosted runners** (`ubuntu-latest`) by default
- Fuzz tests: 30s time-based per target (coverage-guided, not iteration count)
- E2E Nightly runs the full K8s version matrix (1.32, 1.33, 1.34, 1.35)
  in parallel (max-parallel: 4); each version creates a fresh k3d cluster
- Concurrency groups use `cancel-in-progress: false` on main; PRs targeting
  main will not cancel in-flight CI runs

## Safety

- Never commit secrets, API keys, or `.env` files (gitleaks runs in CI)
- Run `make manifests && make generate` before committing CRD/API changes
- Run `make verify` before committing (covers lint, test, helm-docs, CRD freshness)
- After running `make deploy`, `make k3d-deploy`, or `make kind-deploy`, restore
  `git checkout config/manager/kustomization.yaml` before committing
  (kustomize edit set image mutates this file)
- Ask before adding new dependencies
- Ask before destructive cluster operations (delete namespaces, CRDs)
- The operator manages live pod resources; always test resize logic on a
  local cluster (`make k3d-deploy` or `make kind-deploy`) before pushing changes to resize or
  safety packages
