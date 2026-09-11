#!/usr/bin/env bash
# Copyright 2026 attune Authors
# SPDX-License-Identifier: Apache-2.0
#
# Classifier for hack/e2e-verify-resize-subresource.sh.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SCRIPT="${ROOT}/hack/e2e-verify-resize-subresource.sh"
NIGHTLY="${ROOT}/.github/workflows/e2e-nightly.yaml"
MAKEFILE="${ROOT}/Makefile"

echo "PLAN: classify e2e-verify-resize-subresource.sh"

fail() {
  echo "FAIL: $*"
  echo "DONE: ok=false"
  exit 1
}

[[ -f "${SCRIPT}" ]] || fail "missing ${SCRIPT}"
[[ -x "${SCRIPT}" ]] || fail "${SCRIPT} is not executable"

echo "DO: nightly must call the helper and not inline recreate"
grep -F -q 'hack/e2e-verify-resize-subresource.sh' "${NIGHTLY}" \
  || fail "e2e-nightly.yaml does not invoke hack/e2e-verify-resize-subresource.sh"
if grep -n 'recreate_v132_cluster' "${NIGHTLY}"; then
  fail "inline recreate_v132_cluster still present in e2e-nightly.yaml"
fi
if grep -n 'seq 1 24' "${NIGHTLY}"; then
  fail "120s first-poll still present in e2e-nightly.yaml"
fi
grep -F -q 'test_e2e_verify_resize_subresource.sh' "${MAKEFILE}" \
  || fail "Makefile python-test does not run this classifier"
echo "OK: nightly and Makefile wire the helper"

TMP="$(mktemp -d)"
trap 'rm -rf "${TMP}"' EXIT

API_WITH_RESIZE='{"resources":[{"name":"pods/resize"}]}'
API_WITHOUT='{"resources":[{"name":"pods"}]}'

write_kubectl() {
  local mode="$1"
  cat >"${TMP}/kubectl" <<EOF
#!/usr/bin/env bash
set -euo pipefail
STATE="${TMP}/state"
mkdir -p "\${STATE}"
cmd="\${1:-}"
shift || true
case "\${cmd}" in
  get)
    raw=""
    for a in "\$@"; do
      path=""
      if [[ "\$a" == --raw=* ]]; then
        path="\${a#--raw=}"
      elif [[ "\$a" == "--raw" ]]; then
        raw=1
        continue
      elif [[ -n "\$raw" ]]; then
        path="\$a"
        raw=""
      else
        continue
      fi
      if [[ "\$path" == "/readyz" ]]; then
        echo ok
        exit 0
      fi
      n=\$(cat "\${STATE}/api" 2>/dev/null || echo 0)
      n=\$((n + 1))
      echo "\${n}" >"\${STATE}/api"
      case "${mode}" in
        present)
          echo '${API_WITH_RESIZE}'
          ;;
        missing)
          echo '${API_WITHOUT}'
          ;;
        after-recreate)
          rec=\$(cat "\${STATE}/recreates" 2>/dev/null || echo 0)
          if (( rec > 0 )); then
            echo '${API_WITH_RESIZE}'
          else
            echo '${API_WITHOUT}'
          fi
          ;;
        after-two)
          rec=\$(cat "\${STATE}/recreates" 2>/dev/null || echo 0)
          if (( rec >= 2 )); then
            echo '${API_WITH_RESIZE}'
          else
            echo '${API_WITHOUT}'
          fi
          ;;
      esac
      exit 0
    done
    echo "unexpected kubectl get \$*" >&2
    exit 2
    ;;
  wait)
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

write_k3d() {
  cat >"${TMP}/k3d" <<EOF
#!/usr/bin/env bash
set -euo pipefail
echo "k3d \$*" >>"${TMP}/state/k3d.log"
exit 0
EOF
  chmod +x "${TMP}/k3d"
}

write_docker() {
  cat >"${TMP}/docker" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
exit 0
EOF
  chmod +x "${TMP}/docker"
}

write_delete() {
  cat >"${TMP}/k3d-delete.sh" <<EOF
#!/usr/bin/env bash
set -euo pipefail
mkdir -p "${TMP}/state"
n=\$(cat "${TMP}/state/recreates" 2>/dev/null || echo 0)
n=\$((n + 1))
echo "\${n}" >"${TMP}/state/recreates"
exit 0
EOF
  chmod +x "${TMP}/k3d-delete.sh"
}

run_helper() {
  CLUSTER_NAME=e2e-v1.32 \
    KUBECONFIG="${TMP}/kubeconfig" \
    K3S_IMAGE=rancher/k3s:v1.32.13-k3s1 \
    KUBECTL="${TMP}/kubectl" \
    K3D="${TMP}/k3d" \
    DOCKER="${TMP}/docker" \
    K3D_DELETE="${TMP}/k3d-delete.sh" \
    RESIZE_VERIFY_ATTEMPTS=2 \
    RESIZE_VERIFY_SLEEP=0 \
    RESIZE_RECREATE_ATTEMPTS=3 \
    RESIZE_READYZ_ATTEMPTS=2 \
    RESIZE_READYZ_SLEEP=0 \
    bash "${SCRIPT}"
}

echo "DO: already-registered must not recreate"
rm -rf "${TMP}/state"
mkdir -p "${TMP}/state"
write_kubectl present
write_k3d
write_docker
write_delete
out="$(run_helper)"
echo "${out}" | grep -q 'recreated=0' || fail "expected recreated=0, got: ${out}"
if [[ -f "${TMP}/state/recreates" ]]; then
  fail "must not recreate when pods/resize is already registered"
fi
echo "OK: present cluster skips recreate"

echo "DO: missing then present after one recreate"
rm -rf "${TMP}/state"
mkdir -p "${TMP}/state"
write_kubectl after-recreate
out="$(run_helper)"
echo "${out}" | grep -q 'recreated=1' || fail "expected recreated=1, got: ${out}"
rec="$(cat "${TMP}/state/recreates")"
[[ "${rec}" == "1" ]] || fail "expected 1 recreate, got ${rec}"
echo "OK: one recreate recovers"

echo "DO: first recreate still missing, second recovers"
rm -rf "${TMP}/state"
mkdir -p "${TMP}/state"
write_kubectl after-two
out="$(run_helper)"
echo "${out}" | grep -q 'recreated=2' || fail "expected recreated=2, got: ${out}"
rec="$(cat "${TMP}/state/recreates")"
[[ "${rec}" == "2" ]] || fail "expected 2 recreates, got ${rec}"
echo "OK: second recreate recovers"

echo "DO: still missing after three recreates must fail"
rm -rf "${TMP}/state"
mkdir -p "${TMP}/state"
write_kubectl missing
if out="$(run_helper)"; then
  fail "expected non-zero after exhausted recreates: ${out}"
fi
rec="$(cat "${TMP}/state/recreates")"
[[ "${rec}" == "3" ]] || fail "expected 3 recreates, got ${rec}"
echo "OK: exhausted recreates fail"

echo "DONE: ok=true"
echo "NEXT: none"
exit 0
