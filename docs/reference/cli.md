The `kubectl-rightsize` plugin provides quick access to policy status,
savings estimates, and per-container recommendations.

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
| READY | `Monitoring`, `InsufficientData`, `PrometheusUnavailable`, or `InvalidConfig` |
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
| CPU SAVED | Total CPU request reduction (e.g., `350m`) |
| MEMORY SAVED | Total memory request reduction (e.g., `232Mi`) |
| EST. MONTHLY | Estimated monthly cost savings (e.g., `$12.78`) |

### recommendations

Shows per-container current vs recommended values with confidence scores.

```bash
kubectl rightsize recommendations
kubectl rightsize recommendations -n production
```

### history

Shows past resize operations with timestamps, before/after values, and outcomes.

```bash
kubectl rightsize history
kubectl rightsize history -n production
```

| Column | Description |
|--------|-------------|
| TIMESTAMP | When the resize occurred |
| WORKLOAD | Name of the resized workload |
| CONTAINER | Container that was resized |
| RESOURCE | `cpu` or `memory` |
| FROM | Previous resource value |
| TO | New resource value |
| RESULT | `Success`, `Failed`, or `Reverted` |

### version

Shows the plugin version. Works without cluster access.

```bash
kubectl rightsize version
```

## Structured output

All commands support `-o json` and `-o yaml`:

```bash
kubectl rightsize status -o json
kubectl rightsize recommendations -o yaml
```

## Flags

| Flag | Short | Description |
|------|-------|-------------|
| `--namespace` | `-n` | Target namespace (defaults to current context) |
| `--all-namespaces` | `-A` | List across all namespaces |
| `--kubeconfig` | | Path to kubeconfig file |
| `--output` | `-o` | Output format: `json` or `yaml` |