## Prerequisites

| Requirement | Minimum Version |
|-------------|-----------------|
| Kubernetes  | 1.33+           |
| Helm        | 3.16+           |
| Prometheus  | 2.x (with `container_cpu_usage_seconds_total` and `container_memory_working_set_bytes`) |

!!! note "In-Place Pod Resize"
    Kubernetes 1.33+ is required because kube-rightsize uses the
    [In-Place Pod Resize](https://kubernetes.io/blog/2025/12/19/kubernetes-v1-35-in-place-pod-resize-ga/)
    feature, which reached GA in that release. Older clusters are not supported.

## Install with Helm (recommended)

Create a namespace and install the chart from the OCI registry:

```bash
kubectl create namespace kube-rightsize-system

helm install kube-rightsize \
  oci://ghcr.io/sebtardif/charts/kube-rightsize \
  --namespace kube-rightsize-system
```

!!! tip "Prometheus address"
    The Prometheus address is configured per-policy in
    `RightSizePolicy.spec.metricsSource.prometheus.address`, or globally
    via the `RightSizeDefaults` CRD. It is not a Helm chart value.
    If neither is set, the operator auto-discovers Prometheus by checking
    for the Prometheus Operator CRD, then well-known service names
    (`prometheus-server`, `prometheus-kube-prometheus-prometheus`) in
    common namespaces.

### Upgrading

```bash
helm upgrade kube-rightsize \
  oci://ghcr.io/sebtardif/charts/kube-rightsize \
  --namespace kube-rightsize-system
```

## Install with raw manifests

If you prefer not to use Helm, apply the static install manifest:

```bash
kubectl create namespace kube-rightsize-system

kubectl apply -f \
  https://github.com/SebTardif/kube-rightsize/releases/latest/download/install.yaml
```

!!! warning
    The Prometheus address is configured per-policy in
    `RightSizePolicy.spec.metricsSource.prometheus.address` or globally
    via the `RightSizeDefaults` CRD. Auto-discovery is also available
    if neither is set (see the Helm installation tip above).

## Verify the installation

Check that the operator pod is running:

```bash
kubectl -n kube-rightsize-system get pods
```

Expected output:

```text
NAME                              READY   STATUS    RESTARTS   AGE
kube-rightsize-6f8b4c7d9f-xk2pq  1/1     Running   0          30s
```

Verify that both CRDs are registered:

```bash
kubectl get crds | grep rightsize
```

```text
rightsizedefaults.rightsize.io    2026-01-15T00:00:00Z
rightsizepolicies.rightsize.io   2026-01-15T00:00:00Z
```

## Next steps

Head to the [Quick Start](quickstart.md) to create your first RightSizePolicy.
