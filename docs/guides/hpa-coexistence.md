# HPA Coexistence

Attune is designed to work alongside Horizontal Pod Autoscalers
(HPAs) without causing scaling conflicts or death spirals.

## Why HPA + VPA was problematic

VPA and HPA both react to CPU utilization. When VPA increases requests, the
utilization percentage drops, causing HPA to scale in. When HPA scales in,
per-pod load increases, causing VPA to increase requests again. This feedback
loop is the classic "death spiral."

## How Attune avoids conflicts

Attune adjusts **resource requests** (and optionally limits), while
HPA adjusts **replica count**. The operator does not change the number of
pods. Because in-place resize modifies cgroup limits on running pods without
restarting them, the HPA's utilization metric reflects the new allocation
immediately.

The conflict detector identifies HPAs targeting the same workload and logs a
notice:

```text
HPA my-hpa targets the same Deployment/my-app; attune will adjust
requests without interfering with HPA scaling
```

## Configuration tips

### Use `RequestsOnly` for CPU

When an HPA uses CPU utilization as its metric, set `controlledValues` to
`RequestsOnly` so that limits remain unchanged:

```yaml
spec:
  cpu:
    percentile: 95
    overhead: "20"
    controlledValues: RequestsOnly
    minAllowed: "100m"
    maxAllowed: "4000m"
```

!!! tip
    HPA computes utilization as `usage / request`. Lowering requests increases
    the utilization percentage, which may cause HPA to scale out. Set
    conservative bounds to prevent requests from dropping too far.

### QoS-aware HPA target adjustment

When Attune changes a pod's CPU request, it recalculates the HPA target to
preserve the same absolute CPU threshold:

```
newTarget = originalTarget * (originalRequest / newRequest)
```

A pod-level `Resource` metric uses the pod CPU total. The resized container
contributes the applied CPU (`To` on the successful in-place history row),
not the live request and not the recommendation. A container with no
successful in-place CPU row contributes its live CPU request on both sides.
For example, the app container goes from `400m` to `200m` and a sidecar
stays at `200m`. The pod total goes from `600m` to `400m`, and a target of
80 becomes 120 (`80 * 600 / 400`), not 160 from the app alone.

The first `Resource` adjustment stores the original utilization percent as
`attune.io/original-target-cpu` and the original pod CPU request as
`attune.io/original-cpu-request` (`600m` in that example). Later resizes
reuse those stored values so the absolute threshold does not drift. A
single-container workload at `200m` and 80% (160m absolute) becomes 40% at
`400m`, then 20% at `800m`, not 40% again.

A `ContainerResource` metric uses only the named container. The first
adjustment stores that container's original percent as
`attune.io/hpa-cpu-target.<container>` and that container's original CPU
request as `attune.io/hpa-cpu-base.<container>`. Those keys are separate
from the pod-total keys. An unchanged container's metric is left alone.

The upper cap on this target depends on the pod's QoS class:

- **Burstable** (limit > request): targets above 100% are allowed, up to
  `floor(limit / request * 100)`. The container can burst up to its CPU
  limit, so utilization above 100% of request is achievable. This preserves
  the absolute threshold without triggering premature scale-outs.
- **Guaranteed** (limit == request): targets are capped at 100%. Utilization
  cannot exceed 100% when cgroups enforce `limit == request`. After a
  `RequestsAndLimits` apply, dest leftover is the applied CPU (`To`), not
  the pre-resize leftover, so the cap stays 100% instead of a stale
  higher dest that live pods can never reach.
- **BestEffort** (no requests/limits): not applicable; HPA resource metrics
  require requests to be set.

For example, if a Burstable pod has `request: 300m` and `limit: 1000m` with
HPA target 70%, and requests drop from 500m to 300m:

```
newTarget = 70 * (500 / 300) = 116%
burstableCap = floor(1000 / 300 * 100) = 333%
finalTarget = min(116, 333) = 116%
```

The 116% target preserves the original 350m absolute threshold
(`116% * 300m = 348m`).

### Set appropriate bounds

Choose a `min` bound for CPU that keeps the HPA utilization target in a
reasonable range. For example, if HPA targets 70% utilization and pods
typically use 200m, a `min: "200m"` prevents requests from dropping below
actual usage.

### Memory utilization moves with the request

A memory HPA that uses utilization scales on usage divided by the request.
Shrinking the memory request raises that percentage, so the HPA can add
replicas. When the HPA annotation `attune.io/auto-tune` is `"true"`, Attune
retunes memory utilization targets in the same update as CPU.

The new percent is the original target times the original request divided
by the new request, truncated toward zero, then capped by the memory limit.
A Resource metric uses the sum of container memory requests, so an unchanged
container dilutes the ratio. A ContainerResource metric uses only that
container. Object metrics and AverageValue targets are left unchanged.

The original Resource target and request are stored on the HPA as
`attune.io/original-target-memory` and `attune.io/original-memory-request`.
ContainerResource baselines share one JSON annotation,
`attune.io/original-container-memory`, keyed by container name. A later
resize multiplies the stored original target by the stored request divided
by the new request.

`updateStrategy.hpaTargetBounds.memory` is an optional percent band. It is
not filled with 50 and 90. Set `min` or `max` when you want a tighter range
than the limit cap. 50 and 90 are a common choice. The limit cap still wins
over a user minimum. CPU bounds do not apply to memory.

`memory.allowDecrease` still defaults to false, so a default policy does
not shrink memory requests. With auto-tune, a decrease moves the utilization
target with the request instead of leaving the HPA to scale out. Nothing
in the Helm chart turns the annotation on.

## Monitoring coexistence

Watch both HPA and AttunePolicy status together:

```bash
kubectl get hpa,ap -o wide
```

Check for conflict-related events:

```bash
kubectl get events --field-selector reason=HPAConflict
```

## Scale to zero

Attune classifies each workload into one of three states. This is
per-workload. There is no policy-wide ScaledToZero condition.

1. **HPA ScaledToZero**: the matching HPA has
   `status.conditions[type=ScaledToZero]=True`. Attune reads that
   condition string on any cluster. Older clusters never set it. This
   is not a human turning the app off.
2. **Manual zero**: the owner Deployment or StatefulSet has
   `spec.replicas` set to `0`, and there is no ScaledToZero condition.
   Attune uses `spec.replicas` only, never `status.replicas`.
3. **Active**: otherwise. Recommend and apply continue as usual.

Idle means HPA ScaledToZero or manual zero. For that workload only,
Attune skips in-place resize, template persist, startup boost, and
eviction. Leftover Running pods stay untouched. Attune does not evict
the last replica to finish scale-to-zero.

HPA conflict detection is unchanged: same target plus a CPU or memory
Resource or ContainerResource metric. ScaledToZero is not a new
conflict type.

CREATE initial sizing does not look at the owner replica count. When
HPA scales the workload back up, new pods still receive initial
sizing.

## When to avoid combining them

If your HPA scales on **custom metrics** that are derived from resource
requests (e.g. a custom ratio metric), changes to requests may affect the
scaling signal. In this case, use `Recommend` mode to review changes
manually before applying them.
