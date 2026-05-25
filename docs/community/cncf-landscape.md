# CNCF Landscape

Attune is listed on the [CNCF Cloud Native Landscape](https://landscape.cncf.io/)
under **Orchestration & Management > Scheduling & Orchestration**, alongside
projects like VPA, Karpenter, and KEDA.

## Category

**Scheduling & Orchestration** -- tools that manage when, where, and how
workloads run on Kubernetes clusters. Attune fits here because it
dynamically adjusts pod resource allocations based on observed usage, which
is the resource-scheduling complement to horizontal autoscaling.

## Submission details

| Field | Value |
|-------|-------|
| **Name** | Attune |
| **Homepage** | [github.com/attune-io/attune](https://github.com/attune-io/attune) |
| **Repository** | [github.com/attune-io/attune](https://github.com/attune-io/attune) |
| **License** | Apache 2.0 |
| **Category** | Scheduling & Orchestration |
| **Description** | Safe in-place Kubernetes pod resource right-sizing operator. Replaces VPA with non-disruptive resizing via the K8s 1.32+ resize subresource. |

## How to submit

The CNCF Landscape is maintained at
[github.com/cncf/landscape](https://github.com/cncf/landscape). To add a
project, submit a PR that adds an entry to `landscape.yml`:

```yaml
- item:
    name: attune
    homepage_url: https://github.com/attune-io/attune
    repo_url: https://github.com/attune-io/attune
    logo: attune.svg
    twitter: null
    crunchbase: null
    description: >-
      Safe in-place Kubernetes pod resource right-sizing operator.
      Replaces VPA with non-disruptive resizing via the K8s 1.32+
      resize subresource. Five graduated modes (Observe, Recommend,
      OneShot, Canary, Auto), multi-layer safety with auto-revert,
      HPA coexistence, time-of-day awareness, and confidence-scaled
      recommendations.
```

### Logo requirements

The CNCF Landscape requires logos in SVG format. The project logo is
available at [`docs/logo.svg`](../logo.svg). Requirements:

- SVG format (vector)
- Square aspect ratio preferred
- No text in the logo (project name is rendered separately)
- Clean paths, no embedded raster images

## Related landscape projects

| Project | Category | Relationship |
|---------|----------|-------------|
| [VPA](https://github.com/kubernetes/autoscaler/tree/master/vertical-pod-autoscaler) | Scheduling & Orchestration | Attune replaces VPA's eviction-based approach with in-place resize |
| [Karpenter](https://karpenter.sh/) | Scheduling & Orchestration | Complementary: Karpenter handles node provisioning, Attune handles pod sizing |
| [KEDA](https://keda.sh/) | Scheduling & Orchestration | Complementary: KEDA handles event-driven horizontal scaling |
| [Goldilocks](https://github.com/FairwindsOps/goldilocks) | Scheduling & Orchestration | Attune goes beyond recommendations to automated in-place resize |