#!/usr/bin/env bash
# Smoke test for collect-fleet-reports.sh (no real cluster required when mocked).
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
tmpdir="$(mktemp -d)"
trap 'rm -rf "$tmpdir"' EXIT

# Fake kubectl that returns a fixed report for one context.
cat > "$tmpdir/kubectl" <<'KUBECTL'
#!/usr/bin/env bash
if [[ "$*" == *"config current-context"* ]]; then
  echo "fake-ctx"
  exit 0
fi
if [[ "$*" == *"get configmap"* ]]; then
  cat <<'JSON'
{"schemaVersion":"v1","clusterId":"fake","policyCount":1,"policiesByMode":{"Auto":1},"readyTrue":1,"readyFalse":0,"insufficientData":0,"workloadsDiscovered":2,"workloadsWithRecommendations":1,"workloadsResized":0,"estimatedMonthlySavingsUSD":1.5}
JSON
  exit 0
fi
echo "unexpected kubectl: $*" >&2
exit 1
KUBECTL
chmod +x "$tmpdir/kubectl"
export PATH="$tmpdir:$PATH"

out="$("$ROOT/scripts/collect-fleet-reports.sh")"
echo "$out" | jq -e 'type=="array" and length==1 and .[0].schemaVersion=="v1" and .[0].context=="fake-ctx"' >/dev/null
echo "OK: collect-fleet-reports.sh smoke test"
