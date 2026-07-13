This page documents every value in the Helm chart's `values.yaml`.

## Operator

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `replicaCount` | int | `1` | Number of operator replicas. Set to `2` for HA with leader election. |
| `image.repository` | string | `ghcr.io/attune-io/attune` | Container image repository |
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

### PrometheusRule alerts

Create a `PrometheusRule` resource for out-of-the-box alerting. Requires the Prometheus Operator CRDs (`monitoring.coreos.com/v1`).

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `metrics.prometheusRule.enabled` | bool | `false` | Create a PrometheusRule with all alert rules below |
| `metrics.prometheusRule.additionalLabels` | object | `{}` | Extra labels for the PrometheusRule resource |

Each alert rule supports `enabled`, `for`, and `severity`. Some rules have additional tuning parameters.

| Rule | Default severity | Default `for` | Extra parameters | Description |
|------|-----------------|---------------|------------------|-------------|
| `reconcileErrors` | warning | 10m | `threshold` (default `"0"`) | Fires when reconcile error rate exceeds threshold |
| `prometheusUnreachable` | warning | 10m | | Fires when Prometheus queries fail |
| `degraded` | critical | 5m | | Fires when workloads are in Degraded state |
| `highRevertRate` | critical | 15m | `threshold` (default `"0.5"`) | Fires when revert rate exceeds 50% |
| `reconcileStale` | warning | 5m | `staleDuration` (default `30m`) | Fires when no reconcile completes within the stale duration |
| `budgetExhausted` | warning | 30m | | Fires when a policy's resize budget is exhausted |
| `dataQuality` | warning | 30m | | Fires when NaN/Inf values are detected in Prometheus data |
| `requestsClamped` | info | 1h | | Fires when recommended requests are clamped to limits |
| `staleRecommendations` | warning | 1h | | Fires when recommendations are marked stale due to Prometheus data gaps |
| `revertFailures` | critical | 5m | | Fires when resize revert operations fail |

To disable a specific rule:

```yaml
metrics:
  prometheusRule:
    enabled: true
    rules:
      requestsClamped:
        enabled: false
```

