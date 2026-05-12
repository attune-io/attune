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

## Resources

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `resources.limits.cpu` | string | `500m` | CPU limit for the operator pod |
| `resources.limits.memory` | string | `256Mi` | Memory limit for the operator pod |
| `resources.requests.cpu` | string | `100m` | CPU request for the operator pod |
| `resources.requests.memory` | string | `128Mi` | Memory request for the operator pod |

## Scheduling

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `nodeSelector` | object | `{}` | Node selector for operator pods |
| `tolerations` | list | `[]` | Tolerations for operator pods |
| `affinity` | object | `{}` | Affinity rules for operator pods |
| `topologySpreadConstraints` | list | `[]` | Topology spread constraints for operator pods |

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

## Collector Cache

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `collectorTTL` | string | `"10m"` | How long unused Prometheus collectors stay cached before eviction. Maps to the `--collector-ttl` manager flag. Increase if policies frequently rotate Prometheus addresses; decrease in memory-constrained environments. |

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

### Status Conditions

The controller sets these conditions on each `RightSizePolicy`:

| Condition | Reasons | Description |
|-----------|---------|-------------|
| `Ready` | `Monitoring`, `InsufficientData`, `PrometheusUnavailable`, `InvalidConfig` | Overall health |
| `Resizing` | `InProgress`, `Idle`, `CooldownActive` | Active resize operation state (only in resize modes) |
| `Degraded` | `HighRevertRate` | Set when 3+ of the last 5 resizes were reverted |

### Exponential Backoff

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
