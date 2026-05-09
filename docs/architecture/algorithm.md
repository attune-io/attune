The recommendation engine is a chain of five composable estimators. Each
estimator wraps the previous one, processing the result before passing it
along.

## Chain overview

```mermaid
flowchart LR
    A["1. Percentile"] --> B["2. Margin"]
    B --> C["3. Confidence"]
    C --> D["4. Bounds"]
    D --> E["5. Change Filter"]
```

The chain is constructed in `recommendation.NewEngine()` and invoked via
`Recommend(profile, current)`.

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

Typical values: `1.2` (20% headroom) for CPU, `1.3` (30%) for memory.
The multiplication is done in millicore precision and rounded up.

## 3. Confidence Estimator

Widens the recommendation when data confidence is low. High-confidence
recommendations (near 1.0) pass through with minimal adjustment. Low
confidence inflates the result to be conservative.

**Formula**:

```
result = inner * (1 + multiplier / max(confidence, 0.1)) ^ exponent
```

| Parameter | Default | Effect |
|-----------|---------|--------|
| `multiplier` | 1.0 | Controls inflation magnitude |
| `exponent` | 2.0 | Controls curve steepness |

Confidence is floored at 0.1 to prevent division by zero.

**Example**: with confidence = 0.5, multiplier = 1.0, exponent = 2.0:

```
factor = (1 + 1.0/0.5)^2 = 3.0^2 = 9.0
```

This means a low-confidence recommendation is inflated 9x, resulting in a
very conservative (high) value that avoids under-provisioning.

### How confidence is computed

Confidence is derived from two components in `metrics.BuildProfile()`:

```
timeComponent = timeSpanDays
dataComponent = sqrt(dataPoints / 24)
confidence    = clamp(min(timeComponent, dataComponent) / 7, 0, 1)
```

A full 7 days of hourly data (168 points) yields confidence near 1.0.

## 4. Bounds Estimator

Clamps the result to user-defined minimum and maximum values:

```
result = clamp(inner, min, max)
```

This ensures recommendations never drop below a safe floor (e.g. `50m` CPU)
or exceed a known capacity ceiling (e.g. `4000m` CPU).

## 5. Change Filter

Prevents thrashing from tiny adjustments and dangerous large swings:

```
changePct = abs(recommended - current) / current * 100

if changePct < MinChangePercent:
    return current            # suppress noise

if changePct > MaxChangePercent:
    return current +/- (current * MaxChangePercent / 100)  # cap
```

| Parameter | Default | Purpose |
|-----------|---------|---------|
| `MinChangePercent` | 10% | Ignore changes below this threshold |
| `MaxChangePercent` | 50% (CPU) / 30% (memory) | Cap changes above this threshold |

## Burst detection

`BuildProfile()` flags bursts when `max > 3x p95`. The `BurstDetected` flag
and `BurstMagnitude` ratio are available on the `UsageProfile` for
downstream consumers to react to spiky workloads.

## Full pipeline example

Given: p95 CPU = 200m, safety margin = 1.2, confidence = 0.8,
bounds = [50m, 4000m], current = 500m, max change = 50%.

| Stage | Calculation | Result |
|-------|-------------|--------|
| Percentile | max across hourly p95 | 200m |
| Margin | 200m * 1.2 | 240m |
| Confidence | 240m * (1 + 1/0.8)^2 = 240m * 5.0625 | 1215m |
| Bounds | clamp(1215m, 50m, 4000m) | 1215m |
| Change Filter | change = abs(1215-500)/500 = 143% > 50%, cap | 750m |

Final recommendation: **750m** (capped at 50% increase from 500m).
