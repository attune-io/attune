# Scaling Guide

This guide covers how to size Attune for your cluster, from small
dev environments to large production deployments with thousands of workloads.

## Architecture Overview

Attune runs as a single-leader Deployment. One replica performs all
reconciliation work while the standby (in HA mode) waits to take over via
leader election. The operator's main scaling dimensions are:

1. **Prometheus query payload** - range queries return time series for matching
   pods; cost grows with replica count, history window, and step size
2. **Prometheus query rate** - how many queries the operator issues per second
3. **Memory** - proportional to watched pods (informer cache) and in-memory
   metric samples during recommendation
4. **API server pressure** - listing pods, patching resize subresources, updating status

## Ops checklist

Do these first on any production or multi-tenant cluster. Details and tables
follow below.

1. **Pick a Helm size preset** that matches how many workloads you target:
   `--set clusterSize=large` (or `xlarge`). See [Cluster Size Presets](#cluster-size-presets).
2. **Scope the informer cache** with `watchNamespaces` to only namespaces that
   have AttunePolicy resources. See [Namespace Scoping](#namespace-scoping).
3. **Apply CRD defaults for scale** via `AttuneDefaults` (or each policy):
   shorter `historyWindow`, larger `queryStep`, longer `cooldown` as in the
   [Recommended CRD Settings](#recommended-crd-settings-by-cluster-size) table.
4. **Treat high-replica Deployments as a first-class cost**: a few Deployments
   with hundreds of pods can cost more in Prometheus traffic than hundreds of
   single-replica apps. See [Large Deployments](#large-deployments-high-replica-counts).
5. **Scope policies deliberately** (named target or tight selectors). Avoid one
   policy matching thousands of Deployments unless you accept larger status and
   longer reconciles. See [Policy design at scale](#policy-design-at-scale).
6. **Watch operator metrics**: `workqueue_depth`,
   `attune_reconcile_duration_seconds`, Prometheus query errors. See
   [Diagnosing with metrics](#diagnosing-with-metrics).

## Quick Start

The fastest way to configure for your cluster size:

```bash
# Option A: one-liner with clusterSize preset
helm install attune ./charts/attune --set clusterSize=large

# Option B: use an example values file for full control
helm install attune ./charts/attune -f charts/attune/examples/values-large.yaml
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
DaemonSets targeted by AttunePolicy resources, **not** total pods. High
replica counts inside a few workloads are a separate dimension (see
[Large Deployments](#large-deployments-high-replica-counts)).

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

These settings are configured on your `AttunePolicy` or
`AttuneDefaults` CRDs, not in the Helm chart. Prefer **`AttuneDefaults`**
(cluster-wide or namespace-scoped) so every policy inherits the same scale
profile without editing each CR.

| Setting | small | medium | large | xlarge |
|---------|-------|--------|-------|--------|
| `cooldown` | 1h | 1h | 2h | 4h |
| `historyWindow` | 168h (7d) | 168h (7d) | 72h (3d) | 48h (2d) |
| `queryStep` | 5m | 5m | 15m | 30m |
| `maxConcurrentResizes` | 1 | 3 | 10 | 20 |

**Why reduce `historyWindow` at scale?** Each Prometheus range query fetches
data for the entire window at `queryStep` resolution. Cost grows with window
size and with the number of time series returned (often one series per pod
container under current query shape). Reducing to 48-72h keeps query latency
and response size manageable while still supporting time-of-day patterns (the
operator buckets samples by hour of day internally).

## Large Deployments (high replica counts)

Query **count** scales with workloads (roughly two range queries per workload
per reconcile for CPU and memory). Query **payload size** scales with how many
pods match the workload's pod name regex.

Default PromQL uses **`max by (container)`** so series count is about the
number of containers, not pods. Set `metricsSource.podAggregation: None` to
restore the legacy path (one series per pod). With legacy `None`, defaults
(`historyWindow: 168h`, `queryStep: 5m`) yield on the order of **~2,000 samples
per series**. Rough order of magnitude for **one** workload, **two** containers,
CPU only, **None** aggregation:

| Ready pods | Approx. series (None) | Approx. series (Max, default) |
|------------|----------------------|-------------------------------|
| 10 | ~20 | ~2 |
| 100 | ~200 | ~2 |
| 500 | ~1,000 | ~2 |

With default Max aggregation, long windows still cost time-series length; use
the CRD window/step table for Prometheus load. With None, one Deployment with
500 pods can still stress Prometheus more than many single-replica apps.

**Ops mitigations (no code change required):**

1. On policies that target high-replica apps, use a shorter `historyWindow`
   and larger `queryStep` than the cluster default (or use the large/xlarge
   CRD table for everything).
2. Prefer **named** `targetRef` for those Deployments rather than a broad
   selector that also pulls in other large fleets on the same policy.
3. Raise `prometheusTimeout` only if queries complete but exceed the default
   5m budget; first reduce query cost (window/step).
4. Add Prometheus recording rules or capacity on the metrics backend if many
   high-replica workloads reconcile often (shorter cooldown, many policies).

See also [Current limitations](#current-limitations-operator-behavior-today)
for what product changes would reduce this further.

## Policy design at scale

- **Named targets** are cheapest to reason about: one policy, one workload,
  predictable status size and reconcile time.
- **Label selectors** are convenient for platform teams. Cost grows with how
  many workloads match **and** how large those workloads are. A single policy
  matching hundreds of Deployments means hundreds of Prometheus query pairs
  per cycle (bounded by the 10 in-policy workers and shared Prom QPS) and a
  large `status.recommendations` list.
- There is no requirement for "one policy per namespace." Use whatever
  grouping matches ownership, but avoid one mega-selector over the entire
  cluster unless you have tuned window/step/cooldown and accept longer
  reconciles.
- **GitOps ConfigMap export** (`updateStrategy.export.configMap`) can hold
  full recommendation detail for automation while you still watch status
  summaries; use when status size or GitOps workflow is a concern.

### Status size

Each reconcile can write `status.recommendations` with per-container current
and recommended resources plus optional explanation chains. Resize history is
capped (50 entries); the recommendations list is not.

If status updates fail with object-size or etcd limit errors, or status
patches become slow under conflict retries:

1. Split the policy so fewer workloads share one CR.
2. Use tighter selectors or named targets.
3. Prefer ConfigMap export for full recommendation payloads when you need
   every field for automation.

## Bottleneck Guide

### What breaks first

1. **Prometheus payload and server load** (common with high-replica workloads).
   Symptom: slow or timed-out range queries, high Prometheus memory/CPU,
   `attune_reconcile_duration_seconds` P99 large,
   `Prometheus query timeout exceeded` on Ready. Fix: reduce `historyWindow`,
   increase `queryStep`, increase `prometheusTimeout` only after cutting cost,
   add recording rules or Prometheus capacity. See
   [Large Deployments](#large-deployments-high-replica-counts).

2. **Reconcile throughput** (common with many policies). By default the
   controller processes one AttunePolicy at a time. With hundreds of policies,
   the work queue grows and recommendations become stale. Symptom:
   `workqueue_depth` is consistently > 0,
   `workqueue_longest_running_processor_seconds` climbs. Fix: increase
   `maxConcurrentReconciles` (or set a `clusterSize` preset). The Prometheus
   rate limiter is shared across all goroutines, so concurrent reconciles
   will not overwhelm Prometheus beyond the configured QPS.

   Within each policy, workloads are processed in parallel (up to 10
   concurrent workers). A single policy targeting 200 Deployments via label
   selector issues Prometheus queries concurrently instead of serially. The
   worker count is fixed; actual throughput is bounded by the Prometheus
   QPS rate limiter, not goroutine count.

3. **Prometheus query rate**. Symptom: reconcile queue grows,
   `attune_reconcile_duration_seconds` P99 increases even when individual
   queries are small. Fix: increase `prometheusQPS` and `prometheusBurst`.
   This works in tandem with `maxConcurrentReconciles`: more goroutines can
   issue queries in parallel, but they share the same QPS budget.

4. **Operator memory**. Symptom: OOMKilled pods. Fix: increase
   `resources.limits.memory`. Memory usage is roughly proportional to the
   total number of pods in **watched** namespaces (informer cache), plus
   temporary sample buffers during recommendation. Pods in the cache are
   field-stripped; full cluster watch still dominates multi-tenant estates.

5. **API server pressure**. Symptom: throttled API requests, slow pod list
   responses. Fix: this is rarely the bottleneck for read-only Recommend
   mode because most reads use the informer cache. Resize waves and status
   updates still hit the API. See [API Server Pressure](#api-server-pressure).

6. **Informer cache memory**. Symptom: operator memory grows linearly with
   cluster size even for namespaces without policies. Fix: use
   `watchNamespaces` to limit the operator to only the namespaces that
   have AttunePolicy resources. See
   [Namespace Scoping](#namespace-scoping) below.

### Diagnosing with metrics

The operator exposes metrics on the `/metrics` endpoint:

| Metric | What it tells you |
|--------|-------------------|
| `attune_reconcile_duration_seconds` | How long each policy reconcile takes. P99 > 30s means queries or workload volume are expensive. |
| `attune_reconcile_duration_seconds_count` | Total reconciles. Compare with error count. |
| `attune_reconcile_errors_total` | Errors per policy. Prometheus timeouts show here. |
| `attune_prometheus_query_duration_seconds` | Per-query latency when exposed; pair with reconcile duration. |
| `attune_resize_total` | Actual in-place resizes performed. |
| `attune_eviction_total` | Eviction fallback attempts when in-place resize is not possible. |
| `attune_reverts_total` | Reverted in-place resizes (safety mechanism). |
| `workqueue_depth` | Controller work queue depth. Consistently > 0 means the operator can't keep up. |
| `workqueue_longest_running_processor_seconds` | Longest in-flight reconcile. |

### Prometheus sizing for Attune

Each AttunePolicy generates on the order of **2 range queries per workload**
per reconcile cycle for recommendations (CPU and memory), plus additional
instant queries for safety (throttle, optional SLO) when resizes are
in flight. At steady state with a 1-hour cooldown:

| Workloads | Queries/hour (order of magnitude) | Prometheus impact |
|-----------|-----------------------------------|-------------------|
| 50 | ~200 | Negligible if pods per workload are modest |
| 500 | ~2,000 | Low for query rate; watch payload if many high-replica apps |
| 5,000 | ~20,000 | Moderate (ensure Prometheus has 4+ cores, 8Gi+ memory) |
| 10,000+ | ~40,000+ | Significant (use recording rules or Thanos) |

These figures estimate **query rate**, not response size. High-replica
workloads can make each query much more expensive; see
[Large Deployments](#large-deployments-high-replica-counts).

## Ready reason: PrometheusSeriesCapped

When a range query returns more series than `--max-prometheus-series`, Attune
keeps a partial result (preferring at least one series per container) and may
set Ready reason `PrometheusSeriesCapped`. Raise the cap, or keep the default
`podAggregation: Max` so series counts stay small.

## Performance features (built-in)

Attune includes several scale controls that reduce Prometheus payload and
operator work. Defaults favor high-replica fleets; override when you need
legacy behavior or richer status.

| Feature | Default | Helm / flag | Policy field |
|---------|---------|-------------|--------------|
| PromQL **max by (container)** aggregation | On (`Max`) | - | `metricsSource.podAggregation`: `Max` / `Avg` / `None` |
| Sample downsampling before percentiles | 10000 samples | `maxProfileSamples` / `--max-profile-samples` | - |
| Prometheus series cap per range query | 5000 | `maxPrometheusSeries` / `--max-prometheus-series` | - |
| Status recommendation cap | 100 | `maxStatusRecommendations` | `updateStrategy.maxStatusRecommendations` |
| Strip status explanations | Off (include) | `statusIncludeExplanations` | `updateStrategy.includeExplanationsInStatus` |
| Workload workers per policy | 10 | `maxWorkloadWorkers` / `--max-workload-workers` | - |
| Requeue jitter | 2m | `requeueJitter` / `--requeue-jitter` | Applied only to full cooldown requeues; skipped during InsufficientData / PrometheusUnavailable |
| Lazy pod lists | Observe skips full lists; Recommend still lists for Deferred/Infeasible UX | - | - |
| Namespace-wide pod list + in-memory match | On | - | - |
| Representative pod sample for metrics | 100 pods | `maxPodsInMetricsQuery` / `--max-pods-in-metrics-query` | - |
| History window operator ceiling | Off (CRD max 720h) | `maxHistoryWindow` / `--max-history-window`; large=`72h`, xlarge=`48h` | - |
| Query step operator floor | Off (CRD min 10s) | `minQueryStep` / `--min-query-step`; large=`10m`, xlarge=`15m` | - |
| Blocker recompute throttle | Off (`0s`; set `5m` for large Recommend fleets) | `blockerRefreshInterval` / `--blocker-refresh-interval` | - |
| Parallel policy reconciles | 2 | `maxConcurrentReconciles` / `--max-concurrent-reconciles`; clusterSize presets 1/2/4/8 | - |
| Informer field strip (Pods + workloads + HPA) | On | - | Write paths use APIReader (live Get) before MergeFrom |
| Pod field strip + optional static selector | Strip always; optional static | `podLabelSelector` / `--pod-label-selector` | Dynamic selectors refreshed for keep diagnostics; no empty Spec stubs |
| Batch safety throttle PromQL | On (when Prometheus collector) | - | - |
| Shared pod List for metrics sample + resize | On when sampling enabled | - | One NS-wide List reused |

### PromQL aggregation

Default queries look like:

```promql
max by (container) (rate(container_cpu_usage_seconds_total{namespace="...",pod=~"..."}[5m]))
```

That keeps the matrix size near **O(containers)** instead of **O(pods)**. Use
`podAggregation: None` only if you need the legacy multi-pod sample pool for
experiments. Prefer **Max** for production rightsizing (size for the busiest pod).

### Recording rules (optional metrics path)

Set both recording metric names to query pre-aggregated rules instead of raw
cadvisor series (no `rate()` wrapper on CPU when `cpuRecordingMetric` is set):

```yaml
spec:
  metricsSource:
    prometheus:
      address: http://prometheus.monitoring.svc:9090
    cpuRecordingMetric: attune:container_cpu:rate5m
    memoryRecordingMetric: attune:container_memory:working_set
    podAggregation: Max   # still applied around the recording metric
```

Recording rules must expose labels `namespace`, `pod`, and `container`.

### Multi-instance namespace sharding

`watchNamespaces` already limits each operator process to a namespace list.
To shard a large cluster:

1. Deploy two (or more) Attune releases with **disjoint** `watchNamespaces`.
2. Give each its own Helm release name and leader-election ID (separate
   installs) so leaders do not fight.
3. Ensure every namespace that has AttunePolicy objects is covered by exactly
   one instance.

Do not point two instances at the same policy namespaces.

### Large Deployments after aggregation

With default `Max` aggregation, high replica counts no longer explode
Prometheus series count for recommendations. Payload still grows with
`historyWindow` and `queryStep` (time series length). Keep the CRD
window/step table above for Prometheus CPU and operator sample caps.

When a workload still has more pods than `maxPodsInMetricsQuery` (default
100), Attune **samples** that many pod names into the metrics `pod=~`
regex (even spacing by name). Resize and safety still see all pods;
sampling only narrows the recommendation query surface for huge fleets
or `podAggregation: None`.

### Tier-aware history and step clamps

Set `clusterSize: large` or `xlarge` (or explicit `maxHistoryWindow` /
`minQueryStep`) so the operator clamps every policy's metrics window and
step at reconcile time. Example: large sets a 72h history ceiling and
10m step floor even if a policy still requests `168h` / `5m`. CRD bounds
(`1h`–`720h`, `10s`–`1h`) still apply first.

### Pod listing and blocker UX

- **Observe** mode skips pod lists entirely.
- **Recommend** and resize modes list pods once **per namespace** and match
  workload selectors in memory (not one List per Deployment).
- Deferred/Infeasible blocker counts recompute every reconcile by default.
  Set `blockerRefreshInterval` (e.g. `5m`) on large Recommend fleets to
  skip List+summarize while the throttle holds and **keep the last
  blocker status values**.

### Informer memory

- **Strip transforms** reduce stored fields for Pods, Deployments, StatefulSets,
  DaemonSets, ReplicaSets, Jobs, CronJobs, and HPAs.
- **Write safety:** template persistence and HPA auto-tune re-Get via
  `APIReader` (direct API) before MergeFrom/Update so stripped cache objects
  cannot wipe container images or HPA metrics.
- **Pod field strip:** all pods are stored with unused fields stripped
  (env, volumes, images). Dynamic policy selectors are refreshed about every
  30s for operator diagnostics and optional static `--pod-label-selector` /
  Helm `podLabelSelector`. Empty Spec stubs are not used: a stub would stick
  until the next watch event after selectors catch up and would break resizes.
  The watch still receives events for all pods in watched namespaces
  (Kubernetes cannot OR arbitrary label selectors in one ListWatch).
- Prefer `watchNamespaces` for multi-tenant mega-clusters to cut API scope.

### Default PromQL Max aggregation (behavioral note)

New installs and upgrades default to `podAggregation: Max`. That is a
deliberate semantic change from pre-#488 unaggregated series. Set
`podAggregation: None` only if you need the legacy multi-pod sample pool.

### Safety throttle batching

When many pods are under deferred safety observation, Attune issues a
batch throttle PromQL vector (`pod=~` / `container=~`) instead of one
instant query per pod/container when the Prometheus collector is in use.
Large observation sets are split into chunks of 64 pod/container pairs so
the regex stays bounded; the Prometheus rate limiter spends one token per
chunk.

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
negligible for any production API server. Enabling Auto mode after a long
Recommend period can produce a larger write wave; use
`maxConcurrentResizes` and per-cycle budgets to cap that.

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
  name: attune
spec:
  priorityLevelConfiguration:
    name: workload-high
  matchingPrecedence: 1000
  rules:
    - subjects:
        - kind: ServiceAccount
          serviceAccount:
            name: attune
            namespace: attune-system
      resourceRules:
        - verbs: ["*"]
          apiGroups: ["*"]
          resources: ["*"]
```

## Namespace Scoping

By default, the operator watches all namespaces for AttunePolicy
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
  resources (Pods, Deployments, HPAs, AttunePolicies, etc.)
- Cluster-scoped resources (Nodes, AttuneDefaults) are always watched
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
        app.kubernetes.io/name: attune
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
