## What's New in v0.1.24

v0.1.24 is a reliability patch. A default Helm install pulls an image tag that exists, a committed resize is no longer undone when annotation persist times out, cluster metrics defaults inherit one provider with the same field rules as policies, and GitOps skips a pull request when the drift table has not changed.

### Highlights

- Helm chart 0.1.24 defaults to `ghcr.io/attune-io/attune:v0.1.24`. Chart 0.1.23 still needs `--set image.tag=v0.1.23`.
- After a successful annotation persist, a client timeout no longer reverts the in-place resize.

### Helm install image tag

- **`helm install` without `--set image.tag` pulled `:0.1.23`, which is not a published image tag.** Git tags and container images use `v0.1.23`. The chart now prefixes `v` onto `appVersion` when `image.tag` is empty. This release also publishes the bare SemVer alias (`0.1.24`) so older charts that still use `appVersion` as the tag can pull ([#547](https://github.com/attune-io/attune/pull/547), [#548](https://github.com/attune-io/attune/pull/548)).

### Resize tracking annotations

- **A resize could revert after the tracking annotations were already written.** If the persist write committed and the client then timed out, the operator treated that as `annotation-persist-failed` and rolled the spec back. It now confirms the annotations with a live Get (retries included) and only reverts when the write did not land ([#545](https://github.com/attune-io/attune/pull/545), [#548](https://github.com/attune-io/attune/pull/548), [#549](https://github.com/attune-io/attune/pull/549)).

### AttuneDefaults metrics source

- **A cluster Datadog, CloudWatch, or VPA default was ignored, or two providers could be copied onto a policy.** Policies that set no provider inherit at most one, in the same order as admission (Prometheus, Datadog, CloudWatch, VPA). `AttuneDefaults` and `AttuneNamespaceDefaults` now reject two providers and apply the same required-field rules as `AttunePolicy` (API key, region, cluster, VPA name, recording-metric names) ([#548](https://github.com/attune-io/attune/pull/548), [#549](https://github.com/attune-io/attune/pull/549), [#550](https://github.com/attune-io/attune/pull/550), [#551](https://github.com/attune-io/attune/pull/551)).

### GitOps pull requests

- **Unchanged drift still opened a GitOps pull request.** When `attune.io/gitops-pr-drift` matches the current table, the operator records `PullRequestUnchanged` and does not open another empty notification PR ([#538](https://github.com/attune-io/attune/pull/538)).

### Upgrade notes

1. Upgrade the chart to 0.1.24, or keep `--set image.tag=v0.1.24`. Chart 0.1.23 still needs `--set image.tag=v0.1.23` until you upgrade.
2. Pull `ghcr.io/attune-io/attune:v0.1.24` (or `ghcr.io/attune-io/attune:0.1.24`). Both tags point at the same digest.
3. If an `AttuneDefaults` object already has two metrics providers, the next edit is rejected until you leave only one.

### Full changelog

https://github.com/attune-io/attune/compare/v0.1.23...v0.1.24
