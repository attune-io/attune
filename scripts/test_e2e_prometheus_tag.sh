#!/usr/bin/env bash
# Helm must install the tag from the Prometheus image input, not a
# second hardcoded tag.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"

echo "DO: e2e Prometheus install uses the image tag"
for f in \
  .github/actions/setup-e2e-cluster/action.yaml \
  .github/workflows/e2e-nightly.yaml
do
  if grep -q 'server.image.tag=v3.4.1' "${ROOT}/${f}"; then
    echo "FAIL: ${f} hardcodes server.image.tag=v3.4.1"
    exit 1
  fi
  grep -q 'PROM_TAG="${PROMETHEUS_IMAGE##\*:}"' "${ROOT}/${f}" || {
    echo "FAIL: ${f} does not derive PROM_TAG from PROMETHEUS_IMAGE"
    exit 1
  }
  grep -q 'server.image.tag="${PROM_TAG}"' "${ROOT}/${f}" || {
    echo "FAIL: ${f} does not pass PROM_TAG to helm"
    exit 1
  }
done
echo "OK: both install paths use the image tag"
echo "DONE: ok=true"
