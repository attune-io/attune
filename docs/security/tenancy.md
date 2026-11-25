# Tenancy

Attune runs once per cluster and resizes pods. The people who create
`AttunePolicy` objects are not the same people who install the operator.
This page says what each of them can make the operator do.

## Trust levels

Cluster level is the operator install and cluster `AttuneDefaults`.

- Helm values and operator flags choose credentials: the Prometheus
  ServiceAccount token, the operator bearer Secret, the Datadog API key
  Secret, the weekly report webhook Secret, and the SigV4 allowlists.
- The weekly report URL is a key in a Secret in the operator namespace.
  It is not a field on `AttunePolicy` or namespace defaults. A namespace
  author cannot set it.
- Cluster `AttuneDefaults` chooses the metrics address, a SigV4 role, a
  CloudWatch role, a VPA in any namespace, and SLO guardrails.
- Those guardrails keep the operator credentials and are not rewritten.

Namespace level is `AttuneNamespaceDefaults` and `AttunePolicy` in that
namespace.

- A policy resizes workloads in its own namespace.
- A Secret reference is a name in that namespace. The name cannot contain
  `/`. When the validating webhook is on, the admission user must be
  allowed to `get` that Secret.
- A Prometheus address, Datadog block, or CloudWatch block set on the
  policy or on `AttuneNamespaceDefaults` does not receive operator
  Prometheus or Datadog credentials.
- `metricsSource.vpa.namespace` must be empty or that same namespace.
  A VPA namespace inherited only from cluster `AttuneDefaults` is still read.
- `sigv4.roleArn` and `cloudwatch.roleArn` must match
  `--sigv4-allowed-role-arns`. `sigv4` with no `roleArn` signs with the
  operator identity and the workspace host must match
  `--sigv4-allowed-workspace-hosts`. Both lists are empty until you set
  them, so a namespace object cannot use them until then. Cluster
  `AttuneDefaults` is not filtered.
- When a namespaced policy assumes a role, STS receives
  `ExternalId` `attune:<namespace>`.
- SLO guardrails written on the policy or on `AttuneNamespaceDefaults`
  are tenant guardrails. When they would run with operator Prometheus
  credentials, Attune adds `namespace="<policy namespace>"` to every
  vector selector. A selector that already requires a different
  namespace is not sent, and it does not extend safety observation
  or canary promotion. Set `--slo-guardrail-enforce-namespace=false`
  to skip those guardrails instead. The breach event does not include
  the numeric value.

Datadog and CloudWatch do not evaluate SLO guardrails. They also do not
extend canary promotion or safety observation for a guardrail window.

## Webhooks off

`--enable-webhooks=false` removes admission. That includes the Secret
SubjectAccessReview and the rejection of a cross-namespace VPA, a
non-allowlisted role, and a non-allowlisted SigV4 host.

Reconcile still refuses those three. A tenant guardrail that would use
operator Prometheus credentials is still scoped, or skipped when
namespace enforcement is off.

## Who should get which CRD

| CRD | Who should create it |
|-----|----------------------|
| `AttuneDefaults` | Cluster admins |
| `AttuneNamespaceDefaults` | Admins of that namespace |
| `AttunePolicy` | The team that owns the workloads in that namespace |

The chart ships `<fullname>-policy-viewer` and `<fullname>-policy-editor`
ClusterRoles. `helm install attune` names them `attune-policy-viewer`
and `attune-policy-editor`. A release name that does not contain
`attune` uses `<release>-attune-policy-editor`. They do not aggregate
into `view`, `edit`, or `admin` unless `rbac.aggregateToView` or
`rbac.aggregateToEdit` is true. Bind the editor role to the teams that
should create policies. Do not bind `AttuneDefaults` the same way.

Kustomize `config/default` sets `namePrefix: attune-`. The role
manifests are named `policy-viewer` and `policy-editor`, so the install
creates `attune-policy-viewer` and `attune-policy-editor`. Bind those
names. The roles have no aggregate labels, so a default install does
not grant every `edit` user `AttunePolicy` create.

## Operator ServiceAccount

The controller ServiceAccount is a separate trust level from the people
who create policies. If the manager process is compromised, this is what
that account can do. The Helm test
`charts/attune/tests/rbac_test.yaml` pins the full rule list. The writes
below are the ones that matter. They are not the only rules.

These stay on a default install. A field on `AttunePolicy` or
`AttuneDefaults` turns the behavior on after the chart is installed, so
the chart does not hide the rule behind a Helm value:

- `pods/resize` update and patch. In-place resize calls `UpdateResize`.
- `pods/eviction` create. Used when `resizeMethod` is `InPlaceOrRecreate`.
  The chart default `InPlaceOnly` does not make the rule optional.
- `pods` get, list, patch, update, and watch. Deletion cleanup
  merge-patches tracking annotations. This page does not remove `update`.
- Deployments and StatefulSets patch and update, and Argo Rollouts get,
  list, patch, and update. Template persistence uses the same policy
  field for all three. Rollouts are not watched.
- HorizontalPodAutoscalers get, list, patch, update, and watch. Retune
  after a resize calls `Update`. It is not a Helm value.
- ConfigMaps create, delete, get, list, patch, update, and watch.
  Recommendation export is off until `export.configMap` is set on a
  policy or on cluster `AttuneDefaults`. The fleet-report writer also
  patches a ConfigMap. The `update` verb stays even though the exporter
  uses Create, Get, Patch, and Delete.

Secret `get` has no `resourceNames`. The operator can read any Secret
by name in every namespace. There is no list or watch. The validating
webhook's SubjectAccessReview checks the policy author. It does not
replace this get. Prometheus bearer tokens and the weekly report Secret
use it.

What is already narrower:

- No wildcard verb or resource on the ClusterRole.
- Leader election is a namespaced Role in the release namespace, only
  when `leaderElection.enabled` is true. Lease get, update, and delete
  are limited to the name `attune.attune.io`. Lease create is not
  name-limited. That Role also creates and patches core Events in the
  release namespace.
- The query-account TokenRequest is a namespaced Role, only when
  `prometheusAuth.queryServiceAccount.create` is true. It can create
  tokens for that one ServiceAccount. The account does not mount a
  token into pods.
- `config.openshift.io/apiservers` get is added only when
  `openshift.enabled` is true.
- Metrics scrapes are a different ClusterRole,
  `<release>-metrics-reader`, with `get` on non-resource URL `/metrics`.
  Helm renders it only when the Prometheus ServiceAccount name and
  namespace are both set. It is not on the operator role, and it does
  not aggregate into `view`, `edit`, or `admin`.

`rbac.aggregateToEdit` and `rbac.aggregateToView` default to false.
That is the viewer and editor section above, not a new rule.
