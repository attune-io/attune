The recommendation engine is a chain of six composable estimators. Each
estimator wraps the previous one, processing the result before passing it
along.

## Chain overview

```mermaid
flowchart LR
    A["1. Percentile"] --> B["2. Margin"]
    B --> C["3. Burst"]
    C --> D["4. Confidence"]
    D --> E["5. Bounds"]
    E --> F["6. Change Filter"]
```

The chain is constructed in `recommendation.NewEngine()` and invoked via
`Recommend(profile, current)`.

## Metrics input (before the chain)

Prometheus queries feed the profile builder. By default,
`metricsSource.podAggregation` is **Max** (`max by (container)`), so each
container name contributes one series (the hottest pod) before percentiles
run. Set `podAggregation: None` or `Avg` only when you need the legacy
multi-pod sample pool. Caps such as `maxPodsInMetricsQuery` and
`maxProfileSamples` further bound query and memory cost; see the
[scaling guide](../guides/scaling.md).

When `cpu.startupBoost.excludeFromHistory` is true, the Prometheus CPU
query is `max by (pod, container)` so each pod and container is one series
before the series cap. cAdvisor per-core series collapse the same way
`max by (container)` does today. The window starts at
`attune.io/startup-boost-at` when that stamp is set and not before
creation, otherwise at `CreationTimestamp`. CPU points from that start
until start plus `startupBoost.duration` plus the rate window are removed,
then the surviving pod series are reduced with `podAggregation` (Max, Avg,
or None). A point at the cutoff stays. Nil and false keep today's
percentile. Memory samples are not filtered. Deleted pods stay until
`historyWindow`. A recreated pod keeps samples older than its new
`CreationTimestamp`. A series with no pod label is left unfiltered.

### Usage surge (off until set)

`cpu.surge` and `memory.surge` are absent by default. An empty `surge: {}`
turns that resource on. The long window still uses the hourly maximum
below. When the short window is hot enough, Attune feeds the chain the
overall percentile of finite samples inside `surge.window` (default 30m)
at `surge.percentile` (default 99). It does not take the max across
hour-of-day buckets for that short statistic. Confidence stays the long
window's confidence. CPU and memory decide separately. A derived memory
request (`memoryFromCpuRatio`) follows the CPU request and does not switch
the memory sample set. `explanation.<resource>.finalAdjustment` includes
`surge` on the resource that used the short window.

The short window fires only when it drops older finite samples, at least
3 finite samples remain, and at least half of `window / queryStep` are
present when `queryStep` is positive. It fires when that short percentile
is at least `triggerRatio` times the long-window percentile (default
`1.5`), or when the long percentile is 0 and the short percentile is
positive. `minimumDataPoints` still applies to the long window only.

## 1. Percentile Estimator

Selects the configured percentile from the usage profile. Usage data is
bucketed into 24 hourly slots, and the estimator takes the **maximum**
across all hours to ensure peak-hour coverage.

```
result = max(selectPercentile(overallPercentiles),
             max(selectPercentile(hourlyPercentiles[0..23])))
```

Supported percentiles: `50`, `90`, `95`, `99` (default: `95` for CPU, `99`
for memory).

### Time-of-day awareness

The hourly bucketing provides built-in time-of-day awareness. A workload
that peaks at 2 PM will have a high p95 in bucket 14, and that peak
propagates through the `max()` to the final recommendation. This prevents
under-provisioning for workloads with strong diurnal patterns.

## 2. Margin Estimator

Multiplies the inner result by a safety factor to provide headroom:

```
result = inner * factor
```

Typical values: `20` (20% headroom) for CPU, `30` (30%) for memory.
Internally, overhead percentage is converted to a multiplier (`1 + overhead/100`)
and applied in millicore precision, rounded up.

## 3. Burst Estimator

