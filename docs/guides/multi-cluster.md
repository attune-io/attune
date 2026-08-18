# Multi-Cluster Operations

This guide covers deploying and operating Attune across multiple
Kubernetes clusters. Whether you run dev/staging/prod environments,
regional clusters, or a mix of both, Attune supports unified
visibility and per-cluster configuration.

## Deployment patterns

### Pattern 1: Independent clusters (recommended start)

Each cluster has its own Prometheus and its own Attune installation.
This is the simplest pattern and works for most teams.

```text
┌─────────────────┐  ┌─────────────────┐  ┌─────────────────┐
│   dev cluster    │  │ staging cluster  │  │  prod cluster   │
│                  │  │                  │  │                  │
│  Prometheus      │  │  Prometheus      │  │  Prometheus      │
│  Attune operator │  │  Attune operator │  │  Attune operator │
│  AttuneDefaults  │  │  AttuneDefaults  │  │  AttuneDefaults  │
└─────────────────┘  └─────────────────┘  └─────────────────┘
```

Install Attune on each cluster with the same Helm chart but
cluster-specific values:

```bash
# Dev cluster: Recommend mode, relaxed defaults
kubectl config use-context dev
helm install attune oci://ghcr.io/attune-io/charts/attune \
  -n attune-system --create-namespace \
  -f values-dev.yaml

# Staging cluster: Canary mode for validation
kubectl config use-context staging
helm install attune oci://ghcr.io/attune-io/charts/attune \
  -n attune-system --create-namespace \
  -f values-staging.yaml

# Prod cluster: Auto mode with conservative settings
kubectl config use-context prod
helm install attune oci://ghcr.io/attune-io/charts/attune \
  -n attune-system --create-namespace \
  -f values-prod.yaml
```

### Pattern 2: Federated Prometheus (Thanos / Cortex / Mimir)

If you use a federated Prometheus setup (Thanos, Cortex, Grafana
Mimir), each cluster's Attune operator points to the local
Prometheus sidecar or query endpoint. The federation layer handles
cross-cluster aggregation for dashboards and alerts, but each
operator only queries its own cluster's metrics.

```yaml
# AttuneDefaults on each cluster -- points to the LOCAL Prometheus
apiVersion: attune.io/v1alpha1
kind: AttuneDefaults
metadata:
  name: cluster-defaults
spec:
  metricsSource:
    prometheus:
      # Use the local Prometheus, not the global query frontend.
      # The operator needs per-pod metrics, which are only available
      # from the local Prometheus that scrapes this cluster's pods.
      address: http://prometheus-server.monitoring:80
```

!!! warning "Do not point Attune at the global query frontend"
    Attune queries per-pod, per-container CPU and memory metrics.
    These are high-cardinality series that federated query frontends
    may deduplicate or downsample. Always point the operator at the
    cluster-local Prometheus for accurate recommendations.

### Pattern 3: GitOps-managed (ArgoCD / Flux)

Store `AttunePolicy` and `AttuneDefaults` manifests in Git alongside
your application manifests. Each cluster's ArgoCD/Flux instance
applies the policies from the appropriate directory or overlay.

```text
gitops-repo/
├── base/
│   └── attune-defaults.yaml      # shared defaults
├── overlays/
│   ├── dev/
│   │   └── kustomization.yaml    # patches: updateStrategy.type=Recommend
│   ├── staging/
│   │   └── kustomization.yaml    # patches: updateStrategy.type=Canary
│   └── prod/
│       └── kustomization.yaml    # patches: updateStrategy.type=Auto
└── apps/
    └── my-app/
        └── attunepolicy.yaml     # base policy, mode overridden per env
```

See the [GitOps Integration guide](gitops-integration.md) for
ConfigMap export mode and ArgoCD/Flux-specific patterns.

## Per-cluster configuration with AttuneDefaults

`AttuneDefaults` is cluster-scoped, so each cluster gets its own
instance. Use this to set environment-specific defaults:

```yaml
# values-dev.yaml -- aggressive settings for fast feedback
apiVersion: attune.io/v1alpha1
kind: AttuneDefaults
metadata:
  name: cluster-defaults
spec:
  metricsSource:
    prometheus:
      address: http://prometheus-server.monitoring:80
  updateStrategy:
    type: Recommend
    cooldown: "5m"
  cpu:
    percentile: 90
    overhead: "10"
  memory:
    percentile: 95
    overhead: "15"
```

```yaml
# values-prod.yaml -- conservative settings for stability
apiVersion: attune.io/v1alpha1
kind: AttuneDefaults
metadata:
  name: cluster-defaults
spec:
  metricsSource:
    prometheus:
      address: http://prometheus-server.monitoring:80
  updateStrategy:
    type: Auto
    cooldown: "2h"
  cpu:
    percentile: 99
    overhead: "30"
  memory:
    percentile: 99
    overhead: "40"
```

Policies that omit a field inherit the cluster's `AttuneDefaults`
value. Policies that set a field explicitly override the default. This
lets you run the same policy manifest across environments with
different behavior.

## Cross-cluster operations with kubectl attune

