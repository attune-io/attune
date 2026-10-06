# Adding a New Workload Type

This guide walks through every file and function that must change when
adding support for a new Kubernetes workload kind. `Rollout`
(`argoproj.io/v1alpha1`) is the worked example for an optional CRD.
Built-in kinds such as Deployment stay on the apps and batch types.

## Checklist

### 1. API types -- add to the validation enum

**File:** `api/v1alpha1/attunepolicy_types.go`

Keep these three lists in sync. The CRD enum and the webhook disagree
when one of them is stale. The webhook uses `IsSupportedTargetKind` and
does not look up the CRD.

```go
// SupportedTargetKindsCSV is the admission error text.
const SupportedTargetKindsCSV = "Deployment, StatefulSet, DaemonSet, CronJob, Job, ReplicaSet, Rollout"

func IsSupportedTargetKind(kind string) bool {
    switch kind {
    case "Deployment", "StatefulSet", "DaemonSet", "CronJob", "Job", "ReplicaSet", "Rollout":
        return true
    default:
        return false
    }
}

// +kubebuilder:validation:Enum=Deployment;StatefulSet;DaemonSet;CronJob;Job;ReplicaSet;Rollout
Kind string `json:"kind"`
```

Admission must accept the kind even when an optional CRD is absent. Do
not discover the CRD in the webhook.

Run `make manifests generate` to regenerate CRDs and deepcopy.

### 2. Workload adapter -- implement the interface

**File:** `internal/controller/workload_adapters.go`

`newWorkloadAdapter` returns nil unless the concrete Go type has a
case. A `workloadKinds` entry alone is not enough: pod selection and
the rollout gate both no-op, and a mid-rollout object would be resized.

For an optional CRD, do not import the upstream module. Rollout uses
the local type in `internal/argorollout` (`spec.selector`,
`spec.template`, `spec.replicas`, `status.updatedReplicas`,
`status.phase`, `status.abort`, and `spec.workloadRef`). Register it
with `scheme.AddKnownTypes`. Scheme registration does not require the
CRD and does not start an informer.

Put `+kubebuilder:skip` on the package (`doc.go`). controller-gen
treats every struct that embeds `TypeMeta` and `ObjectMeta` as a CRD.
This package has no group name, so the output file is
`config/crd/bases/_.yaml`, and `make build-crds` prepends that empty
CRD to `dist/crds.yaml`. Attune does not ship the Rollout CRD.
DeepCopy stays hand-written in the type file.

```go
case *argorollout.Rollout:
    return &rolloutAdapter{Rollout: w}
```

`PodNameRegexSuffix` for Rollout is the Deployment suffix
`-[a-z0-9]+-[a-z0-9]{5}`. A shorter suffix also matches a Deployment
of the same name. Select pods with `spec.selector`, including
`matchExpressions`. Do not require `ownerReferences` to name the
Rollout. The pod's controller owner is the child ReplicaSet.

`IsRollingOut` for Rollout is true when `status.abort` is true or the
phase is `Paused`, `Progressing`, or `Degraded`. `Degraded` is Argo's
aborted, timed-out, or invalid-spec phase, so it skips even when abort
is false and replica counts match. A `Healthy` phase and an empty phase
are not a rollout when `updatedReplicas` is behind `spec.replicas`.
There is no phase named `Abort`. Do not copy the Deployment
nil-replicas check.

ReplicaSet policies must ignore ReplicaSets owned by a Rollout
(`controller: true`, `apiVersion: argoproj.io/v1alpha1`, `kind: Rollout`),
the same way they ignore ReplicaSets owned by a Deployment. Do not
require `controller: true` on the existing Deployment owner check.

### 3. Do not watch an optional CRD

`SetupWithManager` only watches `AttunePolicy`. A watch, `Owns`, or a
`cache.ByObject` entry for Rollout fails `mgr.Start` with
`no matches for kind "Rollout"` when the CRD is absent.

Add the local type to `Client.Cache.DisableFor` in `cmd/manager/main.go`
next to Secrets. Get and List then use the live API reader and never
start an informer. `DisableFor` does not talk to the API server during
setup.

