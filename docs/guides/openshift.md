# OpenShift

Attune runs on OpenShift 4.x clusters with Kubernetes 1.32+. This guide
covers OpenShift-specific installation, TLS profile integration, and
security considerations.

## Installation

### Via OperatorHub (recommended for OpenShift)

OpenShift includes a built-in OperatorHub catalog. Search for "Attune"
in the web console under **Operators > OperatorHub** and click Install.
The OLM bundle includes all CRDs, RBAC, and the operator deployment.

You can also browse the listing online:

- [Red Hat Ecosystem Catalog](https://catalog.redhat.com/software/search?target_platforms=Operator&q=attune) (OpenShift embedded catalog)
- [OperatorHub.io](https://operatorhub.io/operator/attune) (community catalog for any OLM-enabled cluster)

### Via Helm

```bash
kubectl create namespace attune-system

helm install attune \
  oci://ghcr.io/attune-io/charts/attune \
  --namespace attune-system \
  --set openshift.enabled=true
```

The `openshift.enabled=true` flag adds OpenShift-specific RBAC (read
access to `config.openshift.io/apiservers`) and enables TLS profile
auto-detection. Without it, the operator runs with vanilla Kubernetes
RBAC only.

## TLS profile auto-detection

OpenShift clusters enforce a cluster-wide TLS security profile via the
`APIServer` resource at `config.openshift.io/v1`. This profile controls
the minimum TLS version and cipher suites for API server connections.

When `openshift.enabled=true`, Attune reads this profile at startup and
configures its outbound Prometheus connections to match. This prevents
TLS handshake failures when the cluster enforces a stricter profile than
Go's defaults.

| OpenShift TLS Profile | Minimum TLS Version | Go equivalent |
|----------------------|---------------------|---------------|
| Modern | TLS 1.3 | `tls.VersionTLS13` |
| Intermediate (default) | TLS 1.2 | `tls.VersionTLS12` |
| Old | TLS 1.0 | `tls.VersionTLS10` |
| Custom | Parsed from `custom.minTLSVersion` | e.g. `VersionTLS13` -> `tls.VersionTLS13` |

### How it works

1. On startup, the operator checks if the `config.openshift.io/v1` API
   group exists via API discovery
2. If found, it reads the `APIServer/cluster` resource to extract the
   TLS security profile type
3. The detected minimum TLS version is applied to all outbound
   Prometheus HTTP connections
4. If the API is not found (vanilla Kubernetes) or the read fails, the
   operator falls back to Go defaults (TLS 1.2)

### Verifying TLS profile detection

Check the operator logs at startup:

```bash
kubectl logs -n attune-system deploy/attune -c manager | grep -i tls
```

On OpenShift with an Intermediate profile:

```json
{"level":"info","msg":"Detected OpenShift TLS profile","profile":"Intermediate","tlsMinVersion":"0x0303"}
```

On vanilla Kubernetes, no TLS-related messages appear at the default log
level (the fallback to Go defaults is silent). Enable debug logging to
confirm:

```json
{"level":"debug","msg":"OpenShift config API not found, using Go TLS defaults"}
```

## Security Context Constraints

The Attune Helm chart sets a restrictive security context by default:

```yaml
podSecurityContext:
  runAsNonRoot: true
  seccompProfile:
    type: RuntimeDefault

securityContext:
  allowPrivilegeEscalation: false
  capabilities:
    drop: [ALL]
  readOnlyRootFilesystem: true
  runAsNonRoot: true
  runAsUser: 65532
  runAsGroup: 65532
```

These settings are compatible with the OpenShift `restricted-v2` SCC
(the default for non-privileged workloads). No custom SCC is required.

## RBAC differences

When `openshift.enabled=false` (default), the ClusterRole contains only
standard Kubernetes API permissions. When `openshift.enabled=true`, one
additional rule is added:

```yaml
- apiGroups:
    - config.openshift.io
  resources:
    - apiservers
  verbs:
    - get
    - list
```

This is read-only access to the cluster TLS configuration. The operator
never writes to OpenShift API resources.

!!! note "OLM installations"
    When installing via OperatorHub/OLM, the ClusterServiceVersion (CSV)
    bundle always includes the OpenShift RBAC rule. OLM manages the RBAC
    lifecycle automatically.

## Combining with other features

OpenShift integration works alongside all other Attune features:

- **FIPS 140-3 mode** (`fips.enabled=true`): Common on OpenShift
  clusters in regulated environments. The TLS profile detection respects
  the FIPS-approved cipher suites.
- **Prometheus Operator**: OpenShift clusters typically include the
  Prometheus Operator. Enable `metrics.serviceMonitor.enabled=true` for
  automatic scrape target registration.
- **NetworkPolicy**: The default NetworkPolicy allows egress to
  Prometheus and the Kubernetes API. No OpenShift-specific changes are
  needed.
