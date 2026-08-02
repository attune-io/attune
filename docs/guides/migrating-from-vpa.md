# Migrating from VPA

This guide walks you through replacing the Kubernetes Vertical Pod Autoscaler
(VPA) with attune. The migration can be done workload-by-workload
with no downtime.

VPA is gaining in-place-capable update modes in the ecosystem. That narrows the
gap on **apply**, but does not replace a production unattended loop. Attune still
adds startup boost, canary fraction control, SLO PromQL revert, HPA-aware
coexistence, and a graduated Observe → Recommend → Canary → Auto path. See
[Why Attune](../why-attune.md) for the full product narrative.

## VPA vs Attune modes

| VPA Mode | Attune Equivalent | Notes |
|----------|---------------------------|-------|
| `Off` | **Observe** or **Recommend** | Observe: data collection only. Recommend: collect and write recommendations to status |
| `Initial` | **OneShot** | Set resources once; Attune uses in-place resize instead of restart |
| `Auto` (with eviction) | **Canary** or **Auto** | In-place first; add `resizeMethod: InPlaceOrRecreate` if you want eviction fallback instead of skipping infeasible pods |
| `Recommend` (UpdateMode=Off) | **Recommend** | Write recommendations to status without acting |
| In-place-capable VPA modes (where available) | **Canary** then **Auto** | Shared `/resize` primitive; Attune still owns canary, SLO revert, and startup boost |

## Feature matrix (beyond apply)

| Capability | Typical VPA | Attune |
|------------|-------------|--------|
| In-place resize via `/resize` | Where supported by VPA update mode and cluster version | Default path (`InPlaceOnly`) |
| Eviction / recreate fallback | Classic Auto path | Optional `InPlaceOrRecreate` |
| Canary fraction + observation | No first-class canary % | **Canary** mode with optional auto-promote |
| Application SLO revert | Infra signals mainly | **SLO guardrails** (PromQL thresholds) + OOM/throttle/restart/NotReady |
| Startup / cold-start headroom | No | **Startup boost** then scale-back |
| HPA on the same metric | Documented conflict risk | Coexistence design (base requests, not HPA %) |
| Graduated rollout | Off / Initial / Auto | Observe, Recommend, OneShot, Canary, Auto |

## Step-by-step migration

### 1. Install Attune alongside VPA

Both can run in the same cluster. Install Attune per the
[Installation guide](../getting-started/installation.md).

### 2. Create an AttunePolicy in Recommend mode

For each VPA object, create a matching AttunePolicy. Map the VPA config
to AttunePolicy fields:

| VPA field | AttunePolicy field |
|-----------|----------------------|
| `targetRef` | `spec.targetRef` (same structure) |
| `resourcePolicy.containerPolicies[].minAllowed` | `spec.cpu.minAllowed`, `spec.memory.minAllowed` |
| `resourcePolicy.containerPolicies[].maxAllowed` | `spec.cpu.maxAllowed`, `spec.memory.maxAllowed` |
| `resourcePolicy.containerPolicies[].controlledValues` | `spec.cpu.controlledValues`, `spec.memory.controlledValues` |
| `updatePolicy.updateMode` | `spec.updateStrategy.type` |

Example VPA:

```yaml
apiVersion: autoscaling.k8s.io/v1
kind: VerticalPodAutoscaler
metadata:
  name: my-app
spec:
  targetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: my-app
  resourcePolicy:
    containerPolicies:
      - containerName: "*"
        minAllowed:
          cpu: 100m
          memory: 128Mi
        maxAllowed:
          cpu: 4
          memory: 8Gi
  updatePolicy:
    updateMode: "Auto"
```

Equivalent AttunePolicy:

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
  metricsSource:
    prometheus:
      address: http://prometheus-server.monitoring:80
    historyWindow: 168h
  cpu:
    percentile: 95
    overhead: "20"
    minAllowed: "100m"
    maxAllowed: "4000m"
  memory:
    percentile: 99
    overhead: "30"
    minAllowed: "128Mi"
    maxAllowed: "8Gi"
    allowDecrease: false
  updateStrategy:
    type: Recommend
    cooldown: 1h
    autoRevert: true
```

### 3. Compare recommendations

Let both run for at least one full `historyWindow` period (default 7 days).
Compare the VPA recommendations with Attune recommendations:

```bash
kubectl get vpa my-app -o jsonpath='{.status.recommendation}' | jq .
kubectl get attunepolicy my-app -o jsonpath='{.status.recommendations}' | jq .
```

### 4. Disable VPA for the workload

Set the VPA to `Off` mode or delete it:

```bash
kubectl delete vpa my-app
```

### 5. Promote Attune to Canary

```bash
kubectl patch attunepolicy my-app --type merge \
  -p '{"spec":{"updateStrategy":{"type":"Canary","canary":{"percentage":10,"observationPeriod":"30m"}}}}'
```

### 6. Remove VPA entirely

Once all workloads are migrated:

```bash
helm uninstall vpa -n kube-system
kubectl delete crd verticalpodautoscalers.autoscaling.k8s.io \
  verticalpodautoscalercheckpoints.autoscaling.k8s.io
```

!!! warning
    Do not run both VPA (in Auto/Initial mode) and Attune (in
    Canary/Auto mode) on the same workload. The conflict detector will
    warn you, but running both can cause competing resize operations.