On each Rollout reconcile, before Get or List, resolve
`argoproj.io` / `v1alpha1` / `Rollout` with the manager RESTMapper.
`meta.IsNoMatchError` sets Ready False, reason `WorkloadCRDMissing`,
message `argoproj.io/v1alpha1 Rollout CRD is not installed`, and
returns success with a requeue of a few minutes. Do not return a
reconciler error and do not increment `ReconcileErrorsTotal` for that
case. Do not watch `customresourcedefinitions`. A nil mapper in unit
tests is not a missing CRD. A Deployment policy must not call the
mapper. NotFound of a named Rollout stays on the empty-list path.

### 4. RBAC markers

**File:** `internal/controller/attunepolicy_controller.go`

Template persistence patches the Rollout, so the marker needs `patch`
and `update` as well as `get` and `list`. Do not include `watch`.
Rollouts stay outside the cache. The resource is `rollouts`, not
`rollouts.argoproj.io`. Put the marker on the controller.

```go
//+kubebuilder:rbac:groups=argoproj.io,resources=rollouts,verbs=get;list;patch;update
```

Then run `make manifests` to regenerate `config/rbac/role.yaml`.
Installing the rule when the CRD is absent is safe. There is no feature
flag.

### 5. Helm chart RBAC

**File:** `charts/attune/templates/clusterrole.yaml`

```yaml
- apiGroups:
    - argoproj.io
  resources:
    - rollouts
  verbs:
    - get
    - list
    - patch
    - update
```

### 6. Helm chart RBAC test

**File:** `charts/attune/tests/rbac_test.yaml`

```yaml
- it: should include argoproj.io rollouts get list patch and update
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
            - patch
            - update
```

### 7. Regenerate manifests

```bash
make manifests generate
make build-installer
make build-crds
```

`build-installer` rewrites `config/manager/kustomization.yaml`. Restore
that file before commit. `dist/install.yaml` and `dist/crds.yaml` are
tracked release artifacts.

### 8. Tests

Fake-client tests are the gate for an optional CRD. Do not add that CRD
to the nightly E2E matrix and do not vendor the upstream module.

Cover selector matching (including both stable and canary ReplicaSets),
the resize gate (`Paused`, `Progressing`, `Degraded`, `abort`, and a Healthy scale-out that must not skip),
template persistence on the parent object, `spec.workloadRef` leaving
the template alone, and ReplicaSet owner filtering. The webhook accepts
the kind and still rejects an unknown kind.

Prove `mgr.Start` with envtest that does not install the optional CRD.
A fake client never calls RESTMapper, so "type not in the scheme" does
not prove the process stays up. `go.mod` must not gain
`github.com/argoproj/argo-rollouts`.

Built-in kinds that ship with Kubernetes can still add a Chainsaw or
Go E2E test. Rollout does not.

### 9. Documentation

Update these files to mention the new kind:

- `docs/reference/api.md` -- TargetRef.Kind enum and Ready reasons
- `docs/reference/configuration.md` -- `targetRef.kind` and conditions
- `docs/reference/cli.md` -- Ready column and `--filter` when a new Ready reason is added
- `docs/guides/troubleshooting.md` -- missing CRD, when that is the failure
- `docs/guides/upgrading.md` -- when RBAC must be re-applied
- `docs/contributing/adding-workload-types.md` -- this page
- `docs/SPEC.md` -- workload list
- `README.md` -- supported workloads list, for a future kind that is part of the default install story

Rollout does not edit the root README or `charts/attune/README.md`.
There is no chart switch and no new guide file.

### 10. Verify

```bash
make verify-quick    # lint, test, CRD freshness, helm
make helm-unittest   # verify the new RBAC test passes
```

## Reference: current supported kinds

| Kind | API Group | Batch? | Adapter file |
|------|-----------|--------|--------------|
| Deployment | `apps` | No | `workload_adapters.go` |
| StatefulSet | `apps` | No | `workload_adapters.go` |
| DaemonSet | `apps` | No | `workload_adapters.go` |
| CronJob | `batch` | Yes | `workload_adapters.go` |
| Job | `batch` | Yes | `workload_adapters.go` |
| ReplicaSet | `apps` | No | `workload_adapters.go` |
| Rollout | `argoproj.io` | No | `workload_adapters.go` (`internal/argorollout`) |
