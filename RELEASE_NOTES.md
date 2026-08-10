## What's New in v0.1.22

v0.1.22 is a scale and safety release. Large fleets get cheaper Prometheus queries by default, the operator uses less API and informer memory, and node pressure or missing node status no longer allows unsafe request increases. If you run high-replica Deployments or multi-pod policies, read the upgrade notes before rolling out.

### Highlights

- Default PromQL aggregation is **Max** (`max by (container)`), so recommendation cost tracks containers rather than every pod replica.
- Scale hot paths: namespace-wide pod lists, sample pods for metrics, batch throttle queries, informer field stripping, default max concurrent reconciles of 2.
- Stronger MemoryPressure and capacity skip behavior, including fail-closed when node status is unavailable for request increases.

### Behavioral change: default pod aggregation is Max

When `metricsSource.podAggregation` is unset, Attune now defaults to **Max**. Recommendations follow the hottest pod for each container name, and range query series counts stay proportional to containers.

If you relied on the legacy multi-pod sample pool (unaggregated series), set:

```yaml
spec:
  metricsSource:
    podAggregation: None   # or Avg
```

See [Upgrading](https://attune-io.github.io/attune/guides/upgrading/) and [Scaling](https://attune-io.github.io/attune/guides/scaling/).

### Scale and performance

- PromQL Max aggregation by default, with status and docs for series caps and recording rules ([#488](https://github.com/attune-io/attune/pull/488), [#496](https://github.com/attune-io/attune/pull/496), [#499](https://github.com/attune-io/attune/pull/499)).
- Large-fleet gaps closed: NS-wide pod list, metrics pod sampling, history and step clamps, blocker throttle status preservation, workload and HPA informer strip, batch safety throttle, default `--max-concurrent-reconciles=2` ([#490](https://github.com/attune-io/attune/pull/490), [#491](https://github.com/attune-io/attune/pull/491)).
- Production `RateLimitedCollector` implements batch throttle so multi-pod safety uses one PromQL query instead of N rate-limited singles ([#502](https://github.com/attune-io/attune/pull/502), [#501](https://github.com/attune-io/attune/pull/501)).
- Large observation sets chunk batch throttle PromQL into groups of 64 pod/container pairs, with one rate-limit token per chunk (not one token for the whole set) ([#507](https://github.com/attune-io/attune/pull/507), [#508](https://github.com/attune-io/attune/pull/508), [#511](https://github.com/attune-io/attune/pull/511)).

### Capacity and MemoryPressure

- Hold the MemoryPressure gate against stale node cache; live Clientset re-check before `UpdateResize` ([#476](https://github.com/attune-io/attune/pull/476), [#479](https://github.com/attune-io/attune/pull/479)).
- Fail-closed on request increases when node status is unavailable (`attune_capacity_skip_total{reason="unavailable"}`) ([#485](https://github.com/attune-io/attune/pull/485)).
- Hardened MemoryPressure E2E against kubelet inject races ([#482](https://github.com/attune-io/attune/pull/482)).

### Docs and operations

- Scaling ops checklist and high-replica guidance ([#487](https://github.com/attune-io/attune/pull/487)).
- Docs Deploy no longer cancels in-flight main runs ([#489](https://github.com/attune-io/attune/pull/489)).
- Enriched nightly failure GitHub issues with failed jobs and first Go E2E FAIL lines ([#485](https://github.com/attune-io/attune/pull/485)).
- New-user install docs: cert-manager and NetworkPolicy Prometheus port footguns, Helm Deployment name `attune`, plugin status columns ([#512](https://github.com/attune-io/attune/pull/512)).

### Upgrade notes

1. **Review Max aggregation.** Existing policies with unset `podAggregation` switch from unaggregated series to Max. Set `None` or `Avg` only if you need the previous multi-pod sample pool.
2. **Default concurrency is 2** for reconciles (`--max-concurrent-reconciles`). Helm `clusterSize` presets still override when set.
3. Refresh CRDs with the release install path (`helm upgrade` or `dist/crds.yaml` / `dist/install.yaml` from the tag).
4. Re-apply Grafana dashboard and PrometheusRule assets if you manage them out of band.

```bash
kubectl attune status -A
kubectl attune explain -n <namespace> <policy>
```

Full upgrade detail: [Upgrading](https://attune-io.github.io/attune/guides/upgrading/).

### Full changelog

https://github.com/attune-io/attune/compare/v0.1.21...v0.1.22
