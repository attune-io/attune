## Version scheme

Attune follows [Semantic Versioning](https://semver.org/). The Helm
chart version and `appVersion` are kept in sync in `charts/attune/Chart.yaml`.

## Release process

### 1. Prepare the release

Update `CHANGELOG.md` with the new version's changes (or rely on
release-please). Ensure all tests pass:

```bash
make verify
```

If you also want to exercise the local real-cluster end-to-end paths before a
release, run:

```bash
make test-local
```

### 1b. Full E2E matrix (required before tagging a product release)

PR CI runs Chainsaw + Go E2E on **one** Kubernetes version only
(`rancher/k3s:v1.35.4-k3s1` in `ci.yaml`). Version-sensitive behavior
(in-place memory limit clamp on 1.33–1.34 vs decrease on 1.35+, k3s 1.32
feature gate) is only exercised by **E2E Nightly**.

Before merging a release PR or publishing a tag after a feature-heavy
window:

1. Confirm tip of `main` includes the commits you are releasing.
2. Dispatch the full matrix (or wait for the next scheduled run on that tip):

```bash
gh workflow run "E2E Nightly" --repo attune-io/attune
# Monitor: Actions → E2E Nightly → all E2E (K8s v1.32..v1.35) + Fuzz + Nightly Results
```

3. Do not ship until all four K8s matrix cells and Fuzz are green on that SHA.

If the release-please PR is **BEHIND** main, update/rebase it (or let
release-please refresh) so the release notes and version bump include the
latest commits.

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

- Linux binaries for amd64, arm64, arm (v7), ppc64le, and s390x
- A container image pushed to `ghcr.io/attune-io/attune`
- A GitHub release with checksums and release notes

### 4. Container image signing

All release images are signed with [cosign](https://github.com/sigstore/cosign)
using keyless signing (Fulcio + Rekor). Verify a release image:

```bash
cosign verify \
  --certificate-identity-regexp="https://github.com/attune-io/attune" \
  --certificate-oidc-issuer="https://token.actions.githubusercontent.com" \
  ghcr.io/attune-io/attune:v0.2.0
```

### 5. Docker Hub publishing

The release workflow also pushes the same multi-arch image to Docker Hub
at `docker.io/attuneio/attune`. The Docker Hub README is synced from
`docker/README.md` on each release.

Both the GHCR and Docker Hub images share the same digest and are
cosign-signed independently.

### 6. Helm chart publishing

The Helm chart is published as an OCI artifact to two registries:

- **GHCR:** `ghcr.io/attune-io/charts/attune` (primary)
- **Docker Hub:** `docker.io/attuneio/attune-chart` (separate from the container image repo)

The chart version in `charts/attune/Chart.yaml` is bumped automatically by release-please.

The CI pipeline packages, pushes to GHCR, mirrors to Docker Hub via `oras cp`,
and cosign-signs both copies:

```bash
helm package charts/attune
helm push attune-0.2.0.tgz oci://ghcr.io/attune-io/charts
oras cp ghcr.io/attune-io/charts/attune:0.2.0 registry-1.docker.io/attuneio/attune-chart:0.2.0
cosign sign --yes ghcr.io/attune-io/charts/attune:0.2.0
cosign sign --yes docker.io/attuneio/attune-chart:0.2.0
```

### 7. Static install manifest

Generate the combined install manifest for users who do not use Helm:

```bash
make build-installer IMG=ghcr.io/attune-io/attune:latest
make build-crds
```

This writes `dist/install.yaml` and `dist/crds.yaml`, uploaded as release
artifacts. Keep them committed when they change: PR CI runs
`make verify-release-artifacts` inside the CRD Freshness Check job and fails
if `dist/` lags CRDs or kustomize config. Force-add if needed
(`git add -f dist/install.yaml dist/crds.yaml`; `dist/` is gitignored for
local noise).

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
