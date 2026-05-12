All metrics are exposed on the operator's metrics endpoint (default port
8080) and use the `kube_rightsize_` prefix.

## Counters

### kube_rightsize_resize_total

Total number of resize operations performed.

| Label | Description |
|-------|-------------|
| `namespace` | Workload namespace |
| `workload` | Workload name |
| `resource` | `cpu` or `memory` |
| `result` | `Success`, `Failed`, or `Reverted` |

### kube_rightsize_reverts_total

Total number of resize reverts triggered by the safety monitor.

| Label | Description |
|-------|-------------|
| `namespace` | Workload namespace |
| `workload` | Workload name |
| `reason` | `oomkill`, `throttle`, `restart`, or `notready` |

### kube_rightsize_prometheus_query_errors_total

Total number of failed Prometheus queries. No labels.

## Gauges

### kube_rightsize_recommendation_cpu_cores

Recommended CPU cores for each workload container.

| Label | Description |
|-------|-------------|
| `namespace` | Workload namespace |
| `workload` | Workload name |
| `container` | Container name |

### kube_rightsize_recommendation_memory_bytes

Recommended memory (bytes) for each workload container.

| Label | Description |
|-------|-------------|
| `namespace` | Workload namespace |
| `workload` | Workload name |
| `container` | Container name |

### kube_rightsize_savings_cpu_cores_total

Total CPU cores saved per namespace.

| Label | Description |
|-------|-------------|
| `namespace` | Namespace |

### kube_rightsize_savings_memory_bytes_total

Total memory bytes saved per namespace.

| Label | Description |
|-------|-------------|
| `namespace` | Namespace |

### kube_rightsize_savings_estimated_monthly_dollars

Estimated monthly cost savings in USD per namespace, computed from configured
or default pricing ($0.031/vCPU-hour, $0.004/GiB-hour).

| Label | Description |
|-------|-------------|
| `namespace` | Namespace |

### kube_rightsize_confidence

Recommendation confidence score (0-1) per workload container.

| Label | Description |
|-------|-------------|
| `namespace` | Workload namespace |
| `workload` | Workload name |
| `container` | Container name |

## Histograms

### kube_rightsize_resize_duration_seconds

Duration of individual pod resize operations.

| Label | Description |
|-------|-------------|
| `namespace` | Workload namespace |
| `workload` | Workload name |

### kube_rightsize_reconcile_duration_seconds

Duration of each reconciliation loop.

| Label | Description |
|-------|-------------|
| `controller` | Controller name |

### kube_rightsize_reconcile_errors_total

Total number of reconciliation errors by type.

| Label | Description |
|-------|-------------|
| `error_type` | `fetch`, `discover_workloads`, or `status_update` |

### kube_rightsize_prometheus_query_duration_seconds

Duration of each Prometheus query.

| Label | Values |
|-------|--------|
| `query_type` | `cpu_range`, `memory_range`, `cpu_range_fallback`, `memory_fallback` |

## Example PromQL queries

Total successful resizes in the last 24 hours:

```promql
sum(increase(kube_rightsize_resize_total{result="Success"}[24h]))
```

Revert rate as a percentage:

```promql
sum(rate(kube_rightsize_reverts_total[1h]))
/
sum(rate(kube_rightsize_resize_total[1h]))
* 100
```

Total CPU cores saved cluster-wide:

```promql
sum(kube_rightsize_savings_cpu_cores_total)
```

Low-confidence recommendations (below 0.5):

```promql
kube_rightsize_confidence < 0.5
```

P99 reconciliation latency:

```promql
histogram_quantile(0.99, rate(kube_rightsize_reconcile_duration_seconds_bucket[5m]))
```

Prometheus query error rate:

```promql
rate(kube_rightsize_prometheus_query_errors_total[5m])
```
