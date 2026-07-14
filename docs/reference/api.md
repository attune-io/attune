## AttunePolicy

**Group**: `attune.io`  
**Version**: `v1alpha1`  
**Scope**: Namespaced  
**Short name**: `ap`

### Defaulting Behavior

Fields are defaulted in three layers. Only `weight` and `maxConcurrentResizes`
appear in the stored spec when omitted by the user (they are CRD schema or
webhook defaults). All other defaultable fields (`type`, `controlledValues`,
`cooldown`, `historyWindow`, `minimumDataPoints`, `queryStep`, `rateWindow`, `autoRevert`,
`resizeMethod`, `cpu.maxChangePercent`, `memory.maxChangePercent`,
`safetyObservationPeriod`, `excludeKnownSidecars`) are applied
by the controller at reconcile time so that cluster-wide `AttuneDefaults`
and namespace-scoped `AttuneNamespaceDefaults` can override them. These
fields will appear empty in `kubectl get attunepolicy -o yaml` but still control runtime
behavior through the controller's built-in and inherited defaults unless you
override them. Use `kubectl attune explain -n <namespace> <policy>` to see
the effective values for the key controller-applied defaults and whether each
came from the policy, a namespace default, a cluster default, or the built-in
default.

### Spec

```yaml
apiVersion: attune.io/v1alpha1
kind: AttunePolicy
metadata:
  name: example
  namespace: default
spec:
  # Target workload(s) to right-size.
  targetRef:
    kind: Deployment            # Deployment | StatefulSet | DaemonSet | CronJob | Job | ReplicaSet
    name: my-app                # optional: target a specific workload
    selector:                   # optional: target by label selector
      matchLabels:
        tier: api

  # Prometheus metrics configuration.
  metricsSource:
    prometheus:
      address: "http://prometheus:9090"   # Prometheus-compatible URL
      headers:                            # optional: non-secret tenant or routing headers
        X-Scope-OrgID: "my-tenant"        # e.g., Mimir tenant ID
      queryParameters:                    # optional: URL params for Thanos/VictoriaMetrics
        dedup: "true"                     # e.g., Thanos deduplication
        partial_response: "true"          # reserved query keys like query/start/end/step/time/timeout are rejected
      bearerTokenSecret:                  # optional: auth from Secret in the policy namespace
        name: prometheus-token
        key: token
      tls:                                # optional: TLS settings
        insecureSkipVerify: false
    # Alternative: consume VPA recommendations instead of querying Prometheus.
    # At most one of prometheus, datadog, cloudwatch, or vpa may be set.
    # vpa:
    #   name: my-vpa                       # VPA object name
    #   namespace: default                 # defaults to policy namespace
    historyWindow: 168h                    # lookback window (default: 168h)
    minimumDataPoints: 48                  # min samples before recommending (default: 48)
    queryStep: 5m                          # Prometheus range query step interval (default: 5m)
    rateWindow: 5m                         # PromQL rate() window for CPU queries (default: queryStep)

  # CPU recommendation parameters.
  cpu:
    percentile: 95             # target percentile: 50, 90, 95, or 99
    overhead: "20"        # percentage headroom above percentile
    burstSensitivity: "0.1"   # burst boost multiplier (0 = disabled, max 1.0)
    startupBoost:              # optional: temporary CPU boost for cold starts
      multiplier: "3.0"        # scale factor for startup CPU (1.1-10.0)
      duration: 2m             # boost window after pod creation (10s-1h)
    minAllowed: "1m"             # optional: min clamp
    maxAllowed: "4000m"          # optional: max clamp (upper limit: 256 cores)
    controlledValues: RequestsAndLimits  # RequestsOnly | RequestsAndLimits
    maxChangePercent: 50       # max CPU change per cycle (default: 50)
    maxIncreasePercent: 50     # max increase per cycle (default: 50)
    maxDecreasePercent: 30     # max decrease per cycle (default: 30)

  # Memory recommendation parameters.
  memory:
    percentile: 99
    overhead: "30"
    burstSensitivity: "0.1"
    minAllowed: "4Mi"
    maxAllowed: "8Gi"                 # upper limit: 16Ti
    controlledValues: RequestsAndLimits
    allowDecrease: false       # prevent memory decreases (recommended)
    maxChangePercent: 30       # max memory change per cycle (default: 30)
    maxIncreasePercent: 50     # max increase per cycle (default: 50)
    maxDecreasePercent: 30     # max decrease per cycle (default: 30)
    memoryFromCpuRatio: "2.0"  # optional: derive memory from CPU (GiB per core)

  # Pause reconciliation (no metrics, no recommendations, no resizes).
  paused: false                # default: false

  # How and when to apply changes.
  updateStrategy:
    type: Recommend            # Observe | Recommend | OneShot | Canary | Auto
    initialSizing: false       # optional: set pod resources at creation time via webhook
    canary:                    # required when type is Canary
      percentage: 10           # % of pods to resize first
      observationPeriod: 30m   # watch canary pods before proceeding (minimum: 1m)
      autoPromote: true        # promote to full fleet after safe observation (default: false)
    cooldown: 1h               # min time between resize operations (default: 1h)
    autoRevert: true           # revert on safety violation (default: true)
    safetyObservationPeriod: 5m  # post-resize safety watch period (default: 5m, minimum: 1m)
    resizeMethod: InPlaceOnly  # InPlaceOnly | InPlaceOrRecreate (default: InPlaceOnly)
    maxConcurrentResizes: 1    # parallel pod resizes per cycle (default: 1, max: 50)
    maxTotalCpuIncrease: "2000m"    # max aggregate CPU increase per cycle (default: unlimited)
    maxTotalMemoryIncrease: "4Gi"   # max aggregate memory increase per cycle (default: unlimited)
    export:                         # optional: export recommendations to ConfigMaps
      configMap: true               # creates <policy>-<workload>-recommendations ConfigMap
    templatePersistence:            # optional: write recs into Deploy/STS template
      enabled: false                # default off; triggers rolling update when true
      when: AfterSuccessfulResize   # or OnRecommendation
    sloGuardrails:                  # optional: revert if application SLOs breach after resize
      - name: p99-latency
        query: 'histogram_quantile(0.99, rate(http_request_duration_seconds_bucket{namespace="{{ .Namespace }}"}[5m]))'
        threshold: "0.5"
        comparison: above
    schedule:                       # optional: restrict when resizes can occur
      windows:
        - start: "02:00"           # HH:MM (24-hour)
          end: "06:00"
      daysOfWeek: ["Monday", "Tuesday", "Wednesday", "Thursday", "Friday"]
      timezone: "America/New_York" # IANA timezone (default: UTC)

  # Auto-exclude well-known mesh/sidecar names (default: true).
  # Set false to restore pre-feature behavior (only excludedContainers).
  excludeKnownSidecars: true

  # Extra container names to skip (unioned with the known list when
  # excludeKnownSidecars is true).
  excludedContainers:
    - my-company-agent

  # Policy priority (1-1000, higher wins). Default: 100.
  weight: 100
```

