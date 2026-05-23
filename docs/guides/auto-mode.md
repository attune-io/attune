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
     minAllowed: "50m"    # never go below 50 millicores
     maxAllowed: "4000m"  # never exceed 4 cores
   memory:
     minAllowed: "64Mi"   # never go below 64 MiB
     maxAllowed: "8Gi"    # never exceed 8 GiB
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
    overhead: "20"
    minAllowed: "50m"
    maxAllowed: "4000m"
    controlledValues: RequestsAndLimits
  memory:
    percentile: 99
    overhead: "30"
    minAllowed: "64Mi"
    maxAllowed: "8Gi"
    controlledValues: RequestsAndLimits
  updateStrategy:
    type: Auto
    cooldown: 1h
    autoRevert: true
```

## Recommended guardrails

| Setting | Purpose | Suggested value |
|---------|---------|-----------------|
| `overhead` | Headroom above observed usage | 20% (CPU), 30% (memory) |
| `minAllowed/maxAllowed` | Prevent extreme recommendations | Match your resource limits policy |
| `cooldown` | Time between resizes | 1h minimum for production |
| `autoRevert` | Roll back if pods become unhealthy | `true` for production |

The safety monitor watches each resized pod for an observation period before
declaring the resize successful. The default is 5 minutes. To configure it,
set `safetyObservationPeriod`:

```yaml
spec:
  updateStrategy:
    type: Auto
    autoRevert: true
    safetyObservationPeriod: 10m  # safety watch window after each resize
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

The operator sets a `Degraded` condition when 3 or more of the last 5 resizes are reverted.
Monitor this with:

```bash
kubectl get rsp -A -o jsonpath='{range .items[*]}{.metadata.namespace}/{.metadata.name}: {range .status.conditions[*]}{.type}={.reason} {end}{"\n"}{end}'
```

### Prometheus metrics

The operator exports metrics for dashboarding:

- `kube_rightsize_recommendation_cpu_cores` -- Recommended CPU per workload
- `kube_rightsize_recommendation_memory_bytes` -- Recommended memory per workload
- `kube_rightsize_confidence` -- Confidence score (0-1) per workload
- `kube_rightsize_resize_total` -- Total successful, failed, and reverted in-place resize operations
- `kube_rightsize_eviction_total` -- Total eviction fallback attempts when `resizeMethod: InPlaceOrRecreate`
- `kube_rightsize_reverts_total` -- Total reverts (broken down by reason)

Alert on high revert rates:

```yaml
- alert: RightSizeHighRevertRate
  expr: rate(kube_rightsize_reverts_total[1h]) > 0.1
  for: 10m
  annotations:
    summary: "High revert rate for {{ $labels.namespace }}/{{ $labels.workload }}"
```

## Scheduled resizes

By default, resizes can occur at any time. Use the `schedule` field to restrict
resizes to specific time windows and days of the week. Recommendations are always
computed; only the actual resize execution is gated.

```yaml
spec:
  updateStrategy:
    type: Auto
    schedule:
      windows:
        - start: "02:00"
          end: "06:00"
      daysOfWeek: ["Monday", "Tuesday", "Wednesday", "Thursday", "Friday"]
      timezone: "America/New_York"
```

Key behavior:

- If `daysOfWeek` is omitted, all days are allowed.
- If `windows` is omitted, all times are allowed (only day filtering applies).
- Overnight windows work: `start: "22:00", end: "06:00"` wraps past midnight.
- The `ScheduleBlocked` status condition is set when outside the window.
- An invalid timezone name fails open (resizes are allowed) to prevent
  silent lockout from a typo.

Combine scheduling with budget caps for large fleets:

```yaml
spec:
  updateStrategy:
    type: Auto
    schedule:
      windows:
        - start: "02:00"
          end: "06:00"
    maxConcurrentResizes: 10
    maxTotalCpuIncrease: "2000m"
    maxTotalMemoryIncrease: "4Gi"
```

See [`examples/12-scheduled-auto-mode.yaml`](https://github.com/SebTardifLabs/kube-rightsize/blob/main/examples/12-scheduled-auto-mode.yaml) for a complete example.
If resizes are blocked unexpectedly, see the [troubleshooting guide](troubleshooting.md) for schedule-specific diagnostics.

## Exporting recommendations to ConfigMaps

The `export` feature writes recommendation data to ConfigMaps for external
consumption (e.g., GitOps workflows with ArgoCD or Flux that apply resource
patches from CI/CD rather than letting the operator resize directly).

```yaml
spec:
  updateStrategy:
    type: Recommend  # or Auto
    export:
      configMap: true
```

When enabled, the operator creates one ConfigMap per workload, named
`<policy>-<workload>-recommendations`, with an owner reference to the policy
for automatic cleanup. The ConfigMap contains per-container recommended CPU
and memory values.

This is useful in GitOps workflows where:

1. The operator runs in Recommend mode to compute recommendations.
2. A CI/CD pipeline reads the ConfigMaps and generates resource patches.
3. ArgoCD or Flux applies the patches through the normal GitOps flow.

## Promoting from Recommend or Canary

### From Recommend mode

```bash
kubectl patch rsp my-app --type merge \
  -p '{"spec":{"updateStrategy":{"type":"Auto","autoRevert":true}}}'
```

### From Canary mode

```bash
kubectl patch rsp my-app --type merge \
  -p '{"spec":{"updateStrategy":{"type":"Auto"}}}'
```

## Rollback

If Auto mode causes issues, switch back to Recommend immediately:

```bash
kubectl patch rsp my-app --type merge \
  -p '{"spec":{"updateStrategy":{"type":"Recommend"}}}'
```

This stops all future resizes. Already-resized pods keep their current
resources until their next restart.
