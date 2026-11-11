# Attune Helm chart

Helm chart for Attune, the Kubernetes operator that right-sizes pod resource requests and limits in place.

This repository is the chart. The operator container image is [attuneio/attune](https://hub.docker.com/r/attuneio/attune).

## Quick reference

- **Source**: [github.com/attune-io/attune](https://github.com/attune-io/attune) (`charts/attune`)
- **Documentation**: [attune-io.github.io/attune](https://attune-io.github.io/attune/)
- **Issues**: [github.com/attune-io/attune/issues](https://github.com/attune-io/attune/issues)
- **License**: Apache 2.0

## Install from Docker Hub

```bash
helm install attune oci://registry-1.docker.io/attuneio/attune-chart \
  --version __CHART_VERSION__ \
  --namespace attune-system --create-namespace
```

Tags are chart versions without a leading `v`, such as `__CHART_VERSION__`. The same chart is also published at `oci://ghcr.io/attune-io/charts/attune`, which is the path in the project install guide.

The chart installs `ghcr.io/attune-io/attune` with the chart appVersion as the tag. Chart __CHART_VERSION__ uses `ghcr.io/attune-io/attune:__CHART_VERSION__`. It does not pull `attuneio/attune` unless you set `image.repository`.

## Requirements

- Kubernetes 1.32 or newer. On 1.32, enable the `InPlacePodVerticalScaling` feature gate. On 1.33–1.34 it is beta and on by default. On 1.35 and newer it is GA.
- Helm 3.16 or newer, or Helm 4.x
- Prometheus, for usage metrics
- cert-manager 1.12 or newer for webhook TLS. To skip it: `--set webhooks.enabled=false`

Chart 0.1.23 defaults to image tag `0.1.23`, which was never published. If you install that chart, add `--set image.tag=v0.1.23`. Chart 0.1.24 and newer can omit that pin.

## Verify the signature

```bash
cosign verify \
  --certificate-identity-regexp="https://github.com/attune-io/attune" \
  --certificate-oidc-issuer="https://token.actions.githubusercontent.com" \
  docker.io/attuneio/attune-chart:__CHART_VERSION__
```
