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
    [In-Place Pod Resize](https://kubernetes.io/blog/2025/12/19/kubernetes-v1-35-in-place-pod-resize-ga/)
    `/resize` subresource (added in 1.32 alpha).
    On **1.32**, you must enable the `InPlacePodVerticalScaling` feature gate
    on the apiserver, controller-manager, scheduler, and all kubelets.
    On **1.33–1.34**, the feature is enabled by default (beta).
    On **1.35+**, the feature is **GA** (stable).

## Install with Helm (recommended)

Create a namespace and install the chart from the OCI registry.

**cert-manager** is required by default (admission webhooks). Install it first,
or disable webhooks for a quick local trial:

```bash
# Option A: production path (webhooks on)
# Install cert-manager if needed: https://cert-manager.io/docs/installation/
kubectl create namespace attune-system

helm install attune \
  oci://ghcr.io/attune-io/charts/attune \
  --namespace attune-system

# Option B: skip webhooks (no cert-manager)
# helm install attune oci://ghcr.io/attune-io/charts/attune \
#   --namespace attune-system --create-namespace \
#   --set webhooks.enabled=false
```

!!! warning "Chart 0.1.23 pulls a missing image tag"
    The published 0.1.23 chart defaults to `ghcr.io/attune-io/attune:0.1.23`,
    which does not exist. Keep `--set image.tag=v0.1.23` if you install
    that chart. Chart 0.1.24 and newer can omit the pin. If the pod is
    already `ImagePullBackOff`, see
    [Helm install ImagePullBackOff](../guides/troubleshooting.md#helm-install-imagepullbackoff).

!!! tip "Large or multi-tenant clusters"
    Size the operator with a Helm `clusterSize` preset and, when policies
    live in only a few namespaces, set `watchNamespaces`. See the
    [Scaling Guide](../guides/scaling.md) ops checklist before production
    rollout.

!!! warning "NetworkPolicy and Prometheus port"
    The chart enables a NetworkPolicy by default. Egress to Prometheus is
    allowed only on `networkPolicy.prometheusPort` (default **9090**).
    Many examples use a Service URL on port **80**
    (`http://prometheus-server.monitoring:80`). If the Service port is 80
    (or anything other than 9090), set the chart value to match or you will
    see `MetricsUnavailable` / no data:

    ```bash
    helm upgrade attune oci://ghcr.io/attune-io/charts/attune \
      --namespace attune-system \
      --reuse-values \
      --set networkPolicy.prometheusPort=80
    ```

    For a first install on a restrictive cluster, you can temporarily set
    `--set networkPolicy.enabled=false` while you confirm connectivity.
    See the [Helm chart NetworkPolicy notes](https://github.com/attune-io/attune/blob/main/charts/attune/README.md#networkpolicy).

!!! tip "Prometheus address"
    The Prometheus address is configured per-policy in
    `AttunePolicy.spec.metricsSource.prometheus.address`, per namespace via
    the `AttuneNamespaceDefaults` CRD, or globally via the
    `AttuneDefaults` CRD. It is not a Helm chart value.
    If neither is set, the operator auto-discovers Prometheus by checking
    for the Prometheus Operator CRD, then well-known service names
    (`prometheus-server`, `prometheus-kube-prometheus-prometheus`) in
    common namespaces.
    A default install does not send a Prometheus token. The four auth
    cases are in
    [Choose how Attune authenticates](../guides/prometheus-setup.md#choose-how-attune-authenticates).

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

**On OpenShift 4.19+**, the operator is in the built-in OperatorHub
**Community** catalog under package name **`attune`** (display name
**Attune**; CSV names look like `attune.vX.Y.Z`). Search for "Attune"
in the web console under **Operators > OperatorHub**, or verify with:

```bash
oc get packagemanifests -n openshift-marketplace attune
```

For a public web listing of the same package, use
[OperatorHub.io/operator/attune](https://operatorhub.io/operator/attune)
(the Red Hat Ecosystem Catalog website search does not reliably list
community operators). See the [OpenShift guide](../guides/openshift.md)
for catalog details, TLS profile integration, and OpenShift-specific
configuration.

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
    A default install does not send a Prometheus token. See
    [Choose how Attune authenticates](../guides/prometheus-setup.md#choose-how-attune-authenticates).

## Secret access

The operator ServiceAccount can `get` any Secret in the cluster. It cannot
`list` or `watch` Secrets. Helm, Kustomize, and OperatorHub install that
rule on the ClusterRole. `watchNamespaces` limits which namespaces the
informer cache watches. It does not narrow Secret `get`. Limiting Secret
`get` to specific namespaces means a Role in each of those namespaces.
The chart does not ship that Role.

The operator reads a Secret only when a field names it.

| Field | Namespace |
| --- | --- |
| `metricsSource.prometheus.bearerTokenSecret` | namespace of the AttunePolicy or AttuneNamespaceDefaults |
| `metricsSource.datadog.apiKeySecretRef` | namespace of the AttunePolicy or AttuneNamespaceDefaults |
| `updateStrategy.export.pullRequest.tokenSecretRef` | namespace of the AttunePolicy or AttuneNamespaceDefaults |
| Helm `prometheusAuth.existingSecret` | operator namespace |
| Helm `datadogAuth.existingSecret` | operator namespace |

Creating or updating an AttunePolicy or AttuneNamespaceDefaults runs a
SubjectAccessReview for each Secret named by `bearerTokenSecret`,
`apiKeySecretRef`, or `tokenSecretRef`. The user who submitted the object
must be allowed to `get` that Secret in the object's namespace. When the review denies access, the API server rejects the
object with `the admission user must have get on Secret "<name>" in namespace "<namespace>"`.

An object that is already stored keeps using its Secret refs. The check
runs on the next update. A missing SubjectAccessReview still allows the
object. Cluster AttuneDefaults does not run this check. It warns, and
those Secret names are still read from each policy namespace. With
`--datadog-api-key-secret`, a cluster-chosen Datadog config reads the
operator-namespace Secret instead. The Helm operator-namespace Secrets
are read by the operator ServiceAccount. Admission does not review the
policy author for those names.

## Verify the installation

Check that the operator pod is running:

```bash
kubectl -n attune-system get pods
kubectl -n attune-system get deploy
```

Expected output (Helm release name `attune`; Deployment name is **`attune`**):

```text
NAME                              READY   STATUS    RESTARTS   AGE
attune-6f8b4c7d9f-xk2pq  1/1     Running   0          30s
```

!!! note "Deployment name by install path"
    - **Helm** (recommended): Deployment and ServiceAccount are named after
      the release (default `attune`). Logs:
      `kubectl logs -n attune-system deploy/attune`
    - **Raw manifests** (`install.yaml`): Deployment is
      `attune-controller-manager`.
    - **OLM / OperatorHub**: follows the CSV naming (often
      `attune-controller-manager`).

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

# Confirm the cluster can run in-place resize
kubectl attune doctor
```

See the [CLI Reference](../reference/cli.md) for available commands.

## Next steps

Head to the [Quick Start](quickstart.md) to create your first AttunePolicy.
