## What's New in v0.1.26

v0.1.26 adds a waste GRADE on `kubectl attune recommendations` so you can see over-request and under-request at a glance. When Prometheus (or another metrics backend) goes quiet, recommendations are marked stale and the operator stops resizing, boosting, exporting, and opening GitOps PRs from that data.

### Highlights

- `kubectl attune recommendations` prints GRADE (A-F waste, or `U` when current is more than 10% under the rec).
- Stale recommendations stay visible but do not drive apply paths until fresh samples arrive.
- Ready reason `MetricsUnavailable` replaces `PrometheusUnavailable` for every metrics backend.

### Waste grade

- **The recommendations table had no letter for request waste.** GRADE is the worse of CPU or memory waste: A under 10% over (or up to 10% under), B 10-25%, C 25-50%, D 50-75%, F 75% or more. `U` when current is more than 10% below the recommendation, including a zero request. `U` wins if one resource is under and the other is waste. `-` while collecting, when the rec is stale, or when quantities cannot be compared. CSV adds a `grade` column before `confidence_or_status` ([#604](https://github.com/attune-io/attune/pull/604), [#606](https://github.com/attune-io/attune/pull/606)).

### Stale recommendations

- **Empty or gapped Prometheus data used to look like a live rec.** The operator now sets `status.recommendations[].stale` when the newest finite sample is older than `3 * queryStep` (default 15m), or when the last rec is reused after an empty query. Reuse expires after that same bound. Stale recs do not count toward Ready or savings ([#610](https://github.com/attune-io/attune/pull/610), [#620](https://github.com/attune-io/attune/pull/620)).

- **Resize was not the only leftover consumer.** Startup boost, CREATE initial sizing, template persistence, ConfigMap export, `kubectl attune` preview/diff/wizard, and GitOps drift all skip while `stale` is true. The CLI GRADE is `-` and CONFIDENCE / STATUS prints `stale`. The ConfigMap `last-updated` stamp does not move ([#609](https://github.com/attune-io/attune/pull/609), [#611](https://github.com/attune-io/attune/pull/611)).

### Safety and hold

- **A probe blip right after `/resize` triggered an immediate revert.** Immediate post-apply safety now checks OOM and restart spikes only. NotReady, throttle, and SLO stay on the observation window ([#627](https://github.com/attune-io/attune/pull/627)).

- **A one-sided sample gap (CPU present, memory missing, or the reverse) could write template requests.** The operator holds the last rec instead. Hold prefers last rec over a live template snapshot and drops leftover template limits so they do not stick after the hold lifts ([#625](https://github.com/attune-io/attune/pull/625), [#626](https://github.com/attune-io/attune/pull/626)).

- **A failed policy List looked like bootstrap (no conflicts).** The operator now keeps last recs, sets Ready `ConflictCheckFailed`, and skips CREATE initial sizing for that policy until List succeeds ([#660](https://github.com/attune-io/attune/pull/660)).

- **ResourceQuota names that are not `requests.cpu` / `requests.memory` were ignored.** Common aliases such as `cpu` and `memory` are honored ([#621](https://github.com/attune-io/attune/pull/621)).

### GitOps and CloudWatch

- **CloudWatch `PodName="api-*"` matched a pod named `api-*`, not a prefix.** SEARCH is exact. Prefix matching is now client-side ([#620](https://github.com/attune-io/attune/pull/620)).

- **GitOps to a self-hosted forge on a private IP was blocked as SSRF.** `allowPrivateEndpoints` permits RFC1918 and ULA hosts. Link-local IMDS (`169.254.169.254` and `fd00:ec2::254`) stays blocked. Corporate `HTTPS_PROXY` no longer fails the GitOps dial pin ([#620](https://github.com/attune-io/attune/pull/620), [#621](https://github.com/attune-io/attune/pull/621)).

- **GitLab merge requests were matched by title only.** Matching now uses the configured base branch. Labels on an existing MR are not wiped on update. An empty rebase bootstrap is not treated as a new open ([#622](https://github.com/attune-io/attune/pull/622), [#624](https://github.com/attune-io/attune/pull/624)).

### Ready, defaults, and doctor

- **Ready said `PrometheusUnavailable` even for Datadog or CloudWatch.** The reason is now `MetricsUnavailable`. Alerts that match the old reason should also match the new one. The operator still treats the old reason as bootstrap until the next reconcile overwrites it ([#654](https://github.com/attune-io/attune/pull/654), [#655](https://github.com/attune-io/attune/pull/655), [#656](https://github.com/attune-io/attune/pull/656)).

- **AttuneDefaults Datadog or CloudWatch blocks were ignored (windows only).** The policy inherits the provider pointer when it set none. HPA absolute targets are preserved. Query URLs are sanitized before use ([#603](https://github.com/attune-io/attune/pull/603)).

- **Datadog collectors with the same app key but different query options shared a cache entry.** Cache keys now include the app key plus query options. NaN/Inf samples increment `attune_nan_inf_samples_total` ([#653](https://github.com/attune-io/attune/pull/653)).

### Upgrade notes

1. Upgrade the chart to 0.1.26, or set `image.tag` to `0.1.26` or `v0.1.26`.
2. Pull `ghcr.io/attune-io/attune:v0.1.26` or `ghcr.io/attune-io/attune:0.1.26`. Both tags point at the same digest.
3. Apply CRDs before `helm upgrade` if you need new schema fields (`kubectl apply --server-side --force-conflicts` from the release `crds.yaml`).
4. `kubectl attune recommendations` now has a GRADE column. Update any parser that assumed the old column order. CSV is `...,mem_rec,grade,confidence_or_status`.
5. If Ready shows `stale` on a rec, fix metrics reachability. Resize and export resume when fresh samples arrive. Do not treat a frozen ConfigMap `last-updated` as current.
6. Alerts that match `reason="PrometheusUnavailable"` should also match `reason="MetricsUnavailable"`.
7. GitOps to a private forge needs `allowPrivateEndpoints`. IMDS stays blocked.

See [Upgrading](https://github.com/attune-io/attune/blob/main/docs/guides/upgrading.md) for `maxConcurrentResizes` inherit and other 0.1.26 behavior notes.

### Full changelog

https://github.com/attune-io/attune/compare/v0.1.25...v0.1.26
