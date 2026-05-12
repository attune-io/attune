kube-rightsize uses the Kubernetes 1.33+ in-place pod resize API to adjust
container resources without restarting pods. This page explains how the
resize API works and how the operator uses it.

## The `/resize` subresource

Kubernetes 1.33 graduated `InPlacePodVerticalScaling` to GA. This adds a
`/resize` subresource on pods that accepts a modified `PodSpec` with new
container resource requests/limits.

```
PATCH /api/v1/namespaces/{ns}/pods/{name}/resize
```

The kubelet applies the new resources to the running container's cgroup
limits without restarting it. CPU changes take effect immediately; memory
limit increases take effect immediately but decreases only apply when the
container's working set drops below the new limit.

## How kube-rightsize uses it

The operator's resize engine (`internal/resize/engine.go`) performs
resizes via the typed Kubernetes client:

```go
clientset.CoreV1().Pods(namespace).UpdateResize(ctx, name, updatedPod, opts)
```

### Pre-checks before resize

Before calling `UpdateResize`, the controller runs several safety checks:

1. **Pod already at target**: Skips if the running pod's actual resources
   match the recommendation (compares against the live pod, not the
   Deployment template).
2. **Node capacity**: Verifies that total pod requests after resize don't
   exceed the node's allocatable resources.
3. **LimitRange/ResourceQuota**: Checks that the target doesn't violate
   namespace constraints.
4. **QoS preservation**: Ensures the resize won't change the pod's QoS
   class (e.g., from Guaranteed to Burstable).
5. **Resize policy warning**: If the container has `resizePolicy` set to
   `RestartContainer`, the operator logs a warning but proceeds with the
   resize (the kubelet will restart the container).

### Post-resize tracking

After a successful resize, the operator:

1. Writes tracking annotations to the pod (see table below).
2. If `autoRevert: true`, monitors the pod for safety violations (OOMKill,
   CPU throttle, restart spikes, NotReady).
3. Records the operation in `status.resizeHistory`.
4. Emits a Kubernetes Event (`Normal/Resized`).

### Pod tracking annotations

After a resize, the operator writes these annotations to the pod for safety
observation and revert tracking:

| Annotation | Description |
|---|---|
| `rightsize.io/resized-at` | RFC 3339 timestamp of the resize |
| `rightsize.io/resized-container` | Name of the resized container |
| `rightsize.io/resized-workload` | Name of the parent workload |
| `rightsize.io/original-cpu-request` | CPU request before the resize |
| `rightsize.io/original-memory-request` | Memory request before the resize |
| `rightsize.io/original-restart-count` | Container restart count at resize time |

These annotations are removed once the safety observation period completes
(regardless of whether the resize is kept or reverted).

!!! warning "Multi-container limitation"
    When multiple containers in the same pod are resized in the same cycle,
    each container's annotations overwrite the previous container's data.
    This means only the last container's resize is tracked for safety
    observation. A fix is tracked in
    [#45](https://github.com/SebTardifLabs/kube-rightsize/issues/45).

## Limits and caveats

- **Memory decreases**: The kernel only reclaims memory when the working
  set drops below the new limit. If the application holds onto allocated
  memory, the decrease has no practical effect until the process releases it.
- **Init containers**: Not resizable in-place. The operator only resizes
  regular containers.
- **Restart policy**: Containers with `resizePolicy: RestartContainer` will
  be restarted by the kubelet when their resources change.
- **Prometheus address limit**: The operator caches at most 64 unique
  Prometheus collector instances. If more than 64 distinct addresses are
  configured across all policies, additional addresses are rejected with
  an error status.
- **Minimum cooldown**: The operator enforces a minimum cooldown of 1 minute
  regardless of the `cooldown` field value. This prevents accidental DoS
  via rapid resize loops.