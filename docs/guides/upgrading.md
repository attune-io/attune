# Upgrading

This page covers breaking changes and notable behavior shifts between versions.
If you are upgrading from an earlier pre-release, apply every section below your
current version (newest sections first).

Maintainers: before publishing a release after multi-version product changes,
run the full E2E Nightly matrix on tip of `main` (see
[Releasing: full E2E matrix](../contributing/releasing.md#1b-full-e2e-matrix-required-before-tagging-a-product-release)).

## Unreleased

### Policy limit multiplier requires RequestsAndLimits

A policy that sets `cpu.limitMultiplier` or `memory.limitMultiplier`
must set `controlledValues: RequestsAndLimits` on that same block.
Admission rejects an omitted or empty mode. `RequestsOnly` with a
multiplier was already rejected.

An object already stored without that mode is not rewritten. Reconcile
still reports `InvalidConfig` until an update sets `RequestsAndLimits`
or removes the multiplier. `AttuneDefaults` and
`AttuneNamespaceDefaults` may still store a multiplier without the
mode. A policy that omits the multiplier still inherits it.

### RequestsOnly does not inherit a limit multiplier

A policy that sets `cpu.controlledValues` or `memory.controlledValues`
to `RequestsOnly` no longer copies `limitMultiplier` from
`AttuneDefaults` or `AttuneNamespaceDefaults`. The live limit stays,
and that policy is no longer `InvalidConfig` because of a defaults
multiplier.

A policy that omits `controlledValues` still copies the multiplier.
Built-in defaults then fill `RequestsOnly`, and reconcile still reports
`InvalidConfig`. Set `RequestsAndLimits` on that policy, or remove the
multiplier from defaults. A defaults object may still store a
multiplier without `RequestsAndLimits`.

### Rollout workloadRef read failures stay visible

Template persistence still does not patch a Rollout whose
`spec.workloadRef` is set. When that object cannot be read, the
condition reason stays `WorkloadRefUnread` for the rest of the
reconcile. It is not replaced by `TemplateWorkloadRef`, and it is not
removed while the read error is still present.

`TemplateWorkloadRef` means the reference was read and the template
was left alone. A later reconcile with no workloadRef read error
removes a stale `WorkloadRefUnread`. Resize behavior does not change.
Observe mode already kept the unread reason. Recommend mode now does
too.

### Amazon Managed Prometheus signing

Policies that omit `metricsSource.prometheus.sigv4` stay unsigned. There
is no Helm default and no environment variable that turns signing on.
Set `sigv4.region` on a policy, or on `AttuneDefaults`, when that
Prometheus address is an Amazon Managed Prometheus workspace.

### Argo Rollout workloads

`targetRef.kind` accepts `Rollout` (`argoproj.io/v1alpha1`). Helm upgrades
add a ClusterRole rule for `argoproj.io` `rollouts`: get, list, watch,
patch, and update. Raw manifests need that rule on the operator
ClusterRole. Policies that do not set `kind: Rollout` do not change.
Attune does not install the Rollout CRD. A Rollout policy stays Ready
False with reason `WorkloadCRDMissing` until Argo Rollouts is installed.

### Resize during a rollout

Recommendations are still computed while pods are being replaced. Which
pods are resized depends on the workload kind and its strategy.

A RollingUpdate Deployment, including the default strategy, skips resize
only while its generation is ahead of the observed generation, or while
`status.replicas` is still above `status.updatedReplicas`. It does not
freeze because `availableReplicas` is behind `spec.replicas`, or because a
scale-out has not finished. Pods still on the old template are skipped. A
paused Deployment skips pods that are not on the current pod-template-hash
when that hash is known.

A Deployment whose strategy is `Recreate` is not a resize skip. Pods are
resized while `updatedReplicas` is still behind `spec.replicas`. Release
v0.1.32 skipped the whole Deployment in that case.

A RollingUpdate StatefulSet resizes pods whose revision matches
`status.updateRevision` and skips the others, including while a partition
has not reached every replica. A stale generation skips every pod. An
OnDelete StatefulSet is not a resize skip. Pods still on the previous
template are resized. Release v0.1.32 skipped the whole StatefulSet while
`updatedReplicas` was behind `spec.replicas`.

A RollingUpdate DaemonSet resizes pods on the current ControllerRevision
and skips older pods. An OnDelete DaemonSet is not a resize skip. Its pods
are resized.

A standalone ReplicaSet is skipped only while its generation is ahead of
the observed generation. `readyReplicas` behind `spec.replicas` does not
freeze it, so ready and unready pods can be resized. Release v0.1.32
skipped the ReplicaSet while `readyReplicas` was behind.

### DaemonSet controllerrevisions

A RollingUpdate DaemonSet needs `apps/controllerrevisions` get, list, and
watch. A Helm upgrade adds that ClusterRole rule. Raw manifests need the
same rule on the operator ClusterRole.

If that list fails, Attune skips every pod of that DaemonSet and emits
`DaemonSetRevisionUnavailable`. It does not resize them. OnDelete does not
read ControllerRevisions.

The list uses the DaemonSet pod selector, then the owner UID. Resize
decisions stay the same when that selector matches the current revision.
The call returns fewer objects. No new RBAC rule and no user action. An
owned revision the selector does not match still skips the DaemonSet.

Attune reads only the pod label `controller-revision-hash`. It does not
read `pod-template-generation`. Pods that have the hash label resize the
same way. A pod with no hash label is still skipped when the current hash
is known.

### Copy into the next release notes

These behaviors change when the operator is upgraded, with no YAML edit.
The generated notes for the next release must list them. Do not edit
release pull request 889 to add the list.

- Omitted `maxAllowed` is not capped
- CloudWatch `cpuUnit` empty means Millicores
- A stored `0s` cooldown, safety period, or SLO window on a policy or on defaults can be updated and deleted. A new `0s` is rejected. A stored `0s` cooldown still waits 1h
- A policy that sets `limitMultiplier` must set `controlledValues: RequestsAndLimits` on that same object. Admission rejects the apply. A stored policy stays `InvalidConfig` until edited
- A policy that sets `controlledValues: RequestsOnly` does not inherit `limitMultiplier`. Omitting `controlledValues` still inherits it and can be `InvalidConfig`
- RollingUpdate DaemonSets resize the current revision. The pod label is the hash, not the revision name
- A QoS check with `spec.resources` uses one rule on both sides
- A stored HPA CPU base that is missing other containers is repaired. Growth of the same containers keeps the stored original. An annotation an older operator already rewrote is left in place
- A failed Rollout `workloadRef` read keeps reason `WorkloadRefUnread`. A readable ref still reports `TemplateWorkloadRef`
- Datadog null points are dropped

### Stored cooldown of 0s

A stored `cooldown: 0s` is not a wait. The controller treats it as the 1h
default, including when a policy omits cooldown and inherits `0s` from
`AttuneDefaults` or `AttuneNamespaceDefaults`. An update that leaves that
stored `0s` unchanged is accepted on `AttunePolicy`, `AttuneDefaults`, and
`AttuneNamespaceDefaults`, including the update that removes the finalizer,
so the object can be deleted. A create that sets `0s`, or an update that
changes a positive duration to `0s`, is still rejected. Omit the field for
the 1h default, or set at least `1m`. The same admission rule applies to
`safetyObservationPeriod` (a stored `0s` is unset, so the controller uses
5m or a positive canary period) and an SLO `evaluationWindow` (a stored
`0s` uses 5m). Canary `observationPeriod: 0s` still means the built-in
observation period. `historyWindow` below 1h stays rejected on every write,
including an unchanged `0s`.

### Datadog null points

A Datadog point with a JSON `null`, or no value, is dropped. A numeric
zero is kept. Dropping a gap can raise the CPU percentile. CPU
`allowDecrease` still defaults to true, so the next reconcile can raise
the CPU request. Memory `allowDecrease` still defaults to false.

### QoS class changes are not evicted

An in-place resize that would change the pod QoS class is skipped,
including when `resizeMethod` is `InPlaceOrRecreate`. The pod is not
evicted. `InPlaceOnly`, the default, already could not apply that resize,
because the API rejects a QoS class change.

### Standalone ReplicaSet initial sizing

`initialSizing` still defaults to false. A policy that already sets
`initialSizing: true` now sizes new pods of a standalone ReplicaSet whose
selector matches. A ReplicaSet owned by a Deployment still follows the
Deployment.

### Memory HPA targets move when auto-tune is on

A memory utilization HPA used to keep its percent when Attune changed the
memory request, so a smaller request could scale the workload out. An HPA
annotated `attune.io/auto-tune: "true"` now gets the same retune for memory
as for CPU. The original target and the original request are stored on the
HPA. A later resize multiplies that stored target by the stored request
divided by the new request.

`updateStrategy.hpaTargetBounds` is optional. Leave it unset to keep today's
limit cap, including CPU targets above 90. Set `cpu` or `memory` `min` and
`max` only when you want a tighter band. 50 and 90 are a common choice, not
a default. The Helm chart does not turn auto-tune on.

### Usage surge is off until cpu.surge or memory.surge is set

CPU and memory stay on the long history window unless that resource's
`surge` block is set. An empty `surge: {}` turns the feature on for that
resource and fills trigger ratio `1.5`, percentile `99`, and window `30m`.
There is no cluster default unless an `AttuneDefaults` object sets `surge`.
A policy that omits `surge` inherits a defaults surge. To keep a workload
off, omit `surge` on both the policy and `AttuneDefaults`.

When recent usage is hot, Attune shortens the history window for that
resource. The parent percentile still describes the long window. The short
window uses its own percentile and only the samples inside `surge.window`.
One spike does not switch the window. `minimumDataPoints` still gates the
long window.

### OOM bump is off until memory.oomBump is set

Memory requests stay on the percentile path unless `memory.oomBump` is set.
An empty `oomBump: {}` turns the feature on and fills ratio `1.2`,
minBump `100Mi`, maxBumps `3`, and hold `24h`. The step is
`max(ceil(origin * ratio^count), origin + minBump * count)`, then
`maxAllowed`. Origin is the live memory request before the first bump of
the streak, not the latest live request and not the pod template. When
`maxAllowed` is omitted, `maxBumps` is the only cap. After `hold` expires,
recommendations follow the normal percentile, allowDecrease, and template
rules. Hold expiry does not clear the original request stored on the pod.
A later OOM can step again from that same origin until `maxBumps`.

### Startup history exclusion is opt-in

`cpu.startupBoost.excludeFromHistory` drops CPU samples from the percentile
while a pod is inside `startupBoost.duration` plus the rate window. The
cutoff is the pod `CreationTimestamp` plus duration plus `rateWindow`.
Sample timestamps are the end of `rate()`.

Existing startup boost policies keep today's percentile until you set
`excludeFromHistory: true`. Nil and false do not change the percentile.
Memory samples are unchanged. Deleted pods stay in the history window,
because there is no `CreationTimestamp` to cut on. A recreated pod keeps
samples older than its new `CreationTimestamp`. A series with no pod
label is left unfiltered.

### RequestsAndLimits limit multiplier

`cpu.limitMultiplier` and `memory.limitMultiplier` stay off until you set
them. Omitted keeps the live request-to-limit ratio. Explicit `"1"` forces
the limit equal to the new request. Values below 1 are rejected, because a
limit cannot be smaller than its request. `maxAllowed` caps the request,
not the limit. A limit above that cap is the HPA CPU ceiling when
`controlledValues` is `RequestsAndLimits`.

During a startup-boost window the CPU limit is the greater of the boosted
request and the steady multiplied limit. The startup-boost multiplier does
not multiply that limit again. Expiry restores the steady multiplied limit.
A multiplied memory limit stays in place through that window and through
expiry. The boost does not put the pre-resize memory limit back.

A Guaranteed pod skips an in-place CPU multiplier above 1 because the
request and limit would differ. A memory multiplier on an already
Guaranteed pod raises the memory request to the new limit and the resize
proceeds. That applied request can exceed `maxAllowed`.

`RequestsOnly` plus a multiplier on the same object is rejected.
`AttuneDefaults` may carry a multiplier without `RequestsAndLimits`. A
policy that sets `RequestsOnly` does not inherit that multiplier. A
policy that omits `controlledValues` still inherits it, then resolves
to `RequestsOnly`, so that pair is rejected at reconcile.

### HPA auto-tune keeps the stored CPU base

An HPA annotated `attune.io/auto-tune: "true"` stores the pod CPU total
on the first Resource retune. Later resizes reuse that stored request.

A stored base is replaced only when it equals the history old sum of
`spec.containers` and the live pod total is larger. That gap is
containers with no history row. The update writes the pre-resize pod
sum to `attune.io/original-cpu-request` and emits `HPABaseRepaired` only
after that HPA update succeeds. A failed write does not emit the
Warning.

A stored base above that history sum stays, including after the same
containers grow. A stored base below that history sum also stays.
Attune does not add the no-history containers on top of it, because
that base may already include them. Init containers and native
sidecars stay out of the Resource sum. ContainerResource bases are not
repaired this way.

An annotation that an older operator already replaced with a grown sum
is left as stored. This release does not write the earlier number back.

Deleting the keys still stores a fresh base on the next resize. Leave
`attune.io/auto-tune` in place:

- `attune.io/original-target-cpu`
- `attune.io/original-cpu-request`
- any `attune.io/hpa-cpu-target.*` key
- any `attune.io/hpa-cpu-base.*` key

The next successful CPU resize stores the full base when the keys are gone.

### Omitted maxAllowed is not capped

Policies and AttuneDefaults objects that omitted `maxAllowed` were held
at 4000m CPU and 8Gi memory. CPU `allowDecrease` defaults to true, so a
CPU request already above 4000m could drop to that ceiling in one cycle.
Memory `allowDecrease` defaults to false, so the 8Gi ceiling blocked
further growth. A memory request already above 8Gi stayed at the current
request until decrease was enabled.

To restore the old ceiling, set it on the policy or on AttuneDefaults:

```yaml
cpu:
  maxAllowed: "4000m"
memory:
  maxAllowed: "8Gi"
```

An omitted minimum still floors at 1m CPU and 4Mi memory. An explicit
`maxAllowed` of `"0"` remains a cap. The engine applies an explicit max
again after the percent cap. A memory request above that max is published
downward only when `allowDecrease` is true. Explanation status omits
`bounds.max` when the policy has no maximum, so it is not reported as 0.

Apply CRDs before the controller upgrade. Helm does not update CRDs on
`helm upgrade`. The previous CRD requires
`explanation.cpu.bounds.max` and `explanation.memory.bounds.max`. The
next status write for a policy that omits `maxAllowed` fails that
required field until the new CRDs are applied:

```bash
kubectl apply --server-side --force-conflicts -f \
  https://github.com/attune-io/attune/releases/latest/download/crds.yaml
```

### CloudWatch CPU unit

CloudWatch policies that omit `cpuUnit` now treat `container_cpu_usage_total` as millicores (divide by 1000). Older releases divided that metric by 1e9. The next reconcile changes CPU recommendations.

To keep the old scale, set `cpuUnit: Nanocores` on each policy that has its own `cloudwatch` block. Setting it only on AttuneDefaults does not reach those policies. Defaults apply when the policy omits the whole metrics provider.

If the new scale is too small, CPU requests sit at 1m or minAllowed. If it is too large, they move toward maxAllowed. CPU `allowDecrease` still defaults to true, and CloudWatch does not run throttle revert, so a bad step is not undone by the throttle check. Confirm one raw GetMetricData sample against the container limit before rollout.

The OpenTelemetry `awscontainerinsightreceiver` multiplies the core rate by 1000, and its README lists `container_cpu_usage_total` as Millicore ([cpu extractor](https://github.com/open-telemetry/opentelemetry-collector-contrib/blob/main/receiver/awscontainerinsightreceiver/internal/cadvisor/extractors/cpu_extractor.go)).

## v0.1.31 to v0.1.32

v0.1.32 restores CloudWatch samples for Deployments and CronJobs, and
it stops a long history from pushing a configured percentile upward.
Existing policy YAML keeps working. No CRD change is required.

### CloudWatch matches the owner name

Container Insights stores the owner in `PodName`, not the full pod
name. v0.1.31 matched only the full pod name, so Deployments and
CronJobs got no samples.

v0.1.32 accepts that owner name:

- A Deployment matches the ReplicaSet hash. The hash uses Kubernetes
  `SafeEncodeString` (consonants plus the digits 2, 4, 5, 6, 7, 8,
  and 9, length 5 to 10). A sibling such as `api-v2` or `api-worker`
  is not included.
- A CronJob matches the Job name, including the timestamp.
- A DaemonSet, StatefulSet, or Job matches the workload name.
- Clusters that set `prefer_full_pod_name` still match the full pod
  name.

A DaemonSet, StatefulSet, and Job that share one name still share one
CloudWatch series. That metric has no kind.

See [CloudWatch Container Insights](../reference/configuration.md#cloudwatch-container-insights).

### Long histories keep the configured percentile

After 10,000 samples, v0.1.31 kept the highest value in each time
window and computed the percentile from those highs. A configured P95
could land near the real P98. v0.1.32 keeps the midpoint of each
window. The highest sample in the series is still kept, so burst
detection still sees a short spike.

A recommendation can be lower than the one v0.1.31 published for the
same history.

### An empty usage percentile no longer grows the request

When the usage percentile was missing, v0.1.31 still applied the
confidence buffer and could raise the request. v0.1.32 leaves that
request unchanged until a real percentile exists.

### Other corrections

- GitOps drift after a merged branch stays on the workload that owns
  the branch.
- A clock step backward, or a revert that fails, no longer drops the
  increase budget. A failed revert stays recorded as in-place.
- Configured bounds and the increase budget still apply after the
  change filter. Label selectors and metric pod filters keep matching
  the intended pods.

### Upgrade the chart and image

1. Upgrade the chart to 0.1.32, or set `image.tag` to `0.1.32` or
   `v0.1.32`.
2. Pull `ghcr.io/attune-io/attune:v0.1.32` or
   `ghcr.io/attune-io/attune:0.1.32`. Both tags point at the same
   digest.
3. CRDs are unchanged for this release. Helm still does not upgrade
   CRDs on `helm upgrade`.

## v0.1.30 to v0.1.31

v0.1.31 adds operator-level Prometheus authentication and deprecates
`bearerTokenSecret` on cluster `AttuneDefaults` only. Policy YAML that
already copies a token Secret into each namespace keeps working.

### Operator Prometheus identity

The manager can authenticate to Prometheus without a Secret in every
policy namespace:

- Helm `openshift.bindClusterMonitoringView: true` binds OpenShift
  `cluster-monitoring-view` (to the query ServiceAccount when
  `prometheusAuth.queryServiceAccount.create` is true, otherwise the
  manager SA) and sends that token (`--prometheus-use-service-account-token`).
- Helm `prometheusAuth.queryServiceAccount.create: true` TokenRequests a
  dedicated query SA instead of the manager token. The OLM bundle and
  `config/rbac` also create `attune-prometheus-query`, a Role for
  `serviceaccounts/token` on that account, and pass
  `--prometheus-query-service-account=attune-prometheus-query` plus
  `--prometheus-use-service-account-token`. Bind OpenShift
  `cluster-monitoring-view` to `attune-prometheus-query`.
- Helm `prometheusAuth.useServiceAccountToken: true` sends the SA token
  without that binding (vanilla clusters or a binding you created).
- Helm `prometheusAuth.existingSecret` reads one Secret in the operator
  namespace (`--prometheus-bearer-token-secret`).
- If cluster `AttuneDefaults` still names `bearerTokenSecret` and that
  Secret is missing in the policy namespace, the operator falls back to
  the operator identity and logs it.

Operator auth applies only to addresses from cluster `AttuneDefaults`.
It is not sent to a policy address, a namespace-defaults address, or an
auto-discovered Prometheus.

A policy (or `AttuneNamespaceDefaults`) `bearerTokenSecret` still wins
and is still read in that namespace.

See [OpenShift: Thanos Querier](openshift.md#thanos-querier).

### Operator Datadog API key

Helm `datadogAuth.existingSecret` (`--datadog-api-key-secret`) reads one
Secret in the operator namespace when cluster `AttuneDefaults` chose
Datadog. A policy or `AttuneNamespaceDefaults` `apiKeySecretRef` stays
in that namespace. Without the flag, an `apiKeySecretRef` name on
cluster `AttuneDefaults` is still read in each policy namespace.
Admission warns on that field. See [Datadog setup](datadog-setup.md).

### AttuneDefaults bearerTokenSecret is deprecated

Setting `metricsSource.prometheus.bearerTokenSecret` on cluster
`AttuneDefaults` emits an admission warning. The Secret **name** is still
copied onto each policy and looked up in the **policy** namespace. A later
0.1.x will reject that field on `AttuneDefaults` after two tagged minors
have carried the warning. Move cluster auth to the operator identity
above. `AttunePolicy` and `AttuneNamespaceDefaults` are unchanged.

## v0.1.29 to v0.1.30

v0.1.30 changes how `memory.memoryFromCpuRatio` waits for CPU and how
bootstrap requeues work. Existing policy YAML keeps working. Read this
section if you set `memoryFromCpuRatio`, use a cooldown longer than
`queryStep`, or watch Ready `MetricsUnavailable`.

### memoryFromCpuRatio waits for CPU

A valid ratio no longer falls back to Prometheus memory gauges or the
VPA memory target while CPU samples (or the VPA CPU target) are missing.
Ready stays `InsufficientData` and retries at `min(cooldown, queryStep)`
with no `requeueJitter`. Progress (`Collecting data: X/Y`) counts CPU
samples only. A prior rec whose memory explanation contains
`memoryFromCpuRatio` is kept as Stale across a CPU-only gap so
`kubectl attune` and the hold baseline stay. A leftover usage rec from
v0.1.29 is dropped. An invalid ratio (`abc`, `0`, `NaN`, or above 1000)
is ignored and Attune uses the memory signal; the webhook still rejects
those when admission is enabled.

See [memory.memoryFromCpuRatio](../reference/configuration.md#memory-from-cpu-derivation)
and [Troubleshooting: InsufficientData](troubleshooting.md#insufficientdata).

### MetricsUnavailable uses the bootstrap requeue

Ready `MetricsUnavailable` (query error or timeout, including a CPU-only
blip under `memoryFromCpuRatio`) now requeues at `min(cooldown, queryStep)`
without jitter. A policy with `cooldown: 2h` and the default 5m step
retries every 5 minutes instead of waiting the full cooldown. Resize
spacing is unchanged once Ready is `Monitoring`.

See [Troubleshooting: MetricsUnavailable](troubleshooting.md#metricsunavailable).

### Upgrade the chart and image

1. Upgrade the chart to 0.1.30, or set `image.tag` to `0.1.30` or
   `v0.1.30`.
2. Pull `ghcr.io/attune-io/attune:v0.1.30` or
   `ghcr.io/attune-io/attune:0.1.30`. Both tags point at the same
   digest.
3. CRDs are unchanged for this release. Helm still does not upgrade
   CRDs on `helm upgrade`; apply them only if you skipped a previous
   release that added CEL rules.

## v0.1.28 to v0.1.29

v0.1.29 keeps GPU, hugepages, and other extended resources on live
`/resize`, and it fails closed when HPA cannot be listed or when
CREATE would re-admit a just-reverted size. OneShot now records
envelope skips the same way Auto already did. Existing policy YAML
keeps working. Bare integer `minAllowed` / `maxAllowed` values that
v0.1.26 accepted are accepted again. Read this section if you use
GPU or hugepages, OneShot, startup boost, CREATE initial sizing,
HPA auto-tune, or scripts that watch `kubectl attune doctor`.

### Live /resize keeps GPU, hugepages, and ephemeral-storage

A successful in-place resize or safety revert used to drop extended
resources from the live request map. Apply now overlays CPU and
memory only. Template persist still writes only CPU and memory, so
those extra keys stay on the pod.

### AfterSuccessfulResize restore-retry needs a Reverted row

A Safe pod could have its workload template rolled back to the
pre-resize snapshot when live requests happened to match the
clamped revert target. Restore-retry now requires a real `Reverted`
history row.

See [Troubleshooting: Template restore after safety
revert](troubleshooting.md#template-restore-after-safety-revert).

### Persist skips only the failed container

One Failed history row on a single replica used to block template
persist for the whole workload. Persist now omits only the pod and
container that failed or reverted. Envelope math counts running
containers only, not run-to-completion init containers.

See [Troubleshooting: Pod-level resource envelope blocked an
increase](troubleshooting.md#pod-level-resource-envelope-blocked-an-increase).

### OneShot records envelope skips

OneShot still walks past a replica that cannot apply so a sibling
can move. If every needing replica is blocked, it keeps the first
blocked pod and emits `ResizeSkipped` plus a Failed
`envelope_constraint` history row, matching Auto. Boost-window CPU
decreases are walked past the same way QoS and node-pressure skips
already were. Envelope and boost skips do not consume
`maxTotalCpuIncrease`, so a later replica is not deferred with a
misleading BudgetExhausted event.

See [Troubleshooting: OneShot skipped the first replica but others
still need a
resize](troubleshooting.md#oneshot-skipped-the-first-replica-but-others-still-need-a-resize)
and [Troubleshooting: Budget
exhausted](troubleshooting.md#budget-exhausted).

### Startup boost stamps only after a successful boost

`attune.io/startup-boost-at` is written only after a successful
boost `/resize`. Expiry of a never-boosted stamp no longer shrinks
the pod. `RequestsAndLimits` boost raises CPU dest only and does
not add a memory limit. CREATE no longer boosts Job or CronJob
pods (expiry skips batch workloads). A leftover Running pod under
HPA `ScaledToZero` no longer receives a startup boost. A malformed
`attune.io/startup-boost-at` blocks CPU decrease the same way a
failed expiry already did.

See [Startup boost](startup-boost.md) and [Troubleshooting: HPA
ScaledToZero left leftover pods at old
requests](troubleshooting.md#hpa-scaledtozero-left-leftover-pods-at-old-requests).

### CREATE fails closed on HPA list and after a safety revert

CREATE no longer sizes a new pod while `ResizeBlocked=HPAListUnavailable`,
or from a recommendation that was just safety-reverted. Scripts that
assumed CREATE would still size while HPA could not be listed will
see those pods admitted at template size until the list succeeds. A
CronJob pod is no longer admitted unsized when its Job is not yet
in the informer cache; admission reads the Job from the API.

See [Troubleshooting: HPAListUnavailable](troubleshooting.md#hpalistunavailable).

### HPA retune is per-pod

HPA auto-tune used to sum CPU across every resized pod, so the
target depended on how many replicas had already resized. Retune is
per-pod From/To. RequestsAndLimits still caps that dest at 100% of
the recommendation dest. ContainerResource HPA metrics on sidecars
are retuned with the resized container. VPA missing-resource hold
ignores temporary boost stamps so the hold does not ratchet up.

See [HPA coexistence](hpa-coexistence.md).

### Immediate safety revert stamps cooldown

An immediate safety revert now stamps cooldown so the same
recommendation is not re-applied on the next reconcile.

### minAllowed / maxAllowed accept bare integers again

CEL rejected bare integers that v0.1.26 accepted. The rule
type-guards `quantity()` so integer-or-string values work again.
Apply CRDs before `helm upgrade` so the restored rule is on the
cluster.

### kubectl attune doctor Ready

`kubectl attune doctor` reports Ready only when every required
check passed. A failed list or webhook check no longer prints Ready.

See [CLI: doctor](../reference/cli.md#doctor).

### Upgrade the chart and image

1. Upgrade the chart to 0.1.29, or set `image.tag` to `0.1.29` or
   `v0.1.29`.
2. Pull `ghcr.io/attune-io/attune:v0.1.29` or
   `ghcr.io/attune-io/attune:0.1.29`. Both tags point at the same
   digest.
3. Helm does not upgrade CRDs on `helm upgrade`. Apply CRDs before the
   chart upgrade so the restored integer-or-string CEL rule is on the
   cluster:

```bash
kubectl apply --server-side --force-conflicts -f \
  https://github.com/attune-io/attune/releases/latest/download/crds.yaml
```

## v0.1.27 to v0.1.28

v0.1.28 keeps existing pod-level resource envelopes valid when
containers resize, and it treats HPA scale-to-zero as idle instead of
a human shutdown. VPA and HPA list failures now skip apply. A
post-resize OOMKill reverts even when kubelet omits FinishedAt.
Existing policy YAML keeps working. Read this section if you use
pod-level `spec.resources`, HPA `minReplicas: 0`, recommend-only VPA,
VPA targets that omit CPU or memory, or scripts that watch Attune
`ResizeDeferred` Events.

### Existing spec.resources envelopes are raised, not invented

When a pod already has `spec.resources`, live `/resize`, CREATE, and
AfterSuccessfulResize persist raise that envelope so the new container
requests stay valid. Attune does not invent an envelope when the field
is missing, and it does not shrink one.

On clusters that report in-place pod-level resources, one `/resize`
writes the container target and raises envelope requests to cover the
container sum. If that capability is off, an increase that would
exceed the envelope is skipped. Decreases still apply. Persist copies
a live envelope onto the template so the next rollout keeps it. If
listing live envelopes fails, persist is skipped instead of writing a
template without the envelope.

See [Troubleshooting: Pod-level resource envelope blocked an
increase](troubleshooting.md#pod-level-resource-envelope-blocked-an-increase).

### HPA ScaledToZero is idle; CREATE still sizes at zero replicas

Workloads with HPA `ScaledToZero=True` skip in-place resize, persist,
startup boost, and eviction. Leftover Running pods stay at their
current requests. That is idle, not a human setting `spec.replicas` to
`0`. CREATE still applies initial sizing when the owner is at zero
replicas, so scale-up pods are sized.

See [HPA coexistence: Scale to
zero](hpa-coexistence.md#scale-to-zero) and
[Troubleshooting: HPA ScaledToZero left leftover pods at old
requests](troubleshooting.md#hpa-scaledtozero-left-leftover-pods-at-old-requests).

### Recommend-only VPA no longer emits VPAConflict

A VPA with `updateMode: Off` is the documented VPA-as-source path.
`VPAConflict` is skipped for `Off`. Applying modes (`Initial`,
`Recreate`, `InPlaceOrRecreate`, `Auto`) still warn.

### Omitted VPA target CPU or memory is unset

A VPA `target` that omits `cpu` or `memory` is unset, not zero. Attune
holds the omitted resource from live pods (or the template) instead of
recommending from zero. Unlimited metrics sampling
(`--max-pods-in-metrics-query=-1`) still lists pods so that hold can
run.

### VPA or HPA list errors skip apply

Any VPA or HPA list error other than a missing VPA CRD now skips
apply, persist, boost, and CREATE. `ResizeBlocked` is
`VPAListUnavailable` or `HPAListUnavailable`. `kubectl attune status`
and `explain` show those reasons. A missing VPA CRD still skips the
conflict check.

See [Troubleshooting: VPAListUnavailable](troubleshooting.md#vpalistunavailable)
and [Troubleshooting: HPAListUnavailable](troubleshooting.md#hpalistunavailable).

### Post-resize OOMKill classifies without a later FinishedAt

Attune classifies OOMKilled on the current Terminated state or
LastTerminationState, including a 2s kubelet skew. It no longer waits
for FinishedAt to be strictly after resized-at. After a successful
safety revert, cooldown is stamped so the same reconcile does not
re-apply the shrink.

### Attune no-op Events are ResizeUnchanged

The Attune no-op Event reason is `ResizeUnchanged` (applied target
already matches live after filtering, dest clamp, or usage floor).
Kubernetes 1.36 kubelet still uses `ResizeDeferred` when the node is
out of room. Scripts that watched Attune `ResizeDeferred` for no-ops
should watch `ResizeUnchanged`.

### kubectl attune doctor optional cgroup v2 WARN

`kubectl attune doctor` prints an optional `cgroup v2` row. Default is
`WARN` with `could not determine` (expected on k3s, kind, and most
managed clusters). Missing `pods/resize` remains a required FAIL. The
optional row does not change the exit code.

See [Troubleshooting: Doctor cgroup v2 is
WARN](troubleshooting.md#doctor-cgroup-v2-is-warn).

### Upgrade the chart and image

1. Upgrade the chart to 0.1.28, or set `image.tag` to `0.1.28` or
   `v0.1.28`.
2. Pull `ghcr.io/attune-io/attune:v0.1.28` or
   `ghcr.io/attune-io/attune:0.1.28`. Both tags point at the same
   digest.
3. Helm does not upgrade CRDs on `helm upgrade`. Apply CRDs before the
   chart upgrade if you need new schema fields:

```bash
kubectl apply --server-side --force-conflicts -f \
  https://github.com/attune-io/attune/releases/latest/download/crds.yaml
```

## v0.1.26 to v0.1.27

v0.1.27 tightens last-replica eviction, limit-only resizes, the memory
usage floor, and kubectl `--sort-by` validation. It also adds a
fail-closed namespace freeze kill-switch. Existing policy YAML keeps
working. Read this section if you use `InPlaceOrRecreate`,
`controlledValues: RequestsAndLimits`, memory limit decreases, eviction
metrics, `kubectl attune --sort-by`, hand-managed RBAC, OneShot, or
`maxTotalCpuIncrease` / `maxTotalMemoryIncrease`.

### Namespace reads are now required

Apply now reads the policy namespace for `attune.io/freeze=true` and
**fails closed** if that Get fails. A missing `namespaces` `get`,
`list`, and `watch` grant looks like a cluster-wide freeze:

- no in-place resize, eviction, startup boost, or template persist
- CREATE initial sizing is skipped
- `ResizeBlocked=True` with `reason=NamespaceFrozen`
- webhook allow message:
  `cannot read namespace for attune.io/freeze; skipping initial sizing (check namespaces get/list/watch RBAC)`

Chart installs with `rbac.create=true` already have the verbs. If you
maintain your own ClusterRole, add:

```yaml
- apiGroups: [""]
  resources: [namespaces]
  verbs: [get, list, watch]
```

Then confirm the operator ServiceAccount can `get` the policy namespace.
See [Troubleshooting: NamespaceFrozen](troubleshooting.md#namespacefrozen).
The same annotation (`attune.io/freeze=true`) is the intentional
incident kill-switch: recommendations and pending safety revert still
run.

### Total increase budgets now warn at admission

Setting `maxTotalCpuIncrease` or `maxTotalMemoryIncrease` emits an
admission warning that those fields are deprecated. Prefer
`maxCpuIncreasePerMinute` / `maxMemoryIncreasePerMinute` so the cap
does not depend on `reconcileInterval`. The old fields still apply
until you migrate.

### OneShot selection walks past a blocked first replica

OneShot no longer sticks on the first listed replica when that pod is
already at the applied target, or when MemoryPressure, Infeasible plus
InPlaceOnly, quota, or QoS would skip it. Later replicas in the same
workload can now move in the same cycle.

### RequestsAndLimits can apply a limit-only resize

When requests already match the recommendation, `RequestsAndLimits` may
still apply a resize that only changes limits (for example a missing or
drifted memory limit). Previously a request match could skip the whole
container. Check resize history if you see limit-only updates.

### Memory usage floor uses the live container limit

The usage floor compares the target against the live container limit on
the pod, not the workload template. After an in-place resize the template
(and `rec.Current`) can lag. Alerts that assume the template limit is
authoritative should use the live pod instead. Persist applies the same
floor when writing the template, so a lagged `rec.Current` cannot persist
a limit below recent usage.

### Last-replica eviction uses a live Running count

Eviction fallback lists pods through the typed Clientset and counts only
`status.phase=Running` with no deletion timestamp. `spec.replicas` and
NotReady or Pending pods do not count. The list is paginated and stops
once two Running pods are visible. Concurrent resizes on the same
workload serialize that list-plus-evict so two replicas cannot both be
evicted in one pass. History reasons are `eviction_last_replica`,
`eviction_denied`, `eviction_list_failed`, and `eviction_no_selector`.
A failed eviction attempt is not retried for remaining containers on the
same pod in that cycle.

### kubectl attune --sort-by rejects unknown values

`kubectl attune status` and `kubectl attune savings` now exit 1 when
`--sort-by` is not `name`, `namespace`, `savings`, or `age`. Scripts that
passed a typo and got unsorted output now fail closed.

### attune_eviction_total result labels

`attune_eviction_total` now increments for skipped attempts as well as
successes. The `result` label is `success`, `denied`, `last_replica`,
`list_failed`, or `no_selector`. Alerts that treated every increment as
an eviction should filter `result="success"`.

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

A VPA `target` that omits `cpu` or `memory` is unset, not zero. Attune
holds the live pod request when pods are listed, otherwise the last rec
or template request. A cpu-only target no longer feeds `0` into the
memory engine, and does not reset live memory back to the template.

A VPA list error (RBAC or apiserver) no longer looks like "no VPA CRD".
Apply is skipped (`ResizeBlocked=VPAListUnavailable`) so an applying VPA
is not treated as absent. A missing CRD still skips the conflict check.

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
