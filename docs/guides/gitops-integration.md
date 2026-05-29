# GitOps Integration

Attune performs in-place pod resizes via the `/resize` subresource,
which modifies the running pod without changing the Deployment, StatefulSet,
or DaemonSet spec. This has specific implications for GitOps workflows.

## How it works with GitOps

In-place resize changes the **pod** resources, not the **workload template**.
Your Git-stored Deployment spec remains unchanged. This means:

- **No drift**: ArgoCD/Flux won't detect a diff because the Deployment spec
  hasn't changed.
- **Rollouts reset resizes**: When a Deployment is updated (new image, env
  change), the new pods start with the original resources from the template.
  The operator will re-evaluate and resize again after collecting metrics.
- **No feedback loop**: The operator doesn't write back to Git. The
  recommended values live only in the `AttunePolicy` status.

## Recommended workflow

### 1. Start in Recommend mode

Deploy `AttunePolicy` resources in your GitOps repo with `type: Recommend`.
The operator computes recommendations without modifying anything.

```yaml
spec:
  updateStrategy:
    type: Recommend
```

### 2. Review recommendations

```bash
kubectl attune recommendations -n production
```

### 3. Apply recommendations to Git (manual)

Update the Deployment resource requests in your Git repository based on the
recommendations. This makes the optimization permanent and survives rollouts.

### 4. Use Auto mode for continuous optimization

Once you trust the operator's recommendations for a workload, switch to
`Auto` mode. The operator will continuously right-size pods between
deployments. After each deployment, it re-learns the usage pattern and
adjusts.

## ArgoCD-specific notes

- **Resource tracking**: ArgoCD tracks Deployments and StatefulSets but not
  individual pod specs. In-place resizes are invisible to ArgoCD.
- **Health checks**: The `AttunePolicy` CRD has a `Ready` condition.
  ArgoCD can use this for health status if you add a custom health check.
- **Sync waves**: Deploy the operator (Helm chart) in an early sync wave,
  and `AttunePolicy` resources in a later wave.

## Flux-specific notes

- **Kustomization ordering**: Use `dependsOn` to ensure the operator is
  healthy before `AttunePolicy` resources are applied.
- **Health checks**: Flux can check the `Ready` condition on
  `AttunePolicy` resources via `healthChecks` in the Kustomization.

## When to update Git vs let the operator handle it

| Scenario | Action |
|----------|--------|
| Initial right-sizing of a new service | Let operator recommend, then commit to Git |
| Ongoing optimization of stable services | Use Auto mode; commit periodically based on savings reports |
| Pre-deployment sizing | Use Recommend mode, review, commit before promoting |
| Cost reporting | Use `kubectl attune savings` or the Grafana dashboard |
| Pure GitOps (no direct resizes) | Recommend + export.configMap; CI pipeline consumes the ConfigMaps and proposes Git patches (see below) |

## Export mode for GitOps pipelines

For environments that want the operator to compute recommendations but require **all** resource changes to flow through Git (ArgoCD, Flux, etc.):

1. Set `updateStrategy.type: Recommend` (or `Auto` with `export` also enabled) plus `export.configMap: true`.
2. The operator creates one ConfigMap per workload (named `<policy>-<workload>-recommendations`) containing per-container CPU/memory recommendations, confidence, and a RFC3339 `last-updated` timestamp. The ConfigMap carries the `attune.io/policy` label.
3. Your CI/CD pipeline (or a lightweight sidecar) reads the ConfigMaps and proposes patches to the Deployment/StatefulSet specs stored in Git.
4. GitOps applies the patches through the normal sync/approval flow.

See the [Auto mode guide](auto-mode.md#exporting-recommendations-to-configmaps) for the exact ConfigMap schema, example output, and owner-reference cleanup behavior.

**Orphan cleanup (stale recommendation removal)**: When a workload leaves the policy selector (selector change, scale-to-zero, or deletion while the policy still exists), the operator automatically deletes the corresponding recommendation ConfigMap on the next reconcile. Only ConfigMaps bearing the matching `attune.io/policy` label are considered. This guarantees GitOps consumers never see stale recommendations for workloads no longer in scope.

This is the primary integration pattern for strict GitOps shops: the operator provides the intelligence (usage-based recommendations), Git remains the source of truth, and the export + orphan cleanup mechanism keeps the hand-off clean and auditable.