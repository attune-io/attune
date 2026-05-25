## Prerequisites

| Tool | Version |
|------|---------|
| Go | 1.26+ |
| Docker | 24+ |
| kubectl | matching your cluster |
| k3d **or** Kind | k3d 5.8+ / Kind 0.24+ |
| Helm | 3.16+ or 4.x |
| Python 3 + pip | 3.8+ (for `yamllint` in `make verify`) |
| Make | any |

## Clone and build

```bash
git clone https://github.com/attune-io/attune.git
cd attune

# Generate CRD manifests and deepcopy methods
make manifests generate

# Build the operator binary
make build
```

The binary is written to `bin/manager`.

## Local cluster

Create a local Kubernetes cluster. Prefer k3d unless you specifically need Kind,
because CI and nightly workflows run on k3d/K3S:

```bash
# Recommended: k3d (matches CI and nightly workflows)
make k3d-create

# Alternative: Kind (supported, but not the default CI path)
make kind-create
```

Install CRDs into the cluster:

```bash
make install
```

Before running the operator, verify that your current kubeconfig context points at the cluster you just created and that the API server is reachable:

```bash
kubectl config current-context
kubectl cluster-info
```

If `kubectl cluster-info` fails or points at the wrong cluster, switch contexts before running `make run` or any E2E target.

## Running the operator locally

Run the operator against the local cluster (uses your current kubeconfig):

```bash
make run
```

This executes `go run ./cmd/manager/` and connects to the cluster configured
in your current kubeconfig context.

!!! tip
    The operator logs at `info` level by default. Set the `LOG_LEVEL`
    environment variable to `debug` for verbose output.

## Apply sample resources

```bash
kubectl apply -f config/samples/defaults.yaml
kubectl apply -f config/samples/recommend-mode.yaml
```

## Build and deploy to cluster

Build the container image, load it into the local cluster, and deploy:

```bash
# If using k3d:
make k3d-deploy IMG=attune:e2e

# If using Kind:
make kind-deploy IMG=attune:e2e
```

## Linting

```bash
make lint
```

Auto-fix lint issues:

```bash
make lint-fix
```

## Code generation

After modifying API types in `api/v1alpha1/`, regenerate manifests and
deepcopy methods:

```bash
make manifests generate
```

## Cleanup

Delete the local cluster:

```bash
# If using k3d:
make k3d-delete

# If using Kind:
make kind-delete
```

Uninstall CRDs:

```bash
make uninstall
```
