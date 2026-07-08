## What's New in v0.1.17

v0.1.17 tightens the path from "I found Attune" to "I can install it with confidence," refreshes the supply chain under the operator, and hardens the automation that keeps releases and dependency updates moving.

### Easier to find and install on OpenShift

Attune continues to ship to both [OperatorHub.io](https://operatorhub.io/operator/attune) and the OpenShift **Community Operators** catalog on every release.

This release makes that path clearer in the docs:

- **Package name:** `attune` (display name **Attune**; CSV names look like `attune.v0.1.17`)
- **OpenShift versions:** Community catalog entries target **OpenShift 4.19+** (aligned with Kubernetes 1.32+)
- **How to verify on a cluster:** `oc get packagemanifests -n openshift-marketplace attune`
- **Where to browse in a browser:** [OperatorHub.io/operator/attune](https://operatorhub.io/operator/attune)

We also removed a Red Hat Ecosystem Catalog search URL that redirected away from the query and never showed Attune. Community operators are meant to be discovered in the **in-cluster** OperatorHub Community source (and on OperatorHub.io), not via that website search.

([#366](https://github.com/attune-io/attune/pull/366), [#367](https://github.com/attune-io/attune/pull/367))

### Fresher dependencies under the hood

Runtime and build dependencies were brought up to current patch and minor releases, including:

- AWS SDK for Go v2 modules used by CloudWatch metrics integration
- `prometheus/common`
- Go base images used to build the operator
- GitHub Actions used in CI and release

You get the same operator features with a cleaner, more current dependency tree for security scanning and long-term maintenance.

([#348](https://github.com/attune-io/attune/pull/348), [#353](https://github.com/attune-io/attune/pull/353), [#354](https://github.com/attune-io/attune/pull/354), [#361](https://github.com/attune-io/attune/pull/361), [#362](https://github.com/attune-io/attune/pull/362), [#363](https://github.com/attune-io/attune/pull/363))

### More resilient continuous delivery

Several release and dependency-automation improvements landed so the project stays easy to keep current:

- Dependabot updates are approved and auto-merged more reliably (including grouped updates), without blocking on secrets that Dependabot cannot see
- DCO checks skip merge commits that "Update branch" can introduce on bot PRs
- Release-please PRs are auto-approved so the merge queue is not stuck waiting on a human rubber stamp
- Nightly E2E on Kubernetes 1.32 recreates the cluster once if the `pods/resize` subresource is missing after startup (mitigates an intermittent k3s feature-gate race while upstream image tags catch up)

([#345](https://github.com/attune-io/attune/pull/345), [#350](https://github.com/attune-io/attune/pull/350), [#355](https://github.com/attune-io/attune/pull/355), [#356](https://github.com/attune-io/attune/pull/356), [#365](https://github.com/attune-io/attune/pull/365))

### Project quality signals

- README now shows a **90%+** unit coverage badge for `./internal/...` (CI still enforces an 80% floor; measured coverage remains around 92%)
- Retired Go Report Card badge removed after the public service was sunset
- Docs and CONTRIBUTING wording updated to match current coverage and testing expectations

([#364](https://github.com/attune-io/attune/pull/364), [#365](https://github.com/attune-io/attune/pull/365))

### Upgrading

Standard upgrade paths apply. No CRD or API breaking changes in this release.

```bash
# Helm (recommended)
helm upgrade attune oci://ghcr.io/attune-io/charts/attune \
  --namespace attune-system

# OpenShift / OLM
# Upgrade the existing Subscription on channel "stable" (package: attune)
```

See the [installation guide](https://attune-io.github.io/attune/getting-started/installation/) and the [OpenShift guide](https://attune-io.github.io/attune/guides/openshift/) for package name and catalog details.

### Full changelog

https://github.com/attune-io/attune/compare/v0.1.16...v0.1.17
