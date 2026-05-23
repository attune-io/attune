# Upgrading

This page covers breaking changes between versions. If you are upgrading from an
earlier pre-release, apply every section below your current version.

## v1alpha1 Field Renames (Unreleased)

Five CRD fields were renamed to align with ecosystem conventions. Existing
`RightSizePolicy`, `RightSizeDefaults`, and `RightSizeNamespaceDefaults`
resources must be updated before applying the new CRDs.

### Field mapping

| Old field | New field | Conversion |
|-----------|-----------|------------|
| `safetyMargin: "1.2"` | `overhead: "20"` | `(old - 1) * 100` |
| `updateStrategy.mode` | `updateStrategy.type` | rename only |
| `bounds.min` / `bounds.max` | `minAllowed` / `maxAllowed` | rename only |
| `InPlaceOrEvict` | `InPlaceOrRecreate` | rename only |
| `excludeContainers` | `excludedContainers` | rename only |

### Overhead conversion examples

| Old safetyMargin | New overhead | Meaning |
|-----------------|-------------|---------|
| `"1.1"` | `"10"` | 10% headroom |
| `"1.15"` | `"15"` | 15% headroom |
| `"1.2"` | `"20"` | 20% headroom (CPU default) |
| `"1.3"` | `"30"` | 30% headroom (memory default) |
| `"1.5"` | `"50"` | 50% headroom |

### Automated migration

Use `yq` to update all policies in a namespace:

```bash
# Export current policies
kubectl get rightsizepolicies -n production -o yaml > policies.yaml

# Rename fields and convert overhead values
yq -i '
  (.items[].spec.cpu.overhead) |= (. | tonumber - 1) * 100 | tostring |
  (.items[].spec.memory.overhead) |= (. | tonumber - 1) * 100 | tostring
' policies.yaml

# Apply the new CRDs first, then re-apply policies
kubectl apply -f config/crd/bases/
kubectl apply -f policies.yaml
```

For `sed` (simpler cases where overhead is a known value):

```bash
# In-place rename for all YAML files in a directory
sed -i 's/safetyMargin:/overhead:/g' manifests/*.yaml
sed -i 's/overhead: "1.2"/overhead: "20"/g' manifests/*.yaml
sed -i 's/overhead: "1.3"/overhead: "30"/g' manifests/*.yaml
```

### Helm values migration

If you use the Helm chart with custom `defaults.cpu.overhead` or
`defaults.memory.overhead` in your `values.yaml`, update the values:

```yaml
# Before
defaults:
  cpu:
    safetyMargin: "1.2"
  memory:
    safetyMargin: "1.3"

# After
defaults:
  cpu:
    overhead: "20"
  memory:
    overhead: "30"
```
