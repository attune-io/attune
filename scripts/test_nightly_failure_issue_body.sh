#!/usr/bin/env bash
# Unit tests for scripts/nightly-failure-issue-body.sh
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SCRIPT="$ROOT/scripts/nightly-failure-issue-body.sh"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

mkdir -p "$TMP/art"
printf '%s\n' '--- FAIL: TestE2E_NodeMemoryPressure_SkipsMemoryIncrease (1s)' >"$TMP/art/go-e2e.log"
# Passing variant sorts first; the reporter must still surface the failing cell.
printf '%s\n' 'Tests Summary...' '- Failed  tests 0' >"$TMP/art/chainsaw-v1.32.log"
printf '%s\n' '--- FAIL: chainsaw/configmap-export (200.33s)' 'Tests Summary...' '- Failed  tests 1' \
  >"$TMP/art/chainsaw-v1.34.log"

body=$("$SCRIPT" \
  --run-url 'https://example.com/actions/runs/99' \
  --prepare-result failure \
  --e2e-result failure \
  --fuzz-result success \
  --artifact-dir "$TMP/art")

echo "$body" | grep -q 'Prepare result: `failure`'
echo "$body" | grep -q 'E2E result: `failure`'
echo "$body" | grep -q 'Fuzz result: `success`'
echo "$body" | grep -q 'TestE2E_NodeMemoryPressure_SkipsMemoryIncrease'
echo "$body" | grep -q 'https://example.com/actions/runs/99'
echo "$body" | grep -q 'configmap-export'
echo "$body" | grep -q 'Failed  tests 1'

# Minimal path without artifacts still works
body2=$("$SCRIPT" \
  --run-url 'https://example.com/r/1' \
  --e2e-result success \
  --fuzz-result failure)
echo "$body2" | grep -q 'Fuzz result: `failure`'
echo "$body2" | grep -q 'Check the workflow run'

# A cancelled matrix leg must name the step that was still running.
# Post cleanup and the report job itself are not that step.
FAKE="${TMP}/bin"
mkdir -p "$FAKE"
cat >"$FAKE/gh" <<'EOF'
#!/usr/bin/env bash
if [[ "${1:-}" == "api" ]]; then
  cat <<'JSON'
{"jobs":[
  {"name":"E2E (K8s v1.34)","conclusion":"cancelled","steps":[
    {"name":"Create k3d cluster","conclusion":"success"},
    {"name":"Load Prometheus and test images into cluster","conclusion":"cancelled"},
    {"name":"Post Run step-security/harden-runner","conclusion":"failure"}
  ]},
  {"name":"E2E (K8s v1.32)","conclusion":"success","steps":[]},
  {"name":"Nightly Results","conclusion":"failure","steps":[
    {"name":"Summary","conclusion":"failure"}
  ]}
]}
JSON
  exit 0
fi
echo "unexpected gh $*" >&2
exit 1
EOF
chmod +x "$FAKE/gh"
body3=$(PATH="$FAKE:$PATH" "$SCRIPT" \
  --run-url 'https://example.com/actions/runs/907' \
  --run-id 907 \
  --repo attune-io/attune \
  --e2e-result cancelled \
  --fuzz-result success)
echo "$body3" | grep -F -q 'Load Prometheus and test images into cluster' \
  || { echo "$body3"; echo "missing cancelled step"; exit 1; }
echo "$body3" | grep -F -q 'Post Run step-security/harden-runner' \
  && { echo "$body3"; echo "post step leaked into the issue"; exit 1; }
echo "$body3" | grep -F -q 'Nightly Results' \
  && { echo "$body3"; echo "report job leaked into the issue"; exit 1; }

echo "OK: nightly-failure-issue-body tests passed"
