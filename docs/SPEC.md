# attune: Complete Specification

> Safe, in-place Kubernetes pod resource right-sizing operator.
> VPA done right, powered by In-Place Pod Resize (K8s 1.32+).

---

## Table of Contents

1. [Vision & Goals](#vision--goals)
2. [Technology Decisions](#technology-decisions)
3. [CRD Design](#crd-design)
4. [Architecture](#architecture)
5. [Algorithm Design](#algorithm-design)
6. [Resize Engine](#resize-engine)
7. [Safety System](#safety-system)
8. [Metrics & Observability](#metrics--observability)
9. [Testing Strategy](#testing-strategy)
10. [CI/CD Pipeline](#cicd-pipeline)
11. [Distribution](#distribution)
12. [Documentation](#documentation)
13. [Project Structure](#project-structure)
14. [Roadmap](#roadmap)
15. [Competitor Lessons](#competitor-lessons)

---

<a id="vision--goals"></a>
## 1. Vision & Goals

### Problem

99.94% of Kubernetes clusters are over-provisioned. Average CPU utilization is 8%, memory 20%
([CAST AI 2026](https://cast.ai/reports/state-of-kubernetes-optimization/)). VPA, the tool designed to fix this, is universally feared: fewer than 1% of
organizations run it in production ([ScaleOps 2026](https://scaleops.com/blog/why-pod-rightsizing-fails-in-production-a-deep-dive-into-vpa-and-what-actually-works/)).
VPA historically evicts pods (newer modes can attempt in-place where supported),
conflicts with HPA on the same metrics, and has caused cluster-wide outages.

In December 2025, In-Place Pod Resize graduated to **GA** in Kubernetes 1.35
([KEP-1287](https://github.com/kubernetes/enhancements/tree/master/keps/sig-node/1287-in-place-update-pod-resources);
alpha since 1.27, feature-gated through 1.32; beta and enabled by default in
1.33–1.34). CPU and memory can be changed on running pods without restarts.
That unlocks a ground-up redesign of resource right-sizing.

### Mission

Attune is the first production-grade right-sizing operator built exclusively for
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

<a id="technology-decisions"></a>
## 2. Technology Decisions

### 2.1 Language: Go 1.27.1

| Factor | Decision |
|--------|----------|
| Language | Go 1.27.1 |
| Module directive | `go 1.27.1` |
| Rationale | 85%+ of production K8s operators use Go. Largest ecosystem, hiring pool, and controller-runtime support. Green Tea GC (1.26) provides lower latency. |
| What competitors use | right-sizer: Go 1.25, OptiPod: Go 1.24.6, VPA: Go |
| What model operators use | CloudNativePG: Go 1.26.3, Kyverno: Go 1.26.2 |

### 2.2 Framework: Kubebuilder v4 + controller-runtime v0.25.2

| Component | Version | Purpose |
|-----------|---------|---------|
| Kubebuilder | v4.14.0 | Project scaffolding, Makefile, CRD generation |
| controller-runtime | v0.25.2 | Controller lifecycle, reconciliation, caching, webhooks |
| client-go | v0.37.1 | K8s API access, `/resize` subresource calls |
| k8s.io/api | v0.37.1 | K8s type definitions |
| k8s.io/apimachinery | v0.37.1 | Resource quantities, conditions, meta types |

**Why Kubebuilder over Operator SDK**: For a new operator without OLM/OperatorHub requirements,
Kubebuilder provides the cleanest scaffolding. Operator SDK adds OLM bundle generation on top
of the same controller-runtime foundation. We can add Operator SDK later for OperatorHub
distribution.

**Why controller-runtime v0.25.2**: PriorityQueue (default since v0.23.0) enables prioritizing
resize reconciliations for critical pods. Subresource Apply support enables clean SSA patches
to the `/resize` subresource. Generic Validator/Defaulter webhooks provide type-safe CRD
validation.

### 2.3 Prometheus Querying

| Component | Module | Version |
|-----------|--------|---------|
| Query client | `github.com/prometheus/client_golang/api/prometheus/v1` | v1.24.1 |
| Result types | `github.com/prometheus/common/model` | transitive |

The official Prometheus Go client for querying (not exposing metrics). Returns typed results
(`model.Vector`, `model.Matrix`). Supports auth via custom `http.RoundTripper`.

### 2.4 Complete Dependency Table

```
go 1.27.1

# Core
sigs.k8s.io/controller-runtime          v0.25.2
k8s.io/client-go                        v0.37.1
k8s.io/api                              v0.37.1
k8s.io/apimachinery                     v0.37.1

# Prometheus querying
github.com/prometheus/client_golang     v1.24.1

# Testing
github.com/onsi/ginkgo/v2              latest
github.com/onsi/gomega                  latest
github.com/stretchr/testify             latest

# Tools (CI/build, not Go module deps)
kubebuilder                             v4.14.0
golangci-lint                           v2.13.x
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

<a id="crd-design"></a>
## 3. CRD Design

### 3.1 API Group and Version

```
Group:   attune.io
Version: v1alpha1
```

### 3.2 AttunePolicy (Namespaced)

The primary CRD. Defines a right-sizing policy for a set of workloads.

```yaml
apiVersion: attune.io/v1alpha1
kind: AttunePolicy
metadata:
  name: api-services
  namespace: production
spec:
  # Which workloads to target
  targetRef:
    # Option A: specific workload
    kind: Deployment          # Deployment | StatefulSet | DaemonSet | CronJob | Job | ReplicaSet | Rollout
    name: api-server          # optional; omit to match by selector
    # Option B: label selector (matches all matching workloads in namespace)
    selector:
      matchLabels:
        tier: api

  # Prometheus connection
  metricsSource:
    prometheus:
      address: http://prometheus-server.monitoring:80
      headers:
        X-Scope-OrgID: tenant-a
      queryParameters:
        dedup: "true"
      # Optional: auth and TLS settings. On AttunePolicy /
      # AttuneNamespaceDefaults the Secret is in that namespace.
      # Do not use bearerTokenSecret on cluster AttuneDefaults for a
      # shared token (deprecated: name is still read in each policy
      # namespace). Cluster-wide auth is the operator SA token or a
      # Secret in the operator namespace (prometheusAuth).
      # Datadog apiKeySecretRef on cluster AttuneDefaults has the same
      # shape: use datadogAuth.existingSecret for a cluster-wide API key.
      bearerTokenSecret:
        name: prometheus-token
        key: token
      # Amazon Managed Prometheus replaces bearerTokenSecret with sigv4.
      # Do not set both. The address is the workspace root. Attune appends
      # /api/v1/query. region is required. roleArn is optional.
      # sigv4:
      #   region: us-east-1
      #   roleArn: arn:aws:iam::123456789012:role/attune-amp
      tls:
        insecureSkipVerify: false
    # How far back to look for usage patterns
    historyWindow: 168h       # default: 168h (7d), min: 1h, max: 720h
    # Minimum Prometheus range-query samples before making recommendations
    minimumDataPoints: 48     # default: 48 (~4h at the default queryStep: 5m)
    queryStep: 5m             # default: 5m, min: 10s, max: 1h
    rateWindow: 5m            # default: queryStep, min: 30s, max: historyWindow

  # Per-resource configuration
  cpu:
    # Algorithm parameters
    percentile: 95            # supported: 50, 90, 95, 99
    overhead: "20"       # default: 20 (20% headroom above percentile)
    # Optional hard bounds
    minAllowed: "50m"
    maxAllowed: "4000m"
    # Optional: control what is adjusted
    controlledValues: RequestsAndLimits  # RequestsOnly | RequestsAndLimits
    # limitMultiplier stays off until set. Omitted keeps the live ratio.
    # limitMultiplier: "2"
    # Maximum change per reconciliation cycle
    maxChangePercent: 50      # default: 50
    # startupBoost:          # optional CPU cold-start multiplier
    #   multiplier: "2.0"
    #   duration: 2m
    #   # excludeFromHistory: true drops CPU samples from
    #   # attune.io/startup-boost-at when that stamp is set and not
    #   # before creation, otherwise from CreationTimestamp, until that
    #   # start plus duration plus rateWindow. Omitted keeps today's
    #   # percentile. Deleted pods stay until historyWindow.
    #   # A recreated name keeps samples older than the new CreationTimestamp.

  memory:
    percentile: 99            # supported: 50, 90, 95, 99
    overhead: "30"       # default: 30 (30% headroom)
    minAllowed: "64Mi"
    maxAllowed: "8Gi"
    controlledValues: RequestsAndLimits
    # limitMultiplier stays off until set. Omitted keeps the live ratio.
    # limitMultiplier: "1.5"
    # Scaled memory limits are rounded up to a whole byte.
    # Memory-specific safety
    allowDecrease: false      # default: false (OOM risk), set true only when confident
    # Maximum change per reconciliation cycle
    maxChangePercent: 30      # default: 30

  # Optional per-container overrides. Omitted or empty keeps the shared
  # cpu and memory engines. Not on AttuneDefaults or AttuneNamespaceDefaults.
  # containerPolicies:
  #   - containerName: "*"
  #     cpu:
  #       maxAllowed: "300m"
  #   - containerName: sidecar
  #     cpu:
  #       maxAllowed: "200m"

  # Rollout strategy
  updateStrategy:
    type: Recommend           # Observe | Recommend | OneShot | Canary | Auto
    # mode-specific config (for Canary and Auto):
    canary:
      percentage: 10          # % of pods to resize first
      observationPeriod: 30m  # example. Omit or 0s for 5m. Shortest accepted value is 1m.
    # Cooldown between resizes of the same workload
    cooldown: 1h              # default: 1h, min: 1m; other apps are not locked
    # Automatic revert on OOMKill, throttle, restarts, NotReady, or SLO breach
    autoRevert: true          # default: true
    safetyObservationPeriod: 5m  # observe pod post-resize (default: 5m, min: 1m)
    # Opt-in: write recommendations into Deploy/STS pod templates so replacement
    # pods start correctly sized (default off; avoid unmanaged GitOps thrash).
    # templatePersistence:
    #   enabled: true
    #   when: AfterSuccessfulResize  # or OnRecommendation
    sloGuardrails:            # optional: application-level SLO checks post-resize
      - name: p99-latency
        query: "histogram_quantile(0.99, rate(http_duration_seconds_bucket{namespace=\"{{ .Namespace }}\"}[5m]))"
        threshold: "0.5"
        comparison: above     # revert if value > threshold
        evaluationWindow: 5m  # wait before checking (default: 5m, min: 1m)

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
            cpuRequest: "500m"
            cpuLimit: "1000m"
            memoryRequest: "512Mi"
            memoryLimit: "1Gi"
          recommended:
            cpuRequest: "150m"
            cpuLimit: "300m"
            memoryRequest: "280Mi"
            memoryLimit: "560Mi"
          confidence: 0.92
          dataPoints: 1680
          lastUpdated: "2026-01-15T10:30:00Z"

  # Savings estimate
  savings:
    cpuRequestReduction: "1050m"    # total across all pods
    memoryRequestReduction: "696Mi"
    estimatedMonthlySavings: "$142.50"  # if costModel is configured

  # Resize history (last 50)
  resizeHistory:
    - timestamp: "2026-01-15T09:00:00Z"
      workload: api-server
      container: api
      resource: cpu
      from: "500m"
      to: "150m"
      method: InPlace
      result: Success
    - timestamp: "2026-01-15T09:05:00Z"
      workload: worker
      container: app
      resource: cpu+memory
      from: ""
      to: ""
      method: Eviction
      result: Evicted

  # Last GitOps notification PR (survives annotation wipe by Flux/Argo)
  gitopsPR:
    driftFingerprint: "sha256:..."
    lastAttempt: "2026-01-15T09:00:00Z"
    url: "https://github.com/org/repo/pull/41"
```

#### CRD Validation (Webhook)

Validation is implemented in the admission webhook (`internal/webhook/validation.go`),
not via CEL `x-kubernetes-validations` markers. The webhook enforces:

- `minAllowed <= maxAllowed` for both CPU and memory resource configs. On an AttunePolicy write, a min inherited from AttuneNamespaceDefaults or AttuneDefaults is included when the policy omits min and a max is already known on the policy or a container. A stored inverted pair is not rechecked until the next update. A stored min above a non-zero max is clamped to that max and `boundsApplied` is `max`
- `cpu.maxAllowed` must not exceed 256 cores; `memory.maxAllowed` must not exceed 16Ti (AttunePolicy and AttuneDefaults)
- Canary config required when `updateStrategy.type` is `Canary`
- `historyWindow` bounded between 1h and 720h (30 days), including an unchanged stored value below 1h
- `cooldown`, `safetyObservationPeriod`, and an SLO `evaluationWindow` must be at least 1m on create. An unchanged stored `0s` is accepted on `AttunePolicy`, `AttuneDefaults`, and `AttuneNamespaceDefaults`, including a finalizer clear. Changing a positive duration to `0s` is rejected. A stored cooldown of `0s` is not a wait: the controller uses 1h, including after defaults merge. A stored safety period of `0s` is unset (5m, or a positive canary period). A stored SLO window of `0s` uses 5m
- `burstSensitivity` bounded between 0 and 10.0
- A set `limitMultiplier` on an AttunePolicy requires `controlledValues: RequestsAndLimits` on that same CPU or memory block. Omitted and empty modes are rejected. `RequestsOnly` with a multiplier stays rejected by the earlier check. An empty multiplier stays unset, and `"1"` is not exempt. AttuneDefaults and AttuneNamespaceDefaults may still store a multiplier without the mode. A stored policy is not rewritten
- All float fields (percentile, overhead, etc.) reject NaN and Inf
- Prometheus address SSRF protection (scheme, host, and IP validation)
- `containerPolicies` entries: duplicate `containerName`, a second `*`, and an empty `containerName` are rejected. `startupBoost`, `memoryFromCpuRatio`, `decreaseUsageMarginPercent`, `limitMultiplier`, `oomBump`, and `surge` on a container entry are rejected because they stay policy-wide in v1. `minAllowed <= maxAllowed` still applies to each container `cpu` and `memory` block in the webhook. The CRD quantity rule stays on `spec.cpu` and `spec.memory`, because copying it onto each containerPolicies entry exceeds the API server CEL cost budget.

`containerPolicies` is a field-wise list on `AttunePolicySpec` only. A literal name beats `*` per field, and `*` beats the merged policy block. v1 honors `percentile`, `overhead`, `minAllowed`, `maxAllowed`, `burstSensitivity`, `maxChangePercent`, `maxIncreasePercent`, `maxDecreasePercent`, `allowDecrease`, and `controlledValues`. An omitted container `maxAllowed` inherits `*` and then the policy block, and is uncapped only when that effective value is nil. Known sidecars stay excluded by default. Normal init containers are not managed. Policy-wide `startupBoost`, `memoryFromCpuRatio`, `decreaseUsageMarginPercent`, `limitMultiplier`, `oomBump`, and `surge` still apply from the policy block to every non-excluded container, and a container max caps startup boost and a policy memory OOM bump. After the field-wise merge, minAllowed above maxAllowed is rejected, including a defaults min when the container omits min and a max is already known. A stored min above a non-zero max stays clamped to that max until the next update. Container maxAllowed uses the same 256-core and 16Ti ceilings. There is no new Prometheus metric.

At resize time, a Guaranteed memory `limitMultiplier` is not an admission check. The limit is the multiplier times the engine request, then the request is raised to that limit so the pod stays Guaranteed. The applied request can exceed `memory.maxAllowed`. `maxAllowed` still caps the engine request. That raise is not the in-hold `oomBump` clamp. A CPU multiplier that would leave Guaranteed is skipped before `UpdateResize` and is not evicted, including when `resizeMethod` is `InPlaceOrRecreate`. `RequestsOnly` does not apply the multiplier.

#### Printer Columns

```go
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.spec.updateStrategy.type`
// +kubebuilder:printcolumn:name="Workloads",type=integer,JSONPath=`.status.workloads.discovered`
// +kubebuilder:printcolumn:name="Recs",type=integer,JSONPath=`.status.workloads.withRecommendations`
// +kubebuilder:printcolumn:name="Resized",type=integer,JSONPath=`.status.workloads.resized`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:printcolumn:name="CPU Saved",type=string,JSONPath=`.status.savings.cpuRequestReduction`,priority=1
// +kubebuilder:printcolumn:name="Mem Saved",type=string,JSONPath=`.status.savings.memoryRequestReduction`,priority=1
```

```
$ kubectl get attunepolicies
NAME            MODE        WORKLOADS   RECS   RESIZED   READY   AGE
api-services    Canary      3           3      2         True    7d

$ kubectl get attunepolicies -o wide
NAME            MODE        WORKLOADS   RECS   RESIZED   READY   AGE   CPU SAVED   MEM SAVED
api-services    Canary      3           3      2         True    7d    1050m       696Mi
```

### 3.3 AttuneDefaults (Cluster-Scoped, Optional)

Global defaults to avoid repetition across many AttunePolicy resources.
`containerPolicies` is not a field of `AttuneDefaultsSpec`. `AttuneNamespaceDefaults` uses that same spec, so the list is not inherited.

### 3.4 AttuneNamespaceDefaults (Namespaced, Optional)

Namespace-scoped defaults reuse the same spec as `AttuneDefaults` but apply
only within one namespace. Precedence is per field: policy spec >
namespace defaults > cluster `AttuneDefaults` > built-in defaults.
Fields set on the namespace object win; fields left unset (including
`costPricing.cpuPerCoreHour` vs `memoryPerGiBHour` independently) inherit
from cluster `AttuneDefaults`, then built-ins.

```yaml
apiVersion: attune.io/v1alpha1
kind: AttuneDefaults
metadata:
  name: default
spec:
  metricsSource:
    prometheus:
      address: http://prometheus-server.monitoring:80
    historyWindow: 168h
    minimumDataPoints: 48
  cpu:
    percentile: 95
    overhead: "20"
    controlledValues: RequestsAndLimits
  memory:
    percentile: 99
    overhead: "30"
    controlledValues: RequestsAndLimits
    allowDecrease: false
  updateStrategy:
    type: Recommend
    cooldown: 1h
    autoRevert: true
```

### 3.5 Status Conditions

| Condition Type | Reasons | Description |
|---------------|---------|-------------|
| `Ready` | `Monitoring`, `InsufficientData`, `NoWorkloadsFound`, `MetricsUnavailable` (alias `PrometheusUnavailable`), `InvalidConfig`, `WorkloadDiscoveryFailed`, `WorkloadCRDMissing`, `ConflictCheckFailed`, `Paused`, `PrometheusSeriesCapped` | Overall health. `PrometheusSeriesCapped` keeps Ready True and means the query result was partial. `WorkloadCRDMissing` means a Rollout policy's CRD is not installed |
| `TemplatePersistence` | `TemplateWorkloadRef`, `WorkloadRefUnread` | `TemplateWorkloadRef` is False when a Rollout `spec.workloadRef` was read. Attune does not patch that template. `WorkloadRefUnread` stays when that read failed. It is not replaced in the same reconcile |
| `Resizing` | `InProgress`, `Idle`, `CooldownActive` | Active resize operation |
| `Degraded` | `HighRevertRate` | Some resizes failing |
| `ScheduleBlocked` | `OutsideWindow`, `InsideWindow` | Whether the current time is within the configured resize schedule window. An equal start and end never opens. |
| `ResizeBlocked` | `NamespaceFrozen`, `HPAListUnavailable`, `VPAListUnavailable`, `PodsDeferred`, `PodsInfeasible`, `PodsDeferredAndInfeasible` | Namespace freeze, HPA or VPA list failure, or pods stuck Deferred or Infeasible |
| `SafetyObservation` | `Observing`, `Evaluating`, `RestorePending`, `Incomplete` | Pods still carrying `attune.io` resize-tracking annotations |

Status conditions use `meta.SetStatusCondition()` from `k8s.io/apimachinery/pkg/api/meta`
(the Kyverno pattern) with `observedGeneration` on every condition.

---

<a id="architecture"></a>
## 4. Architecture

### 4.1 High-Level Components

```
┌──────────────────────────────────────────────────────────────────┐
│                        attune                             │
│                                                                   │
│  ┌─────────────────────┐    ┌─────────────────────────┐         │
│  │  Policy Controller  │    │  Metrics Collector      │         │
│  │  ─────────────────  │    │  ───────────────────    │         │
│  │  Reconciles         │    │  Queries Prometheus     │         │
│  │  AttunePolicy    │◄──►│  Aggregates usage data  │         │
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

A single controller reconciles `AttunePolicy` resources. The reconcile function:

```
1. FETCH policy and resolve defaults: merge AttuneNamespaceDefaults (if present) over AttuneDefaults; unset namespace fields inherit from cluster, then built-ins
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
| AttunePolicy | Full | Primary reconciliation target |
| AttuneNamespaceDefaults | Full | Namespace defaults lookup |
| AttuneDefaults | Full | Cluster defaults lookup |
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
  resources: ["deployments", "statefulsets", "daemonsets"]
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
- apiGroups: ["attune.io"]
  resources: ["attunepolicies", "attunepolicies/status"]
  verbs: ["get", "list", "watch", "update", "patch"]
- apiGroups: ["attune.io"]
  resources: ["attunedefaults", "attunenamespacedefaults"]
  verbs: ["get", "list", "watch"]
```

---

<a id="algorithm-design"></a>
## 5. Algorithm Design

### 5.1 Composable Estimator Chain

Inspired by VPA's decorator pattern, but with critical improvements:

When `cpu.startupBoost.excludeFromHistory` is true, the Prometheus CPU
query is `max by (pod, container)` so each pod and container is one
series. The exclusion window starts at `attune.io/startup-boost-at`
when that stamp is set and not before creation, otherwise at
CreationTimestamp. CPU samples from that start until start plus duration
plus the rate window are dropped, then the remaining pod series are
reduced with `podAggregation`. A sample at the cutoff stays. Nil and
false keep today's percentile. Memory samples are unchanged. Deleted
pods stay until `historyWindow`. A recreated pod name keeps samples
older than the new CreationTimestamp.

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
│ Overhead         │  Add overhead percentage (default 20% CPU, 30% memory)
│ Estimator        │  Ensures headroom above observed usage
└──────┬───────────┘
       │
       ▼
┌──────────────────┐
│ Confidence       │  Widen recommendation when data is sparse:
│ Multiplier       │  result *= 1 + multiplier * (1 - confidence) ^ exponent
│                  │  confidence = clamp(min(days, sqrt(points/24)) / 7, 0, 1)
│                  │  Factor is 1.0 at confidence 1 and 2.0 at confidence 0
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

1. Attune adjusts **requests** (the base resource allocation)
2. HPA adjusts **replica count** based on utilization percentage of requests
3. By right-sizing requests, HPA's percentage calculations become more accurate

To prevent conflicts:
- Detect HPA presence via informer
- If HPA targets CPU utilization, an omitted `limitMultiplier` keeps the
  request-to-limit ratio. An explicit multiplier replaces that ratio and
  stays off until set. When `controlledValues` is `RequestsAndLimits`, HPA
  auto-tune caps the CPU utilization target using that multiplied limit,
  which can sit above `maxAllowed`.
- If HPA targets custom metrics (not CPU/memory), no conflict exists
- Log a warning if both VPA and Attune target the same workload

Workload scale states (per workload, not a policy-wide condition):
- **HPA ScaledToZero**: matching HPA `status.conditions[type=ScaledToZero]=True`.
  Classifier reads the condition string; it does not gate on cluster version.
- **Manual zero**: owner `spec.replicas == 0` and no ScaledToZero. Never
  `status.replicas`.
- **Active**: otherwise.

Idle (HPA ScaledToZero or manual zero) skips apply for that workload:
no in-place resize, template persist, startup boost, or eviction.
CREATE initial sizing does not look at owner replica count.

---

<a id="resize-engine"></a>
## 6. Resize Engine

### 6.1 Resize Flow

```
1. SELECT target pods based on update strategy mode:
   - OneShot: one eligible needing pod per cycle
   - Canary: canaryPercentage% of pods (round up to at least 1)
     per app; CREATE sizing, startup boost, and HPA stay off until
     that app's own watch promotes it (`status.canary.workloads`)
   - Auto: canary first, then remaining after observation period
     (observation `startTime` is the first successful in-place canary
     resize, not the first skipped attempt; a revert clears the clock)

2. For each selected pod:
   a. PRE-CHECK:
      - Pod is Running and Ready
      - Pod is not being deleted (DeletionTimestamp == nil)
      - Pod is not owned by attune itself
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

On Kubernetes 1.33 and earlier, a NotRequired memory limit decrease is
clamped to the current limit. Kubernetes 1.34 and newer allow that
decrease. Attune skips the platform clamp from 1.34 on. The usage floor
still raises a limit that would sit at or below recent usage plus
`memory.decreaseUsageMarginPercent`.

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
| Pod deleted during resize | New pod uses workload template; with opt-in `templatePersistence`, template tracks recommended/applied sizes so replacements start correctly sized (default off) |
| Node has insufficient resources | Resize marked Deferred; retry on next reconciliation |
| QoS class would change | Pre-check rejects the resize |
| Guaranteed memory limitMultiplier | The request is raised to the multiplied limit and can exceed `memory.maxAllowed`. The pod stays Guaranteed. A CPU multiplier that would leave Guaranteed is skipped and is not evicted. `RequestsOnly` does not apply the multiplier |
| StatefulSet partition holds the old revision | Pods whose `controller-revision-hash` is not `status.updateRevision` stay skipped while `currentRevision` and `updateRevision` differ. A stale generation skips every pod. OnDelete pods are resized |
| LimitRange violation | API server rejects; log and skip |
| ResourceQuota exceeded | API server rejects; log and skip |
| Static CPU/Memory Manager | Infeasible for Guaranteed QoS pods; skip with warning |
| Multiple containers in pod | Resize each independently; any failure skips remaining |
| VPA also targeting workload | Detect via VPA informer; log conflict warning, defer to VPA |

---

<a id="safety-system"></a>
## 7. Safety System

### 7.1 Graduated Rollout Modes

| Mode | Behavior | Risk Level |
|------|----------|------------|
| `Observe` | Collect metrics and track data-point progress; no recommendations surfaced | None |
| `Recommend` | Generate recommendations in status, no changes | None |
| `OneShot` | Resize one eligible needing pod per cycle | Low |
| `Canary` | Resize canary%, monitor, then remaining | Medium |
| `Auto` | Full automated canary-then-fleet | Medium-High |

### 7.2 Auto-Revert

When `autoRevert: true` (default), the Safety Monitor watches resized pods for:

1. **OOMKilled**: Container terminated with reason OOMKilled within observation period
2. **CPU Throttle**: CPU throttle ratio exceeds 50% post-resize. The threshold is fixed.
3. **Excessive Restarts**: Container restart count increases by 2+ post-resize
4. **Pod Not Ready**: Pod becomes NotReady within observation period
5. **SLO Guardrail Breach**: Application-level PromQL query breached its threshold after `evaluationWindow` elapsed (fails open on query errors). A window longer than the observation period keeps tracking until it elapses. The query then runs, including when that is after the observation period.

The observation period starts when the resize is submitted. It does not
finish while the kubelet still has the resize in progress or deferred
(`PodResizePending` with a reason other than `Infeasible`). If the period
elapses first, tracking stays, `attune.io/resize-apply-pending` is set
once, and the period starts again when the kubelet finishes. An infeasible
resize does not extend the period. An in-progress condition that has been
true for an hour does not extend the period and does not start a new one.
A resize that finishes during the original period does not gain a second
window.

On trigger:
1. Restore original resources via `/resize` subresource
2. Emit Kubernetes event on the Pod
3. Update AttunePolicy status with revert reason
4. Increment revert counter
5. Apply exponential backoff before retrying that workload (2x cooldown per revert)

### 7.3 Conflict Detection

Before any resize:
- Check for existing VPA targeting the same workload
- Check for existing HPA (adjust behavior, don't block)
- Check for other AttunePolicy with higher weight
- Check for `attune.io/skip: "true"` annotation on workload (opt-out)
- Check for `attune.io/freeze: "true"` on the policy namespace (skip apply
  only: resize, eviction, startup boost, template persist, CREATE initial
  sizing; recommendations still compute; pending safety observation still
  reverts unsafe pods and restores AfterSuccessfulResize templates; fail
  closed if the namespace cannot be read). Freeze is re-checked immediately
  before apply so a flag set during PromQL still skips persist and resize.
- Check for active rollout on the parent Deployment (don't resize during rollouts)

### 7.4 Memory OOM bump (off until set)

Memory requests stay on the percentile path unless `memory.oomBump` is set. An empty `oomBump: {}` turns the feature on and fills ratio `1.2`, minBump `100Mi`, maxBumps `3`, and hold `24h`. CPU rejects the block. The step is `max(ceil(origin * ratio^count), origin + minBump * count)`, then `maxAllowed`. Origin is the live memory request before the first bump of the streak, not the latest live request and not the pod template. When `maxAllowed` is omitted, `maxBumps` is the only cap. The count increments only after a successful resize. The streak is stored on `attune.io/oom-bump.<container>`. The name segment `oom-bump.<container>` must be at most 63 characters.

After `hold` expires, recommendations follow the normal percentile, allowDecrease, and template rules. Hold expiry does not clear the original request stored on the pod. A later OOM can step again from that same origin until `maxBumps`. During hold, a newer OOM whose next step from the frozen origin is not above the live request takes one step from that live request and keeps the origin. Auto, OneShot, and Canary record this `oomAt` and restart on the pod. The request stays clamped to `maxAllowed`. When `maxAllowed` is already at or below the live request, Auto, OneShot, and Canary consume the signal once as skipped and the count does not increase. Recommend and Observe do not write that stamp, so the same OOM still counts as skipped on every reconcile. Outside a hold, an OOM whose live request is already at or above `maxAllowed` does not raise the request. That signal is stored once and counted as `clamped`, including in Recommend and Observe. A newer finish time counts again. The hold stops a lower percentile from replacing the floor. It does not ignore a new OOM. During hold, the bump floor blocks a memory revert below the floor. A positive memory limit below the floor is raised only when that container's effective `controlledValues` is `RequestsAndLimits`. An empty `containerPolicies` list uses the policy memory block. A zero or missing limit is not created. `oomBump` stays on the policy block. An OOMKill verdict does not undo the bump. A non-OOM termination still reverts. A held OOM does not end observation. Throttle, NotReady, and SLO still run on that pass and still revert CPU, keeping memory at or above the floor. A Guaranteed pod with `RequestsOnly` is skipped. Attune does not evict to change QoS.

`explanation.memory.finalAdjustment` can include `oomBump`. The counter is `attune_oom_bump_total` with result `applied`, `clamped`, `capped`, or `skipped`. When `maxAllowed` drops below a held floor, the next recommendation publishes the cap and leaves the stored floor in place. Omitted `maxAllowed` does not add a cap. Default memory `allowDecrease` is false, so the highest in-hold pod request stays above the cap. That request is not the workload template. A replica still under the cap is raised only to the cap. The recommendation records the skip. A quiet reclamp counts `clamped` once per floor and cap per process. A new OOM step is clamped once in the step math and is not clamped again below `maxAllowed`.

### 7.5 Usage surge (off until set)

CPU and memory each stay on the long history window unless that resource's `surge` block is set. An empty `surge: {}` turns the feature on and fills trigger ratio `1.5`, percentile `99`, and window `30m`. There is no `surge: false`. A policy that omits `surge` inherits an `AttuneDefaults` surge. To keep a workload off, omit `surge` on both.

The long statistic is the published percentile: the max of the overall percentile and the 24 hour-of-day percentiles, at the parent percentile. The short statistic is the overall percentile only, at `surge.percentile`, of finite samples inside `surge.window`. Attune does not take the max across hour-of-day buckets for the short statistic. It fires when the window actually shortened, the finite count is at least 3 and at least half of `window / queryStep` (or at least 3 when `queryStep` is 0), and either the long percentile is positive and `short/long` is at least `triggerRatio`, or the long percentile is 0 and the short percentile is positive. Otherwise the long profile and the parent percentile are used. `minimumDataPoints` gates the long window only. One sample at 1.5 times the long percentile does not fire.

When the short window is selected, confidence is copied from the long profile. Percentile and burst use the short profile. CPU and memory choose separately. `memoryFromCpuRatio` does not switch the memory sample set; derived memory follows the surged CPU request. `explanation.<resource>.finalAdjustment` can include `surge` on the resource that used the short window.

The webhook rejects a trigger ratio that is not finite, not greater than 1, or above 100 (`100` is accepted). Empty trigger ratio is unset. Percentile must be 50, 90, 95, or 99. Window must be at least 5m and on a policy must not be longer than the effective history: `metricsSource.historyWindow` on the policy, then namespace defaults, then cluster defaults, then `168h`. On AttuneDefaults or AttuneNamespaceDefaults, the limit is that object's own `historyWindow`, or `168h`. A window equal to that limit is accepted. The surge check reads defaults only when the policy sets `surge.window` and omits `historyWindow`. The minimum-above-maximum check is a second read when the policy or a named container omits `minAllowed` and sets `maxAllowed`. Explicit `0s` is invalid.

### 7.6 Memory HPA retune (annotation still required)

Auto-tune stays off unless the HPA annotation `attune.io/auto-tune` is `"true"`. A successful in-place memory resize then retunes memory utilization targets with the same truncation as CPU. A Resource metric uses one pod's spec.containers request sum, including unchanged containers and off-pod history rows. Init containers, including native sidecars, stay out of that sum because a Kubernetes Resource metric does too. CREATE initial sizing still counts restartPolicy Always inits. A ContainerResource metric uses that container only, including a named init container. The limit cap matches that scope. Requests-only uses the live limit. No limit, or a limit at or below the new request, caps the percent at 100.

Pod Resource baselines are `attune.io/original-target-memory` and `attune.io/original-memory-request`. ContainerResource baselines share `attune.io/original-container-memory`, a JSON object keyed by container name. A second resize multiplies the stored original target by the stored request divided by the new request. Corrupt JSON is not rewritten. The percent for that cycle still uses this cycle's old and new request.

`updateStrategy.hpaTargetBounds` is optional. Nil or an empty object applies no user band and does not fill 50 or 90. CPU and memory bands are separate. When a side is set, Attune truncates, floors at 1, applies the limit cap, then `max`, then `min`, and publishes the limit cap again if `min` would exceed it. `HPATargetClamped` is a Normal event emitted after the HPA update succeeds, and only when the user band changes the post-limit percent. `min` and `max` are integers from 1 to 10000. `max` must be greater than or equal to `min` when both are set. AttuneDefaults may supply the block. There is no Helm value that turns the annotation or the band on.

---

<a id="metrics--observability"></a>
## 8. Metrics & Observability

### 8.1 Prometheus Metrics Exposed

All metrics use the `attune_` prefix and are exposed on the operator's
metrics endpoint (default port 8080). The operator registers 26 metrics
across five categories:

| Category | Metrics | Examples |
|----------|---------|----------|
| Recommendations | 3 gauges | `attune_recommendation_cpu_cores`, `attune_recommendation_memory_bytes`, `attune_confidence` |
| Resize operations | 4 counters + 1 histogram | `attune_resize_total`, `attune_eviction_total`, `attune_resize_duration_seconds` |
| Safety | 2 counters | `attune_reverts_total`, `attune_throttle_deferred_total` |
| Savings | 3 gauges | `attune_savings_cpu_cores_total`, `attune_savings_memory_bytes_total`, `attune_savings_estimated_monthly_dollars` |
| Data quality | 2 counters | `attune_nan_inf_samples_total`, `attune_request_clamped_total` |
| Operational guards | 4 counters + 1 gauge | `attune_schedule_skipped_total`, `attune_budget_exhausted_total`, `attune_startup_boost_total`, `attune_burst_factor` |
| Operator health | 2 counters + 2 histograms | `attune_reconcile_errors_total`, `attune_reconcile_duration_seconds`, `attune_prometheus_query_duration_seconds` |
| Webhooks | 1 counter + 1 histogram | `attune_webhook_validation_total`, `attune_webhook_duration_seconds` |

For the complete list with labels, descriptions, and query examples, see the
[Metrics Reference](reference/metrics.md).

### 8.2 Kubernetes Events

| Event | Type | Reason | Message Example |
|-------|------|--------|-----------------|
| Resize succeeded | Normal | Resized | "Resized cpu api-server/app: 500m -> 250m" |
| Resize failed | Warning | ResizeFailed | "Failed to resize pod api-server-abc12 container app: node has insufficient resources" |
| Resize skipped (QoS) | Warning | ResizeSkipped | "Resize blocked for pod X container Y: would change QoS class from Burstable to Guaranteed" |
| Resize skipped (envelope) | Warning | ResizeSkipped | "Resize blocked for pod X container Y: pod-level resource envelope would be exceeded" |
| Auto-revert triggered | Warning | Reverted | "Reverted resize on api-server/app: oomkill" |

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

<a id="testing-strategy"></a>
## 9. Testing Strategy

### 9.1 Test Pyramid

```
                    ┌───────────┐
                    │   E2E     │  Chainsaw: real cluster, full lifecycle
                    │   Tests   │  36 Chainsaw + 22 Go E2E scenarios
                    ├───────────┤
                    │Integration│  envtest: real API server + etcd
                    │   Tests   │  Controller reconciliation, CRD validation
                    ├───────────┤
                    │   Unit    │  Standard Go testing + testify
                    │   Tests   │  Algorithm, estimators, resize logic
                    │           │  1500+ test cases
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

**Framework**: standard `testing` + `github.com/stretchr/testify` + `controller-runtime/pkg/envtest`

**What to test**:
- AttunePolicy CR creation, validation, defaulting
- AttuneNamespaceDefaults overrides set fields on cluster `AttuneDefaults`; unset namespace fields inherit from cluster, then built-ins
- AttuneDefaults merging with policy-level overrides
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
| 2 | Create AttunePolicy in Recommend mode | Recommendations appear in status |
| 3 | Create AttunePolicy in OneShot mode | Single pod resized, status updated |
| 4 | Canary rollout | canary% pods resized first |
| 5 | Auto-revert on OOMKill | Resize reverted after simulated OOM |
| 6 | HPA coexistence | No conflict, both operate correctly |
| 7 | Policy conflict resolution | Highest weight policy wins |
| 8 | Opt-out annotation | Workload with skip annotation is ignored |
| 9 | Insufficient data | Policy reports InsufficientData condition |
| 10 | Upgrade operator version | CRDs migrated, no downtime |

**Test cluster**: CI uses k3d, not Kind. The push/PR E2E job runs a single K3S version (`v1.36.4-k3s1`), and `e2e-nightly.yaml` runs Kubernetes `v1.32`–`v1.36` as required cells plus `v1.37` as experimental (k3s prerelease image). 1.32 stays so the alpha `InPlacePodVerticalScaling` feature-gate path keeps running. 1.38 is added when k3s or kindest/node publishes an image. Prometheus is installed in-cluster from the Helm chart and cert-manager is bootstrapped before the operator tests run.

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
- Short names work (`kubectl get ap`)
- Scale subresource (if applicable)

---

<a id="cicd-pipeline"></a>
## 10. CI/CD Pipeline

### 10.1 GitHub Actions Workflows

#### `ci.yaml` - Continuous Integration (PRs, merge_group, and dispatch)

Product tests do not run again on `push` to `main`. The branch ruleset
requires an up-to-date PR, so a squash of a green PR is the same tree.
`push` to `main` still runs cheap promote jobs (release-please, Docs
deploy, Scorecard). Curated GitHub Release notes live on
`release-note-<semver>` (or Actions vars), not on `main`. The Release
job applies them and deletes the notes branch. Version-bump
`release-please*` PRs skip Helm/Docs/YAML lint and CodeQL analyze
(required `CodeQL (go)` / `CodeQL (actions)` still report a stand-in).

```
Jobs:
  changes:
    - dorny/paths-filter classifies Go, Helm, YAML, and docs changes
    - Downstream jobs skip irrelevant work on docs-only or YAML-only diffs

  lint:
    - golangci-lint v2.13.x (with .golangci.yml config)
    - `go mod tidy` cleanliness check
    - License boilerplate verification
    - Documentation defaults / dashboard metrics / tool-version consistency checks

  docs-check:
    - mkdocs build via `make docs-build`
    - Helm README freshness via `make helm-docs-check`
    - Supported tool version reference checks

  yaml-lint:
    - yamllint for `config/` and Helm values/chart metadata

  test-unit:
    - gotestsum over `./api/... ./cmd/... ./internal/... ./pkg/...`
    - race-enabled coverage run
    - Upload JUnit results and Codecov coverage
    - Fail if coverage < 80%

  test-fuzz-bench:
    - targeted Go fuzz runs for recommendation logic
    - benchmark run for `./internal/...`

  test-integration:
    - setup-envtest for Kubernetes 1.35 assets
    - gotestsum over `./test/integration/...` with `-tags=integration`

  test-e2e:
    - Create a k3d cluster for the current default K3S image
    - Install cert-manager and Prometheus in-cluster
    - Build and load the operator image
    - Run Chainsaw and Go E2E suites
    - Collect cluster debug info on failure

  crd-freshness:
    - Run `make manifests generate`
    - Fail if CRDs, RBAC, Helm CRDs, or deepcopy output drift

  helm-lint:
    - helm lint and template validation for chart CI values
    - helm-unittest
    - Helm README freshness check
    - Helm RBAC parity check

  build:
    - Build manager and kubectl plugin binaries
    - Build the container image locally (no push)
```

#### `e2e-nightly.yaml` - Full nightly E2E matrix (scheduled + manual)

```
Jobs:
  prepare-matrix:
    - Expands the selected Kubernetes version input (`v1.32`–`v1.37`, or all)
    - Selects the requested suite (`chainsaw`, `go-e2e`, or all)

  test-e2e:
    - Runs the full k3d/K3S E2E flow per selected version
    - Uses isolated cluster names and kubeconfig paths per matrix entry
    - Uploads per-version logs and debug artifacts

  report:
    - Fails when prepare-matrix, E2E, or fuzz fails, or when one of those
      jobs is cancelled or skipped while another succeeded. A fully
      cancelled run does not fail Nightly Results and does not change
      the open failure issue. A scheduled 1.37 (experimental) failure
      does not fail Nightly Results.
    - On schedule, a red run opens or updates an assigned issue labeled
      e2e-nightly-failure and ready. The assignee is the
      NIGHTLY_FAILURE_ASSIGNEE repository variable, or SebTardif when
      that variable is empty or is the org login. The same UTC day and
      signature updates the issue. The signature names failed or
      cancelled jobs. A skipped job is named only when no other job
      failed. A later UTC day closes the open issue and opens a new one
      whose title includes the consecutive-day count, counted from the
      first failed UTC day. A green scheduled run closes the open
      failure issue. workflow_dispatch does not open or close that issue.
```

#### `release.yaml` - Release (on tag push `v*`)

```
Jobs:
  release:
    - docker/build-push-action builds and pushes multi-arch images to GHCR
    - cosign signs the released container image
    - syft generates an SBOM
    - Trivy scans the released image
    - GoReleaser publishes binaries and release artifacts
    - Attach install manifest and SBOM to the GitHub release

  helm-release:
    - Package and push the Helm chart to GHCR OCI
    - Sign the published chart with cosign
```

#### `bench-baseline.yaml` - Shared bench cache (push to main, path-filtered)

Writes the default-branch Actions cache that PR `test-bench` jobs restore.
PR jobs cannot update that cache scope.

#### `security.yaml` - Security Scanning (on PR, weekly schedule, dispatch)

```
Jobs:
  govulncheck:
    - govulncheck ./...

  trivy:
    - Trivy filesystem scan with self-hosted Docker credential-store workaround

  trivy-image:
    - Build the operator image to a tarball with `docker buildx build --output`
    - Trivy image scan from the tarball

  gitleaks:
    - Full-repo secret scan with `fetch-depth: 0`

Notes:
  - CodeQL and dependency-review are intentionally disabled for this private repo
    because they require GitHub Advanced Security
```

#### `docs.yaml` - Documentation build validation (on docs pushes + manual)

```
Jobs:
  build:
    - mkdocs build via `make docs-build`
    - Upload the built site as a workflow artifact
    - No GitHub Pages deployment workflow is configured
```

#### `dependabot-auto-merge.yaml` - Dependabot merge automation

```
Jobs:
  auto-merge (pull_request_target, Dependabot only):
    - Approves with GITHUB_TOKEN; enables squash auto-merge with the App token
    - All semver types; CI is the safety gate
    - Docker PRs: copy verify-go-version-sync.sh to /tmp, --write --root workspace
  rebase-outdated (push to main):
    - App-token git rebase + force-with-lease on Dependabot PRs with mergeable_state=behind
    - Does not comment @dependabot rebase (Apps are rejected)
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

<a id="distribution"></a>
## 11. Distribution

### 11.1 Helm Chart

**Primary installation method.** Structure:

```
charts/attune/
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
- `image.repository`, `image.tag` (empty tag renders `appVersion`, bare SemVer)
- `resources` (operator pod resources)
- `metrics.enabled` (expose /metrics)
- `securityContext` (non-root, read-only root filesystem, drop all capabilities)

### 11.2 OCI Registry

```bash
# Push Helm chart
helm push attune-0.1.0.tgz oci://ghcr.io/attune-io/charts

# Install from OCI
helm install attune oci://ghcr.io/attune-io/charts/attune --version 0.1.0
```

### 11.3 kubectl Plugin

Distributed via Krew:

```bash
kubectl krew install attune

kubectl attune doctor
kubectl attune status -n production
kubectl attune savings
kubectl attune recommendations -n production
```

### 11.4 Raw Manifests

For users who don't use Helm:

```bash
kubectl apply -f https://github.com/attune-io/attune/releases/latest/download/install.yaml
```

---

<a id="documentation"></a>
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
│   ├── version-best.md         # Version-best Kubernetes (1.32+)
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

If you are using attune in your organization, please add your
company to this list. It helps the project understand its user base
and prioritize features.

| Organization | Contact | Date | Description |
|-------------|---------|------|-------------|
```

---

<a id="project-structure"></a>
## 13. Project Structure

```
attune/
├── .github/
│   ├── workflows/
│   │   ├── ci.yaml
│   │   ├── bench-baseline.yaml
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
│       ├── attunepolicy_types.go
│       ├── attunepolicy_types_test.go
│       ├── attunedefaults_types.go
│       ├── conditions.go
│       ├── zz_generated.deepcopy.go
│       └── doc.go
├── cmd/
│   ├── manager/
│   │   └── main.go              # Operator entry point
│   └── kubectl-attune/
│       └── main.go              # kubectl plugin
├── internal/
│   ├── conflict/
│   │   ├── detector.go          # VPA, HPA, policy conflict detection
│   │   └── detector_test.go
│   ├── controller/              # Reconciler (core business logic)
│   │   ├── attunepolicy_controller.go
│   │   ├── attunepolicy_controller_test.go
│   │   └── ...                  # helpers, resize, prometheus, surge, export, etc.
│   ├── metrics/
│   │   ├── collector.go         # Prometheus/Datadog/CloudWatch query client
│   │   ├── collector_test.go
│   │   ├── profile.go           # UsageProfile construction
│   │   └── profile_test.go
│   ├── operatormetrics/         # Operator-level Prometheus metrics (init-registered)
│   │   └── metrics.go
│   ├── recommendation/
│   │   ├── estimator.go         # Estimator interface
│   │   ├── percentile.go        # Percentile estimator
│   │   ├── margin.go            # Safety margin estimator
│   │   ├── confidence.go        # Confidence multiplier
│   │   ├── bounds.go            # Bounds clamper
│   │   ├── chain.go             # Composable chain
│   │   └── fuzz_test.go
│   ├── resize/
│   │   ├── engine.go            # Pod resize via /resize subresource
│   │   └── engine_test.go
│   ├── safety/
│   │   ├── monitor.go           # OOMKill, throttle, restart, NotReady, SLO guardrails, auto-revert
│   │   └── monitor_test.go
│   ├── throttle/                # Shared throttle checker interface
│   ├── transform/               # Informer cache transform functions
│   ├── validation/              # Shared validation (Prometheus SSRF checks)
│   └── webhook/
│       ├── defaulting.go        # Defaulting webhook
│       ├── validation.go        # Validation webhook
│       └── defaults_validation.go # AttuneDefaults validation
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
│   └── attune/
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

<a id="roadmap"></a>
## 14. Roadmap

### Phase 1: Foundation (MVP)

- [x] Project scaffolding (Kubebuilder)
- [x] AttunePolicy CRD (v1alpha1)
- [x] Prometheus metrics collector
- [x] Percentile-based recommendation engine
- [x] Status reporting (recommendations, conditions)
- [x] Observe and Recommend modes only (no resize)
- [x] Helm chart
- [x] Unit tests (75%+ coverage)
- [x] envtest integration tests
- [x] CI pipeline (lint, test, build)
- [x] README with quickstart

### Phase 2: Resize Engine

- [x] In-place resize via /resize subresource
- [x] OneShot mode
- [x] Canary mode with graduated rollout
- [x] Resize status polling and timeout handling
- [x] QoS preservation checks
- [x] LimitRange/ResourceQuota compatibility
- [x] E2E tests (Chainsaw)
- [x] Security scanning in CI

### Phase 3: Safety & Intelligence

- [x] Safety monitor (OOMKill, throttle, restart, NotReady, SLO guardrails)
- [x] Auto-revert mechanism
- [x] Confidence-based recommendation widening
- [x] Time-of-day-aware algorithm
- [x] Burst detection
- [x] HPA coexistence logic
- [x] VPA conflict detection
- [x] Policy weight-based conflict resolution

### Phase 4: Production Readiness

- [x] Auto mode (canary then fleet)
- [x] AttuneDefaults / AttuneNamespaceDefaults
- [x] Grafana dashboard
- [x] MkDocs documentation site
- [x] Cosign image signing
- [x] SBOM generation
- [x] Release automation (GoReleaser)
- [x] OCI Helm chart distribution
- [x] Fuzz tests
- [x] Benchmark tests

### Phase 5: Ecosystem

- [x] kubectl plugin (via krew)
- [x] Datadog/CloudWatch metrics support
- [x] Memory decrease support (with gradual decrease)
- [x] Multi-cluster aggregated reporting (Phase A/B: federation docs, fleet Grafana dashboard, recording rules, fleet report ConfigMap + collect script; hub control plane deferred)
- [ ] CNCF Sandbox application
- [ ] KubeCon talk proposal
- [ ] ADOPTERS.md with real organizations

---

<a id="competitor-lessons"></a>
## 15. Competitor Lessons

### Patterns Adopted

| Pattern | Source | How We Use It |
|---------|--------|---------------|
| Mandatory resource bounds | OptiPod | `minAllowed`/`maxAllowed` fields |
| Weight-based policy resolution | OptiPod | `weight` field for deterministic conflict resolution |
| Gradual memory decrease | OptiPod | `memory.maxChangePercent` + `allowDecrease` flag |
| Composable estimator chain | VPA | Decorator pattern: percentile -> overhead -> confidence -> bounds |
| Confidence-based widening | VPA | `1 + multiplier * (1 - confidence) ^ exponent` |
| Two-phase resize (CPU then memory) | right-sizer | CPU first (safer), then memory, with proper polling |
| Conditions via meta.SetStatusCondition | Kyverno | Standard library helper, not hand-rolled |
| Print columns with priority | Kyverno | `-o wide` shows savings columns |
| Strict CI shell defaults | CloudNativePG | `bash -Eeuo pipefail -x {0}` in all workflows |
| ADOPTERS.md from day one | CloudNativePG | Social proof drives adoption |
| envtest + property-based testing | OptiPod | Fast feedback + invariant testing |
| Percentage overhead (not multiplier) | CAST AI, KRR, VPA | `overhead: "20"` = +20% headroom (ecosystem consensus) |
| `minAllowed`/`maxAllowed` naming | VPA | Direct match with VPA `containerPolicies` field names |
| `controlledValues` field | VPA | Direct match with VPA (RequestsOnly / RequestsAndLimits) |
| Hierarchical defaults CRD | PerfectScale | Cluster > namespace > policy precedence (3-tier) |
| Per-step change cap in ResourceConfig | StormForge | `maxChangePercent` per resource (StormForge uses `maxPercentIncrease`/`maxPercentDecrease`) |
| Preview/Apply progression | Datadog | Our Observe > Recommend > Canary > Auto mirrors Datadog's Preview > Apply |
| Unified vertical CRD (not VPA+HPA) | Datadog | Single AttunePolicy instead of separate VPA + HPA objects |
| Cron-style scheduling | Oblik | `schedule.windows` + `daysOfWeek` (Oblik uses `cron` + `cronAddRandomMax`) |
| Annotation-based opt-out | CAST AI, Oblik | `attune.io/skip: "true"` for workload exclusion |
| Namespace freeze kill-switch | Kilter | `attune.io/freeze: "true"` on the namespace skips apply (resize, persist, CREATE initial sizing); pending safety revert still runs |

### Anti-Patterns Avoided

| Anti-Pattern | Source | Why We Avoid It |
|-------------|--------|-----------------|
| Bloated CRD (15+ config sections) | right-sizer | Focused CRD + separate defaults CRD |
| Emoji logging / fmt.Printf | right-sizer, OptiPod | Structured logging only (logr) |
| Hardcoded time.Sleep between operations | right-sizer | Proper polling via wait.PollUntilContextCancel |
| No CRD (annotation-only) | kube-reqsizer, Oblik | Full CRD with proper status (Oblik supports both but annotations are fragile at scale) |
| Manual memory string parsing | kube-reqsizer | Always use resource.Quantity |
| Status Phase as bare string | right-sizer | Typed constants with kubebuilder enum validation |
| ObservedGeneration via annotations | right-sizer | Proper status subresource field |
| All containers resized together | VPA | Per-container independent resize |
| HPA conflict undefined | VPA | Detect and handle HPA coexistence |
| SaaS-only with no self-hosted option | CAST AI, PerfectScale, Sedai, nOps | Fully self-contained operator, metrics stay in-cluster |
| Black-box ML recommender | StormForge, Sedai, ScaleOps | Transparent percentile + overhead + confidence chain; every step visible in explanation |
| Combined horizontal + vertical in one field | VPA (`updateMode`) | Separate `type` (what to do) and `resizeMethod` (how to apply) for clarity |
| Platform API instead of CRD | Sedai, Densify | Kubernetes-native CRD; works with GitOps, kubectl, and standard tooling |
| Multiplier-based overhead (1.2x) | kube-reqsizer | Percentage-based overhead ("20" = +20%), matching ecosystem consensus |

### Competitor Landscape (16 tools surveyed)

| Category | Tools | Key takeaway |
|----------|-------|-------------|
| **OSS recommenders** | VPA, Goldilocks, KRR, Kubecost/OpenCost | Good for visibility and one-time audits; no autonomous application (except VPA Auto, which evicts) |
| **OSS appliers** | Oblik, kube-reqsizer, Kedify, k8s-sustain, CruiseKube | In-place `/resize` or cron apply; k8s-sustain adds KEDA/Argo and a dashboard; none match Attune canary + SLO revert |
| **Commercial full-stack** | CAST AI, ScaleOps, StormForge, PerfectScale, Sedai, Densify | Pod + node optimization with ML; $10k-50k+/year; SaaS dependency (except ScaleOps self-hosted) |
| **Observability-integrated** | Datadog, nOps, Spot Ocean | Leverage existing monitoring; Datadog's `DatadogPodAutoscaler` CRD is well-designed |
| **Attune** | (this project) | Focused on in-place resize with safety; open-source; no SaaS; Kubernetes-native CRDs |
