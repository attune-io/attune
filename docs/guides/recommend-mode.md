# Recommend Mode

Recommend mode is the safest way to start with attune. The operator
collects Prometheus metrics, computes recommendations, and writes them to the
policy's `.status.recommendations` field. No pods are modified.

## Creating a Recommend-mode policy

```yaml
apiVersion: attune.io/v1alpha1
kind: AttunePolicy
metadata:
  name: api-services
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
    historyWindow: 168h
    minimumDataPoints: 48
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
    allowDecrease: false
  updateStrategy:
    type: Recommend
    cooldown: 1h
```

## Reading recommendations from status

```bash
kubectl get attunepolicy api-services -o jsonpath='{.status.recommendations[*]}' | jq .
kubectl attune recommendations -n production
kubectl attune explain -n production api-services
```

Each entry in the array contains:

| Field | Description |
|-------|-------------|
| `workload` | Name of the matched Deployment/StatefulSet/DaemonSet |
| `containers[].name` | Container name |
| `containers[].current` | Current CPU/memory requests and limits |
| `containers[].recommended` | Proposed CPU/memory requests and limits |
| `containers[].explanation` | Estimator chain (percentile → margin → burst → confidence → bounds → change filter) |
| `containers[].confidence` | Score between 0 and 1 |
| `containers[].dataPoints` | Number of Prometheus samples used |
| `stale` | When true, Prometheus returned no fresh data; resizes are blocked |

### Why is CPU recommended at Xm?

`kubectl attune explain` (and `status.recommendations[].containers[].explanation`) walks the same chain the controller used:

| Stage | Field | What it means |
|-------|--------|---------------|
| Raw percentile | `rawPercentile` | Selected percentile across hourly buckets (max hour) |
| Overhead | `afterOverhead` | Safety margin applied to the percentile |
| Burst | `burstFactor` / `afterBurst` | Extra headroom when peak >> percentile |
| Confidence | `confidenceFactor` / `afterConfidence` | Widens when data is sparse |
| Bounds | `afterBounds` / `boundsApplied` | Clamped to `minAllowed` / `maxAllowed` |
| Change filter | `afterChangeFilter` / `changeFilterApplied` | Min % change and max step caps |
| Final | `final` / `finalAdjustment` | Value written to recommended (plus any controller post-steps) |

Field names match status JSON so automation can parse without scraping prose.

### Why no resize happened?

Recommendations can look "ready" while pods stay unchanged. Common reasons:

| Reason | Where to look | What to do |
|--------|----------------|------------|
| Change filter / min change | `explanation.*.changeFilterApplied` | Expected for tiny deltas; lower min change or wait for drift |
| Cooldown | condition `Resizing=False` reason `CooldownActive` | Wait for cooldown / backoff |
| Budget cap | events `BudgetExhausted`, metric `attune_budget_exhausted_total` | Raise per-cycle caps or reduce targets |
| Canary not promoted | `status.canary.phase` | Wait for observation or set `autoPromote` |
| Deferred / Infeasible | condition `ResizeBlocked`, `workloads.deferred` / `infeasible` | See [troubleshooting](troubleshooting.md#deferred-or-infeasible-resize-stuck-pods) |
| Stale data | `recommendations[].stale` | Fix Prometheus reachability |
| Schedule window | condition `ScheduleBlocked` | Wait for window or adjust schedule |
| At target after clamp | events / logs for filtering or memory clamp | Expected when already near recommendation |

## Interpreting confidence scores

The confidence score reflects how much data backs the recommendation:

| Score | Meaning | Action |
|-------|---------|--------|
| 0.0 - 0.3 | Very low; sparse recent data | Wait for more data |
| 0.3 - 0.6 | Moderate; partial coverage | Review manually before acting |
| 0.6 - 0.8 | Good; substantial recent coverage | Safe to promote to Canary |
| 0.8 - 1.0 | High; near-complete history coverage | Safe to promote to Auto |

!!! tip
    Increase `historyWindow` and `minimumDataPoints` for workloads with
    weekly traffic patterns so the estimator captures weekday/weekend
    variation.

## Automation / JSON

For scripts, prefer status JSON over parsing `explain` prose:

```bash
kubectl get attunepolicy api-services -n production -o json | \
  jq '.status.recommendations[] | {workload, containers: [.containers[] | {name, recommended, explanation}]}'
```

Schema fields under `explanation` are versioned with the CRD; breaking renames require a CRD/API bump and docs update.

## Estimating savings

The policy status includes aggregated savings:

```bash
kubectl get attunepolicy api-services -o jsonpath='{.status.savings}' | jq .
```

```json
{
  "cpuRequestReduction": "1200m",
  "memoryRequestReduction": "2Gi"
}
```

These values represent the total reduction across all matched workloads.
Multiply by your per-core and per-GiB cloud pricing to estimate monthly
cost savings.

## Promoting to an active mode

When you are satisfied with the recommendations, change the mode:

- Use [Canary](canary-rollout.md) to resize a subset first.
- Use **OneShot** to resize a single pod per reconciliation cycle.
- Use **Auto** to resize all eligible pods (best for non-critical workloads).

```bash
kubectl patch attunepolicy api-services --type merge \
  -p '{"spec":{"updateStrategy":{"type":"Canary","canary":{"percentage":10,"observationPeriod":"30m"}}}}'
```
