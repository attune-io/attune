# Troubleshooting

## Common conditions

Check the policy's conditions for a quick diagnosis:

```bash
kubectl get rsp <name> -o jsonpath='{.status.conditions}' | jq .
```

### PrometheusUnavailable

**Symptom**: Ready condition is `False` with reason `PrometheusUnavailable`.

**Cause**: `PrometheusUnavailable` means the controller could not use
Prometheus for this reconcile. The condition message tells you which step
failed:

- `Cannot resolve Prometheus config` means address resolution failed. The
  operator checks (in order): policy spec, RightSizeNamespaceDefaults,
  RightSizeDefaults, Prometheus Operator CRD, then well-known service names.
- `Cannot create metrics collector`, `reading secret`, or transport errors
  like `TLS handshake timeout` mean the address was found but auth, headers,
  bearer token secret, CA bundle, or TLS setup failed.
- `Prometheus query timeout exceeded` means the reconcile-level timeout
  expired before all Prometheus queries completed.
- `Prometheus query errors (` means Prometheus answered, but one or more
  metric queries failed. This can still happen when Prometheus is reachable.

If the condition message includes `Cannot resolve Prometheus config: SSRF blocked`,
the configured address points at `localhost`, `127.0.0.1`, `::1`, or a
cloud metadata endpoint. Replace it with the in-cluster Prometheus Service
DNS name or ClusterIP. A local `kubectl port-forward` URL on your workstation
will not work.

**Fix address resolution failures**:

1. Set the address explicitly in a `RightSizeDefaults` resource:

    ```yaml
    apiVersion: rightsize.io/v1alpha1
    kind: RightSizeDefaults
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
    kubectl run prom-test --image=curlimages/curl --restart=Never --rm -it -- \
      curl -sf http://prometheus-server.monitoring:80/-/healthy
    ```

If the condition message includes `Cannot create metrics collector`,
`reading secret`, or a transport error like `TLS handshake timeout`,
verify the credentials and connection details before changing timeouts:

1. Check the referenced Secret exists and contains the expected bearer token.
2. Re-check custom headers, CA bundle, and `insecureSkipVerify` settings.
3. Test the exact Prometheus URL from inside the cluster with the same auth
   mechanism the operator uses.

If the condition message includes `Prometheus query timeout exceeded`, the
operator's reconcile-level timeout expired before all workload queries
completed. This typically happens when Prometheus is slow to respond
(not down, just overloaded) or when a policy targets many workloads.

**Fix query timeouts**:

1. Increase the timeout: set Helm `prometheusTimeout: "10m"` (or
   `--prometheus-timeout=10m`).
2. Reduce per-query cost: decrease `historyWindow` or increase `queryStep`
   on the RightSizePolicy or RightSizeDefaults.
3. Check Prometheus health: high query latency often indicates Prometheus
   itself needs more resources or recording rules.

If the condition message includes `Prometheus query errors (`, Prometheus was
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
    kubectl run prom-check --image=curlimages/curl --restart=Never --rm -it -- \
      curl -s 'http://prometheus-server.monitoring:80/api/v1/query?query=container_cpu_usage_seconds_total' \
      | head -c 200
    ```

2. If the result is empty (`"result":[]`), cadvisor scraping is not
   configured. Check your Prometheus scrape configuration for a
   `kubernetes-nodes-cadvisor` or equivalent job.

3. If the result has data but the operator still reports 0 data points,
   check that the `namespace`, `pod`, and `container` label names match.
   Some Prometheus configurations relabel these.

### InsufficientData

**Symptom**: Ready condition is `False` with reason `InsufficientData`.

**Cause**: Not enough Prometheus data points to generate recommendations.
The default minimum is 48 Prometheus range-query samples. With the default
`queryStep: 5m`, that is about 4 hours of data.

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
`Failed to fetch defaults: listing RightSizeNamespaceDefaults ...`.

**Fix**:

1. Check whether the operator can list `RightSizeDefaults` and
   `RightSizeNamespaceDefaults`.
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
   `DaemonSet`, `CronJob`, or `Job`.
2. If you use `targetRef.name`, confirm the workload exists in the same
   namespace as the policy.
3. If you use `targetRef.selector`, confirm it matches at least one workload
   and includes real `matchLabels` or `matchExpressions` entries.
4. Check operator logs for the exact discovery error if the target still
   looks correct.

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

## Webhook / cert-manager issues

### Webhook connection refused

**Symptom**: `kubectl apply -f policy.yaml` returns:

```
Error from server (InternalError): Internal error occurred: failed calling
webhook "vrightsizepolicy.kb.io": Post "https://...": dial tcp ...: connection refused
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
    kubectl apply -f https://github.com/cert-manager/cert-manager/releases/download/v1.17.2/cert-manager.yaml
    kubectl wait --for=condition=Available deployment/cert-manager-webhook -n cert-manager --timeout=120s
    ```

3. Check the Certificate status:

    ```bash
    kubectl get certificate -n kube-rightsize-system
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

