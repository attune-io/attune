#!/usr/bin/env bash
# Copyright 2026 attune Authors
# SPDX-License-Identifier: Apache-2.0
#
# Classifier for hack/verify-helm-image-tag.sh: the live chart must pass,
# and a chart that falls back to bare appVersion (issue #546) must fail.

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
echo "DO: copy chart to ${TMP} and restore the bare-appVersion default"
cp -R "${ROOT}/charts" "${TMP}/"
# Recreate the #546 template: default image.tag to Chart.AppVersion with no v.
sed -i.bak \
  's#{{ include "attune.imageTag" . }}#{{ .Values.image.tag | default .Chart.AppVersion }}#' \
  "${TMP}/charts/attune/templates/deployment.yaml"
rm -f "${TMP}/charts/attune/templates/deployment.yaml.bak"

echo "DO: broken chart must fail"
if bash "${SCRIPT}" --root "${TMP}"; then
  echo "FAIL: bare-appVersion chart passed; the script would not have caught #546"
  echo "DONE: ok=false reason=false-negative"
  exit 1
fi
echo "OK: broken chart failed as expected"

# Chart.appVersion is normally bare SemVer. A future chart that already
# stores vX.Y.Z must still render one v, not vv.
echo "DO: chart with v-prefixed appVersion must still render a single v"
TMPV="$(mktemp -d)"
trap 'rm -rf "${TMP}" "${TMPV}"' EXIT
cp -R "${ROOT}/charts" "${TMPV}/"
sed -i.bak 's/^appVersion: .*/appVersion: v0.1.23/' "${TMPV}/charts/attune/Chart.yaml"
rm -f "${TMPV}/charts/attune/Chart.yaml.bak"
if ! bash "${SCRIPT}" --root "${TMPV}"; then
  echo "FAIL: v-prefixed appVersion must render v0.1.23, not vv0.1.23"
  echo "DONE: ok=false reason=double-v-prefix"
  exit 1
fi
echo "OK: v-prefixed appVersion rendered a single v"

echo "DONE: ok=true"
echo "NEXT: none"
exit 0
