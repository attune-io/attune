## Common conditions

Check the policy's conditions for a quick diagnosis:

```bash
kubectl get rsp <name> -o jsonpath='{.status.conditions}' | jq .
```

### PrometheusUnavailable

**Symptom**: Ready condition is `False` with reason `PrometheusUnavailable`.

**Cause**: The operator cannot reach the configured Prometheus address.

**Fix**:

```bash
# Verify the Prometheus address in the policy spec
kubectl get rsp <name> -o jsonpath='{.spec.metricsSource.prometheus.address}'

# Test connectivity from the operator pod
kubectl -n kube-rightsize-system exec deploy/kube-rightsize -- \
  wget -qO- http://prometheus-server.monitoring:9090/-/healthy
```

### InsufficientData

**Symptom**: Ready condition is `False` with reason `InsufficientData`.

**Cause**: Not enough Prometheus data points to generate recommendations.
The default minimum is 168 (one week of hourly samples).

**Fix**: Wait for more data to accumulate, or lower `minimumDataPoints` for
faster (but less confident) recommendations:

```yaml
spec:
  metricsSource:
    minimumDataPoints: 48  # ~2 days of hourly data
```

### CooldownActive

**Symptom**: The operator logs "Cooldown active, skipping resize" and no
pods are resized.

**Cause**: A resize was performed recently and the cooldown period has not
elapsed.

**Fix**: Wait for the cooldown to expire, or shorten it:

```bash
kubectl patch rsp <name> --type merge \
  -p '{"spec":{"updateStrategy":{"cooldown":"30m"}}}'
```

## Resize failures

### Infeasible resize

**Symptom**: Resize history shows `result: Failed` and operator logs contain
`resize infeasible`.

**Cause**: The node cannot accommodate the new resource values. Common when
increasing resources on a node that is already at capacity.

**Fix**: Ensure the cluster has sufficient allocatable resources, or tighten
bounds to stay within node capacity:

```yaml
spec:
  cpu:
    bounds:
      max: "2000m"  # reduce max to fit on nodes
```

### QoS class change blocked

**Symptom**: Operator logs `Skipping resize: would change QoS class`.

**Cause**: For Guaranteed-class pods, requests must equal limits. If the
policy would set different values for requests and limits, the resize is
skipped.

**Fix**: Set `controlledValues: RequestsAndLimits` so both are updated
together, or switch to `RequestsOnly` if the pod should be Burstable.

## Revert issues

### High revert rate

**Symptom**: Degraded condition with reason `HighRevertRate`.

**Cause**: Multiple consecutive resizes have been reverted due to safety
violations.

**Fix**: Investigate the revert reasons:

```bash
kubectl get rsp <name> -o jsonpath='{.status.resizeHistory}' | \
  jq '[.[] | select(.result=="Reverted")]'
```

Common causes:

- **oomkill**: safety margin is too low for memory. Increase `memory.safetyMargin`.
- **restart**: the application crashes at the new resource level. Check application logs.
- **notready**: readiness probe fails post-resize. Verify probe configuration.

## Debug commands

Operator logs:

```bash
kubectl -n kube-rightsize-system logs deploy/kube-rightsize --tail=100
```

List all policies with status:

```bash
kubectl get rsp --all-namespaces -o wide
```

Inspect a specific policy in detail:

```bash
kubectl describe rsp <name>
```

Check operator metrics:

```bash
kubectl -n kube-rightsize-system port-forward svc/kube-rightsize 8080:8080 &
curl -s localhost:8080/metrics | grep kube_rightsize
```
