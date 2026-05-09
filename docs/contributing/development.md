## Prerequisites

| Tool | Version |
|------|---------|
| Go | 1.26+ |
| Docker | 24+ |
| kubectl | matching your cluster |
| Kind | 0.24+ |
| Helm | 3.16+ |
| Make | any |

## Clone and build

```bash
git clone https://github.com/SebTardif/kube-rightsize.git
cd kube-rightsize

# Generate CRD manifests and deepcopy methods
make manifests generate

# Build the operator binary
make build
```

The binary is written to `bin/manager`.

## Local Kind cluster

Create a Kind cluster running Kubernetes 1.33+:

```bash
make kind-create
```

This runs `kind create cluster --name kube-rightsize --image kindest/node:v1.33.7`.

Install CRDs into the cluster:

```bash
make install
```

## Running the operator locally

Run the operator against the Kind cluster (uses your local kubeconfig):

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

## Build and deploy to Kind

Build the container image, load it into Kind, and deploy:

```bash
make kind-deploy
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

Delete the Kind cluster:

```bash
make kind-delete
```

Uninstall CRDs:

```bash
make uninstall
```
