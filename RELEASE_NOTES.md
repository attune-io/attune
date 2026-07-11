## What's New in v0.1.18

v0.1.18 is a security-focused patch release. The operator container image is rebuilt on Go 1.26.5, which fixes two HIGH-severity vulnerabilities in the Go standard library. If you are running v0.1.17, upgrading ensures your deployed image no longer ships the affected `os` and `crypto/tls` packages.

### Security: Go 1.26.5 fixes two stdlib vulnerabilities

The Go toolchain used to build the operator has been bumped from 1.26.4 to 1.26.5, addressing:

| CVE | Severity | Component | Impact |
|-----|----------|-----------|--------|
| [CVE-2026-39822](https://nvd.nist.gov/vuln/detail/CVE-2026-39822) | HIGH | `os.Root` | Symlink following vulnerability |
| [GO-2026-5856](https://pkg.go.dev/vuln/GO-2026-5856) | HIGH | `crypto/tls` | Encrypted Client Hello (ECH) privacy leak |

Both the `go.mod` directive and the Dockerfile base image have been updated so the published multi-arch container image is built entirely with Go 1.26.5.

([#381](https://github.com/attune-io/attune/pull/381))

### Nightly E2E: more reliable image loading

`k3d image import` can silently fail while reporting success, causing nightly E2E tests to wait several minutes on a Helm install timeout before failing with `ErrImageNeverPull`. The nightly workflow now verifies the operator image is available inside the k3d node immediately after import and retries up to three times if it is missing. This turns a slow, confusing timeout into an immediate, actionable diagnostic.

([#379](https://github.com/attune-io/attune/pull/379), closes [#377](https://github.com/attune-io/attune/issues/377))

### Issue triage automation

New issues now receive automatic triage labels (`needs-triage`, `ready`, `needs-info`) based on the author's association with the project. This helps contributors and maintainers see at a glance which issues have been accepted into the backlog and which still need review.

([#378](https://github.com/attune-io/attune/pull/378))

### Upgrading

No CRD or API breaking changes. Standard upgrade paths apply.

```bash
# Helm (recommended)
helm upgrade attune oci://ghcr.io/attune-io/charts/attune \
  --namespace attune-system

# OpenShift / OLM
# Upgrade the existing Subscription on channel "stable" (package: attune)
```

See the [installation guide](https://attune-io.github.io/attune/getting-started/installation/) for details.

### Full changelog

https://github.com/attune-io/attune/compare/v0.1.17...v0.1.18