For detailed PromQL expressions and alert tuning, see the
[Prometheus setup guide](../guides/prometheus-setup.md#built-in-alerts).

## Webhooks

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `webhooks.enabled` | bool | `true` | Enable admission webhooks for defaulting and validation. Requires cert-manager. |
| `initialSizing.enabled` | bool | `false` | Enable the pod initial sizing mutating webhook. Sets pod resource requests at creation time based on existing AttunePolicy recommendations. Requires namespace label `attune.io/initial-sizing=enabled` and `initialSizing: true` on the policy. |

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
| `watchNamespaces` | list | `[]` | Namespaces to watch for AttunePolicy resources. Empty list means all namespaces (cluster-scoped). Maps to the `--watch-namespaces` manager flag. Set this on large clusters where policies exist in only a few namespaces to dramatically reduce informer cache memory. Cluster-scoped resources (Nodes, AttuneDefaults) are always watched regardless. Requires a pod restart to change. |

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
| `maxConcurrentReconciles` | int/string | `""` (1) | Maximum number of AttunePolicy reconciles running in parallel. Maps to the `--max-concurrent-reconciles` manager flag. The default (1) processes policies sequentially. Increase for clusters with many policies to reduce reconcile queue latency. Auto-set by `clusterSize` preset (small=1, medium=2, large=4, xlarge=8). The Prometheus rate limiter (`prometheusQPS`) is shared across all goroutines, so concurrent reconciles won't overwhelm Prometheus. |

## OpenShift

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `openshift.enabled` | bool | `false` | Enable OpenShift-specific features. Adds RBAC for `config.openshift.io/apiservers` (read-only) to auto-detect the cluster TLS security profile and apply it to outbound Prometheus connections. See the [OpenShift guide](../guides/openshift.md). |

## FIPS 140-3

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `fips.enabled` | bool | `false` | Enable FIPS 140-3 mode. Sets `GODEBUG=fips140=<mode>` to activate Go's CMVP-validated cryptographic module. |
| `fips.mode` | string | `on` | FIPS enforcement level: `on` (prefer FIPS, allow fallback) or `only` (strict, rejects non-FIPS algorithms). See the [FIPS guide](../guides/fips-compliance.md). |

## Logging

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `logging.level` | string | `info` | Log level: `debug`, `info`, `warn`, `error` |
| `logging.format` | string | `json` | Log format: `json` or `text` |

## Cluster-wide Defaults (Helm-managed)

The Helm chart can create an `AttuneDefaults` CR automatically when
`defaults.enabled: true` is set. This is equivalent to creating the
CR manually but managed through Helm values.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `defaults.enabled` | bool | `false` | Create an AttuneDefaults resource with the values below |
| `defaults.cpu.*` | object | | CPU resource defaults (see [Resource Config](#resource-configuration) below) |
| `defaults.memory.*` | object | | Memory resource defaults (see [Resource Config](#resource-configuration) below) |
| `defaults.costPricing.cpuPerCoreHour` | string | `"0.031"` | Cost per vCPU-hour for savings estimates |
| `defaults.costPricing.memoryPerGiBHour` | string | `"0.004"` | Cost per GiB-hour for savings estimates |
| `defaults.metricsSource.*` | object | | Default metrics source (e.g., shared Prometheus address) |
| `defaults.updateStrategy.*` | object | | Default update strategy (type, cooldown, autoRevert, etc.) |

The rendered CR has the same spec as a manually created `AttuneDefaults`
(documented below). All fields from the CRD are available in the Helm
values; see `values.yaml` for the full set.

## CRD Configuration (AttuneDefaults)

These fields are set on the `AttuneDefaults` cluster-scoped CRD, either
directly or via the Helm `defaults.*` values above. They apply to all
`AttunePolicy` resources.

### Cost Pricing

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `costPricing.cpuPerCoreHour` | string | `"0.031"` | USD per vCPU-hour for cost estimation |
| `costPricing.memoryPerGiBHour` | string | `"0.004"` | USD per GiB-hour for cost estimation |

These values are used to compute `status.savings.estimatedMonthlySavings`
on each `AttunePolicy`. Adjust for your cloud provider or reserved
instance pricing.

### Inheritable UpdateStrategy Fields

All `updateStrategy` fields in `AttuneDefaults` are inherited by policies
that do not set them explicitly. Policy-level values always take precedence.

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `type` | string | `Recommend` | `Observe`, `Recommend`, `OneShot`, `Canary`, `Auto` |
| `cooldown` | duration | `1h` | Minimum time between resizes |
| `autoRevert` | bool | `true` | Revert unsafe resizes automatically |
| `resizeMethod` | string | `InPlaceOnly` | `InPlaceOnly` or `InPlaceOrRecreate` |
| `maxConcurrentResizes` | int32 | `1` | Max pods to resize simultaneously |
| `maxTotalCpuIncrease` | quantity | (none) | Max aggregate CPU increase per cycle |
| `maxTotalMemoryIncrease` | quantity | (none) | Max aggregate memory increase per cycle |
| `schedule` | object | (none) | Time windows, days of week, timezone |
| `export` | object | (none) | Metrics export configuration |
| `safetyObservationPeriod` | duration | `5m` | Post-resize observation window (min: 1m) |
| `sloGuardrails` | list | `[]` | Application-level SLO PromQL checks after resize |
| `canary` | object | (none) | Canary rollout configuration (percentage, observationPeriod) |
| `initialSizing` | bool | `false` | Enable mutating webhook for pod creation |

Example: set a cluster-wide maintenance window and budget cap via
`AttuneDefaults`, then individual policies inherit them unless overridden:

```yaml
apiVersion: attune.io/v1alpha1
kind: AttuneDefaults
metadata:
  name: cluster-defaults
spec:
  updateStrategy:
    type: Auto
    cooldown: 30m
    maxTotalCpuIncrease: "2000m"
    schedule:
      windows:
        - start: "02:00"
          end: "06:00"
      daysOfWeek: [Monday, Tuesday, Wednesday, Thursday, Friday]
      timezone: UTC
```

## CRD Configuration (AttuneNamespaceDefaults)

`AttuneNamespaceDefaults` provides namespace-scoped default values that
override cluster-scoped `AttuneDefaults`. Policies in the same namespace
inherit these values unless they specify their own.

**Precedence order:** policy spec > namespace defaults > cluster defaults > built-in defaults

The spec is identical to `AttuneDefaults` (all fields in
`AttuneDefaultsSpec` are available). When multiple
`AttuneNamespaceDefaults` objects exist in the same namespace, the
lexicographically smallest `metadata.name` wins.

### Use case

Different environments often need different right-sizing parameters.
Production namespaces may use higher overheads and conservative
modes, while staging namespaces can be more aggressive:

```yaml
apiVersion: attune.io/v1alpha1
kind: AttuneNamespaceDefaults
metadata:
  name: production-defaults
  namespace: production
spec:
  cpu:
    percentile: 99
    overhead: "30"
  memory:
    percentile: 99
    overhead: "50"
    allowDecrease: false
  updateStrategy:
    type: Canary
    cooldown: 2h
    autoRevert: true
---
apiVersion: attune.io/v1alpha1
kind: AttuneNamespaceDefaults
metadata:
  name: staging-defaults
  namespace: staging
spec:
  cpu:
    percentile: 95
    overhead: "10"
  memory:
    percentile: 95
    overhead: "20"
  updateStrategy:
    type: Auto
    cooldown: 30m
```

See the full example in
[`examples/11-namespace-defaults.yaml`](https://github.com/attune-io/attune/blob/main/examples/11-namespace-defaults.yaml).

### Available Fields

All fields from `AttuneDefaults` are available in
`AttuneNamespaceDefaults`:

| Section | Fields |
|---------|--------|
| `metricsSource` | `prometheus.address`, `prometheus.headers`, `prometheus.queryParameters`, `prometheus.bearerTokenSecret`, `prometheus.tls`, `datadog.site`, `datadog.apiKeySecretRef`, `cloudwatch.region`, `cloudwatch.clusterName`, `cloudwatch.roleArn`, `historyWindow`, `minimumDataPoints`, `queryStep`, `rateWindow` |
| `cpu` | `percentile`, `overhead`, `minAllowed`, `maxAllowed`, `controlledValues`, `burstSensitivity`, `allowDecrease`, `startupBoost`, `maxChangePercent`, `maxIncreasePercent`, `maxDecreasePercent`, `memoryFromCpuRatio` |
| `memory` | Same as `cpu` |
| `updateStrategy` | `type`, `cooldown`, `autoRevert`, `resizeMethod`, `initialSizing`, `maxConcurrentResizes`, `maxTotalCpuIncrease`, `maxTotalMemoryIncrease`, `schedule`, `export`, `canary`, `safetyObservationPeriod`, `sloGuardrails` |
| `costPricing` | `cpuPerCoreHour`, `memoryPerGiBHour` |

## Alternative Metrics Sources

By default, Attune queries Prometheus for CPU and memory usage data.
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

## Policy-Level Fields

### spec.paused

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `paused` | bool | `false` | Halts all reconciliation for this policy: no metrics collection, no recommendations, no resizes. Existing resizes are not reverted. The operator sets `Ready=False` with `reason=Paused`. |

### Directional Change Caps

Per-resource fields in `cpu` and `memory` that limit how much a recommendation can change per cycle:

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `maxIncreasePercent` | int32 | inherits `maxChangePercent` | Maximum percentage increase allowed per resize cycle. If unset, falls back to `maxChangePercent` (CPU: 50, memory: 30). |
| `maxDecreasePercent` | int32 | inherits `maxChangePercent` | Maximum percentage decrease allowed per resize cycle. If unset, falls back to `maxChangePercent` (CPU: 50, memory: 30). |
| `maxChangePercent` | int32 | CPU: `50`, memory: `30` | Symmetric change cap. Used as fallback for `maxIncreasePercent` and `maxDecreasePercent` when they are unset. |

### Controlled Values

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `controlledValues` | string | `RequestsOnly` | `RequestsOnly` adjusts only requests, leaving limits unchanged. `RequestsAndLimits` adjusts both in lockstep. Use `RequestsAndLimits` for Guaranteed-QoS pods (where requests equal limits) or when you want limits to track recommendations. |

### Allow Decrease

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `cpu.allowDecrease` | bool | `true` | Whether CPU requests can be decreased. When `true`, the safety monitor checks for throttling after each decrease. |
| `memory.allowDecrease` | bool | `false` | Whether memory requests can be decreased. Defaults to `false` to prevent OOMKill from sudden memory reductions. |

### Startup Boost

Temporarily increases CPU requests for newly created or restarted pods to accelerate JVM/.NET class loading, JIT compilation, and cache warming.

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `startupBoost.multiplier` | string | (none) | Scales the recommended CPU request during startup. For example, `"3.0"` means 3x the steady-state recommendation. Must be > 1.0 and <= 10.0. |
| `startupBoost.duration` | duration | (none) | How long the boost lasts before reducing to the steady-state recommendation. Must be >= 10s and <= 1h. |

Example:

```yaml
cpu:
  startupBoost:
    multiplier: "3.0"
    duration: 2m
```

See the [startup boost guide](../guides/startup-boost.md) for details.

### Burst Sensitivity

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `burstSensitivity` | string | `"0.1"` | Controls how much burst detection inflates the recommendation. Multiplied by log2(burstMagnitude). Default `"0.1"` gives ~20% boost for magnitude 4, ~30% for 8, ~40% for 16. Set `"0"` to disable burst boost entirely. |

### Memory-from-CPU Derivation

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `memory.memoryFromCpuRatio` | string | (none) | Derives memory recommendation from CPU instead of querying Prometheus for memory metrics. The value is a ratio of GiB per core (e.g., `"2.0"` means 1 core = 2 GiB memory). Useful for JVM and heap-bound workloads where memory is proportional to CPU. |

### SLO Guardrails

Application-level PromQL checks evaluated after each resize during the safety observation period.

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `updateStrategy.sloGuardrails[].name` | string | (required) | Identifies this guardrail for logging and status |
| `updateStrategy.sloGuardrails[].query` | string | (required) | PromQL query returning a scalar. Supports `{{ .Namespace }}`, `{{ .WorkloadName }}`, `{{ .PodName }}` template variables. |
| `updateStrategy.sloGuardrails[].threshold` | string | (required) | Value that triggers a revert |
| `updateStrategy.sloGuardrails[].comparison` | string | `above` | `above` (revert when value > threshold) or `below` |
| `updateStrategy.sloGuardrails[].evaluationWindow` | duration | `5m` | How long after resize to check |

Example:

```yaml
updateStrategy:
  sloGuardrails:
    - name: p99-latency
      query: 'histogram_quantile(0.99, rate(http_request_duration_seconds_bucket{namespace="{{ .Namespace }}"}[5m]))'
      threshold: "0.5"
      comparison: above
    - name: error-rate
      query: 'sum(rate(http_requests_total{namespace="{{ .Namespace }}", code=~"5.."}[5m])) / sum(rate(http_requests_total{namespace="{{ .Namespace }}"}[5m]))'
      threshold: "0.01"
      comparison: above
```

### VPA Recommendation Consumption

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `metricsSource.vpa.name` | string | (required) | Name of the VerticalPodAutoscaler object to consume recommendations from |
| `metricsSource.vpa.namespace` | string | (policy namespace) | Namespace of the VPA. Defaults to the policy's namespace. |

At most one of `prometheus`, `datadog`, `cloudwatch`, or `vpa` may be set per policy.

### Initial Sizing

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `updateStrategy.initialSizing` | bool | `false` | When true and the initial sizing webhook is enabled, new pods matching this policy receive recommended resources at creation time via a mutating admission webhook. Requires the namespace label `attune.io/initial-sizing=enabled`. |

## Status Conditions

The controller sets these conditions on each `AttunePolicy`:

| Condition | Reasons | Description |
|-----------|---------|-------------|
| `Ready` | `Monitoring`, `InsufficientData`, `NoWorkloadsFound`, `PrometheusUnavailable`, `InvalidConfig`, `WorkloadDiscoveryFailed`, `Paused` | Overall health |
| `Resizing` | `InProgress`, `Idle`, `CooldownActive` | Active resize operation state (only in resize modes) |
| `Degraded` | `HighRevertRate` | Set when 3+ of the last 5 resizes were reverted |
| `ScheduleBlocked` | `OutsideWindow`, `InsideWindow` | Set when `updateStrategy.schedule` is configured; indicates whether the current time is within an allowed resize window |

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
