# Attune

Safe, in-place Kubernetes pod resource right-sizing. VPA done right.

## Quick reference

- **Source**: [github.com/attune-io/attune](https://github.com/attune-io/attune)
- **Documentation**: [github.com/attune-io/attune#documentation](https://github.com/attune-io/attune#documentation)
- **Issues**: [github.com/attune-io/attune/issues](https://github.com/attune-io/attune/issues)
- **License**: Apache 2.0

## What is Attune?

Attune is a Kubernetes operator that automatically right-sizes pod resource
requests and limits using In-Place Pod Resize (beta in Kubernetes 1.33+, alpha
with feature gate in 1.32). No pod restarts, no HPA conflicts, no outages.

## Supported tags

- `latest` - latest stable release
- `vX.Y.Z` - specific version (e.g., `v0.1.1`)

## Supported architectures

- `linux/amd64`
- `linux/arm64`
- `linux/arm/v7`
- `linux/ppc64le`
- `linux/s390x`

## How to use this image

> **Recommended registry**: For production use, pull from GHCR to avoid
> Docker Hub rate limits:
> ```bash
> ghcr.io/attune-io/attune:latest
> ```

### Install with Helm (recommended)

```bash
helm install attune oci://ghcr.io/attune-io/charts/attune \
  --namespace attune-system --create-namespace
```

### Pull from Docker Hub

```bash
docker pull attuneio/attune:latest
```

### Verify image signature

All images are signed with cosign (keyless, Sigstore):

```bash
cosign verify \
  --certificate-identity-regexp="https://github.com/attune-io/attune" \
  --certificate-oidc-issuer="https://token.actions.githubusercontent.com" \
  attuneio/attune:latest
```

## Security

- Runs as non-root (UID 65532)
- Distroless base image (no shell, no package manager)
- Signed with cosign + SLSA Level 3 provenance
- Trivy-scanned on every release

## Source

[https://github.com/attune-io/attune](https://github.com/attune-io/attune)