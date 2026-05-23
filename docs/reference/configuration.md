This page documents every value in the Helm chart's `values.yaml`.

## Operator

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `replicaCount` | int | `1` | Number of operator replicas. Set to `2` for HA with leader election. |
| `image.repository` | string | `ghcr.io/sebtardiflabs/kube-rightsize` | Container image repository |
| `image.pullPolicy` | string | `IfNotPresent` | Image pull policy |
| `image.tag` | string | `""` | Image tag. Defaults to the chart's `appVersion`. |
| `imagePullSecrets` | list | `[]` | Image pull secrets for private registries |
| `nameOverride` | string | `""` | Override the chart name |
| `fullnameOverride` | string | `""` | Override the fully qualified app name |

## Service Account

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `serviceAccount.create` | bool | `true` | Create a ServiceAccount for the operator |
| `serviceAccount.annotations` | object | `{}` | Annotations to add to the ServiceAccount |
| `serviceAccount.name` | string | `""` | ServiceAccount name. Auto-generated if empty. |

## Pod configuration

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `podAnnotations` | object | `{}` | Additional pod annotations |
| `podSecurityContext.runAsNonRoot` | bool | `true` | Run pod as non-root |
| `podSecurityContext.seccompProfile.type` | string | `RuntimeDefault` | Seccomp profile |
| `securityContext.allowPrivilegeEscalation` | bool | `false` | Deny privilege escalation |
| `securityContext.capabilities.drop` | list | `["ALL"]` | Drop all Linux capabilities |
| `securityContext.readOnlyRootFilesystem` | bool | `true` | Read-only root filesystem |
| `securityContext.runAsNonRoot` | bool | `true` | Run container as non-root |
| `securityContext.runAsUser` | int | `65532` | UID for the container process |
| `securityContext.runAsGroup` | int | `65532` | GID for the container process |

## Cluster Size Presets

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `clusterSize` | string | `""` | Cluster size preset (`small`, `medium`, `large`, `xlarge`, or empty). Sets resources, rate limits, and replica count in one shot. See the [Scaling Guide](../guides/scaling.md) for details. |
| `prometheusQPS` | number | `10` | Prometheus query rate limit (queries per second). Increase for large clusters with many policies. |
| `prometheusBurst` | int | `20` | Prometheus query burst allowance. |

## Resources

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `resources` | object | `{}` | Operator pod resource requests and limits. When empty, defaults are derived from `clusterSize` (or the `small` tier if `clusterSize` is also empty). |
| `resources.limits.cpu` | string | (preset) | CPU limit for the operator pod |
| `resources.limits.memory` | string | (preset) | Memory limit for the operator pod |
| `resources.requests.cpu` | string | (preset) | CPU request for the operator pod |
| `resources.requests.memory` | string | (preset) | Memory request for the operator pod |

## Scheduling

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `nodeSelector` | object | `{}` | Node selector for operator pods |
| `tolerations` | list | `[]` | Tolerations for operator pods |
| `affinity` | object | `{}` | Affinity rules for operator pods |
| `topologySpreadConstraints` | list | `[]` | Topology spread constraints for operator pods |
| `priorityClassName` | string | `""` | Priority class name for the operator pod (recommended: `system-cluster-critical` for production) |

## Leader election

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `leaderElection.enabled` | bool | `true` | Enable leader election. Required for `replicaCount > 1`. |

## Operator metrics

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `metrics.enabled` | bool | `true` | Expose operator metrics endpoint |
| `metrics.port` | int | `8080` | Metrics endpoint port |
| `metrics.serviceMonitor.enabled` | bool | `false` | Create a Prometheus Operator ServiceMonitor |
| `metrics.serviceMonitor.additionalLabels` | object | `{}` | Extra labels for the ServiceMonitor |
| `metrics.serviceMonitor.interval` | string | `30s` | Scrape interval |

## Webhooks

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `webhooks.enabled` | bool | `true` | Enable admission webhooks for defaulting and validation. Requires cert-manager. |

## Grafana Dashboard

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `grafanaDashboard.enabled` | bool | `false` | Create a ConfigMap with the Grafana dashboard. Auto-discovered by the Grafana sidecar via the `grafana_dashboard: "1"` label. |
| `grafanaDashboard.additionalLabels` | object | `{}` | Extra labels for the dashboard ConfigMap (e.g., folder selection). |

## Network Policy

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `networkPolicy.enabled` | bool | `true` | Enable a NetworkPolicy restricting operator pod traffic to DNS, K8s API, Prometheus, and metrics/health/webhook ports. |
| `networkPolicy.prometheusPort` | int | `9090` | TCP port allowed for egress to Prometheus backend pods. Must match the Prometheus pod port, not the Service port. |

## Collector Cache

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `collectorTTL` | string | `"10m"` | How long unused Prometheus collectors stay cached before eviction. Maps to the `--collector-ttl` manager flag. Increase if policies frequently rotate Prometheus addresses; decrease in memory-constrained environments. |

