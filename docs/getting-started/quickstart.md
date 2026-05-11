This guide walks you through creating a RightSizePolicy, reviewing its
recommendations, and promoting to Canary mode, all in about five minutes.

!!! info "Prerequisites"
    Make sure the operator is installed before proceeding. See
    [Installation](installation.md) for Helm and raw manifest options.

## 1. Create a RightSizePolicy in Recommend mode

Start in **Recommend** mode so that no pods are modified. The operator will
collect metrics and write recommendations to the resource status.

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
      address: http://prometheus-server.monitoring:9090
    historyWindow: 168h
    minimumDataPoints: 168
  cpu:
    percentile: 95
    safetyMargin: "1.2"
    bounds:
      min: "50m"
      max: "4000m"
  memory:
    percentile: 99
    safetyMargin: "1.3"
    bounds:
      min: "64Mi"
      max: "8Gi"
    allowDecrease: false
  updateStrategy:
    mode: Recommend
    cooldown: 1h
```

```bash
kubectl apply -f rightsizepolicy.yaml
```

## 2. Check status

```bash
kubectl get rightsizepolicy my-app -o wide
```

Right after applying, the policy will be collecting data:

```text
NAME     MODE        WORKLOADS   RESIZED   READY              AGE
my-app   Recommend   1           0         InsufficientData   5m
```

> **Note:** With the default `minimumDataPoints: 168`, the operator needs ~7 days of
> hourly Prometheus samples before generating recommendations. To see results faster
> during evaluation, set `minimumDataPoints: 24` (1 day of data).

After enough data has accumulated:

```text
NAME     MODE        WORKLOADS   RESIZED   READY   AGE   CPU SAVED   MEM SAVED
my-app   Recommend   1           0         True    7d    200m        256Mi
```

## 3. Review recommendations

```bash
kubectl get rightsizepolicy my-app -o jsonpath='{.status.recommendations}' | jq .
```

The output shows per-container current and recommended values along with the
confidence score and data-point count. See
[Recommend Mode](../guides/recommend-mode.md) for details on interpreting
these fields.

## 4. Upgrade to Canary mode

Once you are happy with the recommendations, switch the policy to **Canary**
mode. This resizes a percentage of pods first, observes them, and only
proceeds if they remain healthy.

```yaml
spec:
  updateStrategy:
    mode: Canary
    canary:
      percentage: 10
      observationPeriod: 30m
    maxCpuChangePercent: 50
    maxMemoryChangePercent: 30
    cooldown: 2h
    autoRevert: true
```

```bash
kubectl apply -f rightsizepolicy.yaml
```

## 5. Verify the resize

```bash
kubectl get rightsizepolicy my-app
```

The `RESIZED` column increments as pods are resized in place. If a safety
violation occurs (OOMKill, excessive restarts, pod NotReady), the operator
auto-reverts the affected pods.

!!! tip
    Watch resize events in real time with
    `kubectl get events --field-selector reason=Resized -w`.

## Next steps

- Read [Concepts](concepts.md) to understand modes, estimators, and safety.
- Explore the [Canary Rollout guide](../guides/canary-rollout.md) for
  production best practices.