The `kubectl attune` plugin supports querying multiple clusters from
a single command. Results include a `CLUSTER` column showing which
context each policy belongs to.

### View status across all clusters

```bash
kubectl attune status --all-contexts
```

```text
CLUSTER   NAMESPACE   NAME       TYPE        WORKLOADS   RECS   RESIZED   READY   AGE
dev       default     my-app     Recommend   3           3      0         True    7d
staging   default     my-app     Canary      3           3      1         True    5d
prod      default     my-app     Auto        3           3      3         True    30d
prod      payments    checkout   Auto        2           2      2         True    14d
```

### Query specific clusters

```bash
kubectl attune status --contexts prod-us,prod-eu
```

### Compare savings across clusters

```bash
kubectl attune savings --all-contexts --sort-by savings
```

### View recommendations for a specific cluster

```bash
kubectl attune recommendations --contexts staging -n default my-app
```

!!! note "Supported commands"
    Multi-cluster mode works with `status`, `savings`,
    `recommendations`, and `history`. The `wizard`, `explain`, and
    `diff` commands operate on a single context only.

## Observability across clusters

### Grafana dashboards

With independent clusters, import the same Attune dashboard into each
cluster's Grafana. Use Grafana's data source selector to switch
between clusters.

With federated Prometheus (Thanos/Mimir), create a single dashboard
that queries the global endpoint. Add an `external_labels` cluster
identifier to distinguish metrics:

```yaml
# Prometheus configuration on each cluster
global:
  external_labels:
    cluster: prod-us-east-1
```

Then modify the dashboard's PromQL to include the cluster label:

```promql
sum by (cluster, namespace) (rate(attune_resize_total[5m]))
```

### PrometheusRule alerts

Deploy the PrometheusRule on each cluster independently. Alerts fire
per-cluster, which is usually what you want since each cluster has its
own operational context.

```bash
# Enable alerts on all clusters
for ctx in dev staging prod; do
  kubectl config use-context "$ctx"
  helm upgrade attune oci://ghcr.io/attune-io/charts/attune \
    --set metrics.prometheusRule.enabled=true
done
```

For centralized alerting with Alertmanager federation, no Attune-specific
configuration is needed. The standard Alertmanager routing and
inhibition rules apply.

## Fleet observability with federated Prometheus

Platform teams often need **org-wide** answers (savings this month, stuck
policies, resize health) without logging into every cluster. Attune stays
**per-cluster for control**, and uses **metrics federation + optional fleet
reports** for read-only rollups.

### Label convention

Each cluster Prometheus must set a stable cluster identity:

```yaml
global:
  external_labels:
    cluster: prod-us-east-1   # required for fleet faceting
```

Do **not** add high-cardinality labels. `cluster` is the only fleet facet
Attune documents for first-party dashboards and recording rules.

### Fleet Grafana dashboard

Ship and enable the fleet dashboard (cluster variable, resize/revert/savings
panels):

```bash
helm upgrade attune oci://ghcr.io/attune-io/charts/attune \
  --set grafanaFleetDashboard.enabled=true
```

Source of truth: `deploy/grafana/fleet-dashboard.json` (also packaged as
`charts/attune/files/grafana-fleet-dashboard.json`).

Panels include:

| Panel | Signal |
|-------|--------|
| Resize success / fail by cluster | `attune_resize_total` |
| Reverts by cluster | `attune_reverts_total` |
| Estimated monthly savings by cluster | `attune_savings_estimated_monthly_dollars` |
| CPU / memory savings by cluster | savings gauges |
| Fleet report export failures | `attune_fleet_report_export_total` |

Point the dashboard datasource at the **global** Thanos/Mimir/AMP query
frontend (not the per-cluster operator scrape target).

### Recording rules (org rollups)

Enable first-party recording rules on each cluster (or only on the global
ruler, if you prefer a single place):

```bash
helm upgrade attune oci://ghcr.io/attune-io/charts/attune \
  --set metrics.prometheusRule.enabled=true \
  --set metrics.prometheusRule.fleetRecordingRules.enabled=true
```

| Recording rule | Purpose |
|----------------|---------|
| `attune:resize:rate5m` | Resize rate by cluster, namespace, result |
| `attune:reverts:rate5m` | Revert rate by cluster, namespace, reason |
| `attune:savings:cpu_cores` | Freeable CPU cores by cluster/namespace |
| `attune:savings:memory_bytes` | Freeable memory by cluster/namespace |
| `attune:savings:estimated_monthly_dollars` | Approximate monthly USD by cluster/namespace |
| `attune:fleet_report:export_failures_rate5m` | Fleet ConfigMap export failure rate |

Example org PromQL:

```promql
# Resizes succeeded across the fleet (last hour)
sum(increase(attune_resize_total{result="success"}[1h]))

# Estimated monthly savings by cluster (approximate)
sum by (cluster) (attune:savings:estimated_monthly_dollars)
```

Savings figures are already approximate per cluster. Org rollups must not
claim higher precision than the sum of per-cluster estimates.

### Fleet status export (optional, Phase B)

