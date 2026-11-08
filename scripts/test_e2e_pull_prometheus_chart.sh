#!/usr/bin/env bash
# Copyright 2026 attune Authors
# SPDX-License-Identifier: Apache-2.0
#
# Classifier + unit tests for hack/e2e-pull-prometheus-chart.sh.
# Nightly #1125 died on a GitHub Releases HTTP 504 while pulling the
# pinned Prometheus chart. Both E2E paths must share the helper, cache
# the tarball by chart version (not by k3s image), and skip helm repo
# add / helm pull when the restored file is that chart.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SCRIPT="${ROOT}/hack/e2e-pull-prometheus-chart.sh"
NIGHTLY="${ROOT}/.github/workflows/e2e-nightly.yaml"
COMPOSITE="${ROOT}/.github/actions/setup-e2e-cluster/action.yaml"
CI_YAML="${ROOT}/.github/workflows/ci.yaml"

echo "PLAN: classify e2e-pull-prometheus-chart.sh"

fail() {
  echo "FAIL: $*"
  echo "DONE: ok=false"
  exit 1
}

[[ -f "${SCRIPT}" ]] || fail "missing ${SCRIPT}"

read_version() {
  local file="$1"
  sed -n 's/.*PROMETHEUS_CHART_VERSION: "\([^"]*\)".*/\1/p' "${file}" | awk 'NR==1 { print; exit }'
}

echo "DO: nightly and setup-e2e-cluster must call the helper"
grep -F -q 'hack/e2e-pull-prometheus-chart.sh' "${NIGHTLY}" \
  || fail "e2e-nightly.yaml does not invoke hack/e2e-pull-prometheus-chart.sh"
grep -F -q 'hack/e2e-pull-prometheus-chart.sh' "${COMPOSITE}" \
  || fail "setup-e2e-cluster/action.yaml does not invoke hack/e2e-pull-prometheus-chart.sh"
if grep -n 'helm pull ' "${NIGHTLY}" "${COMPOSITE}"; then
  fail "inline helm pull still present; use the helper"
fi
if grep -n 'helm repo add ' "${NIGHTLY}" "${COMPOSITE}"; then
  fail "inline helm repo add still present; use the helper"
fi
echo "OK: both install paths call the helper"

echo "DO: chart cache key is the chart version, shared by every k3s cell"
grep -F -q 'path: /tmp/prometheus-${{ env.PROMETHEUS_CHART_VERSION }}.tgz' "${NIGHTLY}" \
  || fail "e2e-nightly.yaml missing the pinned chart cache path"
grep -F -q 'path: /tmp/prometheus-${{ inputs.prometheus-chart-version }}.tgz' "${COMPOSITE}" \
  || fail "setup-e2e-cluster missing the pinned chart cache path"
grep -F -q 'key: prometheus-chart-${{ runner.os }}-${{ env.PROMETHEUS_CHART_VERSION }}' "${NIGHTLY}" \
  || fail "e2e-nightly.yaml missing the version-scoped chart cache key"
grep -F -q 'key: prometheus-chart-${{ runner.os }}-${{ inputs.prometheus-chart-version }}' "${COMPOSITE}" \
  || fail "setup-e2e-cluster missing the version-scoped chart cache key"
if grep -E '^[[:space:]]*key: prometheus-chart-' "${NIGHTLY}" "${COMPOSITE}" | grep -q 'k3s'; then
  fail "prometheus chart cache key still includes the k3s image"
fi
grep -F -q 'PROMETHEUS_CHART_VERSION: ${{ inputs.prometheus-chart-version }}' "${COMPOSITE}" \
  || fail "setup-e2e-cluster does not pass the chart version into the helper"
echo "OK: chart cache is version-scoped"

VER="$(read_version "${CI_YAML}")"
NIGHTLY_VER="$(read_version "${NIGHTLY}")"
[[ -n "${VER}" ]] || fail "could not read PROMETHEUS_CHART_VERSION from ci.yaml"
[[ "${VER}" == "${NIGHTLY_VER}" ]] || fail "ci.yaml chart ${VER} != nightly ${NIGHTLY_VER}"
grep -F -q 'DOWNLOAD_ATTEMPTS:-3' "${SCRIPT}" \
  || fail "helper default retry count is no longer 3"
echo "OK: pinned chart version ${VER}, three pull attempts"

TMP="$(mktemp -d)"
trap 'rm -rf "${TMP}"' EXIT

write_chart() {
  local dest="$1" version="$2" extra="$3"
  local root="${TMP}/src-${version}-${extra}"
  rm -rf "${root}"
  mkdir -p "${root}/prometheus/charts/aaa"
  cat >"${root}/prometheus/Chart.yaml" <<EOF
apiVersion: v2
name: prometheus
version: ${version}
EOF
  # A nested Chart.yaml that names the same chart must not win.
  cat >"${root}/prometheus/charts/aaa/Chart.yaml" <<EOF
apiVersion: v2
name: prometheus
version: 99.0.0
EOF
  tar -czf "${dest}" -C "${root}" prometheus
}