### Status

| Field | Type | Description |
|-------|------|-------------|
| `conditions` | `[]Condition` | Standard Kubernetes conditions (Ready, Resizing, Degraded, ScheduleBlocked) |
| `cooldown.effectiveCooldown` | `Duration` | Current cooldown including exponential backoff |
| `cooldown.backoffMultiplier` | `int32` | Current backoff multiplier (1, 2, 4, 8, or 16) |
| `cooldown.consecutiveReverts` | `int32` | Number of consecutive reverts driving the backoff |
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
| `recommendations[].containers[].explanation` | `ContainerRecommendationExplanation` | Persisted reasoning used by `kubectl attune explain` |
| `recommendations[].containers[].explanation.cpu` | `ResourceRecommendationExplanation` | CPU estimator-chain details |
| `recommendations[].containers[].explanation.memory` | `ResourceRecommendationExplanation` | Memory estimator-chain details |
| `recommendations[].containers[].confidence` | `float64` | Confidence score (0-1) |
| `recommendations[].containers[].dataPoints` | `int32` | Prometheus samples used |
| `recommendations[].containers[].lastUpdated` | `Time` | Last recommendation timestamp |
| `recommendations[].stale` | `bool` | `true` when Prometheus returned no fresh data; resize is blocked until fresh data arrives |
| `recommendations[].lastDataTime` | `Time` | Timestamp of the most recent Prometheus data point |

