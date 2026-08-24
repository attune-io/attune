#!/usr/bin/env bash
# Copyright 2026 attune Authors
# SPDX-License-Identifier: Apache-2.0
#
# Classifier for hack/verify-helm-image-tag.sh: the live chart must pass,
# and a chart that still prefixes v onto appVersion must fail.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SCRIPT="${ROOT}/hack/verify-helm-image-tag.sh"

echo "PLAN: classify verify-helm-image-tag.sh against live and broken charts"

echo "DO: live chart must pass"
if ! bash "${SCRIPT}" --root "${ROOT}"; then
  echo "FAIL: live chart failed verify-helm-image-tag.sh"
  echo "DONE: ok=false reason=live-chart-failed"
  exit 1
fi
echo "OK: live chart passed"

TMP="$(mktemp -d)"
trap 'rm -rf "${TMP}"' EXIT
echo "DO: copy chart to ${TMP} and restore the v-prefix helper"
cp -R "${ROOT}/charts" "${TMP}/"
# Recreate the post-#546 helper: prefix v onto Chart.AppVersion.
sed -i.bak \
  's#{{- .Chart.AppVersion | trimPrefix "v" -}}#{{- printf "v%s" (.Chart.AppVersion | trimPrefix "v") -}}#' \
  "${TMP}/charts/attune/templates/_helpers.tpl"
if ! grep -q 'printf "v%s"' "${TMP}/charts/attune/templates/_helpers.tpl"; then
  echo "FAIL: could not inject v-prefix helper into copied chart"
  echo "DONE: ok=false reason=fixture-not-mutated"
  exit 1
fi
rm -f "${TMP}/charts/attune/templates/_helpers.tpl.bak"

echo "DO: v-prefix chart must fail"
if bash "${SCRIPT}" --root "${TMP}"; then
  echo "FAIL: v-prefix chart passed; the script would not catch a helper regression"
  echo "DONE: ok=false reason=false-negative"
  exit 1
fi
echo "OK: v-prefix chart failed as expected"

# Chart.appVersion is normally bare SemVer. A future chart that already
# stores vX.Y.Z must still render the bare tag, not vX.Y.Z.
echo "DO: chart with v-prefixed appVersion must still render bare SemVer"
TMPV="$(mktemp -d)"
trap 'rm -rf "${TMP}" "${TMPV}"' EXIT
cp -R "${ROOT}/charts" "${TMPV}/"
sed -i.bak 's/^appVersion: .*/appVersion: v0.1.23/' "${TMPV}/charts/attune/Chart.yaml"
rm -f "${TMPV}/charts/attune/Chart.yaml.bak"
if ! bash "${SCRIPT}" --root "${TMPV}"; then
  echo "FAIL: v-prefixed appVersion must render 0.1.23, not v0.1.23"
  echo "DONE: ok=false reason=v-prefix-not-stripped"
  exit 1
fi
echo "OK: v-prefixed appVersion rendered bare SemVer"

echo "DONE: ok=true"
echo "NEXT: none"
exit 0
