# attune

Safe, in-place Kubernetes pod resource right-sizing operator

## Prerequisites

- Kubernetes 1.32+ (1.32 requires the `InPlacePodVerticalScaling` feature gate; 1.33–1.34 beta enabled by default; 1.35+ GA)
- Prometheus (for usage metrics)
- Helm 3.16+ or 4.x
- [cert-manager](https://cert-manager.io/docs/installation/) (for webhook TLS; to skip, use `--set webhooks.enabled=false`)

## Installation

```bash
helm install attune oci://ghcr.io/attune-io/charts/attune \
  --namespace attune-system --create-namespace
```

## Values

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| affinity | object | `{}` | Affinity rules |
| blockerRefreshInterval | string | `"0s"` | Minimum interval between Deferred/Infeasible blocker recomputes when not resizing. Zero (default) recomputes every cycle. Set "5m" on large Recommend fleets to cut List work. |
| clusterSize | string | `""` | Cluster size preset: sets resources, rate limits, and replica count. Valid values: small, medium, large, xlarge, or "" (no preset). Any explicitly set value overrides the preset. See docs/guides/scaling.md for details. |
| collectorTTL | string | `"10m"` | Collector cache TTL for unused Prometheus connections (Go duration, e.g. "10m", "1h") |
| defaults | object | `{"enabled":false,"updateStrategy":{"autoRevert":true,"cooldown":"1h","maxConcurrentResizes":1,"resizeMethod":"InPlaceOnly","type":"Recommend"}}` | Cluster-wide defaults (creates an AttuneDefaults CR) |
| defaults.enabled | bool | `false` | Create an AttuneDefaults resource with the values below |
| defaults.updateStrategy | object | `{"autoRevert":true,"cooldown":"1h","maxConcurrentResizes":1,"resizeMethod":"InPlaceOnly","type":"Recommend"}` | Default update strategy applied to all policies that don't override it |
| defaults.updateStrategy.autoRevert | bool | `true` | Auto-revert unsafe resizes |
| defaults.updateStrategy.cooldown | string | `"1h"` | Cooldown between resize cycles (Go duration, minimum 1m) |
| defaults.updateStrategy.maxConcurrentResizes | int | `1` | Max concurrent pod resizes per cycle (1-50) |
| defaults.updateStrategy.resizeMethod | string | `"InPlaceOnly"` | Resize method: InPlaceOnly or InPlaceOrRecreate |
| defaults.updateStrategy.type | string | `"Recommend"` | Resize type: Observe, Recommend, OneShot, Canary, Auto |
| fips | object | `{"enabled":false,"mode":"on"}` | FIPS 140-3 compliance mode. When enabled, sets GODEBUG=fips140=<mode> to activate Go's CMVP-validated cryptographic module (Certificate #5247). The binary always embeds the module; this toggle controls whether it is active at runtime. |
| fips.enabled | bool | `false` | Enable FIPS 140-3 mode |
| fips.mode | string | `"on"` | FIPS enforcement level: "on" (approved algorithms preferred, fallbacks allowed) or "only" (non-approved algorithms panic). Use "on" for Kubernetes operators because client-go uses X25519 which is not FIPS-approved. |
| fleetReport | object | `{"clusterId":"","configMapName":"attune-fleet-report","enabled":false,"interval":"5m","namespace":""}` | Optional per-cluster fleet summary ConfigMap for multi-cluster collectors (#369). Writes a versioned JSON report (schemaVersion v1) for scripts like scripts/collect-fleet-reports.sh. |
| fleetReport.clusterId | string | `""` | Stable cluster id written into the report (e.g. prod-us-east-1) |
| fleetReport.configMapName | string | `"attune-fleet-report"` | ConfigMap name |
| fleetReport.enabled | bool | `false` | Enable periodic fleet report ConfigMap export (default off) |
| fleetReport.interval | string | `"5m"` | How often to refresh the report |
| fleetReport.namespace | string | `""` | ConfigMap namespace (empty = release namespace) |
| fullnameOverride | string | `""` | Override the full name |
| grafanaDashboard.additionalLabels | object | `{}` | Additional labels for the dashboard ConfigMap (e.g., for folder selection) |
| grafanaDashboard.enabled | bool | `false` | Create a ConfigMap with the Grafana dashboard (auto-discovered by Grafana sidecar) |
| grafanaFleetDashboard.additionalLabels | object | `{}` | Additional labels for the fleet dashboard ConfigMap |
| grafanaFleetDashboard.enabled | bool | `false` | Create a ConfigMap with the multi-cluster fleet Grafana dashboard (cluster variable). Use with federated Prometheus that has external_labels.cluster on each scrape. |
| image.pullPolicy | string | `"IfNotPresent"` | Image pull policy |
| image.repository | string | `"ghcr.io/attune-io/attune"` | Container image repository |
| image.tag | string | `""` | Image tag (defaults to Chart appVersion) |
| imagePullSecrets | list | `[]` | Image pull secrets |
| initialSizing | object | `{"enabled":false}` | Initial sizing webhook configuration. When enabled, a mutating webhook sets pod resource requests at creation time based on existing AttunePolicy recommendations. Requires namespace label attune.io/initial-sizing=enabled and initialSizing: true on the policy. |
| initialSizing.enabled | bool | `false` | Enable the pod initial sizing mutating webhook. |
| leaderElection | object | `{"enabled":true}` | Leader election (enable for HA with replicaCount > 1) |
| logging | object | `{"format":"json","level":"info"}` | Logging configuration |
| logging.format | string | `"json"` | Log format (json, text) |
| logging.level | string | `"info"` | Log level (debug, info, warn, error) |
| maxConcurrentReconciles | string | `""` | Maximum number of AttunePolicy reconciles running in parallel. Empty uses binary default (2) or clusterSize presets (small=1, medium=2, large=4, xlarge=8). |
| maxHistoryWindow | string | `""` | Operator ceiling for metrics historyWindow (Go duration). Empty = no extra clamp. clusterSize large/xlarge auto-set 72h/48h when this is empty. |
| maxPodsInMetricsQuery | int | `100` | Cap pods named in metrics pod=~ regexes for huge Deployments (representative sample). Negative disables sampling. Default 100. |
| maxProfileSamples | int | `10000` | Cap samples passed into recommendation BuildProfile after downsampling. Negative disables. Default 10000. |
| maxPrometheusSeries | int | `5000` | Cap series kept from each Prometheus range query matrix. Zero uses the binary default (5000); negative disables. Default 5000. |
| maxStatusRecommendations | int | `100` | Default cap for status.recommendations (full set still used for resizes). |
| maxWorkloadWorkers | int | `10` | Maximum parallel workers processing workloads within one AttunePolicy reconcile. Default 10. Raise for policies targeting many Deployments when prometheusQPS allows. |
| metrics | object | `{"enabled":true,"port":8080,"prometheusRule":{"additionalLabels":{},"enabled":false,"fleetRecordingRules":{"enabled":false},"rules":{"budgetExhausted":{"enabled":true,"for":"30m","severity":"warning"},"dataQuality":{"enabled":true,"for":"30m","severity":"warning"},"degraded":{"enabled":true,"for":"5m","severity":"critical"},"gitopsPRFailures":{"enabled":true,"for":"5m","severity":"warning"},"highRevertRate":{"enabled":true,"for":"15m","severity":"critical","threshold":"0.5"},"memoryLimitUnsafe":{"enabled":true,"for":"1h","severity":"info"},"podsDeferred":{"enabled":true,"for":"1h","severity":"warning"},"podsInfeasible":{"enabled":true,"for":"30m","severity":"warning"},"prometheusUnreachable":{"enabled":true,"for":"10m","severity":"warning"},"reconcileErrors":{"enabled":true,"for":"10m","severity":"warning","threshold":"0"},"reconcileStale":{"enabled":true,"for":"5m","severity":"warning","staleDuration":"30m"},"requestsClamped":{"enabled":true,"for":"1h","severity":"info"},"revertFailures":{"enabled":true,"for":"5m","severity":"critical"},"staleRecommendations":{"enabled":true,"for":"1h","severity":"warning"}}},"serviceMonitor":{"additionalLabels":{},"enabled":false,"interval":"30s"}}` | Metrics endpoint |
| metrics.prometheusRule.additionalLabels | object | `{}` | Additional labels for the PrometheusRule |
| metrics.prometheusRule.enabled | bool | `false` | Create a PrometheusRule for out-of-the-box alerting. Requires the Prometheus Operator CRDs (monitoring.coreos.com/v1). |
| metrics.prometheusRule.fleetRecordingRules | object | `{"enabled":false}` | Recording rules for multi-cluster / federated Prometheus rollups (#369). Safe to enable on single-cluster installs (cluster label may be empty). |
| metrics.prometheusRule.fleetRecordingRules.enabled | bool | `false` | Emit attune:* recording rules for org-wide Grafana and PromQL |
| metrics.prometheusRule.rules | object | `{"budgetExhausted":{"enabled":true,"for":"30m","severity":"warning"},"dataQuality":{"enabled":true,"for":"30m","severity":"warning"},"degraded":{"enabled":true,"for":"5m","severity":"critical"},"gitopsPRFailures":{"enabled":true,"for":"5m","severity":"warning"},"highRevertRate":{"enabled":true,"for":"15m","severity":"critical","threshold":"0.5"},"memoryLimitUnsafe":{"enabled":true,"for":"1h","severity":"info"},"podsDeferred":{"enabled":true,"for":"1h","severity":"warning"},"podsInfeasible":{"enabled":true,"for":"30m","severity":"warning"},"prometheusUnreachable":{"enabled":true,"for":"10m","severity":"warning"},"reconcileErrors":{"enabled":true,"for":"10m","severity":"warning","threshold":"0"},"reconcileStale":{"enabled":true,"for":"5m","severity":"warning","staleDuration":"30m"},"requestsClamped":{"enabled":true,"for":"1h","severity":"info"},"revertFailures":{"enabled":true,"for":"5m","severity":"critical"},"staleRecommendations":{"enabled":true,"for":"1h","severity":"warning"}}` | Override default alert rules. Each key matches a rule name; set enabled: false to disable individual rules. |
| metrics.prometheusRule.rules.budgetExhausted.for | string | `"30m"` | How long the condition must persist before firing |
| metrics.prometheusRule.rules.dataQuality.for | string | `"30m"` | How long the condition must persist before firing |
| metrics.prometheusRule.rules.gitopsPRFailures.enabled | bool | `true` | Fire when opt-in GitOps PR automation reports result=failed |
| metrics.prometheusRule.rules.highRevertRate.for | string | `"15m"` | How long the condition must persist before firing |
| metrics.prometheusRule.rules.highRevertRate.threshold | string | `"0.5"` | Revert rate threshold (fraction, e.g. 0.5 = 50%) |
| metrics.prometheusRule.rules.memoryLimitUnsafe.enabled | bool | `true` | Fire when memory limit decreases are floored or skipped as unsafe |
| metrics.prometheusRule.rules.podsDeferred.enabled | bool | `true` | Fire when pods remain Deferred (node capacity) for in-place resize |
| metrics.prometheusRule.rules.podsInfeasible.enabled | bool | `true` | Fire when pods remain Infeasible for in-place resize |
| metrics.prometheusRule.rules.reconcileErrors.for | string | `"10m"` | How long the condition must persist before firing |
| metrics.prometheusRule.rules.reconcileErrors.threshold | string | `"0"` | Error rate threshold (per second, averaged over 5m) |
| metrics.prometheusRule.rules.reconcileStale.staleDuration | string | `"30m"` | Fire when no reconcile completes within this duration |
| metrics.prometheusRule.rules.requestsClamped.for | string | `"1h"` | How long the condition must persist before firing |
| metrics.prometheusRule.rules.revertFailures.for | string | `"5m"` | How long the condition must persist before firing |
| metrics.prometheusRule.rules.staleRecommendations.for | string | `"1h"` | How long the condition must persist before firing |
| metrics.serviceMonitor.additionalLabels | object | `{}` | Additional labels for the ServiceMonitor |
| metrics.serviceMonitor.enabled | bool | `false` | Create a ServiceMonitor for Prometheus Operator |
| metrics.serviceMonitor.interval | string | `"30s"` | Scrape interval |
| minQueryStep | string | `""` | Operator floor for metrics queryStep (Go duration). Empty = no extra clamp. clusterSize large/xlarge auto-set 10m/15m when this is empty. |
| nameOverride | string | `""` | Override the chart name |
| networkPolicy | object | `{"enabled":true,"prometheusPort":9090}` | NetworkPolicy configuration for operator ingress and egress ports |
| networkPolicy.enabled | bool | `true` | Enable NetworkPolicy for the operator pod |
| networkPolicy.prometheusPort | int | `9090` | TCP port allowed by NetworkPolicy for Prometheus backend pods |
| nodeSelector | object | `{}` | Node selector |
| openshift | object | `{"enabled":false}` | OpenShift integration |
| openshift.enabled | bool | `false` | Enable OpenShift-specific features (TLS profile auto-detection). When enabled, the ClusterRole includes read access to config.openshift.io/apiservers for TLS security profile detection. |
| podAnnotations | object | `{}` | Pod annotations |
| podSecurityContext | object | `{"runAsNonRoot":true,"seccompProfile":{"type":"RuntimeDefault"}}` | Pod security context |
| priorityClassName | string | `""` | Priority class name for the operator pod (recommended: system-cluster-critical for production) |
| prometheusBurst | int | `20` | Prometheus query burst allowance. |
| prometheusQPS | int | `10` | Prometheus query rate limit (queries per second). Higher values reduce reconcile latency but increase Prometheus load. |
| prometheusTimeout | string | `"5m"` | Maximum time for workload processing (including Prometheus queries) per reconciliation cycle (Go duration). If exceeded, partial results are used and the status condition indicates the timeout. |
| replicaCount | int | `1` | Number of operator replicas (use 2 for HA with leader election) |
| requeueJitter | string | `"2m"` | Maximum deterministic requeue jitter to spread policies that share a cooldown. Set to "0s" to disable. Default 2m (empty omits the flag and keeps the binary default). |
| resources | object | `{}` | Operator pod resources. When empty, defaults are derived from clusterSize (or "small" if clusterSize is also empty). Set explicit values for production. |
| securityContext | object | `{"allowPrivilegeEscalation":false,"capabilities":{"drop":["ALL"]},"readOnlyRootFilesystem":true,"runAsGroup":65532,"runAsNonRoot":true,"runAsUser":65532}` | Container security context |
| serviceAccount.annotations | object | `{}` | Annotations to add to the ServiceAccount |
| serviceAccount.create | bool | `true` | Create a ServiceAccount |
| serviceAccount.name | string | `""` | ServiceAccount name (generated if not set) |
| statusIncludeExplanations | bool | `true` | Write recommendation explanation chains into status (can bloat large policies). |
| tolerations | list | `[]` | Tolerations |
| topologySpreadConstraints | list | `[]` | Topology spread constraints |
| watchNamespaces | list | `[]` | Namespaces to watch for AttunePolicy resources. Empty means all namespaces (cluster-scoped). Set this to reduce informer cache memory on large clusters where policies exist in only a few namespaces. Cluster-scoped resources (Nodes, AttuneDefaults) are always watched regardless. |
| webhooks | object | `{"enabled":true}` | Webhook configuration (requires cert-manager installed in the cluster) |
| webhooks.enabled | bool | `true` | Enable admission webhooks for defaulting and validation. Requires cert-manager to be installed for TLS certificate provisioning. |

## CRDs

This chart installs the required CRDs on `helm install`:
- `attunepolicies.attune.io`
- `attunedefaults.attune.io`
- `attunenamespacedefaults.attune.io`

> **Note:** Helm does not update CRDs on `helm upgrade`.
> Before upgrading, apply the latest CRDs manually:
> ```bash
> kubectl apply --server-side --force-conflicts -f \
>   https://github.com/attune-io/attune/releases/latest/download/crds.yaml
> ```

## Prometheus Configuration

Prometheus address is configured per-policy in `AttunePolicy.spec.metricsSource.prometheus.address`,
via namespace-scoped `AttuneNamespaceDefaults`, or globally via the
`AttuneDefaults` CRD. It is not a chart value.

If `networkPolicy.enabled=true`, the operator pod allows egress to Prometheus on
`networkPolicy.prometheusPort` (default `9090`). For the `prometheus-community/prometheus`
chart, keep this at `9090` even if the Service URL uses port `80`, because
NetworkPolicy egress matches the backend pod port.

## Security Defaults

The chart defaults to a restricted runtime profile:

- Pod security context sets `runAsNonRoot: true` and pod-level `seccompProfile.type: RuntimeDefault`
- Container security context drops all Linux capabilities, disables privilege escalation,
  uses a read-only root filesystem, and runs as UID/GID `65532`

If your cluster enforces Pod Security Admission or custom policies, verify these
settings match your environment before overriding them.

## NetworkPolicy

When `networkPolicy.enabled=true`, the chart creates a policy for the operator pod that:

- allows webhook ingress on `9443` when `webhooks.enabled=true`
- allows metrics ingress on `metrics.port` when `metrics.enabled=true`
- allows egress to the Kubernetes API on `443`
- allows DNS egress on UDP/TCP `53`
- allows egress to Prometheus on `networkPolicy.prometheusPort`

Clusters with default-deny policies may need matching Prometheus namespace or source
policies so scraping can still reach the operator metrics Service.

## Grafana Dashboard

**Auto-provision (recommended):** set `grafanaDashboard.enabled=true` to create a
ConfigMap labeled for the Grafana sidecar:

```bash
helm upgrade attune oci://ghcr.io/attune-io/charts/attune \
  --namespace attune-system \
  --reuse-values \
  --set grafanaDashboard.enabled=true
```

**Manual import:** A pre-built dashboard is also included in
[`deploy/grafana/dashboard.json`](https://github.com/attune-io/attune/blob/main/deploy/grafana/dashboard.json).
Import it into Grafana to visualize recommendations, resize operations, and savings.

## Uninstall

```bash
helm uninstall attune -n attune-system
```

CRDs are not removed by `helm uninstall`. To remove them:

```bash
kubectl delete crd attunepolicies.attune.io attunedefaults.attune.io attunenamespacedefaults.attune.io
```
