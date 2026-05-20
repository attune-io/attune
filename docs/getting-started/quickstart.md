# Quick Start

This guide walks you through creating a RightSizePolicy, reviewing its
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
    - **Kubernetes 1.33+** — required for In-Place Pod Resize (GA). Older clusters are not supported.
    - **Prometheus** — the operator queries `container_cpu_usage_seconds_total` and `container_memory_working_set_bytes`.
    - **kube-rightsize installed** — see [Installation](installation.md) for Helm and raw manifest options.

## 1. Create a RightSizePolicy in Recommend mode

Start in **Recommend** mode so that no pods are modified. The operator will
collect metrics and write recommendations to the resource status.

All fields have production-ready defaults (P95 CPU, P99 memory, 1.2x/1.3x
safety margins, sensible bounds). A minimal policy is just:

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
  cpu: {}
  memory: {}
  metricsSource:
    prometheus:
      address: http://prometheus-server.monitoring:80
```

!!! tip "Skip the Prometheus address on every policy"
    Create a cluster-scoped `RightSizeDefaults` resource with the Prometheus
    address and it will apply to all policies. If only one namespace should
    inherit that address, use a namespaced `RightSizeNamespaceDefaults`
    instead. The repo includes `examples/05-cluster-defaults.yaml` for a
    cluster-wide setup and `examples/11-namespace-defaults.yaml` for a
    namespace-only setup.
    ```yaml
    apiVersion: rightsize.io/v1alpha1
    kind: RightSizeDefaults
    metadata:
      name: cluster-defaults
    spec:
      metricsSource:
        prometheus:
          address: http://prometheus-server.monitoring:80
    ```
    With this set, your policies only need `targetRef`.

??? note "Full configuration reference"
    All defaults can be overridden per-policy. See
    [Configuration Reference](../reference/configuration.md) for the complete
    list of fields including `cpu.percentile`, `memory.safetyMargin`,
    `updateStrategy.cooldown`, bounds, and more.

```bash
kubectl apply -f rightsizepolicy.yaml
```

## 2. Check status

```bash
kubectl get rightsizepolicy my-app -o wide
```

Right after applying, the policy will be collecting data:

```text
NAME     MODE        WORKLOADS   RECS   RESIZED   READY   AGE   CPU SAVED   MEM SAVED
my-app   Recommend   1           0      0         False   5m    0           0
```

> **Note:** `READY=False` here means the policy is still in the `InsufficientData`
> phase. Check `.status.conditions` to see the reason and progress message.
>
> `minimumDataPoints` counts Prometheus range-query samples, not hours. With
> the default `queryStep: 5m`, `minimumDataPoints: 48` needs about 4 hours of
> data before recommendations can appear. Lower it for faster evaluation, or
> raise it for more confidence.
>
> Most defaultable fields are applied by the controller at reconcile time so
> that `RightSizeDefaults` and `RightSizeNamespaceDefaults` can override them.
> That means omitted fields may still look empty in `kubectl get rsp -o yaml`
> even though the effective values are already in use. Use
> `kubectl rightsize explain <policy>` to inspect those effective values.

After enough data has accumulated:

```text
NAME     MODE        WORKLOADS   RECS   RESIZED   READY   AGE   CPU SAVED   MEM SAVED
my-app   Recommend   1           1      0         True    7d    200m        256Mi
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
