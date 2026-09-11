# Startup Boost

Some applications need significantly more CPU during startup than at steady
state. JVMs perform class loading and JIT compilation, ML models load weights
into memory, and interpreted runtimes compile bytecode. Rather than
over-provisioning permanently, StartupBoost applies a temporary CPU multiplier
that expires after a configurable duration.

## How it works

Startup boost has two apply paths.

**CREATE webhook.** When a matching policy has a recommendation and
`initialSizing` is on, the mutating webhook writes
`recommended_cpu * multiplier` onto the new pod. Recommend mode
CREATE is limited to CronJob and Job owners (those pods cannot be
resized in place). In `RequestsAndLimits` mode the webhook also
raises the CPU dest with the boosted request so Guaranteed pods
still get headroom.

**In-place resize.** After the pod is running, the operator detects a
newly created pod whose age (`now` minus `pod.CreationTimestamp`) is
still within `startupBoost.duration`. Container start time is not used.
It resizes each eligible container's CPU request to
`recommended_cpu * multiplier`. Native sidecars (init containers with
`restartPolicy: Always`) are included. Known sidecar names such as
`istio-proxy` stay excluded via `EffectiveExcludedContainers`.
`RequestsAndLimits` raises dest with the boosted request the same way
CREATE does.

After a successful apply, the operator writes
`attune.io/startup-boost-at`. The boost expires when that timestamp
plus `duration` elapses and dest returns to the steady-state
recommendation. Container Ready is not checked.

If the boosted CPU would exceed `maxAllowed` or the node's allocatable
CPU, the boost is capped. `RequestsOnly` also dest-caps leftover dest
and does not raise dest.

### Native sidecars

Init containers with `restartPolicy: Always` follow the same apply and
expiry path as regular containers. Known sidecar names (`istio-proxy`,
and the rest of the built-in list) remain auto-excluded unless you set
`excludeKnownSidecars: false`. See
[Istio integration](istio-integration.md#native-sidecar-mode).

## Configuration

Add `startupBoost` to the CPU resource config:

```yaml
apiVersion: attune.io/v1alpha1
kind: AttunePolicy
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
    overhead: "20"
    startupBoost:
      multiplier: "3.0"   # 3x the steady-state recommendation
      duration: 2m         # boost expires 2 minutes after pod creation
    minAllowed: "100m"
    maxAllowed: "8000m"
  memory:
    percentile: 99
    overhead: "30"
  updateStrategy:
    type: Auto
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
the configured `maxAllowed`, it is capped at `maxAllowed`. Set `maxAllowed`
high enough to accommodate the boosted value if you want the full multiplier
effect. With `cpu.controlledValues: RequestsAndLimits`, dest is raised
with the boosted request so Guaranteed pods keep request equal to dest
and still receive headroom. `RequestsOnly` dest-caps leftover dest and
does not raise dest.

For example, with a 500m recommendation, 3.0x multiplier, and maxAllowed of
1000m, the boosted request will be 1000m (capped), not 1500m.

## Monitoring

The operator tracks startup boost activity through the metric:

```
attune_startup_boost_total
```

This counter increments each time a startup boost is applied. Use it to
track how often boosts fire and whether `duration` covers the startup
window (expiry is `attune.io/startup-boost-at` plus `duration`, not
container Ready).

The pre-built [Grafana dashboard](https://github.com/attune-io/attune/blob/main/deploy/grafana/dashboard.json)
includes a Startup Boost panel that visualizes this metric.

## Limitations

- StartupBoost only applies to **CPU**. Memory startup spikes are handled
  by the standard overhead and percentile configuration.
- The boost requires the operator to be running when pods start. If the
  operator is down during a deployment, pods start with their current
  requests and receive the boost on the next reconcile (if still within
  the duration window).
- In-place boost requires a resize-capable mode (Auto, OneShot, or
  Canary). CREATE webhook boost also applies in Recommend mode when
  the pod owner is a CronJob or Job, because those pods cannot be
  resized in place. Observe mode never applies a boost.

See [`examples/14-startup-boost.yaml`](https://github.com/attune-io/attune/blob/main/examples/14-startup-boost.yaml) for a complete example.
