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
| `updateStrategy.maxCpuChangePercent` | `cpu.maxChangePercent` | move to cpu section |
| `updateStrategy.maxMemoryChangePercent` | `memory.maxChangePercent` | move to memory section |

### Overhead conversion examples

| Old safetyMargin | New overhead | Meaning |
|-----------------|-------------|---------|
| `"1.1"` | `"10"` | 10% headroom |
| `"1.15"` | `"15"` | 15% headroom |
| `"1.2"` | `"20"` | 20% headroom (CPU default) |
| `"1.3"` | `"30"` | 30% headroom (memory default) |
| `"1.5"` | `"50"` | 50% headroom |

### Automated migration

**Using `sed`** (covers all five renames):

```bash
# All five renames in one pass
sed -i \
  -e 's/safetyMargin:/overhead:/g' \
  -e 's/overhead: "1.1"/overhead: "10"/g' \
  -e 's/overhead: "1.15"/overhead: "15"/g' \
  -e 's/overhead: "1.2"/overhead: "20"/g' \
  -e 's/overhead: "1.25"/overhead: "25"/g' \
  -e 's/overhead: "1.3"/overhead: "30"/g' \
  -e 's/overhead: "1.5"/overhead: "50"/g' \
  -e 's/InPlaceOrEvict/InPlaceOrRecreate/g' \
  -e 's/excludeContainers:/excludedContainers:/g' \
  manifests/*.yaml

# mode -> type (only in updateStrategy context to avoid false positives)
sed -i '/updateStrategy/,/^[^ ]/{s/mode:/type:/g}' manifests/*.yaml

# bounds.min/max -> minAllowed/maxAllowed (remove nesting manually if used)
```

**Using `yq`** (handles overhead conversion and bounds restructuring):

```bash
# Export current policies
kubectl get rightsizepolicies -n production -o yaml > policies.yaml

# Rename safetyMargin to overhead and convert values
yq -i '
  .items[].spec.cpu |= (
    .overhead = ((.safetyMargin | tonumber - 1) * 100 | tostring) |
    del(.safetyMargin)
  ) |
  .items[].spec.memory |= (
    .overhead = ((.safetyMargin | tonumber - 1) * 100 | tostring) |
    del(.safetyMargin)
  )
' policies.yaml

# Apply the new CRDs first, then re-apply policies
kubectl apply -f config/crd/bases/
kubectl apply -f policies.yaml
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
