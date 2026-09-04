The `kubectl-attune` plugin provides quick access to policy status,
savings estimates, per-container recommendations, and recommendation
reasoning.

## Installation

```bash
# Install via Krew (recommended)
kubectl krew install attune

# Or build from source
make build-plugin
sudo cp bin/kubectl-attune /usr/local/bin/
```

## Commands

### status

Shows all policies with their conditions, workload counts, and age.

```bash
kubectl attune status
kubectl attune status -n production
kubectl attune status -A
kubectl attune status --watch          # live-refresh every 10s
kubectl attune status --sort-by savings
kubectl attune status --filter pending
kubectl attune status --contexts prod-east,prod-west
kubectl attune status --all-contexts
kubectl attune savings --sort-by savings -A
```

| Flag | Description |
|------|-------------|
| `-w`, `--watch` | Continuously refresh the status table every 10 seconds. Press Ctrl+C to stop. Useful during initial data collection to track progress without manually re-running the command. |
| `--sort-by` | Sort output by field: `name`, `namespace`, `savings`, or `age`. |
| `--filter` | Filter policies by Ready condition reason: `degraded`, `pending`, `collecting`, `ready`, or `noworkloads`. |

| Column | Description |
|--------|-------------|
| PENDING | Workloads with active recommendations that are still awaiting resize |
| READY | Current `Ready` reason (`Monitoring`, `InsufficientData`, `NoWorkloadsFound`, `MetricsUnavailable` (alias `PrometheusUnavailable`), `InvalidConfig`, `WorkloadDiscoveryFailed`, `ConflictCheckFailed`, or `Paused`), or the current `Ready` condition message when `Ready=False` includes actionable details |
| RESIZING | `InProgress`, `Idle`, `CooldownActive`, or `-` (non-resize modes) |
| DEGRADED | `HighRevertRate` or `-` |
| CANARY | Canary phase. With per-app rows: `CanaryInProgress (1/2 apps)`. Legacy: `CanaryInProgress (2 pods)`. `-` when mode is not Canary |
| EXPORT | `CM` when `export.configMap: true` (recommendations written to ConfigMaps for GitOps), `-` otherwise |

When any policy has per-workload errors, they are printed below the table
with the workload name and error message.

### savings

Shows aggregate CPU and memory savings per policy with estimated monthly
cost savings.

```bash
kubectl attune savings
kubectl attune savings -n production
```

| Column | Description |
|--------|-------------|
| NAMESPACE | Namespace of the policy |
| NAME | Policy name |
| CPU SAVED | Total CPU request reduction (e.g., `350m`) |
| MEMORY SAVED | Total memory request reduction (e.g., `232Mi`) |
| % SAVED | CPU savings as percentage of total CPU requests |
| EST. MONTHLY | Estimated monthly cost savings (e.g., `$12.78`) |

When multiple policies have savings data, a **TOTAL** row is appended
with aggregate CPU, memory, percentage, and estimated monthly savings.

The `--sort-by` flag also works with the `savings` command.

### preview

Shows a per-container comparison of current vs recommended resources for a
single policy. Use this before promoting from Recommend to Canary or Auto
to preview what changes would be applied. Workloads whose recommendation is
stale are skipped until Prometheus returns fresh data.

```bash
kubectl attune preview -n production api-services
```

| Column | Description |
|--------|-------------|
| WORKLOAD | Target workload name |
| CONTAINER | Container name |
| RESOURCE | `CPU` or `Memory` |
| CURRENT | Current resource request |
| RECOMMENDED | Recommended resource request |
| CHANGE | Delta description |

`preview` requires both a policy name and a single namespace.

### recommendations

Shows per-container current vs recommended values with a waste grade and
confidence scores. When a policy is still collecting data, GRADE is `-` and
the last column shows the current status message instead. When any policy
uses export mode, a footer note points to `kubectl attune export list` for
the GitOps ConfigMap view and last-export timestamps.

```bash
kubectl attune recommendations
kubectl attune recommendations -n production
```

| Column | Description |
|--------|-------------|
| NAMESPACE | Namespace of the policy |
| POLICY | Policy name |
| WORKLOAD | Target workload name |
| CONTAINER | Container name |
| CPU REQ | Current CPU request |
| CPU REC | Recommended CPU request |
| MEM REQ | Current memory request |
| MEM REC | Recommended memory request |
| GRADE | Request waste vs recommendation (worse of CPU or memory): A under 10% over (or up to 10% under), B 10-25%, C 25-50%, D 50-75%, F 75% or more. `U` when current is more than 10% below the recommendation, including a zero request. `U` wins if one resource is under-provisioned and the other is waste. `-` while collecting, when the workload rec is stale, or when quantities cannot be compared. |
| CONFIDENCE / STATUS | Confidence percentage when recommendations exist, `stale` when the workload rec is based on cached Prometheus data, otherwise the current `Ready` message or reason |

### export

