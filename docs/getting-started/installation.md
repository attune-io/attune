# Installation

## Prerequisites

| Requirement | Minimum Version |
|-------------|-----------------|
| Kubernetes  | 1.32+           |
| Helm        | 3.16+ or 4.x    |
| Prometheus  | 2.x (with `container_cpu_usage_seconds_total` and `container_memory_working_set_bytes`) |
| cert-manager | 1.12+ (for webhook TLS; optional if installing with `--set webhooks.enabled=false`) |

!!! note "In-Place Pod Resize"
    Kubernetes 1.32+ is required because Attune uses the
    [In-Place Pod Resize](https://kubernetes.io/blog/2025/05/16/kubernetes-v1-33-in-place-pod-resize-beta/)
    `/resize` subresource, which was added in 1.32.
    On **1.32**, you must enable the `InPlacePodVerticalScaling` feature gate
    on the apiserver, controller-manager, scheduler, and all kubelets.
    On **1.33+**, the feature is enabled by default (beta).

## Install with Helm (recommended)

Create a namespace and install the chart from the OCI registry:

```bash
kubectl create namespace attune-system

helm install attune \
  oci://ghcr.io/attune-io/charts/attune \
  --namespace attune-system
```

!!! tip "Prometheus address"
    The Prometheus address is configured per-policy in
    `AttunePolicy.spec.metricsSource.prometheus.address`, per namespace via
    the `AttuneNamespaceDefaults` CRD, or globally via the
    `AttuneDefaults` CRD. It is not a Helm chart value.
    If neither is set, the operator auto-discovers Prometheus by checking
    for the Prometheus Operator CRD, then well-known service names
    (`prometheus-server`, `prometheus-kube-prometheus-prometheus`) in
    common namespaces.

### Upgrading

!!! important "CRDs are not updated by `helm upgrade`"
    Helm's `crds/` directory only installs CRDs on `helm install`.
    Before upgrading, apply the latest CRDs manually:

    ```bash
    kubectl apply --server-side --force-conflicts -f \
      https://github.com/attune-io/attune/releases/latest/download/crds.yaml
    ```

```bash
helm upgrade attune \
  oci://ghcr.io/attune-io/charts/attune \
  --namespace attune-system
```

## Install with raw manifests

If you prefer not to use Helm, apply the static install manifest:

```bash
kubectl create namespace attune-system

kubectl apply -f \
  https://github.com/attune-io/attune/releases/latest/download/install.yaml
```

!!! warning
    The Prometheus address is configured per-policy in
    `AttunePolicy.spec.metricsSource.prometheus.address`, per namespace via
    `AttuneNamespaceDefaults`, or globally via the `AttuneDefaults` CRD.
    Auto-discovery is also available
    if neither is set (see the Helm installation tip above).

## Verify the installation

Check that the operator pod is running:

```bash
kubectl -n attune-system get pods
```

Expected output:

```text
NAME                              READY   STATUS    RESTARTS   AGE
attune-6f8b4c7d9f-xk2pq  1/1     Running   0          30s
```

Verify that the three CRDs are registered:

```bash
kubectl get crds | grep attune
```

```text
attunedefaults.attune.io             2026-01-15T00:00:00Z
attunenamespacedefaults.attune.io    2026-01-15T00:00:00Z
attunepolicies.attune.io             2026-01-15T00:00:00Z
```

## Next steps

Head to the [Quick Start](quickstart.md) to create your first AttunePolicy.
