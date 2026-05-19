# Adding a New Workload Type

This guide walks through every file and function that must change when
adding support for a new Kubernetes workload kind (e.g. Argo Rollout,
Knative Service).

## Checklist

### 1. API types -- add to the validation enum

**File:** `api/v1alpha1/rightsizepolicy_types.go`

Add the new kind to the `kubebuilder:validation:Enum` marker on
`TargetRef.Kind`:

```go
// +kubebuilder:validation:Enum=Deployment;StatefulSet;DaemonSet;CronJob;Job;Rollout
Kind string `json:"kind"`
```

Run `make manifests generate` to regenerate CRDs and deepcopy.

### 2. Workload functions -- add switch arms

**File:** `internal/controller/workload.go`

Six functions contain a `switch workload.(type)` over the supported
kinds. Add a case for the new kind in **all six**:

| Function | What it does | What to implement |
|----------|-------------|-------------------|
| `getWorkloadByName` | Fetch by namespace/name | `r.Get(ctx, key, &obj)` |
| `listWorkloadsBySelector` | List by label selector | `r.List(ctx, &list, opts...)` |
| `getPodSelectorLabels` | Pod selector for discovery | Return `.Spec.Selector.MatchLabels` |
| `getContainers` | Container list for metrics | Return `.Spec.Template.Spec.Containers` |
| `isRollingOut` | Rolling update detection | Compare `.Status.Replicas` vs `.Status.UpdatedReplicas` |
| `getPodRegex` | Prometheus pod name regex | Return `name + "-[a-z0-9]+"` (varies by kind) |

Also update `isBatchWorkload()` if the new kind is a batch workload.

### 3. RBAC markers

**File:** `internal/controller/rightsizepolicy_controller.go`

Add a `kubebuilder:rbac` marker for the new resource group. For
example, Argo Rollouts need:

```go
//+kubebuilder:rbac:groups=argoproj.io,resources=rollouts,verbs=get;list;watch
```

Then run `make manifests` to regenerate `config/rbac/role.yaml`.

### 4. Helm chart RBAC

**File:** `charts/kube-rightsize/templates/clusterrole.yaml`

Add a new rule block:

```yaml
- apiGroups:
    - argoproj.io
  resources:
    - rollouts
  verbs:
    - get
    - list
    - watch
```

### 5. Helm chart RBAC test

**File:** `charts/kube-rightsize/tests/rbac_test.yaml`

Add a new test case:

```yaml
- it: should include rollout read permissions
  asserts:
    - contains:
        path: rules
        content:
          apiGroups:
            - argoproj.io
          resources:
            - rollouts
          verbs:
            - get
            - list
            - watch
```

### 6. Regenerate manifests

```bash
make manifests generate
```

Verify the CRD schema includes the new kind in the enum, and the RBAC
ClusterRole includes the new API group.

### 7. E2E test

Create a Chainsaw test under `test/e2e/` or a Go E2E test under
`test/e2e/go/` that:

1. Creates a workload of the new kind
2. Creates a RightSizePolicy targeting it
3. Verifies the operator discovers the workload
4. Verifies recommendations are computed (if Prometheus is available)

### 8. Documentation

Update these files to mention the new kind:

- `docs/reference/api.md` -- TargetRef.Kind enum
- `docs/getting-started/quickstart.md` -- if adding a common kind
- `README.md` -- supported workloads list
- `AGENTS.md` -- if the kind requires special handling

### 9. Verify

```bash
make verify-quick    # lint, test, CRD freshness, helm
make helm-unittest   # verify the new RBAC test passes
```

## Reference: current supported kinds

| Kind | API Group | Batch? | File |
|------|-----------|--------|------|
| Deployment | `apps` | No | `workload.go` |
| StatefulSet | `apps` | No | `workload.go` |
| DaemonSet | `apps` | No | `workload.go` |
| CronJob | `batch` | Yes | `workload.go` |
| Job | `batch` | Yes | `workload.go` |