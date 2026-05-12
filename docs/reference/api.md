## RightSizePolicy

**Group**: `rightsize.io`  
**Version**: `v1alpha1`  
**Scope**: Namespaced  
**Short name**: `rsp`

### Spec

```yaml
apiVersion: rightsize.io/v1alpha1
kind: RightSizePolicy
metadata:
  name: example
  namespace: default
spec:
  # Target workload(s) to right-size.
  targetRef:
    kind: Deployment            # Deployment | StatefulSet | DaemonSet
    name: my-app                # optional: target a specific workload
    selector:                   # optional: target by label selector
      matchLabels:
        tier: api

  # Prometheus metrics configuration.
  metricsSource:
    prometheus:
      address: "http://prometheus:9090"   # Prometheus URL
    historyWindow: 168h                    # lookback window (default: 168h)
    minimumDataPoints: 168                 # min samples before recommending (default: 168)

  # CPU recommendation parameters.
  cpu:
    percentile: 95             # target percentile: 50, 90, 95, or 99
    safetyMargin: "1.2"        # multiplier for headroom (1.2 = 20%)
    bounds:                    # optional: min/max clamps
      min: "50m"
      max: "4000m"
    controlledValues: RequestsAndLimits  # RequestsOnly | RequestsAndLimits

  # Memory recommendation parameters.
  memory:
    percentile: 99
    safetyMargin: "1.3"
    bounds:
      min: "64Mi"
      max: "8Gi"
    controlledValues: RequestsAndLimits
    allowDecrease: false       # prevent memory decreases (recommended)

  # How and when to apply changes.
  updateStrategy:
    mode: Recommend            # Recommend | OneShot | Canary | Auto (Observe is an alias for Recommend)
    canary:                    # required when mode is Canary
      percentage: 10           # % of pods to resize first
      observationPeriod: 30m   # watch canary pods before proceeding
    maxCpuChangePercent: 50    # max CPU change per cycle (default: 50)
    maxMemoryChangePercent: 30 # max memory change per cycle (default: 30)
    cooldown: 1h               # min time between resize operations (default: 1h)
    autoRevert: true           # revert on safety violation (default: true)

  # Containers to skip (e.g., service mesh sidecars).
  excludeContainers:
    - istio-proxy
    - linkerd-proxy

  # Policy priority (1-1000, higher wins). Default: 100.
  weight: 100
```

### Status

| Field | Type | Description |
|-------|------|-------------|
| `conditions` | `[]Condition` | Standard Kubernetes conditions (Ready, Resizing, Degraded) |
| `workloads.discovered` | `int32` | Number of workloads matching the target |
| `workloads.withRecommendations` | `int32` | Workloads with active recommendations |
| `workloads.resized` | `int32` | Workloads that have been resized |
| `workloads.pending` | `int32` | Workloads awaiting resize |
| `workloads.dataPointsCollected` | `int32` | Max data points collected across all containers |
| `workloads.dataPointsRequired` | `int32` | Minimum data points needed before recommendations |
| `recommendations[].workload` | `string` | Workload name |
| `recommendations[].kind` | `string` | Workload kind |
| `recommendations[].containers[].name` | `string` | Container name |
| `recommendations[].containers[].current` | `ResourceValues` | Current CPU/memory requests and limits |
| `recommendations[].containers[].recommended` | `ResourceValues` | Proposed CPU/memory requests and limits |
| `recommendations[].containers[].confidence` | `float64` | Confidence score (0-1) |
| `recommendations[].containers[].dataPoints` | `int32` | Prometheus samples used |
| `recommendations[].containers[].lastUpdated` | `Time` | Last recommendation timestamp |
| `savings.cpuRequestReduction` | `string` | Total CPU request reduction (e.g. "1200m") |
| `savings.cpuRequestTotal` | `string` | Total current CPU requests across all workloads (e.g. "2000m") |
| `savings.memoryRequestReduction` | `string` | Total memory request reduction (e.g. "2Gi") |
| `savings.memoryRequestTotal` | `string` | Total current memory requests across all workloads (e.g. "2Gi") |
| `savings.estimatedMonthlySavings` | `string` | Estimated monthly cost savings |
| `resizeHistory[].timestamp` | `Time` | When the resize occurred |
| `resizeHistory[].workload` | `string` | Resized workload name |
| `resizeHistory[].container` | `string` | Resized container name |
| `resizeHistory[].resource` | `string` | `cpu` or `memory` |
| `resizeHistory[].from` | `string` | Previous value |
| `resizeHistory[].to` | `string` | New value |
| `resizeHistory[].method` | `string` | `InPlace` |
| `resizeHistory[].result` | `string` | `Success`, `Failed`, or `Reverted` |

