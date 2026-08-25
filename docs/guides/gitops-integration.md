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

## Recommended workflow (GitOps-durable recommendations)

Default in-place resize does **not** update Git or workload templates. After a
deploy, new pods start with Git resources until Attune re-collects metrics and
resizes again. Use this flow so recommended sizes **survive the next deploy**.

### 1. Start in Recommend mode with export

Deploy `AttunePolicy` resources in your GitOps repo with `type: Recommend`
and ConfigMap export enabled.

```yaml
spec:
  updateStrategy:
    type: Recommend
    export:
      configMap: true
```

### 2. Review recommendations

```bash
kubectl attune recommendations -n production
kubectl attune explain -n production api-services
kubectl attune export list -n production
```

### 3. Emit a workload template patch and open a PR

```bash
# Strategic-merge-style YAML for Deployment/StatefulSet/CronJob templates
kubectl attune diff -n production -o yaml > /tmp/attune-resource-patches.yaml
```

Review the patch, apply it to your Git repo (or feed it to kustomize/helm
values), and let Argo CD / Flux sync. New pods then start near recommended
sizes without waiting for a full re-observation cycle.

Alternatively, read ConfigMaps named
`<policy>-<workload>-recommendations` (schema key `schema-version: v1`).

### 4. Optional continuous in-place between deploys

Once you trust recommendations, switch to `Auto` (or Canary first). In-place
resizes keep running pods healthy between deploys; keep committing template
updates periodically so rollouts stay near target.

**Do not** enable `templatePersistence` under unmanaged GitOps sync unless you
intentionally want the cluster template to diverge from Git (see configuration
reference).

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
| Pure GitOps (no direct resizes) | Recommend + export.configMap; CI pipeline consumes the ConfigMaps and proposes Git patches (see below). **Do not** enable `templatePersistence` under unmanaged sync; the operator would patch live templates and Git would thrash them. |
| Cluster is source of truth | Optional `updateStrategy.templatePersistence` (Deployment/StatefulSet) so new pods inherit recommended sizes after resize or on recommendation; see configuration reference |

## Export mode for GitOps pipelines

For environments that want the operator to compute recommendations but require **all** resource changes to flow through Git (ArgoCD, Flux, etc.):

1. Set `updateStrategy.type: Recommend` (or `Auto` with `export` also enabled) plus `export.configMap: true`.
2. The operator creates one ConfigMap per workload (named `<policy>-<workload>-recommendations`) containing per-container CPU/memory recommendations, confidence, and a RFC3339 `last-updated` timestamp. The ConfigMap carries the `attune.io/policy` label.
3. Your CI/CD pipeline (or a lightweight sidecar) reads the ConfigMaps and proposes patches to the Deployment/StatefulSet specs stored in Git.
4. GitOps applies the patches through the normal sync/approval flow.

