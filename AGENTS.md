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

### Pod resize

The `/resize` subresource is not available via the controller-runtime client.
Use a typed `kubernetes.Clientset` and call `UpdateResize()`:

```go
clientset.CoreV1().Pods(ns).UpdateResize(ctx, name, pod, metav1.UpdateOptions{})
```

### Code generation

Run `make manifests` after changing CRD types or RBAC markers. Run
`make generate` after changing API types. Commit the generated output.

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
