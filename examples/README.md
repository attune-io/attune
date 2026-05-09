# Examples

Working examples for kube-rightsize, from a single policy to full production
setups. Every file can be applied directly with `kubectl apply -f`.

## Quick-start files

| File | Scenario | Mode |
|------|----------|------|
| [01-getting-started.yaml](01-getting-started.yaml) | Minimal policy for a single Deployment | Recommend |
| [02-canary-rollout.yaml](02-canary-rollout.yaml) | Graduated 10% canary with auto-revert | Canary |
| [03-auto-mode.yaml](03-auto-mode.yaml) | Fully automated for trusted workloads | Auto |
| [04-hpa-coexistence.yaml](04-hpa-coexistence.yaml) | Right-sizing alongside a HorizontalPodAutoscaler | Recommend |
| [05-cluster-defaults.yaml](05-cluster-defaults.yaml) | RightSizeDefaults CRD with simplified policy | Recommend |
| [06-multi-workload-selector.yaml](06-multi-workload-selector.yaml) | Label selector targeting many Deployments | Canary |

Start with `01-getting-started.yaml` to see recommendations without touching
any pods. Promote to `02-canary-rollout.yaml` once you trust the numbers.

## Composite scenarios

These directories contain complete, multi-resource setups that mirror
real-world deployments:

| Directory | What it shows |
|-----------|---------------|
| [multi-namespace/](multi-namespace/) | Dev/staging/prod policies with Kustomize overlays, different aggressiveness per environment |
| [full-stack/](full-stack/) | Everything needed for one app: namespace, Deployment, ServiceMonitor, RightSizeDefaults, RightSizePolicy, and Grafana dashboard ConfigMap |

## Prerequisites

All examples assume:

- Kubernetes 1.35+ (in-place pod resize GA)
- Prometheus reachable inside the cluster
- kube-rightsize operator installed (`helm install kube-rightsize oci://ghcr.io/sebtardif/charts/kube-rightsize`)