Recommend mode is the safest way to start with kube-rightsize. The operator
collects Prometheus metrics, computes recommendations, and writes them to the
policy's `.status.recommendations` field. No pods are modified.

## Creating a Recommend-mode policy

```yaml
apiVersion: rightsize.io/v1alpha1
kind: RightSizePolicy
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
      address: http://prometheus.monitoring:9090
    historyWindow: 7d
    minimumDataPoints: 168
  cpu:
    percentile: 95
    safetyMargin: "1.2"
    bounds:
      min: "50m"
      max: "4000m"
    controlledValues: RequestsAndLimits
  memory:
    percentile: 99
    safetyMargin: "1.3"
    bounds:
      min: "64Mi"
      max: "8Gi"
    allowDecrease: false
  updateStrategy:
    mode: Recommend
    cooldown: 1h
```

## Reading recommendations from status

```bash
kubectl get rsp api-services -o jsonpath='{.status.recommendations[*]}' | jq .
```

Each entry in the array contains:

| Field | Description |
|-------|-------------|
| `workload` | Name of the matched Deployment/StatefulSet/DaemonSet |
| `containers[].name` | Container name |
| `containers[].current` | Current CPU/memory requests and limits |
| `containers[].recommended` | Proposed CPU/memory requests and limits |
| `containers[].confidence` | Score between 0 and 1 |
| `containers[].dataPoints` | Number of Prometheus samples used |

## Interpreting confidence scores

The confidence score reflects how much data backs the recommendation:

| Score | Meaning | Action |
|-------|---------|--------|
| 0.0 - 0.3 | Very low; less than ~2 days of data | Wait for more data |
| 0.3 - 0.6 | Moderate; partial coverage | Review manually before acting |
| 0.6 - 0.8 | Good; roughly a full week of data | Safe to promote to Canary |
| 0.8 - 1.0 | High; well-covered history window | Safe to promote to Auto |

!!! tip
    Increase `historyWindow` and `minimumDataPoints` for workloads with
    weekly traffic patterns so the estimator captures weekday/weekend
    variation.

## Estimating savings

The policy status includes aggregated savings:

```bash
kubectl get rsp api-services -o jsonpath='{.status.savings}' | jq .
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
kubectl patch rsp api-services --type merge \
  -p '{"spec":{"updateStrategy":{"mode":"Canary","canary":{"percentage":10,"observationPeriod":"30m"}}}}'
```
