#!/usr/bin/env bash
# Copyright 2026 attune-io
# SPDX-License-Identifier: Apache-2.0
#
# Collect attune fleet report ConfigMaps from one or more kubeconfig contexts
# into a single JSON array (Phase B of issue #369).
#
# Usage:
#   scripts/collect-fleet-reports.sh
#   scripts/collect-fleet-reports.sh --contexts dev,staging,prod
#   scripts/collect-fleet-reports.sh --all-contexts --namespace attune-system
#
# Requires: kubectl, jq
set -euo pipefail

NAMESPACE="${NAMESPACE:-attune-system}"
CM_NAME="${CM_NAME:-attune-fleet-report}"
CONTEXTS=""
ALL_CONTEXTS=0

while [[ $# -gt 0 ]]; do
  case "$1" in
    --contexts)
      CONTEXTS="${2:-}"
      shift 2
      ;;
    --all-contexts)
      ALL_CONTEXTS=1
      shift
      ;;
    --namespace|-n)
      NAMESPACE="${2:-}"
      shift 2
      ;;
    --name)
      CM_NAME="${2:-}"
      shift 2
      ;;
    -h|--help)
      sed -n '2,20p' "$0"
      exit 0
      ;;
    *)
      echo "unknown arg: $1" >&2
      exit 2
      ;;
  esac
done

if ! command -v jq >/dev/null 2>&1; then
  echo "jq is required" >&2
  exit 1
fi

# Bash 3.2-compatible (macOS /bin/bash): avoid mapfile.
CTX_LIST=()
if [[ "$ALL_CONTEXTS" -eq 1 ]]; then
  while IFS= read -r line; do
    [[ -n "$line" ]] && CTX_LIST+=("$line")
  done < <(kubectl config get-contexts -o name)
elif [[ -n "$CONTEXTS" ]]; then
  IFS=',' read -r -a CTX_LIST <<< "$CONTEXTS"
else
  CTX_LIST=("$(kubectl config current-context)")
fi

echo "["
first=1
for ctx in "${CTX_LIST[@]}"; do
  [[ -z "$ctx" ]] && continue
  raw="$(kubectl --context="$ctx" -n "$NAMESPACE" get configmap "$CM_NAME" \
    -o jsonpath='{.data.report\.json}' 2>/dev/null || true)"
  if [[ -z "$raw" ]]; then
    entry="$(jq -nc --arg ctx "$ctx" --arg ns "$NAMESPACE" --arg name "$CM_NAME" \
      '{context:$ctx, error:"configmap not found or empty", namespace:$ns, configMap:$name}')"
  else
    entry="$(jq -c --arg ctx "$ctx" '. + {context:$ctx}' <<<"$raw")"
  fi
  if [[ "$first" -eq 1 ]]; then
    first=0
  else
    echo ","
  fi
  printf '%s' "$entry"
done
echo
echo "]"
