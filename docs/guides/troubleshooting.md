# Troubleshooting

## Common conditions

Check the policy's conditions for a quick diagnosis:

```bash
kubectl get attunepolicy <name> -o jsonpath='{.status.conditions}' | jq .
```

### PrometheusSeriesCapped

**Symptom:** Ready is True with reason `PrometheusSeriesCapped`, or logs show
"Prometheus range query series capped" or a CloudWatch series/page cap.

**Cause:** A range query returned more series than `--max-prometheus-series`
(default 5000). CloudWatch `GetMetricData` uses the same series default and
also stops after 20 result pages. Attune keeps partial data (preferring at
least one series per container) and continues.

**Fix:**

1. Keep the default `metricsSource.podAggregation: Max` (or set it explicitly)
   so series count tracks containers, not pods.
2. Raise `maxPrometheusSeries` / `--max-prometheus-series` if you need more
   series under `None` or `Avg`.
3. Use recording rules (`cpuRecordingMetric` / `memoryRecordingMetric`) for
   pre-aggregated metrics.

See [Scaling: PrometheusSeriesCapped](scaling.md#ready-reason-prometheusseriescapped).

### MetricsUnavailable

**Symptom**: Ready condition is `False` with reason `MetricsUnavailable`.
Policies last written before this reason existed may still show the
older `PrometheusUnavailable` alias until the next reconcile.

**Cause**: `MetricsUnavailable` means the controller could not use the
metrics backend (Prometheus, Datadog, or CloudWatch) for this reconcile.
The condition message tells you which step failed:

- `Cannot resolve Prometheus config` means address resolution failed. The
  operator checks (in order): policy spec, one defaults source
  (`AttuneNamespaceDefaults` if present, otherwise `AttuneDefaults`),
  Prometheus Operator CRD, then well-known service names.
- `Cannot create metrics collector`, `reading secret`, or transport errors
  like `TLS handshake timeout` mean the address was found but auth, headers,
  bearer token secret, CA bundle, or TLS setup failed.
- `Metrics query timeout exceeded` means the reconcile-level timeout
  expired before all backend queries completed.
- `Metrics query errors (` means the backend answered, but one or more
  metric queries failed. This can still happen when the backend is reachable.
- `cannot read Datadog API key`, `datadog API returned 403`, or
  `datadog API returned 429` means the Datadog Secret is missing, the
  API key is rejected, or Datadog rate-limited the collector.
- `loading AWS config`, `AccessDeniedException`, or `AssumeRole` means
  CloudWatch IAM credentials, role assumption, or `GetMetricData` failed.

If the condition message includes `Cannot resolve Prometheus config: SSRF blocked`,
the configured address points at `localhost`, `127.0.0.1`, `::1`, or a
cloud metadata endpoint. Replace it with the in-cluster Prometheus Service
DNS name or ClusterIP. A local `kubectl port-forward` URL on your workstation
will not work.

**Fix address resolution failures**:

1. Set the address explicitly in a `AttuneDefaults` resource:

    ```yaml
    apiVersion: attune.io/v1alpha1
    kind: AttuneDefaults
    metadata:
      name: default
    spec:
      metricsSource:
        prometheus:
          address: http://prometheus-server.monitoring:80
    ```

2. Verify the Prometheus Service exists and note its port:

    ```bash
    kubectl get svc -n monitoring
    # Check the PORT(S) column: "80/TCP" means use :80, not :9090
    ```

3. Test connectivity from inside the cluster:

    ```bash
    kubectl run prom-test --image=curlimages/curl --restart=Never --rm --attach --command -- \
      curl -sf http://prometheus-server.monitoring:80/-/healthy
    ```

If the condition message includes `Cannot create metrics collector`,
`reading secret`, or a transport error like `TLS handshake timeout`,
verify the credentials and connection details before changing timeouts:

1. Check the referenced Secret exists in the policy namespace and contains the expected bearer token.
2. Re-check custom headers, CA bundle, and `insecureSkipVerify` settings.
3. Test the exact Prometheus URL from inside the cluster with the same auth
   mechanism the operator uses.

If the condition message includes `Metrics query timeout exceeded`, the
operator's reconcile-level timeout expired before all workload queries
completed. This typically happens when the metrics backend is slow to
respond (not down, just overloaded) or when a policy targets many workloads.

**Fix query timeouts**:

1. Increase the timeout: set Helm `prometheusTimeout: "10m"` (or
   `--prometheus-timeout=10m`).
2. Reduce per-query cost: decrease `historyWindow` or increase `queryStep`
   on the AttunePolicy or AttuneDefaults.
3. Check Prometheus health: high query latency often indicates Prometheus
   itself needs more resources or recording rules.

If the condition message includes `Metrics query errors (`, the backend was
reachable but one or more metric queries still failed.

**Fix query errors**:

1. Check the operator logs for the exact failing query and backend error.
2. Replay the failing query directly against Prometheus to confirm whether the
   backend rejects it or returns partial data.
3. If the backend is overloaded, reduce query cost with a shorter
   `historyWindow` or a larger `queryStep`.

See the [Prometheus Setup](prometheus-setup.md) guide for full details on
address resolution and common installations.

### Prometheus reachable but queries return no data

**Symptom**: Ready condition is `InsufficientData` even after days of running.
Operator logs show `"cpuPoints":0,"memPoints":0`.

**Cause**: Prometheus is reachable but cadvisor metrics are not being scraped,
or label names have been relabeled.

**Fix**:

1. Verify cadvisor metrics exist in Prometheus:

    ```bash
    kubectl run prom-check --image=curlimages/curl --restart=Never --rm --attach --command -- \
      curl -s 'http://prometheus-server.monitoring:80/api/v1/query?query=container_cpu_usage_seconds_total' \
      | head -c 200
    ```

2. If the result is empty (`"result":[]`), cadvisor scraping is not
   configured. Check your Prometheus scrape configuration for a
   `kubernetes-nodes-cadvisor` or equivalent job.

3. If the result has data but the operator still reports 0 data points,
   check that the `namespace`, `pod`, and `container` label names match.
   Some Prometheus configurations relabel these.

### NoWorkloadsFound

**Symptom**: Ready condition is `False` with reason `NoWorkloadsFound`.

**Cause**: The policy's `targetRef` does not match any workloads in the
namespace. This is usually a typo in the workload name or an incorrect
`kind` (e.g., targeting a `Deployment` when the workload is a `StatefulSet`).

**Fix**:

1. Verify the workload exists:

    ```bash
    kubectl get deploy,sts,ds -n <namespace>
    ```

2. Check the `targetRef.name` spelling in your policy. If using a label
   selector, verify the labels exist on the target workload:

    ```bash
    kubectl get deploy <name> -n <namespace> --show-labels
    ```

3. Ensure the `targetRef.kind` matches the workload type (`Deployment`,
   `StatefulSet`, `DaemonSet`, `ReplicaSet`, `Job`, or `CronJob`).

### ConflictCheckFailed

**Symptom**: Ready condition is `False` with reason `ConflictCheckFailed`.

**Cause**: The operator could not list `AttunePolicy` objects in the
namespace while checking for overlapping targets. That cycle does not
compute new recommendations. Last-known recommendations stay on status
so this does not look like bootstrap `InsufficientData`.

**Fix**:

1. Confirm the operator ServiceAccount can `list` and `watch`
   `attunepolicies.attune.io`.
2. Check API server health and operator logs for the list error.
3. Watch `attune_reconcile_errors_total{error_type="list_policies"}`.

### InsufficientData

**Symptom**: Ready condition is `False` with reason `InsufficientData`.

**Check first**: `kubectl attune doctor` confirms Kubernetes 1.32+ and
`pods/resize`. A failed Prometheus ping is optional (printed as `WARN`)
and does not prove the operator cannot reach an in-cluster address.
A 401 or 403 on an address that sets `bearerTokenSecret` or custom
`headers` is skipped the same way: doctor does not send those credentials.

**Cause**: Not enough Prometheus data points to generate recommendations.
The default minimum is 48 Prometheus range-query samples. With the default
`queryStep: 5m`, that is about 4 hours of data.

During this state the operator requeues at `min(cooldown, queryStep)` and
does **not** add `requeueJitter`. A policy with `cooldown: 1m` and the
default 5m step therefore retries every minute, not every 1–3 minutes.

**Fix**: Wait for more data to accumulate, or adjust these settings:

- **`minimumDataPoints`**: Lower for faster (but less confident) recommendations.
- **`historyWindow`**: If too short (e.g. `1h`), Prometheus may not have enough
  samples within the window. The default is `168h` (7 days). Ensure the window
  is long enough for your scrape interval to produce at least `minimumDataPoints`
  data points.

```yaml
spec:
  metricsSource:
    minimumDataPoints: 48   # ~4 hours of data at the default queryStep: 5m
    historyWindow: 168h     # query the last 7 days of metrics
```

### InvalidConfig

**Symptom**: Ready condition is `False` with reason `InvalidConfig`.

**Cause**: The controller could not fetch or apply defaults cleanly before
continuing. The condition message includes the failing step, such as
`Failed to fetch defaults: listing AttuneNamespaceDefaults ...`.

**Fix**:

1. Check whether the operator can list `AttuneDefaults` and
   `AttuneNamespaceDefaults`.
2. Verify the defaults objects themselves are valid and that only the
   expected objects exist in the namespace.
3. Check operator logs for the exact failing API call or validation error.

### WorkloadDiscoveryFailed

**Symptom**: Ready condition is `False` with reason `WorkloadDiscoveryFailed`.

**Cause**: The operator could not resolve the policy's `targetRef` into the
workloads it should inspect. The condition message includes the failing step,
for example an unsupported kind, an invalid selector, or a client/list error.

**Fix**:

1. Verify `spec.targetRef.kind` is one of `Deployment`, `StatefulSet`,
   `DaemonSet`, `CronJob`, `Job`, or `ReplicaSet`.
2. If you use `targetRef.name`, confirm the workload exists in the same
   namespace as the policy.
3. If you use `targetRef.selector`, confirm it matches at least one workload
   and includes real `matchLabels` or `matchExpressions` entries.
4. Check operator logs for the exact discovery error if the target still
   looks correct.

### New pods still start at template size

**Symptom**: `updateStrategy.initialSizing` is true, but new pods keep the
Deployment template requests. The pod has no `attune.io/initial-sizing=applied`
annotation.

**Cause**: The mutating webhook only patches CREATE when every gate passes.
A selector policy also has to fetch the owning Deployment, StatefulSet, or
DaemonSet and match `targetRef.selector`. Get or parse errors skip the
pod (the CREATE is still allowed). A stale recommendation also skips
initial sizing (last-known values stay in status, but CREATE is not patched).

**Fix**:

1. Confirm the Helm/operator value `initialSizing.enabled` is true and the
   namespace has label `attune.io/initial-sizing=enabled`.
2. Confirm the policy is Auto, OneShot, or Canary (not Observe or Recommend)
   and `updateStrategy.initialSizing: true`.
3. If `targetRef.selector` is set, confirm the owner object exists and its
   labels match. An empty selector matches nothing.
4. On Canary, CREATE sizing waits until that app is promoted, or the
   assigned pod name is already in `status.canary.workloads[].pods`.
   ReplicaSet CREATE often has an empty `metadata.name` and only
   `generateName`. An empty name is not treated as a canary-slice
   identity, so those pods stay at template size until the app is
   promoted.
5. The webhook applies a rec when every container has confidence at least
   0.5, or this workload already has a successful in-place resize. A 1h
   `historyWindow` never reaches 0.5 on its own.
6. Check operator logs for `fetching workload for initial-sizing selector`
   or `initial sizing applied`. When CREATE has no assigned name, that
   Info line uses `generateName` (for example `my-app-abc-`), not the
   name kubelet later assigns.

### Paused

**Symptom**: Ready condition is `False` with reason `Paused`.

**Cause**: `spec.paused` is set to `true` on the policy. The operator skips
all reconciliation: no metrics collection, no recommendations, no resizes.
Existing resizes are not reverted.

**Fix**: Set `spec.paused: false` or remove the field entirely. The operator
will resume reconciliation on the next cycle.

### CooldownActive

**Symptom**: The operator logs "Cooldown active, skipping resize" and no
pods are resized.

**Cause**: A resize was performed recently and the cooldown period has not
elapsed.

**Fix**: Wait for the cooldown to expire, or shorten it:

```bash
kubectl patch attunepolicy <name> --type merge \
  -p '{"spec":{"updateStrategy":{"cooldown":"30m"}}}'
```

## Webhook / cert-manager issues

### Webhook connection refused

**Symptom**: `kubectl apply -f policy.yaml` returns:

```
Error from server (InternalError): Internal error occurred: failed calling
webhook "vattunepolicy.kb.io": Post "https://...": dial tcp ...: connection refused
```

**Cause**: The webhook server is not running or the TLS certificate is not
ready. This typically means cert-manager is missing or broken.

**Fix**:

1. Verify cert-manager is installed and running:

    ```bash
    kubectl get pods -n cert-manager
    # All 3 pods (cert-manager, cainjector, webhook) should be Running
    ```

2. If cert-manager is not installed, install it:

    ```bash
    kubectl apply -f https://github.com/cert-manager/cert-manager/releases/download/v1.21.1/cert-manager.yaml
    kubectl wait --for=condition=Available deployment/cert-manager-webhook -n cert-manager --timeout=120s
    ```

3. Check the Certificate status:

    ```bash
    kubectl get certificate -n attune-system
    # Status should be True (Ready)
    ```

4. If the Certificate is not ready, check the cert-manager logs:

    ```bash
    kubectl logs -n cert-manager deploy/cert-manager --tail=20
    ```

### Webhook timeout

**Symptom**: Policy creation takes 30 seconds then fails with timeout.

**Cause**: The webhook pod is running but the cainjector has not patched the
CA bundle into the webhook configuration yet.

**Fix**: Wait for cainjector to inject the CA bundle (usually resolves within
1-2 minutes after cert-manager is ready):

```bash
kubectl get validatingwebhookconfiguration -o yaml | grep caBundle | head -1
# If empty, cainjector has not run yet. Wait and retry.
```

## Resize failures

### Resize subresource not found (K8s 1.32)

**Symptom**: Operator logs contain `the server does not allow this method
on the requested resource` or `pod resize subresource is not enabled` when
attempting a resize.

**Cause**: On Kubernetes 1.32, the In-Place Pod Resize feature is alpha and
disabled by default. The `/resize` subresource is only available when the
`InPlacePodVerticalScaling` feature gate is enabled on all control plane
components and kubelets.

**Check first**: `kubectl attune doctor` reports whether the cluster is
1.32+ and whether discovery lists `pods/resize`.

**Fix**: Enable the feature gate on all components. For managed clusters,
check your provider's documentation. For self-managed clusters:

```bash
# API server, controller-manager, and scheduler flags:
--feature-gates=InPlacePodVerticalScaling=true

# Kubelet config (on every node):
featureGates:
  InPlacePodVerticalScaling: true
```

On **Kubernetes 1.33+**, this feature gate is enabled by default and no
action is needed.

### Recommendation looks wrong or never resizes

**Symptom**: `kubectl attune recommendations` shows values you do not trust, or Auto/Canary never applies them.

**Diagnose**:

```bash
kubectl attune explain -n <ns> <policy>
kubectl get attunepolicy <policy> -n <ns> -o jsonpath='{.status.conditions}' | jq .
kubectl attune history -n <ns>
```

Use the explanation chain (percentile → overhead → confidence → bounds → change filter) to see which stage moved the number. See [Recommend mode: reading recommendations](recommend-mode.md#reading-recommendations-from-status) for the field table and skip reasons (cooldown, budget, canary, Deferred/Infeasible, stale, schedule).

**Common fixes**:

- Too aggressive: raise `overhead` or tighten `maxAllowed`
- Too conservative / sparse data: wait for more data points or lower `minimumDataPoints` only for evaluation
- Never resizes with tiny delta: change filter; expected when already near target
- Stuck on node capacity: Deferred/Infeasible section below

### Deferred or Infeasible resize (stuck pods)

**Symptom**: Policy status shows `ResizeBlocked=True`, or:

```bash
kubectl get attunepolicy <name> -o jsonpath='{.status.workloads}' | jq .
# deferred > 0 and/or infeasible > 0

kubectl get attunepolicy <name> -o jsonpath='{range .status.conditions[?(@.type=="ResizeBlocked")]}{.reason} {.message}{"\n"}{end}'

kubectl attune history -n <ns>
# Failed rows with reason "infeasible"
```

| Signal | Meaning | Operator behavior |
|--------|---------|-------------------|
| **Deferred** | Kubelet accepted the request but cannot apply it yet (often free request capacity on the node). Pod condition `PodResizePending` reason `Deferred`. | Pod is **not eligible** for a new resize until the condition clears. **Retry**: every reconcile after eligibility returns (no extra config). |
| **Infeasible** | Kubelet cannot complete the resize in-place on this node. | With default `resizeMethod: InPlaceOnly`, skip + history `Failed`/`infeasible` + event `InfeasibleBlocked`. With `InPlaceOrRecreate`, attempt eviction fallback (subject to PDB / last-replica guards). |

**Metrics** (see [metrics reference](../reference/metrics.md)):

```promql
attune_pods_deferred{namespace="...", policy="..."}
attune_pods_infeasible{namespace="...", policy="..."}
histogram_quantile(0.95, sum by (le) (rate(attune_deferred_age_seconds_bucket[15m])))
rate(attune_infeasible_skipped_total[15m])
rate(attune_eviction_total[15m])
```

**Fix**:

1. **Deferred**: free capacity on the node, wait for other pods to scale down, or reduce the recommended increase (`maxAllowed` / change caps). No restart is required for Attune; it retries automatically when the condition clears.
2. **Infeasible**: free node capacity, lower bounds, or enable eviction fallback:

```yaml
spec:
  updateStrategy:
    resizeMethod: InPlaceOrRecreate  # opt-in eviction when in-place is impossible
  cpu:
    maxAllowed: "2000m"  # keep increases within typical node headroom
```

3. Confirm the live pod condition:

```bash
kubectl get pod <pod> -o jsonpath='{range .status.conditions[?(@.type=="PodResizePending")]}{.reason} {.message}{"\n"}{end}'
```

**Retry policy (current defaults)**:

- **Deferred**: skip until kubelet clears `PodResizePending`; next reconcile retries. No max deferred age cut-off (watch `attune_deferred_age_seconds` and `ResizeBlocked` message for escalation).
- **Infeasible + InPlaceOnly**: skip every cycle until the condition clears or you change `resizeMethod` / capacity.
- **Infeasible + InPlaceOrRecreate**: one eviction attempt per container resize path; if eviction is denied (PDB, last replica), history records `Failed`/`infeasible` and the next cycle may try again.

### QoS class change blocked

**Symptom**: Operator logs `Skipping resize: would change QoS class`.

**Cause**: For Guaranteed-class pods, requests must equal limits. If the
policy would set different values for requests and limits, the resize is
skipped.

**Fix**: Set `controlledValues: RequestsAndLimits` so both are updated
together, or switch to `RequestsOnly` if the pod should be Burstable.

### ResourceQuota exceeded

**Symptom**: Operator logs `Skipping resize: quota/limitrange violation`
with a message mentioning `exceed ResourceQuota`.

**Cause**: The resize would increase CPU or memory requests beyond the
remaining headroom in the namespace's ResourceQuota. Attune uses
`requests.cpu` / `requests.memory` when those names are present, or the
`cpu` / `memory` aliases when a quota uses the short names.

**Fix**:

1. Check current quota usage:

    ```bash
    kubectl get resourcequota -n <namespace>
    ```

2. Either increase the quota limits, or tighten the policy's resource
   bounds so recommendations stay within quota.

### Resize skipped: quota list unavailable

**Symptom**: Operator logs `quota list unavailable; skipping request increase`
or `ResourceQuota list unavailable` / `LimitRange list unavailable`.
Request increases are skipped.

**Cause**: The controller could not list ResourceQuotas or LimitRanges in
the pod namespace, usually because the operator ServiceAccount is missing
`list`/`watch` on those resources. Request increases fail closed so a
quota cannot be exceeded while the check is unavailable. Decreases still
proceed.

**Fix**:

1. Confirm the operator ServiceAccount can list quotas and limit ranges
   in the workload namespace:

    ```bash
    # Helm release "attune" → ServiceAccount "attune" in the operator
    # namespace (typically attune-system). Raw manifests use
    # attune-controller-manager instead.
    kubectl auth can-i list resourcequotas -n <ns> \
      --as system:serviceaccount:<operator-ns>:attune
    kubectl auth can-i list limitranges -n <ns> \
      --as system:serviceaccount:<operator-ns>:attune
    ```

2. Grant `list` and `watch` on `resourcequotas` and `limitranges` if
   either command returns `no`.

## Revert issues

### High revert rate

**Symptom**: `Degraded` condition is `True` with reason `HighRevertRate`, or
multiple entries in `.status.resizeHistory` show `result: Reverted`.

**Cause**: 3+ of the last 5 resize operations were reverted due to safety
violations. The controller applies exponential backoff (2x cooldown per
consecutive revert, capped at 16x).

Check the current backoff state:

```bash
kubectl get attunepolicy <name> -o jsonpath='{.status.cooldown}'
# Example: {"backoffMultiplier":8,"consecutiveReverts":3,"effectiveCooldown":"8h0m0s"}
```

**Fix**: Investigate the revert reasons:

```bash
kubectl get attunepolicy <name> -o jsonpath='{.status.resizeHistory}' | \
  jq '[.[] | select(.result=="Reverted")]'
```

Common causes:

- **oomkill**: overhead is too low for memory. Increase `memory.overhead`.
- **throttle**: CPU throttle ratio exceeded 50% post-resize. Increase `cpu.overhead`.
- **restart**: the application crashes at the new resource level. Check application logs.
- **notready**: readiness probe fails post-resize. Verify probe configuration.
- **slo:&lt;name&gt;**: an SLO guardrail query breached its threshold after resize. Review the guardrail's PromQL query and threshold in `updateStrategy.sloGuardrails`.

### Fleet report export failures or empty fleet dashboard

**Symptom**: `attune_fleet_report_export_total{result="failed"}` increases, or
the fleet Grafana dashboard shows no series when filtering by cluster.

**Cause**:

1. Fleet report is disabled (default) or the ConfigMap write failed (RBAC /
   wrong namespace).
2. Federated dashboards require Prometheus `external_labels.cluster` on each
   cluster scrape. Without that label, `cluster=~"$cluster"` panels stay empty.
3. Operator `watchNamespaces` is set, so the report only includes a subset of
   policies.
4. `estimatedMonthlySavingsUSD` is 0 even though some policies show a savings
   string. The rollup only adds parseable dollar amounts. Empty, non-numeric,
   `NaN`, and `Inf` values count as 0. Check `unparseableSavings` in
   `report.json` (non-zero means values were dropped).

**Fix**:

1. Enable export: Helm `fleetReport.enabled=true` (or `--fleet-report-enabled`).
2. Check the ConfigMap: `kubectl -n <release-ns> get cm attune-fleet-report -o yaml`
3. Set `global.external_labels.cluster` on each Prometheus; reload federation.
4. Use leader election with HA when fleet report is enabled.
5. If the USD total is 0, inspect `status.savings.estimatedMonthlySavings` on
   each policy. Fix `costPricing` so the string is a finite number such as
   `$12.50`.

```promql
sum(rate(attune_fleet_report_export_total{result="failed"}[5m]))
```

### Resize skipped for node capacity or pressure

**Symptom**: Events show `ResizeSkipped` with "exceed node allocatable",
"node free request budget exceeded by neighbors",
"node has MemoryPressure/DiskPressure/PIDPressure", or
"node status unavailable", and `attune_capacity_skip_total` increments
(`reason` label: `allocatable`, `neighbors`, `pressure`, or `unavailable`).

**Cause**: Always-on safety gates refuse request **increases** that would
make this pod's total requests exceed the node's allocatable, that would
not fit after other pods on the node have reserved requests, that would
raise requests while the node is under pressure, or when the Node object
cannot be loaded (API/RBAC failure). Decreases still proceed.

**Fix**:

1. Free capacity on the node (evict low-priority pods) or move the workload.
2. Lower `maxAllowed` / change caps so recommendations fit typical node shapes.
3. For DaemonSets, size against the **smallest** node pool that runs them.
4. For `unavailable`: check operator RBAC for `nodes` get/list/watch and
   apiserver health; Attune fails closed on increases until the node is readable.
5. Inspect formulas in [Node capacity](../architecture/node-capacity.md).

```promql
sum by (namespace, policy, reason) (rate(attune_capacity_skip_total[1h]))
```

### OOM after memory limit decrease

**Symptom**: After enabling memory decreases (`memory.allowDecrease: true`
and `controlledValues: RequestsAndLimits`) on Kubernetes 1.35+, pods OOMKill
when limits shrink, Events show `MemoryLimitUsageFloor`, metrics show
`attune_memory_limit_decrease_total{result="clamped_usage"}` or
`skipped_unsafe`, or the opt-in `AttuneMemoryLimitUnsafe` alert fires.

**Cause**: Live memory limit decreases race with usage spikes. Attune floors
the target limit above recent usage (recommendation raw percentile) plus
`memory.decreaseUsageMarginPercent` (default 10%). If OOM still occurs, the
margin or overhead is too low, or usage is spikier than the percentile window.

**Fix**:

1. Raise margin: `memory.decreaseUsageMarginPercent: 20` (or higher).
2. Raise `memory.overhead` so recommendations (and limits) stay further above
   steady usage.
3. Keep `memory.maxDecreasePercent` modest so large drops step down over
   multiple cycles.
4. Confirm cluster version is 1.35+; on 1.33–1.34, limit decreases are
   platform-clamped (`result="clamped_platform"`).

```promql
# Floored or blocked memory limit decreases
sum by (namespace, policy, result) (
  rate(attune_memory_limit_decrease_total[1h])
)
```

### Revert failures

**Symptom**: Entries in `.status.resizeHistory` show `result: Failed`, or
`attune_revert_failures_total` is incrementing.

**Cause**: The operator detected a safety issue (OOMKill, throttle, etc.)
and tried to revert the pod to its original resources, but the `/resize`
subresource call failed. The pod remains at the post-resize resource level.

**Fix**: Check operator logs for the revert error:

```bash
kubectl logs -l app.kubernetes.io/name=attune --tail=100 | grep "Failed to revert"
```

Common causes:

- **Conflict**: another controller (HPA, VPA) is modifying the same pod.
  Use `attune_revert_failures_total` to track frequency.
- **Pod evicted**: the pod was evicted between the safety check and revert.
- **RBAC**: the operator ServiceAccount lacks `update` on the `pods/resize`
  subresource.

```promql
# Alert when reverts are failing
sum by (namespace, workload) (rate(attune_revert_failures_total[5m])) > 0
```

### Resizes not happening during expected window

**Symptom**: Operator logs "Outside resize window, skipping resize" even
though you expect the window to be open.

**Cause**: The `schedule.timezone` does not match your local time.
Windows are evaluated in the configured timezone (default: UTC).

**Fix**: Verify your timezone is correct:

```yaml
schedule:
  windows:
    - start: "02:00"
      end: "06:00"
  timezone: "America/New_York"  # not UTC
```

Check the current time in the configured timezone:

```bash
TZ="America/New_York" date "+%H:%M %A"
```

### Budget exhausted

**Symptom**: Operator logs "Budget exhausted, deferring resize to next
cycle" and some pods are not resized.

**Cause**: The total CPU or memory increase across all pods exceeds the
configured `maxTotalCpuIncrease` or `maxTotalMemoryIncrease`.

**Fix**: Either increase the budget or accept that resizes are spread
across multiple reconcile cycles (this is the intended behavior for
gradual rollout):

```yaml
updateStrategy:
  maxTotalCpuIncrease: "4000m"    # 4 cores per cycle
  maxTotalMemoryIncrease: "8Gi"   # 8 GiB per cycle
```

### Policy rejected: invalid schedule timezone

**Symptom**: `kubectl apply` fails with:
```
admission webhook "validation.attune.io" denied the request:
updateStrategy.schedule.timezone "PST" is not a valid IANA timezone
```

**Cause**: The timezone must be a valid IANA timezone name from the
[tz database](https://en.wikipedia.org/wiki/List_of_tz_database_time_zones).
Common mistakes include using abbreviations that Go's `time.LoadLocation`
does not recognize.

**Fix**: Use the canonical IANA region/city name:

| Invalid | Valid alternative |
|---------|-----------------|
| `PST` | `America/Los_Angeles` |
| `IST` | `Asia/Kolkata` |

Note: `US/Eastern`, `EST`, and `CET` are valid IANA timezone links and
will be accepted, but the canonical forms (`America/New_York`,
`Europe/Berlin`) are recommended for clarity.

```bash
# List all valid timezones on your system:
timedatectl list-timezones
```

### Policy rejected: invalid day of week

**Symptom**: `kubectl apply` fails with:
```
admission webhook "validation.attune.io" denied the request:
updateStrategy.schedule.daysOfWeek contains invalid day "Wed"
```

**Cause**: Day names must be the full English name. Abbreviations and
non-English names are not accepted.

**Fix**: Use the full name (case-insensitive):

```yaml
schedule:
  daysOfWeek: ["Monday", "Tuesday", "Wednesday", "Thursday", "Friday"]
```

Valid values: `Monday`, `Tuesday`, `Wednesday`, `Thursday`, `Friday`,
`Saturday`, `Sunday`.

### Policy rejected: invalid CloudWatch clusterName

**Symptom**: `kubectl apply` fails with:
```
admission webhook "validation.attune.io" denied the request:
metricsSource.cloudwatch.clusterName: clusterName must be an EKS-style name
```

**Cause**: `clusterName` is interpolated into a CloudWatch SEARCH expression.
Only EKS-style names are accepted: 1-100 characters, start with
alphanumeric, then alphanumeric, hyphen, or underscore. Quotes, spaces,
and other SEARCH metacharacters are rejected.

**Fix**: Use the EKS cluster name as shown in the AWS console (for example
`my-eks-cluster`).

## Deleting a policy

When you delete a `AttunePolicy`, the operator uses a
`attune.io/cleanup` finalizer to clean up before the resource is
garbage-collected:

1. **Annotations removed**: all tracking annotations (`attune.io/resized-at`,
   `attune.io/policy`, etc.) and the `attune.io/tracked` label are
   removed from pods managed by that policy.
2. **Resources retained**: pods keep their current (resized) CPU and memory
   values. The operator does not revert resources to pre-resize values.
3. **Gauges cleaned**: Prometheus gauge metrics for the policy are removed.
4. **Finalizer removed**: only after cleanup succeeds. If a pod update
   fails, the finalizer remains and the controller retries on the next
   reconcile cycle.

If the policy appears stuck in `Terminating`, check the operator logs for
pod update errors during cleanup:

```bash
kubectl logs -n attune-system deploy/attune | grep "deletion cleanup"
```

## Large cluster performance

Start with the [Scaling Guide](scaling.md) ops checklist (`clusterSize`,
`watchNamespaces`, CRD window/step/cooldown). The notes below match common
symptoms to those knobs.

### Stale recommendations (slow reconciliation)

If `workqueue_depth` is consistently > 0 and
`workqueue_longest_running_processor_seconds` climbs, the operator cannot
keep up with the reconcile queue. Solutions (in order of impact):

1. **Increase `maxConcurrentReconciles`** (or use a `clusterSize` preset).
2. **Scope with `--watch-namespaces`** to reduce informer cache size.
3. Policies targeting many workloads via label selector process up to
   10 workloads in parallel per reconcile cycle.
4. **High-replica Deployments**: reduce `historyWindow` and increase
   `queryStep` (query payload scales with pods today). See
   [Large Deployments](scaling.md#large-deployments-high-replica-counts).

See the [Scaling Guide](scaling.md) for tuning details and preset values.

### High memory usage

If the operator pod is OOMKilled or uses unexpectedly high memory, the
informer cache may be caching too many objects. Use `--watch-namespaces`
to limit the cache to the namespaces where your policies exist. Also raise
operator memory via a `clusterSize` preset if you intentionally watch a
large pod count.

## Apply paths skipped due to stale recommendations

When Prometheus does not return fresh data during a reconcile cycle, the
operator marks the recommendation as **stale** and skips apply paths that
would act on outdated metrics. A rec is stale when the newest finite
sample is older than `3 * queryStep` (default 15m), or when the last
known rec is reused after an empty query. Reuse expires after that same
bound; then the rec drops and Ready can become `InsufficientData`.
Stale recs do not increment `workloads.withRecommendations` and do not
enter savings gauges. Resize is one skipped apply path; startup boost,
initial sizing (CREATE webhook), and template persistence are also skipped,
the recommendation ConfigMap is not rewritten, and a GitOps apply PR is not
opened. You will see this in the operator logs (resize example):

```
Skipping resize for workload with stale recommendation  workload=my-app
```

The `attune_stale_recommendations_total` counter increments each
time this happens. Common causes:

1. **Prometheus is temporarily unavailable** or responding slowly.
2. **The `historyWindow` is too short** for the workload's scrape interval,
   so range queries return no data.
3. **Pod label changes** caused the PromQL regex to stop matching.

To diagnose, enable debug logging and check the Prometheus query results:

```bash
kubectl logs -n attune-system deploy/attune \
  | grep -E "stale|Prometheus query returned no data"
```

Apply paths resume automatically once fresh data is available.

## Deployment-owned ReplicaSet targeting

If a `AttunePolicy` targets a ReplicaSet that is owned by a Deployment,
the operator rejects it with an error:

```
ReplicaSet my-ns/my-rs is owned by a Deployment; target the Deployment instead
```

Deployment-owned ReplicaSets are also automatically filtered from
selector-based discovery to prevent double-resizing (the Deployment and its
child ReplicaSet would both match). To right-size the workload, target the
parent Deployment instead.

## Known limitations

### Maximum Prometheus addresses

The operator caches at most 64 unique Prometheus collector connections.
Clusters with more than 64 distinct Prometheus addresses across all policies
will see errors on additional addresses. In practice this is rarely hit
since most clusters use 1-2 Prometheus instances.

### Minimum cooldown floor

The operator enforces a minimum cooldown of 1 minute regardless of the
configured `cooldown` value. Setting `cooldown: 10s` effectively becomes
`cooldown: 1m`. This prevents accidental resource churn.

## Enabling debug logs

The operator supports multiple log verbosity levels. By default it runs
at `info` level. To enable debug logging:

```bash
# Enable debug logs (V(1): queries, pod selection, cache, recommendations)
helm upgrade attune attune/attune \
  --set logging.level=debug

# Enable verbose trace logs (V(2): per-sample data, full recommendation chain)
helm upgrade attune attune/attune \
  --set logging.level=2
```

You can also switch to human-readable text format for local debugging:

```bash
helm upgrade attune attune/attune \
  --set logging.level=debug --set logging.format=text
```

Revert to normal after debugging:

```bash
helm upgrade attune attune/attune \
  --set logging.level=info
```

### NaN or Inf values in Prometheus data

**Symptom**: Debug logs (V(1)) show messages like `All CPU samples were
NaN/Inf` or `All memory samples were NaN/Inf`, and the policy remains in
`InsufficientData` state despite Prometheus being reachable.

**Cause**: Prometheus queries can return NaN (e.g., 0/0 division in rate
queries when no samples exist yet) or Inf when scrape data is missing or
contains malformed values. The operator filters out non-finite values
before computing recommendations to prevent corrupted percentile
calculations.

**Fix**:

1. Check if Prometheus has cAdvisor metrics for your namespace:

    ```bash
    kubectl exec -n monitoring prometheus-0 -- \
      wget -qO- 'http://localhost:9090/api/v1/query?query=container_cpu_usage_seconds_total{namespace="YOUR_NS"}' \
      | head -c 200
    ```

2. If the query returns data but values are NaN, check for recording rules
   or relabeling that might divide by zero.
3. Wait for more scrape cycles. NaN values are common during the first few
   minutes after pod creation when Prometheus has only one data point
   (rate computation needs at least two).

The `attune_nan_inf_samples_total` counter increments each time this
happens, broken down by container and metric type (`cpu` or `memory`).
Use it to alert on persistent data quality issues:

```promql
rate(attune_nan_inf_samples_total[1h]) > 0
```

### Requests clamped to limits

**Symptom**: Debug logs (V(1)) show `Requests clamped to limits` with a
list of affected resources (e.g., `cpu`, `memory`).

**Cause**: The recommended CPU or memory request exceeds the container's
current limit. This happens when `controlledValues` is set to
`RequestsOnly` (limits stay at their current values) and the
recommendation grows beyond those limits. The operator caps the request
at the limit to prevent the API server from rejecting the resize.

**Fix**: Either increase the container's limits, or switch to
`controlledValues: RequestsAndLimits` so the operator can scale limits
proportionally with requests.

The `attune_request_clamped_total` counter increments each time a request
is capped, broken down by container and resource. Use it to detect
policies where limits are consistently too tight:

```promql
rate(attune_request_clamped_total[1h]) > 0
```

## Sidecar not resized (or proxy resized unexpectedly)

### Known sidecar auto-exclude is on (default)

Attune skips well-known mesh and sidecar container names by default
(`excludeKnownSidecars: true`), including `istio-proxy`, `linkerd-proxy`,
`consul-dataplane`, `kuma-dp`, `vault-agent`, and common Cloud SQL proxy
names. Operator logs show `reason=known sidecar auto-exclude` when this
path applies.

**If you want to right-size those containers again** (previous behavior):

```yaml
spec:
  excludeKnownSidecars: false
```

Or set the same field on `AttuneDefaults` / `AttuneNamespaceDefaults` for
cluster- or namespace-wide opt-out when policies leave the field unset.

### Custom sidecars still resized

Names not on the built-in list are still right-sized unless listed in
`excludedContainers`. Add the container name explicitly:

```yaml
spec:
  excludedContainers:
    - my-company-agent
```

`kubectl attune explain <policy>` prints `Exclude known sidecars` and the
**effective** excluded set (known list union user list).

## Template persistence not updating the workload template

Opt-in `updateStrategy.templatePersistence` patches Deployment/StatefulSet
pod templates so replacement pods start correctly sized. If the live template
never changes, check these first.

### Feature disabled or wrong `when`

Default is off. Enable explicitly:

```yaml
spec:
  updateStrategy:
    type: Auto # or Recommend with when: OnRecommendation
    templatePersistence:
      enabled: true
      when: AfterSuccessfulResize # default when enabled
```

- **`AfterSuccessfulResize`**: only after a successful in-place resize. In
  Recommend/Observe modes this never fires (webhook warns). Use
  `when: OnRecommendation` for Recommend.
- **`OnRecommendation`**: still skipped in **Observe** mode.
- **Canary**: template patches wait until canary reaches `FullRollout`.
- **Stale recommendation**: `recommendations[].stale` is true; the template is not patched until fresh Prometheus data.

### Mid-rollout or no-op

The operator skips patches while a Deployment/StatefulSet is rolling out,
and no-ops when the template already matches. Events:

- `TemplatePatched` (Normal) on success
- `TemplatePatchFailed` (Warning) on API errors

History entries use `method=TemplatePersistence` and
`result=TemplatePatched` (or `Failed`). Metric:

```promql
rate(attune_template_patch_total{result="failed"}[15m]) > 0
```

### GitOps thrash

If Argo CD / Flux reverts the template every sync, do **not** enable
template persistence under unmanaged sync. Prefer `export.configMap` or
`initialSizing` (see [GitOps integration](gitops-integration.md)).

### GitOps PR opens empty PRs every cooldown

**Symptom**: A new GitHub PR appears on each `pullRequest.cooldown` (default
24h) with only an empty bootstrap commit. The description table matches
the previous PR.

**Cause**: GitOps PR automation writes the drift table in the PR body. It
does not patch Deployment YAML. Merging that PR (and deleting the head
branch) leaves the live template unchanged, so drift stays true.

**Fix**:

1. Do not merge empty notification PRs as the apply step.
2. Apply a real patch: `kubectl attune diff -n <ns> -o yaml`, commit it
   to git, let Flux/Argo sync. Wait for reason `NoDrift`.
3. To stop new PRs immediately, set `export.pullRequest.enabled: false`
   or `dryRun: true`.
4. On Attune versions that include the unchanged-drift skip, the
   condition is `PullRequestUnchanged` instead of another empty PR.
   A dry-run of the same table does not block the first live PR (no
   `gitops-pr-url` yet). If a 0.1.24 dry-run already wrote
   `attune.io/gitops-pr-last-attempt`, delete that annotation or wait
   out `cooldown` before the first live open.
5. v0.1.24 wrote `attune.io/gitops-pr-drift` only after opening a PR.
   Upgrading from 0.1.22/0.1.23 therefore still opened one more empty
   PR when `cooldown` expired (last-attempt and URL were present, the
   fingerprint was not). Later versions record the live table when a
   prior PR URL exists and the fingerprint annotation is missing, then
   skip. Confirm the annotation is present:

   ```bash
   kubectl get attunepolicy -A -o jsonpath='{range .items[*]}{.metadata.namespace}{"/"}{.metadata.name}{" drift="}{.metadata.annotations.attune\.io/gitops-pr-drift}{" url="}{.metadata.annotations.attune\.io/gitops-pr-url}{"\n"}{end}'
   ```

   If `url` is set and `drift` is empty after upgrade, Flux/Argo may be
   replacing operator annotations. Current Attune also stores the
   fingerprint on `status.gitopsPR`, which GitOps apply does not
   replace. Helm does not update CRDs on `helm upgrade`; apply
   `crds.yaml` or the API server prunes `status.gitopsPR` (see
   [Upgrading](upgrading.md)). Leave the last PR open or set
   `dryRun: true` only if `status.gitopsPR.driftFingerprint` is also
   empty. Also print:

   ```bash
   kubectl get attunepolicy -A -o jsonpath='{range .items[*]}{.metadata.namespace}{"/"}{.metadata.name}{" fp="}{.status.gitopsPR.driftFingerprint}{" url="}{.status.gitopsPR.url}{"\n"}{end}'
   ```

### GitOps PR failing

**Symptom**: Status condition `GitOpsPullRequest` is `False` with reason
`PullRequestFailed` or `GitOpsEndpointBlocked`, Events or metrics show
`attune_gitops_pr_total{result="failed"}`, or the `AttuneGitOpsPRFailures`
alert fires.

**Cause**: Opt-in `updateStrategy.export.pullRequest` could not create or
update a PR. Common cases:

1. Token Secret missing, wrong key, or RBAC cannot read Secrets.
2. Invalid `provider` / `repository` / optional `apiUrl`. Preflight
   rejects a non-HTTPS URL, userinfo, loopback, link-local (including
   IMDS), or a private RFC1918/ULA host when
   `allowPrivateEndpoints` is false. Dial-time SSRF still blocks
   loopback, link-local, and IMDS even when
   `allowPrivateEndpoints: true`. Set that flag only for self-hosted
   forges on RFC1918/ULA. GitOps `apiUrl` is stricter than Prometheus:
   in-cluster loopback and metadata addresses stay blocked.
3. Forge API error (auth, branch protection, missing head branch before
   bootstrap, rate limits). Status is the static message
   `PR API request failed` (no API body).

Match the condition **message** to the check that failed:

| Message | When |
|---------|------|
| `apiUrl is not an allowed HTTPS host` | Preflight on `apiUrl` (reason `GitOpsEndpointBlocked`) |
| `GitOps API URL resolved to a disallowed address` | Dial-time SSRF (reason `GitOpsEndpointBlocked`) |
| `PR API request failed` | Forge API error after the URL was allowed (reason `PullRequestFailed`) |

**Fix**:

1. Inspect the condition message (never paste tokens into tickets):

   ```bash
   kubectl get attunepolicy <name> -n <ns> \
     -o jsonpath='{range .status.conditions[?(@.type=="GitOpsPullRequest")]}{.reason}{" "}{.message}{"\n"}{end}'
   ```

2. Confirm Secret name/key and that the operator ServiceAccount can `get`
   that Secret.
3. Use `dryRun: true` first to validate drift detection without forge calls.
4. Check PromQL:

   ```promql
   sum by (namespace, policy, result) (rate(attune_gitops_pr_total[1h]))
   ```

See [GitOps integration: pull request automation](gitops-integration.md#pull-request-automation-opt-in-phase-b).

## Helm install ImagePullBackOff

**Symptom:** After `helm install` from `oci://ghcr.io/attune-io/charts/attune`,
the operator pod stays `0/1` with `ErrImagePull` or `ImagePullBackOff` for
`ghcr.io/attune-io/attune:0.1.x` (no `v` prefix).

**Cause:** Chart `appVersion` is SemVer without `v`. Releases through
0.1.23 published only `vX.Y.Z`. Chart 0.1.23 used bare `appVersion` as
the image tag when `image.tag` was empty.

**Fix:** Pin a published tag, then upgrade:

```bash
helm upgrade attune oci://ghcr.io/attune-io/charts/attune \
  --namespace attune-system \
  --reuse-values \
  --set image.tag=v0.1.23
```

From 0.1.24, releases publish both `vX.Y.Z` and `X.Y.Z`. You can still
override `image.tag` for local or E2E images.

## Debug commands

Operator logs:

```bash
kubectl -n attune-system logs -l app.kubernetes.io/name=attune --tail=100
```

List all policies with status:

```bash
kubectl get attunepolicy --all-namespaces -o wide
```

Inspect a specific policy in detail:

```bash
kubectl describe attunepolicy <name>
```

Check operator metrics:

```bash
kubectl -n attune-system port-forward svc/attune-metrics 8080:8080 >/tmp/attune-metrics-pf.log 2>&1 &
PF_PID=$!
trap 'kill "$PF_PID" 2>/dev/null || true' EXIT
curl -s localhost:8080/metrics | grep attune
```
