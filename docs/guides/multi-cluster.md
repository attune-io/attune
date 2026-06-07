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