Lists recommendation exports written to ConfigMaps when a policy has `updateStrategy.export.configMap: true`
(the primary GitOps integration pattern with ArgoCD, Flux, etc.).

```bash
kubectl attune export
kubectl attune export list
kubectl attune export list -n production
kubectl attune export list -A
```

The `LAST UPDATED` column shows the RFC3339 timestamp when the operator last wrote that workload's recommendations
(the value inside the ConfigMap's `last-updated` key). This is the authoritative handoff for GitOps pipelines.
A frozen `last-updated` is not fresh when `status.recommendations[].stale` is true: the operator does not rewrite
the ConfigMap or bump the timestamp until Prometheus returns new data.

| Column     | Description |
|------------|-------------|
| POLICY     | AttunePolicy that owns the export |
| WORKLOAD   | Workload name (e.g. Deployment name) |
| KIND       | Workload kind (Deployment, StatefulSet, etc.) |
| CONTAINERS | Number of containers with recommendations in the export |
| LAST UPDATED | When the ConfigMap was last refreshed by the operator |

`kubectl attune export` (no subcommand) is equivalent to `export list`. The output is the exact data your
GitOps system should consume; `kubectl attune recommendations` shows the same values from status (except
in Observe mode, where only the ConfigMaps are populated).

See the [GitOps Integration guide](https://github.com/attune-io/attune/blob/main/docs/guides/gitops-integration.md) for the full workflow.

### explain

Shows the stored recommendation reasoning for a single policy, including
percentile selection, overhead, confidence adjustment, bounds, and
change filtering for CPU and memory. It also prints the effective values for
all controller-applied defaults: `type`, `cooldown`, `queryStep`,
Metrics source (`prometheus` / `datadog` / `cloudwatch` / `vpa`),
`minimumDataPoints`, `historyWindow`, `resizeMethod`, `autoRevert`,
`initialSizing`, `maxConcurrentResizes`, `rateWindow`, `export`,
`templatePersistence`, budget caps (`maxTotalCPUIncrease`,
`maxTotalMemoryIncrease`), cost pricing (`cpuPerCoreHour`,
`memoryPerGiBHour`), and per-resource fields (`percentile`,
`overhead`, `minAllowed`, `maxAllowed`, `controlledValues`,
`allowDecrease`, `burstSensitivity`, `maxChangePercent`,
`maxIncreasePercent`, `maxDecreasePercent`, `memoryFromCpuRatio`).
Each value shows whether it came from the policy, a namespace default, a cluster
default, or the built-in default. When export mode + Recommend/Observe is active, a note explains the GitOps implications.

```bash
kubectl attune explain -n production api-services
```

`explain` requires both a policy name and a single namespace. Put flags before the policy name, for example `kubectl attune explain -n production api-services`.

### diff

Shows resource change recommendations in diff format for GitOps workflows. Outputs the difference between current and recommended resources for each workload. Workloads whose recommendation is stale are skipped until Prometheus returns fresh data.

```bash
kubectl attune diff
kubectl attune diff -n production
kubectl attune diff -o yaml    # structured YAML output
```

Useful for piping into ArgoCD or Flux review processes, or for manual review before promoting from Recommend to Auto mode.

### history

Shows past resize operations with timestamps, before/after values, and outcomes.

```bash
kubectl attune history
kubectl attune history -n production
```

| Column | Description |
|--------|-------------|
| NAMESPACE | Namespace of the policy |
| POLICY | Name of the AttunePolicy |
| TIMESTAMP | When the resize occurred |
| WORKLOAD | Name of the resized workload |
| CONTAINER | Container that was resized |
| RESOURCE | `cpu`, `memory`, or `cpu+memory` |
| FROM | Previous resource value |
| TO | New resource value |
| METHOD | `InPlace` or `Eviction` |
| RESULT | `Success`, `Failed`, `Reverted`, or `Evicted` |
| REASON | Why a resize was reverted or failed (`oomkill`, `restart`, `notready`, `throttle`, `slo:<name>`). Shows `-` for successful resizes. |

### wizard

Interactive guided workflow for creating and promoting policies. No flags
to memorize; the wizard walks through each decision.

```bash
kubectl attune wizard                # create a new policy
kubectl attune wizard promote        # promote an existing policy's mode
```

**Create flow**: selects namespace, workload kind, workload name,
auto-detects Prometheus, asks for CPU/memory percentiles and starting mode,
then offers to apply directly or save the YAML to a file.

**Promote flow**: lists existing policies with their current mode and
status, shows the recommendation summary, and updates the mode after
confirmation. Stale recommendations are skipped (same as `preview` and
`diff`) until Prometheus returns fresh data.

The wizard does not support multi-cluster mode (`--all-contexts` /
`--contexts`).

### doctor

Preflight for in-place resize. Checks the cluster before you expect
Attune to collect data or call `/resize`.

```bash
kubectl attune doctor
kubectl attune doctor -n production
kubectl attune doctor -A
```

Doctor is single-context. `--all-contexts` and `--contexts` are rejected.

| Check | Required | Passes when |
|-------|----------|-------------|
| Kubernetes version | Yes | Server version is 1.32 or newer |
| `pods/resize` | Yes | Discovery lists the `pods/resize` subresource |
| Prometheus | No | Skip-without-ping is `WARN` (`ok:false`), not Pass: no address was seen (none set, or listing failed with no objects). Also `WARN` for in-cluster DNS (`.svc` / `.cluster.local`) and for HTTP 401/403 on an address that sets `bearerTokenSecret` or `headers` (this host does not send the operator's auth). Other addresses, including `service.namespace` without `.svc`, are GET `/-/healthy` from this host (SSRF-checked first). Optional failures print `WARN`, not `FAIL`. |

Exit 0 when every required check passes. A failed Prometheus check is
printed as `WARN` and does not change the exit code. The ping runs on
the machine that invoked kubectl, not inside the operator pod.

### version

Shows the plugin version. Works without cluster access.

```bash
kubectl attune version
```

## Structured output

`--output` / `-o` is supported with:

- `status` only: `-o json` or `-o yaml` dumps raw `AttunePolicy` objects
- `diff`: `-o yaml` prints YAML patch manifests (not raw policy objects)
- `savings` and `recommendations`: CSV (header plus one data row per
  policy or container; no totals row). Recommendations CSV includes
  `grade` after `mem_rec` (same A-F bands as the table GRADE column,
  plus `U` when under-provisioned, or `-` while collecting or stale).
  `confidence_or_status` is the last column:
  a confidence percent when recommendations exist, otherwise the
  policy Ready reason (same as the table's CONFIDENCE / STATUS column).

```bash
kubectl attune status -o json
kubectl attune status -A -o yaml
kubectl attune savings -A -o csv
kubectl attune recommendations -n prod -o csv
```

For other commands, use the human-oriented plugin output, or fetch raw objects
with `kubectl get attunepolicy -o json|yaml`.

## Flags

| Flag | Short | Description |
|------|-------|-------------|
| `--namespace` | `-n` | Target namespace (defaults to current context) |
| `--all-namespaces` | `-A` | List across all namespaces |
| `--kubeconfig` | | Path to kubeconfig file |
| `--output` | `-o` | `status`: raw `AttunePolicy` objects as `json` or `yaml`. `diff`: YAML patch manifests (`-o yaml` only). `savings` and `recommendations`: `csv` |
| `--watch` | `-w` | Continuously refresh status every 10 seconds (`status` only) |
| `--sort-by` | | Sort output: `name`, `namespace`, `savings`, `age` (`status` and `savings` only) |
| `--filter` | | Filter by condition: `degraded`, `pending`, `collecting`, `ready`, `noworkloads` (`status` only) |
| `--all-contexts` | | Query all kubeconfig contexts and merge results (`status`, `savings`, `recommendations`, `history`, `diff`) |
| `--contexts` | | Comma-separated list of specific kubeconfig contexts to query (same commands as `--all-contexts`) |

```bash
kubectl attune status --sort-by savings
kubectl attune status --filter pending
kubectl attune status --contexts prod-east,prod-west
kubectl attune status --all-contexts
kubectl attune savings --sort-by savings -A
```

## Manager Binary Flags

The operator manager binary (`cmd/manager`) accepts these flags. They are
typically set via the Helm chart `values.yaml` rather than directly.

| Flag | Default | Description |
|------|---------|-------------|
| `--metrics-bind-address` | `:8080` | Address the Prometheus metrics endpoint binds to |
| `--health-probe-bind-address` | `:8081` | Address the health/readiness probe endpoint binds to |
| `--leader-elect` | `false` | Enable leader election (required for HA with multiple replicas) |
| `--enable-webhooks` | `true` | Enable admission webhooks for defaulting and validation (requires cert-manager) |
| `--collector-ttl` | `10m` | How long unused collectors (Prometheus, Datadog, CloudWatch) stay cached before eviction |
| `--zap-log-level` | `info` | Log verbosity: `debug`, `info`, `error`, or integer (higher = more verbose) |
| `--zap-encoder` | `json` | Log format: `json` (default) or `console` (human-readable) |
| `--zap-stacktrace-level` | `error` | Minimum level for automatic stacktrace capture |
| `--zap-devel` | `false` | Enable development mode (console encoder, debug level, stacktrace on warn) |
| `--prometheus-qps` | `10` | Maximum Prometheus queries per second across all policies |
| `--prometheus-burst` | `20` | Maximum burst of Prometheus queries above the QPS limit |
| `--prometheus-timeout` | `5m` | Maximum time for all Prometheus queries in a single reconciliation |
| `--max-concurrent-reconciles` | `1` | Number of policies reconciled concurrently |
| `--watch-namespaces` | (all) | Comma-separated list of namespaces to watch (empty = all namespaces) |