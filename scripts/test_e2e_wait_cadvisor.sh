#!/usr/bin/env bash
# Copyright 2026 attune Authors
# SPDX-License-Identifier: Apache-2.0
#
# Classifier for hack/e2e-wait-cadvisor.sh: PR-CI composite and local
# _deploy-stack must share the helper; missing series is a soft fail.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SCRIPT="${ROOT}/hack/e2e-wait-cadvisor.sh"
MAKEFILE="${ROOT}/Makefile"
COMPOSITE="${ROOT}/.github/actions/setup-e2e-cluster/action.yaml"

echo "PLAN: classify e2e-wait-cadvisor.sh"

fail() {
  echo "FAIL: $*"
  echo "DONE: ok=false"
  exit 1
}

[[ -f "${SCRIPT}" ]] || fail "missing ${SCRIPT}"
[[ -x "${SCRIPT}" ]] || fail "${SCRIPT} is not executable"

echo "DO: setup-e2e-cluster and Makefile must call the helper"
grep -F -q 'hack/e2e-wait-cadvisor.sh' "${COMPOSITE}" \
  || fail "setup-e2e-cluster/action.yaml does not invoke hack/e2e-wait-cadvisor.sh"
grep -F -q 'hack/e2e-wait-cadvisor.sh' "${MAKEFILE}" \
  || fail "Makefile does not invoke hack/e2e-wait-cadvisor.sh"
if grep -n 'Waiting for Prometheus to scrape cAdvisor metrics' "${COMPOSITE}"; then
  fail "inline cAdvisor wait still present in setup-e2e-cluster; use the helper"
fi
echo "OK: both install paths call the helper"

echo "DO: Makefile pins Prometheus image and uses 5m Helm timeout"
grep -qE 'PROMETHEUS_IMAGE \?= quay.io/prometheus/prometheus:v3.4.1' "${MAKEFILE}" \
  || fail "Makefile missing PROMETHEUS_IMAGE pin matching CI"
if ! awk '
  /helm install prometheus / {p=1}
  p && /--wait --timeout 5m/ {found=1}
  p && /^[^[:space:]#]/ && !/helm install prometheus / {exit}
  END {exit found ? 0 : 1}
' "${MAKEFILE}"; then
  fail "Makefile Prometheus helm install is not --wait --timeout 5m"
fi
echo "OK: Makefile Prometheus pin and timeout"

TMP="$(mktemp -d)"
trap 'rm -rf "${TMP}"' EXIT

write_kubectl() {
  local empty_queries="$1"
  cat >"${TMP}/kubectl" <<EOF
#!/usr/bin/env bash
set -euo pipefail
STATE="${TMP}/state"
mkdir -p "\${STATE}"
cmd="\${1:-}"
shift || true
case "\${cmd}" in
  get)
    if [[ "\${NO_POD:-0}" == "1" ]]; then
      exit 0
    fi
    echo "pod/prometheus-server-0"
    exit 0
    ;;
  exec)
    n=\$(cat "\${STATE}/query" 2>/dev/null || echo 0)
    n=\$((n + 1))
    echo "\${n}" >"\${STATE}/query"
    if (( n <= ${empty_queries} )); then
      echo '{"status":"success","data":{"resultType":"vector","result":[]}}'
      exit 0
    fi
    echo '{"status":"success","data":{"resultType":"vector","result":[{"metric":{"__name__":"container_cpu_usage_seconds_total"},"value":[1,"1"]}]}}'
    exit 0
    ;;
  *)
    echo "unexpected kubectl \${cmd} \$*" >&2
    exit 2
    ;;
esac
EOF
  chmod +x "${TMP}/kubectl"
}

run_helper() {
  KUBECTL="${TMP}/kubectl" \
    CADVISOR_ATTEMPTS=3 \
    CADVISOR_SLEEP=0 \
    bash "${SCRIPT}"
}

echo "DO: first empty query then series must report found"
rm -rf "${TMP}/state"
write_kubectl 1
out="$(run_helper)"
echo "${out}" | grep -q 'found=true' || fail "expected found=true, got: ${out}"
query_n="$(cat "${TMP}/state/query")"
[[ "${query_n}" == "2" ]] || fail "expected 2 queries, got ${query_n}"
echo "OK: waited then found series"

echo "DO: never-found series must soft-fail and exit 0"
rm -rf "${TMP}/state"
write_kubectl 99
if ! out="$(run_helper)"; then
  fail "missing series should exit 0"
fi
echo "${out}" | grep -q 'found=false' || fail "expected found=false, got: ${out}"
query_n="$(cat "${TMP}/state/query")"
[[ "${query_n}" == "3" ]] || fail "expected 3 queries, got ${query_n}"
echo "OK: exhausted attempts and proceeded"

echo "DO: missing Prometheus pod must soft-fail and skip exec"
rm -rf "${TMP}/state"
write_kubectl 0
if ! out="$(NO_POD=1 run_helper)"; then
  fail "missing pod should exit 0"
fi
echo "${out}" | grep -q 'found=false' || fail "missing pod expected found=false, got: ${out}"
if [[ -f "${TMP}/state/query" ]]; then
  fail "exec must not run when no Prometheus pod exists"
fi
echo "OK: no pod proceeds without exec"

echo "DONE: ok=true"
echo "NEXT: none"
exit 0
