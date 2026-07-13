# Istio Integration

Attune works with all three Istio deployment models. This guide
covers configuration for each mode.

## Deployment models

| Istio Mode | How it works | Operator config |
|---|---|---|
| **Sidecar** (traditional) | Injects `istio-proxy` as a regular container | Auto-excluded by default (`excludeKnownSidecars: true`) |
| **Ambient** (ztunnel, GA in Istio 1.24+) | Per-node DaemonSet, no per-pod sidecar | No special config needed |
| **Native sidecar** (`ENABLE_NATIVE_SIDECARS=true`) | Injects `istio-proxy` as init container with `restartPolicy: Always` | Auto-excluded by default (same known list) |

## Sidecar mode (traditional)

Istio injects `istio-proxy` as a regular container. Attune **auto-excludes**
`istio-proxy` by default (`excludeKnownSidecars` defaults to `true`), so a
normal policy already right-sizes only your application containers:

```yaml
apiVersion: attune.io/v1alpha1
kind: AttunePolicy
metadata:
  name: my-app
spec:
  targetRef:
    kind: Deployment
    name: my-app
  # excludeKnownSidecars: true  # default; istio-proxy is skipped automatically
  metricsSource:
    prometheus:
      address: http://prometheus-server.monitoring:80
  cpu:
    percentile: 95
    overhead: "20"
  memory:
    percentile: 99
    overhead: "30"
  updateStrategy:
    type: Auto
```

The operator will compute recommendations and resize only your application
containers, leaving `istio-proxy` untouched.

### Right-size the proxy on purpose

To restore older behavior (size every container, including `istio-proxy`):

```yaml
spec:
  excludeKnownSidecars: false
```

To size `istio-proxy` but still skip other custom agents, set
`excludeKnownSidecars: false` and list only the names you still want
skipped in `excludedContainers`.

## Ambient mode

In ambient mode, Istio uses a per-node ztunnel DaemonSet instead of
per-pod sidecars. There is no `istio-proxy` container in your pods, so
Attune works transparently with no special configuration.

## Native sidecar mode

When Istio is configured with `ENABLE_NATIVE_SIDECARS=true` (requires
Kubernetes 1.28+), it injects `istio-proxy` as an init container with
`restartPolicy: Always`. These "native sidecar" containers run for the
pod's lifetime and are visible to attune.

The operator automatically detects native sidecar containers (init
containers with `restartPolicy=Always`) and includes them in workload
analysis. The known-sidecar list still matches on name, so `istio-proxy`
is auto-excluded by default (same as traditional sidecar mode). Set
`excludeKnownSidecars: false` only if you intentionally want Attune to
right-size the proxy.

The operator's safety monitor also checks native sidecar container
statuses for OOMKill and restart spike detection.

## Metrics considerations

When using Istio's sidecar (traditional or native), Prometheus metrics
for `container_cpu_usage_seconds_total` and
`container_memory_working_set_bytes` include the `istio-proxy` container.
The operator queries metrics per-container, so excluded containers do not
affect recommendations for your application containers.