## Prometheus Query Timeout

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `prometheusTimeout` | string | `"5m"` | Maximum time allowed for workload processing (including Prometheus queries) during a single reconciliation cycle. Maps to the `--prometheus-timeout` manager flag. If exceeded, the reconciler uses partial results and surfaces the timeout in the policy's status condition. Increase for clusters with slow Prometheus instances or very large numbers of workloads per policy. |

## Namespace Scoping

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `watchNamespaces` | list | `[]` | Namespaces to watch for RightSizePolicy resources. Empty list means all namespaces (cluster-scoped). Maps to the `--watch-namespaces` manager flag. Set this on large clusters where policies exist in only a few namespaces to dramatically reduce informer cache memory. Cluster-scoped resources (Nodes, RightSizeDefaults) are always watched regardless. Requires a pod restart to change. |

Example:

```yaml
watchNamespaces:
  - production
  - staging
  - team-alpha
```

## Reconcile Concurrency

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `maxConcurrentReconciles` | int/string | `""` (1) | Maximum number of RightSizePolicy reconciles running in parallel. Maps to the `--max-concurrent-reconciles` manager flag. The default (1) processes policies sequentially. Increase for clusters with many policies to reduce reconcile queue latency. Auto-set by `clusterSize` preset (small=1, medium=2, large=4, xlarge=8). The Prometheus rate limiter (`prometheusQPS`) is shared across all goroutines, so concurrent reconciles won't overwhelm Prometheus. |

## Logging

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `logging.level` | string | `info` | Log level: `debug`, `info`, `warn`, `error` |
| `logging.format` | string | `json` | Log format: `json` or `text` |

## CRD Configuration (RightSizeDefaults)

These fields are set on the `RightSizeDefaults` cluster-scoped CRD, not in
the Helm `values.yaml`. They apply to all `RightSizePolicy` resources.

### Cost Pricing

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `costPricing.cpuPerCoreHour` | string | `"0.031"` | USD per vCPU-hour for cost estimation |
| `costPricing.memoryPerGiBHour` | string | `"0.004"` | USD per GiB-hour for cost estimation |

These values are used to compute `status.savings.estimatedMonthlySavings`
on each `RightSizePolicy`. Adjust for your cloud provider or reserved
instance pricing.

### Inheritable UpdateStrategy Fields

All `updateStrategy` fields in `RightSizeDefaults` are inherited by policies
that do not set them explicitly. Policy-level values always take precedence.

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `mode` | string | `Recommend` | `Observe`, `Recommend`, `OneShot`, `Canary`, `Auto` |
| `cooldown` | duration | `1h` | Minimum time between resizes |
| `autoRevert` | bool | `true` | Revert unsafe resizes automatically |
| `resizeMethod` | string | `InPlaceOnly` | `InPlaceOnly` or `InPlaceOrRecreate` |
| `maxCpuChangePercent` | int32 | `50` | Max CPU change per resize (%) |
| `maxMemoryChangePercent` | int32 | `30` | Max memory change per resize (%) |
| `maxConcurrentResizes` | int32 | `1` | Max pods to resize simultaneously |
| `maxTotalCpuIncrease` | quantity | (none) | Max aggregate CPU increase per cycle |
| `maxTotalMemoryIncrease` | quantity | (none) | Max aggregate memory increase per cycle |
| `schedule` | object | (none) | Time windows, days of week, timezone |
| `export` | object | (none) | Metrics export configuration |

Example: set a cluster-wide maintenance window and budget cap via
`RightSizeDefaults`, then individual policies inherit them unless overridden:

```yaml
apiVersion: rightsize.io/v1alpha1
kind: RightSizeDefaults
metadata:
  name: cluster-defaults
spec:
  updateStrategy:
    mode: Auto
    cooldown: 30m
    maxTotalCpuIncrease: "2000m"
    schedule:
      windows:
        - start: "02:00"
          end: "06:00"
      daysOfWeek: [Monday, Tuesday, Wednesday, Thursday, Friday]
      timezone: UTC
```

## CRD Configuration (RightSizeNamespaceDefaults)

`RightSizeNamespaceDefaults` provides namespace-scoped default values that
override cluster-scoped `RightSizeDefaults`. Policies in the same namespace
inherit these values unless they specify their own.

**Precedence order:** policy spec > namespace defaults > cluster defaults > built-in defaults

The spec is identical to `RightSizeDefaults` (all fields in
`RightSizeDefaultsSpec` are available). When multiple
`RightSizeNamespaceDefaults` objects exist in the same namespace, the
lexicographically smallest `metadata.name` wins.

### Use case

Different environments often need different right-sizing parameters.
Production namespaces may use higher safety margins and conservative
modes, while staging namespaces can be more aggressive:

