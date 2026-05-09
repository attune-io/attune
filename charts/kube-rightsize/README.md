# kube-rightsize Helm Chart

Safe, in-place Kubernetes pod resource right-sizing operator.

## Prerequisites

- Kubernetes 1.33+ (In-Place Pod Resize GA)
- Prometheus (for usage metrics)
- Helm 3.16+

## Installation

```bash
helm install kube-rightsize oci://ghcr.io/sebtardif/charts/kube-rightsize \
  --namespace kube-rightsize-system --create-namespace
```

## Configuration

| Parameter | Description | Default |
|-----------|-------------|---------|
| `replicaCount` | Number of operator replicas | `1` |
| `image.repository` | Container image repository | `ghcr.io/sebtardif/kube-rightsize` |
| `image.tag` | Image tag (defaults to chart appVersion) | `""` |
| `image.pullPolicy` | Image pull policy | `IfNotPresent` |
| `resources.limits.cpu` | CPU limit | `500m` |
| `resources.limits.memory` | Memory limit | `128Mi` |
| `resources.requests.cpu` | CPU request | `100m` |
| `resources.requests.memory` | Memory request | `64Mi` |
| `prometheus.address` | Prometheus server URL | `http://prometheus-server.monitoring:9090` |
| `leaderElection.enabled` | Enable leader election for HA | `true` |
| `metrics.enabled` | Enable metrics endpoint | `true` |
| `metrics.port` | Metrics port | `8080` |
| `metrics.serviceMonitor.enabled` | Create Prometheus ServiceMonitor | `false` |
| `metrics.serviceMonitor.interval` | Scrape interval | `30s` |
| `logging.level` | Log level (debug, info, warn, error) | `info` |
| `logging.format` | Log format (json, text) | `json` |

## CRDs

This chart automatically installs the required CRDs:
- `rightsizepolicies.rightsize.io`
- `rightsizedefaults.rightsize.io`

## Grafana Dashboard

A pre-built Grafana dashboard is included in `dashboards/grafana-dashboard.json`.
Import it into Grafana to visualize recommendations, resize operations, and savings.

## Uninstall

```bash
helm uninstall kube-rightsize -n kube-rightsize-system
```

CRDs are not removed by `helm uninstall`. To remove them:

```bash
kubectl delete crd rightsizepolicies.rightsize.io rightsizedefaults.rightsize.io
```
