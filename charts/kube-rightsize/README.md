# kube-rightsize

Safe, in-place Kubernetes pod resource right-sizing operator

## Prerequisites

- Kubernetes 1.33+ (In-Place Pod Resize GA)
- Prometheus (for usage metrics)
- Helm 3.16+
- [cert-manager](https://cert-manager.io/docs/installation/) (for webhook TLS; to skip, use `--set webhooks.enabled=false`)

## Installation

```bash
helm install kube-rightsize oci://ghcr.io/sebtardif/charts/kube-rightsize \
  --namespace kube-rightsize-system --create-namespace
```

## Values

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| affinity | object | `{}` | Affinity rules |
| fullnameOverride | string | `""` | Override the full name |
| grafanaDashboard.additionalLabels | object | `{}` | Additional labels for the dashboard ConfigMap (e.g., for folder selection) |
| grafanaDashboard.enabled | bool | `false` | Create a ConfigMap with the Grafana dashboard (auto-discovered by Grafana sidecar) |
| image.pullPolicy | string | `"IfNotPresent"` | Image pull policy |
| image.repository | string | `"ghcr.io/sebtardif/kube-rightsize"` | Container image repository |
| image.tag | string | `""` | Image tag (defaults to Chart appVersion) |
| imagePullSecrets | list | `[]` | Image pull secrets |
| leaderElection | object | `{"enabled":true}` | Leader election (enable for HA with replicaCount > 1) |
| logging | object | `{"format":"json","level":"info"}` | Logging configuration |
| logging.format | string | `"json"` | Log format (json, text) |
| logging.level | string | `"info"` | Log level (debug, info, warn, error) |
| metrics | object | `{"enabled":true,"port":8080,"serviceMonitor":{"additionalLabels":{},"enabled":false,"interval":"30s"}}` | Metrics endpoint |
| metrics.serviceMonitor.additionalLabels | object | `{}` | Additional labels for the ServiceMonitor |
| metrics.serviceMonitor.enabled | bool | `false` | Create a ServiceMonitor for Prometheus Operator |
| metrics.serviceMonitor.interval | string | `"30s"` | Scrape interval |
| nameOverride | string | `""` | Override the chart name |
| networkPolicy | object | `{"enabled":false}` | Network policy restricts pod traffic to only required endpoints |
| networkPolicy.enabled | bool | `false` | Enable NetworkPolicy for the operator pod |
| nodeSelector | object | `{}` | Node selector |
| podAnnotations | object | `{}` | Pod annotations |
| podSecurityContext | object | `{"runAsNonRoot":true,"seccompProfile":{"type":"RuntimeDefault"}}` | Pod security context |
| priorityClassName | string | `""` | Priority class name for the operator pod (recommended: system-cluster-critical for production) |
| replicaCount | int | `1` | Number of operator replicas (use 2 for HA with leader election) |
| resources | object | `{"limits":{"cpu":"500m","memory":"256Mi"},"requests":{"cpu":"100m","memory":"128Mi"}}` | Operator pod resources |
| securityContext | object | `{"allowPrivilegeEscalation":false,"capabilities":{"drop":["ALL"]},"readOnlyRootFilesystem":true,"runAsGroup":65532,"runAsNonRoot":true,"runAsUser":65532}` | Container security context |
| serviceAccount.annotations | object | `{}` | Annotations to add to the ServiceAccount |
| serviceAccount.create | bool | `true` | Create a ServiceAccount |
| serviceAccount.name | string | `""` | ServiceAccount name (generated if not set) |
| tolerations | list | `[]` | Tolerations |
| topologySpreadConstraints | list | `[]` | Topology spread constraints |
| webhooks | object | `{"enabled":true}` | Webhook configuration (requires cert-manager installed in the cluster) |
| webhooks.enabled | bool | `true` | Requires cert-manager to be installed for TLS certificate provisioning. |

## CRDs

This chart automatically installs the required CRDs:
- `rightsizepolicies.rightsize.io`
- `rightsizedefaults.rightsize.io`

## Prometheus Configuration

Prometheus address is configured per-policy in `RightSizePolicy.spec.metricsSource.prometheus.address`
or globally via the `RightSizeDefaults` CRD. It is not a chart value.

## Grafana Dashboard

A pre-built Grafana dashboard is included in [`deploy/grafana/dashboard.json`](https://github.com/SebTardifLabs/kube-rightsize/blob/main/deploy/grafana/dashboard.json).
Import it into Grafana to visualize recommendations, resize operations, and savings.

## Uninstall

```bash
helm uninstall kube-rightsize -n kube-rightsize-system
```

CRDs are not removed by `helm uninstall`. To remove them:

```bash
kubectl delete crd rightsizepolicies.rightsize.io rightsizedefaults.rightsize.io
```