`BuildProfile()` flags bursts when `max > 3x p95`. The burst estimator
uses `BurstMagnitude` to apply a logarithmic overhead boost after the
base overhead and before the confidence adjustment. See the
[Burst detection](#burst-detection) section below for the full formula and
sensitivity tuning.

## 4. Confidence Estimator

Widens the recommendation when data confidence is low. High-confidence
recommendations (near 1.0) pass through with minimal adjustment. Low
confidence inflates the result to be conservative.

**Formula** (see `confidenceFactor` in `internal/recommendation/chain.go`):

```
factor = 1 + multiplier * (1 - confidence) ^ exponent
result = inner * factor
```

Confidence is clamped to the range 0 through 1 before this step. There is
no division, so a confidence of 0 is safe. With the built-in multiplier
and exponent, the factor is 1.0 at confidence 1 and 2.0 at confidence 0.

| Parameter | Default | Effect |
|-----------|---------|--------|
| `multiplier` | 1.0 | Controls inflation magnitude |
| `exponent` | 2.0 | Controls curve steepness |

**Example**: with confidence = 0.5, multiplier = 1.0, exponent = 2.0:

```
factor = 1 + 1.0 * (1 - 0.5)^2 = 1.25
```

A low-confidence recommendation is widened by that factor, not by a
multiple of the reciprocal of confidence.

### How confidence is computed

Confidence is derived from two components in `metrics.BuildProfile()`:

```
timeComponent = timeSpanDays
dataComponent = sqrt(dataPoints / 24)
confidence    = clamp(min(timeComponent, dataComponent) / 7, 0, 1)
```

A full 7-day history window at the default `queryStep: 5m` yields confidence near 1.0.

## 5. Bounds Estimator

Clamps the result to user-defined minimum and maximum values:

```
result = clamp(inner, min, max)
```

This ensures recommendations never drop below a safe floor (e.g. `50m` CPU)
or exceed a known capacity ceiling (e.g. `4000m` CPU).

## 6. Change Filter

Prevents thrashing from tiny adjustments and dangerous large swings:

```
changePct = abs(recommended - current) / current * 100

if changePct < MinChangePercent:
    return current            # suppress noise

if changePct > MaxChangePercent:
    return current +/- (current * MaxChangePercent / 100)  # cap
```

The filter runs after bounds. If keeping the current value, or stopping
at the directional cap, would leave the result outside `[min, max]`, the
bounds win inside the engine and that engine result is clamped back
inside. A request already above an explicit `maxAllowed` is pulled down
in the engine result, including past `maxDecreasePercent`. Memory
`allowDecrease` defaults to false and runs after the engine, so that
memory pull is published only when decrease is enabled. CPU decrease
defaults to true, so the CPU pull is published on this cycle. An omitted
`maxAllowed` has no ceiling.

| Parameter | Default | Purpose |
|-----------|---------|---------|
| `MinChangePercent` | 10% | Ignore changes below this threshold |
| `MaxChangePercent` | 50% (CPU) / 30% (memory) | Cap changes above this threshold |

## Burst detection

`BuildProfile()` flags bursts when `max > 3x p95`. The recommendation engine
uses `BurstMagnitude` to apply a logarithmic overhead boost:

```
burstFactor = 1 + sensitivity * log2(BurstMagnitude)
```

The `sensitivity` defaults to `0.1` and can be configured per resource via
`spec.cpu.burstSensitivity` / `spec.memory.burstSensitivity`. Set to `"0"`
to disable burst boost entirely (useful for batch jobs).

| Burst magnitude | Boost (sensitivity=0.1) | Boost (sensitivity=0.2) |
|-----------------|-------------------------|-------------------------|
| 4x              | +20%                    | +40%                    |
| 8x              | +30%                    | +60%                    |
| 16x             | +40%                    | +80%                    |
| 100x            | +66%                    | +133%                   |

This step runs after the base overhead and before the confidence
adjustment. When no burst is detected (or magnitude <= 1), the factor
is 1.0 (no change). The burst factor is visible in `kubectl attune explain`
output via the `burstFactor` and `afterBurst` fields, and as the
`attune_burst_factor` Prometheus metric.

## Full pipeline example

Given: p95 CPU = 200m, overhead = 20%, confidence = 0.8,
policy bounds = [1m, 4000m] (set on this policy),
current = 500m, max change = 50%. CPU quantities round up to the next millicore.

| Stage | Calculation | Result |
|-------|-------------|--------|
| Percentile | max across hourly p95 | 200m |
| Overhead | 200m * (1 + 20/100) = 200m * 1.2 | 240m |
| Confidence | factor = 1 + (1 - 0.8)^2 = 1.04; ceil(240m * 1.04) | 250m |
| Bounds | 250m is inside the policy bounds [1m, 4000m] | 250m |
| Change Filter | abs(250 - 500) / 500 = 50%, which is not greater than 50% | 250m |

Final recommendation: **250m**. The decrease cap does not move it, because
the confidence step already landed on a 50% decrease.
