# kube-rightsize

**Safe, in-place Kubernetes pod resource right-sizing. VPA done right.**

kube-rightsize is a Kubernetes operator that automatically right-sizes pod
resource requests and limits using
[In-Place Pod Resize](https://kubernetes.io/blog/2025/12/19/kubernetes-v1-35-in-place-pod-resize-ga/)
(GA in Kubernetes 1.33+). In-place by default, optional eviction fallback for
infeasible resizes, and no HPA conflicts.

## The Problem

Average Kubernetes CPU utilization is **8%**. That means 92% of the compute
you're paying for is idle. Industry-wide, this adds up to
**$44.5 billion** in projected cloud waste (Harness 2025), and **70%** of
organizations cite overprovisioning as their #1 cost driver (CNCF 2023).

The existing tool for this, VPA, evicts pods to resize them. It conflicts
with HPA, causes cascading failures, and fewer than **1%** of teams run
it fully automated (ScaleOps 2026). Recommendation-only tools like
Goldilocks show you the numbers but leave you with hundreds of YAML edits
that sit in the backlog for months.

Kubernetes 1.33 changed this by graduating In-Place Pod Resize to GA. The
foundation for non-disruptive right-sizing now exists. kube-rightsize is
the operator built to use it.

## How It's Different

| | VPA | Goldilocks | kube-rightsize |
|---|---|---|---|
| Resize method | Evicts pods | No resize (recommend only) | **In-place** (no restarts) |
| HPA compatible | No (death spirals) | N/A | **Yes** (adjusts base, not %) |
| Safety | Minimal guardrails | N/A | **Graduated rollout + auto-revert** |
| Algorithm | Backward-looking histograms | VPA recommender | **Time-of-day-aware + burst detection** |
| Production path | <1% use automated | N/A | **Observe, Recommend, Canary, Auto** |

## Who Is This For?

- **Platform teams** managing dozens of namespaces where developers set
  resource requests once and never look at them again.
- **FinOps teams** that need concrete dollar estimates per workload and a
  safe path from "we know it's overprovisioned" to "it's fixed."
- **SREs** running latency-sensitive services where pod restarts during peak
  traffic are not an option.
- **Anyone running HPA** who has been told "VPA and HPA don't mix."

## Key Features

- **In-place resize** via the Kubernetes 1.33+ `/resize` subresource
- **Graduated rollout**: Observe, Recommend, OneShot, Canary, Auto
- **Auto-revert** on OOMKill, CPU throttle, restart spikes, or pod NotReady
- **HPA coexistence** without death spirals
- **Confidence scaling** for sparse data
- **Time-of-day awareness** for bursty workloads
- **Mandatory bounds** (no unbounded recommendations)

**[Estimate your savings](savings-calculator.md)** with the interactive
calculator, or read **[Why kube-rightsize?](why-kube-rightsize.md)** for
the full story.

## Get Started

- [Installation](getting-started/installation.md) -- Helm install in 5 minutes
- [Quick Start](getting-started/quickstart.md) -- Create your first policy
- [Migrating from VPA](guides/migrating-from-vpa.md) -- Step-by-step replacement

## Reference

- [API Reference](reference/api.md)
- [CLI Reference](reference/cli.md)
- [Configuration](reference/configuration.md)
- [Specification](SPEC.md)
- [Contributing](https://github.com/SebTardifLabs/kube-rightsize/blob/main/CONTRIBUTING.md)
