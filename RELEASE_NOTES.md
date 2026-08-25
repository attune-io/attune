## What's New in v0.1.25

v0.1.25 adds `kubectl attune doctor` so you can check Kubernetes 1.32+ and `pods/resize` before you wait on recommendations. Canary is per app: cooldown, CREATE sizing, startup boost, and HPA retune no longer lock every workload on the same policy. Helm chart 0.1.25 defaults to the bare SemVer image tag that this release publishes.

### Highlights

- Run `kubectl attune doctor` after install to confirm the cluster can in-place resize.
- Multi-app canary policies promote, cool down, and CREATE-size each app on its own clock.

### kubectl attune doctor

- **There was no preflight command to confirm the cluster can in-place resize.** `kubectl attune doctor` checks the server version, discovery of `pods/resize`, and GET `/-/healthy` on Prometheus addresses from policies and defaults. The ping runs on the machine that invoked kubectl, not inside the operator pod. In-cluster DNS (`.svc` / `.cluster.local`) is skipped. HTTP 401/403 on an address that sets `bearerTokenSecret` or headers is skipped, because doctor does not send the operator's credentials. Optional Prometheus failures print `WARN` and keep exit code 0 ([#582](https://github.com/attune-io/attune/pull/582), [#583](https://github.com/attune-io/attune/pull/583), [#585](https://github.com/attune-io/attune/pull/585), [#589](https://github.com/attune-io/attune/pull/589)).

### Canary isolation

- **Canary observation started before any pod was resized.** The clock now starts on the first successful in-place canary resize. A revert clears that app's clock ([#568](https://github.com/attune-io/attune/pull/568)).

- **One app's cooldown or canary slice locked every other app on the policy.** Cooldown, CREATE initial sizing, startup boost, and HPA retune are per app. After a cooldown skip, the operator requeues when that stamp expires instead of waiting a full new cooldown. Selector-based policies fetch the owning Deployment, StatefulSet, or DaemonSet so CREATE isolation applies there too ([#571](https://github.com/attune-io/attune/pull/571), [#572](https://github.com/attune-io/attune/pull/572), [#574](https://github.com/attune-io/attune/pull/574), [#575](https://github.com/attune-io/attune/pull/575)).

- **A request increase only compared this pod to node allocatable.** The operator now subtracts other pods on the node before growing requests ([#571](https://github.com/attune-io/attune/pull/571)).

- **ReplicaSet CREATE often has an empty pod name.** Canary matching does not treat that empty name as a canary-slice identity. When CREATE has no assigned name, Info logs use `generateName`. Verbosity 1 records the skip, including an empty `pod` field ([#587](https://github.com/attune-io/attune/pull/587), [#589](https://github.com/attune-io/attune/pull/589)).

### GitOps pull requests

- **After upgrading from 0.1.22, cooldown expiry opened another empty GitOps PR.** When the last-attempt URL is set and the drift fingerprint annotation is empty, the operator adopts the current table instead of opening a duplicate. A failed API open (empty URL) still retries ([#577](https://github.com/attune-io/attune/pull/577), [#580](https://github.com/attune-io/attune/pull/580)).

### CLI CSV output

- **`kubectl attune recommendations -o csv` and `savings -o csv` write a header plus one data row per policy or container.** The recommendations last column is `confidence_or_status`: a confidence percent when recommendations exist, otherwise the policy Ready reason ([#581](https://github.com/attune-io/attune/pull/581), [#586](https://github.com/attune-io/attune/pull/586)).

### Helm image tag

- **Chart 0.1.24 prefixed `v` onto `appVersion` when `image.tag` was empty.** This release publishes both `v0.1.25` and `0.1.25`. An empty `image.tag` now uses bare `appVersion` (`ghcr.io/attune-io/attune:0.1.25`). An explicit `image.tag` is left unchanged ([#554](https://github.com/attune-io/attune/pull/554)).

### Upgrade notes

1. Upgrade the chart to 0.1.25, or set `image.tag` to `0.1.25` or `v0.1.25`.
2. Pull `ghcr.io/attune-io/attune:v0.1.25` or `ghcr.io/attune-io/attune:0.1.25`. Both tags point at the same digest.
3. Chart 0.1.24 still defaults to `:v0.1.24`. Chart 0.1.23 still needs `--set image.tag=v0.1.23`.
4. Run `kubectl attune doctor` after install to confirm Kubernetes 1.32+ and `pods/resize`.
5. Multi-app canary policies now promote per app. Check `status.canary.workloads[]` for each app's phase and remaining cooldown.

### Full changelog

https://github.com/attune-io/attune/compare/v0.1.24...v0.1.25