When you need structured summaries for tools that are not Prometheus-centric,
enable the operator-side fleet report (default **off**):

```yaml
fleetReport:
  enabled: true
  configMapName: attune-fleet-report
  clusterId: prod-us-east-1
  interval: 5m
```

The leader writes a ConfigMap labeled `attune.io/fleet-report=true` with:

| Key | Content |
|-----|---------|
| `schema-version` | `v1` (breaking changes require a version bump) |
| `cluster-id` | From `fleetReport.clusterId` |
| `generated-at` | RFC3339 UTC |
| `report.json` | Full JSON document (see schema below) |

**`report.json` schema (v1):**

| Field | Type | Description |
|-------|------|-------------|
| `schemaVersion` | string | Always `v1` for this shape |
| `clusterId` | string | Optional stable id |
| `generatedAt` | timestamp | When the report was built |
| `policyCount` | int | Number of AttunePolicy objects |
| `policiesByMode` | map | Counts by stored `updateStrategy.type` (empty type counted as `Recommend`) |
| `readyTrue` / `readyFalse` | int | Ready condition counts |
| `insufficientData` | int | Policies blocked on data |
| `workloadsDiscovered` | int | Sum of discovered workloads |
| `workloadsWithRecommendations` | int | Sum with recommendations |
| `workloadsResized` | int | Sum resized |
| `estimatedMonthlySavingsUSD` | float | Sum of parseable policy savings (empty, non-numeric, NaN, and Inf values count as 0) |
| `reclaimedCpuRequestMilli` | int | Freeable CPU millicores (when present) |
| `reclaimedMemoryRequestBytes` | int | Freeable memory bytes (when present) |

Collectors must ignore unknown fields. Metric:
`attune_fleet_report_export_total{result="success|failed"}`.

If the operator is scoped with `--watch-namespaces` / `watchNamespaces`, the
report only includes policies in watched namespaces (partial cluster view).
For a full-cluster report, leave watchNamespaces empty.

With HA (`replicaCount` > 1), enable leader election so only one pod writes
the ConfigMap.

### Collect reports from N clusters

```bash
# Current context only
scripts/collect-fleet-reports.sh

# Explicit contexts
scripts/collect-fleet-reports.sh --contexts dev,staging,prod

# All kubeconfig contexts
scripts/collect-fleet-reports.sh --all-contexts -n attune-system
```

Output is a JSON array of reports (or error objects per context) suitable for
CI tables or FinOps pipelines. Requires `kubectl` and `jq`.

### What this is not

- No multi-cluster **resize control** from a hub (still dangerous for v1).
- No replacement for per-cluster Attune installs.
- Fleet reports and federated metrics are **read-only** aggregation paths.

## Example: graduated rollout across environments

A common pattern is to validate recommendations in lower environments
before enabling auto-resize in production:

| Environment | Mode | Cooldown | Percentile | Overhead | Purpose |
|-------------|------|----------|------------|----------|---------|
| Dev | Recommend | 5m | P90 | 10% | Fast feedback, catch regressions |
| Staging | Canary | 30m | P95 | 20% | Validate resizes on 1 pod first |
| Prod | Auto | 2h | P99 | 30% | Conservative auto-resize |

1. Deploy a policy in **Recommend** mode in dev
2. Review recommendations with `kubectl attune diff -n default my-app`
3. Promote to **Canary** in staging and observe for a week
4. Check revert rate: `kubectl attune status --contexts staging`
5. If stable, promote to **Auto** in prod

```bash
# Quick cross-cluster status check
kubectl attune status --all-contexts --filter ready
```

## Troubleshooting

### "context not found" errors

The `--all-contexts` flag reads from your kubeconfig file. Verify
available contexts:

```bash
kubectl config get-contexts
```

### Partial failures

If one cluster is unreachable, the plugin prints a warning and
continues with the remaining clusters:

```text
WARNING: context "dev": dial tcp 10.0.0.1:6443: connect: connection refused
CLUSTER   NAMESPACE   NAME     TYPE   WORKLOADS   RECS   RESIZED   READY   AGE
prod      default     my-app   Auto   3           3      3         True    30d
```

### Different Attune versions across clusters

The plugin reads the `AttunePolicy` status fields, which are backward
compatible across minor versions. You can safely query clusters running
different Attune versions from the same plugin binary.


## Fleet observability (metrics federation)

Attune installs one operator per cluster. For org-wide visibility without a
multi-cluster control plane:

1. Scrape each cluster's `attune_*` metrics into a central Prometheus, Thanos,
   Mimir, or AMP with a consistent `cluster` (or `cluster_id`) external label.
2. Import the Helm Grafana dashboard and add a `cluster` template variable
   filtering on that label.
3. Useful org-wide panels:
   - `sum by (cluster) (increase(attune_resize_total[24h]))`
   - `sum by (cluster) (attune_savings_estimated_monthly)`
   - `sum by (cluster) (attune_pods_infeasible)` / `attune_pods_deferred` when available
4. Keep Auto mode local to each cluster; aggregate **read-only** reporting only
   in the first phase.

See the [metrics reference](../reference/metrics.md) for metric names and labels.
