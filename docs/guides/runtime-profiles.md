# Runtime profiles

Some language runtimes do not adapt when the container **memory** cgroup
limit changes in place. Kubernetes and the IPPR GA guidance recommend
`resizePolicy: RestartContainer` for memory when the app cannot adjust
dynamically (for example some JVM and Python setups).

Attune `runtimeProfile` applies **safe defaults and admission warnings**
for those workloads. It does **not** rewrite pod specs or inject JVM flags.

## Profiles

| Profile | When unset fields default | Guidance |
|---------|---------------------------|----------|
| `generic` (default / empty) | No profile-specific defaults | Use policy fields as written |
| `java` | `memory.allowDecrease=false`, `memory.overhead=40` if unset | Prefer RestartContainer for memory on the pod; avoid live heap shrink |
| `python` | `memory.allowDecrease=false` if unset | Same caution as java for many interpreters |
| `nodejs` | `memory.allowDecrease=false` if unset | Same caution for V8 heap |
| `golang` | No extra defaults | Typically adapts to cgroup limits |

Explicit policy values always win over profile defaults.

## Example

```yaml
apiVersion: attune.io/v1alpha1
kind: AttunePolicy
metadata:
  name: jvm-api
spec:
  runtimeProfile: java
  targetRef:
    kind: Deployment
    name: payments-api
  metricsSource:
    prometheus:
      address: http://prometheus-server.monitoring:80
  updateStrategy:
    type: Recommend
```

Admission warns if `runtimeProfile` is `java`/`python`/`nodejs` and
`memory.allowDecrease: true`.

## Interaction with Kubernetes 1.35+ memory decreases

On 1.35+, Attune can apply live memory limit decreases when the platform
allows them. Profiles that set `allowDecrease=false` still block Attune
from recommending or applying decreases until you opt in. See the
architecture [resize API](../architecture/resize-api.md) notes and
startup boost for JVM cold-start CPU headroom.

## Related

- [Startup boost](startup-boost.md) for temporary CPU at start (often paired with Java)
- [Safety architecture](../architecture/safety.md) for auto-revert after resize
