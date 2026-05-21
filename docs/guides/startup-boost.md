# Startup Boost

Some applications need significantly more CPU during startup than at steady
state. JVMs perform class loading and JIT compilation, ML models load weights
into memory, and interpreted runtimes compile bytecode. Rather than
over-provisioning permanently, StartupBoost applies a temporary CPU multiplier
that expires after a configurable duration.

## How it works

1. The operator detects a newly created or restarted pod (within the boost
   duration window from container start time).
2. It resizes the pod's CPU request to `recommended_cpu * multiplier`.
3. Once the duration expires (or the container reaches Ready, whichever comes
   first), the operator resizes back to the steady-state recommendation.
4. If the boosted CPU would exceed the container's CPU limit or the node's
   allocatable CPU, the boost is capped automatically.

## Configuration

Add `startupBoost` to the CPU resource config:

```yaml
apiVersion: rightsize.io/v1alpha1
kind: RightSizePolicy
metadata:
  name: jvm-api
  namespace: production
spec:
  targetRef:
    kind: Deployment
    name: jvm-api
  metricsSource:
    prometheus:
      address: http://prometheus-server.monitoring:80
  cpu:
    percentile: 95
    safetyMargin: "1.2"
    startupBoost:
      multiplier: "3.0"   # 3x the steady-state recommendation
      duration: 2m         # boost expires 2 minutes after pod creation
    bounds:
      min: "100m"
      max: "8000m"
  memory:
    percentile: 99
    safetyMargin: "1.3"
  updateStrategy:
    mode: Auto
    cooldown: 1h
```

### Parameters

| Field | Type | Constraints | Description |
|-------|------|-------------|-------------|
| `multiplier` | string | > 1.0, <= 10.0 | Scales the recommended CPU request during startup |
| `duration` | Duration | >= 10s, <= 1h | Maximum time the boost remains active |

### Choosing a multiplier

| Workload type | Typical multiplier | Typical duration |
|---------------|-------------------|------------------|
| Spring Boot / JVM | 2.0 - 3.0 | 1m - 3m |
| .NET / ASP.NET Core | 1.5 - 2.0 | 30s - 1m |
| ML model loading (PyTorch, TensorFlow) | 3.0 - 5.0 | 2m - 5m |
| Node.js / Python (light init) | 1.5 | 30s |

Start conservative (2.0x, 2m) and increase if startup is still slow. Check
container startup time in your monitoring to calibrate duration.

## Interaction with bounds

The boost is applied **after** bounds clamping. If the boosted value exceeds
the configured `bounds.max`, it is capped at `bounds.max`. Set `bounds.max`
high enough to accommodate the boosted value if you want the full multiplier
effect.

For example, with a 500m recommendation, 3.0x multiplier, and bounds.max of
1000m, the boosted request will be 1000m (capped), not 1500m.

## Monitoring

The operator tracks startup boost activity through the metric:

```
kube_rightsize_startup_boost_total
```

This counter increments each time a startup boost is applied. Use it to
track how often boosts fire and whether the duration is calibrated correctly
(if boosts expire before Ready, duration may be too short).

## Limitations

- StartupBoost only applies to **CPU**. Memory startup spikes are handled
  by the standard safety margin and percentile configuration.
- The boost requires the operator to be running when pods start. If the
  operator is down during a deployment, pods start with their current
  requests and receive the boost on the next reconcile (if still within
  the duration window).
- Only applies when the operator is in a resize-capable mode (Auto,
  OneShot, or Canary). In Recommend mode, the boost is computed in the
  recommendation but not applied.

See [`examples/14-startup-boost.yaml`](https://github.com/SebTardifLabs/kube-rightsize/blob/main/examples/14-startup-boost.yaml) for a complete example.
