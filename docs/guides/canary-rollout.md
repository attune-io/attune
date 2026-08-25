# Canary Rollout

Canary mode resizes a small percentage of pods first, watches them for safety
violations, and only proceeds to the full fleet after the observation period
passes without issues.

## Configuring canary rollout

```yaml
spec:
  cpu:
    maxChangePercent: 50       # max CPU change per resize cycle
  memory:
    maxChangePercent: 30       # max memory change per resize cycle
  updateStrategy:
    type: Canary
    canary:
      percentage: 10          # resize 10% of pods first
      observationPeriod: 30m  # watch canary pods for 30 minutes
      autoPromote: true       # promote to full fleet automatically
    cooldown: 2h
    autoRevert: true
```

| Field | Description |
|-------|-------------|
| `canary.percentage` | Percentage of eligible pods to resize in the first wave |
| `canary.observationPeriod` | How long the operator monitors canary pods before proceeding |
| `canary.autoPromote` | Automatically promote to full fleet after observation passes without reverts (default: false) |
| `cpu.maxChangePercent` | Maximum CPU change per resize cycle (default 50%) |
| `memory.maxChangePercent` | Maximum memory change per resize cycle (default 30%) |
| `cooldown` | Minimum time between successive resizes of the same workload |
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
4. **Observation**: `status.canary.startTime` is set only after a successful
   in-place canary resize. During `observationPeriod` after that resize, the
   safety monitor checks for OOMKill, restart spikes, pod NotReady, CPU
   throttle, and SLO guardrail breaches. Skipped cycles (budget, node
   pressure, already at target) do not start the clock.
5. **Verdict**: if that app's canary pods remain healthy, **that app**
   is promoted (`status.canary.workloads[].phase=FullRollout`). Other
   apps on the same policy keep watching. Policy `status.canary.phase`
   becomes `FullRollout` only when every listed app has been promoted.
   A revert on one app resets that app's clock only.
6. **Isolation**: while an app is still in canary, CREATE initial sizing,
   startup boost, and HPA retune do not apply to the rest of that app
   (or to other apps). New pods stay at their template size until that
   app is promoted (or the pod is already in `status.canary.workloads[].pods`).
   Selector-based policies are included: the CREATE webhook fetches the
   owning Deployment/StatefulSet/DaemonSet and matches `targetRef.selector`.
7. **Cooldown**: the operator waits for that workload's `cooldown` before
   resizing it again. Other apps on the policy are not locked.

## Monitoring canary pods

The operator tracks which pods were selected for the canary subset in
`status.canary.pods` and per app in `status.canary.workloads`. The
`kubectl attune status` CANARY column shows `CanaryInProgress (1/2 apps)`
when some apps are promoted and others are still watching. List the
per-app rows:

```bash
kubectl get attunepolicy my-app -o jsonpath='{.status.canary.workloads}' | jq .
```

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
kubectl get attunepolicy my-app -o jsonpath='{.status.resizeHistory}' | jq '.[] | select(.result=="Reverted")'
```

!!! warning
    If you see repeated reverts, review the `reason` field (oomkill, restart, throttle, slo:&lt;name&gt;,
    notready) and consider increasing the overhead or adjusting bounds
    before retrying.

## Promoting from canary to full fleet

### Automatic promotion

When `autoPromote: true`, the operator handles promotion automatically:

1. After the canary pods pass the observation period measured from the
   successful in-place resize, with zero reverts, the operator sets
   `status.canary.phase: FullRollout`.
2. On the next reconcile, all eligible pods are resized (same as Auto mode).
3. If any revert occurs during observation, promotion is blocked, the
   observation clock is cleared, and a new watch starts after the next
   successful in-place resize. The operator continues resizing only the
   canary subset until that new watch passes.

Check the canary phase:

```bash
kubectl get attunepolicy my-app -o jsonpath='{.status.canary.phase}'
# CanaryInProgress -> FullRollout
```

**Spec change resets the canary cycle.** If you edit the policy spec
(e.g., change `percentile` or `overhead`) while a canary cycle is in
progress or in `FullRollout`, the operator resets the observation timer.
The new configuration is re-validated from scratch before promotion.

### Manual promotion

When `autoPromote` is false (default), promote to **Auto** mode manually
after canary pods have run successfully through multiple cooldown cycles:

```bash
kubectl patch attunepolicy my-app --type merge \
  -p '{"spec":{"updateStrategy":{"type":"Auto"}}}'
```

Or increase the canary percentage gradually:

```bash
kubectl patch attunepolicy my-app --type merge \
  -p '{"spec":{"updateStrategy":{"canary":{"percentage":50}}}}'
```

## Rollback

To stop all resizing immediately, switch back to Recommend mode:

```bash
kubectl patch attunepolicy my-app --type merge \
  -p '{"spec":{"updateStrategy":{"type":"Recommend"}}}'
```

!!! tip
    Existing pod resources are not reverted when you change modes. Pods keep
    their current allocations; only future resize operations are affected.
