All metrics are exposed on the operator's metrics endpoint (default port
8080) and use the `attune_` prefix.

## Counters

### attune_resize_total

Total number of in-place resize operations performed.

| Label | Description |
|-------|-------------|
| `namespace` | Workload namespace |
| `workload` | Workload name |
| `resource` | `cpu` or `memory` |
| `result` | `success`, `failed`, or `reverted` |

### attune_reverts_total

Total number of resize reverts triggered by the safety monitor.

| Label | Description |
|-------|-------------|
| `namespace` | Workload namespace |
| `workload` | Workload name |
| `reason` | `oomkill`, `throttle`, `restart`, `notready`, `slo:<name>`, `re-fetch-failed`, or `annotation-persist-failed` |

### attune_revert_failures_total

Total number of failed resize revert attempts. A non-zero value means the
operator tried to restore a pod's original resources but the `/resize`
subresource call failed, leaving the pod running with post-resize resources
that may be causing issues.

| Label | Description |
|-------|-------------|
| `namespace` | Workload namespace |
| `workload` | Workload name |
| `reason` | Same reason labels as `attune_reverts_total` |

```promql
# Alert when reverts are failing
sum by (namespace, workload) (rate(attune_revert_failures_total[5m])) > 0
```

### attune_template_patch_total

Total number of workload pod template resource patches (template persistence).

| Label | Description |
|-------|-------------|
| `namespace` | Policy namespace |
| `workload` | Workload name |
| `result` | `success` or `failed` |

```promql
sum by (namespace) (rate(attune_template_patch_total{result="failed"}[15m])) > 0
```


### attune_prometheus_query_errors_total

Total number of failed Prometheus queries.

| Label | Description |
|-------|-------------|
| `namespace` | Workload namespace where the query originated |
| `query_type` | `cpu_grouped` or `memory_grouped` |

### attune_reconcile_errors_total

Total number of reconciliation errors by type.

| Label | Description |
|-------|-------------|
| `error_type` | `fetch`, `fetch_defaults`, `prometheus_config`, `collector_options`, `collector_create`, `discover_workloads`, `get_pods`, `compute_recommendations`, `status_update`, or `safety_observation` |

### attune_webhook_validation_total

Total number of webhook admission decisions.

| Label | Description |
|-------|-------------|
| `operation` | `validate_create`, `validate_update`, `defaulting`, `defaults_validate_create`, `defaults_validate_update`, `namespace_defaults_validate_create`, or `namespace_defaults_validate_update` |
| `result` | `allowed` or `rejected` |

### attune_schedule_skipped_total

Total resize cycles skipped because the current time is outside the
configured schedule window.

| Label | Description |
|-------|-------------|
| `namespace` | Policy namespace |
| `policy` | Policy name |

### attune_budget_exhausted_total

Total resize operations deferred because the per-cycle budget cap
(`maxTotalCpuIncrease` / `maxTotalMemoryIncrease`) was exhausted.

| Label | Description |
|-------|-------------|
| `namespace` | Policy namespace |
| `policy` | Policy name |

### attune_eviction_total

Total eviction attempts when `resizeMethod: InPlaceOrRecreate` falls back
to pod eviction after an in-place resize fails or is marked Infeasible.

| Label | Description |
|-------|-------------|
| `namespace` | Workload namespace |
| `workload` | Workload name |
| `result` | `success` or `denied` |

### attune_throttle_deferred_total

Total number of throttle safety checks deferred because the Prometheus rate
window grace period has not elapsed. Incremented when a pod's observation
period is shorter than 5 minutes, meaning there is not yet enough data for
a reliable throttle check.

| Label | Description |
|-------|-------------|
| `namespace` | Workload namespace |
| `workload` | Workload name |

### attune_startup_boost_total

Total startup boost lifecycle events (boost applied, expired, or failed).

| Label | Description |
|-------|-------------|
| `namespace` | Workload namespace |
| `workload` | Workload name |
| `action` | `applied`, `expired`, or `failed` |

### attune_infeasible_skipped_total

Total pods skipped because kubelet marked the in-place resize as
Infeasible and `resizeMethod` is `InPlaceOnly` (no eviction fallback).

| Label | Description |
|-------|-------------|
| `namespace` | Workload namespace |
| `workload` | Workload name |

### attune_stale_recommendations_total

Total times recommendations were marked stale due to Prometheus data gaps.

| Label | Description |
|-------|-------------|
| `namespace` | Policy namespace |
| `policy` | Policy name |

### attune_request_clamped_total

