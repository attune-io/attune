# Scaling Guide

This guide covers how to size kube-rightsize for your cluster, from small
dev environments to large production deployments with thousands of workloads.

## Architecture Overview

kube-rightsize runs as a single-leader Deployment. One replica performs all
reconciliation work while the standby (in HA mode) waits to take over via
leader election. The operator's main scaling dimensions are:

1. **Prometheus query rate** - how fast it can fetch metrics for each policy
2. **Memory** - proportional to the number of watched pods and their metric history
3. **API server pressure** - listing pods, patching resize subresources, updating status

## Quick Start

The fastest way to configure for your cluster size:

```bash
# Option A: one-liner with clusterSize preset
helm install kube-rightsize ./charts/kube-rightsize --set clusterSize=large

# Option B: use an example values file for full control
helm install kube-rightsize ./charts/kube-rightsize -f charts/kube-rightsize/examples/values-large.yaml
```

## Cluster Size Presets

The `clusterSize` value sets multiple parameters at once. Any explicitly set
value always overrides the preset.

### Preset Reference

| Setting | small | medium | large | xlarge |
|---------|-------|--------|-------|--------|
| **Target workloads** | up to ~100 | ~100-500 | ~500-5,000 | 5,000+ |
| `resources.requests.cpu` | 100m | 250m | 500m | 1000m |
| `resources.requests.memory` | 128Mi | 256Mi | 512Mi | 1Gi |
| `resources.limits.cpu` | 500m | 1000m | 2000m | 4000m |
| `resources.limits.memory` | 256Mi | 512Mi | 2Gi | 4Gi |
| `prometheusQPS` | 10 | 20 | 40 | 80 |
| `prometheusBurst` | 20 | 40 | 80 | 160 |
| `maxConcurrentReconciles` | 1 | 2 | 4 | 8 |
| `replicaCount` | 1 | 1 | 2 | 2 |

The "workloads" count means the number of Deployments, StatefulSets, and
DaemonSets targeted by RightSizePolicy resources, not total pods.

### Override Behavior

Explicit values always win. For example, to use `large` resources but
a custom QPS:

```yaml
clusterSize: large
prometheusQPS: 60  # overrides the large preset's 40
```

When `clusterSize` is empty (the default), no preset is applied. Resources
default to the `small` tier to ensure the operator always has resource
requests set.

### Recommended CRD Settings by Cluster Size

These settings are configured on your `RightSizePolicy` or
`RightSizeDefaults` CRDs, not in the Helm chart. They affect per-policy
reconciliation behavior.

| Setting | small | medium | large | xlarge |
|---------|-------|--------|-------|--------|
| `cooldown` | 1h | 1h | 2h | 4h |
| `historyWindow` | 168h (7d) | 168h (7d) | 72h (3d) | 48h (2d) |
| `queryStep` | 5m | 5m | 15m | 30m |
| `maxConcurrentResizes` | 1 | 3 | 10 | 20 |

**Why reduce `historyWindow` at scale?** Each Prometheus query fetches data
for the entire window. At 5,000+ workloads, a 7-day window produces very
large responses. Reducing to 48-72h keeps query latency manageable while
still capturing weekly patterns (the operator uses hourly P95 aggregation
internally).

## Bottleneck Guide

### What breaks first

1. **Reconcile throughput** (most common at scale). By default the controller
   processes one RightSizePolicy at a time. With hundreds of policies, the
   work queue grows and recommendations become stale. Symptom:
   `workqueue_depth` is consistently > 0,
   `workqueue_longest_running_processor_seconds` climbs. Fix: increase
   `maxConcurrentReconciles` (or set a `clusterSize` preset). The Prometheus
   rate limiter is shared across all goroutines, so concurrent reconciles
   won't overwhelm Prometheus.

2. **Prometheus query rate**. Symptom: reconcile queue grows,
   `kube_rightsize_reconcile_duration_seconds` P99 increases. Fix: increase
   `prometheusQPS` and `prometheusBurst`. This works in tandem with
   `maxConcurrentReconciles`: more goroutines can issue queries in parallel,
   but they share the same QPS budget.

3. **Operator memory**. Symptom: OOMKilled pods. Fix: increase
   `resources.limits.memory`. Memory usage is roughly proportional to the
   total number of pods across all targeted workloads.

4. **Prometheus server load**. Symptom: slow or timed-out Prometheus
   queries, high memory on Prometheus itself. Fix: reduce `historyWindow`
   and increase `queryStep` on the CRDs. Consider Prometheus recording
   rules or Thanos query federation.

5. **API server pressure**. Symptom: throttled API requests, slow pod list
   responses. Fix: this is rarely the bottleneck since the operator uses
   informer caches. If you see it, check that your API server is
   appropriately sized.

### Diagnosing with metrics

The operator exposes metrics on the `/metrics` endpoint:

| Metric | What it tells you |
|--------|-------------------|
| `kube_rightsize_reconcile_duration_seconds` | How long each policy reconcile takes. P99 > 30s means queries are slow. |
| `kube_rightsize_reconcile_total` | Total reconciles. Compare with error count. |
| `kube_rightsize_reconcile_errors_total` | Errors per policy. Prometheus timeouts show here. |
| `kube_rightsize_resize_total` | Actual resizes performed. |
| `kube_rightsize_resize_reverted_total` | Reverted resizes (safety mechanism). |
| `workqueue_depth` | Controller work queue depth. Consistently > 0 means the operator can't keep up. |
| `workqueue_longest_running_processor_seconds` | Longest in-flight reconcile. |

### Prometheus sizing for kube-rightsize

Each RightSizePolicy generates 2-4 Prometheus queries per reconcile cycle
(CPU usage, memory usage, OOM events, restart events). At steady state with
a 1-hour cooldown:

| Workloads | Queries/hour | Prometheus impact |
|-----------|-------------|-------------------|
| 50 | ~200 | Negligible |
| 500 | ~2,000 | Low (< 1% of a typical Prometheus) |
| 5,000 | ~20,000 | Moderate (ensure Prometheus has 4+ cores, 8Gi+ memory) |
| 10,000+ | ~40,000+ | Significant (use recording rules or Thanos) |

## HA Deployment

For production clusters, run two replicas with leader election:

```yaml
replicaCount: 2
leaderElection:
  enabled: true
priorityClassName: system-cluster-critical
```

The `large` and `xlarge` presets automatically set `replicaCount: 2`.
Only one replica is active at a time; the standby takes over in ~15 seconds
if the leader fails.

Consider adding a `PodDisruptionBudget` (the chart creates one when
`replicaCount > 1`) and `topologySpreadConstraints` to spread replicas
across nodes:

```yaml
topologySpreadConstraints:
  - maxSkew: 1
    topologyKey: kubernetes.io/hostname
    whenUnsatisfiable: DoNotSchedule
    labelSelector:
      matchLabels:
        app.kubernetes.io/name: kube-rightsize
```

## Monitoring the Operator

Enable the ServiceMonitor for Prometheus Operator integration:

```yaml
metrics:
  serviceMonitor:
    enabled: true
    additionalLabels:
      release: prometheus   # match your Prometheus Operator selector
```

Enable the Grafana dashboard for an at-a-glance view:

```yaml
grafanaDashboard:
  enabled: true
```

The dashboard shows reconcile latency, queue depth, resize activity,
and resource usage in a single pane.
