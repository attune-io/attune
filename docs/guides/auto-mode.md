# Auto Mode

Auto mode is the production end-state for kube-rightsize. The operator
continuously resizes all eligible pods based on observed metrics. Before
enabling Auto mode, you should have validated recommendations through
Recommend and/or Canary mode.

## Prerequisites

Before switching to Auto mode:

1. **Run in Recommend mode** for at least 1 full history window (default 7 days)
   to build confidence in the recommendations
2. **Verify recommendations are reasonable** using the kubectl plugin:
   ```bash
   kubectl rightsize recommendations -n <namespace>
   ```
3. **Test with Canary mode** (optional but recommended) to validate resizes
   on a subset of pods before the full fleet
4. **Configure appropriate bounds** to prevent extreme recommendations:
   ```yaml
   cpu:
     bounds:
       min: 50m    # never go below 50 millicores
       max: 4000m  # never exceed 4 cores
   memory:
     bounds:
       min: 64Mi   # never go below 64 MiB
       max: 8Gi    # never exceed 8 GiB
   ```

## Creating an Auto-mode policy

```yaml
apiVersion: rightsize.io/v1alpha1
kind: RightSizePolicy
metadata:
  name: my-app
  namespace: production
spec:
  targetRef:
    kind: Deployment
    selector:
      matchLabels:
        tier: api
  metricsSource:
    prometheus:
      address: http://prometheus-server.monitoring:80
    historyWindow: 168h  # 7 days of data
  cpu:
    percentile: 95
    safetyMargin: "1.2"
    bounds:
      min: 50m
      max: "4000m"
    controlledValues: RequestsAndLimits
  memory:
    percentile: 99
    safetyMargin: "1.3"
    bounds:
      min: 64Mi
      max: 8Gi
    controlledValues: RequestsAndLimits
  updateStrategy:
    mode: Auto
    cooldown: 1h
    autoRevert: true
```

## Recommended guardrails

| Setting | Purpose | Suggested value |
|---------|---------|-----------------|
| `safetyMargin` | Headroom above observed usage | 1.2 (CPU), 1.3 (memory) |
| `bounds.min/max` | Prevent extreme recommendations | Match your resource limits policy |
| `cooldown` | Time between resizes | 1h minimum for production |
| `autoRevert` | Roll back if pods become unhealthy | `true` for production |

The safety monitor watches each resized pod for an observation period before
declaring the resize successful. The default is 5 minutes. To configure it,
set `canary.observationPeriod` (this field is shared across all resize modes):

```yaml
spec:
  updateStrategy:
    mode: Auto
    autoRevert: true
    canary:
      observationPeriod: 10m  # safety watch window after each resize
```

### Safety margin guidance

- **CPU**: 1.2x (20% headroom) works well for steady-state services. Use 1.5x
  for bursty workloads.
- **Memory**: 1.3x (30% headroom) is recommended because memory pressure causes
  OOM kills. Never go below 1.1x for production.

## Monitoring Auto mode

### Check policy status

```bash
# Overview of all policies
kubectl rightsize status -A

# Estimated savings
kubectl rightsize savings -n production

# Detailed per-container recommendations
kubectl rightsize recommendations -n production
```

### Watch for degradation

The operator sets a `Degraded` condition when the revert rate exceeds 50%.
Monitor this with:

```bash
kubectl get rsp -A -o jsonpath='{range .items[*]}{.metadata.namespace}/{.metadata.name}: {range .status.conditions[*]}{.type}={.reason} {end}{"\n"}{end}'
```

### Prometheus metrics

The operator exports metrics for dashboarding:

- `kube_rightsize_recommendation_cpu_cores` -- Recommended CPU per workload
- `kube_rightsize_recommendation_memory_bytes` -- Recommended memory per workload
- `kube_rightsize_confidence` -- Confidence score (0-1) per workload
- `kube_rightsize_resize_total` -- Total resizes performed
- `kube_rightsize_reverts_total` -- Total reverts (broken down by reason)

Alert on high revert rates:

```yaml
- alert: RightSizeHighRevertRate
  expr: rate(kube_rightsize_reverts_total[1h]) > 0.1
  for: 10m
  annotations:
    summary: "High revert rate for {{ $labels.namespace }}/{{ $labels.workload }}"
```

## Promoting from Recommend or Canary

### From Recommend mode

```bash
kubectl patch rsp my-app --type merge \
  -p '{"spec":{"updateStrategy":{"mode":"Auto","autoRevert":true}}}'
```

### From Canary mode

```bash
kubectl patch rsp my-app --type merge \
  -p '{"spec":{"updateStrategy":{"mode":"Auto"}}}'
```

## Rollback

If Auto mode causes issues, switch back to Recommend immediately:

```bash
kubectl patch rsp my-app --type merge \
  -p '{"spec":{"updateStrategy":{"mode":"Recommend"}}}'
```

This stops all future resizes. Already-resized pods keep their current
resources until their next restart.
