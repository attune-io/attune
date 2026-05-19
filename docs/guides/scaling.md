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

   Within each policy, workloads are processed in parallel (up to 10
   concurrent workers). This means a single policy targeting 200 Deployments
   via label selector issues Prometheus queries concurrently instead of
   serially, reducing recommendation latency from minutes to seconds. The
   worker count is fixed; actual throughput is bounded by the Prometheus
   QPS rate limiter, not goroutine count.

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
   informer caches. See [API Server Pressure](#api-server-pressure) below
   for details.

6. **Informer cache memory**. Symptom: operator memory grows linearly with
   cluster size even for namespaces without policies. Fix: use
   `watchNamespaces` to limit the operator to only the namespaces that
   have RightSizePolicy resources. See
   [Namespace Scoping](#namespace-scoping) below.

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

## API Server Pressure

### Client-side rate limiting is disabled by default

The operator uses controller-runtime v0.24.x, which sets the Kubernetes
client's QPS to `-1` (disabled). This means there is **no client-side
rate limiting**. All API server throttling is handled by Kubernetes
[API Priority and Fairness](https://kubernetes.io/docs/concepts/cluster-administration/flow-control/)
(APF), which is GA since Kubernetes 1.29.

This is intentional and matches the direction of the ecosystem. Operators
like cert-manager explicitly recommend disabling client-side limiting on
clusters with APF enabled.

### Per-reconcile API call budget

Most reads go through the informer cache (zero API calls). Writes happen
only during resize phases:

| Phase | API calls | Notes |
|-------|-----------|-------|
| Read-only (cached) | 0 | Get/List via informer cache |
| Status update | 1-2 per policy | Direct write to status subresource |
| Per pod resize | ~5-6 calls | UpdateResize + annotation persist + re-fetch |
| Safety observation | 1-2 per tracked pod | Direct get + update |

At steady state (Recommend mode, 1-hour cooldown), a cluster with 500
policies generates roughly 500-1000 API writes per hour, which is
negligible for any production API server.

### Cloud provider APF limits

| Provider | Mutating inflight | Total inflight | Notes |
|----------|-------------------|----------------|-------|
| AKS | 200 (standard) / 50 (free) | 600 / 150 | Scales with SKU tier |
| GKE | 200 | 600 | Control plane auto-scales |
| EKS | 200 | 600 | Multiple HA API servers |

The operator lands in the `global-default` APF priority level unless you
create a custom `FlowSchema`. For most deployments, the default allocation
is more than sufficient.

### When to add a custom FlowSchema

If the operator is deployed outside `kube-system` and you want guaranteed
API server capacity, create a `FlowSchema` that assigns its service account
to a dedicated priority level:

```yaml
apiVersion: flowcontrol.apiserver.k8s.io/v1
kind: FlowSchema
metadata:
  name: kube-rightsize
spec:
  priorityLevelConfiguration:
    name: workload-high
  matchingPrecedence: 1000
  rules:
    - subjects:
        - kind: ServiceAccount
          serviceAccount:
            name: kube-rightsize
            namespace: kube-rightsize-system
      resourceRules:
        - verbs: ["*"]
          apiGroups: ["*"]
          resources: ["*"]
```

## Namespace Scoping

By default, the operator watches all namespaces for RightSizePolicy
resources. On large clusters (10,000+ namespaces) where policies exist in
only a few namespaces, this wastes informer cache memory watching
namespaces that will never have policies.

Set `watchNamespaces` to limit the operator to specific namespaces:

```yaml
watchNamespaces:
  - production
  - staging
  - team-alpha
```

Or via CLI flag:

```bash
--watch-namespaces=production,staging,team-alpha
```

**Behavior:**

- When empty (default): watches all namespaces (cluster-scoped)
- When set: only watches the listed namespaces for namespace-scoped
  resources (Pods, Deployments, HPAs, RightSizePolicies, etc.)
- Cluster-scoped resources (Nodes, RightSizeDefaults) are always watched
  regardless of this setting
- Requires a restart to change the namespace list

**Memory impact:** On a 10,000-namespace cluster with policies in 50
namespaces, setting `watchNamespaces` reduces informer cache memory by
roughly 99% for namespace-scoped resources (Pods, Deployments, HPAs).

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
