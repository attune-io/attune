## Version scheme

kube-rightsize follows [Semantic Versioning](https://semver.org/). The Helm
chart version and `appVersion` are kept in sync in `charts/kube-rightsize/Chart.yaml`.

## Release process

### 1. Prepare the release

Update `CHANGELOG.md` with the new version's changes. Ensure all tests pass:

```bash
make test-all
```

### 2. Tag the release

Create an annotated Git tag:

```bash
git tag -a v0.2.0 -m "Release v0.2.0"
git push origin v0.2.0
```

### 3. GoReleaser

The CI pipeline uses [GoReleaser](https://goreleaser.com/) to build binaries
and create the GitHub release. GoReleaser is triggered automatically when a
tag matching `v*` is pushed.

GoReleaser produces:

- Linux binaries for amd64 and arm64
- A container image pushed to `ghcr.io/sebtardiflabs/kube-rightsize`
- A GitHub release with checksums and release notes

### 4. Container image signing

All release images are signed with [cosign](https://github.com/sigstore/cosign)
using keyless signing (Fulcio + Rekor). Verify a release image:

```bash
cosign verify \
  --certificate-identity-regexp="https://github.com/SebTardifLabs/kube-rightsize" \
  --certificate-oidc-issuer="https://token.actions.githubusercontent.com" \
  ghcr.io/sebtardiflabs/kube-rightsize:v0.2.0
```

### 5. Helm chart publishing

The Helm chart is published as an OCI artifact to `ghcr.io/sebtardiflabs/charts/kube-rightsize`.

Update the chart version in `charts/kube-rightsize/Chart.yaml`:

```yaml
version: 0.2.0
appVersion: "0.2.0"
```

The CI pipeline packages and pushes the chart automatically:

```bash
helm package charts/kube-rightsize
helm push kube-rightsize-0.2.0.tgz oci://ghcr.io/sebtardiflabs/charts
```

### 6. Static install manifest

Generate the combined install manifest for users who do not use Helm:

```bash
make build-installer
```

This writes `dist/install.yaml`, which is uploaded as a release artifact.

## Pre-release checklist

- [ ] All tests pass (`make test && make test-e2e`)
- [ ] `CHANGELOG.md` updated
- [ ] `Chart.yaml` version and appVersion bumped
- [ ] No uncommitted changes
- [ ] Tag pushed to origin
- [ ] GitHub Actions billing is active (the release workflow uses
  `ubuntu-latest`, not self-hosted runners)

## Patch releases

For patch releases on an older minor version, create a release branch:

```bash
git checkout -b release-0.1 v0.1.0
# cherry-pick fixes
git tag -a v0.1.1 -m "Release v0.1.1"
git push origin v0.1.1
```
