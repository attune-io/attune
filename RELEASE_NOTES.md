## What's New in v0.1.23

v0.1.23 is a reliability patch. Fleet savings totals stay finite, first recommendations no longer wait extra jitter while metrics are still collecting, and the operator image is built with Go 1.26.6.

### Highlights

- Fleet report monthly savings skip Inf, NaN, and other unparseable values, and count them in `unparseableSavings`.
- Requeue jitter is skipped while Ready is `InsufficientData` or `PrometheusUnavailable`.

### Fleet report

- **Inf or NaN savings could make the fleet USD total Inf, or look like $0 after a silent drop.** `estimatedMonthlySavingsUSD` now adds only finite numbers. Unparseable values increment `unparseableSavings`. The operator logs at Info when that count is nonzero ([#532](https://github.com/attune-io/attune/pull/532), [#533](https://github.com/attune-io/attune/pull/533)).
- **Huge or negative reclaim quantities could wrap to the wrong sign.** CPU and memory reclaim sums reject those Quantity values and saturate int64 addition ([#533](https://github.com/attune-io/attune/pull/533)).

### Bootstrap timing

- **First recommendations and ConfigMap export could wait up to cooldown plus `--requeue-jitter` (default 2m).** Jitter no longer applies while Ready is `InsufficientData` or `PrometheusUnavailable`. Helm values, `--requeue-jitter` help, and troubleshooting describe that skip ([#531](https://github.com/attune-io/attune/pull/531), [#532](https://github.com/attune-io/attune/pull/532)).

### Build and security

- Operator and `kubectl attune` now build with Go 1.26.6. `go.mod` and the Dockerfile stay on the same patch ([#524](https://github.com/attune-io/attune/pull/524)).

### Upgrade notes

1. If you read fleet `report.json`, treat `unparseableSavings` as additive. A successful export can still show `$0` when every savings field failed to parse.
2. Short-cooldown policies collect first recommendations sooner. Steady-state cooldown jitter is unchanged.
3. Pull `ghcr.io/attune-io/attune:v0.1.23` (or rebuild from the tag) for the Go 1.26.6 toolchain.

### Full changelog

https://github.com/attune-io/attune/compare/v0.1.22...v0.1.23
