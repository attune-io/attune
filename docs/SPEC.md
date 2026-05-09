# kube-rightsize: Complete Specification

> Safe, in-place Kubernetes pod resource right-sizing operator.
> VPA done right, powered by In-Place Pod Resize (K8s 1.35+).

---

## Table of Contents

1. [Vision & Goals](#1-vision--goals)
2. [Technology Decisions](#2-technology-decisions)
3. [CRD Design](#3-crd-design)
4. [Architecture](#4-architecture)
5. [Algorithm Design](#5-algorithm-design)
6. [Resize Engine](#6-resize-engine)
7. [Safety System](#7-safety-system)
8. [Metrics & Observability](#8-metrics--observability)
9. [Testing Strategy](#9-testing-strategy)
10. [CI/CD Pipeline](#10-cicd-pipeline)
11. [Distribution](#11-distribution)
12. [Documentation](#12-documentation)
13. [Project Structure](#13-project-structure)
14. [Roadmap](#14-roadmap)
15. [Competitor Lessons](#15-competitor-lessons)

---

## 1. Vision & Goals

### Problem

99.94% of Kubernetes clusters are over-provisioned. Average CPU utilization is 8%, memory 20%
(CAST AI 2026). VPA, the tool designed to fix this, is universally feared: fewer than 1% of
organizations run it fully automated (ScaleOps 2026). VPA evicts pods, conflicts with HPA, and
has caused cluster-wide outages.

In December 2025, In-Place Pod Resize graduated to GA in Kubernetes 1.35 (KEP-1287). For the
first time, CPU and memory can be changed on running pods without restarts. This unlocks a
ground-up redesign of resource right-sizing.

### Mission

kube-rightsize is the first production-grade right-sizing operator built exclusively for
in-place resize. It exists to make VPA obsolete by delivering:

1. **Zero-downtime right-sizing**: Resize pods in-place without restarts (CPU) or with
   minimal container-only restarts (memory)
2. **Safety-first design**: Graduated rollout from observe to full-fleet, with automatic
   revert on OOMKill or throttle
3. **HPA coexistence**: Adjusts base resource requests without breaking HPA percentage targets
4. **Production confidence**: Composable recommendation algorithm with confidence-based
   widening for sparse data

### Non-Goals

- Traffic shifting or canary deployments (use Argo Rollouts/Flagger)
- Node-level autoscaling (use Karpenter/Cluster Autoscaler)
- Cost visibility dashboards (use OpenCost/Kubecost)
- GPU or ephemeral-storage right-sizing (not supported by in-place resize API)

---

## 2. Technology Decisions

### 2.1 Language: Go 1.26

| Factor | Decision |
|--------|----------|
| Language | Go 1.26.x |
| Module directive | `go 1.26` |
| Rationale | 85%+ of production K8s operators use Go. Largest ecosystem, hiring pool, and controller-runtime support. Green Tea GC (1.26) provides lower latency. |
| What competitors use | right-sizer: Go 1.25, OptiPod: Go 1.24.6, VPA: Go |
| What model operators use | CloudNativePG: Go 1.26.3, Kyverno: Go 1.26.2 |

### 2.2 Framework: Kubebuilder v4 + controller-runtime v0.24.0

| Component | Version | Purpose |
|-----------|---------|---------|
| Kubebuilder | v4.14.0 | Project scaffolding, Makefile, CRD generation |
| controller-runtime | v0.24.0 | Controller lifecycle, reconciliation, caching, webhooks |
| client-go | v0.36.x | K8s API access, `/resize` subresource calls |
| k8s.io/api | v0.36.x | K8s type definitions |
| k8s.io/apimachinery | v0.36.x | Resource quantities, conditions, meta types |

**Why Kubebuilder over Operator SDK**: For a new operator without OLM/OperatorHub requirements,
Kubebuilder provides the cleanest scaffolding. Operator SDK adds OLM bundle generation on top
of the same controller-runtime foundation. We can add Operator SDK later for OperatorHub
distribution.

**Why controller-runtime v0.24.0**: PriorityQueue (default since v0.23.0) enables prioritizing
resize reconciliations for critical pods. Subresource Apply support enables clean SSA patches
to the `/resize` subresource. Generic Validator/Defaulter webhooks provide type-safe CRD
validation.

### 2.3 Prometheus Querying

| Component | Module | Version |
|-----------|--------|---------|
| Query client | `github.com/prometheus/client_golang/api/prometheus/v1` | v1.23.2 |
| Result types | `github.com/prometheus/common/model` | transitive |

The official Prometheus Go client for querying (not exposing metrics). Returns typed results
(`model.Vector`, `model.Matrix`). Supports auth via custom `http.RoundTripper`.

### 2.4 Complete Dependency Table

```
go 1.26

# Core
sigs.k8s.io/controller-runtime          v0.24.0
k8s.io/client-go                        v0.36.x
k8s.io/api                              v0.36.x
k8s.io/apimachinery                     v0.36.x

# Prometheus querying
github.com/prometheus/client_golang     v1.23.2

# Testing
github.com/onsi/ginkgo/v2              latest
github.com/onsi/gomega                  latest
github.com/stretchr/testify             latest

# Tools (CI/build, not Go module deps)
kubebuilder                             v4.14.0
golangci-lint                           v2.12.x
goreleaser                              v2.15.x
ko                                      latest
cosign                                  latest
trivy                                   latest
chainsaw                                v0.2.15
ct (chart-testing)                      v3.14.x
crdoc                                   v0.6.4
helm-docs                               latest
```

---

## 3. CRD Design

### 3.1 API Group and Version

```
Group:   rightsize.io
Version: v1alpha1
```

### 3.2 RightSizePolicy (Namespaced)

The primary CRD. Defines a right-sizing policy for a set of workloads.

```yaml
apiVersion: rightsize.io/v1alpha1
kind: RightSizePolicy
metadata:
  name: api-services
  namespace: production
spec:
  # Which workloads to target
  targetRef:
    # Option A: specific workload
    kind: Deployment          # Deployment | StatefulSet | DaemonSet
    name: api-server          # optional; omit to match by selector
    # Option B: label selector (matches all matching workloads in namespace)
    selector:
      matchLabels:
        tier: api

  # Prometheus connection
  metricsSource:
    prometheus:
      address: http://prometheus.monitoring:9090
      # Optional: custom TLS and auth
      # tlsConfig:
      #   secretRef:
      #     name: prometheus-tls
      # bearerTokenSecretRef:
      #   name: prometheus-token
    # How far back to look for usage patterns
    historyWindow: 7d         # default: 7d, min: 1d, max: 30d
    # Minimum data points before making recommendations
    minimumDataPoints: 168    # default: 168 (7 days * 24 hours)

  # Per-resource configuration
  cpu:
    # Algorithm parameters
    percentile: 95            # default: 95, range: 50-99
    safetyMargin: 1.2         # default: 1.2 (20% headroom above percentile)
    # Hard bounds (REQUIRED -- never let recommendations go unbounded)
    bounds:
      min: "50m"              # required
      max: "4000m"            # required
    # Optional: control what is adjusted
    controlledValues: RequestsAndLimits  # RequestsOnly | RequestsAndLimits

  memory:
    percentile: 99            # default: 99 (more conservative for OOM risk)
    safetyMargin: 1.3         # default: 1.3 (30% headroom)
    bounds:
      min: "64Mi"             # required
      max: "8Gi"              # required
    controlledValues: RequestsAndLimits
    # Memory-specific safety
    allowDecrease: false      # default: false (OOM risk), set true only when confident

  # Rollout strategy
  updateStrategy:
    mode: Recommend           # Observe | Recommend | OneShot | Canary | Auto
    # mode-specific config (for Canary and Auto):
    canary:
      percentage: 10          # % of pods to resize first
      observationPeriod: 30m  # monitor canary pods for this long
    # Maximum change per reconciliation cycle
    maxCpuChangePercent: 50   # default: 50
    maxMemoryChangePercent: 30 # default: 30
    # Cooldown between resize cycles
    cooldown: 1h              # default: 1h, min: 5m
    # Automatic revert on OOMKill or excessive CPU throttle
    autoRevert: true          # default: true

  # Priority/weight for conflict resolution
  # When multiple policies match a workload, highest weight wins
  weight: 100                 # default: 100, range: 1-1000

status:
  # Standard conditions
  conditions:
    - type: Ready
      status: "True"
      reason: Monitoring
      message: "Watching 3 workloads, 12 pods"
      lastTransitionTime: "2026-01-15T10:30:00Z"
      observedGeneration: 2
    - type: Resizing
      status: "False"
      reason: Idle
      lastTransitionTime: "2026-01-15T10:30:00Z"
      observedGeneration: 2

  # Discovered workloads
  workloads:
    discovered: 3
    withRecommendations: 3
    resized: 2
    pending: 1

  # Recommendations summary
  recommendations:
    - workload: api-server
      kind: Deployment
      containers:
        - name: api
          current:
            cpu:
              request: "500m"
              limit: "1000m"
            memory:
              request: "512Mi"
              limit: "1Gi"
          recommended:
            cpu:
              request: "150m"
              limit: "300m"
            memory:
              request: "280Mi"
              limit: "560Mi"
          confidence: 0.92
          dataPoints: 1680
          lastUpdated: "2026-01-15T10:30:00Z"

  # Savings estimate
  savings:
    cpuRequestReduction: "1050m"    # total across all pods
    memoryRequestReduction: "696Mi"
    estimatedMonthlySavings: "$142.50"  # if costModel is configured

  # Resize history (last 10)
  resizeHistory:
    - timestamp: "2026-01-15T09:00:00Z"
      workload: api-server
      container: api
      resource: cpu
      from: "500m"
      to: "150m"
      method: InPlace
      result: Success
```

#### CRD Validation (CEL Rules)

```yaml
# Applied via +kubebuilder:validation:XValidation markers on the Go types

# cpu.bounds: min must be less than max
x-kubernetes-validations:
  - rule: "self.bounds.min <= self.bounds.max"
    message: "cpu.bounds.min must be less than or equal to cpu.bounds.max"

# memory.bounds: min must be less than max
  - rule: "self.bounds.min <= self.bounds.max"
    message: "memory.bounds.min must be less than or equal to memory.bounds.max"

# updateStrategy: canary config required when mode is Canary
  - rule: "self.updateStrategy.mode != 'Canary' || has(self.updateStrategy.canary)"
    message: "canary configuration is required when mode is Canary"

# weight: immutable after creation (prevents runtime priority races)
  - rule: "self.weight == oldSelf.weight"
    message: "weight cannot be changed after creation"

# historyWindow: reasonable bounds
  - rule: "self.metricsSource.historyWindow >= duration('24h')"
    message: "historyWindow must be at least 24 hours"
```

#### Printer Columns

```go
// +kubebuilder:printcolumn:name="Mode",type=string,JSONPath=`.spec.updateStrategy.mode`
// +kubebuilder:printcolumn:name="Workloads",type=integer,JSONPath=`.status.workloads.discovered`
// +kubebuilder:printcolumn:name="Resized",type=integer,JSONPath=`.status.workloads.resized`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:printcolumn:name="CPU Saved",type=string,JSONPath=`.status.savings.cpuRequestReduction`,priority=1
// +kubebuilder:printcolumn:name="Mem Saved",type=string,JSONPath=`.status.savings.memoryRequestReduction`,priority=1
```

```
$ kubectl get rightsizepolicies
NAME            MODE        WORKLOADS   RESIZED   READY   AGE
api-services    Canary      3           2         True    7d

$ kubectl get rightsizepolicies -o wide
NAME            MODE        WORKLOADS   RESIZED   READY   AGE   CPU SAVED   MEM SAVED
api-services    Canary      3           2         True    7d    1050m       696Mi
```

### 3.3 RightSizeDefaults (Cluster-Scoped, Optional)

Global defaults to avoid repetition across many RightSizePolicy resources.

```yaml
apiVersion: rightsize.io/v1alpha1
kind: RightSizeDefaults
metadata:
  name: default
spec:
  metricsSource:
    prometheus:
      address: http://prometheus.monitoring:9090
    historyWindow: 7d
    minimumDataPoints: 168
  cpu:
    percentile: 95
    safetyMargin: 1.2
    controlledValues: RequestsAndLimits
  memory:
    percentile: 99
    safetyMargin: 1.3
    controlledValues: RequestsAndLimits
    allowDecrease: false
  updateStrategy:
    mode: Recommend
    cooldown: 1h
    autoRevert: true
```

### 3.4 Status Conditions

| Condition Type | Reasons | Description |
|---------------|---------|-------------|
| `Ready` | `Monitoring`, `InsufficientData`, `PrometheusUnavailable`, `InvalidConfig` | Overall health |
| `Resizing` | `InProgress`, `Idle`, `CooldownActive` | Active resize operation |
| `Degraded` | `PartialFailure`, `HighRevertRate` | Some resizes failing |

Status conditions use `meta.SetStatusCondition()` from `k8s.io/apimachinery/pkg/api/meta`
(the Kyverno pattern) with `observedGeneration` on every condition.

---

## 4. Architecture

### 4.1 High-Level Components

```
┌──────────────────────────────────────────────────────────────────┐
│                        kube-rightsize                             │
│                                                                   │
│  ┌─────────────────────┐    ┌─────────────────────────┐         │
│  │  Policy Controller  │    │  Metrics Collector      │         │
│  │  ─────────────────  │    │  ───────────────────    │         │
│  │  Reconciles         │    │  Queries Prometheus     │         │
│  │  RightSizePolicy    │◄──►│  Aggregates usage data  │         │
│  │  CRs                │    │  Builds time-of-day     │         │
│  │  Discovers target   │    │  profiles               │         │
│  │  workloads          │    │  Detects bursts         │         │
│  └──────────┬──────────┘    └─────────────────────────┘         │
│             │                                                    │
│  ┌──────────▼──────────┐    ┌─────────────────────────┐         │
│  │  Recommender Engine │    │  Resize Engine          │         │
│  │  ─────────────────  │    │  ───────────────────    │         │
│  │  Composable         │    │  In-place via /resize   │         │
│  │  estimator chain:   │    │  subresource            │         │
│  │  percentile ->      │◄──►│  CPU first, then memory │         │
│  │  margin ->          │    │  Poll for completion    │         │
│  │  confidence ->      │    │  Timeout cascade:       │         │
│  │  bounds clamping    │    │  Deferred/Infeasible    │         │
│  └─────────────────────┘    └─────────────────────────┘         │
│                                                                   │
│  ┌─────────────────────┐    ┌─────────────────────────┐         │
│  │  Safety Monitor     │    │  Status Reporter        │         │
│  │  ─────────────────  │    │  ───────────────────    │         │
│  │  Watches OOMKills   │    │  Updates CRD status     │         │
│  │  Detects CPU        │    │  conditions             │         │
│  │  throttle           │◄──►│  Emits Prometheus       │         │
│  │  Tracks restarts    │    │  metrics                │         │
│  │  Auto-reverts       │    │  Sends notifications    │         │
│  │  Blocks bad resizes │    │  Records history        │         │
│  └─────────────────────┘    └─────────────────────────┘         │
│                                                                   │
└──────────────────────────────────────────────────────────────────┘
```

### 4.2 Controller Reconciliation Loop

A single controller reconciles `RightSizePolicy` resources. The reconcile function:

```
1. FETCH policy and resolve defaults from RightSizeDefaults
2. DISCOVER target workloads (by name or label selector)
3. For each workload:
   a. CHECK for conflicting policies (highest weight wins)
   b. QUERY Prometheus for historical usage data
   c. VALIDATE data sufficiency (minimum data points)
   d. COMPUTE recommendation via estimator chain
   e. COMPARE recommendation to current resources
   f. IF mode allows resize AND change exceeds threshold AND cooldown expired:
      i.  SELECT pods (all, canary %, or single)
      ii. RESIZE pods via /resize subresource (CPU first, then memory)
      iii. MONITOR resized pods for safety (OOM, throttle, restarts)
      iv. REVERT if safety checks fail
   g. UPDATE status (recommendations, savings, conditions, history)
4. REQUEUE after cooldown interval
```

### 4.3 Informer Configuration

| Resource | Cache | Purpose |
|----------|-------|---------|
| RightSizePolicy | Full | Primary reconciliation target |
| RightSizeDefaults | Full | Global defaults lookup |
| Deployment | Metadata-only | Discover target workloads, read replicas |
| StatefulSet | Metadata-only | Discover target workloads |
| DaemonSet | Metadata-only | Discover target workloads |
| Pod | Full | Read current resources, status, conditions |
| HorizontalPodAutoscaler | Metadata-only | Detect HPA conflicts |
| Event | None (use watch) | Detect OOMKill events |

### 4.4 RBAC Requirements

```yaml
# Pods: read + resize subresource
- apiGroups: [""]
  resources: ["pods"]
  verbs: ["get", "list", "watch"]
- apiGroups: [""]
  resources: ["pods/resize"]
  verbs: ["update", "patch"]

# Workload controllers: read-only
- apiGroups: ["apps"]
  resources: ["deployments", "statefulsets", "daemonsets", "replicasets"]
  verbs: ["get", "list", "watch"]

# Events: read (OOMKill detection) + create (operator events)
- apiGroups: ["events.k8s.io"]
  resources: ["events"]
  verbs: ["get", "list", "watch", "create", "patch"]

# HPA: read-only (conflict detection)
- apiGroups: ["autoscaling"]
  resources: ["horizontalpodautoscalers"]
  verbs: ["get", "list", "watch"]

# Own CRDs: full access
- apiGroups: ["rightsize.io"]
  resources: ["rightsizepolicies", "rightsizepolicies/status"]
  verbs: ["get", "list", "watch", "update", "patch"]
- apiGroups: ["rightsize.io"]
  resources: ["rightsizedefaults"]
  verbs: ["get", "list", "watch"]
```

---

## 5. Algorithm Design

### 5.1 Composable Estimator Chain

Inspired by VPA's decorator pattern, but with critical improvements:

```
Raw Prometheus Data
       │
       ▼
┌──────────────────┐
│ Percentile       │  Select P95 (CPU) or P99 (memory) from histogram
│ Estimator        │  Using configurable percentile per policy
└──────┬───────────┘
       │
       ▼
┌──────────────────┐
│ Safety Margin    │  Multiply by safetyMargin (default 1.2 CPU, 1.3 memory)
│ Estimator        │  Ensures headroom above observed usage
└──────┬───────────┘
       │
       ▼
┌──────────────────┐
│ Confidence       │  Widen recommendation when data is sparse:
│ Multiplier       │  result *= (1 + multiplier / confidence) ^ exponent
│                  │  confidence = min(days_of_data, sqrt(data_points/24))
└──────┬───────────┘
       │
       ▼
┌──────────────────┐
│ Bounds           │  Clamp to user-defined min/max
│ Clamper          │  Enforce QoS class preservation (requests <= limits)
└──────┬───────────┘
       │
       ▼
┌──────────────────┐
│ Change           │  Reject if change < threshold (prevent micro-adjustments)
│ Filter           │  Reject if change > maxChangePercent (prevent shocks)
└──────┬───────────┘
       │
       ▼
  Final Recommendation
```

Each estimator is an interface:

```go
type Estimator interface {
    Estimate(usage UsageProfile, current resource.Quantity) resource.Quantity
}
```

This makes each stage independently testable and composable.

### 5.2 Prometheus Queries

```promql
# CPU usage (rate of CPU seconds consumed)
rate(container_cpu_usage_seconds_total{
  namespace="$NAMESPACE",
  pod=~"$POD_PREFIX.*",
  container="$CONTAINER",
  container!=""
}[$STEP])

# Memory usage (working set, excludes cache)
container_memory_working_set_bytes{
  namespace="$NAMESPACE",
  pod=~"$POD_PREFIX.*",
  container="$CONTAINER",
  container!=""
}

# CPU throttling (detect under-provisioning)
rate(container_cpu_cfs_throttled_periods_total{...}[$STEP])
/ rate(container_cpu_cfs_periods_total{...}[$STEP])
```

### 5.3 Time-of-Day Awareness

Instead of a single histogram over the entire history window, build 24 hourly profiles
(optionally 168 for weekday/weekend distinction):

```go
type UsageProfile struct {
    // HourlyPercentiles[hour][percentile] = value
    // hour: 0-23, percentile: p50, p90, p95, p99, max
    HourlyPercentiles [24]PercentileSet

    // Overall (used when insufficient hourly data)
    OverallPercentiles PercentileSet

    // Burst detection
    BurstDetected      bool
    BurstMagnitude     float64  // peak / p95 ratio
    BurstDuration      time.Duration

    // Data quality
    DataPoints         int
    TimeSpanDays       float64
    Confidence         float64  // 0.0 - 1.0
}
```

The recommendation uses the **maximum** across all hourly profiles at the configured
percentile, ensuring the recommendation covers the busiest hour of the day.

### 5.4 HPA Coexistence

When an HPA targets the same Deployment on CPU:

1. kube-rightsize adjusts **requests** (the base resource allocation)
2. HPA adjusts **replica count** based on utilization percentage of requests
3. By right-sizing requests, HPA's percentage calculations become more accurate

To prevent conflicts:
- Detect HPA presence via informer
- If HPA targets CPU utilization, kube-rightsize adjusts CPU requests but NOT limits
  (preserving the request-to-limit ratio for HPA's calculations)
- If HPA targets custom metrics (not CPU/memory), no conflict exists
- Log a warning if both VPA and kube-rightsize target the same workload

---

## 6. Resize Engine

### 6.1 Resize Flow

```
1. SELECT target pods based on update strategy mode:
   - OneShot: all pods at once
   - Canary: canaryPercentage% of pods (round up to at least 1)
   - Auto: canary first, then remaining after observation period

2. For each selected pod:
   a. PRE-CHECK:
      - Pod is Running and Ready
      - Pod is not being deleted (DeletionTimestamp == nil)
      - Pod is not owned by kube-rightsize itself
      - No active resize in progress (PodResizeInProgress condition)
      - QoS class will be preserved after resize
      - New values satisfy LimitRange constraints
   b. RESIZE CPU (if needed):
      - Patch via /resize subresource
      - Poll status.containerStatuses[].resources until CPU matches
      - Timeout: 60 seconds
      - On failure: log, emit event, skip memory resize
   c. RESIZE MEMORY (if needed):
      - Patch via /resize subresource
      - Poll status.containerStatuses[].resources until memory matches
      - Timeout: 120 seconds (memory resize can be slower)
      - On Infeasible: record, do not retry until spec changes
      - On Deferred: record, retry on next reconciliation
   d. POST-CHECK:
      - Verify pod is still Running and Ready
      - Start safety observation window
```

### 6.2 client-go Resize Pattern

```go
func (r *ResizeEngine) ResizePod(ctx context.Context, pod *corev1.Pod,
    container string, target corev1.ResourceRequirements) error {

    updated := pod.DeepCopy()
    for i := range updated.Spec.Containers {
        if updated.Spec.Containers[i].Name == container {
            updated.Spec.Containers[i].Resources = target
            break
        }
    }

    _, err := r.clientset.CoreV1().Pods(pod.Namespace).UpdateResize(
        ctx, pod.Name, updated, metav1.UpdateOptions{},
    )
    return err
}
```

### 6.3 Resize Status Polling

```go
func (r *ResizeEngine) WaitForResize(ctx context.Context, ns, podName,
    container string, target corev1.ResourceRequirements, timeout time.Duration) error {

    ctx, cancel := context.WithTimeout(ctx, timeout)
    defer cancel()

    return wait.PollUntilContextCancel(ctx, 3*time.Second, true,
        func(ctx context.Context) (done bool, err error) {
            pod, err := r.clientset.CoreV1().Pods(ns).Get(ctx, podName, metav1.GetOptions{})
            if err != nil {
                return false, err
            }

            // Check for Infeasible (permanent failure)
            for _, cond := range pod.Status.Conditions {
                if string(cond.Type) == "PodResizePending" &&
                    cond.Status == corev1.ConditionTrue &&
                    cond.Reason == "Infeasible" {
                    return false, fmt.Errorf("resize infeasible: %s", cond.Message)
                }
            }

            // Check if actual resources match target
            for _, cs := range pod.Status.ContainerStatuses {
                if cs.Name == container && cs.Resources != nil {
                    if quantitiesMatch(cs.Resources, target) {
                        return true, nil
                    }
                }
            }
            return false, nil
        })
}
```

### 6.4 Edge Cases

| Scenario | Handling |
|----------|---------|
| Pod deleted during resize | Ignore; new pod from Deployment will use original template |
| Node has insufficient resources | Resize marked Deferred; retry on next reconciliation |
| QoS class would change | Pre-check rejects the resize |
| LimitRange violation | API server rejects; log and skip |
| ResourceQuota exceeded | API server rejects; log and skip |
| Static CPU/Memory Manager | Infeasible for Guaranteed QoS pods; skip with warning |
| Multiple containers in pod | Resize each independently; any failure skips remaining |
| VPA also targeting workload | Detect via VPA informer; log conflict warning, defer to VPA |

---

## 7. Safety System

### 7.1 Graduated Rollout Modes

| Mode | Behavior | Risk Level |
|------|----------|------------|
| `Observe` | Collect metrics, build profiles, no output | None |
| `Recommend` | Generate recommendations in status, no changes | None |
| `OneShot` | Resize one pod, monitor, stop | Low |
| `Canary` | Resize canary%, monitor, then remaining | Medium |
| `Auto` | Full automated canary-then-fleet | Medium-High |

### 7.2 Auto-Revert

When `autoRevert: true` (default), the Safety Monitor watches resized pods for:

1. **OOMKilled**: Container terminated with reason OOMKilled within observation period
2. **CPU Throttle**: CPU throttle ratio exceeds 50% (configurable) post-resize
3. **Excessive Restarts**: Container restart count increases by 2+ post-resize
4. **Pod Not Ready**: Pod becomes NotReady within observation period

On trigger:
1. Restore original resources via `/resize` subresource
2. Emit Kubernetes event on the Pod
3. Update RightSizePolicy status with revert reason
4. Increment revert counter
5. Apply exponential backoff before retrying that workload (2x cooldown per revert)

### 7.3 Conflict Detection

Before any resize:
- Check for existing VPA targeting the same workload
- Check for existing HPA (adjust behavior, don't block)
- Check for other RightSizePolicy with higher weight
- Check for `rightsize.io/skip: "true"` annotation on workload (opt-out)
- Check for active rollout on the parent Deployment (don't resize during rollouts)

---

## 8. Metrics & Observability

### 8.1 Prometheus Metrics Exposed

```
# Recommendation gauge (per workload, container, resource)
kube_rightsize_recommendation_cpu_cores{namespace, workload, container}
kube_rightsize_recommendation_memory_bytes{namespace, workload, container}

# Current vs recommended delta
kube_rightsize_savings_cpu_cores_total{namespace}
kube_rightsize_savings_memory_bytes_total{namespace}

# Resize operations
kube_rightsize_resize_total{namespace, workload, resource, result}  # result: success|failure|reverted
kube_rightsize_resize_duration_seconds{namespace, workload, resource}

# Safety
kube_rightsize_reverts_total{namespace, workload, reason}  # reason: oomkill|throttle|restart|notready
kube_rightsize_confidence{namespace, workload, container}

# Operator health
kube_rightsize_reconcile_duration_seconds{controller}
kube_rightsize_reconcile_errors_total{controller}
kube_rightsize_prometheus_query_duration_seconds{}
kube_rightsize_prometheus_query_errors_total{}
```

### 8.2 Kubernetes Events

| Event | Type | Reason | Message Example |
|-------|------|--------|-----------------|
| Recommendation generated | Normal | RecommendationReady | "Recommending cpu=150m (was 500m) for api-server/api" |
| Resize initiated | Normal | ResizeStarted | "Resizing pod api-server-abc12 cpu: 500m -> 150m (in-place)" |
| Resize succeeded | Normal | ResizeSucceeded | "Pod api-server-abc12 resized successfully" |
| Resize failed | Warning | ResizeFailed | "Pod api-server-abc12 resize failed: Infeasible" |
| Auto-revert triggered | Warning | ResizeReverted | "Reverted api-server-abc12: OOMKilled after resize" |
| Conflict detected | Warning | ConflictDetected | "VPA my-vpa also targets Deployment api-server" |
| Insufficient data | Normal | InsufficientData | "Only 48/168 data points for api-server; waiting" |

### 8.3 Grafana Dashboard

Ship a pre-built Grafana dashboard JSON covering:
- Savings overview (CPU/memory saved across cluster)
- Per-namespace breakdown
- Recommendation vs. actual usage over time
- Resize success/failure rates
- Revert rate and reasons
- Confidence scores
- Prometheus query latency

---

## 9. Testing Strategy

### 9.1 Test Pyramid

```
                    ┌───────────┐
                    │   E2E     │  Chainsaw: real cluster, full lifecycle
                    │   Tests   │  ~10 scenarios
                    ├───────────┤
                    │Integration│  envtest: real API server + etcd
                    │   Tests   │  Controller reconciliation, CRD validation
                    │           │  ~50 test cases
                    ├───────────┤
                    │   Unit    │  Standard Go testing + testify
                    │   Tests   │  Algorithm, estimators, resize logic
                    │           │  ~200+ test cases
                    └───────────┘
```

### 9.2 Unit Tests

**Framework**: Standard `testing` + `github.com/stretchr/testify`

**What to unit test** (table-driven tests):
- Each estimator in the chain (percentile, margin, confidence, bounds, change filter)
- UsageProfile construction from Prometheus data
- Time-of-day profile aggregation
- Burst detection algorithm
- Confidence calculation
- QoS class preservation check
- HPA conflict detection logic
- Resource quantity arithmetic (CPU millicore, memory byte conversions)
- Resize patch construction
- Status condition building

**Coverage target**: 80%+ on `internal/` packages.

### 9.3 Integration Tests (envtest)

**Framework**: Ginkgo v2 + Gomega + `controller-runtime/pkg/envtest`

**What to test**:
- RightSizePolicy CR creation, validation, defaulting
- RightSizeDefaults merging with policy-level overrides
- Controller discovers workloads by name and by selector
- Controller handles workload updates (new pods, scale events)
- Controller resolves policy conflicts (highest weight wins)
- Status conditions are set correctly
- Status recommendations are populated
- CRD CEL validation rules reject invalid inputs
- Printer columns render correctly
- Finalizer cleanup on policy deletion

**Test setup**:
```go
var _ = BeforeSuite(func() {
    testEnv = &envtest.Environment{
        CRDDirectoryPaths: []string{
            filepath.Join("..", "..", "config", "crd", "bases"),
        },
    }
    cfg, err := testEnv.Start()
    Expect(err).NotTo(HaveOccurred())
    // ... setup manager, controllers
})
```

**Key pattern**: Use a **non-cached client** for assertions to avoid stale reads:
```go
// Bad: uses cached client, may see stale data
Expect(k8sClient.Get(ctx, key, &policy)).To(Succeed())

// Good: use a separate non-cached client for assertions
directClient, _ := client.New(cfg, client.Options{})
Eventually(func(g Gomega) {
    g.Expect(directClient.Get(ctx, key, &policy)).To(Succeed())
    g.Expect(policy.Status.Workloads.Discovered).To(Equal(3))
}).Should(Succeed())
```

### 9.4 E2E Tests (Chainsaw)

**Framework**: Kyverno Chainsaw v0.2.15

**Test scenarios**:

| # | Scenario | What It Validates |
|---|----------|-------------------|
| 1 | Install operator via Helm | Deployment runs, CRDs registered |
| 2 | Create RightSizePolicy in Recommend mode | Recommendations appear in status |
| 3 | Create RightSizePolicy in OneShot mode | Single pod resized, status updated |
| 4 | Canary rollout | canary% pods resized first |
| 5 | Auto-revert on OOMKill | Resize reverted after simulated OOM |
| 6 | HPA coexistence | No conflict, both operate correctly |
| 7 | Policy conflict resolution | Highest weight policy wins |
| 8 | Opt-out annotation | Workload with skip annotation is ignored |
| 9 | Insufficient data | Policy reports InsufficientData condition |
| 10 | Upgrade operator version | CRDs migrated, no downtime |

**Test cluster**: Kind cluster created in CI with metrics-server and a mock Prometheus.

### 9.5 Fuzz Tests

**Framework**: Go native fuzzing (`go test -fuzz`)

**What to fuzz**:
- CRD validation functions (malformed resource quantities, empty strings, boundary values)
- Prometheus query response parsing (malformed JSON, NaN values, empty vectors)
- Estimator chain with extreme inputs (zero usage, max int64, negative values)
- Resize patch construction with edge-case resource values

```go
func FuzzEstimatorChain(f *testing.F) {
    f.Add(float64(0.1), float64(1.0), 95, 1.2)
    f.Fuzz(func(t *testing.T, usage, current float64, percentile int, margin float64) {
        if percentile < 50 || percentile > 99 || margin < 1.0 || margin > 5.0 {
            t.Skip()
        }
        // Ensure estimator never panics, always returns positive value
        result := chain.Estimate(usage, current, percentile, margin)
        if result.IsZero() || result.Cmp(resource.Quantity{}) < 0 {
            t.Errorf("estimator returned non-positive: %v", result)
        }
    })
}
```

### 9.6 Benchmark Tests

**Framework**: Standard Go benchmarks (`testing.B`)

**What to benchmark**:
- Prometheus response parsing (1K, 10K, 100K data points)
- Percentile calculation on large datasets
- Estimator chain execution
- Resize patch construction
- Status update serialization

```go
func BenchmarkPercentileCalculation(b *testing.B) {
    data := generateSamples(100000)
    b.ResetTimer()
    for b.Loop() {
        calculatePercentile(data, 95)
    }
}
```

### 9.7 Conformance Tests

Validate compatibility with Kubernetes API conventions:
- CRD structural schema validation passes `kubectl apply --dry-run=server`
- Status subresource works correctly
- Printer columns render
- Short names work (`kubectl get rsp`)
- Scale subresource (if applicable)

---

## 10. CI/CD Pipeline

### 10.1 GitHub Actions Workflows

#### `ci.yaml` - Continuous Integration (on every PR and push to main)

```
Jobs:
  lint:
    - golangci-lint v2.12.x (with .golangci.yml config)
    - yamllint (CRD manifests, Helm chart)
    - actionlint (validate workflow files)
    - shellcheck (bash scripts)

  test-unit:
    - go test ./internal/... ./api/... -race -count=1 -coverprofile=coverage.out
    - Upload coverage to Codecov
    - Fail if coverage < 80%

  test-integration:
    - Setup envtest binaries (setup-envtest)
    - go test ./test/integration/... -race -count=1 -tags=integration
    - Timeout: 15 minutes

  test-e2e:
    - Create Kind cluster (kubernetes version matrix: 1.35, 1.36)
    - Install CRDs + operator
    - Run Chainsaw tests
    - Upload test results as artifacts
    - Timeout: 30 minutes

  crd-freshness:
    - Run `make manifests`
    - `git diff --exit-code config/crd/`
    - Fails if generated CRDs are stale

  helm-lint:
    - helm lint charts/kube-rightsize
    - ct lint --config .ct.yaml
    - helm-unittest

  build:
    - ko build --platform=linux/amd64,linux/arm64 (no push)
    - Verify image builds successfully

  security:
    - trivy fs . --severity HIGH,CRITICAL
    - trivy image (from build step)
    - go vuln check (govulncheck)
```

#### `release.yaml` - Release (on tag push v*)

```
Jobs:
  release:
    - goreleaser release
    - ko build + push to ghcr.io
    - cosign sign image by digest
    - trivy scan released image
    - Generate SBOM (syft)
    - Attach to GitHub Release: binary, CRD YAML, Helm chart, SBOM, checksums

  helm-release:
    - helm package charts/kube-rightsize
    - helm push to OCI registry (ghcr.io)
    - cosign sign Helm chart
    - Update Helm chart index
```

#### `security.yaml` - Security Scanning (scheduled nightly + on PR)

```
Jobs:
  codeql:
    - GitHub CodeQL analysis (Go)
  
  govulncheck:
    - govulncheck ./...
  
  trivy-repo:
    - trivy fs . --severity HIGH,CRITICAL --exit-code 1

  dependency-review:
    - actions/dependency-review-action (on PR only)
```

#### `docs.yaml` - Documentation (on push to main when docs/ changes)

```
Jobs:
  build-docs:
    - mkdocs build --strict
    - crdoc --resources config/crd/bases --output docs/reference/api.md
    - Deploy to GitHub Pages
```

### 10.2 CI Configuration Files

**.golangci.yml** (key linters):

```yaml
version: "2"
linters:
  enable:
    - importas       # Enforce corev1, metav1 aliases
    - forbidigo      # Ban fmt.Printf, context.Background() in controllers
    - ginkgolinter   # Catch Ginkgo/Gomega anti-patterns
    - errorlint      # errors.Is/errors.As enforcement
    - revive         # Style
    - staticcheck    # Advanced analysis
    - bodyclose      # HTTP response body leak prevention
    - nilerr         # Nil error return detection
    - govet          # Vet checks
    - unused         # Dead code
    - gosec          # Security
  settings:
    importas:
      alias:
        - pkg: k8s.io/api/core/v1
          alias: corev1
        - pkg: k8s.io/apimachinery/pkg/apis/meta/v1
          alias: metav1
        - pkg: k8s.io/apimachinery/pkg/api/errors
          alias: apierrors
    forbidigo:
      forbid:
        - pattern: ^fmt\.Print
          msg: "Use structured logging (slog or logr)"
        - pattern: ^context\.Background
          msg: "Use the context passed to Reconcile"
```

### 10.3 Branch Protection

```
main branch:
  - Require PR reviews (1 reviewer)
  - Require status checks: lint, test-unit, test-integration, crd-freshness, helm-lint, build
  - Require up-to-date branches
  - No force push
  - No deletion
```

---

## 11. Distribution

### 11.1 Helm Chart

**Primary installation method.** Structure:

```
charts/kube-rightsize/
├── Chart.yaml
├── values.yaml
├── values.schema.json
├── README.md              # Auto-generated by helm-docs
├── templates/
│   ├── _helpers.tpl
│   ├── deployment.yaml
│   ├── serviceaccount.yaml
│   ├── clusterrole.yaml
│   ├── clusterrolebinding.yaml
│   ├── service.yaml         # Webhook service
│   ├── certificate.yaml     # Webhook TLS (cert-manager or self-signed)
│   └── tests/
│       └── test-connection.yaml
└── ci/
    ├── default-values.yaml
    ├── ha-values.yaml
    └── minimal-values.yaml
```

**Key values.yaml fields**:
- `replicaCount` (default: 1, HA: 2 with leader election)
- `image.repository`, `image.tag`
- `resources` (operator pod resources)
- `prometheus.address` (default Prometheus URL)
- `metrics.enabled` (expose /metrics)
- `securityContext` (non-root, read-only root filesystem, drop all capabilities)

### 11.2 OCI Registry

```bash
# Push Helm chart
helm push kube-rightsize-0.1.0.tgz oci://ghcr.io/sebtardif/charts

# Install from OCI
helm install kube-rightsize oci://ghcr.io/sebtardif/charts/kube-rightsize --version 0.1.0
```

### 11.3 kubectl Plugin (Future)

Distributed via krew:

```bash
kubectl krew install rightsize

kubectl rightsize status -n production
kubectl rightsize savings
kubectl rightsize recommendations -n production
```

### 11.4 Raw Manifests

For users who don't use Helm:

```bash
kubectl apply -f https://github.com/SebTardif/kube-rightsize/releases/latest/download/install.yaml
```

---

## 12. Documentation

### 12.1 Documentation Site

**Framework**: MkDocs + Material for MkDocs

**Structure**:

```
docs/
├── index.md                    # Overview, elevator pitch
├── getting-started/
│   ├── installation.md         # Helm, raw manifests, prerequisites
│   ├── quickstart.md           # 5-minute first policy
│   └── concepts.md             # CRDs, modes, algorithm overview
├── guides/
│   ├── recommend-mode.md       # Safe first step
│   ├── canary-rollout.md       # Production right-sizing
│   ├── hpa-coexistence.md      # Using with HPA
│   ├── gitops-integration.md   # Flux, ArgoCD compatibility
│   ├── migrating-from-vpa.md   # Step-by-step VPA replacement
│   └── troubleshooting.md      # Common issues, debug steps
├── reference/
│   ├── api.md                  # Auto-generated CRD reference
│   ├── metrics.md              # Prometheus metrics reference
│   ├── configuration.md        # Helm values reference
│   └── cli.md                  # kubectl plugin reference
├── architecture/
│   ├── design.md               # Architecture overview
│   ├── algorithm.md            # Estimator chain details
│   ├── safety.md               # Safety system design
│   └── resize-api.md           # K8s In-Place Resize reference
└── contributing/
    ├── development.md          # Local dev setup
    ├── testing.md              # Running tests
    └── releasing.md            # Release process
```

### 12.2 README.md

Must include:
- One-sentence description
- Architecture diagram
- 5-minute quickstart
- Feature comparison table (vs VPA, Goldilocks)
- CRD example
- Link to docs site
- Badges (CI, Go version, License, CNCF if applicable)
- ADOPTERS.md link

### 12.3 ADOPTERS.md

Create from day one (even if empty). CloudNativePG's format:

```markdown
# Adopters

If you are using kube-rightsize in your organization, please add your
company to this list. It helps the project understand its user base
and prioritize features.

| Organization | Contact | Date | Description |
|-------------|---------|------|-------------|
```

---

## 13. Project Structure

```
kube-rightsize/
├── .github/
│   ├── workflows/
│   │   ├── ci.yaml
│   │   ├── release.yaml
│   │   ├── security.yaml
│   │   └── docs.yaml
│   ├── ISSUE_TEMPLATE/
│   │   ├── bug_report.md
│   │   └── feature_request.md
│   ├── PULL_REQUEST_TEMPLATE.md
│   └── dependabot.yml
├── api/
│   └── v1alpha1/
│       ├── groupversion_info.go
│       ├── rightsizepolicy_types.go
│       ├── rightsizepolicy_types_test.go
│       ├── rightsizedefaults_types.go
│       ├── conditions.go
│       ├── zz_generated.deepcopy.go
│       └── doc.go
├── cmd/
│   └── manager/
│       └── main.go
├── internal/
│   ├── controller/
│   │   ├── rightsizepolicy_controller.go
│   │   ├── rightsizepolicy_controller_test.go
│   │   └── suite_test.go
│   ├── metrics/
│   │   ├── collector.go         # Prometheus query client
│   │   ├── collector_test.go
│   │   ├── profile.go           # UsageProfile construction
│   │   └── profile_test.go
│   ├── recommendation/
│   │   ├── estimator.go         # Estimator interface
│   │   ├── percentile.go        # Percentile estimator
│   │   ├── percentile_test.go
│   │   ├── margin.go            # Safety margin estimator
│   │   ├── margin_test.go
│   │   ├── confidence.go        # Confidence multiplier
│   │   ├── confidence_test.go
│   │   ├── bounds.go            # Bounds clamper
│   │   ├── bounds_test.go
│   │   ├── chain.go             # Composable chain
│   │   ├── chain_test.go
│   │   └── fuzz_test.go
│   ├── resize/
│   │   ├── engine.go            # Pod resize via /resize
│   │   ├── engine_test.go
│   │   ├── status.go            # Resize status polling
│   │   └── status_test.go
│   ├── safety/
│   │   ├── monitor.go           # OOMKill, throttle, restart detection
│   │   ├── monitor_test.go
│   │   ├── revert.go            # Auto-revert logic
│   │   └── revert_test.go
│   ├── conflict/
│   │   ├── detector.go          # VPA, HPA, policy conflict detection
│   │   └── detector_test.go
│   └── webhook/
│       ├── defaulting.go        # Defaulting webhook
│       └── validation.go        # Validation webhook (for complex rules)
├── config/
│   ├── crd/
│   │   └── bases/               # Generated CRD manifests
│   ├── rbac/
│   │   ├── role.yaml
│   │   └── role_binding.yaml
│   ├── manager/
│   │   └── manager.yaml
│   ├── webhook/
│   └── samples/
│       ├── recommend-mode.yaml
│       ├── canary-mode.yaml
│       └── defaults.yaml
├── charts/
│   └── kube-rightsize/
│       ├── Chart.yaml
│       ├── values.yaml
│       ├── values.schema.json
│       └── templates/
├── test/
│   ├── e2e/                     # Chainsaw test cases
│   │   ├── install/
│   │   ├── recommend-mode/
│   │   ├── canary-rollout/
│   │   ├── auto-revert/
│   │   └── hpa-coexistence/
│   └── integration/             # envtest-based tests
├── docs/                        # MkDocs site
├── hack/                        # Development scripts
│   ├── setup-envtest.sh
│   └── update-codegen.sh
├── .golangci.yml
├── .goreleaser.yaml
├── .ko.yaml
├── Makefile
├── Dockerfile                   # Fallback (ko is primary)
├── go.mod
├── go.sum
├── LICENSE                      # Apache 2.0
├── README.md
├── ADOPTERS.md
├── CONTRIBUTING.md
├── CHANGELOG.md
└── SECURITY.md
```

---

## 14. Roadmap

### Phase 1: Foundation (MVP)

- [ ] Project scaffolding (Kubebuilder)
- [ ] RightSizePolicy CRD (v1alpha1)
- [ ] Prometheus metrics collector
- [ ] Percentile-based recommendation engine
- [ ] Status reporting (recommendations, conditions)
- [ ] Observe and Recommend modes only (no resize)
- [ ] Helm chart
- [ ] Unit tests (80%+ coverage)
- [ ] envtest integration tests
- [ ] CI pipeline (lint, test, build)
- [ ] README with quickstart

### Phase 2: Resize Engine

- [ ] In-place resize via /resize subresource
- [ ] OneShot mode
- [ ] Canary mode with graduated rollout
- [ ] Resize status polling and timeout handling
- [ ] QoS preservation checks
- [ ] LimitRange/ResourceQuota compatibility
- [ ] E2E tests (Chainsaw)
- [ ] Security scanning in CI

### Phase 3: Safety & Intelligence

- [ ] Safety monitor (OOMKill, throttle, restart detection)
- [ ] Auto-revert mechanism
- [ ] Confidence-based recommendation widening
- [ ] Time-of-day-aware algorithm
- [ ] Burst detection
- [ ] HPA coexistence logic
- [ ] VPA conflict detection
- [ ] Policy weight-based conflict resolution

### Phase 4: Production Readiness

- [ ] Auto mode (canary then fleet)
- [ ] RightSizeDefaults (cluster-scoped)
- [ ] Grafana dashboard
- [ ] MkDocs documentation site
- [ ] Cosign image signing
- [ ] SBOM generation
- [ ] Release automation (GoReleaser)
- [ ] OCI Helm chart distribution
- [ ] Fuzz tests
- [ ] Benchmark tests

### Phase 5: Ecosystem

- [ ] kubectl plugin (via krew)
- [ ] Datadog/CloudWatch metrics support
- [ ] Memory decrease support (with gradual decrease)
- [ ] Multi-cluster aggregated reporting
- [ ] CNCF Sandbox application
- [ ] KubeCon talk proposal
- [ ] ADOPTERS.md with real organizations

---

## 15. Competitor Lessons

### Patterns Adopted

| Pattern | Source | How We Use It |
|---------|--------|---------------|
| Mandatory resource bounds | OptiPod | `bounds.min`/`bounds.max` are required fields |
| Weight-based policy resolution | OptiPod | `weight` field for deterministic conflict resolution |
| Gradual memory decrease | OptiPod | `maxMemoryChangePercent` + `allowDecrease` flag |
| Composable estimator chain | VPA | Decorator pattern: percentile -> margin -> confidence -> bounds |
| Confidence-based widening | VPA | `(1 + multiplier/confidence)^exponent` formula |
| Two-phase resize (CPU then memory) | right-sizer | CPU first (safer), then memory, with proper polling |
| Conditions via meta.SetStatusCondition | Kyverno | Standard library helper, not hand-rolled |
| Print columns with priority | Kyverno | `-o wide` shows savings columns |
| Strict CI shell defaults | CloudNativePG | `bash -Eeuo pipefail -x {0}` in all workflows |
| ADOPTERS.md from day one | CloudNativePG | Social proof drives adoption |
| envtest + property-based testing | OptiPod | Fast feedback + invariant testing |

### Anti-Patterns Avoided

| Anti-Pattern | Source | Why We Avoid It |
|-------------|--------|-----------------|
| Bloated CRD (15+ config sections) | right-sizer | Focused CRD + separate defaults CRD |
| Emoji logging / fmt.Printf | right-sizer, OptiPod | Structured logging only (logr) |
| Hardcoded time.Sleep between operations | right-sizer | Proper polling via wait.PollUntilContextCancel |
| No CRD (annotation-only) | kube-reqsizer | Full CRD with proper status |
| Manual memory string parsing | kube-reqsizer | Always use resource.Quantity |
| Status Phase as bare string | right-sizer | Typed constants with kubebuilder enum validation |
| ObservedGeneration via annotations | right-sizer | Proper status subresource field |
| All containers resized together | VPA | Per-container independent resize |
| HPA conflict undefined | VPA | Detect and handle HPA coexistence |
