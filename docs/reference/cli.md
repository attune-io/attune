The `kubectl-rightsize` plugin provides quick access to policy status,
savings estimates, per-container recommendations, and recommendation
reasoning.

## Installation

```bash
# Build from source
make build-plugin

# Copy to your PATH
sudo cp bin/kubectl-rightsize /usr/local/bin/
```

## Commands

### status

Shows all policies with their conditions, workload counts, and age.

```bash
kubectl rightsize status
kubectl rightsize status -n production
kubectl rightsize status -A
```

| Column | Description |
|--------|-------------|
| PENDING | Workloads with active recommendations that are still awaiting resize |
| READY | Current `Ready` reason (`Monitoring`, `InsufficientData`, `PrometheusUnavailable`, `InvalidConfig`, or `WorkloadDiscoveryFailed`), or the current `Ready` condition message when `Ready=False` includes actionable details |
| RESIZING | `InProgress`, `Idle`, `CooldownActive`, or `-` (non-resize modes) |
| DEGRADED | `HighRevertRate` or `-` |

### savings

Shows aggregate CPU and memory savings per policy with estimated monthly
cost savings.

```bash
kubectl rightsize savings
kubectl rightsize savings -n production
```

| Column | Description |
|--------|-------------|
| NAMESPACE | Namespace of the policy |
| NAME | Policy name |
| CPU SAVED | Total CPU request reduction (e.g., `350m`) |
| MEMORY SAVED | Total memory request reduction (e.g., `232Mi`) |
| % SAVED | CPU savings as percentage of total CPU requests |
| EST. MONTHLY | Estimated monthly cost savings (e.g., `$12.78`) |

### recommendations

Shows per-container current vs recommended values with confidence scores.
When a policy is still collecting data, the last column shows the current
status message instead.

```bash
kubectl rightsize recommendations
kubectl rightsize recommendations -n production
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

### explain

Shows the stored recommendation reasoning for a single policy, including
percentile selection, safety margin, confidence adjustment, bounds, and
change filtering for CPU and memory.

```bash
kubectl rightsize explain -n production api-services
```

`explain` requires both a policy name and a single namespace. Put flags before the policy name, for example `kubectl rightsize explain -n production api-services`.

### history

Shows past resize operations with timestamps, before/after values, and outcomes.

```bash
kubectl rightsize history
kubectl rightsize history -n production
```

| Column | Description |
|--------|-------------|
| NAMESPACE | Namespace of the policy |
| POLICY | Name of the RightSizePolicy |
| TIMESTAMP | When the resize occurred |
| WORKLOAD | Name of the resized workload |
| CONTAINER | Container that was resized |
| RESOURCE | `cpu`, `memory`, or `cpu+memory` |
| FROM | Previous resource value |
| TO | New resource value |
| METHOD | `InPlace` or `Eviction` |
| RESULT | `Success`, `Failed`, `Reverted`, or `Evicted` |

### version

Shows the plugin version. Works without cluster access.

```bash
kubectl rightsize version
```

## Structured output

`--output` / `-o` prints the raw `RightSizePolicy` objects returned by the
cluster as JSON or YAML. The selected command still controls namespace
resolution, but structured output is not command-specific.

```bash
kubectl rightsize status -o json
kubectl rightsize status -A -o yaml
```

## Flags

| Flag | Short | Description |
|------|-------|-------------|
| `--namespace` | `-n` | Target namespace (defaults to current context) |
| `--all-namespaces` | `-A` | List across all namespaces |
| `--kubeconfig` | | Path to kubeconfig file |
| `--output` | `-o` | Output raw `RightSizePolicy` objects as `json` or `yaml` (not command-specific) |

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