write_helm() {
  local mode="$1"
  cat >"${TMP}/helm" <<EOF
#!/usr/bin/env bash
set -euo pipefail
LOG="${TMP}/helm.log"
printf '%s\n' "\$*" >>"\${LOG}"
if [[ "\$1" != "pull" ]]; then
  exit 0
fi
dest_dir=""
prev=""
for arg in "\$@"; do
  if [[ "\${prev}" == "--destination" ]]; then
    dest_dir="\${arg}"
  fi
  prev="\${arg}"
done
[[ -n "\${dest_dir}" ]] || exit 2
if [[ "${mode}" == "fail" ]]; then
  printf '%s\n' 'not-a-chart' >"\${dest_dir}/prometheus-${VER}.tgz"
  exit 1
fi
cp "${TMP}/good.tgz" "\${dest_dir}/prometheus-${VER}.tgz"
exit 0
EOF
  chmod +x "${TMP}/helm"
}

run_helper() {
  HELM="${TMP}/helm" \
    PROMETHEUS_CHART_VERSION="${VER}" \
    DOWNLOAD_ATTEMPTS="${DOWNLOAD_ATTEMPTS:-3}" \
    DOWNLOAD_RETRY_SLEEP=0 \
    bash "${SCRIPT}" "${TMP}/prometheus-${VER}.tgz"
}

write_chart "${TMP}/good.tgz" "${VER}" good

echo "DO: valid dest is reused without helm"
cp "${TMP}/good.tgz" "${TMP}/prometheus-${VER}.tgz"
write_helm fail
if ! run_helper; then
  fail "valid dest should be treated as cached"
fi
if [[ -f "${TMP}/helm.log" ]]; then
  fail "cached dest must not invoke helm"
fi
echo "OK: cached valid dest"

echo "DO: dots in the pinned version are not wildcards"
write_chart "${TMP}/prometheus-${VER}.tgz" "27x22y0" wild
rm -f "${TMP}/helm.log"
write_helm ok
if ! run_helper; then
  fail "27x22y0 must not count as cached ${VER}"
fi
[[ -s "${TMP}/helm.log" ]] || fail "wildcard version must invoke helm"
echo "OK: version match is exact"

echo "DO: nested Chart.yaml with a different version is not a cache hit"
write_chart "${TMP}/prometheus-${VER}.tgz" "0.0.1" nested
rm -f "${TMP}/helm.log"
write_helm ok
if ! run_helper; then
  fail "wrong root version should pull"
fi
[[ -s "${TMP}/helm.log" ]] || fail "expected helm pull when the root version mismatches"
grep -q '^pull ' "${TMP}/helm.log" || fail "helm log missing pull"
grep -q 'repo add' "${TMP}/helm.log" || fail "cache miss must helm repo add"
grep -q 'repo update' "${TMP}/helm.log" || fail "cache miss must helm repo update"
rm -rf "${TMP}/extracted"
mkdir -p "${TMP}/extracted"
tar -xzf "${TMP}/prometheus-${VER}.tgz" -C "${TMP}/extracted" prometheus/Chart.yaml
grep -q "version: ${VER}" "${TMP}/extracted/prometheus/Chart.yaml" \
  || fail "pull did not replace the mismatched chart"
echo "OK: mismatched root version pulls"

echo "DO: empty and garbage files are discarded"
: >"${TMP}/prometheus-${VER}.tgz"
rm -f "${TMP}/helm.log"
write_helm ok
if ! run_helper; then
  fail "empty dest should pull"
fi
[[ -s "${TMP}/helm.log" ]] || fail "empty dest must not count as cached"
printf '%s\n' 'Gateway Timeout' >"${TMP}/prometheus-${VER}.tgz"
rm -f "${TMP}/helm.log"
if ! run_helper; then
  fail "garbage dest should pull"
fi
if grep -q 'Gateway Timeout' "${TMP}/prometheus-${VER}.tgz"; then
  fail "garbage dest was kept"
fi
echo "OK: discarded empty and garbage dest"

echo "DO: three failed pulls remove the partial file and exit non-zero"
rm -f "${TMP}/prometheus-${VER}.tgz" "${TMP}/helm.log"
write_helm fail
if DOWNLOAD_ATTEMPTS=3 run_helper; then
  fail "failed pulls should exit non-zero"
fi
if [[ -f "${TMP}/prometheus-${VER}.tgz" ]]; then
  fail "failed pull left a file at dest"
fi
pulls="$(grep -c '^pull ' "${TMP}/helm.log" || true)"
[[ "${pulls}" == "3" ]] || fail "expected 3 pull attempts, got ${pulls}"
echo "OK: fail-closed after 3 pulls"

echo "DO: wrong dest filename fails before helm"
rm -f "${TMP}/helm.log"
if HELM="${TMP}/helm" \
  PROMETHEUS_CHART_VERSION="${VER}" \
  DOWNLOAD_RETRY_SLEEP=0 \
  bash "${SCRIPT}" "${TMP}/chart.tgz"; then
  fail "misnamed dest should exit non-zero"
fi
if [[ -f "${TMP}/helm.log" ]]; then
  fail "misnamed dest must not invoke helm"
fi
echo "OK: misnamed dest"

echo "DONE: ok=true"
echo "NEXT: none"
exit 0
