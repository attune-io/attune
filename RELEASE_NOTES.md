## What's New in v0.1.19

v0.1.19 is a reliability-focused patch release. It hardens safety observation and startup boost under real cluster churn, closes overnight schedule edge cases, and stops non-finite metric values from silently poisoning recommendations and exported ConfigMaps. If you run Auto mode with safety reverts, scheduled windows, or startup boost, this release is worth taking promptly.

### Safer resize history and observation

A single safety revert no longer rewrites every prior successful history entry for the same workload and container. Only the **most recent resize cycle** is marked reverted, so `consecutiveReverts` no longer inflates and the policy does not jump to Degraded or escalated backoff after one event.

When the controller cannot list pods during safety observation (transient API errors), it now treats observation as still pending and requeues on the short observation interval instead of waiting a full cooldown. That closes a gap where a failed list could delay safety detection for many minutes.

([#389](https://github.com/attune-io/attune/pull/389))

### Overnight schedule windows

Scheduled resize windows that span midnight (for example `22:00`–`06:00`) now honor `daysOfWeek` correctly for the post-midnight portion. Previously, a window allowed only on Wednesday could incorrectly block resizes after midnight into Thursday even though they still belonged to Wednesday's overnight window.

([#389](https://github.com/attune-io/attune/pull/389))

### Startup boost respects policy ceilings and survives conflicts

Startup boost CPU is capped at `spec.cpu.maxAllowed` before any container limit clamp. A large boost multiplier can no longer push requests above the admin-configured ceiling when the container limit is higher than `maxAllowed`.

Writing the boost timestamp annotation now retries on conflict with a re-fetch. A 409 from concurrent kubelet status updates no longer leaves the annotation unset, which previously could prevent boost expiry and keep elevated CPU indefinitely.

([#388](https://github.com/attune-io/attune/pull/388), [#389](https://github.com/attune-io/attune/pull/389))

### Non-finite metrics and scaling

NaN and Inf values from Prometheus-adjacent paths, Datadog, and CloudWatch are rejected or filtered before they enter recommendation math or safety threshold checks. Confidence written into export ConfigMaps is coerced to a finite value so consumers never see the strings `NaN` or `+Inf`.

Resource scaling also clamps int64 overflow to `MaxInt64` instead of wrapping to a negative quantity, which could otherwise produce invalid resource recommendations under extreme factors.

`RevertPod` treats a deleted pod as a successful no-op instead of retrying a missing object during observation.

([#385](https://github.com/attune-io/attune/pull/385), [#388](https://github.com/attune-io/attune/pull/388), [#389](https://github.com/attune-io/attune/pull/389))

### Helm chart defaults and schema

The Helm `AttuneDefaults` template no longer fails when `defaults.enabled` is true and `updateStrategy` is null: the updateStrategy block is only rendered when present.

`values.schema.json` now validates PrometheusRule severity enums, tightens canary and SLO guardrail object shapes, and requires `name` / `query` / `threshold` on SLO items so typos fail at install time instead of at runtime.

([#387](https://github.com/attune-io/attune/pull/387))

### kubectl plugin and docs

`kubectl attune explain` prints additional effective fields that were missing from the output, including Weight, ExcludedContainers, Schedule, Canary, and SLOGuardrails.

Configuration reference docs gain dedicated sections for `controlledValues`, `allowDecrease`, `startupBoost`, and `burstSensitivity`, and correct a few defaults and reason strings that had drifted from the code.

([#384](https://github.com/attune-io/attune/pull/384), [#385](https://github.com/attune-io/attune/pull/385))

### Dependencies and tests

- Base image digest update for `distroless/static`
- Go module patch/minor bumps (AWS SDK v2 CloudWatch path, `prometheus/common`, `golang.org/x/sync`)
- GitHub Actions used in CI and release bumped
- Regression tests for startup boost maxAllowed cap, export confidence NaN handling, safety list failure requeue, and empty defaults Type inheritance

([#390](https://github.com/attune-io/attune/pull/390), [#391](https://github.com/attune-io/attune/pull/391), [#392](https://github.com/attune-io/attune/pull/392), [#393](https://github.com/attune-io/attune/pull/393))

### Upgrading

No CRD or API breaking changes. Standard upgrade paths apply.

```bash
# Helm (recommended)
helm upgrade attune oci://ghcr.io/attune-io/charts/attune \
  --namespace attune-system

# OpenShift / OLM
# Upgrade the existing Subscription on channel "stable" (package: attune)
```

See the [installation guide](https://attune-io.github.io/attune/getting-started/installation/) for details.

### Full changelog

https://github.com/attune-io/attune/compare/v0.1.18...v0.1.19
