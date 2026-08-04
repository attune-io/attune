## What's New in v0.1.21

v0.1.21 is a large product release. Attune now helps with GitOps handoff (versioned export schemas and optional pull request automation), multi-cluster fleet reports, safer memory work on Kubernetes 1.35+, language runtime profiles, and clearer operator status when resizes are Deferred or Infeasible. If you run GitOps, multi-cluster fleets, JVM-heavy workloads, or mixed 1.33–1.35 clusters, read the upgrade notes below before rolling out.

### Highlights

- Opt-in **GitOps pull request** automation (with dry-run) plus **versioned recommendation ConfigMaps** for durable export.
- **Fleet report** ConfigMap for multi-cluster rollups, with metrics and dashboard panels.
- **Kubernetes 1.35+** live memory limit decreases, with a **usage floor** so limits stay above recent usage.
- **Runtime profiles** (java, python, nodejs, golang) apply safe in-memory memory defaults.
- **ResizeBlocked** status when pods are Deferred or Infeasible, plus capacity and node-pressure skip metrics.

### GitOps and multi-cluster

- **Versioned recommendation export.** ConfigMaps written for GitOps include `schema-version` and an `attune.io/export-schema` label so pipelines can evolve safely ([#438](https://github.com/attune-io/attune/pull/438)).
- **Opt-in GitOps pull request automation.** When export pull requests are enabled, Attune can open or update PRs against your forge (dry-run path for CI without credentials). Missing head branches can be bootstrapped from the base branch ([#449](https://github.com/attune-io/attune/pull/449), [#456](https://github.com/attune-io/attune/pull/456), [#455](https://github.com/attune-io/attune/pull/455)).
- **Multi-cluster fleet report.** Optional fleet report ConfigMap export for federation-friendly rollups, with Grafana and PrometheusRule coverage ([#452](https://github.com/attune-io/attune/pull/452)).

See [GitOps integration](https://attune-io.github.io/attune/guides/gitops-integration/) and [Multi-cluster](https://attune-io.github.io/attune/guides/multi-cluster/).

### Memory safety and Kubernetes 1.35+

- **Version-aware memory limit decreases.** On Kubernetes 1.35+, Attune no longer clamps live memory limit decreases the way it does on 1.33–1.34 when the platform allows them and the policy uses `controlledValues: RequestsAndLimits` with decrease allowed. On 1.33–1.34, limit decreases remain clamped as before ([#439](https://github.com/attune-io/attune/pull/439)).
- **Usage floor on limit decreases.** Target memory limits stay above recent usage times `(1 + decreaseUsageMarginPercent/100)` (default 10 percent), reducing OOM races when shrinking limits ([#447](https://github.com/attune-io/attune/pull/447)).
- **Language runtime profiles.** Set `runtimeProfile` to `java`, `python`, `nodejs`, or `golang` for safe in-memory defaults (for example java uses higher memory overhead and does not enable memory decrease unless you set it). Defaults are not written back onto the CR ([#440](https://github.com/attune-io/attune/pull/440)).

See [Runtime profiles](https://attune-io.github.io/attune/guides/runtime-profiles/) and [Upgrading](https://attune-io.github.io/attune/guides/upgrading/).

### Capacity, pressure, and stuck resizes

- **Node pressure-aware skips.** Request *increases* are skipped when the node has MemoryPressure (memory), DiskPressure, or PIDPressure, so Attune does not pile more demand onto a stressed node. Decreases still proceed ([#441](https://github.com/attune-io/attune/pull/441)).
- **Capacity skip metrics and reclaimed capacity.** Prometheus metrics and Grafana panels for capacity and pressure skips, plus reclaimed request capacity signals ([#448](https://github.com/attune-io/attune/pull/448)).
- **Deferred and Infeasible operator UX.** Policy status surfaces `workloads.deferred` and `workloads.infeasible`, a `ResizeBlocked` condition with clear reasons, and related metrics and alerts ([#436](https://github.com/attune-io/attune/pull/436)).

See [Troubleshooting: Deferred or Infeasible](https://attune-io.github.io/attune/guides/troubleshooting/#deferred-or-infeasible-resize-stuck-pods) and [Bin packing](https://attune-io.github.io/attune/guides/bin-packing/).

### Docs and diagnostics

- Recommendation **explainability** guide and skip-reason tables for `kubectl attune explain` and status explanations ([#437](https://github.com/attune-io/attune/pull/437)).
- Standalone **SLO guardrails** guide (PromQL post-resize checks) linked from quickstart and nav ([#470](https://github.com/attune-io/attune/pull/470)).
- GA-era messaging refresh across docs and ADOPTERS ([#435](https://github.com/attune-io/attune/pull/435)).
- Pre-release **E2E Nightly** matrix guidance for maintainers (1.32–1.35) ([#473](https://github.com/attune-io/attune/pull/473)).

### Hardening and tests

- GitOps SSRF checks for pull request `apiUrl`, incomplete-config paths, and related unit and Chainsaw coverage ([#455](https://github.com/attune-io/attune/pull/455), [#464](https://github.com/attune-io/attune/pull/464)).
- Cluster E2E for version-aware memory limit decrease, Deferred/Infeasible status injection, MemoryPressure skip with events, and java profile overhead in explanations ([#471](https://github.com/attune-io/attune/pull/471), [#472](https://github.com/attune-io/attune/pull/472)).
- Full K8s 1.32–1.35 E2E Nightly matrix and fuzz coverage exercised on the release candidate tip before this tag.

### Upgrade notes

- Most new capabilities are **opt-in** (GitOps PR automation, fleet report, runtime profiles). Existing policies keep working without YAML edits.
- On **Kubernetes 1.35+**, memory **limits** can decrease in place when the policy allows decrease and uses requests-and-limits control. If you rely on limits never going down, use `memory.allowDecrease: false`, a restrictive runtime profile, `controlledValues: RequestsOnly` (default), or stay on 1.33–1.34.
- Refresh CRDs with the release install path (`helm upgrade` or `dist/crds.yaml` / `dist/install.yaml` from the tag).
- Re-apply Grafana dashboard and PrometheusRule assets if you manage them out of band; new panels and alerts cover GitOps PR, fleet export, memory limit decrease safety, capacity skips, and Deferred/Infeasible.
- Upgrade `kubectl attune` with the release so `explain` shows GitOps PR and runtime profile effective fields.

```bash
kubectl attune status -A
kubectl attune explain -n <namespace> <policy>
```

Full upgrade detail: [Upgrading](https://attune-io.github.io/attune/guides/upgrading/).

### Full changelog

https://github.com/attune-io/attune/compare/v0.1.20...v0.1.21