See the [Auto mode guide](auto-mode.md#exporting-recommendations-to-configmaps) for the exact ConfigMap schema, example output, and owner-reference cleanup behavior.

**Orphan cleanup (stale recommendation removal)**: When a workload leaves the policy selector (selector change, scale-to-zero, or deletion while the policy still exists), the operator automatically deletes the corresponding recommendation ConfigMap on the next reconcile. Only ConfigMaps bearing the matching `attune.io/policy` label are considered. This guarantees GitOps consumers never see stale recommendations for workloads no longer in scope.

**Consuming the ConfigMap from CI (example)**:

```bash
# In your pipeline, extract the recommendation for a container
CPU_REQ=$(kubectl get cm my-app-my-deployment-recommendations -n prod \
  -o jsonpath='{.data.main\.cpu-request}')
# Then propose a patch to your Deployment in Git (or use kustomize/helm values update)
```

**CLI support:** `kubectl attune export list` (or just `kubectl attune export`) shows all exported recommendation ConfigMaps with last-updated timestamps, workload/kind, and container counts. `kubectl attune status` includes an `EXPORT` column (`CM` vs `-`). `kubectl attune explain` surfaces the `export` effective value and prints a GitOps-mode note when Recommend+export (or Observe+export) is active. `kubectl attune recommendations` also prints an export footer note. This makes the recommended pure-GitOps workflow fully first-class in the plugin.

See the full schema and more examples in the [Auto mode guide](auto-mode.md#exporting-recommendations-to-configmaps).

This is the primary integration pattern for strict GitOps shops: the operator provides the intelligence (usage-based recommendations), Git remains the source of truth, and the export + orphan cleanup mechanism keeps the hand-off clean and auditable.

## Pull request automation (opt-in Phase B)

Default **off**. When enabled, Attune compares recommendations to **workload
pod templates** (Deployment / StatefulSet / DaemonSet). If any container
request drifts by at least `minChangePercent` (default 10), the operator
opens or updates a GitHub or GitLab pull request (subject to `cooldown`,
default 24h).

### Security

- Token is read from a Kubernetes Secret via `tokenSecretRef`.
- Tokens are **never** written to logs, events, or status.
- Use a fine-scoped PAT with write access to create branches and PRs:
  - **GitHub** (fine-grained): repository **Contents: Read and write** and
    **Pull requests: Read and write** on the target repo (classic: `repo` on
    private repos, or `public_repo` for public-only).
  - **GitLab**: `api` (or project-scoped token with write_repository +
    write to merge requests) on one project.
  Prefer short-lived or fine-grained tokens.
- Optional `apiUrl` (Enterprise GitHub / self-hosted GitLab) is validated
  like Prometheus addresses: `http`/`https` only, and cloud metadata plus
  loopback hosts are rejected so a policy cannot aim the operator token at
  `169.254.169.254` or similar. Private ClusterIP-style hosts remain allowed
  for on-prem Git forges.

### Example

```yaml
spec:
  updateStrategy:
    type: Recommend
    export:
      configMap: true
      pullRequest:
        enabled: true
        provider: github          # or gitlab
        repository: org/app-manifests
        baseBranch: main
        tokenSecretRef:
          name: attune-gitops
          key: token
        minChangePercent: 10
        cooldown: 24h
        dryRun: true              # log + status only; no API call
        labels:
          - attune
          - rightsizing
```

### Status

Condition type `GitOpsPullRequest`:

| Reason | Meaning |
|--------|---------|
| `PullRequestDisabled` | Feature off |
| `NoDrift` | Templates already near recommendations |
| `PullRequestUnchanged` | Drift table matches the last PR; no new empty PR |
| `PullRequestCooldown` | Waiting for cooldown |
| `PullRequestDryRun` | Would open/update PR |
| `PullRequestOpen` | PR URL in message |
| `PullRequestFailed` | API or config error (message is safe) |

Metric: `attune_gitops_pr_total{result=created|updated|dry_run|failed}`.

### Dry-run path

Set `dryRun: true` for first enablement or CI. The operator records
`PullRequestDryRun` and increments `dry_run` without calling GitHub/GitLab.

### First enablement

Use this ordered path when turning on PR automation for the first time.

1. **Export recommendations first** (optional but recommended): set
   `export.configMap: true` and confirm
   `kubectl attune export list` shows ConfigMaps. See
   [Export mode](#export-mode-for-gitops-pipelines) above.
2. **Create a fine-grained token** with the scopes in
   [Security](#security) (GitHub: Contents + Pull requests write on one
   repo; GitLab: project write for repository and merge requests).
3. **Store the token** in the policy namespace (never in the CR):
   ```bash
   kubectl -n production create secret generic attune-gitops \
     --from-literal=token="ghp_..."
   ```
4. **Enable dry-run** on the policy (`pullRequest.enabled: true`,
   `dryRun: true`, `repository`, `tokenSecretRef`). Wait for condition
   `GitOpsPullRequest` with reason `PullRequestDryRun` (first pass)
   or `PullRequestUnchanged` (same table on the next reconcile).
   `NoDrift` means templates already match. Metric
   `attune_gitops_pr_total{result="dry_run"}` increases once when
   drift is first recorded.
5. **Inspect what a real PR would say**: the condition message includes
   the repository and drift count; the body template is the same as
   live PRs (see [Status](#status)).
6. **Turn off dry-run** (`dryRun: false` or omit). On the first live run
   with drift, Attune **bootstraps the head branch** if missing, then
   opens the PR. A prior dry-run of the same table records the fingerprint
   only; it does not set a PR URL, so it does not block that first live PR:
   - **GitHub:** empty bootstrap commit on
     `attune/recommendations-<ns>-<policy>` (same tree as `baseBranch`).
   - **GitLab:** branch from `baseBranch` plus
     `.attune/RECOMMENDATION_DRIFT.md` (create, or update if that file
     already exists on base after a prior merge).
7. **Apply template patches via Git**, not by hoping the empty/bootstrap
   commit is enough:
   ```bash
   kubectl attune diff -n production -o yaml > /tmp/attune-patches.yaml
   # Review, commit to the PR branch (or a new branch), merge via normal GitOps.
   ```
8. **Verify**: condition reason `PullRequestOpen` and message containing
   the PR URL; annotation `attune.io/gitops-pr-url`; metric
   `result="created"` or `updated`. Failures set `PullRequestFailed`
   without logging the token.

Cooldown (default 24h) prevents PR thrash. After a merge, if the head branch
is deleted, the next cycle may bootstrap again **only when the drift table
changed**. If template vs recommendation is the same set as the last PR,
the condition is `PullRequestUnchanged` and Attune does not open another
empty PR. A prior successful PR (`attune.io/gitops-pr-url`) with no stored
fingerprint (upgrades from 0.1.22/0.1.23) is treated the same way: Attune
records the live table and skips instead of opening one more empty PR.
The same fingerprint is stored on `status.gitopsPR` so a Flux or Argo
apply that replaces `metadata.annotations` does not start the empty-PR
loop again.
Apply real template patches (`kubectl attune diff`) so drift
clears (`NoDrift`).

### Branch bootstrap

The operator updates an existing open PR whose head branch is
`attune/recommendations-<ns>-<policy>`. When no open PR exists and that head
branch is **missing** on the remote, Attune creates it automatically:

- **GitHub:** empty bootstrap commit on the new branch (same tree as
  `baseBranch`, so the PR has a single commit delta).
- **GitLab:** branch created from `baseBranch` with a small marker file at
  `.attune/RECOMMENDATION_DRIFT.md` (GitLab rejects MRs with no file delta).

The PR/MR description still carries the full drift table. Template patches
remain a review step (`kubectl attune diff` or your pipeline). Dry-run never
creates branches or PRs.

Re-bootstrap after a merge is expected when the head branch was deleted.
On GitLab, if `.attune/RECOMMENDATION_DRIFT.md` already exists on
`baseBranch`, Attune retries with an update action so the delta stays
non-empty.

If bootstrap fails (token scopes, missing `baseBranch`, network), status shows
`PullRequestFailed` with a redacted API error.

