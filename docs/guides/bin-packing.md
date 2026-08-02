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