```yaml
apiVersion: rightsize.io/v1alpha1
kind: RightSizeNamespaceDefaults
metadata:
  name: production-defaults
  namespace: production
spec:
  cpu:
    percentile: 99
    safetyMargin: "1.3"
  memory:
    percentile: 99
    safetyMargin: "1.5"
    allowDecrease: false
  updateStrategy:
    mode: Canary
    cooldown: 2h
    autoRevert: true
---
apiVersion: rightsize.io/v1alpha1
kind: RightSizeNamespaceDefaults
metadata:
  name: staging-defaults
  namespace: staging
spec:
  cpu:
    percentile: 95
    safetyMargin: "1.1"
  memory:
    percentile: 95
    safetyMargin: "1.2"
  updateStrategy:
    mode: Auto
    cooldown: 30m
```

See the full example in
[`examples/11-namespace-defaults.yaml`](https://github.com/SebTardifLabs/kube-rightsize/blob/main/examples/11-namespace-defaults.yaml).

### Available Fields

All fields from `RightSizeDefaults` are available in
`RightSizeNamespaceDefaults`:

| Section | Fields |
|---------|--------|
| `metricsSource` | `prometheus.address`, `prometheus.headers`, `prometheus.queryParameters`, `prometheus.bearerTokenSecret`, `prometheus.tls`, `datadog.site`, `datadog.apiKeySecretRef`, `cloudwatch.region`, `cloudwatch.clusterName`, `cloudwatch.roleArn`, `historyWindow`, `minimumDataPoints`, `queryStep`, `rateWindow` |
| `cpu` | `percentile`, `safetyMargin`, `minAllowed`, `maxAllowed`, `controlledValues`, `burstSensitivity`, `allowDecrease`, `startupBoost` |
| `memory` | Same as `cpu` |
| `updateStrategy` | `mode`, `cooldown`, `autoRevert`, `resizeMethod`, `maxCpuChangePercent`, `maxMemoryChangePercent`, `maxConcurrentResizes`, `maxTotalCpuIncrease`, `maxTotalMemoryIncrease`, `schedule`, `export`, `canary` |
| `costPricing` | `cpuPerCoreHour`, `memoryPerGiBHour` |

## Alternative Metrics Sources

By default, kube-rightsize queries Prometheus for CPU and memory usage data.
The CRD also supports Datadog and CloudWatch Container Insights as
alternative metrics sources. **At most one** of `prometheus`, `datadog`, or
`cloudwatch` may be set per policy.

> The Datadog collector queries the `/api/v1/query` endpoint and converts
> nanocores to cores automatically. The CloudWatch collector uses the
> Container Insights `ContainerInsights` namespace and supports IRSA/Pod
> Identity credentials with optional cross-account role assumption.

### Datadog

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `metricsSource.datadog.site` | string | `datadoghq.com` | Datadog site (e.g., `datadoghq.eu`, `us5.datadoghq.com`, `ddog-gov.com`) |
| `metricsSource.datadog.apiKeySecretRef.name` | string | (required) | Name of the Secret containing the Datadog API key |
| `metricsSource.datadog.apiKeySecretRef.key` | string | (required) | Key within the Secret that holds the API key |

### CloudWatch Container Insights

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `metricsSource.cloudwatch.region` | string | (required) | AWS region (e.g., `us-east-1`) |
| `metricsSource.cloudwatch.clusterName` | string | (required) | EKS cluster name for Container Insights metric filtering |
| `metricsSource.cloudwatch.roleArn` | string | `""` | Optional IAM role ARN for cross-account access (IRSA/Pod Identity used if empty) |

## Status Conditions

The controller sets these conditions on each `RightSizePolicy`:

| Condition | Reasons | Description |
|-----------|---------|-------------|
| `Ready` | `Monitoring`, `InsufficientData`, `NoWorkloadsFound`, `PrometheusUnavailable`, `InvalidConfig`, `WorkloadDiscoveryFailed` | Overall health |
| `Resizing` | `InProgress`, `Idle`, `CooldownActive` | Active resize operation state (only in resize modes) |
| `Degraded` | `HighRevertRate` | Set when 3+ of the last 5 resizes were reverted |

## Exponential Backoff

When consecutive resizes are reverted, the cooldown doubles per revert
(capped at 16x). A successful resize resets the multiplier.

| Consecutive reverts | Effective cooldown |
|---------------------|-------------------|
| 0 | 1x base |
| 1 | 2x |
| 2 | 4x |
| 3 | 8x |
| 4+ | 16x (cap) |

## Example: HA deployment with ServiceMonitor

```yaml
replicaCount: 2
leaderElection:
  enabled: true
metrics:
  serviceMonitor:
    enabled: true
    additionalLabels:
      release: prometheus
resources:
  limits:
    cpu: 1
    memory: 256Mi
  requests:
    cpu: 200m
    memory: 128Mi
```
