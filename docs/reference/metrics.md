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
| `reason` | `oomkill`, `throttle`, `restart`, `notready`, `slo:<name>`, `re-fetch-failed`, or `annotation-persist-failed` (write did not land; a timeout after a committed persist is treated as success and does not revert) |

### attune_revert_failures_total

Total number of failed resize revert attempts. A non-zero value means the
operator tried to restore a pod's original resources but the `/resize`
subresource call failed (immediate apply-path revert or safety
observation revert), leaving the pod running with post-resize resources
that may be causing issues.

A zero value does not mean a safety restore finished. Template restore
after a successful in-place revert is a separate path: it logs
`Failed to restore template after safety revert` and does not increment
this counter. The pod is already back at original resources; the
template may still hold the persisted size until the next retry.

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
| `error_type` | `fetch`, `fetch_defaults`, `metrics_source`, `discover_workloads`, `list_policies`, `list_hpas`, `list_vpas`, `get_pods`, `compute_recommendations`, `status_update`, or `safety_observation` |

`safety_observation` means a pending-observation list, confirm Get, safety
check, or tracking-annotation cleanup failed. Tracking stays on the pod
and the next reconcile retries.

### attune_webhook_validation_total

Total number of webhook admission decisions.

| Label | Description |
|-------|-------------|
| `operation` | `validate_create`, `validate_update`, `defaulting`, `defaults_validate_create`, `defaults_validate_update`, `namespace_defaults_validate_create`, `namespace_defaults_validate_update`, or `pod-initial-sizing` |
| `result` | `allowed` or `rejected` |

### attune_schedule_skipped_total

Total resize cycles skipped because the current time is outside the
configured schedule window. A window whose start and end are the same
minute is also counted here, because that window never opens.

| Label | Description |
|-------|-------------|
| `namespace` | Policy namespace |
| `policy` | Policy name |

### attune_budget_exhausted_total

Total resize operations the increase budget refused. The counter covers
two events, which the policy events tell apart (`kubectl describe
attunepolicy <name>`):

- `BudgetExhausted`: the increase fits every configured cap, but not the
  budget left this cycle (`maxTotalCpuIncrease` / `maxTotalMemoryIncrease`)
  or this minute (`maxCpuIncreasePerMinute` / `maxMemoryIncreasePerMinute`).
  The resize is deferred, and a later cycle can run it.
- `IncreaseExceedsBudget`: one container's increase is larger than
  `maxCpuIncreasePerMinute`, `maxMemoryIncreasePerMinute`,
  `maxTotalCpuIncrease`, or `maxTotalMemoryIncrease`. Waiting does not help,
  and the counter keeps rising on every reconcile while that recommendation
  stands. Raise that cap or lower the target.

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
| `result` | `success`, `denied`, `last_replica`, `list_failed`, or `no_selector` |

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

### attune_pods_deferred

Gauge: pods currently Deferred by the kubelet for in-place resize
(`PodResizePending` reason `Deferred`). Updated each reconcile from
policy status `workloads.deferred`.

| Label | Description |
|-------|-------------|
| `namespace` | Policy namespace |
| `policy` | Policy name |

Example alert when deferred pods linger:

```promql
attune_pods_deferred > 0
```

### attune_pods_infeasible

Gauge: pods currently marked Infeasible for in-place resize on their node.
Updated each reconcile from policy status `workloads.infeasible`.

| Label | Description |
|-------|-------------|
| `namespace` | Policy namespace |
| `policy` | Policy name |

### attune_deferred_age_seconds

Histogram of Deferred condition age (seconds) when a deferred pod is
observed during reconcile and `LastTransitionTime` is set.

| Label | Description |
|-------|-------------|
| `namespace` | Policy namespace |
| `policy` | Policy name |

```promql
histogram_quantile(0.95, sum by (le, namespace, policy) (rate(attune_deferred_age_seconds_bucket[15m])))
```

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

### attune_fleet_report_export_total

Outcomes of optional per-cluster **fleet summary** ConfigMap export
(`--fleet-report-enabled`). Used by multi-cluster collectors.

