# SLO guardrails

Infrastructure signals (OOMKill, restarts, throttle, NotReady) catch many bad
resizes. Application-level SLOs catch the rest: latency spikes, error-rate
jumps, or availability drops that do not show up as container crashes.

`updateStrategy.sloGuardrails` lets you attach PromQL checks that run after
each resize. If a query breaches its threshold, Attune reverts the resize with
reason `slo:<guardrail-name>`.

Architecture detail lives under
[Safety: SLO guardrails](../architecture/safety.md#slo-guardrails). This guide
covers configuration and day-2 use.

## When to use them

Add guardrails before promoting a policy from Canary to Auto on user-facing
workloads. Pair them with `autoRevert: true` (the default) so breaches undo
the change automatically.

Guardrails are optional. Policies without them still revert on OOMKill, restart
spikes, NotReady, and CPU throttle.

## How they work

1. Attune resizes a pod (Auto, OneShot, Canary, or other applying modes).
2. The safety monitor waits for each guardrail's `evaluationWindow` (default
   `5m`, minimum `1m`) so the app can stabilize.
3. It runs the PromQL query against the policy metrics source.
4. If the scalar result breaches the threshold in the configured direction
   (`above` or `below`), the resize is reverted with reason
   `slo:<name>`.
5. Query errors, empty series, NaN, or Inf **fail open**: that guardrail is
   skipped and logged rather than forcing a false revert.

## Configuration

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `name` | string | (required) | Label for logs, events, and revert reason |
| `query` | string | (required) | PromQL returning a scalar |
| `threshold` | string | (required) | Value that triggers a revert |
| `comparison` | string | `above` | `above` (value > threshold) or `below` |
| `evaluationWindow` | duration | `5m` | Wait after resize before evaluating (min `1m`) |

Template variables in `query`:

| Variable | Expands to |
|----------|------------|
| `{{ .Namespace }}` | Policy / pod namespace |
| `{{ .WorkloadName }}` | Workload name |
| `{{ .PodName }}` | Resized pod name |

### Example

```yaml
apiVersion: attune.io/v1alpha1
kind: AttunePolicy
metadata:
  name: checkout-api
  namespace: production
spec:
  targetRef:
    kind: Deployment
    name: checkout-api
  metricsSource:
    prometheus:
      address: http://prometheus-server.monitoring:80
  updateStrategy:
    type: Canary
    autoRevert: true
    safetyObservationPeriod: 5m
    sloGuardrails:
      - name: p99-latency
        query: >-
          histogram_quantile(0.99,
            rate(http_request_duration_seconds_bucket{
              namespace="{{ .Namespace }}",
              pod="{{ .PodName }}"
            }[5m]))
        threshold: "0.5"
        comparison: above
        evaluationWindow: 5m
      - name: error-rate
        query: >-
          sum(rate(http_requests_total{
            namespace="{{ .Namespace }}",
            pod="{{ .PodName }}",
            code=~"5.."
          }[5m]))
          /
          sum(rate(http_requests_total{
            namespace="{{ .Namespace }}",
            pod="{{ .PodName }}"
          }[5m]))
        threshold: "0.01"
        comparison: above
```

You can also set `sloGuardrails` on `AttuneDefaults` or
`AttuneNamespaceDefaults` so policies inherit them when unset. See
[inheritable UpdateStrategy fields](../reference/configuration.md#inheritable-updatestrategy-fields).

## Operational tips

- Prefer **pod-scoped** labels (`pod="{{ .PodName }}"`) when the metric exists
  at pod level so canary pods are judged on their own traffic.
- Keep `evaluationWindow` at least as long as your SLO query range when the
  range needs warm data (for example `[5m]` rates).
- Start with Canary so only a fraction of pods are exposed while you tune
  thresholds.
- Use `kubectl attune history` to confirm revert reasons; SLO breaches show as
  `slo:<name>`.
- Guardrails require a working Prometheus (or compatible) metrics source on the
  policy. They do not run against Datadog/CloudWatch query languages.

## Troubleshooting

| Symptom | What to check |
|---------|----------------|
| Revert reason `slo:p99-latency` | Threshold too tight, or traffic mix after resize changed latency |
| No SLO reverts while latency is bad | Query returns empty/error (fails open); metric labels wrong; window not elapsed |
| False reverts right after resize | Widen `evaluationWindow`; exclude cold-start noise from the query |
| Guardrail never evaluated | Mode is Observe/Recommend (no applying resize), or `autoRevert: false` |

Operator logs at V(1) include skipped guardrails (query errors, NaN/Inf). See
[Troubleshooting: high revert rate](troubleshooting.md#high-revert-rate) and
[architecture safety](../architecture/safety.md).

## Related

- [Safety architecture](../architecture/safety.md) (auto-revert triggers, observation period)
- [Configuration reference: SLO Guardrails](../reference/configuration.md#slo-guardrails)
- [Canary rollout](canary-rollout.md)
- [Auto mode](auto-mode.md)
- [Metrics reference](../reference/metrics.md) (`attune_reverts_total` reason labels)
