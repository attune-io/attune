Canary mode resizes a small percentage of pods first, watches them for safety
violations, and only proceeds to the full fleet after the observation period
passes without issues.

## Configuring canary rollout

```yaml
spec:
  updateStrategy:
    mode: Canary
    canary:
      percentage: 10          # resize 10% of pods first
      observationPeriod: 30m  # watch canary pods for 30 minutes
    maxCpuChangePercent: 50
    maxMemoryChangePercent: 30
    cooldown: 2h
    autoRevert: true
```

| Field | Description |
|-------|-------------|
| `canary.percentage` | Percentage of eligible pods to resize in the first wave |
| `canary.observationPeriod` | How long the operator monitors canary pods before proceeding |
| `maxCpuChangePercent` | Maximum CPU change per resize cycle (default 50%) |
| `maxMemoryChangePercent` | Maximum memory change per resize cycle (default 30%) |
| `cooldown` | Minimum time between successive resize operations |
| `autoRevert` | Automatically restore original resources on safety violation |

!!! note
    At least one pod is always selected, even if `percentage` would calculate
    to zero. For a 3-replica Deployment with `percentage: 10`, one pod is
    resized.

## Step-by-step process

1. **Recommendations computed**: the estimator chain produces per-container
   targets based on Prometheus data.
2. **Canary selection**: the operator picks `ceil(percentage * eligible / 100)`
   pods. Only running pods without an active resize or pending deletion qualify.
3. **In-place resize**: the operator calls `UpdateResize` on each selected pod.
4. **Observation**: during `observationPeriod`, the safety monitor checks for
   OOMKill, restart spikes, and pod NotReady.
5. **Verdict**: if all canary pods remain healthy, the resize is considered
   successful. If any violation is detected, the operator auto-reverts.
6. **Cooldown**: the operator waits for the `cooldown` duration before the
   next reconciliation cycle.

## Monitoring canary pods

Watch resize events:

```bash
kubectl get events --field-selector reason=Resized -w
```

Check which pods have been resized:

```bash
kubectl get pods -l app=my-app -o custom-columns=\
NAME:.metadata.name,\
CPU_REQ:.spec.containers[0].resources.requests.cpu,\
MEM_REQ:.spec.containers[0].resources.requests.memory
```

## Handling auto-revert

When the safety monitor detects a problem, it reverts the pod's resources
and records the event in `.status.resizeHistory` with `result: Reverted`.

```bash
kubectl get rsp my-app -o jsonpath='{.status.resizeHistory}' | jq '.[] | select(.result=="Reverted")'
```

!!! warning
    If you see repeated reverts, review the `reason` field (oomkill, restart,
    notready) and consider increasing the safety margin or adjusting bounds
    before retrying.

## Promoting from canary to full fleet

Once canary pods have run successfully through multiple cooldown cycles,
promote to **Auto** mode to resize all eligible pods:

```bash
kubectl patch rsp my-app --type merge \
  -p '{"spec":{"updateStrategy":{"mode":"Auto"}}}'
```

Or increase the canary percentage gradually:

```bash
kubectl patch rsp my-app --type merge \
  -p '{"spec":{"updateStrategy":{"canary":{"percentage":50}}}}'
```

## Rollback

To stop all resizing immediately, switch back to Recommend mode:

```bash
kubectl patch rsp my-app --type merge \
  -p '{"spec":{"updateStrategy":{"mode":"Recommend"}}}'
```

!!! tip
    Existing pod resources are not reverted when you change modes. Pods keep
    their current allocations; only future resize operations are affected.
