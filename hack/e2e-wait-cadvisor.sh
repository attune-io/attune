#!/usr/bin/env bash
# Copyright 2026 attune Authors
# SPDX-License-Identifier: Apache-2.0
#
# Wait until Prometheus has scraped cAdvisor series
# (container_cpu_usage_seconds_total). Used by setup-e2e-cluster and
# make _deploy-stack so the two paths cannot drift.
# Soft-fails after CADVISOR_ATTEMPTS so local/CI can proceed.
#
# Overrides for tests: KUBECTL, CADVISOR_NAMESPACE, CADVISOR_ATTEMPTS,
# CADVISOR_SLEEP, CADVISOR_QUERY.

set -euo pipefail

KUBECTL="${KUBECTL:-kubectl}"
NAMESPACE="${CADVISOR_NAMESPACE:-monitoring}"
ATTEMPTS="${CADVISOR_ATTEMPTS:-30}"
SLEEP="${CADVISOR_SLEEP:-5}"
QUERY="${CADVISOR_QUERY:-container_cpu_usage_seconds_total}"

echo "PLAN: wait for cAdvisor metrics in Prometheus (${ATTEMPTS}x${SLEEP}s)"

prom_pods="$("${KUBECTL}" get pods -n "${NAMESPACE}" \
  -l app.kubernetes.io/name=prometheus,app.kubernetes.io/component=server \
  -o name || true)"
PROM_POD=""
while IFS= read -r line; do
  if [[ -n "${line}" ]]; then
    PROM_POD="${line}"
    break
  fi
done <<< "${prom_pods}"

if [[ -z "${PROM_POD}" ]]; then
  echo "WARNING: no Prometheus server pod in ${NAMESPACE}, proceeding anyway"
  echo "DONE: ok=true found=false"
  echo "NEXT: none"
  exit 0
fi

echo "DO: query ${QUERY} via ${PROM_POD}"

i=0
while (( i < ATTEMPTS )); do
  i=$((i + 1))
  result="$("${KUBECTL}" exec -n "${NAMESPACE}" "${PROM_POD}" -- \
    wget -qO- "http://localhost:9090/api/v1/query?query=${QUERY}" 2>/dev/null || true)"
  if echo "${result}" | grep -q '"result":\[{'; then
    echo "OK: cAdvisor metrics available after ${i}x${SLEEP}s"
    echo "DONE: ok=true found=true"
    echo "NEXT: none"
    exit 0
  fi
  if (( i == ATTEMPTS )); then
    elapsed=$((ATTEMPTS * SLEEP))
    echo "WARNING: cAdvisor metrics not found after ${elapsed}s, proceeding anyway"
    echo "DONE: ok=true found=false"
    echo "NEXT: none"
    exit 0
  fi
  echo "WAIT: no cAdvisor series yet (${i}/${ATTEMPTS})"
  sleep "${SLEEP}"
done
