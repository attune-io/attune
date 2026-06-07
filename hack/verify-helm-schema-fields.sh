#!/usr/bin/env bash
# Copyright 2026 attune Authors
# SPDX-License-Identifier: Apache-2.0
#
# Verify that Helm values.schema.json field names match the CRD field names.
# Catches field name mismatches like cpuPerCorePerHour vs cpuPerCoreHour
# that cause silent data loss when additionalProperties: false is set.
#
# The generated CRD YAML is the single source of truth for field names
# (derived from Go struct json tags by controller-gen).

set -euo pipefail

SCHEMA="charts/attune/values.schema.json"
CRD="config/crd/bases/attune.io_attunedefaults.yaml"

if [ ! -f "$SCHEMA" ]; then
  echo "ERROR: $SCHEMA not found"
  exit 1
fi
if [ ! -f "$CRD" ]; then
  echo "ERROR: $CRD not found (run 'make manifests' first)"
  exit 1
fi

ERRORS=0

# compare_fields SECTION JQ_SCHEMA_PATH PYTHON_CRD_PATH
# Compares property names between the Helm schema and the CRD.
compare_fields() {
  local section="$1"
  local jq_path="$2"
  local python_path="$3"

  # Extract schema field names
  local schema_fields
  schema_fields=$(jq -r "${jq_path} | keys[]" "$SCHEMA" 2>/dev/null | sort)
  if [ -z "$schema_fields" ]; then
    echo "WARNING: no properties found in schema at ${jq_path}"
    return
  fi

  # Extract CRD field names
  local crd_fields
  crd_fields=$(python3 -c "
import yaml, sys
with open('${CRD}') as f:
    crd = yaml.safe_load(f)
props = crd['spec']['versions'][0]['schema']['openAPIV3Schema']['properties']['spec']['properties']${python_path}
for k in sorted(props.keys()):
    print(k)
" 2>/dev/null)
  if [ -z "$crd_fields" ]; then
    echo "WARNING: no properties found in CRD at spec.properties${python_path}"
    return
  fi

  # Check: schema fields must exist in CRD
  while IFS= read -r field; do
    if ! echo "$crd_fields" | grep -qx "$field"; then
      echo "ERROR: defaults.${section} schema has '${field}' but CRD does not (typo or renamed field)"
      ERRORS=$((ERRORS + 1))
    fi
  done <<< "$schema_fields"

  # Check: CRD fields missing from schema (warning only, schema may intentionally omit)
  while IFS= read -r field; do
    if ! echo "$schema_fields" | grep -qx "$field"; then
      # Only warn if the section uses additionalProperties: false
      local strict
      local section_path="${jq_path%.properties}"
      strict=$(jq -r "${section_path} | .additionalProperties // true" "$SCHEMA" 2>/dev/null)
      if [ "$strict" = "false" ]; then
        echo "WARNING: defaults.${section} CRD has '${field}' but schema omits it (users cannot set this via Helm)"
      fi
    fi
  done <<< "$crd_fields"
}

echo "Checking Helm schema field names against CRD..."

compare_fields "costPricing" \
  ".properties.defaults.properties.costPricing.properties" \
  "['costPricing']['properties']"

compare_fields "cpu" \
  ".properties.defaults.properties.cpu.properties" \
  "['cpu']['properties']"

compare_fields "memory" \
  ".properties.defaults.properties.memory.properties" \
  "['memory']['properties']"

compare_fields "updateStrategy" \
  ".properties.defaults.properties.updateStrategy.properties" \
  "['updateStrategy']['properties']"

if [ "$ERRORS" -gt 0 ]; then
  echo "FAIL: ${ERRORS} field name mismatch(es) between Helm schema and CRD."
  echo "The CRD (generated from Go types) is the source of truth."
  echo "Update values.schema.json to match."
  exit 1
fi

echo "OK: all Helm schema field names match CRD field names."