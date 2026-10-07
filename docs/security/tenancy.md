# Tenancy

Attune runs once per cluster and resizes pods. The people who create
`AttunePolicy` objects are not the same people who install the operator.
This page says what each of them can make the operator do.

## Trust levels

Cluster level is the operator install and cluster `AttuneDefaults`.

- Helm values and operator flags choose credentials: the Prometheus
  ServiceAccount token, the operator bearer Secret, the Datadog API key
  Secret, and the SigV4 allowlists.
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
