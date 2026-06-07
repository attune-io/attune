# Quick Start

This guide walks you through creating an AttunePolicy, reviewing its
recommendations, and promoting to Canary mode, all in about five minutes.

!!! tip "Interactive demo"
    Want to see the full flow on a real cluster first? Run the automated
    demo script that provisions a k3d cluster, deploys a sample workload,
    and shows recommendations appearing in real time:

    ```bash
    ./hack/demo.sh
    ```

    The script cleans up automatically on exit. Use `--no-cleanup` to keep
    the cluster for exploration.

!!! info "Prerequisites"
    - **Kubernetes 1.32+** — 1.32 requires the `InPlacePodVerticalScaling` feature gate; 1.33+ has it enabled by default.
    - **A metrics source** — Prometheus (default), Datadog, or CloudWatch Container Insights. This guide uses Prometheus; see [Datadog Setup](../guides/datadog-setup.md) or [CloudWatch Setup](../guides/cloudwatch-setup.md) for alternatives.
    - **Attune installed** — see [Installation](installation.md) for Helm and raw manifest options.

## 1. Create an AttunePolicy in Recommend mode

Start in **Recommend** mode so that no pods are modified. The operator will
collect metrics and write recommendations to the resource status.

All fields have production-ready defaults (P95 CPU, P99 memory, 20%/30%
overhead, sensible bounds). A minimal policy is just:

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
  cpu: {}
  memory: {}
  metricsSource:
    prometheus:
      address: http://prometheus-server.monitoring:80
```

!!! tip "Skip the Prometheus address on every policy"
    Create a cluster-scoped `AttuneDefaults` resource with the Prometheus
    address and it will apply to all policies. If only one namespace should
    inherit that address, use a namespaced `AttuneNamespaceDefaults`
    instead. The repo includes `examples/05-cluster-defaults.yaml` for a
    cluster-wide setup and `examples/11-namespace-defaults.yaml` for a
    namespace-only setup.
    ```yaml
    apiVersion: attune.io/v1alpha1
    kind: AttuneDefaults
    metadata:
      name: cluster-defaults
    spec:
      metricsSource:
        prometheus:
          address: http://prometheus-server.monitoring:80
    ```
    With this set, your policies only need `targetRef`.

!!! info "Requests only vs requests and limits"
    By default, Attune adjusts **requests only** (`controlledValues:
    RequestsOnly`). If your containers have CPU/memory limits set, the
    recommendation may be capped at the limit value. Set
    `controlledValues: RequestsAndLimits` on the policy if you want Attune
    to scale both. See [Configuration Reference](../reference/configuration.md#available-fields)
    for details.

??? note "Full configuration reference"
    All defaults can be overridden per-policy. See
    [Configuration Reference](../reference/configuration.md) for the complete
    list of fields including `cpu.percentile`, `memory.overhead`,
    `updateStrategy.cooldown`, bounds, and more.

```bash
kubectl apply -f attunepolicy.yaml
```

## 2. Check status

```bash
kubectl get attunepolicy my-app -o wide
```

Right after applying, the policy will be collecting data:

```text
NAME     TYPE        WORKLOADS   RECS   RESIZED   READY   AGE   CPU SAVED   MEM SAVED
my-app   Recommend   1           0      0         False   5m    0           0
```

> **Note:** `READY=False` here means the policy is still in the `InsufficientData`
> phase. Check `.status.conditions` to see the reason and progress message.
>
> `minimumDataPoints` counts Prometheus range-query samples, not hours. With
> the default `queryStep: 5m`, `minimumDataPoints: 48` needs about 4 hours of
> data before recommendations can appear. Lower it for faster evaluation, or
> raise it for more confidence.

!!! tip "Quick evaluation"

    For a faster first look (~1 hour instead of ~4 hours), set
    `minimumDataPoints: 12` in your policy:

    ```yaml
    spec:
      metricsSource:
        prometheus:
          address: http://prometheus-server.monitoring:80
        minimumDataPoints: 12    # ~1 hour at the default queryStep: 5m
    ```

    After validating that recommendations look reasonable, remove
    `minimumDataPoints` to use the default (48) for production accuracy.

> Most defaultable fields are applied by the controller at reconcile time so
> that `AttuneDefaults` and `AttuneNamespaceDefaults` can override them.
> That means omitted fields may still look empty in `kubectl get attunepolicy -o yaml`
> even though the policy is already following the built-in and inherited runtime
> behavior unless you override those fields. Use `kubectl attune explain -n
> <namespace> <policy>` to inspect the effective values for the key
> controller-applied defaults.

After enough data has accumulated:

```text
NAME     TYPE        WORKLOADS   RECS   RESIZED   READY   AGE   CPU SAVED   MEM SAVED
my-app   Recommend   1           1      0         True    7d    200m        256Mi
```

## 3. Review recommendations

```bash
kubectl get attunepolicy my-app -o jsonpath='{.status.recommendations}' | jq .
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
  cpu:
    maxChangePercent: 50
  memory:
    maxChangePercent: 30
  updateStrategy:
    type: Canary
    canary:
      percentage: 10
      observationPeriod: 30m
    cooldown: 2h
    autoRevert: true
```

```bash
kubectl apply -f attunepolicy.yaml
```

## 5. Verify the resize

```bash
kubectl get attunepolicy my-app
```

The `RESIZED` column increments as pods are resized in place. If a safety
violation occurs (OOMKill, excessive restarts, pod NotReady, or SLO guardrail breach), the operator
auto-reverts the affected pods.

!!! tip
    Watch resize events in real time with
    `kubectl get events --field-selector reason=Resized -w`.

## Next steps

- Follow the [First 30 Days](first-30-days.md) guide for a day-by-day
  walkthrough from first install to production Auto mode.
- Read [Concepts](concepts.md) to understand modes, estimators, and safety.
- Set up [Prometheus integration](../guides/prometheus-setup.md) with
  ServiceMonitor, Grafana dashboard, and PrometheusRule alerts.
- Explore the [Canary Rollout guide](../guides/canary-rollout.md) for
  production best practices.
