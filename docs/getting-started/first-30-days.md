# Your First 30 Days with kube-rightsize

This guide walks you through what to expect after installing kube-rightsize,
from the initial data collection to full automation. Each phase builds
confidence before moving to the next.

## Day 0: Install and create your first policy

After [installing the operator](installation.md), create a
[RightSizePolicy](../reference/api.md) targeting a non-critical workload:

```yaml
apiVersion: rightsize.io/v1alpha1
kind: RightSizePolicy
metadata:
  name: my-first-policy
  namespace: my-app
spec:
  targetRef:
    kind: Deployment
    name: my-api
  metricsSource:
    prometheus:
      address: http://prometheus-server.monitoring:80
```

Verify the operator is running and picked up your policy:

```bash
kubectl rightsize status -n my-app
```

You should see your policy with **Ready** showing `InsufficientData` or a
progress message like `Collecting data: 0/48 data points (0%)`. This is
normal.

!!! tip "Verify operator config"
    Check the operator logs to confirm it started with the right settings:
    ```bash
    kubectl logs -n kube-rightsize-system deploy/kube-rightsize-controller-manager | head -5
    ```
    The first line shows all configured parameters (Prometheus QPS,
    watch namespaces, webhooks, etc.).

## Hours 0-4: Data collection

The operator queries Prometheus every 5 minutes (default `queryStep`) and
needs 48 data points (default `minimumDataPoints`) before generating
recommendations. This takes roughly **4 hours**.

Watch progress live:

```bash
kubectl rightsize status -w -n my-app
```

What to check during this phase:

- **Ready shows `NoWorkloadsFound`?** Your `targetRef.name` or `kind` doesn't
  match any workload. Check spelling and namespace.
- **Ready shows `PrometheusUnavailable`?** The operator can't reach your
  Prometheus instance. Verify the address and network policy.
- **Progress percentage is climbing?** Everything is working. Wait for it
  to reach 100%.

!!! tip "Quick evaluation"
    For a faster first look (~1 hour), set `minimumDataPoints: 12` in your
    policy. See the [quickstart](quickstart.md) for details. Remove it
    before going to production.

## Day 1: First recommendations

Once data collection completes, the policy transitions to
**Ready=Monitoring** and recommendations appear:

```bash
kubectl rightsize recommendations -n my-app
```

This shows current vs recommended resource values with a confidence score.
Low confidence (< 70%) means the operator hasn't seen enough variance yet;
it will improve over the coming days.

To understand **why** a specific recommendation was made:

```bash
kubectl rightsize explain my-first-policy -n my-app
```

This traces the full recommendation pipeline: raw percentile, safety
margin, confidence adjustment, bounds clamping, and change filter.

## Days 1-7: Build trust in Recommend mode

During the first week, check recommendations daily:

```bash
kubectl rightsize recommendations -n my-app
kubectl rightsize savings -n my-app
```

Look for:

- **Confidence scores increasing** as more data is collected
- **Recommendations stabilizing** (not swinging wildly between checks)
- **Savings estimates** that make sense for your workload

If recommendations look unreasonable, adjust the policy:

- Recommendations too aggressive? Increase `safetyMargin` (e.g., `"1.3"`)
- Recommendations too conservative? Decrease `safetyMargin` (e.g., `"1.1"`)
- Don't want memory to decrease? Set `memory.allowDecrease: false`

## Week 2: Promote to Canary mode

Once you trust the recommendations, switch to Canary mode to test on a
small subset of pods:

```bash
kubectl patch rsp my-first-policy -n my-app --type=merge \
  -p '{"spec":{"updateStrategy":{"mode":"Canary","canary":{"percentage":10}}}}'
```

This resizes only 10% of matching pods. Monitor:

```bash
kubectl rightsize status -w -n my-app
kubectl rightsize history -n my-app
```

The history command shows each resize with its result and reason. If a
resize is reverted, the **REASON** column tells you why (oomkill, restart,
notready, throttle).

!!! warning
    If you see repeated reverts, increase the safety margin or adjust
    bounds before proceeding. See [troubleshooting](../guides/troubleshooting.md).

## Weeks 3-4: Full automation

After successful canary resizes with no reverts, promote to Auto:

```bash
kubectl patch rsp my-first-policy -n my-app --type=merge \
  -p '{"spec":{"updateStrategy":{"mode":"Auto"}}}'
```

Now the operator manages resources continuously. Check savings:

```bash
kubectl rightsize savings -A
```

The TOTAL row at the bottom shows aggregate cluster-wide savings.

## Expanding to more workloads

Once the first policy has been running in Auto mode for a week, expand by
creating policies for more workloads. Use
[RightSizeDefaults](../reference/api.md#rightsizedefaults) to set
org-wide defaults (safety margins, bounds) so individual policies stay
minimal.

## Quick reference: what to run when

| When | Command | What you're checking |
|------|---------|---------------------|
| Just installed | `kubectl rightsize status -w` | Operator picked up your policy |
| Waiting for data | `kubectl rightsize status -w` | Progress percentage climbing |
| First recommendations | `kubectl rightsize recommendations` | Values look reasonable |
| Understanding a recommendation | `kubectl rightsize explain <policy>` | Full pipeline trace |
| After enabling Canary/Auto | `kubectl rightsize history` | Resizes succeeding, no reverts |
| Monthly review | `kubectl rightsize savings -A` | Cluster-wide savings total |