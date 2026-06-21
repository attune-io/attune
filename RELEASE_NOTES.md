## What's New in v0.1.16

### Smarter HPA target adjustment for Burstable pods

HPA target utilization is now QoS-aware. Previously, computed HPA targets were
unconditionally capped at 100%, which broke absolute CPU threshold preservation
for Burstable pods where the limit exceeds the request. Attune now caps at
`floor(limit/request * 100)` for Burstable pods, allowing targets above 100%
when the container can legitimately burst beyond its request. Guaranteed pods
retain the 100% cap. ([#335](https://github.com/attune-io/attune/pull/335))

The HPA cap also now uses the post-resize CPU limit instead of the pre-resize
value. When Attune resizes limits alongside requests, using the old limit
overestimated the cap, allowing a slightly higher HPA target than the container
could actually burst to after the resize. ([#336](https://github.com/attune-io/attune/pull/336))

### More conservative sizing for low-confidence workloads

Removed a vestigial confidence floor (0.1) from the recommendation chain. The
floor was a leftover from an older formula and capped the safety buffer at 81%
for low-confidence workloads. Without the floor, workloads with near-zero
confidence now receive up to 100% safety buffer, which is the
conservative-direction: more headroom until the operator has enough data to be
confident in its recommendations. ([#335](https://github.com/attune-io/attune/pull/335))

### Supply chain: native GitHub attestations

Replaced `slsa-framework/slsa-github-generator` reusable workflows with
`actions/attest-build-provenance` for build provenance. The native attestation
action produces the same SLSA v1.0 provenance via Sigstore but can be
SHA-pinned, improving the OpenSSF Scorecard Pinned-Dependencies score. Cosign
signatures and attestation bundles (`.intoto.jsonl`) continue to be attached to
every release. ([#340](https://github.com/attune-io/attune/pull/340))

### Curated release notes support

The release workflow now supports an optional `RELEASE_NOTES.md` file at the
repo root. When present on the tagged commit, it replaces the auto-generated
release-please body. When absent, nothing changes. A cleanup PR is automatically
created to remove the file after the release completes.
([#339](https://github.com/attune-io/attune/pull/339))

### Code quality

Deduplicated ~76 lines of repeated validation and recommendation initialization
code across the webhook validators and controller recommendation paths. Shared
helpers (`validateResourceConfigFields`, `validateDurationFloor`,
`newContainerRecommendation`) reduce maintenance surface without changing any
behavior. ([#341](https://github.com/attune-io/attune/pull/341))

### Dependencies

- Go 1.26, controller-runtime v0.24.1, K8s API v0.36.1
- Bumped `golang` base image, `k8s.io/*` modules, and GitHub Actions
