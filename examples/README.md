# Examples

Working examples for kube-rightsize, from a single policy to full production
setups. Standalone `.yaml` files can be applied directly from the repo root
with `kubectl apply -f examples/<file>.yaml`, while directory-based scenarios
include their own instructions and may use `kubectl apply -k`.

## Quick-start files

| File | Scenario | Mode |
|------|----------|------|
| [01-getting-started.yaml](01-getting-started.yaml) | Minimal policy for a single Deployment | Recommend |
| [02-canary-rollout.yaml](02-canary-rollout.yaml) | Graduated 10% canary with auto-revert | Canary |
| [03-auto-mode.yaml](03-auto-mode.yaml) | Fully automated for trusted workloads | Auto |
| [04-hpa-coexistence.yaml](04-hpa-coexistence.yaml) | Right-sizing alongside a HorizontalPodAutoscaler | Recommend |
| [05-cluster-defaults.yaml](05-cluster-defaults.yaml) | RightSizeDefaults CRD with simplified policy | Recommend |
| [06-multi-workload-selector.yaml](06-multi-workload-selector.yaml) | Label selector targeting many Deployments | Recommend |
| [07-sidecar-exclusion.yaml](07-sidecar-exclusion.yaml) | Skip service mesh sidecars with `excludeContainers` | Recommend |

Start with `01-getting-started.yaml` to see recommendations without touching
any pods. Promote to `02-canary-rollout.yaml` once you trust the numbers. If
you want shared Prometheus settings, continue with `05-cluster-defaults.yaml`
for cluster-wide defaults or `11-namespace-defaults.yaml` for a namespace-only
setup.

## Advanced scenario files

| File | Scenario | Mode |
|------|----------|------|
| [08-observe-mode.yaml](08-observe-mode.yaml) | Validate Prometheus connectivity before generating recommendations | Observe |
| [09-oneshot-mode.yaml](09-oneshot-mode.yaml) | Resize one pod per reconcile cycle for cautious rollout | OneShot |
| [10-cronjob-policy.yaml](10-cronjob-policy.yaml) | Recommend-only sizing for CronJobs and Jobs | Recommend |
| [11-namespace-defaults.yaml](11-namespace-defaults.yaml) | Namespace-scoped defaults layered over cluster defaults | Mixed |
| [12-scheduled-auto-mode.yaml](12-scheduled-auto-mode.yaml) | Auto mode limited to maintenance windows with per-cycle budgets | Auto |
| [13-multi-datasource.yaml](13-multi-datasource.yaml) | Mimir, Thanos, bearer-token auth, and TLS examples | Recommend |
| [14-startup-boost.yaml](14-startup-boost.yaml) | Temporary CPU boost for cold-start workloads (JVMs, ML models) | Auto |

## Composite scenarios

These directories contain complete, multi-resource setups that mirror
real-world deployments:

| Directory | What it shows |
|-----------|---------------|
| [multi-namespace/](multi-namespace/) | Dev/staging/prod policies with Kustomize overlays, different aggressiveness per environment |
| [full-stack/](full-stack/) | Everything needed for one app: namespace, Deployment, ServiceMonitor, RightSizeDefaults, RightSizePolicy, and Grafana dashboard ConfigMap |

## Prerequisites

All examples assume:

- Kubernetes 1.32+ (1.32 requires the `InPlacePodVerticalScaling` feature gate; 1.33+ enabled by default)
- Prometheus reachable inside the cluster
- kube-rightsize operator installed (for example: `helm install kube-rightsize oci://ghcr.io/sebtardiflabs/charts/kube-rightsize --namespace kube-rightsize-system --create-namespace`)