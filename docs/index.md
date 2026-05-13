# kube-rightsize

**Safe, in-place Kubernetes pod resource right-sizing. VPA done right.**

kube-rightsize is a Kubernetes operator that automatically right-sizes pod
resource requests and limits using
[In-Place Pod Resize](https://kubernetes.io/blog/2025/12/19/kubernetes-v1-35-in-place-pod-resize-ga/)
(GA in Kubernetes 1.33+). No pod restarts. No evictions. No HPA conflicts.

## The Problem

| Stat | Source |
|------|--------|
| Average CPU utilization is **8%** | CAST AI 2026 Report |
| **70%** cite overprovisioning as #1 cost driver | CNCF FinOps Microsurvey 2023 |
| **<1%** run VPA fully automated in production | ScaleOps 2026 |
| In-Place Pod Resize is **GA** since K8s 1.33 | KEP-1287, 2025 |

Developers set resource requests once and never revisit them. VPA was supposed
to fix this but it evicts pods, conflicts with HPA, and has caused cluster
outages. kube-rightsize replaces VPA with a safety-first operator built
exclusively for in-place resize.

## Key Features

- **In-place resize** via the Kubernetes 1.33+ `/resize` subresource
- **Graduated rollout**: Observe, Recommend, OneShot, Canary, Auto
- **Auto-revert** on OOMKill, CPU throttle, restart spikes, or pod NotReady
- **HPA coexistence** without death spirals
- **Confidence scaling** for sparse data
- **Time-of-day awareness** for bursty workloads
- **Mandatory bounds** (no unbounded recommendations)

## Quick Links

- **[Why kube-rightsize?](why-kube-rightsize.md)** -- The problem, why VPA
  fails, and how in-place resize changes everything
- **[Savings Calculator](savings-calculator.md)** -- Estimate your monthly
  savings with an interactive calculator
- [Installation](getting-started/installation.md)
- [Quick Start](getting-started/quickstart.md)
- [Specification](SPEC.md)
- [API Reference](reference/api.md)
- [Contributing](https://github.com/SebTardifLabs/kube-rightsize/blob/main/CONTRIBUTING.md)
