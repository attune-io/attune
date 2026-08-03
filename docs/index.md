# Attune

**Safe, in-place Kubernetes pod resource right-sizing. VPA done right.**

Attune is a Kubernetes operator that automatically right-sizes pod
resource requests and limits using
[In-Place Pod Resize](https://kubernetes.io/blog/2025/12/19/kubernetes-v1-35-in-place-pod-resize-ga/)
(**GA** in Kubernetes 1.35, beta and enabled by default in 1.33–1.34, alpha with
feature gate in 1.32). In-place by default, optional eviction fallback for
infeasible resizes, and no HPA conflicts.

## The Problem

Average Kubernetes CPU utilization is **8%**. That means 92% of the compute
you're paying for is idle. Industry-wide, this adds up to
**$44.5 billion** in projected cloud waste ([Harness 2025](https://www.harness.io/finops-in-focus)), and **70%** of
organizations cite overprovisioning as their #1 cost driver ([CNCF 2023](https://www.cncf.io/blog/2023/12/20/cncf-cloud-native-finops-cloud-financial-management-microsurvey/)).

The existing tool for this, VPA, evicts pods to resize them. It conflicts
with HPA, causes cascading failures, and fewer than **1%** of teams run
it fully automated ([ScaleOps 2026](https://scaleops.com/blog/why-pod-rightsizing-fails-in-production-a-deep-dive-into-vpa-and-what-actually-works/)). Recommendation-only tools like
Goldilocks show you the numbers but leave you with hundreds of YAML edits
that sit in the backlog for months.

Kubernetes 1.35 graduated In-Place Pod Resize to **GA** (stable). The feature
was beta and enabled by default from 1.33, and available as alpha with a
feature gate on 1.32. The foundation for non-disruptive right-sizing is stable.
Attune is the operator built for the safe unattended loop on top of that
primitive.

## How It's Different

In-place apply is the shared Kubernetes primitive. Attune adds the production
path: canary blast-radius control, startup boost, and SLO-backed auto-revert.

| | VPA | Goldilocks | Attune |
|---|---|---|---|
| Resize method | Eviction-based Auto historically; newer modes can use in-place where supported | No resize (recommend only) | **In-place by default** (optional eviction fallback) |
| HPA compatible | Risky on the same metric (death spirals) | N/A | **Yes** (adjusts base requests, not HPA %) |
| Safety | Minimal guardrails | N/A | **Auto-revert** + **SLO PromQL guardrails** |
| Blast radius | All targeted pods | N/A | **Canary** with observation and optional auto-promote |
| Cold start | None | N/A | **Startup boost**, then scale back |
| Algorithm | Backward-looking histograms | VPA recommender | **Time-of-day-aware + burst detection + confidence** |
| Production path | <1% use automated | Manual apply | **Observe → Recommend → Canary → Auto** |

## Who Is This For?

- **Platform teams** managing dozens of namespaces where developers set
  resource requests once and never look at them again.
- **FinOps teams** that need concrete dollar estimates per workload and a
  safe path from "we know it's overprovisioned" to "it's fixed."
- **SREs** running latency-sensitive services where pod restarts during peak
  traffic are not an option.
- **Anyone running HPA** who has been told "VPA and HPA don't mix."

## Key Features

- **In-place resize** via the Kubernetes `/resize` subresource (GA in 1.35)
- **Graduated rollout**: Observe, Recommend, OneShot, Canary, Auto
- **Canary rollout** with observation period and optional auto-promote
- **Startup boost** for cold-start / JIT CPU headroom
- **Auto-revert** on OOMKill, CPU throttle, restart spikes, pod NotReady, or SLO guardrail breach
- **HPA coexistence** without death spirals
- **Confidence scaling** for sparse data
- **Time-of-day awareness** for bursty workloads
- **Mandatory bounds** (no unbounded recommendations)
- **GitOps export** (versioned recommendation ConfigMaps) and optional [PR automation](guides/gitops-integration.md#pull-request-automation-opt-in-phase-b)
- **Multi-cluster fleet** reporting and Grafana ([multi-cluster guide](guides/multi-cluster.md#fleet-observability-with-federated-prometheus))
- **Memory usage floor** when decreasing limits on Kubernetes 1.35+
- **Runtime profiles** for safer memory defaults by language
- **Capacity and pressure awareness** with reclaimed-request signals for bin packing

**[Estimate your savings](savings-calculator.md)** with the interactive
calculator, or read **[Why Attune?](why-attune.md)** for
the full story.

## Get Started

- [Installation](getting-started/installation.md) -- Helm install in 5 minutes
- [Quick Start](getting-started/quickstart.md) -- Create your first policy
- [Migrating from VPA](guides/migrating-from-vpa.md) -- Step-by-step replacement

## Metrics Sources

Attune works with multiple metrics backends. One source is configured per
policy:

| Backend | Guide | Use case |
|---------|-------|----------|
| **Prometheus** (+ Thanos, Mimir, VictoriaMetrics) | [Prometheus Setup](guides/prometheus-setup.md) | Default for most clusters; auto-discovery available |
| **Datadog** | [Datadog Setup](guides/datadog-setup.md) | Teams already using Datadog for Kubernetes monitoring |
| **CloudWatch** Container Insights | [CloudWatch Setup](guides/cloudwatch-setup.md) | EKS clusters using AWS-native observability |

## Reference

- [API Reference](reference/api.md)
- [CLI Reference](reference/cli.md)
- [Configuration](reference/configuration.md)
- [Specification](SPEC.md)
- [Contributing](https://github.com/attune-io/attune/blob/main/CONTRIBUTING.md)