Total times a recommended resource request was capped at the container's
current limit. Fires when `controlledValues` is `RequestsOnly` and the
recommendation exceeds the limit. See [Troubleshooting: Requests clamped
to limits](../guides/troubleshooting.md#requests-clamped-to-limits).

| Label | Description |
|-------|-------------|
| `namespace` | Policy namespace |
| `policy` | Policy name |
| `container` | Container name |
| `resource` | `cpu` or `memory` |

### attune_nan_inf_samples_total

Total times all Prometheus samples for a container metric were non-finite
(NaN or Inf), making the metric unusable for recommendations. See
[Troubleshooting: NaN or Inf values](../guides/troubleshooting.md#nan-or-inf-values-in-prometheus-data).

| Label | Description |
|-------|-------------|
| `namespace` | Policy namespace |
| `policy` | Policy name |
| `container` | Container name |
| `metric_type` | `cpu` or `memory` |

## Gauges

### attune_recommendation_cpu_cores

Recommended CPU cores for each workload container.

| Label | Description |
|-------|-------------|
| `namespace` | Workload namespace |
| `workload` | Workload name |
| `container` | Container name |

### attune_recommendation_memory_bytes

Recommended memory (bytes) for each workload container.

| Label | Description |
|-------|-------------|
| `namespace` | Workload namespace |
| `workload` | Workload name |
| `container` | Container name |

### attune_savings_cpu_cores_total

Total CPU cores saved per namespace.

| Label | Description |
|-------|-------------|
| `namespace` | Namespace |

### attune_savings_memory_bytes_total

Total memory bytes saved per namespace.

| Label | Description |
|-------|-------------|
| `namespace` | Namespace |

### attune_savings_estimated_monthly_dollars

Estimated monthly cost savings in USD per namespace, computed from configured
or default pricing ($0.031/vCPU-hour, $0.004/GiB-hour).

| Label | Description |
|-------|-------------|
| `namespace` | Namespace |

### attune_confidence

Recommendation confidence score (0-1) per workload container.

| Label | Description |
|-------|-------------|
| `namespace` | Workload namespace |
| `workload` | Workload name |
| `container` | Container name |

### attune_burst_factor

Burst detection multiplier applied to recommendations. A value of 1.0 means
no burst detected; values above 1.0 indicate the recommendation was inflated
to accommodate a detected usage burst.

| Label | Description |
|-------|-------------|
| `namespace` | Workload namespace |
| `workload` | Workload name |
| `container` | Container name |
| `resource` | `cpu` or `memory` |

## Histograms

### attune_resize_duration_seconds

Duration of individual pod resize operations.

| Label | Description |
|-------|-------------|
| `namespace` | Workload namespace |
| `workload` | Workload name |

### attune_reconcile_duration_seconds

Duration of each reconciliation loop.

| Label | Description |
|-------|-------------|
| `controller` | Controller name |
| `namespace` | Policy namespace |
| `policy` | Policy name |

### attune_prometheus_query_duration_seconds

Duration of each Prometheus query.

| Label | Description |
|-------|-------------|
| `query_type` | `cpu_grouped` or `memory_grouped` |

### attune_webhook_duration_seconds

Duration of webhook validation and defaulting operations.

| Label | Description |
|-------|-------------|
| `operation` | `validate_create`, `validate_update`, `defaulting`, `defaults_validate_create`, `defaults_validate_update`, `namespace_defaults_validate_create`, or `namespace_defaults_validate_update` |

## Controller-runtime Workqueue Metrics

These metrics are auto-registered by controller-runtime for the
`AttunePolicy` reconciler workqueue. They are critical for diagnosing
reconcile backlog and throughput at scale.

### workqueue_depth

Current depth of the reconcile workqueue (number of items waiting to be
processed).

| Label | Description |
|-------|-------------|
| `name` | Queue name (e.g. `attunepolicy`) |

### workqueue_adds_total

Total number of items added to the workqueue.

| Label | Description |
|-------|-------------|
| `name` | Queue name |

### workqueue_queue_duration_seconds

Time an item spends waiting in the queue before being processed (histogram).

| Label | Description |
|-------|-------------|
| `name` | Queue name |

### workqueue_work_duration_seconds

Time spent processing an item from the queue (histogram).

| Label | Description |
|-------|-------------|
| `name` | Queue name |

### workqueue_retries_total

Total number of item retries (requeue after error).

| Label | Description |
|-------|-------------|
| `name` | Queue name |

### workqueue_longest_running_processor_seconds

Duration of the longest currently running processor (gauge). A sustained
high value indicates a stuck or very slow reconciliation.

| Label | Description |
|-------|-------------|
| `name` | Queue name |

### workqueue_unfinished_work_seconds

Time that unfinished work has been in progress (gauge). Complements
`longest_running_processor_seconds` by measuring aggregate backlog age.

| Label | Description |
|-------|-------------|
| `name` | Queue name |

## Example PromQL queries

Total successful in-place resizes in the last 24 hours:

```promql
sum(increase(attune_resize_total{result="success"}[24h]))
```

Total successful eviction fallbacks in the last 24 hours:

```promql
sum(increase(attune_eviction_total{result="success"}[24h]))
```

Revert rate as a percentage of successful in-place resizes:

```promql
sum(rate(attune_reverts_total[1h]))
/
sum(rate(attune_resize_total{result="success"}[1h]))
* 100
```

Total CPU cores saved cluster-wide:

```promql
sum(attune_savings_cpu_cores_total)
```

Low-confidence recommendations (below 0.5):

```promql
attune_confidence < 0.5
```

P99 reconciliation latency:

```promql
histogram_quantile(0.99, rate(attune_reconcile_duration_seconds_bucket[5m]))
```

Prometheus query error rate:

```promql
sum(rate(attune_prometheus_query_errors_total[5m]))
```

Reconcile queue depth (backlog indicator):

```promql
workqueue_depth{name="attunepolicy"}
```

Average time items wait in the queue before processing:

```promql
histogram_quantile(0.99, rate(workqueue_queue_duration_seconds_bucket{name="attunepolicy"}[5m]))
```

Reconcile enqueue rate (items added to queue per second):

```promql
rate(workqueue_adds_total{name="attunepolicy"}[5m])
```

Containers with persistent NaN/Inf data quality issues:

```promql
rate(attune_nan_inf_samples_total[1h]) > 0
```

Policies where requests are frequently clamped to limits:

```promql
sum by (namespace, policy) (rate(attune_request_clamped_total[1h])) > 0
```
