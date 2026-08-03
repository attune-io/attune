# Bin packing and cluster autoscaler coexistence

Attune optimizes **pod requests and limits** (vertical loop). Real cluster cost
reduction usually also needs **better packing** and fewer (or smaller) nodes.
Those are complementary jobs.

## What Attune does

- Right-sizes running pods via in-place resize
- Guards increases against node **allocatable** headroom
- Skips request **increases** when the node is under **MemoryPressure**,
  **DiskPressure**, or **PIDPressure** (decreases still allowed)
- Exposes savings metrics and status for capacity planning

## What Attune does not do

- Provision or deprovision nodes
- Replace Cluster Autoscaler, Karpenter-class provisioners, or cloud ASGs
- Own bin-packing placement (the scheduler still places pods)

## Safe coexistence patterns

1. Run Attune so over-requested workloads free **request** capacity.
2. Let your node autoscaler consolidate underutilized nodes on its own
   schedule and disruption budgets.
3. Prefer Recommend → Canary → Auto so aggressive shrink does not fight
   concurrent node drains without observation.
4. Use savings / reclaimed capacity signals (status + Grafana) as input to
   capacity reviews; do not wire Attune as a remote node controller.

## Node capacity awareness

See also issue-aligned behavior:

- Skip resizes that would make total pod requests exceed node allocatable
- Skip request increases under node pressure conditions

Tune `maxAllowed` and change caps so recommendations stay within typical
node shapes for the pool.

## Related

- [Multi-cluster](multi-cluster.md) for fleet dashboards
- [Scaling guide](scaling.md) for operator sizing
- [HPA coexistence](hpa-coexistence.md)

## Reclaimed capacity signals

After each reconcile (non-Observe modes), Attune estimates freeable **request**
capacity if recommended decreases were applied:

| Surface | Field / metric |
|---------|----------------|
| Status | `status.savings.reclaimedCpuRequest`, `reclaimedMemoryRequest` |
| Status (legacy names) | `cpuRequestReduction`, `memoryRequestReduction` |
| Prometheus | `attune_reclaimed_request_cpu_cores{namespace,policy}` |
| Prometheus | `attune_reclaimed_request_memory_bytes{namespace,policy}` |
| Prometheus (namespace totals) | `attune_savings_cpu_cores_total`, `attune_savings_memory_bytes_total` |

### Using with cluster autoscalers

1. Prefer Attune in Recommend or Canary until reclaimed capacity is stable.
2. Feed reclaimed metrics into capacity dashboards (Grafana panel
   "Reclaimed request capacity").
3. Let Cluster Autoscaler / Karpenter consolidate on **their** disruption
   budgets; do not trigger node delete from Attune.
4. Cross-check node pressure skips (`attune_capacity_skip_total`) so you do
   not interpret "no resize" as "no savings opportunity."

### Phase B design (optional export for consolidation controllers)

Future opt-in work (not required for Phase A):

- Annotate or export a small ConfigMap per namespace with aggregate
  `reclaimedCpu` / `reclaimedMemory` and policy generation for external
  consolidation controllers to scrape without Prometheus.
- No hard dependency on a vendor autoscaler API in Attune core.

See also [node capacity formulas](../architecture/node-capacity.md).

