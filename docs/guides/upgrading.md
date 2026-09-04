# Upgrading

This page covers breaking changes and notable behavior shifts between versions.
If you are upgrading from an earlier pre-release, apply every section below your
current version (newest sections first).

Maintainers: before publishing a release after multi-version product changes,
run the full E2E Nightly matrix on tip of `main` (see
[Releasing: full E2E matrix](../contributing/releasing.md#1b-full-e2e-matrix-required-before-tagging-a-product-release)).

## v0.1.25 to v0.1.26

v0.1.26 tightens stale-recommendation handling, CloudWatch collection,
ResourceQuota list fail-closed, VPA `memoryFromCpuRatio`, and GitOps
labels. Ready reason `MetricsUnavailable` replaces `PrometheusUnavailable`
for all metrics backends. Most existing policy YAML keeps working. Read
this section if you inherit `maxConcurrentResizes` from `AttuneDefaults`,
use CloudWatch, ResourceQuota, VPA with a memory ratio, GitOps labels,
Prometheus URL userinfo, or run a self-hosted Git forge.

### Ready reason MetricsUnavailable

When the metrics backend cannot be resolved or queried, Ready is now
`MetricsUnavailable` instead of `PrometheusUnavailable`. Datadog Secret
failures and CloudWatch IAM failures use the same reason. Alerts that
match `reason="PrometheusUnavailable"` should also match
`reason="MetricsUnavailable"`. The operator still treats the old reason
as bootstrap (no requeue jitter) until the next reconcile overwrites it.

### Inherit maxConcurrentResizes from AttuneDefaults

The built-in default moved from the CRD OpenAPI schema into the
controller so `AttuneDefaults` can set a cluster-wide value. Policies
created before that change already have `maxConcurrentResizes: 1` stored
in etcd. That explicit `1` wins over `AttuneDefaults`.

To inherit a cluster-wide value, remove the field from those policies:

```bash
kubectl patch attunepolicy POLICY -n NS --type=json \
  -p='[{"op":"remove","path":"/spec/updateStrategy/maxConcurrentResizes"}]'
```

Helm does not upgrade CRDs on `helm upgrade`. Apply CRDs before the
chart upgrade or new policies on a stale CRD still materialize `1`:

```bash
kubectl apply --server-side --force-conflicts -f \
  https://github.com/attune-io/attune/releases/latest/download/crds.yaml
```

### Stale recommendations expire

A recommendation is fresh only when the newest finite sample is newer
than `3 * queryStep` (default 15m). Older in-window samples are marked
`stale`. Empty Prometheus results reuse the last rec only inside that
same window. After it expires, the rec drops and Ready can become
`InsufficientData`. Stale recs no longer count toward
`workloads.withRecommendations` or savings gauges.

### One-sided Prometheus sample gap

If Prometheus has fresh samples for only one resource (CPU or memory)
and the other series is empty or all NaN, Attune no longer recommends
the pod-template request for the missing arm. After an in-place resize
the template can be stale (256Mi while the live pod is already 1Gi).
The missing arm now holds the live request or the last rec instead.

### CloudWatch pod prefix

CloudWatch `SEARCH` no longer emits `PodName="prefix*"`. Quoted CloudWatch
tokens are exact matches, so that term matched nothing. Filtering is
client-side only.

### CloudWatch series and page cap

`GetMetricData` now stops after 5000 kept series (same default as
Prometheus `maxSeries`) or 20 result pages. The operator keeps partial
data and may set Ready `PrometheusSeriesCapped`. Large Container Insights
namespaces no longer paginate without bound. This is fail-soft, not a
hard query error.

### Prometheus address must not include userinfo

`metricsSource.prometheus.address` no longer accepts
`http://user:password@host`. The webhook rejects create and update. The
reconciler also rejects the address, so existing objects fail immediately
after the operator rolls (`MetricsUnavailable`, SSRF blocked). Move
credentials to `bearerTokenSecret` or `headers`. Apply the same change
on `AttuneDefaults` and `AttuneNamespaceDefaults` before you upgrade.

### AttuneDefaults GitOps pullRequest validation

`AttuneDefaults` and `AttuneNamespaceDefaults` now use the same
`export.pullRequest` rules as `AttunePolicy`: when `enabled` is true,
`repository` and `tokenSecretRef` are required, and `apiUrl` must be
https without userinfo. Defaults CRs that used to persist an incomplete
`pullRequest` block fail on the next UPDATE. Complete the fields or set
`enabled: false` before you upgrade.

### Self-hosted GitOps forges

`export.pullRequest.apiUrl` still rejects private IP literals by default.
Set `allowPrivateEndpoints: true` for RFC1918/ULA self-hosted GitLab,
Gitea, or Bitbucket. Loopback and link-local (including IMDS) stay
blocked. Corporate `HTTPS_PROXY` on a private address is no longer
treated as the SSRF target.

### ResourceQuota List fail-closed

If listing ResourceQuotas or LimitRanges fails (missing `list`/`watch`
RBAC or an API error), Attune skips request increases. Decreases still
apply. This is the same class of fail-closed behavior as
[node-status unavailability](#request-increases-fail-closed-when-node-status-is-unavailable).
See [troubleshooting: quota list unavailable](troubleshooting.md#resize-skipped-quota-list-unavailable).

### Conflict-check policy list fail-closed

If listing `AttunePolicy` objects for conflict detection fails, that
reconcile no longer computes recommendations. Ready is
`ConflictCheckFailed` instead of looking like bootstrap
`InsufficientData`. Last-known recommendations stay on status. Check
RBAC `list`/`watch` on `attunepolicies` and API server health. See
[troubleshooting: ConflictCheckFailed](troubleshooting.md#conflictcheckfailed).

### VPA honors memoryFromCpuRatio

When `metricsSource.vpa` is set and `memory.memoryFromCpuRatio` is also
set, Attune now derives memory from the CPU recommendation instead of
using the VPA memory target. Unset `memory.memoryFromCpuRatio` to keep
VPA memory targets.

### GitOps labels fail-closed

If `export.pullRequest.labels` is set and the forge rejects the labels
API, PR create and update fail instead of succeeding unlabeled. Grant
the forge label permission or drop `labels`.

### GitLab merge request matching

The GitLab list call now filters by source branch, target branch
(`baseBranch`, default `main`), and `per_page=100`. Attune updates an
existing open MR only when that MR's target is `baseBranch`. An MR
whose target is not `baseBranch` is not kept in sync. Attune may open
a new MR against `baseBranch` and leave the old one open.

### GitLab update no longer clears labels

A GitLab MR update used to send an empty `labels` field when
`export.pullRequest.labels` was unset. GitLab treats that as "remove
every label." Updates now omit `labels` unless the policy sets them.

### GitLab re-bootstrap after merge

After the first Attune MR merges, the next drift cycle writes a new
marker commit (timestamped) so GitLab can open another MR. A reused
empty marker file no longer blocks the next cycle.

## v0.1.24 to v0.1.25

v0.1.25 is a GitOps reliability patch. Existing policy YAML keeps working.

### Apply CRDs on Helm upgrade

Helm does not update CRDs on `helm upgrade`. This release adds
`status.gitopsPR` (`driftFingerprint`, `lastAttempt`, `url`) so skip
state survives a Flux or Argo apply that replaces
`metadata.annotations`. Without the new CRD, the API server prunes
that status object and only annotations remain.

Before `helm upgrade`, apply:

```bash
kubectl apply --server-side --force-conflicts -f \
  https://github.com/attune-io/attune/releases/latest/download/crds.yaml
```

Raw `dist/install.yaml` / `dist/crds.yaml` installs already include the
field.

### Empty GitOps PRs after upgrade

0.1.22 stored last-attempt and PR URL but not a drift fingerprint.
After cooldown, 0.1.24 could open another empty PR for the same table.
0.1.25 adopts the live table when a PR URL exists and no fingerprint
is stored, and it does not write last-attempt on dry-run (so the first
live cycle can still open). See
[GitOps integration](gitops-integration.md).

## v0.1.22 to v0.1.23

v0.1.23 is a reliability patch. Existing policies keep working without YAML
edits.

### Requeue jitter is skipped during data collection

While Ready is `InsufficientData` or `PrometheusUnavailable`, the operator
no longer adds `--requeue-jitter` (default 2m) on top of the cooldown.
First recommendations and ConfigMap export can land sooner. Jitter still
applies to steady-state cooldown requeues.

### Fleet report savings stay finite

`estimatedMonthlySavingsUSD` adds only finite numbers. Unparseable values
(NaN, Inf, or garbage) increment `unparseableSavings` and count as 0 in
the USD total. Consumers of `report.json` should treat that field as
additive.

## v0.1.21 to v0.1.22

v0.1.22 ships scale defaults and capacity safety that were previously only on
`main`. Review before upgrading from v0.1.21.

### Default PromQL pod aggregation is Max

When `metricsSource.podAggregation` is unset, Attune now defaults to **Max**
(`max by (container)` over the selected series) instead of leaving series
unaggregated. Recommendations then follow the hottest pod for each container
name, and Prometheus query cost stays proportional to containers rather than
replicas.

**If you relied on multi-pod sample pools (legacy unaggregated behavior),** set
explicitly:

```yaml
spec:
  metricsSource:
    podAggregation: None   # or Avg
```

See [Scaling: PromQL aggregation](scaling.md#promql-aggregation) and
[Scaling: large fleets](scaling.md) for operator flags related to large fleets
(`maxPodsInMetricsQuery`, `maxProfileSamples`, informer field strip).

### Default max concurrent reconciles is 2

The operator default for `--max-concurrent-reconciles` is **2** (was higher in
some earlier builds). Helm `clusterSize` presets still override this when set.
Large clusters that previously relied on more concurrent reconcilers should set
the flag or a preset explicitly.

### Request increases fail closed when node status is unavailable

If the operator cannot read node status for a pod, it **skips request
increases** (decreases still allowed) and increments
`attune_capacity_skip_total{reason="unavailable"}`. This is protective if node
API access or informer lag fails.

### Batch throttle chunking (no config change)

Safety observation batches CPU throttle PromQL queries and splits large
pod/container sets into chunks of 64. No CRD field changes; Prometheus load
for high-replica policies should drop further under rate limiting.

## v0.1.20 to v0.1.21

v0.1.21 is a feature release. Existing policies keep working without YAML
edits. Most new capabilities are **opt-in**. Read this section if you run
Kubernetes 1.35+, GitOps export, multi-cluster rollups, or memory limit
control.

### Safe by default (no action required)

| Area | Behavior |
|------|----------|
| GitOps pull request automation | Off unless `export.pullRequest.enabled: true` |
| Fleet report ConfigMap export | Off unless `fleetReport` (or Helm equivalent) is enabled |
| Runtime profiles | Only apply when `runtimeProfile` is set |
| Export schema versioning | Additive fields on recommendation ConfigMaps; consumers can ignore new keys |

### Behavior that can change without new fields

**Memory limit decreases on Kubernetes 1.35+.** On 1.35+, Attune no longer
clamps memory limits the way it did on 1.33/1.34 when the platform allows
live decreases and the policy uses `controlledValues: RequestsAndLimits` with
decrease allowed. A **usage floor** still keeps the target limit above recent
usage (default `memory.decreaseUsageMarginPercent: 10`). On 1.33–1.34, limit
decreases remain clamped as before.

If you rely on “limits never go down in place,” pin an older cluster version,
set `memory.allowDecrease: false`, use a restrictive
[runtime profile](runtime-profiles.md), or keep `controlledValues: RequestsOnly`
(the default).

**Capacity and node pressure.** Resizes may be skipped more often when nodes
are under pressure; metrics and status explain the skip. This is protective,
not a CRD break.

### Opt-in features worth enabling deliberately

| Feature | Where to start |
|---------|----------------|
| GitOps PR automation | [GitOps integration](gitops-integration.md#pull-request-automation-opt-in-phase-b) |
| Multi-cluster fleet report | [Multi-cluster](multi-cluster.md) |
| Language runtime profiles | [Runtime profiles](runtime-profiles.md) |
| Deferred / Infeasible UX | Status conditions + [troubleshooting](troubleshooting.md#deferred-or-infeasible-resize-stuck-pods) |
| SLO PromQL guardrails (unchanged API; guide improved) | [SLO guardrails](slo-guardrails.md) |

### Operator / install notes

- Refresh CRDs with the release install path (`helm upgrade` or
  `dist/crds.yaml` / `dist/install.yaml` from the tag).
- Grafana dashboard and PrometheusRule assets gain panels/alerts for GitOps
  PR outcomes, memory limit decrease safety, capacity skips, and related
  signals. Re-apply chart or dashboard ConfigMaps if you manage them out of
  band.
- `kubectl attune explain` surfaces GitOps PR and runtime profile effective
  values; upgrade the plugin with the release for matching CLI help.

After upgrade, confirm policies with:

```bash
kubectl attune status -A
kubectl attune explain -n <namespace> <policy>
```

## v1alpha1 Field Renames (v0.1.0)

Five CRD fields were renamed to align with ecosystem conventions. Existing
`AttunePolicy`, `AttuneDefaults`, and `AttuneNamespaceDefaults`
resources must be updated before applying the new CRDs.

### Field mapping

| Old field | New field | Conversion |
|-----------|-----------|------------|
| `safetyMargin: "1.2"` | `overhead: "20"` | `(old - 1) * 100` |
| `updateStrategy.mode` | `updateStrategy.type` | rename only |
| `bounds.min` / `bounds.max` | `minAllowed` / `maxAllowed` | rename only |
| `InPlaceOrEvict` | `InPlaceOrRecreate` | rename only |
| `excludeContainers` | `excludedContainers` | rename only |
| `updateStrategy.maxCpuChangePercent` | `cpu.maxChangePercent` | move to cpu section |
| `updateStrategy.maxMemoryChangePercent` | `memory.maxChangePercent` | move to memory section |

### Overhead conversion examples

| Old safetyMargin | New overhead | Meaning |
|-----------------|-------------|---------|
| `"1.1"` | `"10"` | 10% headroom |
| `"1.15"` | `"15"` | 15% headroom |
| `"1.2"` | `"20"` | 20% headroom (CPU default) |
| `"1.3"` | `"30"` | 30% headroom (memory default) |
| `"1.5"` | `"50"` | 50% headroom |

### Automated migration

**Using `sed`** (covers all five renames):

```bash
# All five renames in one pass
sed -i \
  -e 's/safetyMargin:/overhead:/g' \
  -e 's/overhead: "1.1"/overhead: "10"/g' \
  -e 's/overhead: "1.15"/overhead: "15"/g' \
  -e 's/overhead: "1.2"/overhead: "20"/g' \
  -e 's/overhead: "1.25"/overhead: "25"/g' \
  -e 's/overhead: "1.3"/overhead: "30"/g' \
  -e 's/overhead: "1.5"/overhead: "50"/g' \
  -e 's/InPlaceOrEvict/InPlaceOrRecreate/g' \
  -e 's/excludeContainers:/excludedContainers:/g' \
  manifests/*.yaml

# mode -> type (only in updateStrategy context to avoid false positives)
sed -i '/updateStrategy/,/^[^ ]/{s/mode:/type:/g}' manifests/*.yaml

# bounds.min/max -> minAllowed/maxAllowed (remove nesting manually if used)
```

**Using `yq`** (handles overhead conversion and bounds restructuring):

```bash
# Export current policies
kubectl get attunepolicies -n production -o yaml > policies.yaml

# Rename safetyMargin to overhead and convert values
yq -i '
  .items[].spec.cpu |= (
    .overhead = ((.safetyMargin | tonumber - 1) * 100 | tostring) |
    del(.safetyMargin)
  ) |
  .items[].spec.memory |= (
    .overhead = ((.safetyMargin | tonumber - 1) * 100 | tostring) |
    del(.safetyMargin)
  )
' policies.yaml

# Apply the new CRDs first, then re-apply policies
kubectl apply -f config/crd/bases/
kubectl apply -f policies.yaml
```

### Helm values migration

If you use the Helm chart with custom `defaults.cpu.overhead` or
`defaults.memory.overhead` in your `values.yaml`, update the values:

```yaml
# Before
defaults:
  cpu:
    safetyMargin: "1.2"
  memory:
    safetyMargin: "1.3"

# After
defaults:
  cpu:
    overhead: "20"
  memory:
    overhead: "30"
```
