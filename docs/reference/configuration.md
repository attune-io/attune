This page documents every value in the Helm chart's `values.yaml`.

## Operator

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `replicaCount` | int | `1` | Number of operator replicas. Set to `2` for HA with leader election. |
| `image.repository` | string | `ghcr.io/attune-io/attune` | Container image repository |
| `image.pullPolicy` | string | `IfNotPresent` | Image pull policy |
| `image.tag` | string | `""` | Image tag. When empty, the chart uses `appVersion` (bare SemVer). Releases also publish the `vX.Y.Z` alias. |
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
| `budgetExhausted` | warning | 30m | | Fires when a policy's increase budget refuses resizes, either deferred (`BudgetExhausted`) or blocked by a cap smaller than one increase (`IncreaseExceedsBudget`) |
| `dataQuality` | warning | 30m | | Fires when NaN/Inf values are detected in Prometheus data |
| `requestsClamped` | info | 1h | | Fires when recommended requests are clamped to limits |
| `staleRecommendations` | warning | 1h | | Fires when recommendations are marked stale due to Prometheus data gaps |
| `revertFailures` | critical | 5m | | Fires when resize revert operations fail |
| `oomBumpCapped` | info | 5m | | Fires when one or more OOM bumps in the last hour are capped at `maxBumps` or clamped to `maxAllowed`. One sample is enough. The whole PrometheusRule stays off until `metrics.prometheusRule.enabled` is true. This alert does not turn `memory.oomBump` on. |

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
| `grafanaFleetDashboard.enabled` | bool | `false` | Create a multi-cluster fleet Grafana dashboard ConfigMap (`cluster` variable). Requires Prometheus `external_labels.cluster`. See [Fleet observability](../guides/multi-cluster.md#fleet-observability-with-federated-prometheus). |
| `grafanaFleetDashboard.additionalLabels` | object | `{}` | Extra labels for the fleet dashboard ConfigMap. |
| `metrics.prometheusRule.fleetRecordingRules.enabled` | bool | `false` | Emit `attune:*` recording rules for org-wide PromQL rollups (safe when `cluster` label is empty). |
| `fleetReport.enabled` | bool | `false` | Periodically write a versioned fleet summary ConfigMap for multi-cluster collectors. |
| `fleetReport.configMapName` | string | `attune-fleet-report` | Fleet report ConfigMap name. |
| `fleetReport.namespace` | string | `""` | ConfigMap namespace (empty = release namespace). |
| `fleetReport.clusterId` | string | `""` | Optional stable cluster id written into the report. |
| `fleetReport.interval` | string | `5m` | Refresh interval for the fleet report. |

## Network Policy

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `networkPolicy.enabled` | bool | `true` | Enable a NetworkPolicy restricting operator pod traffic to DNS, K8s API, Prometheus, and metrics/health/webhook ports. |
| `networkPolicy.prometheusPort` | int | `9090` | TCP port allowed for egress to Prometheus backend pods. Must match the Prometheus pod port, not the Service port. |

## Collector Cache

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `collectorTTL` | string | `"10m"` | How long unused collectors (Prometheus, Datadog, CloudWatch) stay cached before eviction. Maps to the `--collector-ttl` manager flag. Increase if policies frequently rotate Prometheus addresses; decrease in memory-constrained environments. |

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
| `maxConcurrentReconciles` | int/string | `""` (2) | Maximum number of AttunePolicy reconciles running in parallel. Maps to the `--max-concurrent-reconciles` manager flag (binary default 2). Empty Helm value uses 2, or clusterSize presets (small=1, medium=2, large=4, xlarge=8). The Prometheus rate limiter (`prometheusQPS`) is shared across all goroutines. |
| `maxWorkloadWorkers` | int | `10` | Parallel workers for workloads inside one policy reconcile (`--max-workload-workers`). |
| `requeueJitter` | string | `"2m"` | Max extra delay added only to full cooldown requeues (`--requeue-jitter`). Skipped while Ready is `InsufficientData` or `MetricsUnavailable` so bootstrap is not delayed by up to 2m. Set `"0s"` to disable. |
| `maxProfileSamples` | int | `10000` | Cap samples after downsampling before BuildProfile (`--max-profile-samples`). |
| `maxPrometheusSeries` | int | `5000` | Cap series per Prometheus range query (`--max-prometheus-series`). |
| `maxStatusRecommendations` | int | `100` | Default status.recommendations cap (`--max-status-recommendations`). |
| `statusIncludeExplanations` | bool | `true` | Write explanation chains to status (`--status-include-explanations`). |
| `maxPodsInMetricsQuery` | int | `100` | Cap pods named in metrics `pod=~` regexes for huge workloads (`--max-pods-in-metrics-query`). Negative disables sampling. |
| `maxHistoryWindow` | string | `""` | Operator ceiling for metrics historyWindow (`--max-history-window`). large/xlarge auto 72h/48h. |
| `minQueryStep` | string | `""` | Operator floor for metrics queryStep (`--min-query-step`). large/xlarge auto 10m/15m. |
| `blockerRefreshInterval` | string | `"0s"` | Min interval between Deferred/Infeasible blocker recomputes when not resizing (`--blocker-refresh-interval`). Zero recomputes every cycle; use `5m` for large Recommend fleets. |
| `podLabelSelector` | string | `""` | Optional static pod label selector (`--pod-label-selector`), OR'd with dynamic selectors derived from active AttunePolicy targets for informer cache keep rules. |

## Prometheus query auth

Cluster-wide credentials belong on the operator. A policy
`bearerTokenSecret` still wins and is read in the policy namespace.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `prometheusAuth.useServiceAccountToken` | bool | `false` | Send operator Prometheus bearer auth for addresses from cluster `AttuneDefaults`. Not used for policy or namespace-defaults addresses, auto-discovery, or when `Authorization` headers are already set (`--prometheus-use-service-account-token`). |
| `prometheusAuth.existingSecret.name` | string | `""` | Secret in the **operator** namespace (`--prometheus-bearer-token-secret`). Same address rules as `useServiceAccountToken`. Empty disables. A policy-set `bearerTokenSecret` is read in the policy namespace and does not fall back. An inherited cluster name falls back only on NotFound. Attune trims both ends of the token and keeps interior spaces. |
| `prometheusAuth.existingSecret.key` | string | `token` | Key in that Secret (`--prometheus-bearer-token-key`). |
| `prometheusAuth.queryServiceAccount.create` | bool | `false` | Create a dedicated query ServiceAccount and TokenRequest it instead of the manager token (`--prometheus-query-service-account`). |
| `prometheusAuth.queryServiceAccount.name` | string | `""` | Query SA name. Empty uses `<release>-prometheus-query` when create is true. |

## Datadog query auth

Cluster-wide Datadog credentials belong on the operator. A policy or
`AttuneNamespaceDefaults` `apiKeySecretRef` is still read in that namespace.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `datadogAuth.existingSecret.name` | string | `""` | Secret in the **operator** namespace (`--datadog-api-key-secret`). Used only when cluster `AttuneDefaults` chose the Datadog block. Empty keeps the policy-namespace lookup of an inherited name. |
| `datadogAuth.existingSecret.key` | string | `api-key` | API key field in that Secret (`--datadog-api-key-secret-key`). An optional `app-key` in the same Secret is still read. Attune trims both ends of each value and keeps interior spaces. A whitespace-only `app-key` is omitted. |

## OpenShift

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `openshift.enabled` | bool | `false` | Enable OpenShift-specific features. Adds RBAC for `config.openshift.io/apiservers` (read-only) to auto-detect the cluster TLS security profile and apply it to outbound Prometheus connections. See the [OpenShift guide](../guides/openshift.md). |
| `openshift.bindClusterMonitoringView` | bool | `false` | Bind OpenShift `cluster-monitoring-view` to the query ServiceAccount when `prometheusAuth.queryServiceAccount.create` is true, otherwise the manager ServiceAccount, and send that token to Prometheus. See [Thanos Querier](../guides/openshift.md#thanos-querier). |

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
| `defaults.cpu.*` | object | | CPU resource defaults (see [Resource configuration](#resource-configuration) below) |
| `defaults.memory.*` | object | | Memory resource defaults (see [Resource configuration](#resource-configuration) below) |
| `defaults.costPricing.cpuPerCoreHour` | string | `"0.031"` | Cost per vCPU-hour for savings estimates |
| `defaults.costPricing.memoryPerGiBHour` | string | `"0.004"` | Cost per GiB-hour for savings estimates |
| `defaults.excludeKnownSidecars` | bool | (operator default `true` if unset) | When set on the AttuneDefaults CR, policies that leave the field unset inherit this value. `false` restores exclude-only-via-`excludedContainers`. |
| `defaults.metricsSource.*` | object | | Default metrics source (e.g., shared Prometheus address). At most one of prometheus, datadog, cloudwatch, or vpa. Provider field rules match policies (Datadog API key, CloudWatch region and cluster, VPA name, recording-metric names). |
| `defaults.updateStrategy.*` | object | | Default update strategy (type, cooldown, autoRevert, etc.) |

The rendered CR has the same spec as a manually created `AttuneDefaults`
(documented below). All fields from the CRD are available in the Helm
values; see `values.yaml` for the full set.

## CRD Configuration (AttuneDefaults)

These fields are set on the `AttuneDefaults` cluster-scoped CRD, either
directly or via the Helm `defaults.*` values above. They apply to all
`AttunePolicy` resources.

### Resource configuration

`cpu` and `memory` on `AttunePolicy`, `AttuneDefaults`, and
`AttuneNamespaceDefaults` share the same bound fields. When a bound is
set, the operator clamps each recommendation to that range. Admission
also rejects `maxAllowed` above a hard ceiling so cluster or namespace
defaults cannot merge uncapped bounds onto policies after the policy
itself was admitted.

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `cpu.minAllowed` | quantity | 1m | Minimum CPU recommendation when the field is omitted. An explicit value, including one below 1m, replaces this floor. When both bounds are set on the same object, must be less than or equal to `cpu.maxAllowed`. On an AttunePolicy, admission also rejects a min from `AttuneDefaults` or `AttuneNamespaceDefaults` when this object omits min and a max is already known. A stored min above a non-zero max stays until the next update. The recommendation is then clamped to that max (`boundsApplied` is `max`). |
| `cpu.maxAllowed` | quantity | (none) | Maximum CPU recommendation (for example `"4000m"`). Must not exceed 256 cores. Omitted means no maximum. A new AttunePolicy write fails when a defaults min is above this max. A stored min above a non-zero max is clamped to this max until the object is updated. A zero max keeps the raised floor. |
| `memory.minAllowed` | quantity | 4Mi | Minimum memory recommendation when the field is omitted. An explicit value replaces this floor. When both bounds are set on the same object, must be less than or equal to `memory.maxAllowed`. On an AttunePolicy, admission also rejects a min from `AttuneDefaults` or `AttuneNamespaceDefaults` when this object omits min and a max is already known. A stored min above a non-zero max stays until the next update. The recommendation is then clamped to that max (`boundsApplied` is `max`). |
| `memory.maxAllowed` | quantity | (none) | Maximum memory recommendation (for example `"8Gi"`). Must not exceed 16Ti. Omitted means no maximum. An in-hold `memory.oomBump` floor above this value is published at the cap. The stored floor is not rewritten. A new AttunePolicy write fails when a defaults min is above this max. A stored min above a non-zero max is clamped to this max until the object is updated. A Guaranteed raise after `memory.limitMultiplier` can set the live pod request above this cap. It does not raise the status recommendation. The cap still applies to the engine request before that raise. That raise is not the in-hold `oomBump` clamp. |

The 256-core and 16Ti values are admission caps only. They reject
oversized `maxAllowed`; they do not inject a default clamp when the
field is unset. Helm `defaults.cpu.*` and `defaults.memory.*` are
subject to the same webhook because the chart creates an
`AttuneDefaults` resource.

An explicit `maxAllowed` is applied again after the percent cap, which
can move farther than `maxDecreasePercent` in that engine step. CPU
`allowDecrease` defaults to true, so a live CPU request above the max
is published at the cap on this cycle. Memory `allowDecrease` defaults
to false, so a live memory request above the max stays at the current
request until decrease is enabled. An omitted `maxAllowed` is not
capped. `"0"` is a real cap, not an omitted maximum. An explicit
`minAllowed` below 1m or 4Mi replaces the built-in floor. Admission
compares min and max after the defaults merge on AttunePolicy writes.
The policy value wins when it is set. A missing min is taken from
`AttuneNamespaceDefaults` when that object sets one, otherwise from
`AttuneDefaults`, and only when a max is already known on the policy
or a container. The error names both
quantities and both objects. A defaults list error fails only that
write. A policy that sets its own min does not list defaults to
discover a max. `kubectl attune explain` may show an invalid merged
pair. Explain does not reject the object. An in-hold
`memory.oomBump` floor above `maxAllowed` is published at that cap on
the next reconcile. The annotation floor and `holdUntil` stay. Default
memory `allowDecrease` still keeps the live request until decrease is
allowed. The recommendation records that skip.

### Cost Pricing

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `costPricing.cpuPerCoreHour` | string | `"0.031"` | USD per vCPU-hour for cost estimation |
| `costPricing.memoryPerGiBHour` | string | `"0.004"` | USD per GiB-hour for cost estimation |

These values are used to compute `status.savings.estimatedMonthlySavings`
on each `AttunePolicy`. Adjust for your cloud provider or reserved
instance pricing.

### Exclude known sidecars

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `excludeKnownSidecars` | bool | `true` (built-in when unset) | When true, auto-exclude well-known mesh/sidecar container names (`istio-proxy`, `linkerd-proxy`, and others). When false, only each policy's `excludedContainers` list is used (pre-feature behavior). Inherited by policies that leave the field unset. |

### Inheritable UpdateStrategy Fields

All `updateStrategy` fields in `AttuneDefaults` are inherited by policies
that do not set them explicitly. Policy-level values always take precedence.

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `type` | string | `Recommend` | `Observe`, `Recommend`, `OneShot`, `Canary`, `Auto`. OneShot applies at most one needing pod per cycle. Replicas that are already at the applied target, or that are blocked by QoS, node pressure, quota, or Infeasible plus InPlaceOnly, are skipped so another replica can still resize. |
| `cooldown` | duration | `1h` | Minimum time between resizes of the same workload. Other apps on the same policy are not locked. When every matched app is still cooling, the next reconcile waits only until the soonest per-app window expires (a watch event does not restart a full cooldown). Zero is invalid on create: omit the field for the 1h default. The shortest accepted value is 1m. An `AttunePolicy`, `AttuneDefaults`, or `AttuneNamespaceDefaults` object already stored with `0s` can be updated while that value stays, and the controller still waits 1h, including when a policy omits cooldown and inherits `0s`. Changing a positive duration to `0s` is rejected. |
| `autoRevert` | bool | `true` | Revert unsafe resizes automatically |
| `resizeMethod` | string | `InPlaceOnly` | `InPlaceOnly` or `InPlaceOrRecreate` |
| `maxConcurrentResizes` | int32 | `1` (built-in when unset) | Max pods to resize simultaneously. Omitted on the policy so AttuneDefaults can apply before the built-in 1. |
| `maxStatusRecommendations` | *int32 | `100` (operator default) | Cap for `status.recommendations` length; full set still drives resizes |
| `includeExplanationsInStatus` | *bool | `true` | When false, strip recommendation explanation chains from status |
| `maxTotalCpuIncrease` | quantity | (none) | Deprecated. Max aggregate CPU increase per cycle. Prefer `maxCpuIncreasePerMinute`. |
| `maxTotalMemoryIncrease` | quantity | (none) | Deprecated. Max aggregate memory increase per cycle. Prefer `maxMemoryIncreasePerMinute`. |
| `maxCpuIncreasePerMinute` | quantity | (none) | Max aggregate CPU increase per wall-clock minute (token bucket) |
| `maxMemoryIncreasePerMinute` | quantity | (none) | Max aggregate memory increase per wall-clock minute (token bucket) |
| `schedule` | object | (none) | Time windows, days of week, timezone |
| `export` | object | (none) | Metrics export configuration |
| `safetyObservationPeriod` | duration | `5m` | Post-resize observation window. Omit the field for 5m. Zero is invalid on create, and changing a positive duration to `0s` is rejected. The shortest accepted value is 1m. A stored `0s` on `AttunePolicy`, `AttuneDefaults`, or `AttuneNamespaceDefaults` can stay through an unrelated update and is treated as unset (5m, or a positive canary period). |
| `sloGuardrails` | list | `[]` | Application-level SLO PromQL checks after resize |
| `canary` | object | (none) | Canary rollout (`percentage`, `observationPeriod`, `autoPromote`). Omitted or `0s` `observationPeriod` uses the built-in observation period, not a rejected value. `autoPromote` defaults to false. When true, a clean observation period resizes the remaining pods. When false, switch the policy to Auto yourself. CREATE sizing, startup boost, and HPA stay off for an app until that app is promoted. |
| `initialSizing` | bool | `false` | Enable mutating webhook for pod creation |
| `hpaTargetBounds` | object | (none) | Optional percent band for auto-tuned HPA utilization targets. `cpu` and `memory` each have optional `min` and `max` from 1 to 10000. Unset means no band, so today's limit cap stays, including CPU targets above 90. 50 and 90 are a recommended opt-in, not a default. The band does not turn auto-tune on. |

Request increases also skip when this pod plus other pods on the same
node would exceed allocatable. That gate is always on and is not a
policy field. See [Node capacity](../architecture/node-capacity.md).

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

**Precedence order (per field):** policy spec > namespace defaults >
cluster defaults > built-in defaults.

When both cluster and namespace defaults exist, fields set on the
namespace object win; fields left unset on the namespace object are
filled from the cluster `AttuneDefaults` (not from built-ins alone).
Example: cluster sets `cooldown=10m` and `percentile=95`; namespace sets
only `percentile=90` → effective defaults use `percentile=90` and
`cooldown=10m`.

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
| `metricsSource` | `prometheus.address`, `prometheus.headers`, `prometheus.queryParameters`, `prometheus.bearerTokenSecret`, `prometheus.sigv4`, `prometheus.tls`, `datadog.site`, `datadog.apiKeySecretRef`, `cloudwatch.region`, `cloudwatch.clusterName`, `cloudwatch.roleArn`, `cloudwatch.cpuUnit`, `historyWindow`, `minimumDataPoints`, `queryStep`, `rateWindow`, `podAggregation`, `cpuRecordingMetric`, `memoryRecordingMetric` |
| `cpu` | `percentile`, `overhead`, `minAllowed`, `maxAllowed`, `controlledValues`, `burstSensitivity`, `allowDecrease`, `startupBoost`, `surge`, `maxChangePercent`, `maxIncreasePercent`, `maxDecreasePercent` |
| `memory` | Same as `cpu` (no `startupBoost`), plus `decreaseUsageMarginPercent`, `memoryFromCpuRatio`, `oomBump`, and `surge` |
| `updateStrategy` | `type`, `cooldown`, `autoRevert`, `resizeMethod`, `initialSizing`, `maxConcurrentResizes`, `maxStatusRecommendations`, `includeExplanationsInStatus`, `maxTotalCpuIncrease`, `maxTotalMemoryIncrease`, `maxCpuIncreasePerMinute`, `maxMemoryIncreasePerMinute`, `schedule`, `export`, `canary`, `safetyObservationPeriod`, `sloGuardrails`, `templatePersistence`, `hpaTargetBounds` |
| `costPricing` | `cpuPerCoreHour`, `memoryPerGiBHour` |

`containerPolicies` is not part of `AttuneDefaultsSpec`. `AttuneNamespaceDefaults` uses that same spec, so the list is not in this table and is not inherited. Set it on each `AttunePolicy`.

## Alternative Metrics Sources

By default, Attune queries Prometheus for CPU and memory usage data.
The CRD also supports Datadog, CloudWatch Container Insights, and VPA
as alternative metrics sources. **At most one** of `prometheus`,
`datadog`, `cloudwatch`, or `vpa` may be set on a policy or on
`AttuneDefaults` / `AttuneNamespaceDefaults`. Omitting
`spec.metricsSource` on an `AttunePolicy` is valid; `MergeDefaults`
copies the provider from namespace or cluster defaults before the
operator queries. The wizard inherit option uses that omit shape.

> The Datadog collector queries the `/api/v1/query` endpoint and converts
> nanocores to cores automatically. The CloudWatch collector uses the
> Container Insights `ContainerInsights` namespace and supports IRSA/Pod
> Identity credentials with optional cross-account role assumption.

### Prometheus

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `metricsSource.prometheus.bearerTokenSecret` | object | (optional) | Secret `name` + `key` for a bearer token in the **policy** namespace (`AttunePolicy` or `AttuneNamespaceDefaults`). Deprecated on cluster `AttuneDefaults`: the name is still inherited and read in each policy namespace; use `prometheusAuth` or `openshift.bindClusterMonitoringView` instead. Amazon Managed Prometheus does not use this Secret. Attune trims both ends of the token and keeps interior spaces. |
| `metricsSource.prometheus.sigv4.region` | string | (required when `sigv4` is set) | AWS region of the Amazon Managed Prometheus workspace, for example `us-east-1`. There is no default. Attune signs queries with SigV4 service `aps`. Omitted `sigv4` does not sign. Do not combine with `bearerTokenSecret`, an `Authorization` header, or an `X-Amz-*` header. |
| `metricsSource.prometheus.sigv4.roleArn` | string | (optional) | IAM role ARN to assume. Empty uses the pod identity chain (IRSA or Pod Identity). The role needs `aps:QueryMetrics`. |
| `metricsSource.prometheus.tls.insecureSkipVerify` | bool | `false` | Skip TLS certificate verification. Use only for a self-signed development endpoint. Prefer the cluster CA when you have the bundle. |

### Datadog

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `metricsSource.datadog.site` | string | `datadoghq.com` | Datadog site (e.g., `datadoghq.eu`, `us5.datadoghq.com`, `ddog-gov.com`) |
| `metricsSource.datadog.apiKeySecretRef.name` | string | (required) | Secret name for the Datadog API key. On `AttunePolicy` or `AttuneNamespaceDefaults` the Secret is in that namespace. On cluster `AttuneDefaults` the name is still copied onto each policy; set `datadogAuth.existingSecret` so a cluster-chosen block reads the operator namespace instead. Attune trims both ends of the API key and the optional `app-key`, and keeps interior spaces. A whitespace-only `app-key` is omitted. |
| `metricsSource.datadog.apiKeySecretRef.key` | string | (required) | Key within the Secret that holds the API key. When `datadogAuth.existingSecret` is set for a cluster-chosen Datadog block, the data key is `datadogAuth.existingSecret.key` (`--datadog-api-key-secret-key`), not this inherited key. |

### CloudWatch Container Insights

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `metricsSource.cloudwatch.region` | string | (required) | AWS region (e.g., `us-east-1`) |
| `metricsSource.cloudwatch.clusterName` | string | (required) | EKS cluster name for Container Insights metric filtering (1-100 chars, alphanumeric / hyphen / underscore) |
| `metricsSource.cloudwatch.roleArn` | string | `""` | Optional IAM role ARN for cross-account access (`arn:aws:iam::ACCOUNT:role/NAME`; IRSA/Pod Identity used if empty) |
| `metricsSource.cloudwatch.cpuUnit` | string | `Millicores` | Scale of `container_cpu_usage_total`. Millicores divides by 1000. Cores leaves the value unchanged. Nanocores divides by 1e9. Empty means Millicores. |

## Policy-Level Fields

Bound fields (`minAllowed`, `maxAllowed`) and their admission caps
(256 cores CPU, 16Ti memory) are documented under
[Resource configuration](#resource-configuration). The same fields and
caps apply to `AttunePolicy`, `AttuneDefaults`, and
`AttuneNamespaceDefaults`.

### spec.targetRef.kind

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `targetRef.kind` | string | `Deployment` | Workload kind. One of `Deployment`, `StatefulSet`, `DaemonSet`, `CronJob`, `Job`, `ReplicaSet`, or `Rollout`. `Rollout` is `argoproj.io/v1alpha1` from Argo Rollouts. Attune does not install that CRD and does not watch it. A policy that sets `kind: Rollout` before the CRD exists becomes Ready False with reason `WorkloadCRDMissing` and retries. Other policies are unchanged. Pods are selected with `spec.selector`, including `matchExpressions`. The pod owner is the child ReplicaSet. Resize waits while the Rollout phase is `Paused`, `Progressing`, or `Degraded`, or while `status.abort` is true. `Degraded` is an aborted, timed-out, or invalid spec, so it waits even when `status.abort` is false and replica counts match. A `Healthy` phase, or an empty phase, does not wait because `status.updatedReplicas` is behind `spec.replicas`. That lag is a scale-out. Argo reports a replica lag as `Progressing`. Recommendations are still stored. Template persistence patches the Rollout `spec.template`, not the child ReplicaSet. If `spec.workloadRef` is set and `spec.template` has no containers, recommendations use the referenced Deployment, StatefulSet, or ReplicaSet pod template. The Rollout template is still not patched. If that object cannot be read, or it has no containers, `TemplatePersistence` is False with reason `WorkloadRefUnread`, and that reason stays for the reconcile. A readable reference uses reason `TemplateWorkloadRef` instead. CREATE initial sizing does not resolve a Rollout owner. A CronJob pod name ends in the CronJob controller minute stamp (unix time divided by 60: 8 digits until 2160-02-18, 9 digits after that) and a 5-character hash. Indexed completion inserts the completion index before that hash. The suffix is not a 10-digit unix-seconds timestamp. |

### Resize while a StatefulSet partition is held

A RollingUpdate StatefulSet resizes a pod whose `controller-revision-hash` matches `status.updateRevision`. Pods on the older revision stay skipped while `status.currentRevision` and `status.updateRevision` differ. A held `partition` keeps that difference on purpose, so those pods stay skipped until the revisions match. The pod ordinal is not read. A stale generation (`metadata.generation` ahead of `status.observedGeneration`) skips every pod, including pods already on `updateRevision`. OnDelete is not this skip. Those pods are resized.

### spec.paused

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `runtimeProfile` | string | (none) | Optional language/runtime profile (`generic`, `java`, `python`, `golang`, `nodejs`). Applies safe memory defaults and admission warnings. See [runtime profiles](../guides/runtime-profiles.md). |
| `weight` | int32 | `100` | Priority when two policies match the same workload. Range 1 to 1000. The higher value applies. Equal weights: the lexicographically smaller policy name wins, and the other defers. This field is on `AttunePolicy` only. |
| `paused` | bool | `false` | Halts all reconciliation for this policy: no metrics collection, no recommendations, no resizes. Existing resizes are not reverted. The operator sets `Ready=False` with `reason=Paused`. |

### Namespace freeze (`attune.io/freeze`)

Annotate a namespace to stop apply during an incident without pausing
recommendation computation. The value must be exactly `true`, the same
parser as `attune.io/skip` (`True`, `1`, and `yes` do not freeze).

```bash
kubectl annotate namespace <ns> attune.io/freeze=true
```

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: production
  annotations:
    attune.io/freeze: "true"
```

| Annotation | Scope | Effect |
|------------|-------|--------|
| `attune.io/freeze=true` | Namespace | Skip new apply: in-place resize, eviction, startup boost, template persist, and CREATE initial sizing. Metrics, recommendations, status, and export still update. Pending safety observation still reverts unsafe pods and restores AfterSuccessfulResize templates. `ResizeBlocked=True` with `reason=NamespaceFrozen`. |
| `attune.io/skip=true` | Workload | Skip that workload entirely (no recommendations). Independent of freeze. |

If the operator cannot read the namespace, apply is skipped (fail closed)
and the same `NamespaceFrozen` reason is set. Freeze is not a rollback of
already-applied successful recommendations. Pending safety observation
still reverts unsafe pods and restores AfterSuccessfulResize templates.
Remove the annotation to resume apply on the next reconcile.

### Container exclusion

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `excludeKnownSidecars` | bool | `true` | When true, skip well-known mesh/sidecar names in addition to `excludedContainers`. Set `false` to restore list-only behavior. |
| `excludedContainers` | []string | `[]` | Extra container names to skip. When `excludeKnownSidecars` is true, this list is **unioned** with the built-in known list (never replaces it). |

Built-in known names include `istio-proxy`, `linkerd-proxy`, `consul-dataplane`,
`kuma-dp`, `vault-agent`, `cloud-sql-proxy`, `cloudsql-proxy`, and `gce-proxy`.

### Per-container policies

`containerPolicies` is on `AttunePolicy` only. It is not a field of
`AttuneDefaults` or `AttuneNamespaceDefaults`. An omitted or empty list
keeps one shared CPU engine and one shared memory engine from `spec.cpu`
and `spec.memory`. Policies that omit the list behave as they do today.

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `containerPolicies` | list | omitted | Up to 100 entries. Exact case-sensitive container names. No regular expressions. |
| `containerPolicies[].containerName` | string | required | Container name, or `*` once, as a field-wise fallback. |
| `containerPolicies[].cpu` / `memory` | object | omitted | Optional `ResourceConfig`. Omitted `maxAllowed` inherits `*` and then the policy max. It is uncapped only when that effective value is nil. |

v1 reads these fields from a container entry: `percentile`, `overhead`,
`minAllowed`, `maxAllowed`, `burstSensitivity`, `maxChangePercent`,
`maxIncreasePercent`, `maxDecreasePercent`, `allowDecrease`, and
`controlledValues`.

Per field, a literal container name wins over `*`, and `*` wins over the
merged policy block. That block is already merged from the policy, then
`AttuneNamespaceDefaults`, then `AttuneDefaults`. `kubectl attune explain`
prints that winner as `container`, `wildcard`, `policy`, `namespace defaults`,
`cluster defaults`, `defaults`, or `built-in`. Percentile `0`, overhead
`""`, and nil pointers are unset. Overhead `"0"` is set and does not
inherit `"20"`. An unset CPU `allowDecrease` still allows decreases. An
unset memory `allowDecrease` still blocks them. An omitted
`maxAllowed` inherits `*` and then the policy max. A container entry
cannot clear a policy max. The effective value is uncapped only when
it is still nil. Same-block minAllowed above maxAllowed on a container
entry is rejected by the webhook. A defaults min is included when the
container omits min and the effective max is already known. The CRD
quantity rule stays on `spec.cpu` and `spec.memory` only. Copying it onto each of the 100
container entries exceeds the API server CEL cost budget.

`excludedContainers` and `excludeKnownSidecars` win before any container
entry. With the default `excludeKnownSidecars: true`, `istio-proxy` stays
excluded until the policy sets `excludeKnownSidecars: false` and does not
list the name. Only app containers and init containers with
`restartPolicy: Always` are recommended. A normal init is not managed,
even when it is named here.

`startupBoost`, `memoryFromCpuRatio`, `decreaseUsageMarginPercent`,
`limitMultiplier`, `oomBump`, and `surge` stay policy-wide. The webhook
rejects them on a container entry. Policy-level copies still apply to
every container that is not excluded. A container `maxAllowed` caps
policy startup boost and a policy memory OOM bump. After the field-wise
merge, `minAllowed` above `maxAllowed` is rejected, including a
defaults min when the container omits min and a max is already known.
Container `maxAllowed` uses the same 256-core and 16Ti ceilings as `spec.cpu` and
`spec.memory`. No extra Prometheus metric is emitted for this list.

```yaml
containerPolicies:
  - containerName: "*"
    cpu:
      maxAllowed: "300m"
  - containerName: sidecar
    cpu:
      maxAllowed: "200m"
```

### Template persistence

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `updateStrategy.templatePersistence.enabled` | bool | `false` | When true, write recommended resources into Deployment/StatefulSet pod templates so new pods start correctly sized. **Opt-in only.** Do not enable under unmanaged GitOps without adopting recommendations in Git; prefer `export` or `initialSizing` instead. |
| `updateStrategy.templatePersistence.when` | string | `AfterSuccessfulResize` | `AfterSuccessfulResize`: patch template after a successful in-place resize or a successful Eviction (`InPlaceOrRecreate`) so replacement pods start from the new requests. The write uses dest-clamped apply `To` from resize history, not the raw rec, so leftover dest limits do not land 500m on the template after live applied 200m. `OnRecommendation`: patch when a recommendation is accepted (works in Recommend mode). |

Template changes trigger a rolling update. The operator no-ops when the template already matches and skips patches mid-rollout. **Observe mode never patches.** **Canary** defers template writes until `FullRollout` so a partial canary resize does not roll the whole fleet via the template. Requests are clamped to limits the same way as live resize.

### Export and GitOps pull requests

Write recommendations to ConfigMaps for Argo CD / Flux, and optionally open
provider PRs when templates drift. Full cookbook:
[GitOps integration](../guides/gitops-integration.md).

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `updateStrategy.export.configMap` | bool | `false` | When true, write versioned recommendation ConfigMaps named `<policy>-<workload>-recommendations` (schema key `schema-version: v1`). |
| `updateStrategy.export.pullRequest.enabled` | bool | `false` | Opt-in PR automation (GitHub or GitLab). Requires `repository` and `tokenSecretRef` when enabled. |
| `updateStrategy.export.pullRequest.provider` | string | `github` | `github` or `gitlab`. |
| `updateStrategy.export.pullRequest.repository` | string | (required when enabled) | `owner/repo` (GitHub) or project path/id (GitLab). |
| `updateStrategy.export.pullRequest.tokenSecretRef` | object | (required when enabled) | Secret `name` + `key` for a fine-scoped API token. Never log the token. The creating user must be allowed to get that Secret. |
| `updateStrategy.export.pullRequest.apiUrl` | string | provider default | Enterprise GitHub or self-hosted GitLab API base (SSRF-validated). |
| `updateStrategy.export.pullRequest.allowPrivateEndpoints` | bool | `false` | Permit RFC1918/ULA API hosts for self-hosted forges. Loopback, link-local (IMDS), and unspecified stay blocked. |
| `updateStrategy.export.pullRequest.baseBranch` | string | `main` | Target branch for the PR. |
| `updateStrategy.export.pullRequest.cooldown` | duration | `24h` | Minimum time between PR create/update attempts for this policy. |
| `updateStrategy.export.pullRequest.minChangePercent` | int32 | `10` | Minimum absolute percent change (per container resource vs template) to open or update a PR (1-100). |
| `updateStrategy.export.pullRequest.dryRun` | bool | `false` | When true, set status only (`PullRequestDryRun`); no remote API call. |
| `updateStrategy.export.pullRequest.labels` | []string | `[]` | Labels applied to the PR when the provider supports them (max 20). |

Status condition `GitOpsPullRequest` reports dry-run, open, no-drift,
unchanged, cooldown, failed, or disabled. Metric: `attune_gitops_pr_total`.
Inspect export ConfigMaps
with `kubectl attune export list` and effective PR settings with
`kubectl attune explain <policy>`.

### Directional Change Caps

Per-resource fields in `cpu` and `memory` that limit how much a recommendation can change per cycle:

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `maxIncreasePercent` | int32 | inherits `maxChangePercent` | Maximum percentage increase allowed per resize cycle. If unset, falls back to `maxChangePercent` (CPU: 50, memory: 30). |
| `maxDecreasePercent` | int32 | inherits `maxChangePercent` | Maximum percentage decrease allowed per resize cycle. If unset, falls back to `maxChangePercent` (CPU: 50, memory: 30). |
| `maxChangePercent` | int32 | CPU: `50`, memory: `30` | Symmetric change cap. Used as fallback for `maxIncreasePercent` and `maxDecreasePercent` when they are unset. |
| `decreaseUsageMarginPercent` | int32 | memory: `10` (CPU: ignored) | Minimum headroom above recent memory usage when decreasing memory **limits**. Target limit must be at least `usage * (1 + margin/100)`, where usage is the recommendation raw percentile. Prevents client-side OOM races on Kubernetes 1.35+. Set `0` to require limit strictly above usage. |

### Controlled Values

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `controlledValues` | string | `RequestsOnly` | `RequestsOnly` adjusts only requests and keeps the live limit. `RequestsAndLimits` scales the limit with the request. Omitted `limitMultiplier` keeps the live ratio. Explicit `"1"` forces equality. `maxAllowed` caps the engine request, not the limit. A Guaranteed memory raise can set the applied request above `memory.maxAllowed`. `RequestsOnly` can still change QoS when a live limit already equals the new request. That resize is skipped. |
| `cpu.limitMultiplier` | string | (none) | Multiple applied to the new CPU request when `cpu.controlledValues` is `RequestsAndLimits`. Omitted keeps the live request-to-limit ratio. Explicit `"1"` forces the limit equal to the new request. CPU limits keep millicore precision. A container with no current limit keeps that limit omitted. `RequestsOnly` plus a multiplier on the same object is rejected. A multiplier set on the policy also requires `RequestsAndLimits` on that same object when `controlledValues` is omitted or empty. Admission rejects that apply. A stored object is not rewritten and stays `InvalidConfig` until edited. `AttuneDefaults` may carry a multiplier without `RequestsAndLimits`. A policy that sets `controlledValues: RequestsOnly` does not inherit that multiplier. A policy that omits `controlledValues` still inherits it, then built-in defaults fill `RequestsOnly`, and reconcile reports `InvalidConfig`. Minimum `1`. Maximum `100`. Values below 1 are rejected because a limit cannot be smaller than its request. Zero, negative, NaN, Inf, and values above 100 are rejected. `maxAllowed` caps the engine request, not the limit. A multiplier that would change a Guaranteed pod to another QoS class is skipped and is not evicted. `resizeMethod: InPlaceOrRecreate` does not evict that pod either. |
| `memory.limitMultiplier` | string | (none) | Multiple applied to the new memory request when `memory.controlledValues` is `RequestsAndLimits`. Omitted keeps the live request-to-limit ratio. Explicit `"1"` sets the limit to the new request, rounded up to a whole byte. Scaled memory limits are rounded up to a whole byte, with or without a multiplier, to float64 precision: a product within 1e-14 (relative) of a whole byte snaps to that byte. A container with no current limit keeps that limit omitted. `RequestsOnly` plus a multiplier on the same object is rejected. A multiplier set on the policy also requires `RequestsAndLimits` on that same object when `controlledValues` is omitted or empty. Admission rejects that apply. A stored object is not rewritten and stays `InvalidConfig` until edited. `AttuneDefaults` may carry a multiplier without `RequestsAndLimits`. A policy that sets `controlledValues: RequestsOnly` does not inherit that multiplier. A policy that omits `controlledValues` still inherits it, then built-in defaults fill `RequestsOnly`, and reconcile reports `InvalidConfig`. Minimum `1`. Maximum `100`. Values below 1 are rejected because a limit cannot be smaller than its request. Zero, negative, NaN, Inf, and values above 100 are rejected. `maxAllowed` caps the engine request, not the limit. On a Guaranteed pod the request is raised to the multiplied limit so the request stays equal to the limit. That applied request can exceed `memory.maxAllowed`. The pod stays Guaranteed. |

When `controlledValues` is `RequestsAndLimits`, HPA auto-tune caps the CPU
utilization target using the multiplied CPU limit. A limit above
`maxAllowed` is still that cap. The cap is not the bare new request.

### Allow Decrease

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `cpu.allowDecrease` | bool | `true` | Whether CPU requests can be decreased. When `true`, the safety monitor checks for throttling after each decrease. |
| `memory.allowDecrease` | bool | `false` | Whether memory requests can be decreased. Defaults to `false` to prevent OOMKill from sudden memory reductions. |
| `memory.decreaseUsageMarginPercent` | int32 | `10` | See `decreaseUsageMarginPercent` above. Only meaningful when memory limit decreases are allowed (Kubernetes 1.35+ and `controlledValues: RequestsAndLimits`). |

### Startup Boost

Temporarily increases CPU requests for newly created or restarted pods to accelerate JVM/.NET class loading, JIT compilation, and cache warming.

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `startupBoost.multiplier` | string | (none) | Scales the recommended CPU request during startup. For example, `"3.0"` means 3x the steady-state recommendation. Must be > 1.0 and <= 10.0. This is not `limitMultiplier`. During the boost window the CPU limit is the greater of the boosted request and the steady multiplied limit, not the boosted request times `limitMultiplier`. Expiry restores that steady limit. |
| `startupBoost.duration` | duration | (none) | How long the boost lasts before reducing to the steady-state recommendation. Must be >= 10s and <= 1h. CREATE and live reconcile dest-cap the boosted request at leftover dest when `controlledValues` is `RequestsOnly`. When it is `RequestsAndLimits` and a rec dest is set, dest-cap uses that rec dest (leftover dest is not a skip). Job and CronJob pods skip CREATE boost because expiry cannot run. |
| `startupBoost.excludeFromHistory` | bool | omitted (false) | When true, drop CPU samples from the percentile while the pod is inside startup. The window starts at `attune.io/startup-boost-at` when that annotation is set and not before pod creation, otherwise at `CreationTimestamp`. The cutoff is that start plus `startupBoost.duration` plus `rateWindow`. A sample at the cutoff stays. Nil and false keep today's percentile. Memory samples are unchanged. Deleted pods stay in history until `historyWindow` because there is no creation time or stamp to cut on. A recreated pod keeps samples older than its new `CreationTimestamp`. A series with no pod label is left unfiltered. |

`startupBoost` is policy-wide. A container `maxAllowed` from
`containerPolicies` caps the boosted CPU. `memoryFromCpuRatio`,
`decreaseUsageMarginPercent`, `limitMultiplier`, `oomBump`, and `surge`
are policy-wide the same way. The webhook rejects those fields on a
container entry.

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

### Usage surge

Shortens the history window while recent usage is hot. The block is absent by default, so the resource stays on the long window. An empty `surge: {}` turns the feature on for that resource and fills the defaults below. CPU and memory each have their own block. There is no `surge: false`. A policy that omits `surge` inherits an `AttuneDefaults` surge. To keep a workload off, omit `surge` on both.

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `cpu.surge` / `memory.surge` | object | absent | Off until this object is set. `{}` turns the feature on and fills trigger ratio, percentile, and window. |
| `surge.triggerRatio` | string | `1.5` when the block is set | Short percentile divided by the long-window percentile. Must be greater than 1 and at most 100. Empty is filled with `1.5`. |
| `surge.percentile` | int | `99` when the block is set | Percentile of the short window. One of 50, 90, 95, 99. This does not replace the parent percentile on the long window. |
| `surge.window` | duration | `30m` when the block is set | How far back the short window reaches. At least 5m. On a policy, must not be longer than the effective history: `metricsSource.historyWindow` on the policy, then namespace defaults, then cluster defaults, then `168h`. On AttuneDefaults or AttuneNamespaceDefaults, the limit is that object's own `historyWindow`, or `168h`. A window equal to that limit is accepted. A policy that sets `historyWindow` needs no defaults read for the surge check. It still lists defaults when the policy or a named container omits `minAllowed` and sets `maxAllowed`. Explicit `0s` is invalid. |

The long statistic is still the max of the overall percentile and the 24 hour-of-day percentiles, at the parent percentile. The short statistic is the overall percentile only, of finite samples inside the window. Attune fires when the window drops older finite samples, at least 3 finite samples remain (and at least half of `window / queryStep` when the step is positive), and the short percentile is at least `triggerRatio` times the long percentile. A long percentile of 0 fires when the short percentile is positive. One spike in the short window does not fire. `minimumDataPoints` still gates the long window only. When the short window is selected, confidence stays the long window's confidence. Burst still runs on the chosen profile. `explanation.<resource>.finalAdjustment` includes `surge` on the resource that used the short window. `memoryFromCpuRatio` follows the CPU request and does not switch the memory sample set.

Example:

```yaml
cpu:
  surge: {}
```

### Memory-from-CPU Derivation

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `memory.memoryFromCpuRatio` | string | (none) | Derives memory from the CPU recommendation (GiB per core) instead of the memory signal from the active source (Prometheus usage or VPA memory target). For example, `"2.0"` means 1 core = 2 GiB memory. Useful for JVM and heap-bound workloads where memory is proportional to CPU. The derived value still goes through min/max/change caps. When a valid ratio is set and CPU samples are below `minimumDataPoints` (or the VPA CPU target is unset), Attune does not fall back to the memory signal: Ready stays `InsufficientData` and reconcile retries at `min(cooldown, queryStep)` until a CPU recommendation exists. A CPU query error sets Ready to `MetricsUnavailable` and uses the same short requeue (no `requeueJitter`). An invalid ratio (non-numeric, non-positive, or above 1000) is ignored and Attune uses the memory signal; the webhook rejects these when admission is enabled. A prior rec whose memory explanation contains `memoryFromCpuRatio` is kept as Stale across a CPU-only gap. |

### OOM bump

Raises the memory request after `OOMKilled`. The block is absent by default, so memory requests stay on the percentile path. An empty `oomBump: {}` turns the feature on and fills the defaults below. A `cpu.oomBump` block is rejected. The count increments only after a successful resize.

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `memory.oomBump` | object | absent | Off until this object is set. `{}` turns the feature on and fills ratio, minBump, maxBumps, and hold. |
| `oomBump.ratio` | string | `1.2` when the block is set | Multiplies the original memory request at each successful step. Minimum 1. Maximum 10. `"1.2"` means 20 percent per step. |
| `oomBump.minBump` | quantity | `100Mi` when the block is set | Added to the original request once per successful step. Must be positive. The step uses whichever of the ratio and this floor is larger. |
| `oomBump.maxBumps` | int32 | `3` when the block is set | Integer from 1 through 10. How many successful steps are allowed from the original request. When `maxAllowed` is omitted, this is the only cap. |
| `oomBump.hold` | duration | `24h` when the block is set | How long the applied floor stays above a lower percentile. Minimum `1m`. Maximum `168h`. A newer OOM during the hold still steps above the live request. Auto, OneShot, and Canary record that signal on the pod. The hold stops the percentile from falling below the floor. It does not ignore a new OOM. Expiry does not clear the original request stored on the pod. |

The step is `max(ceil(origin * ratio^count), origin + minBump * count)`, then `maxAllowed`. Origin is the live memory request before the first bump of the streak, not the latest live request and not the pod template. During hold, a newer OOM whose next origin step is not above the live request takes one step from that live request and keeps the original origin. Auto, OneShot, and Canary record this `oomAt` and restart on the pod. The step is still clamped to `maxAllowed`. When `maxAllowed` is already at or below the live request, Auto, OneShot, and Canary store the signal once as skipped and `count` does not increase. Recommend and Observe do not write that stamp, so the same OOM still counts as `skipped` on every reconcile. After `hold` expires, recommendations follow the normal percentile, `allowDecrease`, and template rules. A later OOM can step again from that same origin until `maxBumps`.

A held floor is published again on later reconciles. If `maxAllowed` is set and that floor is above it, the published request is the cap. Omitted `maxAllowed` does not cap. The stored floor, count, and `holdUntil` stay. The clamp does not extend the hold and is not a new OOM. A new OOM step is still clamped once inside the step math and is not clamped again below `maxAllowed`. Default memory `allowDecrease` is false. A clamp below the highest in-hold pod request is not resized until decrease is allowed. That request can sit above the workload template when template persistence is off. The recommendation keeps it and records the `allowDecrease` skip. A replica still under the cap is raised only to the cap. When `allowDecrease` is true, the recommendation shows the cap. Recommend does not resize. Observe does not resize. Auto, OneShot, and Canary resize down only when decrease is allowed.

A safety revert keeps the memory request at or above the bump floor. It raises a positive memory limit to that floor only when this container's effective `controlledValues` is `RequestsAndLimits`. An empty `containerPolicies` list uses `spec.memory.controlledValues`. A literal container name beats `*`, and `*` beats the policy block. A zero or missing limit is not created. `oomBump` itself is not settable on a container entry.

Attune stores the streak on the pod annotation `attune.io/oom-bump.<container>`. The container name must fit so the name segment `oom-bump.<container>` is at most 63 characters. The name is not truncated. The value is `count=<n>,origin=<qty>,floor=<qty>,oomAt=<RFC3339>,restart=<n>,holdUntil=<RFC3339>`.

Example:

```yaml
memory:
  oomBump: {}
```

### SLO Guardrails

Application-level PromQL checks evaluated after each resize during the safety observation period.

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `updateStrategy.sloGuardrails[].name` | string | (required) | Identifies this guardrail for logging and status |
| `updateStrategy.sloGuardrails[].query` | string | (required) | PromQL query returning a scalar. Supports `{{ .Namespace }}`, `{{ .WorkloadName }}`, `{{ .PodName }}` template variables. |
| `updateStrategy.sloGuardrails[].threshold` | string | (required) | Value that triggers a revert |
| `updateStrategy.sloGuardrails[].comparison` | string | `above` | `above` (revert when value > threshold) or `below` |
| `updateStrategy.sloGuardrails[].evaluationWindow` | duration | `5m` | How long after resize to check. Omit the field for 5m. Zero is invalid on create, and changing a positive window to `0s` is rejected. An unchanged stored `0s` on `AttunePolicy`, `AttuneDefaults`, or `AttuneNamespaceDefaults` is accepted only when the stored entry at the same list index is also `0s`, and uses the 5m default. The shortest accepted positive value is 1m. |

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

`memory.memoryFromCpuRatio` applies here too: when a valid ratio is set,
memory is derived from the CPU recommendation instead of the VPA memory
target. If the VPA CPU target is unset, Attune waits (`InsufficientData`)
instead of using the VPA memory target, retries at
`min(cooldown, queryStep)`, and keeps a prior ratio-derived rec as Stale.

Set that VPA to `updateMode: Off`. Attune then consumes
`status.recommendation.containerRecommendations[].target` and applies
via `/resize`. An omitted `cpu` or `memory` key in `target` is unset,
not zero: Attune holds the live pod request when pods are listed,
otherwise the last rec or template request, instead of running the
engine on `0`. An applying VPA on the same
workload still emits `VPAConflict`.

At most one of `prometheus`, `datadog`, `cloudwatch`, or `vpa` may be set
on a policy or on `AttuneDefaults` / `AttuneNamespaceDefaults`.

### Initial Sizing

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `updateStrategy.initialSizing` | bool | `false` | When true and the initial sizing webhook is enabled, new pods matching this policy (by `targetRef.name` or `targetRef.selector`) receive recommended resources at creation time via a mutating admission webhook, clamped so requests do not exceed leftover limits (same as live resize). Native sidecars are included. When `cpu.startupBoost` is set and `controlledValues` is `RequestsAndLimits`, CREATE writes rec dest first when rec dest is set, then raises dest to the boosted request (capped at `maxAllowed`). A zero rec dest dest-caps leftover dest only and does not invent dest. `RequestsOnly` still dest-caps leftover dest. CREATE stamps `attune.io/startup-boost-at` so the next reconcile does not shrink the pod during the boost window. Owner Get includes Deployment, StatefulSet, DaemonSet, ReplicaSet, Job, and CronJob. A Deployment-owned ReplicaSet is resolved to the Deployment and is not a ReplicaSet target. CronJob pods are owned by a Job; CREATE walks that Job to the CronJob so recommend-only batch recs can land (in-place resize and persist skip Jobs). Recommend (the CronJob default) is allowed for Job and CronJob CREATE; Observe still skips. `initialSizing`, `type`, and `controlledValues` follow the same merge as reconcile (`AttuneDefaults` then `AttuneNamespaceDefaults`, then the policy). A defaults list error skips CREATE (fail closed). `spec.paused` skips CREATE. `ResizeBlocked=HPAListUnavailable` or `VPAListUnavailable` also skips CREATE so leftover recs are not applied while list-based apply is blocked. Requires the namespace label `attune.io/initial-sizing=enabled`. A rec is used when every container has confidence at least 0.5, or this workload already has a successful in-place resize. Canary still skips CREATE for an app until that app is promoted (or the assigned pod name is already in the canary slice). ReplicaSet CREATE with an empty name is not treated as a slice identity. |

## Status Conditions

The controller sets these conditions on each `AttunePolicy`:

| Condition | Reasons | Description |
|-----------|---------|-------------|
| `Ready` | `Monitoring`, `InsufficientData`, `NoWorkloadsFound`, `MetricsUnavailable` (alias `PrometheusUnavailable`), `InvalidConfig`, `WorkloadDiscoveryFailed`, `WorkloadCRDMissing`, `ConflictCheckFailed`, `Paused`, `PrometheusSeriesCapped` | Overall health. `PrometheusSeriesCapped` keeps Ready True: the reconcile succeeded and the query result is partial. See [Ready reason: PrometheusSeriesCapped](../guides/scaling.md#ready-reason-prometheusseriescapped). `WorkloadCRDMissing` means `targetRef.kind` is `Rollout` and `argoproj.io/v1alpha1` Rollout is not installed. Reconcile succeeds and retries. This is not `InvalidConfig`. |
| `Resizing` | `InProgress`, `Idle`, `CooldownActive` | Latest reconcile (only in resize modes). `InProgress` means this cycle resized at least one workload in place. `CooldownActive` means every workload that has a recommendation this cycle is still cooling and this cycle resized nothing. `Idle` means this cycle resized nothing and cooldown is not active. |
| `Degraded` | `HighRevertRate` | Set when 3+ of the last 5 resizes were reverted |
| `ScheduleBlocked` | `OutsideWindow`, `InsideWindow` | Set when `updateStrategy.schedule` is configured; indicates whether the current time is within an allowed resize window |
| `ResizeBlocked` | `NamespaceFrozen`, `HPAListUnavailable`, `VPAListUnavailable`, `PodsDeferred`, `PodsInfeasible`, `PodsDeferredAndInfeasible` | Namespace freeze kill-switch, HPA or VPA list failure (in-place resize, persist, boost, and CREATE skipped), or pods stuck Deferred or Infeasible; see troubleshooting "NamespaceFrozen", "HPAListUnavailable", "VPAListUnavailable", and "Deferred or Infeasible resize" |
| `SafetyObservation` | `Observing`, `Evaluating`, `RestorePending`, `Incomplete` | True while pods still carry `attune.io` resize-tracking annotations. Derived from those annotations each reconcile; not a second in-memory store. Removed when no tracked pods remain. |
| `TemplatePersistence` | `TemplateWorkloadRef` | False when a Rollout `spec.workloadRef` was read. Attune does not patch that template. Recommendations still read the referenced pod template. Ready stays independent. This reason is not written over `WorkloadRefUnread`. Removed when no targeted Rollout has `spec.workloadRef` and the unread reason is not set. |
| `TemplatePersistence` | `WorkloadRefUnread` | False when the referenced object cannot be read or has no containers. No recommendation is stored for that Rollout. Template persistence leaves this reason in place for that reconcile, including Recommend mode. Removed on a later reconcile whose workload errors no longer include a workloadRef read failure. |
| `GitOpsPullRequest` | `PullRequestOpen`, `PullRequestFailed`, `GitOpsEndpointBlocked`, `NoDrift`, `PullRequestUnchanged`, `PullRequestCooldown`, `PullRequestDryRun`, `PullRequestDisabled` | Opt-in `export.pullRequest` automation status (see [GitOps integration](../guides/gitops-integration.md)) |

`explanation.memory.finalAdjustment` can include `oomBump` when the published memory request was raised or held by `memory.oomBump`. The block is absent by default, so this note is not written until `oomBump` is set.

`explanation.cpu.finalAdjustment` or `explanation.memory.finalAdjustment` can include `surge` when that resource used the short window. The block is absent by default, so this note is not written until `surge` is set. A memory request derived with `memoryFromCpuRatio` does not add `surge` to the memory note.

`status.workloads.resized` counts workloads with a successful in-place resize in the latest reconcile: this cycle's apply, plus a successful in-place row a concurrent reconcile wrote during this reconcile. Rows already in the snapshot do not count, and neither does a success from an earlier hour. `status.resizeHistory` is the retained list (capped at 50 entries, not a time window). It is not the source of `workloads.resized`. An idle reconcile stores `resized: 0` even when older successes are still in that list.

`status.workloads.pending` is `workloads.withRecommendations - workloads.resized`, floored at 0. After an idle cycle it equals the recommendation count.

### Status fields (GitOps PR)

`status.gitopsPR` is written alongside the `attune.io/gitops-pr-*`
annotations. Status survives a Flux or Argo apply that replaces
`metadata.annotations` on the policy. Reads prefer status, then
annotations. Unchanged skip does not rewrite wiped annotations
(that would fight GitOps apply).

| Field | Type | Description |
|-------|------|-------------|
| `status.gitopsPR.driftFingerprint` | string | Stable hash of the last notified drift table. Matching live drift sets `PullRequestUnchanged` instead of opening another empty PR. |
| `status.gitopsPR.lastAttempt` | date-time | When Attune last tried to open or update a PR (also used for cooldown). Dry-run does not set this. |
| `status.gitopsPR.url` | string | Last successfully opened or updated pull request URL. |

## Exponential Backoff

When consecutive resizes of the **same workload** are reverted, that
app's cooldown doubles per revert (capped at 16x). A successful resize
on that app resets its multiplier. Other apps on the policy keep their
own timer. `status.cooldown` reports the highest backoff among apps.

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
