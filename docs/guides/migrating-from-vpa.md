# Migrating from VPA

This guide walks you through replacing the Kubernetes Vertical Pod Autoscaler
(VPA) with kube-rightsize. The migration can be done workload-by-workload
with no downtime.

## VPA vs kube-rightsize modes

| VPA Mode | kube-rightsize Equivalent | Notes |
|----------|---------------------------|-------|
| `Off` | **Observe** or **Recommend** | Observe: data collection only. Recommend: collect and write recommendations to status |
| `Initial` | **OneShot** | Set resources once; kube-rightsize uses in-place resize instead of restart |
| `Auto` (with eviction) | **Canary** or **Auto** | In-place first; add `resizeMethod: InPlaceOrRecreate` if you want eviction fallback instead of skipping infeasible pods |
| `Recommend` (UpdateMode=Off) | **Recommend** | Write recommendations to status without acting |

## Step-by-step migration

### 1. Install kube-rightsize alongside VPA

Both can run in the same cluster. Install kube-rightsize per the
[Installation guide](../getting-started/installation.md).

### 2. Create a RightSizePolicy in Recommend mode

For each VPA object, create a matching RightSizePolicy. Map the VPA config
to RightSizePolicy fields:

| VPA field | RightSizePolicy field |
|-----------|----------------------|
| `targetRef` | `spec.targetRef` (same structure) |
| `resourcePolicy.containerPolicies[].minAllowed` | `spec.cpu.minAllowed`, `spec.memory.minAllowed` |
| `resourcePolicy.containerPolicies[].maxAllowed` | `spec.cpu.maxAllowed`, `spec.memory.maxAllowed` |
| `resourcePolicy.containerPolicies[].controlledValues` | `spec.cpu.controlledValues`, `spec.memory.controlledValues` |
| `updatePolicy.updateMode` | `spec.updateStrategy.mode` |

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

Equivalent RightSizePolicy:

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
      address: http://prometheus-server.monitoring:80
    historyWindow: 168h
  cpu:
    percentile: 95
    safetyMargin: "1.2"
    minAllowed: "100m"
      maxAllowed: "4000m"
  memory:
    percentile: 99
    safetyMargin: "1.3"
    minAllowed: "128Mi"
      maxAllowed: "8Gi"
    allowDecrease: false
  updateStrategy:
    mode: Recommend
    cooldown: 1h
    autoRevert: true
```

### 3. Compare recommendations

Let both run for at least one full `historyWindow` period (default 7 days).
Compare the VPA recommendations with kube-rightsize recommendations:

```bash
kubectl get vpa my-app -o jsonpath='{.status.recommendation}' | jq .
kubectl get rsp my-app -o jsonpath='{.status.recommendations}' | jq .
```

### 4. Disable VPA for the workload

Set the VPA to `Off` mode or delete it:

```bash
kubectl delete vpa my-app
```

### 5. Promote kube-rightsize to Canary

```bash
kubectl patch rsp my-app --type merge \
  -p '{"spec":{"updateStrategy":{"mode":"Canary","canary":{"percentage":10,"observationPeriod":"30m"}}}}'
```

### 6. Remove VPA entirely

Once all workloads are migrated:

```bash
helm uninstall vpa -n kube-system
kubectl delete crd verticalpodautoscalers.autoscaling.k8s.io \
  verticalpodautoscalercheckpoints.autoscaling.k8s.io
```

!!! warning
    Do not run both VPA (in Auto/Initial mode) and kube-rightsize (in
    Canary/Auto mode) on the same workload. The conflict detector will
    warn you, but running both can cause competing resize operations.
