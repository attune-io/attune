# Prometheus authentication

Attune queries Prometheus from the operator process. These files are the
four cases in the
[Prometheus setup guide](../../docs/guides/prometheus-setup.md#choose-how-attune-authenticates).

| Case | What you want | File |
|------|----------------|------|
| 1. No auth | In-cluster Prometheus, no token | [../05-cluster-defaults.yaml](../05-cluster-defaults.yaml) |
| 2. Per namespace | One Secret in that namespace, on the namespace defaults | [02-per-namespace.yaml](02-per-namespace.yaml) |
| 3. Cluster-wide | One operator identity for every policy that inherits the cluster address | [03-cluster-wide.yaml](03-cluster-wide.yaml) |
| 4. Cluster-wide, with an exception | Case 3, plus one namespace that brings its own address and Secret | [04-namespace-override.yaml](04-namespace-override.yaml) |

Apply one case. Do not apply 02, 03, and 04 together: they are different
Prometheus setups, not layers on one cluster.

## 2. Per namespace

```bash
kubectl -n production create secret generic prom-token \
  --from-file=token=./prom-token
kubectl apply -f examples/prometheus-auth/02-per-namespace.yaml
```

The Secret stays in `production`. Policies in that namespace omit
`metricsSource`. The operator does not send its own token.

## 3. Cluster-wide

`03-cluster-wide.yaml` is only the address. The credential is the
operator, not a field on `AttuneDefaults`.

Helm on OpenShift:

```yaml
openshift:
  bindClusterMonitoringView: true
prometheusAuth:
  queryServiceAccount:
    create: true
```

Helm when you bind the query ServiceAccount yourself:

```yaml
prometheusAuth:
  useServiceAccountToken: true
  queryServiceAccount:
    create: true
```

A long-lived token in the operator namespace (Mimir, Grafana Cloud):

```yaml
prometheusAuth:
  existingSecret:
    name: mimir-token
    key: token
```

OperatorHub and the kustomize install already pass
`--prometheus-use-service-account-token` and
`--prometheus-query-service-account=attune-prometheus-query`. Bind
OpenShift `cluster-monitoring-view` once:

```bash
oc adm policy add-cluster-role-to-user cluster-monitoring-view \
  -z attune-prometheus-query -n attune-system
kubectl apply -f examples/prometheus-auth/03-cluster-wide.yaml
```

A default `helm install` does not send this token. Set the values above,
then apply the address.

## 4. Namespace exception

Apply `04-namespace-override.yaml` on top of case 3. Namespaces that
still inherit the cluster address keep the operator token. `payments`
uses `payments/prom-token` because that namespace defaults object sets
its own address.

```bash
kubectl -n payments create secret generic prom-token \
  --from-file=token=./prom-token
kubectl apply -f examples/prometheus-auth/04-namespace-override.yaml
```

Do not repeat `prometheus.address` on `AttuneNamespaceDefaults` for a
namespace that should keep the operator token. Setting only
`bearerTokenSecret`, with no address, does not attach that Secret to
the cluster address.
