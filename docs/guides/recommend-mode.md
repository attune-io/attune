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
| 0.0 - 0.3 | Very low; sparse recent data | Wait for more data |
| 0.3 - 0.6 | Moderate; partial coverage | Review manually before acting |
| 0.6 - 0.8 | Good; substantial recent coverage | Safe to promote to Canary |
| 0.8 - 1.0 | High; near-complete history coverage | Safe to promote to Auto |

!!! tip
    Increase `historyWindow` and `minimumDataPoints` for workloads with
    weekly traffic patterns so the estimator captures weekday/weekend
    variation.

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
kubectl patch rsp api-services --type merge \
  -p '{"spec":{"updateStrategy":{"type":"Canary","canary":{"percentage":10,"observationPeriod":"30m"}}}}'
```
