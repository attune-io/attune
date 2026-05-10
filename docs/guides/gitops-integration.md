kube-rightsize performs in-place pod resizes via the `/resize` subresource,
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
  recommended values live only in the `RightSizePolicy` status.

## Recommended workflow

### 1. Start in Recommend mode

Deploy `RightSizePolicy` resources in your GitOps repo with `mode: Recommend`.
The operator computes recommendations without modifying anything.

```yaml
spec:
  updateStrategy:
    mode: Recommend
```

### 2. Review recommendations

```bash
kubectl rightsize recommendations -n production
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
- **Health checks**: The `RightSizePolicy` CRD has a `Ready` condition.
  ArgoCD can use this for health status if you add a custom health check.
- **Sync waves**: Deploy the operator (Helm chart) in an early sync wave,
  and `RightSizePolicy` resources in a later wave.

## Flux-specific notes

- **Kustomization ordering**: Use `dependsOn` to ensure the operator is
  healthy before `RightSizePolicy` resources are applied.
- **Health checks**: Flux can check the `Ready` condition on
  `RightSizePolicy` resources via `healthChecks` in the Kustomization.

## When to update Git vs let the operator handle it

| Scenario | Action |
|----------|--------|
| Initial right-sizing of a new service | Let operator recommend, then commit to Git |
| Ongoing optimization of stable services | Use Auto mode; commit periodically based on savings reports |
| Pre-deployment sizing | Use Recommend mode, review, commit before promoting |
| Cost reporting | Use `kubectl rightsize savings` or the Grafana dashboard |