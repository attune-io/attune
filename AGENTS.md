# AGENTS.md

## Project

Kubernetes operator for in-place pod resource right-sizing (VPA replacement).
Requires Kubernetes 1.33+ (In-Place Pod Resize GA). Built with Go 1.26,
controller-runtime v0.24.1, Kubebuilder v4, K8s API v0.36.1.

## Commands

- Install deps: `go mod download`
- Build: `make build`
- Build plugin: `make build-plugin`
- Build image: `make docker-build IMG=kube-rightsize:dev`
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
- Local cluster (k3d): `make k3d-create && make k3d-deploy IMG=kube-rightsize:e2e`
- Local cluster (Kind): `make kind-create && make kind-deploy IMG=kube-rightsize:e2e`
- Full local test (auto-provisions k3d): `make test-local`
- Local smoke test (auto-provisions k3d): `make test-local-smoke`
- E2E tests: `make test-e2e` (requires local cluster with operator deployed)

## Structure

- `api/v1alpha1/` - CRD type definitions (RightSizePolicy, RightSizeDefaults)
- `cmd/manager/` - Operator entry point
- `cmd/kubectl-rightsize/` - kubectl plugin
- `internal/controller/` - Reconciler (core business logic)
- `internal/metrics/` - Prometheus metrics collection and rate limiting
- `internal/recommendation/` - Composable estimator chain (percentile, margin, confidence, bounds, change filter)
- `internal/resize/` - In-place pod resize engine via /resize subresource
- `internal/safety/` - Post-resize safety observation and rollback
- `internal/conflict/` - HPA conflict detection
- `internal/webhook/` - Admission webhooks (defaulting + validation)
- `internal/operatormetrics/` - Operator-level Prometheus metrics (init-registered)
- `internal/validation/` - Shared validation (Prometheus address SSRF checks)
- `internal/throttle/` - Shared throttle checker interface (breaks import cycle)
- `config/` - Kustomize manifests (CRDs, RBAC, manager deployment)
- `charts/kube-rightsize/` - Helm chart with cert-manager webhook support
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
forbidden by the linter (except in `cmd/kubectl-rightsize/`).

### resource.Quantity

Use `resource.ParseQuantity()` (returns error) instead of `resource.MustParse()`
(panics). Use DecimalSI format for CPU, BinarySI for memory. Use Go `time.Duration`
for all durations (e.g., `168h` not `7d`).

### Webhooks

controller-runtime v0.24.x uses typed generic interfaces. Register webhooks with:

```go
// RightSizePolicy: defaulting + validation
ctrl.NewWebhookManagedBy(mgr, &rightsizev1alpha1.RightSizePolicy{}).
    WithDefaulter(&webhook.RightSizePolicyDefaulter{}).
    WithValidator(&webhook.RightSizePolicyValidator{}).
    Complete()

// RightSizeDefaults: validation only (costPricing fields)
ctrl.NewWebhookManagedBy(mgr, &rightsizev1alpha1.RightSizeDefaults{}).
    WithValidator(&webhook.RightSizeDefaultsValidator{}).
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

### Code generation

Run `make manifests` after changing CRD types or RBAC markers. Run
`make generate` after changing API types. Commit the generated output.

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
- `charts/kube-rightsize/templates/clusterrole.yaml` + its test

Currently, Secrets are the only resource in `DisableFor` (get-only is safe).
All other resources accessed via the client need `list`/`watch`.

### Adding a new defaultable field

Fields that should be overridable by `RightSizeDefaults` must use
pointer types (`*int32`, `*bool`, `*metav1.Duration`) so nil=unset
is distinguishable from zero/false. Update all 6 locations:

1. `api/v1alpha1/rightsizepolicy_types.go` - Add `*T` field with
   `json:"name,omitempty"` and `// +optional`
2. `api/v1alpha1/defaults.go` - Add `DefaultXxx` constant
3. `internal/controller/helpers.go` `applyBuiltInDefaults()` - Add
   nil check + default assignment
4. `internal/controller/helpers.go` `mergeDefaults()` - Add merge
   clause with `inherited` tracking
5. `internal/webhook/validation.go` - Add validation if needed
6. Run `make manifests && make generate` to regenerate CRD + deepcopy

If the field also belongs in `RightSizeDefaults`, add it to
`api/v1alpha1/rightsizedefaults_types.go` as well.

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

### MkDocs documentation links

MkDocs strict mode rejects relative links that resolve outside the `docs/`
directory. When referencing files elsewhere in the repo (e.g., `charts/`,
`scripts/`), use absolute GitHub URLs instead of relative paths:

```markdown
<!-- BAD: relative path outside docs/ — MkDocs strict mode rejects this -->
[Helm README](../../charts/kube-rightsize/README.md)

<!-- GOOD: absolute GitHub URL -->
[Helm README](https://github.com/SebTardifLabs/kube-rightsize/tree/main/charts/kube-rightsize#prometheusrule)
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
- E2E tests use Chainsaw v0.2.15 on k3d or Kind clusters (K8s 1.33, 1.34, 1.35 matrix in CI)
- E2E tests that modify CRs mid-test must use a refetch/retry loop to handle
  optimistic concurrency conflicts (the operator reconciles the same object
  concurrently, causing `the object has been modified` errors on update)
- Go E2E tests (`test/e2e-go/`) must include `t.Parallel()` as the first line.
  Every test creates a unique namespace via `uniqueNS()`, so they are fully
  isolated. Without `t.Parallel()`, 13 tests run sequentially (~12 min);
  with it, they run concurrently (~2 min, bounded by OOMKill at 127s).
- E2E test policies must use `Cooldown: 1m` (the minimum). The operator requeues
  after the cooldown period, even during the data collection phase (InsufficientData).
  A longer cooldown (e.g., 10m) means the operator won't retry for 10 minutes if
  the first reconcile finds no Prometheus data, causing `waitForResize` timeouts.
  This was the root cause of the `TestE2E_OOMKill_TriggersRevert` flaky test
  (failed in 6/8 CI runs, fixed in 523292a).

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
