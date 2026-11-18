# Node capacity and pressure formulas

Attune applies **always-on** node capacity and pressure gates in the resize
path (not opt-in). They are safety defaults: skip or avoid request increases
that cannot fit on the current node or that worsen a stressed node. They do
not require a policy feature flag.

Related product work: capacity-aware recommendations (#372 / #445) and
bin-packing / reclaimed capacity signals (#431 / #446).

## Multi-container pod formula

For a pod on node `N` with target container `C` and proposed target requests
`T_cpu`, `T_mem`:

1. Build the **planned** pod, the shape Attune would write:
   - Merge `T_*` into `C`'s requests. A resource the target omits keeps
     its current value
   - When in-place pod-level resize is on and the pod has
     `spec.resources`, raise that envelope to cover the containers, as the
     resize does
2. Take the planned pod's **effective request**, the scheduler's formula
   (`k8s.io/component-helpers/resource.PodRequests`):
   - The sum of `spec.containers` and native sidecars (init containers
     with `restartPolicy: Always`)
   - Or, if larger, each regular init container plus the native sidecars
     declared before it. A finished init container still counts: the
     scheduler and kubelet resize admission do not check the phase
   - Pod-level `spec.resources.requests` replace that result for each
     resource they set
   - Plus `spec.overhead` (RuntimeClass overhead)
3. That gives CPU millicores and memory bytes → `sum_cpu`, `sum_mem`
4. Read `N.status.allocatable` for CPU and memory
5. **Skip resize** if `sum_cpu > allocatable_cpu` OR `sum_mem > allocatable_mem`

Reason string: `total pod requests would exceed node allocatable`.

Metric: `attune_capacity_skip_total{reason="allocatable"}`.

## Neighbor free request budget

For a **request increase** on a scheduled pod (`spec.nodeName` set):

1. List the pods on the same node (all namespaces), leaving out
   `Succeeded` and `Failed` pods
2. Sum the effective request of each listed pod (same formula as step 2
   above, from its current spec), then subtract this pod's own entry as
   listed → `neighbor_sum`. Neighbors with no requests add zero
3. Skip if `sum_* + neighbor_sum` exceeds allocatable CPU or memory

Known limit: kubelet sizes a pod with a pending resize from its
allocated resources, not its spec. Attune reads the spec only, so a
neighbor in the middle of a decrease is under-counted and a `Deferred`
increase is over-counted until kubelet catches up.

Reason string: `node free request budget exceeded by neighbors`.

Metric: `attune_capacity_skip_total{reason="neighbors"}`.

Decreases stay allowed. Pods without `nodeName` skip this gate (not
scheduled yet). DaemonSet pods use the same formula on their current
node. A failed neighbor list **skips the increase** (fail-closed; same
class as unavailable node status). There is no extra safety margin:
the comparison is a hard greater-than against allocatable.

Static and mirror pods without requests do not consume the budget.

## DaemonSet special case

DaemonSet pods are **node-bound**: there is one pod per node (subject to
nodeSelector / affinity). Capacity gates still use the same formula against
the **current** node:

- Increases that would make *this* DaemonSet pod's total requests exceed
  that node's allocatable are skipped.
- Attune does not rebalance across nodes or skip entire DaemonSets cluster-wide.
- Prefer conservative `maxAllowed` for DaemonSets so recommendations stay
  within the smallest node pool shape you schedule them on.

## Pressure gates

When the node has any of these conditions `True`:

| Condition | Effect |
|-----------|--------|
| `MemoryPressure` | Skip **memory request increases** (CPU increases still allowed) |
| `DiskPressure` | Skip **any request increase** (CPU or memory) |
| `PIDPressure` | Skip **any request increase** |

**Decreases remain allowed** under pressure (they free capacity).

Metric: `attune_capacity_skip_total{reason="pressure"}`.

Pressure is read via the typed Kubernetes clientset (live API), not only
the informer cache, and is re-checked immediately before `UpdateResize`
so a mid-reconcile condition flip cannot authorize an increase.

## Unavailable node status (fail-closed for increases)

If the pod has a `nodeName` but Attune cannot load the Node object
(Clientset and controller-runtime client both fail or are unset),
**request increases are skipped**. Decreases still proceed (same idea as
pressure: free capacity without needing a pressure read).

Reason string: `node status unavailable; skipping request increase`.
Metric: `attune_capacity_skip_total{reason="unavailable"}`.
Event: `ResizeSkipped`.

Empty `nodeName` (not scheduled yet) does not use this path; scheduling
gates apply elsewhere.

## Always-on default

These gates run for every resize attempt. There is no `capacityAware: false`
escape hatch in the first release. To effectively disable pressure sensitivity
you would need nodes without those conditions; allocatable skip only fires
when the proposed pod total exceeds allocatable.

## Reclaimed capacity (bin packing signal)

When recommendations are **below** current requests, status and metrics
expose freeable request capacity for node consolidation tools:

- Status: `status.savings.reclaimedCpuRequest` / `reclaimedMemoryRequest`
  (aliases of the reduction fields)
- Metrics: `attune_reclaimed_request_cpu_cores`, `attune_reclaimed_request_memory_bytes`

These are **estimated** from recommendations, not measured node free space.
Use them with Cluster Autoscaler / Karpenter-class tools as capacity review
inputs, not as a remote node controller. See [bin packing](../guides/bin-packing.md).
