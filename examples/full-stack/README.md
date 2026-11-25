# Full-Stack Example

Everything needed to right-size a single application, from namespace creation
to Grafana dashboard. Apply the whole directory at once:

```bash
kubectl apply -f examples/full-stack/
```

## What's included

| File | Resource | Purpose |
|------|----------|---------|
| `01-namespace.yaml` | Namespace | Isolated namespace for the workload |
| `02-deployment.yaml` | Deployment | Sample app with initial resource requests |
| `03-service-monitor.yaml` | ServiceMonitor | HTTPS scrape of the operator metrics. The Prometheus ServiceAccount still needs `get` on `/metrics`. |
| `04-defaults.yaml` | AttuneDefaults | Org-wide defaults (Prometheus address, no token, bounds) |
| `05-policy.yaml` | AttunePolicy | Per-workload policy in Canary mode |
| `06-grafana-dashboard.yaml` | ConfigMap | Auto-provisioned Grafana dashboard via sidecar |

## Prerequisites

- Kubernetes 1.32+
- Attune operator installed
- Prometheus Operator (for ServiceMonitor)
- A ClusterRoleBinding that lets the Prometheus ServiceAccount `get` `/metrics`. The sample ServiceMonitor uses HTTPS and skips TLS verification. The chart creates that binding when `metrics.serviceMonitor.serviceAccountName` and `serviceAccountNamespace` are set. This directory does not include the binding.
- Grafana with sidecar dashboard provisioning (label: `grafana_dashboard: "1"`)