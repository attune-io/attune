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

### Set appropriate bounds

Choose a `min` bound for CPU that keeps the HPA utilization target in a
reasonable range. For example, if HPA targets 70% utilization and pods
typically use 200m, a `min: "200m"` prevents requests from dropping below
actual usage.

### Memory is always safe

Memory-based HPAs (less common) scale on `memory` utilization, not requests.
Attune can safely adjust memory requests alongside a memory-based HPA
because the working set size does not change when the request changes.

## Monitoring coexistence

Watch both HPA and AttunePolicy status together:

```bash
kubectl get hpa,ap -o wide
```

Check for conflict-related events:

```bash
kubectl get events --field-selector reason=HPAConflict
```

## When to avoid combining them

If your HPA scales on **custom metrics** that are derived from resource
requests (e.g. a custom ratio metric), changes to requests may affect the
scaling signal. In this case, use `Recommend` mode to review changes
manually before applying them.
