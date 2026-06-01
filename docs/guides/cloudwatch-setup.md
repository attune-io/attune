# CloudWatch Setup

Attune can use Amazon CloudWatch Container Insights as its metrics source
instead of Prometheus. This guide covers prerequisites, IAM configuration,
policy setup, and verification for EKS clusters.

## Prerequisites

- **Amazon EKS cluster** with
  [Container Insights](https://docs.aws.amazon.com/AmazonCloudWatch/latest/monitoring/Container-Insights-setup-EKS-quickstart.html)
  enabled. Container Insights publishes container-level metrics to the
  `ContainerInsights` CloudWatch namespace.
- **IAM permissions** for the operator's pod to call
  `cloudwatch:GetMetricData` (see [IAM setup](#step-1-configure-iam-permissions) below).
- **Attune installed** (see [Installation](../getting-started/installation.md)).

## Required CloudWatch metrics

Container Insights publishes these metrics to the `ContainerInsights`
namespace with dimensions `ClusterName`, `Namespace`, `PodName`, and
`ContainerName`:

| Metric | What it measures |
|--------|-----------------|
| `container_cpu_usage_total` | CPU usage in nanocores per container (converted to cores internally) |
| `container_memory_working_set` | Memory actively used per container in bytes |

The operator uses CloudWatch
[SEARCH expressions](https://docs.aws.amazon.com/AmazonCloudWatch/latest/monitoring/using-search-expressions.html)
to find all matching containers within a namespace and pod prefix, then
groups results by `ContainerName`.

!!! note
    Container Insights must be enabled for both `ContainerName`-level and
    `PodName`-level dimensions. The default "enhanced observability" mode
    in EKS provides these. If you use the basic Container Insights mode,
    only cluster and node-level metrics are available.

## Step 1: Configure IAM permissions

The operator needs `cloudwatch:GetMetricData` permission. There are two
approaches:

=== "IRSA (IAM Roles for Service Accounts)"

    Create an IAM policy:

    ```json
    {
      "Version": "2012-10-17",
      "Statement": [
        {
          "Effect": "Allow",
          "Action": "cloudwatch:GetMetricData",
          "Resource": "*"
        }
      ]
    }
    ```

    Create an IAM role with the IRSA trust policy and attach the policy:

    ```bash
    # Create the IAM policy
    aws iam create-policy \
      --policy-name AttuneCWReadOnly \
      --policy-document file://attune-cw-policy.json

    # Create the IRSA role
    eksctl create iamserviceaccount \
      --cluster my-eks-cluster \
      --namespace attune-system \
      --name attune-controller-manager \
      --attach-policy-arn arn:aws:iam::<ACCOUNT_ID>:policy/AttuneCWReadOnly \
      --approve
    ```

    Or annotate the existing ServiceAccount via Helm:

    ```bash
    helm upgrade attune oci://ghcr.io/attune-io/charts/attune \
      --set serviceAccount.annotations."eks\.amazonaws\.com/role-arn"=arn:aws:iam::<ACCOUNT_ID>:role/AttuneCWRole
    ```

=== "EKS Pod Identity"

    If your cluster uses [EKS Pod Identity](https://docs.aws.amazon.com/eks/latest/userguide/pod-identities.html),
    create a Pod Identity association:

    ```bash
    aws eks create-pod-identity-association \
      --cluster-name my-eks-cluster \
      --namespace attune-system \
      --service-account attune-controller-manager \
      --role-arn arn:aws:iam::<ACCOUNT_ID>:role/AttuneCWRole
    ```

=== "Cross-account access"

    For centralized monitoring where CloudWatch data is in a different
    account, specify `roleArn` in the policy to assume a cross-account
    IAM role:

    ```yaml
    spec:
      metricsSource:
        cloudwatch:
          region: us-east-1
          clusterName: my-eks-cluster
          roleArn: arn:aws:iam::123456789012:role/CloudWatchReadOnly
    ```

    The operator uses STS `AssumeRole` to obtain temporary credentials.
    The target role must trust the operator's IAM role/identity.

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
    cloudwatch:
      region: us-east-1
      clusterName: my-eks-cluster
  cpu: {}
  memory: {}
```

### Configuration fields

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `region` | string | (required) | AWS region where CloudWatch metrics are stored (e.g., `us-east-1`) |
| `clusterName` | string | (required) | EKS cluster name, used as the `ClusterName` dimension filter |
| `roleArn` | string | (optional) | IAM role ARN to assume for cross-account access |

## Step 3: Verify the integration

### Check policy conditions

```bash
kubectl get attunepolicy my-app -n production -o wide
```

| Condition | Meaning |
|-----------|---------|
| `Ready: True, Reason: Monitoring` | CloudWatch reachable, recommendations computed |
| `Ready: False, Reason: InsufficientData` | CloudWatch reachable but not enough history yet |

Check the operator logs for CloudWatch-specific messages:

```bash
kubectl logs -n attune-system deployment/attune-controller-manager \
  --tail=50 | grep -i cloudwatch
```

### Verify Container Insights metrics exist

Confirm that Container Insights is publishing container-level metrics for
your workload:

```bash
aws cloudwatch list-metrics \
  --namespace ContainerInsights \
  --metric-name container_cpu_usage_total \
  --dimensions Name=ClusterName,Value=my-eks-cluster \
               Name=Namespace,Value=production \
  --region us-east-1
```

If this returns no results, Container Insights is not enabled or not
publishing container-level dimensions. See the
[Container Insights troubleshooting guide](https://docs.aws.amazon.com/AmazonCloudWatch/latest/monitoring/Container-Insights-troubleshooting.html).

### Common errors

| Error | Cause | Fix |
|-------|-------|-----|
| `loading AWS config` | No AWS credentials available | Configure IRSA, Pod Identity, or an instance profile for the operator pod |
| `CloudWatch GetMetricData failed: AccessDeniedException` | Missing IAM permission | Add `cloudwatch:GetMetricData` to the operator's IAM role |
| `empty result from CloudWatch instant query` | No metrics for the workload | Verify Container Insights is enabled and publishing container-level dimensions |
| `creating CloudWatch collector: operation error STS: AssumeRole` | Cross-account role assumption failed | Check the trust policy on the target role |

## Rate limiting

The operator rate-limits CloudWatch API calls to **5 QPS** with a burst
of 10, well within CloudWatch's default quota of 50 transactions per
second for `GetMetricData`.

Collectors are cached by `region + clusterName + roleARN`, so multiple
policies targeting the same cluster share a single API client.

## Using CloudWatch as the cluster default

Instead of configuring CloudWatch on every policy, set it in
`AttuneDefaults`:

```yaml
apiVersion: attune.io/v1alpha1
kind: AttuneDefaults
metadata:
  name: cluster-defaults
spec:
  metricsSource:
    cloudwatch:
      region: us-east-1
      clusterName: my-eks-cluster
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

Unlike Datadog (which requires a Secret per namespace), CloudWatch
authentication uses IAM roles attached to the pod's ServiceAccount.
A single `AttuneDefaults` resource works for all namespaces without
additional Secrets.

## Limitations

- **EKS only.** Container Insights metrics are only available on Amazon
  EKS clusters. Self-managed Kubernetes on EC2 can use Container Insights
  if the CloudWatch Agent is deployed manually, but EKS is the tested
  configuration.
- **No auto-discovery.** The `region` and `clusterName` fields are
  always required. The operator does not auto-detect the EKS cluster.
- **One metrics source per policy.** A policy cannot combine CloudWatch
  and Prometheus data. Set `metricsSource.cloudwatch` or
  `metricsSource.prometheus`, not both.
- **Safety monitor throttle detection** uses the same metrics source as
  recommendations. CloudWatch Container Insights does not expose CFS
  throttle metrics (`container_cpu_cfs_throttled_periods_total`), so
  CPU throttle-based auto-revert is not available when using CloudWatch.
  OOMKill detection, restart monitoring, and pod readiness checks still
  work.
- **Metric resolution.** CloudWatch Container Insights publishes metrics
  at 1-minute intervals (60-second period minimum). The operator clamps
  the query period to a minimum of 60 seconds.
