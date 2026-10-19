# Weekly report

Attune can post one plain-text rightsizing report per cluster per ISO week.
The report is off unless you enable it. It goes to Slack, Teams, or both.
It is not a cloud bill and it is not dollars saved this week.

The dollars are the gap between current requests and the current non-stale
recommendation, priced from `costPricing` or the built-in rates, times 730 hours.

## Turn it on

Create the Secret in the operator namespace. The chart does not create it
and does not store the URL.

```bash
kubectl -n attune-system create secret generic attune-weekly-report \
  --from-literal=slack-url='https://hooks.slack.com/services/T000/B000/XXXX'
```

A Teams Workflows URL is allowed only on
`*.environment.api.powerplatform.com` with `/powerautomate/` in the path.
Office 365 connector hosts are rejected. In the flow, copy the trigger
`text` field into the channel message. Attune does not build an Adaptive Card.

```yaml
weeklyReport:
  enabled: true
  endpoints:
    - name: slack
      secretName: attune-weekly-report
      secretKey: slack-url
```

At most two endpoints. Names are DNS labels and must be unique. Two endpoints
must not share one Secret name and key.

The manager flags are `--weekly-report-enabled` and repeatable
`--weekly-report-endpoint=name/secretName/secretKey`. `POD_NAMESPACE` must
be set when the report is enabled. There is no URL flag.

Set `fleetReport.clusterId` when more than one cluster posts to the same
channel. The chart passes `--fleet-report-cluster-id` when either report is
enabled. An empty id leaves the week header without a cluster name.

## What gets sent

The leader checks once at start, then every 15 minutes. The first eligible
moment is Monday 00:00 UTC. A missed week is one report on the next start,
not a backlog.

The stamp is ConfigMap `attune-weekly-report` in the operator namespace,
annotation `attune.io/last-report-week` (for example `2026-W43`). The chart
does not create that ConfigMap. Helm would wipe a chart-owned stamp on upgrade.

Delivery is at-least-once. A crash after a chat service returns success and
before the stamp is written can send that week again.

Enable the report on one operator. With `--watch-namespaces`, the message
includes only the namespaces that instance watches.

Zero policies still write the stamp and do not post. A missing Secret does
not stamp the week unless another endpoint already returned success.

`kubectl describe attunepolicy` shows reconcile error text. The chat message
names the policy and the workload and leaves that text out.

## Alert

`metrics.prometheusRule.rules.weeklyReportStale` is off by default. Turn it
on only when `weeklyReport.enabled` is true. A disabled report never moves
`attune_weekly_report_last_success_timestamp_seconds`, so the alert would
fire all the time.
