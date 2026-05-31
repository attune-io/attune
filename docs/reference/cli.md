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
```

| Flag | Description |
|------|-------------|
| `-w`, `--watch` | Continuously refresh the status table every 10 seconds. Press Ctrl+C to stop. Useful during initial data collection to track progress without manually re-running the command. |
| `--sort-by` | Sort output by field: `name`, `namespace`, `savings`, or `age`. |
| `--filter` | Filter policies by Ready condition reason: `degraded`, `pending`, `collecting`, `ready`, or `noworkloads`. |

| Column | Description |
|--------|-------------|
| PENDING | Workloads with active recommendations that are still awaiting resize |
| READY | Current `Ready` reason (`Monitoring`, `InsufficientData`, `NoWorkloadsFound`, `PrometheusUnavailable`, `InvalidConfig`, `WorkloadDiscoveryFailed`, or `Paused`), or the current `Ready` condition message when `Ready=False` includes actionable details |
| RESIZING | `InProgress`, `Idle`, `CooldownActive`, or `-` (non-resize modes) |
| DEGRADED | `HighRevertRate` or `-` |
| CANARY | Canary phase and pod count (e.g., `CanaryInProgress (2 pods)`) when mode is Canary, `-` otherwise |
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
to preview what changes would be applied.

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

Shows per-container current vs recommended values with confidence scores.
When a policy is still collecting data, the last column shows the current
status message instead. When any policy uses export mode, a footer note points
to `kubectl attune export list` for the GitOps ConfigMap view and last-export timestamps.

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
| CONFIDENCE / STATUS | Confidence percentage when recommendations exist, otherwise the current `Ready` message or reason |

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
`minimumDataPoints`, `historyWindow`, `resizeMethod`, `autoRevert`,
`initialSizing`, `maxConcurrentResizes`, `rateWindow`, `export`,
budget caps (`maxTotalCPUIncrease`, `maxTotalMemoryIncrease`), and per-resource
fields (`percentile`, `overhead`, `minAllowed`, `maxAllowed`,
`controlledValues`, `allowDecrease`, `burstSensitivity`,
`maxChangePercent`, `maxIncreasePercent`, `maxDecreasePercent`,
`memoryFromCpuRatio`). Each value shows
whether it came from the policy, a namespace default, a cluster
default, or the built-in default. When export mode + Recommend/Observe is active, a note explains the GitOps implications.

```bash
kubectl attune explain -n production api-services
```

`explain` requires both a policy name and a single namespace. Put flags before the policy name, for example `kubectl attune explain -n production api-services`.

### diff

Shows resource change recommendations in diff format for GitOps workflows. Outputs the difference between current and recommended resources for each workload.

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
| REASON | Why a resize was reverted or failed (`oomkill`, `restart`, `notready`, `throttle`, etc.). Shows `-` for successful resizes. |

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
confirmation.

The wizard does not support multi-cluster mode (`--all-contexts` /
`--contexts`).

### version

Shows the plugin version. Works without cluster access.

```bash
kubectl attune version
```

## Structured output

`--output` / `-o` is supported with the `status` command (JSON or YAML) and the
`diff` command (YAML only). It prints the raw `AttunePolicy` objects returned by
the cluster.

```bash
kubectl attune status -o json
kubectl attune status -A -o yaml
```

For other commands, use the human-oriented plugin output, or fetch raw objects
with `kubectl get attunepolicy -o json|yaml`.

## Flags

| Flag | Short | Description |
|------|-------|-------------|
| `--namespace` | `-n` | Target namespace (defaults to current context) |
| `--all-namespaces` | `-A` | List across all namespaces |
| `--kubeconfig` | | Path to kubeconfig file |
| `--output` | `-o` | Output raw `AttunePolicy` objects as `json` or `yaml` (`status` and `diff`) |
| `--watch` | `-w` | Continuously refresh status every 10 seconds (`status` only) |
| `--sort-by` | | Sort output: `name`, `namespace`, `savings`, `age` (`status` and `savings` only) |
| `--filter` | | Filter by condition: `degraded`, `pending`, `collecting`, `ready`, `noworkloads` (`status` only) |
| `--all-contexts` | | Query all kubeconfig contexts and merge results (`status`, `savings`, `recommendations`, `history` only) |
| `--contexts` | | Comma-separated list of specific kubeconfig contexts to query (same commands as `--all-contexts`) |

## Manager Binary Flags

The operator manager binary (`cmd/manager`) accepts these flags. They are
typically set via the Helm chart `values.yaml` rather than directly.

| Flag | Default | Description |
|------|---------|-------------|
| `--metrics-bind-address` | `:8080` | Address the Prometheus metrics endpoint binds to |
| `--health-probe-bind-address` | `:8081` | Address the health/readiness probe endpoint binds to |
| `--leader-elect` | `false` | Enable leader election (required for HA with multiple replicas) |
| `--enable-webhooks` | `true` | Enable admission webhooks for defaulting and validation (requires cert-manager) |
| `--collector-ttl` | `10m` | How long unused Prometheus collectors stay cached before eviction |
| `--zap-log-level` | `info` | Log verbosity: `debug`, `info`, `error`, or integer (higher = more verbose) |
| `--zap-encoder` | `json` | Log format: `json` (default) or `console` (human-readable) |
| `--zap-stacktrace-level` | `error` | Minimum level for automatic stacktrace capture |
| `--zap-devel` | `false` | Enable development mode (console encoder, debug level, stacktrace on warn) |
| `--prometheus-qps` | `10` | Maximum Prometheus queries per second across all policies |
| `--prometheus-burst` | `20` | Maximum burst of Prometheus queries above the QPS limit |
| `--prometheus-timeout` | `5m` | Maximum time for all Prometheus queries in a single reconciliation |
| `--max-concurrent-reconciles` | `1` | Number of policies reconciled concurrently |
| `--watch-namespaces` | (all) | Comma-separated list of namespaces to watch (empty = all namespaces) |