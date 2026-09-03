#!/usr/bin/env bash
# Unit tests for hack/verify-chainsaw-scripts.sh.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SCRIPT="${ROOT}/hack/verify-chainsaw-scripts.sh"
tmpdir="$(mktemp -d)"
trap 'rm -rf "$tmpdir"' EXIT

assert_eq() {
  local want="$1" got="$2" name="$3"
  if [[ "$want" != "$got" ]]; then
    echo "FAIL: $name: want=$want got=$got" >&2
    exit 1
  fi
  echo "ok: $name"
}

assert_contains() {
  local needle="$1" haystack="$2" name="$3"
  if [[ "$haystack" != *"$needle"* ]]; then
    echo "FAIL: $name: missing '${needle}' in: ${haystack}" >&2
    exit 1
  fi
  echo "ok: $name"
}

write_test() {
  local dir="$1" name="$2" body="$3"
  mkdir -p "${dir}/test/e2e/${name}"
  cat >"${dir}/test/e2e/${name}/chainsaw-test.yaml" <<EOF
apiVersion: chainsaw.kyverno.io/v1alpha1
kind: Test
metadata:
  name: ${name}
spec:
  steps:
    - name: check
      try:
        - script:
            content: |
${body}
EOF
}

echo "PLAN: exercise verify-chainsaw-scripts bare grep/curl detection"

good="${tmpdir}/good"
write_test "$good" "ok" "              set -e
              grep -q attune_resize_total /tmp/m
              echo found"

if ! out="$("$SCRIPT" --root "$good" 2>&1)"; then
  echo "FAIL: good fixture should pass: $out" >&2
  exit 1
fi
assert_contains "OK:" "$out" "good fixture with set -e"

bad="${tmpdir}/bad"
write_test "$bad" "metrics" "              grep -q attune_resize_total /tmp/m
              echo found"
set +e
out="$("$SCRIPT" --root "$bad" 2>&1)"
rc=$?
set -e
assert_eq "1" "$rc" "bare grep -q exits 1"
assert_contains "bare 'grep'" "$out" "bare grep is reported"

cond="${tmpdir}/cond"
write_test "$cond" "wait" "              if echo \"\$result\" | grep -q '\"result\":\[{'; then
                echo ok
                exit 0
              fi
              echo WARNING proceeding"
if ! out="$("$SCRIPT" --root "$cond" 2>&1)"; then
  echo "FAIL: conditional grep should pass: $out" >&2
  exit 1
fi
assert_contains "OK:" "$out" "grep inside if is allowed"

andlist="${tmpdir}/andlist"
write_test "$andlist" "probes" "              set -e
              curl -sf http://127.0.0.1:9/readyz && echo readyz OK
              echo Health probes OK"
set +e
out="$("$SCRIPT" --root "$andlist" 2>&1)"
rc=$?
set -e
assert_eq "1" "$rc" "set -e plus curl && echo exits 1"
assert_contains "&&/||" "$out" "AND-OR list is reported"

echo "DONE: verify-chainsaw-scripts classifier"