| Label | Description |
|-------|-------------|
| `result` | `success` or `failed` |

```promql
sum(rate(attune_fleet_report_export_total{result="failed"}[5m]))
```

See [Fleet observability](../guides/multi-cluster.md#fleet-observability-with-federated-prometheus).

### attune_gitops_pr_total

Opt-in GitOps pull request automation outcomes (Phase B). Default-off feature
under `updateStrategy.export.pullRequest`.

| Label | Description |
|-------|-------------|
| `namespace` | Policy namespace |
| `policy` | Policy name |
| `result` | `created`, `updated`, `dry_run`, or `failed` |

```promql
sum by (namespace, policy, result) (rate(attune_gitops_pr_total[1h]))
```

Failures (also used by the opt-in `AttuneGitOpsPRFailures` PrometheusRule when
`metrics.prometheusRule` is enabled):

```promql
sum by (namespace, policy) (increase(attune_gitops_pr_total{result="failed"}[15m])) > 0
```

See [GitOps integration: pull request automation](../guides/gitops-integration.md#pull-request-automation-opt-in-phase-b)
and [Troubleshooting: GitOps PR failing](../guides/troubleshooting.md#gitops-pr-failing).

### attune_memory_limit_decrease_total

Outcomes of memory **limit** decrease attempts (Kubernetes 1.34+ live
decrease path and platform clamps on older clusters).

| Label | Description |
|-------|-------------|
| `namespace` | Policy namespace |
| `policy` | Policy name |
| `result` | `applied`, `clamped_platform`, `clamped_usage`, or `skipped_unsafe` |

| `result` | Meaning |
|----------|---------|
| `applied` | In-place resize applied a lower memory limit |
| `clamped_platform` | Limit decrease blocked because cluster rejects NotRequired decreases (Kubernetes 1.33 and earlier) |
| `clamped_usage` | Target limit raised above recent usage + `decreaseUsageMarginPercent` |
| `skipped_unsafe` | Usage floor equaled the current limit (no safe decrease) |

```promql
# Unsafe or floored memory limit decreases
sum by (namespace, policy, result) (
  rate(attune_memory_limit_decrease_total{result=~"clamped_usage|skipped_unsafe"}[1h])
)
```

Alert sketch (opt-in `AttuneMemoryLimitUnsafe` when `metrics.prometheusRule` is
enabled):

```promql
sum by (namespace, policy) (
  rate(attune_memory_limit_decrease_total{result=~"clamped_usage|skipped_unsafe"}[1h])
) > 0
```

See [Troubleshooting: OOM after memory limit decrease](../guides/troubleshooting.md#oom-after-memory-limit-decrease).

### attune_oom_bump_total

Memory request steps after `OOMKilled` when `memory.oomBump` is set. The feature is off until that block is set. An empty `oomBump: {}` turns it on. The counter increments for a planned outcome. `applied` is recorded only after the resize succeeds, which is also when the annotation count increments. A new OOM step that the byte math labels `clamped` is also recorded only after that resize. A quiet reclamp, a held floor lowered to `maxAllowed` with no new OOM, counts `clamped` once per distinct floor and cap in one operator process. That includes Recommend and Observe, and it includes a clamp that `allowDecrease` then keeps off the live request. A later reconcile with the same floor and cap does not count again. A restart counts that pair once more. `capped` and `skipped` are recorded without a resize.

| Label | Description |
|-------|-------------|
| `namespace` | Policy namespace |
| `policy` | Policy name |
| `result` | `applied`, `clamped`, `capped`, or `skipped` |

| `result` | Meaning |
|----------|---------|
| `applied` | In-place resize applied the next step from the original request |
| `clamped` | The step was above `maxAllowed`, so the request was clamped to that cap. A new OOM step counts after the resize succeeds. A container already at `maxAllowed`, outside a hold, counts one `clamped` sample per OOM and the request does not move. Recommend and Observe count that sample once. A held floor lowered to a `maxAllowed` that was added or reduced counts once per floor and cap per process, even when the live request does not move. A percentile with no in-hold floor is not counted |
| `capped` | `maxBumps` is already reached. When `maxAllowed` is omitted, `maxBumps` is the only cap |
| `skipped` | No bump was applied. An excluded container, unresolved config, or an in-hold OOM whose `maxAllowed` is already at or below the live request is stored once in Auto, OneShot, and Canary, so those modes count that signal once. Recommend and Observe do not write that in-hold stamp, so they count the same OOM on every reconcile. An OOM already at `maxAllowed` outside a hold is `clamped`, not `skipped`. A budget skip, or a Guaranteed pod with `RequestsOnly`, is counted on each reconcile that still sees it, including in Auto, OneShot, and Canary. A newer finish time or restart counts again. A container name that does not fit the annotation key is logged and not counted |

```promql
sum by (namespace, policy, result) (rate(attune_oom_bump_total[1h]))
```

Opt-in `AttuneOOMBumpCapped` (info, pending 5m) uses `increase` so one
`capped` or `clamped` sample in the hour is enough. It does not mean the
bump failed to apply. The PrometheusRule object stays off until
`metrics.prometheusRule.enabled` is true, and the alert does not turn
`memory.oomBump` on.

```promql
sum by (namespace, policy) (increase(attune_oom_bump_total{result=~"capped|clamped"}[1h])) > 0
```

See [Troubleshooting: Memory request rose after OOMKilled](../guides/troubleshooting.md#memory-request-rose-after-oomkilled).

### attune_nan_inf_samples_total

Total times every sample in a series was unusable, so that series could
not feed a recommendation. Prometheus counts NaN and Inf. Datadog also
counts a series whose points are all JSON null or missing. A numeric zero is a real
sample and is not counted. See
[Troubleshooting: NaN or Inf values](../guides/troubleshooting.md#nan-or-inf-values-in-prometheus-data).

| Label | Description |
|-------|-------------|
| `namespace` | Policy namespace |
| `policy` | Policy name |
| `container` | Container name. Collector-dropped NaN/Inf use `container=untracked` so scrape cardinality stays bounded. |
| `metric_type` | `cpu` or `memory` |

### attune_capacity_skip_total

Resize attempts skipped because of node **allocatable** headroom, node
**neighbor request budget**, node **pressure** conditions, or
**unavailable** node status when applying a request increase (always-on
safety gates). See
[Node capacity formulas](../architecture/node-capacity.md) and
[Troubleshooting: Resize skipped for capacity](../guides/troubleshooting.md#resize-skipped-for-node-capacity-or-pressure).

| Label | Description |
|-------|-------------|
| `namespace` | Policy namespace |
| `policy` | Policy name |
| `reason` | `allocatable`, `neighbors`, `pressure`, or `unavailable` |

```promql
sum by (namespace, policy, reason) (
  rate(attune_capacity_skip_total[1h])
)
```

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

### attune_reclaimed_request_cpu_cores

Estimated freeable CPU request cores for a policy if recommended decreases
were applied. Bin-packing / cluster-autoscaler capacity planning signal.
Same quantity as savings CPU reduction, labeled by policy.

| Label | Description |
|-------|-------------|
| `namespace` | Policy namespace |
| `policy` | Policy name |

### attune_reclaimed_request_memory_bytes

Estimated freeable memory request bytes for a policy if recommended decreases
were applied.

| Label | Description |
|-------|-------------|
| `namespace` | Policy namespace |
| `policy` | Policy name |

```promql
sum by (namespace, policy) (attune_reclaimed_request_cpu_cores)
sum by (namespace, policy) (attune_reclaimed_request_memory_bytes)
```

See [Bin packing](../guides/bin-packing.md#reclaimed-capacity-signals).

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
| `operation` | `validate_create`, `validate_update`, `defaulting`, `defaults_validate_create`, `defaults_validate_update`, `namespace_defaults_validate_create`, `namespace_defaults_validate_update`, or `pod-initial-sizing` |

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