| `savings.cpuRequestReduction` | `string` | Total CPU request reduction (e.g. "1200m") |
| `savings.cpuRequestTotal` | `string` | Total current CPU requests across all workloads (e.g. "2000m") |
| `savings.memoryRequestReduction` | `string` | Total memory request reduction (e.g. "2Gi") |
| `savings.memoryRequestTotal` | `string` | Total current memory requests across all workloads (e.g. "2Gi") |
| `savings.estimatedMonthlySavings` | `string` | Estimated monthly cost savings |
| `savings.cpuRequestIncrease` | `string` | Total CPU increase for under-provisioned workloads (e.g. "500m") |
| `savings.memoryRequestIncrease` | `string` | Total memory increase for under-provisioned workloads (e.g. "512Mi") |
| `savings.estimatedMonthlyCostIncrease` | `string` | Estimated monthly cost increase for under-provisioned workloads |
| `resizeHistory[].timestamp` | `Time` | When the resize occurred |
| `resizeHistory[].workload` | `string` | Resized workload name |
| `resizeHistory[].container` | `string` | Resized container name |
| `resizeHistory[].resource` | `string` | `cpu`, `memory`, or `cpu+memory` |
| `resizeHistory[].from` | `string` | Previous value |
| `resizeHistory[].to` | `string` | New value |
| `resizeHistory[].method` | `string` | `InPlace`, `Eviction`, or `TemplatePersistence` |
| `resizeHistory[].result` | `string` | `Success`, `Failed`, `Reverted`, `Evicted`, or `TemplatePatched` |
| `resizeHistory[].reason` | `string` | Why a resize was reverted or failed (e.g. `oomkill`, `restart`, `notready`, `slo:<name>`). Empty for successful resizes. |
| `workloadErrors[].workload` | `string` | Workload name that encountered an error during reconciliation |
| `workloadErrors[].error` | `string` | Human-readable error description |
| `canary.phase` | `string` | `CanaryInProgress` or `FullRollout` |
| `canary.startTime` | `Time` | When the canary subset was first resized |
| `canary.observedGeneration` | `int64` | Policy generation when this canary cycle started |
| `canary.pods` | `[]string` | Names of pods selected for the canary subset |

`ResourceRecommendationExplanation` contains the intermediate fields emitted by
the estimator chain: `rawPercentile`, `overhead`, `afterOverhead`,
`burstFactor`, `afterBurst`, `confidence`, `confidenceFactor`, `afterConfidence`, `bounds`,
`boundsApplied`, `afterBounds`, `minChangePercent`, `maxChangePercent`,
`changeFilterApplied`, `afterChangeFilter`, `final`, and optional
`finalAdjustment`.

### Condition types

| Type | Reasons | Description |
|------|---------|-------------|
| `Ready` | `Monitoring`, `InsufficientData`, `NoWorkloadsFound`, `PrometheusUnavailable`, `InvalidConfig`, `WorkloadDiscoveryFailed`, `Paused` | Overall health |
| `Resizing` | `InProgress`, `Idle`, `CooldownActive` | Active resize operation state |
| `Degraded` | `HighRevertRate` | High revert rate detected (3+ of last 5 reverted) |
| `ScheduleBlocked` | `OutsideWindow`, `InsideWindow` | Whether the current time is within the configured resize schedule window |

### Print columns

```bash
kubectl get attunepolicy
```

```text
NAME     TYPE        WORKLOADS   RECS   RESIZED   READY   AGE
```

Pass `-o wide` to include `CPU Saved` and `Mem Saved` columns.

### Kubernetes Events

The operator emits Kubernetes events on the `AttunePolicy` object.
View them with `kubectl describe attunepolicy <name>` or
`kubectl get events --field-selector involvedObject.kind=AttunePolicy`.

