# Datadog Setup

Attune can use Datadog as its metrics source instead of Prometheus.
This guide covers prerequisites, authentication, policy configuration,
and verification.

## Prerequisites

- **Datadog Agent** running in your cluster with the Kubernetes integration
  enabled. The Agent must be collecting `kubernetes.cpu.usage.total` and
  `kubernetes.memory.working_set` metrics.
- **Datadog API key** (required) and optionally an **Application key**
  (recommended for higher rate limits).
- **Attune installed** (see [Installation](../getting-started/installation.md)).

## Required Datadog metrics

The operator queries these Container-level metrics from the Datadog
`/api/v1/query` endpoint:

| Metric | What it measures |
|--------|-----------------|
| `kubernetes.cpu.usage.total` | CPU usage in nanocores per container (converted to cores internally) |
| `kubernetes.memory.working_set` | Memory actively used per container in bytes |

These metrics are collected automatically by the Datadog Agent's
Kubernetes integration when it has access to the kubelet. No custom
metric configuration or DogStatsD setup is needed.

!!! note
    Results are grouped by the `kube_container_name` tag. If your Datadog
    Agent relabels or drops this tag, the operator cannot distinguish
    between containers in multi-container pods.

## Step 1: Create the API key Secret

Create a Kubernetes Secret containing your Datadog API key. The Secret
must live in the same namespace as the AttunePolicy that references it.

```bash
kubectl create secret generic datadog-keys \
  --from-literal=api-key=<YOUR_DATADOG_API_KEY> \
  --from-literal=app-key=<YOUR_DATADOG_APP_KEY> \
  -n production
```

The `app-key` key is optional but recommended. The Datadog API enforces
lower rate limits for API-key-only authentication.

| Secret key | Required | Purpose |
|------------|----------|---------|
| `api-key` | Yes | Authenticates against the Datadog API (`DD-API-KEY` header) |
| `app-key` | No | Authenticates for higher rate limits (`DD-APPLICATION-KEY` header) |

!!! warning "Secret namespace"
    The Secret must be in the **same namespace** as the AttunePolicy.
    Cross-namespace Secret references are not supported.

## Step 2: Create an AttunePolicy

```yaml
apiVersion: attune.io/v1alpha1
kind: AttunePolicy
metadata:
  name: my-app
  namespace: production
spec:
  targetRef:
    kind: Deployment
    name: my-app
  metricsSource:
    datadog:
      site: datadoghq.com
      apiKeySecretRef:
        name: datadog-keys
        key: api-key
  cpu: {}
  memory: {}
```

### Configuration fields

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `site` | string | `datadoghq.com` | Datadog site domain. Use `datadoghq.eu` for EU, `us5.datadoghq.com` for US5, etc. |
| `apiKeySecretRef.name` | string | (required) | Name of the Secret containing the API key |
| `apiKeySecretRef.key` | string | (required) | Key within the Secret that holds the API key value |

??? note "Datadog sites"
    Datadog operates in multiple regions. Set `site` to match your account:

    | Region | Site value |
    |--------|-----------|
    | US1 (default) | `datadoghq.com` |
    | US3 | `us3.datadoghq.com` |
    | US5 | `us5.datadoghq.com` |
    | EU1 | `datadoghq.eu` |
    | AP1 | `ap1.datadoghq.com` |
    | US1-FED | `ddog-gov.com` |

## Step 3: Verify the integration

### Check policy conditions

```bash
kubectl get attunepolicy my-app -n production -o wide
```

| Condition | Meaning |
|-----------|---------|
| `Ready: True, Reason: Monitoring` | Datadog reachable, recommendations computed |
| `Ready: False, Reason: InsufficientData` | Datadog reachable but not enough history yet |

If the condition message mentions a Datadog API error, check the
operator logs:

```bash
kubectl logs -n attune-system deployment/attune-controller-manager \
  --tail=50 | grep -i datadog
```

### Common errors

| Error | Cause | Fix |
|-------|-------|-----|
| `cannot read Datadog API key` | Secret not found or missing key | Verify the Secret exists in the policy namespace with the correct key name |
| `datadog API returned 403` | Invalid API key or insufficient permissions | Regenerate the API key in the Datadog console |
| `datadog API returned 429` | Rate limited | Add an `app-key` to the Secret for higher limits; the operator rate-limits to ~0.08 QPS per collector |
| `empty result from Datadog instant query` | No metrics for the workload | Verify the Datadog Agent is collecting Kubernetes metrics for the target namespace and pods |

## Rate limiting

The operator rate-limits Datadog API calls to approximately **0.08 QPS**
(~300 requests/hour) with a burst of 3, well within Datadog's default
rate limit of 300 requests/hour for API-key-only authentication and
3600 requests/hour with an Application key.

If you have many policies using Datadog, consider:

- Adding an Application key to increase the rate limit
- Using cluster-wide or namespace defaults to share the collector across
  policies (the operator caches collectors by site + API key)

## Using Datadog as the cluster default

Instead of configuring Datadog on every policy, set it in
`AttuneDefaults` or `AttuneNamespaceDefaults`:

=== "Cluster-wide"

    ```yaml
    apiVersion: attune.io/v1alpha1
    kind: AttuneDefaults
    metadata:
      name: cluster-defaults
    spec:
      metricsSource:
        datadog:
          site: datadoghq.com
          apiKeySecretRef:
            name: datadog-keys
            key: api-key
    ```

    !!! warning
        The Secret referenced in `AttuneDefaults` must exist in **every
        namespace** that has an AttunePolicy, since Secret access is
        namespace-scoped. Consider using a namespace defaults object
        per namespace instead.

=== "Per-namespace"

    ```yaml
    apiVersion: attune.io/v1alpha1
    kind: AttuneNamespaceDefaults
    metadata:
      name: team-defaults
      namespace: production
    spec:
      metricsSource:
        datadog:
          site: datadoghq.com
          apiKeySecretRef:
            name: datadog-keys
            key: api-key
    ```

With defaults configured, policies only need `targetRef`:

```yaml
apiVersion: attune.io/v1alpha1
kind: AttunePolicy
metadata:
  name: my-app
  namespace: production
spec:
  targetRef:
    kind: Deployment
    name: my-app
  cpu: {}
  memory: {}
```

## Limitations

- **No auto-discovery.** Unlike Prometheus, Datadog has no in-cluster
  service to discover automatically. The `apiKeySecretRef` is always
  required.
- **One metrics source per policy.** A policy cannot combine Datadog and
  Prometheus data. Set `metricsSource.datadog` or
  `metricsSource.prometheus`, not both.
- **Safety monitor throttle detection** uses the same metrics source as
  recommendations. Datadog does not expose CFS throttle metrics
  (`container_cpu_cfs_throttled_periods_total`), so CPU throttle-based
  auto-revert is not available when using Datadog. OOMKill detection,
  restart monitoring, and pod readiness checks still work.
