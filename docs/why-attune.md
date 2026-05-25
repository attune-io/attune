# Why Attune

## The $44 Billion Problem You're Contributing To

Every Kubernetes cluster wastes money. Not "maybe" or "probably." **Every single one.**

The numbers are staggering:

| Stat | Source |
|------|--------|
| **8%** average CPU utilization across K8s clusters | [CAST AI 2026 State of K8s Optimization](https://cast.ai/reports/state-of-kubernetes-optimization/) |
| **99.94%** of clusters are overprovisioned | [CAST AI 2025 Cost Benchmark](https://cast.ai/reports/kubernetes-cost-benchmark/) |
| **83%** of container costs are idle resources | [Datadog State of Cloud Costs 2024](https://www.datadoghq.com/state-of-cloud-costs/) |
| **$44.5 billion** in projected cloud infrastructure waste for 2025 | [Harness FinOps in Focus 2025](https://www.harness.io/finops-in-focus) |
| **70%** cite overprovisioning as the #1 cost driver | [CNCF FinOps Microsurvey 2023](https://www.cncf.io/blog/2023/12/20/cncf-cloud-native-finops-cloud-financial-management-microsurvey/) |

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

### Recommendation-only tools don't solve this

Tools like Goldilocks and Robusta KRR took a different approach: skip VPA's
dangerous Auto mode entirely and just show you the recommendations. Goldilocks
creates a dashboard. KRR prints a table. Both are useful for a one-time audit.

The problem is what happens next. For a platform running 200 microservices,
"useful recommendations" means 200 Deployment YAML edits, 200 pull requests,
200 code reviews, and 200 rollouts. Most teams create a Jira ticket titled
"right-size services," and it sits in the backlog for six months. The
recommendations go stale. New services deploy with the same inflated defaults.
Nothing changes.

Diagnostic tools tell you what to fix. They don't fix it. At scale, the gap
between "knowing" and "doing" is where savings go to die.

Attune closes that gap. It computes the recommendation AND applies it
to the running pod, with graduated safety controls so you don't have to
babysit each change. No YAML edits, no pull requests, no backlog tickets.

## What Changed: In-Place Pod Resize (Kubernetes 1.32+)

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

## Enter Attune

**Attune is a Kubernetes operator built exclusively for in-place pod
right-sizing.** It was designed from the ground up for the post-KEP-1287 world,
not retrofitted onto a VPA architecture that was never meant for it.

### How it works

```
 You deploy a AttunePolicy CR
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
 │  + overhead          │
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

You don't have to go from zero to fully-automated overnight. Attune
provides a graduated path:

| Mode | What happens | Risk level |
|------|-------------|------------|
| **Observe** | Collects metrics and tracks data-point progress; no recommendations surfaced | Zero |
| **Recommend** | Collects metrics and writes recommendations to the policy status | Zero |
| **OneShot** | Resizes one pod per reconciliation cycle, then stops | Minimal |
| **Canary** | Resizes 10% of pods first, watches them, then auto-promotes to the rest (optional) | Low |
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

Attune adjusts the *base resource request*, not the replica count.
This means HPA continues to scale horizontally based on its configured
metrics, while Attune ensures each replica is right-sized. No death
spiral. No conflicting signals.

### Time-of-day awareness

Unlike VPA's single-number histogram, Attune buckets usage data into
24 hourly profiles and takes the **maximum across all hours**. This means:

- A workload that peaks at 2 PM gets a recommendation based on that peak
- Overnight batch jobs don't drag down daytime recommendations
- Weekend vs. weekday patterns are captured in the overall percentile

### Confidence-scaled recommendations

When data is sparse (you just deployed the policy, or the workload is new),
recommendations are automatically inflated to be conservative. As more data
accumulates, confidence increases and recommendations become more precise.

With a full 7-day history window and the default `queryStep: 5m`, confidence reaches
1.0 and the recommendation reflects actual observed behavior with minimal padding.

## Who Is This For?

### Platform engineering teams

You manage dozens or hundreds of namespaces. Developers set resource requests
once and never look at them again. You're tired of fielding tickets about
cluster capacity while dashboards show 8% utilization.

**Attune gives you cluster-wide and namespace-scoped defaults**.
Use `AttuneDefaults` for cluster baselines, `AttuneNamespaceDefaults`
for environment- or team-specific overrides, and per-namespace
`AttunePolicy` resources for workload-level customization.

### FinOps teams

You know the cluster is overprovisioned but can't quantify it or fix it
without disrupting production. The Grafana dashboard and `kubectl attune
savings` command give you concrete dollar estimates per workload, and the
graduated rollout modes let you capture savings without risk.

### SREs running latency-sensitive services

You can't afford pod restarts during peak traffic. VPA is off the table. But
you also know your services are requesting 4 cores and using 0.5. In-place
resize lets you reclaim that 3.5 cores without touching a single pod lifecycle.

### Teams running HPA

You've been told "VPA and HPA don't mix." Attune fixes the base
request so each HPA-scaled replica is right-sized, while HPA continues to
handle horizontal scaling. They complement each other.

### Anyone running Kubernetes 1.32+

If your cluster supports in-place pod resize, you're leaving money on the
table by not using it.

## Real-World Scenario: An API Service

Let's walk through a concrete example.

**Before attune:**

| Resource | Requested | Actual P95 usage | Utilization |
|----------|-----------|------------------|-------------|
| CPU | 2000m (2 cores) | 400m | 20% |
| Memory | 4Gi | 1.2Gi | 30% |

This deployment has 10 replicas. On AWS EKS at on-demand pricing:

- CPU waste: 1.6 cores x 10 replicas x $0.031/core-hr x 730 hr/mo = **$362/mo**
- Memory waste: 2.8 GiB x 10 replicas x $0.004/GiB-hr x 730 hr/mo = **$82/mo**
- **Total waste: $444/month for one service**

**After Attune (with P95 + 20% overhead):**

| Resource | Original | Recommended | Savings |
|----------|----------|-------------|---------|
| CPU | 2000m | 480m (400m + 20%) | **76%** |
| Memory | 4Gi | 1.56Gi (1.2Gi + 30%) | **61%** |

The operator applies this change in-place. No restarts. No HPA interference.
The pods continue serving traffic with the same performance, just using fewer
reserved resources.

Now multiply this across 50 services and you're saving $20,000+/month.

## How Attune Compares

The Kubernetes rightsizing ecosystem spans 16+ tools, from open-source
recommenders to full-stack commercial platforms. Here is how they compare
across the capabilities that matter most.

### Open-source tools

| | VPA | Goldilocks | KRR (Robusta) | Oblik | kube-reqsizer | **Attune** |
|---|---|---|---|---|---|---|
| **Primary function** | Recommend + apply | VPA dashboard | CLI recommender | VPA applier | Usage-based controller | **Recommend + in-place apply** |
| **Resize method** | Evict/recreate, InPlaceOrRecreate (1.33+) | No resize | No resize | Cron-based rollout | Rolling restart | **In-place only** |
| **HPA compatible** | No (conflicts on CPU metric) | N/A | N/A | N/A | No | **Yes** |
| **Safety system** | Minimal (PDB only) | N/A | N/A | min-diff thresholds | None | **Multi-layer (OOMKill, throttle, revert)** |
| **Time-of-day aware** | No (24h half-life histogram) | No | No | No | No | **Yes (hourly profiles)** |
| **Graduated rollout** | No (all-or-nothing) | N/A | N/A | No | No | **5 modes (Observe to Auto)** |
| **Per-resource config** | containerPolicies[] | N/A | CLI flags | Annotations per resource | N/A | **Typed CRD (cpu/memory sections)** |
| **Confidence scaling** | Internal, not configurable | N/A | N/A | N/A | N/A | **Configurable, visible in status** |
| **Config model** | CRD | VPA + labels | CLI flags | CRD + annotations | Annotations | **CRD + defaults hierarchy** |
| **Cluster-wide defaults** | No | No | No | Env vars | No | **Yes (AttuneDefaults CRD)** |

### Commercial platforms

| | CAST AI | StormForge | ScaleOps | PerfectScale | Datadog | nOps | Spot Ocean | Sedai |
|---|---|---|---|---|---|---|---|---|
| **Resize method** | In-place + rollout | In-place + rollout | In-place + rollout | In-place + rollout | In-place + rollout | VPA-based | VPA-based | Agent-based |
| **Recommender** | ML, usage-based | ML (Bayesian) | Real-time + burst | AI + risk scoring | Usage histograms | VPA + policies | VPA | Reinforcement learning |
| **HPA coordination** | Yes | Yes (adjusts targets) | Yes | Yes | Yes (unified CRD) | Partial | Partial | Yes |
| **Per-step change cap** | Change sensitivity % | maxPercentIncrease/Decrease | N/A | Policy levels | N/A | N/A | N/A | SLO guardrails |
| **Graduated rollout** | Immediate/deferred | Incremental % steps | Continuous | Risk-scored | Preview/Apply modes | Scheduled windows | N/A | DataPilot to AutoPilot |
| **Config model** | Proprietary CRD | Annotations | Proprietary CRD | Hierarchical CRDs | DatadogPodAutoscaler CRD | VPA + annotations | Standard VPA CRD | Platform API |
| **Node optimization** | Yes (Spot, bin-pack) | No | Yes | No | No | Yes (EKS) | Yes (Spot, headroom) | Yes |
| **Self-hosted option** | No (SaaS) | Hybrid | Yes | No (SaaS) | Agent + SaaS | No (SaaS) | No (SaaS) | No (SaaS) |
| **Open source** | No | No | No | No | Agent only | No | No | No |
| **Typical cost** | % of savings | Per-cluster | Per-cluster | Per-cluster | Included with Datadog | % of savings | Per-node | Per-workload |

### Attune vs. the field

| Capability | Attune | How many of 16 tools have it |
|-----------|---------------|----------------------------|
| In-place resize (no eviction) | Yes | 9/16 (VPA 1.33+, CAST AI, StormForge, ScaleOps, PerfectScale, Datadog, nOps, Spot Ocean, Kedify) |
| HPA coexistence | Yes | 7/16 |
| Multi-layer safety with auto-revert | Yes | 2/16 (us, PerfectScale) |
| Graduated rollout (3+ modes) | Yes | 4/16 (us, StormForge, PerfectScale, Sedai) |
| Time-of-day awareness | Yes | 3/16 (us, StormForge, ScaleOps) |
| Cluster-wide defaults CRD | Yes | 2/16 (us, PerfectScale) |
| Fully open source (Apache 2.0) | Yes | 5/16 (VPA, Goldilocks, KRR, Oblik, kube-reqsizer) |
| No SaaS dependency | Yes | 7/16 (all OSS + ScaleOps) |
| kubectl plugin with savings estimates | Yes | 0/16 |
| Canary rollout for resizes | Yes | 0/16 |

### Where commercial tools win

Commercial platforms like CAST AI, ScaleOps, and StormForge offer
capabilities that Attune intentionally does not cover:

- **Node optimization**: Spot instance management, bin-packing, and cluster
  autoscaling. Pair Attune with
  [Karpenter](https://karpenter.sh/) for an open-source equivalent.
- **ML/predictive recommenders**: Bayesian optimization (StormForge),
  reinforcement learning (Sedai), and risk-scored automation (PerfectScale)
  can outperform percentile-based recommendations for highly variable workloads.
- **Multi-cloud dashboards**: Unified cost views across AWS, GCP, and Azure
  with commitment optimization.

### Where Attune wins

- **Full control**: The recommendation algorithm is open, auditable, and
  modifiable. No black-box ML.
- **No SaaS dependency**: Your metrics stay in your Prometheus. No data
  leaves the cluster.
- **Kubernetes-native**: Standard CRDs, conditions, events, and kubectl
  plugin. Works with existing GitOps workflows.
- **Safety-first**: The only open-source tool with OOMKill detection,
  CPU throttle monitoring, restart spike detection, and automatic revert
  with exponential backoff.
- **Cost**: Free forever (Apache 2.0). Commercial tools charge $10,000-50,000+/year
  for large clusters.

## Getting Started

It takes less than 5 minutes to start seeing recommendations.

### 1. Install the operator

```bash
helm install attune oci://ghcr.io/attune-io/charts/attune \
  --namespace attune-system --create-namespace
```

### 2. Create your first policy

```yaml
apiVersion: attune.io/v1alpha1
kind: AttunePolicy
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
    type: Recommend
```

### 3. Wait for data, then review

```bash
# After enough data collection for your queryStep
kubectl attune recommendations -n default
kubectl attune savings -n default
```

### 4. Promote to Canary, then Auto

```bash
# Try on 10% of pods first (autoPromote handles the rest)
kubectl patch rsp my-app --type merge \
  -p '{"spec":{"updateStrategy":{"type":"Canary","canary":{"percentage":10,"autoPromote":true},"autoRevert":true}}}'
```

With `autoPromote: true`, the operator automatically promotes to the full
fleet after the observation period passes without safety violations. No
manual mode switch needed.

That's it. No agents to install. No SaaS to configure. No pods to restart.

---

**Next steps:**

- [Estimate your savings](savings-calculator.md) with the interactive calculator
- [Quick Start guide](getting-started/quickstart.md) for a hands-on walkthrough
- [Migrating from VPA](guides/migrating-from-vpa.md) if you're replacing an
  existing VPA setup
- [Concepts](getting-started/concepts.md) for a deep dive into how the
  recommendation engine works