| Event Reason | Type | Description |
|---|---|---|
| `RecommendationsReady` | Normal | First recommendations became available (transitions from 0 to >0 workloads with data) |
| `Resized` | Normal | A container was successfully resized in-place |
| `DecreaseSuppressed` | Normal | A CPU or memory decrease was blocked by `allowDecrease=false` |
| `ScheduleSkipped` | Normal | Resize was skipped because the current time is outside the configured schedule window |
| `ResizeFailed` | Warning | An in-place resize API call failed |
| `BudgetExhausted` | Warning | The per-reconcile resize budget was exhausted before all workloads could be resized |
| `InfeasibleBlocked` | Warning | A resize was blocked because it would exceed node capacity |
| `ResizeSkipped` | Warning | A resize was skipped (e.g. pod in bad state, rolling out) |
| `Reverted` | Warning | A resize was reverted due to safety observation failure (OOMKill, CPU throttle, restarts, or SLO guardrail breach) |
| `Evicted` | Warning | A pod was evicted as a fallback when in-place resize was not possible |
| `StaleRecommendation` | Warning | Recommendations are stale (no fresh Prometheus data) |
| `CooldownActive` | Normal | Resize deferred because the cooldown period has not elapsed |
| `HPAConflict` | Warning | An HPA targets the same workload and may conflict with resizing |
| `VPAConflict` | Warning | A VPA targets the same workload |
| `ConfigClamped` | Warning | A policy field was clamped to its allowed range at runtime |
| `ExportFailed` | Warning | Failed to export recommendations to ConfigMap |
| `RestartOnResize` | Normal | Container will restart on resize due to `RestartContainer` resize policy |
| `MemoryLimitClamped` | Normal | Memory limit decrease skipped due to K8s v1.33 restriction |
| `PolicyConflict` | Warning | Multiple policies target the same workload |
| `RolloutInProgress` | Normal | Resize skipped because the workload is mid-rollout |
| `WorkloadOptOut` | Normal | Workload opted out via annotation |

Events use 1-hour deduplication to prevent log spam. Identical events are emitted at most once per hour; condition changes produce new events immediately. Specific events can be suppressed per-policy using the `attune.io/suppress-warnings` annotation (comma-separated list of event reasons).

---

## AttuneDefaults

**Scope**: Cluster  
**Short name**: `ad`

```yaml
apiVersion: attune.io/v1alpha1
kind: AttuneDefaults
metadata:
  name: default
spec:
  metricsSource:    # same structure as AttunePolicy.spec.metricsSource
    prometheus:
      address: http://prometheus-server.monitoring:80
    historyWindow: 168h
    minimumDataPoints: 48
    queryStep: 5m
    rateWindow: 5m
  cpu:              # same structure as AttunePolicy.spec.cpu
    percentile: 95
    overhead: "20"
    controlledValues: RequestsAndLimits
  memory:           # same structure as AttunePolicy.spec.memory
    percentile: 99
    overhead: "30"
    controlledValues: RequestsAndLimits
    allowDecrease: false
  updateStrategy:   # same structure as AttunePolicy.spec.updateStrategy
    type: Recommend
    cooldown: 1h
    autoRevert: true
  costPricing:      # optional, for EstimatedMonthlySavings computation
    cpuPerCoreHour: "0.031"     # USD per vCPU-hour (default: $0.031)
    memoryPerGiBHour: "0.004"   # USD per GiB-hour (default: $0.004)
```

AttuneDefaults fields are merged into every AttunePolicy at
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

`AttuneDefaults` and `AttuneNamespaceDefaults` both have validating
webhooks that reject invalid `costPricing`, schedule, and Prometheus
address values. If `cpuPerCoreHour` or `memoryPerGiBHour` is set, the
webhook validates that each is a parseable positive float. Invalid values
(e.g., `"banana"`, `"-0.5"`), invalid schedule settings, and blocked
Prometheus addresses are rejected at admission time.

---

## AttuneNamespaceDefaults

**Scope**: Namespaced
**Short name**: `and`

```yaml
apiVersion: attune.io/v1alpha1
kind: AttuneNamespaceDefaults
metadata:
  name: production-defaults
  namespace: production
spec:
  # Same fields as AttuneDefaults.spec
  metricsSource:
    prometheus:
      address: http://prometheus-server.monitoring:80
  cpu:
    percentile: 99
    overhead: "30"
  memory:
    percentile: 99
    overhead: "50"
    allowDecrease: false
  updateStrategy:
    type: Canary
    cooldown: 2h
    autoRevert: true
```

AttuneNamespaceDefaults provides per-namespace defaults that override
cluster-scoped AttuneDefaults. This enables different configurations
for different environments (e.g., conservative settings for production,
aggressive settings for staging).

**Resolution order**: policy spec first, then one defaults source.

If a namespace has an AttuneNamespaceDefaults, the controller uses it
instead of the cluster-scoped AttuneDefaults for all policies in that
namespace. Fields not specified in the namespace defaults are not inherited
from cluster defaults; they fall back to the operator's built-in defaults.

If multiple defaults objects exist at the same scope, selection is
deterministic: the controller uses the object with the lexicographically
smallest `metadata.name`.
