# kube-rightsize

Safe, in-place Kubernetes pod resource right-sizing operator

## Prerequisites

- Kubernetes 1.32+ (1.32 requires the `InPlacePodVerticalScaling` feature gate; 1.33+ has it enabled by default)
- Prometheus (for usage metrics)
- Helm 3.16+ or 4.x
- [cert-manager](https://cert-manager.io/docs/installation/) (for webhook TLS; to skip, use `--set webhooks.enabled=false`)

## Installation

```bash
helm install kube-rightsize oci://ghcr.io/sebtardiflabs/charts/kube-rightsize \
  --namespace kube-rightsize-system --create-namespace
```

## Values

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| affinity | object | `{}` | Affinity rules |
| clusterSize | string | `""` | Cluster size preset: sets resources, rate limits, and replica count. Valid values: small, medium, large, xlarge, or "" (no preset). Any explicitly set value overrides the preset. See docs/guides/scaling.md for details. |
| collectorTTL | string | `"10m"` | Collector cache TTL for unused Prometheus connections (Go duration, e.g. "10m", "1h") |
| defaults | object | `{"enabled":false,"updateStrategy":{"autoRevert":true,"cooldown":"1h","maxConcurrentResizes":1,"resizeMethod":"InPlaceOnly","type":"Recommend"}}` | Cluster-wide defaults (creates a RightSizeDefaults CR) |
| defaults.enabled | bool | `false` | Create a RightSizeDefaults resource with the values below |
| defaults.updateStrategy | object | `{"autoRevert":true,"cooldown":"1h","maxConcurrentResizes":1,"resizeMethod":"InPlaceOnly","type":"Recommend"}` | Default update strategy applied to all policies that don't override it |
| defaults.updateStrategy.autoRevert | bool | `true` | Auto-revert unsafe resizes |
| defaults.updateStrategy.cooldown | string | `"1h"` | Cooldown between resize cycles (Go duration, minimum 1m) |
| defaults.updateStrategy.maxConcurrentResizes | int | `1` | Max concurrent pod resizes per cycle (1-50) |
| defaults.updateStrategy.resizeMethod | string | `"InPlaceOnly"` | Resize method: InPlaceOnly or InPlaceOrRecreate |
| defaults.updateStrategy.type | string | `"Recommend"` | Resize type: Observe, Recommend, OneShot, Canary, Auto |
| fullnameOverride | string | `""` | Override the full name |
| grafanaDashboard.additionalLabels | object | `{}` | Additional labels for the dashboard ConfigMap (e.g., for folder selection) |
| grafanaDashboard.enabled | bool | `false` | Create a ConfigMap with the Grafana dashboard (auto-discovered by Grafana sidecar) |
| image.pullPolicy | string | `"IfNotPresent"` | Image pull policy |
| image.repository | string | `"ghcr.io/sebtardiflabs/kube-rightsize"` | Container image repository |
| image.tag | string | `""` | Image tag (defaults to Chart appVersion) |
| imagePullSecrets | list | `[]` | Image pull secrets |
| initialSizing | object | `{"enabled":false}` | Initial sizing webhook configuration. When enabled, a mutating webhook sets pod resource requests at creation time based on existing RightSizePolicy recommendations. Requires namespace label rightsize.io/initial-sizing=enabled and initialSizing: true on the policy. |
| initialSizing.enabled | bool | `false` | Enable the pod initial sizing mutating webhook. |
| leaderElection | object | `{"enabled":true}` | Leader election (enable for HA with replicaCount > 1) |
| logging | object | `{"format":"json","level":"info"}` | Logging configuration |
| logging.format | string | `"json"` | Log format (json, text) |
| logging.level | string | `"info"` | Log level (debug, info, warn, error) |
| maxConcurrentReconciles | string | `""` | Maximum number of RightSizePolicy reconciles running in parallel. Increase for large clusters with many policies (e.g. 4 for 200+ policies). |
| metrics | object | `{"enabled":true,"port":8080,"prometheusRule":{"additionalLabels":{},"enabled":false,"rules":{"degraded":{"enabled":true,"for":"5m","severity":"critical"},"highRevertRate":{"enabled":true,"for":"15m","severity":"critical","threshold":"0.5"},"prometheusUnreachable":{"enabled":true,"for":"10m","severity":"warning"},"reconcileErrors":{"enabled":true,"for":"10m","severity":"warning","threshold":"0"},"reconcileStale":{"enabled":true,"for":"5m","severity":"warning","staleDuration":"30m"}}},"serviceMonitor":{"additionalLabels":{},"enabled":false,"interval":"30s"}}` | Metrics endpoint |
| metrics.prometheusRule.additionalLabels | object | `{}` | Additional labels for the PrometheusRule |
| metrics.prometheusRule.enabled | bool | `false` | Create a PrometheusRule for out-of-the-box alerting. Requires the Prometheus Operator CRDs (monitoring.coreos.com/v1). |
| metrics.prometheusRule.rules | object | `{"degraded":{"enabled":true,"for":"5m","severity":"critical"},"highRevertRate":{"enabled":true,"for":"15m","severity":"critical","threshold":"0.5"},"prometheusUnreachable":{"enabled":true,"for":"10m","severity":"warning"},"reconcileErrors":{"enabled":true,"for":"10m","severity":"warning","threshold":"0"},"reconcileStale":{"enabled":true,"for":"5m","severity":"warning","staleDuration":"30m"}}` | Override default alert rules. Each key matches a rule name; set enabled: false to disable individual rules. |
| metrics.prometheusRule.rules.highRevertRate.for | string | `"15m"` | How long the condition must persist before firing |
| metrics.prometheusRule.rules.highRevertRate.threshold | string | `"0.5"` | Revert rate threshold (fraction, e.g. 0.5 = 50%) |
| metrics.prometheusRule.rules.reconcileErrors.for | string | `"10m"` | How long the condition must persist before firing |
| metrics.prometheusRule.rules.reconcileErrors.threshold | string | `"0"` | Error rate threshold (per second, averaged over 5m) |
| metrics.prometheusRule.rules.reconcileStale.staleDuration | string | `"30m"` | Fire when no reconcile completes within this duration |
| metrics.serviceMonitor.additionalLabels | object | `{}` | Additional labels for the ServiceMonitor |
| metrics.serviceMonitor.enabled | bool | `false` | Create a ServiceMonitor for Prometheus Operator |
| metrics.serviceMonitor.interval | string | `"30s"` | Scrape interval |
| nameOverride | string | `""` | Override the chart name |
| networkPolicy | object | `{"enabled":true,"prometheusPort":9090}` | NetworkPolicy configuration for operator ingress and egress ports |
| networkPolicy.enabled | bool | `true` | Enable NetworkPolicy for the operator pod |
| networkPolicy.prometheusPort | int | `9090` | TCP port allowed by NetworkPolicy for Prometheus backend pods |
| nodeSelector | object | `{}` | Node selector |
| podAnnotations | object | `{}` | Pod annotations |
| podSecurityContext | object | `{"runAsNonRoot":true,"seccompProfile":{"type":"RuntimeDefault"}}` | Pod security context |
| priorityClassName | string | `""` | Priority class name for the operator pod (recommended: system-cluster-critical for production) |
| prometheusBurst | int | `20` | Prometheus query burst allowance. |
| prometheusQPS | int | `10` | Prometheus query rate limit (queries per second). Higher values reduce reconcile latency but increase Prometheus load. |
| prometheusTimeout | string | `"5m"` | Maximum time for workload processing (including Prometheus queries) per reconciliation cycle (Go duration). If exceeded, partial results are used and the status condition indicates the timeout. |
| replicaCount | int | `1` | Number of operator replicas (use 2 for HA with leader election) |
| resources | object | `{}` | Operator pod resources. When empty, defaults are derived from clusterSize (or "small" if clusterSize is also empty). Set explicit values for production. |
| securityContext | object | `{"allowPrivilegeEscalation":false,"capabilities":{"drop":["ALL"]},"readOnlyRootFilesystem":true,"runAsGroup":65532,"runAsNonRoot":true,"runAsUser":65532}` | Container security context |
| serviceAccount.annotations | object | `{}` | Annotations to add to the ServiceAccount |
| serviceAccount.create | bool | `true` | Create a ServiceAccount |
| serviceAccount.name | string | `""` | ServiceAccount name (generated if not set) |
| tolerations | list | `[]` | Tolerations |
| topologySpreadConstraints | list | `[]` | Topology spread constraints |
| watchNamespaces | list | `[]` | Namespaces to watch for RightSizePolicy resources. Empty means all namespaces (cluster-scoped). Set this to reduce informer cache memory on large clusters where policies exist in only a few namespaces. Cluster-scoped resources (Nodes, RightSizeDefaults) are always watched regardless. |
| webhooks | object | `{"enabled":true}` | Webhook configuration (requires cert-manager installed in the cluster) |
| webhooks.enabled | bool | `true` | Enable admission webhooks for defaulting and validation. Requires cert-manager to be installed for TLS certificate provisioning. |

## CRDs

This chart installs the required CRDs on `helm install`:
- `rightsizepolicies.rightsize.io`
- `rightsizedefaults.rightsize.io`
- `rightsizenamespacedefaults.rightsize.io`

> **Note:** Helm does not update CRDs on `helm upgrade`.
> Before upgrading, apply the latest CRDs manually:
> ```bash
> kubectl apply --server-side --force-conflicts -f \
>   https://github.com/SebTardifLabs/kube-rightsize/releases/latest/download/crds.yaml
> ```

## Prometheus Configuration

Prometheus address is configured per-policy in `RightSizePolicy.spec.metricsSource.prometheus.address`,
via namespace-scoped `RightSizeNamespaceDefaults`, or globally via the
`RightSizeDefaults` CRD. It is not a chart value.

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
helm upgrade kube-rightsize oci://ghcr.io/sebtardiflabs/charts/kube-rightsize \
  --namespace kube-rightsize-system \
  --reuse-values \
  --set grafanaDashboard.enabled=true
```

**Manual import:** A pre-built dashboard is also included in
[`deploy/grafana/dashboard.json`](https://github.com/SebTardifLabs/kube-rightsize/blob/main/deploy/grafana/dashboard.json).
Import it into Grafana to visualize recommendations, resize operations, and savings.

## Uninstall

```bash
helm uninstall kube-rightsize -n kube-rightsize-system
```

CRDs are not removed by `helm uninstall`. To remove them:

```bash
kubectl delete crd rightsizepolicies.rightsize.io rightsizedefaults.rightsize.io rightsizenamespacedefaults.rightsize.io
```
