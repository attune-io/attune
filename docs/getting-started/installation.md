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

!!! info "Also available on Docker Hub"
    The container image is also published to Docker Hub at
    `docker.io/attuneio/attune` for discoverability. For production
    use, GHCR is recommended (no rate limits on public packages).

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

## Install with OLM (OperatorHub)

If your cluster has the [Operator Lifecycle Manager](https://olm.operatorframework.io/)
installed, you can install Attune directly from
[OperatorHub.io](https://operatorhub.io/operator/attune).

**On OpenShift**, the operator is available in the built-in OperatorHub catalog.
Search for "Attune" in the web console under **Operators > OperatorHub** and
click Install. You can also browse the listing on the
[Red Hat Ecosystem Catalog](https://catalog.redhat.com/software/search?target_platforms=Operator&q=attune).
See the [OpenShift guide](../guides/openshift.md) for TLS profile integration
and OpenShift-specific configuration.

**On other clusters with OLM**, install the operator by creating a
`Subscription`:

```bash
# Ensure OLM is installed (https://olm.operatorframework.io/docs/getting-started/)
# Then create the subscription:
kubectl create -f https://operatorhub.io/install/attune.yaml
```

This subscribes to the `stable` channel and auto-updates when new versions
are published. The OLM bundle includes all CRDs, RBAC, and the operator
deployment.

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

## kubectl plugin (optional)

The `kubectl attune` plugin provides quick access to recommendations,
savings estimates, and resize history.

```bash
# Install via Krew (recommended)
kubectl krew install attune

# Or build from source
make build-plugin && sudo cp bin/kubectl-attune /usr/local/bin/
```

See the [CLI Reference](../reference/cli.md) for available commands.

## Next steps

Head to the [Quick Start](quickstart.md) to create your first AttunePolicy.
