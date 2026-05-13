<p align="center">
  <img src="docs/logo.jpg" alt="kube-rightsize logo" width="200">
</p>

# kube-rightsize

[![CI](https://github.com/SebTardifLabs/kube-rightsize/actions/workflows/ci.yaml/badge.svg)](https://github.com/SebTardifLabs/kube-rightsize/actions/workflows/ci.yaml)
[![Security](https://github.com/SebTardifLabs/kube-rightsize/actions/workflows/security.yaml/badge.svg)](https://github.com/SebTardifLabs/kube-rightsize/actions/workflows/security.yaml)
[![Go Version](https://img.shields.io/github/go-mod/go-version/SebTardifLabs/kube-rightsize)](go.mod)
[![License](https://img.shields.io/github/license/SebTardifLabs/kube-rightsize)](LICENSE)

**Safe, in-place Kubernetes pod resource right-sizing. VPA done right.**

kube-rightsize is a Kubernetes operator that automatically right-sizes pod
resource requests and limits using [In-Place Pod Resize](https://kubernetes.io/blog/2025/12/19/kubernetes-v1-35-in-place-pod-resize-ga/)
(GA in Kubernetes 1.33+). No pod restarts. No evictions. No HPA conflicts.

---

## Why

| Problem | Impact |
|---------|--------|
| Average CPU utilization is **8%** | Billions wasted industry-wide (CAST AI 2026) |
| **70%** cite overprovisioning as #1 cost driver | Resources allocated "just in case" never reclaimed (CNCF 2023) |
| **<1%** run VPA fully automated | VPA evicts pods, conflicts with HPA, causes outages (ScaleOps 2026) |
| In-Place Pod Resize is **GA** (K8s 1.33+) | The foundation for non-disruptive right-sizing now exists |

## How It's Different

| | VPA | Goldilocks | kube-rightsize |
|---|---|---|---|
| Resize method | Evicts pods | No resize (recommend only) | **In-place** (no restarts) |
| HPA compatible | No (death spirals) | N/A | **Yes** (adjusts base, not %) |
| Safety | Minimal guardrails | N/A | **Graduated rollout + auto-revert** |
| Algorithm | Backward-looking histograms | VPA recommender | **Time-of-day-aware + burst detection** |
| Production confidence | <1% use automated | N/A | **Observe -> Recommend -> Canary -> Auto** |

> **Migrating from VPA?** See the step-by-step [migration guide](docs/guides/migrating-from-vpa.md) for field-by-field mapping, side-by-side YAML, and zero-downtime cutover.

## Quick Start

### Prerequisites

- Kubernetes 1.33+ (In-Place Pod Resize GA)
- Prometheus (for usage metrics)
- Helm 3.16+
- [cert-manager](https://cert-manager.io/docs/installation/) (for admission webhook TLS; to skip, install with `--set webhooks.enabled=false`)

### Install

```bash
helm install kube-rightsize oci://ghcr.io/sebtardiflabs/charts/kube-rightsize \
  --namespace kube-rightsize-system --create-namespace
```

### Create a Policy

Start in **Recommend** mode (safe, no changes applied):

```yaml
apiVersion: rightsize.io/v1alpha1
kind: RightSizePolicy
metadata:
  name: api-services
  namespace: production
spec:
  targetRef:
    kind: Deployment
    selector:
      matchLabels:
        tier: api
  metricsSource:
    prometheus:
      address: http://prometheus-server.monitoring:80
  cpu:
    percentile: 95
    safetyMargin: "1.2"
    bounds:
      min: "50m"
      max: "4000m"
  memory:
    percentile: 99
    safetyMargin: "1.3"
    bounds:
      min: "64Mi"
      max: "8Gi"
  updateStrategy:
    mode: Recommend
```

```bash
kubectl apply -f policy.yaml
```

### Check Recommendations

```bash
kubectl get rightsizepolicies -n production
# NAME            MODE        WORKLOADS   RESIZED   READY              AGE
# api-services    Recommend   3           0         InsufficientData   5m
```

> **Note:** The default `minimumDataPoints: 48` requires ~2 days of hourly
> Prometheus samples before recommendations appear. For faster evaluation,
> set `metricsSource.minimumDataPoints: 24` (1 day) or increase to 168
> (7 days) for higher confidence. See the
> [quickstart guide](docs/getting-started/quickstart.md) for details.

```bash
kubectl get rightsizepolicies api-services -n production -o yaml
# status.recommendations shows per-container recommendations with savings estimates
```

> **Upgrading?** Review the [changelog](CHANGELOG.md) for breaking metric label changes and default changes such as `minimumDataPoints`.
>
> **Helm installs:** If you use restrictive cluster networking, review the chart's generated [Helm README](charts/kube-rightsize/README.md#networkpolicy) before installing with the default `networkPolicy.enabled=true`. The policy allows webhook ingress on `9443`, metrics ingress on the configured `metrics.port` (default `8080`), DNS egress, Kubernetes API egress on `443`, and Prometheus egress on `networkPolicy.prometheusPort` (default `9090`).
>
### Upgrade to Canary Mode

Once you trust the recommendations, switch to Canary mode to apply changes
to 10% of pods first:

```yaml
spec:
  updateStrategy:
    mode: Canary
    canary:
      percentage: 10
      observationPeriod: 30m
    autoRevert: true
```

See the [examples/](examples/) directory for more scenarios: Auto mode,
HPA coexistence, cluster-wide defaults, and multi-workload selectors.

## kubectl Plugin

A `kubectl rightsize` plugin provides quick access to policy status,
savings, recommendations, and recommendation reasoning without raw YAML
parsing.

```bash
# Build the plugin
make build-plugin

# Copy to your PATH
cp bin/kubectl-rightsize /usr/local/bin/

# Usage
kubectl rightsize status -n production
kubectl rightsize savings -n production
kubectl rightsize recommendations -n production
kubectl rightsize explain api-services -n production

# All namespaces
kubectl rightsize status -A
```

Example output:

```
NAMESPACE    NAME           MODE      WORKLOADS   RESIZED   READY   RESIZING   DEGRADED   AGE
production   api-services   Canary    3           1         True    -          -          2d

NAMESPACE    POLICY         WORKLOAD     CONTAINER   CPU REQ   CPU REC   MEM REQ   MEM REC   CONFIDENCE
production   api-services   api-server   app         500m      320m      512Mi     384Mi     92.0%
```

## Grafana Dashboard

**Helm chart (recommended):** Enable `grafanaDashboard.enabled: true` in your
Helm values to auto-provision the dashboard via the Grafana sidecar:

```bash
helm upgrade kube-rightsize oci://ghcr.io/sebtardiflabs/charts/kube-rightsize \
  --set grafanaDashboard.enabled=true
```

**Manual import:** The raw JSON is at
[`deploy/grafana/dashboard.json`](deploy/grafana/dashboard.json). Import it
into Grafana and select your Prometheus data source.

The dashboard includes:
- **Overview**: total resizes, reverts, CPU/memory saved
- **Resize Operations**: resize rate by result, reverts by reason
- **Recommendations**: per-workload CPU/memory recommendations and confidence scores
- **Operator Health**: reconcile latency (p50/p99), Prometheus query duration, query errors

## Architecture

```
┌──────────────────────────────────────────────────┐
│                 kube-rightsize                     │
│                                                    │
│  Policy         Metrics         Recommender        │
│  Controller ──► Collector ──►  Engine              │
│       │                     (percentile -> margin  │
│       │                      -> confidence ->      │
│       ▼                      bounds clamping)      │
│  Resize         Safety                             │
│  Engine ◄────► Monitor                             │
│  (/resize       (OOMKill, throttle,                │
│   subresource)   restarts, auto-revert)            │
└──────────────────────────────────────────────────┘
         │                    │
         ▼                    ▼
    Kubernetes API       Prometheus
    (Pod /resize)        (usage data)
```

## Features

- **In-place resize**: Adjusts CPU and memory on running pods via the K8s 1.33+
  `/resize` subresource. No evictions, no rolling restarts.
- **Graduated rollout**: Five modes from zero-risk observation to full automation:
  Observe, Recommend, OneShot, Canary, Auto.
- **Auto-revert**: Automatically restores original resources if a resized pod gets
  OOMKilled, CPU-throttled, experiences restart spikes, or becomes NotReady.
- **HPA coexistence**: Right-sizes the base resource request without interfering
  with HPA's percentage-based scaling decisions.
- **Confidence scaling**: Recommendations widen automatically when data is sparse,
  becoming more precise as data accumulates.
- **Time-of-day awareness**: Uses hourly usage profiles so recommendations cover
  the busiest hour, not just the average.
- **Always-bounded recommendations**: Resource bounds (`min`/`max`) can be set
  per-policy. When omitted, safe defaults apply (CPU: 50m-4000m, Memory: 64Mi-8Gi).
- **Sidecar exclusion**: Skip specific containers (e.g., `istio-proxy`,
  `linkerd-proxy`) from recommendations and resizes via `excludeContainers`.
- **Node capacity guard**: Validates that total pod resource requests after resize
  will not exceed node allocatable, preventing eviction.
- **Prometheus auto-discovery**: Finds Prometheus automatically via the Prometheus
  Operator CRD or well-known service names when no explicit address is configured.
- **Conflict detection**: Detects and warns about VPA, other RightSizePolicy, or
  active Deployment rollouts targeting the same workload.
- **Cost savings estimation**: Computes `EstimatedMonthlySavings` based on
  configurable pricing (default: $0.031/vCPU-hr, $0.004/GiB-hr). Visible via
  `kubectl rightsize savings` and the Grafana dashboard.
- **LimitRange/ResourceQuota guard**: Skips resizes that would violate namespace
  LimitRange constraints or exceed ResourceQuota headroom.
- **Degraded condition**: Sets `Degraded` with reason `HighRevertRate` when 3+
  of the last 5 resizes are reverted, signaling parameters need adjustment.
- **Exponential backoff**: Cooldown doubles per consecutive revert (capped at
  16x), preventing repeated failed resizes.
- **Kubernetes Events**: Emits `Normal/Resized` and `Warning/Reverted` events
  on the policy, visible via `kubectl describe`.
- **Multi-data-source support**: Works with Thanos, VictoriaMetrics, Grafana
  Mimir, and managed Prometheus services. Custom headers, bearer token auth
  from Secrets, and TLS configuration available on `PrometheusConfig`.
- **Scheduled resize windows**: Restrict resizes to specific time windows
  and days of the week via `updateStrategy.schedule`. Supports timezones
  and overnight windows. Recommendations compute continuously regardless.
- **Per-cycle budget caps**: `maxTotalCpuIncrease` and `maxTotalMemoryIncrease`
  limit aggregate resource increases per reconcile cycle, preventing cluster-wide
  spikes when many pods need scaling up.
- **Eviction fallback**: Optional `resizeMethod: InPlaceOrEvict` falls back to
  pod eviction when in-place resize fails. Respects PDBs and never evicts
  the last replica. Default `InPlaceOnly` preserves current zero-disruption behavior.
- **Batch workload support**: Targets CronJobs and Jobs for recommend-only
  right-sizing based on historical usage of completed pods.
- **Namespace-scoped defaults**: `RightSizeNamespaceDefaults` provides
  per-namespace defaults that override the cluster-scoped `RightSizeDefaults`,
  enabling different configurations for production vs staging.

## Documentation

| Guide | Description |
|-------|-------------|
| [Why kube-rightsize?](docs/why-kube-rightsize.md) | The problem, why VPA fails, and how in-place resize changes everything |
| [Savings Calculator](docs/savings-calculator.md) | Estimate your monthly savings with an interactive calculator |
| [Quickstart](docs/getting-started/quickstart.md) | Get running in 5 minutes |
| [Migrating from VPA](docs/guides/migrating-from-vpa.md) | Step-by-step VPA replacement |
| [HPA Coexistence](docs/guides/hpa-coexistence.md) | Running alongside HPA |
| [Canary Rollout](docs/guides/canary-rollout.md) | Graduated rollout strategy |
| [CLI Reference](docs/reference/cli.md) | kubectl plugin commands |
| [API Reference](docs/reference/api.md) | CRD specification |
| [Troubleshooting](docs/guides/troubleshooting.md) | Common issues and solutions |
| [Examples](examples/) | Ready-to-use policy manifests |
| [Contributing](CONTRIBUTING.md) | Development setup and guidelines |

## License

Apache License 2.0. See [LICENSE](LICENSE) for details.