### Condition types

| Type | Reasons | Description |
|------|---------|-------------|
| `Ready` | `Monitoring`, `InsufficientData`, `PrometheusUnavailable`, `InvalidConfig` | Overall health |
| `Resizing` | `InProgress`, `Idle`, `CooldownActive` | Active resize operation state |
| `Degraded` | `HighRevertRate` | High revert rate detected (3+ of last 5 reverted) |

### Print columns

```bash
kubectl get rsp
```

```text
NAME     MODE        WORKLOADS   RESIZED   READY   AGE
```

Pass `-o wide` to include `CPU Saved` and `Mem Saved` columns.

---

## RightSizeDefaults

**Scope**: Cluster  
**Short name**: `rsd`

```yaml
apiVersion: rightsize.io/v1alpha1
kind: RightSizeDefaults
metadata:
  name: default
spec:
  metricsSource:    # same structure as RightSizePolicy.spec.metricsSource
    prometheus:
      address: http://prometheus-server.monitoring:80
    historyWindow: 168h
    minimumDataPoints: 168
  cpu:              # same structure as RightSizePolicy.spec.cpu
    percentile: 95
    safetyMargin: "1.2"
    controlledValues: RequestsAndLimits
  memory:           # same structure as RightSizePolicy.spec.memory
    percentile: 99
    safetyMargin: "1.3"
    controlledValues: RequestsAndLimits
    allowDecrease: false
  updateStrategy:   # same structure as RightSizePolicy.spec.updateStrategy
    mode: Recommend
    cooldown: 1h
    autoRevert: true
  costPricing:      # optional, for EstimatedMonthlySavings computation
    cpuPerCoreHour: "0.031"     # USD per vCPU-hour (default: $0.031)
    memoryPerGiBHour: "0.004"   # USD per GiB-hour (default: $0.004)
```

RightSizeDefaults fields are merged into every RightSizePolicy at
reconciliation time. Policy-level values always take precedence.

### Cost pricing

The `costPricing` section configures per-unit pricing used to compute
`status.savings.estimatedMonthlySavings`. If omitted, standard on-demand
Linux pricing is used.

| Field | Default | Description |
|-------|---------|-------------|
| `cpuPerCoreHour` | `0.031` | Cost per vCPU-hour |
| `memoryPerGiBHour` | `0.004` | Cost per GiB-hour |

**Formula**: `(cpuCoresSaved * cpuPrice + memGiBSaved * memPrice) * 730 hours/month`

#### Reference pricing by provider

The defaults use AWS on-demand pricing. Adjust for your environment:

| Provider | Instance type | `cpuPerCoreHour` | `memoryPerGiBHour` | Notes |
|----------|---------------|------------------|--------------------|-------|
| **AWS** (default) | m6i on-demand | `"0.031"` | `"0.004"` | US East, Linux |
| AWS Savings Plans | m6i 1yr | `"0.020"` | `"0.003"` | ~35% discount |
| **GCP** on-demand | e2-standard | `"0.034"` | `"0.005"` | US |
| GCP committed | e2-standard 1yr | `"0.022"` | `"0.003"` | ~35% discount |
| **Azure** PAYG | D4s v5 | `"0.036"` | `"0.005"` | East US |
| Azure Reserved | D4s v5 1yr | `"0.022"` | `"0.003"` | ~38% discount |
| **On-prem** | bare metal | `"0.010"` | `"0.001"` | Amortized hardware |

These are approximate. Use your actual billing data for accurate savings
estimates. For reserved instances, use the reserved rate so savings reflect
true recoverability.

### Webhook validation

`RightSizeDefaults` has a validating webhook that rejects invalid
`costPricing` values. If `cpuPerCoreHour` or `memoryPerGiBHour` is set,
the webhook validates that each is a parseable positive float. Invalid
values (e.g., `"banana"`, `"-0.5"`) are rejected at admission time.
