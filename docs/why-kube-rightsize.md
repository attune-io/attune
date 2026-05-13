# Why kube-rightsize

## The $44 Billion Problem You're Contributing To

Every Kubernetes cluster wastes money. Not "maybe" or "probably." **Every single one.**

The numbers are staggering:

| Stat | Source |
|------|--------|
| **8%** average CPU utilization across K8s clusters | [CAST AI 2026 State of K8s Optimization](https://cast.ai/reports/state-of-kubernetes-optimization/) |
| **99.94%** of clusters are overprovisioned | [CAST AI 2025 Cost Benchmark](https://cast.ai/blog/kubernetes-cost-report/) |
| **83%** of container costs are idle resources | [Datadog State of Cloud Costs 2024](https://www.datadoghq.com/state-of-cloud-costs/) |
| **$44.5 billion** in projected cloud infrastructure waste for 2025 | [Harness FinOps in Focus 2025](https://www.harness.io/blog/finops-in-focus-report) |
| **70%** cite overprovisioning as the #1 cost driver | [CNCF FinOps Microsurvey 2023](https://www.cncf.io/reports/cncf-finops-microsurvey-2023/) |

Here's what's happening: your developers set `resources.requests` to "something
that works," add a generous safety margin because they don't want 3 AM pages,
and never touch it again. Multiply that across every container in every
deployment in every namespace, and you're paying for 10x the compute you
actually use.

## Why Nobody Fixes This (Even Though Everyone Knows About It)

The Kubernetes ecosystem has had a tool for this since 2018: the **Vertical Pod
Autoscaler (VPA)**. So why does less than 1% of the industry run it in
production?

### VPA evicts your pods

VPA's "Auto" mode works by **evicting pods and recreating them** with new
resource values. In theory, this sounds fine. In practice, it means:

- **Pod restarts during traffic spikes.** VPA sees high usage, recommends more
  resources, and evicts the pod to apply them. The pod restarts, loses its
  in-memory state, and starts handling requests from a cold cache, exactly when
  you need it most.

- **JVM cold start penalties.** Java applications lose their JIT-compiled code
  on every restart. A warmed-up JVM might handle 10,000 req/s; a cold one
  handles 2,000. VPA evictions can cause a 5x throughput drop that cascades
  through your service mesh.

- **Stateful workload disruption.** Databases, message brokers, and ML training
  jobs lose progress. A 6-hour training run evicted at hour 5 wastes 5 hours of
  GPU time.

- **Cascading failures.** When VPA evicts a Prometheus pod (which it monitors
  itself through), the recommender loses its data source and starts making
  blind recommendations. This [actually happened in production](https://medium.com/learnings-from-the-paas/when-verticalpodautoscaler-goes-rogue-how-an-autoscaler-took-down-our-cluster-8c7479d5be3c)
  and took down an entire cluster's observability.

### VPA fights with HPA

If you're running Horizontal Pod Autoscaler (HPA) on the same workloads (and
you probably should be), VPA creates a death spiral:

1. VPA sees low average CPU usage and **lowers resource requests**
2. HPA sees CPU utilization spike (because the same load on smaller requests =
   higher percentage) and **scales out**
3. More replicas with lower requests = lower average usage again
4. VPA lowers requests further
5. Eventually pods are too small to handle even a single request, and
   everything falls over

This is why the official Kubernetes documentation explicitly warns against
running VPA and HPA on the same metric.

### VPA's recommender is a black box

VPA uses backward-looking exponential histograms with a 24-hour half-life.
This means:

- It reacts slowly to genuine load increases
- It doesn't understand time-of-day patterns (your 2 AM recommendation
  shouldn't be based on your 2 PM peak)
- It treats all workloads the same, whether it's a CPU-intensive API server or
  a memory-heavy cache
- The recommendations are not bounded by default, so VPA can recommend
  resources larger than any node in your cluster

### The result: nobody uses VPA in production

Teams install VPA in `Recommend` mode, look at the numbers, manually apply some
changes once a quarter, and move on. The promise of automated right-sizing
remains unfulfilled.

## What Changed: In-Place Pod Resize (Kubernetes 1.33+)

In December 2025, Kubernetes v1.35 graduated
[In-Place Pod Resize](https://kubernetes.io/blog/2025/12/19/kubernetes-v1-35-in-place-pod-resize-ga/)
to GA (stable). This feature, tracked as
[KEP-1287](https://github.com/kubernetes/enhancements/tree/master/keps/sig-node/1287-in-place-update-pod-resources),
was 7 years in the making.

**What it does:** The kubelet can now adjust a container's CPU and memory
limits by modifying the cgroup configuration directly, without restarting the
container or evicting the pod. Your application never notices.

This changes everything. The entire reason VPA was dangerous, the eviction and
restart cycle, is no longer necessary. A smart operator can now:

- Read usage metrics from Prometheus
- Calculate optimal resource levels
- Apply them to running pods via the `/resize` subresource
- Monitor for problems and revert if needed

All without a single pod restart.

## Enter kube-rightsize

**kube-rightsize is a Kubernetes operator built exclusively for in-place pod
right-sizing.** It was designed from the ground up for the post-KEP-1287 world,
not retrofitted onto a VPA architecture that was never meant for it.

### How it works

```
 You deploy a RightSizePolicy CR
            │
            ▼
 ┌─────────────────────┐     ┌──────────────┐
 │  Metrics Collector   │────►│  Prometheus   │
 │  (hourly profiles)   │◄────│  (CPU + Mem)  │
 └─────────┬───────────┘     └──────────────┘
           │
           ▼
 ┌─────────────────────┐
 │  Recommender Engine  │
 │  P95/P99 percentile  │
 │  + safety margin     │
 │  + confidence scaling│
 │  + bounds clamping   │
 │  + change filter     │
 └─────────┬───────────┘
           │
           ▼
 ┌─────────────────────┐     ┌──────────────┐
 │   Resize Engine      │────►│  K8s API     │
 │   (/resize sub-      │     │  /resize     │
 │    resource)         │     │  subresource │
 └─────────┬───────────┘     └──────────────┘
           │
           ▼
 ┌─────────────────────┐
 │   Safety Monitor     │
 │   OOMKill detection  │
 │   CPU throttle check │
 │   Restart spike      │
 │   Pod NotReady       │
 │   Auto-revert        │
 └──────────────────────┘
```

### Five modes for every comfort level

You don't have to go from zero to fully-automated overnight. kube-rightsize
provides a graduated path:

| Mode | What happens | Risk level |
|------|-------------|------------|
| **Observe** | Collects metrics and tracks data-point progress; no recommendations surfaced | Zero |
| **Recommend** | Collects metrics and writes recommendations to the policy status | Zero |
| **OneShot** | Resizes one pod per reconciliation cycle, then stops | Minimal |
| **Canary** | Resizes 10% of pods first, watches them, then rolls out to the rest | Low |
| **Auto** | Continuously resizes all eligible pods based on observed metrics | Production-ready |

Most teams follow this progression:

```
Week 1-2:  Recommend mode  →  Validate recommendations look sane
Week 3:    Canary mode     →  Resize 10% of pods, watch for issues
Week 4+:   Auto mode       →  Let the operator handle it continuously
```

### Safety is not an afterthought

Every resize is guarded by a multi-layer safety system:

- **OOMKill detection**: If a resized container gets OOMKilled, the operator
  immediately reverts to the original resources.
- **CPU throttle monitoring**: If CPU throttling exceeds 50% post-resize, the
  operator reverts.
- **Restart spike detection**: 2+ restarts after a resize triggers a revert.
- **Pod health checks**: If the pod loses its `Ready` condition, the operator
  reverts.
- **Exponential backoff**: Each consecutive revert doubles the cooldown period
  (capped at 16x), so the operator doesn't keep hammering a problematic workload.
- **Degraded condition**: When 3+ of the last 5 resizes are reverted, the
  policy is flagged as `Degraded` so you know the parameters need tuning.
- **LimitRange/ResourceQuota guard**: Resizes that would violate namespace
  constraints are skipped entirely.
- **Node capacity guard**: The operator checks that total pod resource
  requests after resize won't exceed node allocatable.

### HPA coexistence, for real

kube-rightsize adjusts the *base resource request*, not the replica count.
This means HPA continues to scale horizontally based on its configured
metrics, while kube-rightsize ensures each replica is right-sized. No death
spiral. No conflicting signals.

### Time-of-day awareness

Unlike VPA's single-number histogram, kube-rightsize buckets usage data into
24 hourly profiles and takes the **maximum across all hours**. This means:

- A workload that peaks at 2 PM gets a recommendation based on that peak
- Overnight batch jobs don't drag down daytime recommendations
- Weekend vs. weekday patterns are captured in the overall percentile

### Confidence-scaled recommendations

When data is sparse (you just deployed the policy, or the workload is new),
recommendations are automatically inflated to be conservative. As more data
accumulates, confidence increases and recommendations become more precise.

After 7 days of hourly data (168 data points), confidence reaches 1.0 and the
recommendation reflects actual observed behavior with minimal padding.

## Who Is This For?

### Platform engineering teams

You manage dozens or hundreds of namespaces. Developers set resource requests
once and never look at them again. You're tired of fielding tickets about
cluster capacity while dashboards show 8% utilization.

**kube-rightsize gives you a single CRD** (`RightSizeDefaults`) to set
cluster-wide defaults, and per-namespace `RightSizePolicy` resources that
developers can customize.

### FinOps teams

You know the cluster is overprovisioned but can't quantify it or fix it
without disrupting production. The Grafana dashboard and `kubectl rightsize
savings` command give you concrete dollar estimates per workload, and the
graduated rollout modes let you capture savings without risk.

### SREs running latency-sensitive services

You can't afford pod restarts during peak traffic. VPA is off the table. But
you also know your services are requesting 4 cores and using 0.5. In-place
resize lets you reclaim that 3.5 cores without touching a single pod lifecycle.

### Teams running HPA

You've been told "VPA and HPA don't mix." kube-rightsize fixes the base
request so each HPA-scaled replica is right-sized, while HPA continues to
handle horizontal scaling. They complement each other.

### Anyone running Kubernetes 1.33+

If your cluster supports in-place pod resize, you're leaving money on the
table by not using it.

## Real-World Scenario: An API Service

Let's walk through a concrete example.

**Before kube-rightsize:**

| Resource | Requested | Actual P95 usage | Utilization |
|----------|-----------|------------------|-------------|
| CPU | 2000m (2 cores) | 400m | 20% |
| Memory | 4Gi | 1.2Gi | 30% |

This deployment has 10 replicas. On AWS EKS at on-demand pricing:

- CPU waste: 1.6 cores x 10 replicas x $0.031/core-hr x 730 hr/mo = **$362/mo**
- Memory waste: 2.8 GiB x 10 replicas x $0.004/GiB-hr x 730 hr/mo = **$82/mo**
- **Total waste: $444/month for one service**

**After kube-rightsize (with P95 + 20% safety margin):**

| Resource | Original | Recommended | Savings |
|----------|----------|-------------|---------|
| CPU | 2000m | 480m (400m x 1.2) | **76%** |
| Memory | 4Gi | 1.56Gi (1.2Gi x 1.3) | **61%** |

The operator applies this change in-place. No restarts. No HPA interference.
The pods continue serving traffic with the same performance, just using fewer
reserved resources.

Now multiply this across 50 services and you're saving $20,000+/month.

## How kube-rightsize Compares

| | VPA | Goldilocks | Commercial tools | **kube-rightsize** |
|---|---|---|---|---|
| **Resize method** | Evicts pods | No resize | Varies (some in-place) | **In-place only** |
| **HPA compatible** | No | N/A | Varies | **Yes** |
| **Safety system** | Minimal | N/A | Proprietary | **Open, multi-layer** |
| **Time-of-day aware** | No | No | Some | **Yes** |
| **Graduated rollout** | No (all-or-nothing) | N/A | Some | **5 modes** |
| **Cost** | Free | Free | $$$$ | **Free (Apache 2.0)** |
| **Lock-in** | None | None | Vendor-specific | **None** |
| **K8s version** | Any | Any | Any | **1.33+** |

### vs. Commercial alternatives (CAST AI, ScaleOps, Zesty)

Commercial tools like CAST AI, ScaleOps, and Zesty offer comprehensive
optimization suites that cover node provisioning, spot instances, and pod
right-sizing. They're excellent products, but they come with trade-offs:

- **Cost**: Commercial tools charge based on optimized spend or per-node. For
  large clusters, this can be $10,000-50,000+/year.
- **Data residency**: Your Prometheus metrics and cluster metadata flow
  through a third-party SaaS platform.
- **Vendor lock-in**: Proprietary CRDs, agents, and dashboards that don't
  follow Kubernetes-native patterns.

kube-rightsize is a focused, open-source alternative for teams that want:

- Full control over their right-sizing logic
- No SaaS dependency
- Kubernetes-native CRDs and tooling
- The ability to audit and modify the recommendation algorithm
- Integration with existing Prometheus and Grafana infrastructure

If you need the broader optimization suite (spot instances, node provisioning,
multi-cloud), pair kube-rightsize with [Karpenter](https://karpenter.sh/) for
a fully open-source stack.

## Getting Started

It takes less than 5 minutes to start seeing recommendations.

### 1. Install the operator

```bash
helm install kube-rightsize oci://ghcr.io/sebtardiflabs/charts/kube-rightsize \
  --namespace kube-rightsize-system --create-namespace
```

### 2. Create your first policy

```yaml
apiVersion: rightsize.io/v1alpha1
kind: RightSizePolicy
metadata:
  name: my-app
  namespace: default
spec:
  targetRef:
    kind: Deployment
    name: my-app
  metricsSource:
    prometheus:
      address: http://prometheus-server.monitoring:80
  updateStrategy:
    mode: Recommend
```

### 3. Wait for data, then review

```bash
# After 1-2 days of data collection
kubectl rightsize recommendations -n default
kubectl rightsize savings -n default
```

### 4. Promote to Canary, then Auto

```bash
# Try on 10% of pods first
kubectl patch rsp my-app --type merge \
  -p '{"spec":{"updateStrategy":{"mode":"Canary","canary":{"percentage":10},"autoRevert":true}}}'

# Once validated, go to Auto
kubectl patch rsp my-app --type merge \
  -p '{"spec":{"updateStrategy":{"mode":"Auto"}}}'
```

That's it. No agents to install. No SaaS to configure. No pods to restart.

---

**Next steps:**

- [Estimate your savings](savings-calculator.md) with the interactive calculator
- [Quick Start guide](getting-started/quickstart.md) for a hands-on walkthrough
- [Migrating from VPA](guides/migrating-from-vpa.md) if you're replacing an
  existing VPA setup
- [Concepts](getting-started/concepts.md) for a deep dive into how the
  recommendation engine works
