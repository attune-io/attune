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
| `03-service-monitor.yaml` | ServiceMonitor | Prometheus scraping for the operator's metrics |
| `04-defaults.yaml` | AttuneDefaults | Org-wide defaults (Prometheus address, bounds) |
| `05-policy.yaml` | AttunePolicy | Per-workload policy in Canary mode |
| `06-grafana-dashboard.yaml` | ConfigMap | Auto-provisioned Grafana dashboard via sidecar |

## Prerequisites

- Kubernetes 1.32+
- attune operator installed
- Prometheus Operator (for ServiceMonitor)
- Grafana with sidecar dashboard provisioning (label: `grafana_dashboard: "1"`)