### ResourceQuota exceeded

**Symptom**: Operator logs `Skipping resize: quota/limitrange violation`
with a message mentioning `exceed ResourceQuota`.

**Cause**: The resize would increase CPU or memory requests beyond the
remaining headroom in the namespace's ResourceQuota.

**Fix**:

1. Check current quota usage:

    ```bash
    kubectl get resourcequota -n <namespace>
    ```

2. Either increase the quota limits, or tighten the policy's resource
   bounds so recommendations stay within quota.

## Revert issues

### High revert rate

**Symptom**: `Degraded` condition is `True` with reason `HighRevertRate`, or
multiple entries in `.status.resizeHistory` show `result: Reverted`.

**Cause**: 3+ of the last 5 resize operations were reverted due to safety
violations. The controller applies exponential backoff (2x cooldown per
consecutive revert, capped at 16x).

Check the current backoff state:

```bash
kubectl get rsp <name> -o jsonpath='{.status.cooldown}'
# Example: {"backoffMultiplier":8,"consecutiveReverts":3,"effectiveCooldown":"8h0m0s"}
```

**Fix**: Investigate the revert reasons:

```bash
kubectl get rsp <name> -o jsonpath='{.status.resizeHistory}' | \
  jq '[.[] | select(.result=="Reverted")]'
```

Common causes:

- **oomkill**: safety margin is too low for memory. Increase `memory.safetyMargin`.
- **throttle**: CPU throttle ratio exceeded 50% post-resize. Increase `cpu.safetyMargin`.
- **restart**: the application crashes at the new resource level. Check application logs.
- **notready**: readiness probe fails post-resize. Verify probe configuration.

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
admission webhook "validation.rightsize.io" denied the request:
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
admission webhook "validation.rightsize.io" denied the request:
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

## Deleting a policy

When you delete a `RightSizePolicy`, the operator uses a
`rightsize.io/cleanup` finalizer to clean up before the resource is
garbage-collected:

1. **Annotations removed**: all tracking annotations (`rightsize.io/resized-at`,
   `rightsize.io/policy`, etc.) and the `rightsize.io/tracked` label are
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
kubectl logs -n kube-rightsize-system deploy/kube-rightsize-controller-manager | grep "deletion cleanup"
```

## Large cluster performance

### Stale recommendations (slow reconciliation)

If `workqueue_depth` is consistently > 0 and
`workqueue_longest_running_processor_seconds` climbs, the operator cannot
keep up with the reconcile queue. Solutions (in order of impact):

1. **Increase `maxConcurrentReconciles`** (or use a `clusterSize` preset).
2. **Scope with `--watch-namespaces`** to reduce informer cache size.
3. Policies targeting many workloads via label selector now process up to
   10 workloads in parallel per reconcile cycle.

See the [Scaling Guide](scaling.md) for tuning details and preset values.

### High memory usage

If the operator pod is OOMKilled or uses unexpectedly high memory, the
informer cache may be caching too many objects. Use `--watch-namespaces`
to limit the cache to the namespaces where your policies exist.

## Resizes skipped due to stale recommendations

When Prometheus does not return fresh data during a reconcile cycle, the
operator marks the recommendation as **stale** and skips the resize to avoid
acting on outdated metrics. You will see this in the operator logs:

```
Skipping resize for workload with stale recommendation  workload=my-app
```

The `kube_rightsize_stale_recommendations_total` counter increments each
time this happens. Common causes:

1. **Prometheus is temporarily unavailable** or responding slowly.
2. **The `historyWindow` is too short** for the workload's scrape interval,
   so range queries return no data.
3. **Pod label changes** caused the PromQL regex to stop matching.

To diagnose, enable debug logging and check the Prometheus query results:

```bash
kubectl logs -n kube-rightsize-system deploy/kube-rightsize-controller-manager \
  | grep -E "stale|Prometheus query returned no data"
```

Resizes resume automatically once fresh data is available.

## Deployment-owned ReplicaSet targeting

If a `RightSizePolicy` targets a ReplicaSet that is owned by a Deployment,
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
helm upgrade kube-rightsize kube-rightsize/kube-rightsize \
  --set logging.level=debug

# Enable verbose trace logs (V(2): per-sample data, full recommendation chain)
helm upgrade kube-rightsize kube-rightsize/kube-rightsize \
  --set logging.level=2
```

You can also switch to human-readable text format for local debugging:

```bash
helm upgrade kube-rightsize kube-rightsize/kube-rightsize \
  --set logging.level=debug --set logging.format=text
```

Revert to normal after debugging:

```bash
helm upgrade kube-rightsize kube-rightsize/kube-rightsize \
  --set logging.level=info
```

## Debug commands

Operator logs:

```bash
kubectl -n kube-rightsize-system logs -l app.kubernetes.io/name=kube-rightsize --tail=100
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
kubectl -n kube-rightsize-system port-forward svc/kube-rightsize-metrics 8080:8080 &
curl -s localhost:8080/metrics | grep kube_rightsize
```
