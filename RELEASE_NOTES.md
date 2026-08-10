## What's New in v0.1.22

v0.1.22 is a scale and safety release for large fleets. Recommendations default to the hottest pod per container (cheaper Prometheus queries), the operator uses less API and informer memory, safety throttle checks batch and chunk correctly under rate limits, and request increases no longer apply when node status is missing or under pressure. If you run high-replica Deployments or multi-pod policies, read the upgrade notes before rolling out.

### Highlights

- Default PromQL aggregation is **Max** (`max by (container)`), so recommendation cost tracks containers rather than every pod replica.
- Scale hot paths for large fleets: namespace-wide pod lists, sample pods for metrics, batch and chunked throttle queries, informer field stripping, default max concurrent reconciles of 2.
- Stronger MemoryPressure and capacity behavior, including fail-closed when node status is unavailable for request increases.
- Full K8s **1.32–1.35** E2E Nightly matrix and fuzz tests green on the release candidate tip.

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
- Large-fleet gaps closed: namespace-wide pod list, metrics pod sampling, history and step clamps, blocker throttle status preservation, workload and HPA informer strip, batch safety throttle, default `--max-concurrent-reconciles=2` ([#490](https://github.com/attune-io/attune/pull/490), [#491](https://github.com/attune-io/attune/pull/491)).
- Production `RateLimitedCollector` implements batch throttle so multi-pod safety uses one PromQL query instead of N rate-limited singles ([#502](https://github.com/attune-io/attune/pull/502), [#501](https://github.com/attune-io/attune/pull/501)).
- Large observation sets chunk batch throttle PromQL into groups of 64 pod/container pairs, with one rate-limit token per chunk (not one token for the whole set). A mid-batch query failure returns no partial map, so silent pods are not treated as "no throttle" ([#507](https://github.com/attune-io/attune/pull/507), [#508](https://github.com/attune-io/attune/pull/508), [#511](https://github.com/attune-io/attune/pull/511), [#514](https://github.com/attune-io/attune/pull/514)).

### Capacity and MemoryPressure

- Hold the MemoryPressure gate against a stale node cache; live Clientset re-check immediately before `UpdateResize` ([#476](https://github.com/attune-io/attune/pull/476), [#479](https://github.com/attune-io/attune/pull/479)).
- Fail-closed on request increases when node status is unavailable (`attune_capacity_skip_total{reason="unavailable"}`). Decreases still proceed ([#485](https://github.com/attune-io/attune/pull/485)).
- Hardened MemoryPressure and OOMKill E2E paths against kubelet inject races and first-resize flakiness ([#482](https://github.com/attune-io/attune/pull/482), [#510](https://github.com/attune-io/attune/pull/510)).

### Docs and operations

- Scaling ops checklist and high-replica guidance ([#487](https://github.com/attune-io/attune/pull/487)).
- Enriched nightly failure GitHub issues with failed jobs and first Go E2E FAIL lines ([#485](https://github.com/attune-io/attune/pull/485)).
- New-user install docs: cert-manager and NetworkPolicy Prometheus port footguns, Helm Deployment name `attune` vs raw/OLM names, plugin status columns, expanded v0.1.22 upgrade section ([#512](https://github.com/attune-io/attune/pull/512), [#514](https://github.com/attune-io/attune/pull/514)).
- Docs Deploy on main no longer cancels an in-flight Pages deploy when a newer Docs run starts ([#489](https://github.com/attune-io/attune/pull/489)).

### Upgrade notes

1. **Review Max aggregation.** Existing policies with unset `podAggregation` switch from unaggregated series to Max. Set `None` or `Avg` only if you need the previous multi-pod sample pool.
2. **Default concurrency is 2** for reconciles (`--max-concurrent-reconciles`). Helm `clusterSize` presets still override when set.
3. **Request increases fail closed** when node status cannot be read. Watch `attune_capacity_skip_total{reason="unavailable"}` if increases stop unexpectedly.
4. Refresh CRDs with the release install path (`helm upgrade` or `dist/crds.yaml` / `dist/install.yaml` from the tag).
5. Re-apply Grafana dashboard and PrometheusRule assets if you manage them out of band.

```bash
kubectl attune status -A
kubectl attune explain -n <namespace> <policy>
```

Full upgrade detail: [Upgrading](https://attune-io.github.io/attune/guides/upgrading/).

### Full changelog

https://github.com/attune-io/attune/compare/v0.1.21...v0.1.22
