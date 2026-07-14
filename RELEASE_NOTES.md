## What's New in v0.1.20

v0.1.20 is a product-focused release. Attune now skips well-known mesh and sidecar containers by default, can optionally write recommendations into Deployment and StatefulSet pod templates so replacement pods start correctly sized, and merges cluster and namespace defaults per field the way the docs always described. If you run service meshes, frequent deploys that recreate pods, or multi-layer defaults CRs, this release is worth taking carefully and reading the upgrade notes below.

### Known sidecars excluded by default

Attune no longer right-sizes well-known mesh and agent containers unless you opt out. When `excludeKnownSidecars` is unset (the new built-in default is **true**), these names are skipped in addition to any entries in `excludedContainers`:

`istio-proxy`, `linkerd-proxy`, `consul-dataplane`, `kuma-dp`, `vault-agent`, `cloud-sql-proxy`, `cloudsql-proxy`, `gce-proxy`

Effective exclude set is the **union** of that list and `spec.excludedContainers`. You do not need to list `istio-proxy` yourself for mesh-aware policies.

**Restore previous list-only behavior:**

```yaml
spec:
  excludeKnownSidecars: false
```

Or set the same field on `AttuneDefaults` / `AttuneNamespaceDefaults` for cluster- or namespace-wide opt-out when policies leave the field unset.

`kubectl attune explain` prints the effective excluded set so you can confirm what the operator will skip.

([#400](https://github.com/attune-io/attune/pull/400))

### Opt-in template persistence (Deployment and StatefulSet)

In-place resize still only changes **running pods**. After a normal deploy, new pods were created from the original template and stayed wrong-sized until Attune recommended and resized again.

You can now opt in so Attune also patches **workload pod templates** when recommendations are ready:

```yaml
spec:
  updateStrategy:
    type: Auto   # or Recommend with when: OnRecommendation
    templatePersistence:
      enabled: true
      # AfterSuccessfulResize (default when enabled) | OnRecommendation
      when: AfterSuccessfulResize
```

| `when` | Behavior |
|--------|----------|
| `AfterSuccessfulResize` (default when enabled) | Patch the template only after a successful in-place resize for that workload |
| `OnRecommendation` | Patch when a recommendation is accepted (works in Recommend mode without a resize) |

Supported kinds: **Deployment** and **StatefulSet**. The operator no-ops when the template already matches, skips mid-rollout, skips Observe mode, and defers template writes during Canary until full rollout so a partial canary does not roll the whole fleet via the template.

**GitOps:** do **not** enable this under unmanaged Argo CD / Flux sync if Git will thrash the live template every reconcile. Prefer `export.configMap` or `initialSizing` for pure GitOps; use template persistence when the cluster (or a controlled adopt flow) is source of truth for resources.

Metrics: `attune_template_patch_total`. Events: `TemplatePatched` / `TemplatePatchFailed`. History records `method=TemplatePersistence` and `result=TemplatePatched`.

([#403](https://github.com/attune-io/attune/pull/403), [#404](https://github.com/attune-io/attune/pull/404), [#406](https://github.com/attune-io/attune/pull/406), [#407](https://github.com/attune-io/attune/pull/407), [#408](https://github.com/attune-io/attune/pull/408))

### Three-tier defaults merge

When both cluster `AttuneDefaults` and namespace `AttuneNamespaceDefaults` exist, Attune now merges them **per field**:

```text
built-in  <  cluster AttuneDefaults  <  namespace AttuneNamespaceDefaults  <  policy
```

Previously, a namespace defaults object **replaced** the cluster layer entirely, so fields omitted on the namespace object fell through to **built-in** defaults, not to the cluster CR.

**Example:** cluster sets `percentile=95` and `cooldown=10m`; namespace sets only `percentile=90` → effective defaults use `percentile=90` and `cooldown=10m` (cluster fills the gap).

Single-layer setups (only cluster, only namespace, or neither) are unchanged. `kubectl attune explain` labels combined inheritance as `merged defaults (namespace+cluster)` when both CRs apply.

([#409](https://github.com/attune-io/attune/pull/409), [#410](https://github.com/attune-io/attune/pull/410), [#411](https://github.com/attune-io/attune/pull/411))

### Reliability and polish

- Template persistence history values (`TemplatePersistence` / `TemplatePatched` / `template`) are valid in the CRD status schema so status updates are not rejected after a successful template patch. ([#404](https://github.com/attune-io/attune/pull/404))
- Template patch events are deduplicated; requests are clamped to limits the same way as live resize; Canary and Observe guards are covered by unit and E2E tests. ([#406](https://github.com/attune-io/attune/pull/406), [#407](https://github.com/attune-io/attune/pull/407), [#408](https://github.com/attune-io/attune/pull/408))
- Changelog layout and merge-test assertions cleaned up for clearer inheritance failures. ([#402](https://github.com/attune-io/attune/pull/402))

### Upgrading

Apply the usual upgrade path, then review the two behavior-sensitive items:

1. **Sidecars** — default exclude is on. Set `excludeKnownSidecars: false` if you intentionally right-size mesh proxies with Attune.
2. **Dual defaults CRs** — if you use both cluster and namespace defaults and relied on namespace objects fully isolating a namespace from cluster settings, re-check effective values with `kubectl attune explain`.

Template persistence stays **off** until you enable it; no action required if you leave it unset.

CRDs change with this release (new fields and status history enums). Helm/OLM upgrades apply them as part of the chart/bundle install.

```bash
# Helm (recommended)
helm upgrade attune oci://ghcr.io/attune-io/charts/attune \
  --namespace attune-system

# OpenShift / OLM
# Upgrade the existing Subscription on channel "stable" (package: attune)
```

See the [installation guide](https://attune-io.github.io/attune/getting-started/installation/) and [configuration reference](https://attune-io.github.io/attune/reference/configuration/) for field details.

### Full changelog

https://github.com/attune-io/attune/compare/v0.1.19...v0.1.20
