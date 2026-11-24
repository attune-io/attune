#!/usr/bin/env bash
# Copyright 2026 attune Authors
# SPDX-License-Identifier: Apache-2.0
#
# Rewrite artifacthub.io/images on published Helm charts so Artifact Hub
# scans the operator image a default install pulls, not :latest.
# Without --push this only prints the plan. With --push it pushes the
# chart to GHCR, copies it to Docker Hub, and cosign-signs both.
# Re-pushing replaces the OCI digest, so the old signature is no longer
# valid. The sign steps attach a signature to the new digest.
#
# REPUBLISH_RECORD_DIR, when set, records push and sign commands and
# does not run them. Tests use it. Production leaves it unset.

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PY="${ROOT}/hack/repoint_chart_image.py"
GHCR_CHART="ghcr.io/attune-io/charts/attune"
DOCKER_CHART="docker.io/attuneio/attune-chart"
EXISTS_CMD="${EXISTS_CMD:-}"
PUSH=0
ALL=0
CHART=""
VERSIONS=()

usage() {
  cat <<'EOF'
Usage: republish-chart-images.sh [--version VER]... [--all | --chart DIR] [--push]

--version VER   Chart version to pull from GHCR. Repeatable.
--all           Every published numeric chart tag (not digests or artifacthub.io).
--chart DIR     Rewrite this unpacked chart instead of pulling.
--push          Push, copy to Docker Hub, and cosign-sign. Default is a plan.
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --version)
      [[ $# -ge 2 ]] || { echo "FAIL: --version requires a value" >&2; exit 1; }
      VERSIONS+=("$2")
      shift 2
      ;;
    --all) ALL=1; shift ;;
    --chart)
      [[ $# -ge 2 ]] || { echo "FAIL: --chart requires a directory" >&2; exit 1; }
      CHART="$2"
      shift 2
      ;;
    --push) PUSH=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *)
      echo "FAIL: unknown argument: $1" >&2
      usage >&2
      exit 1
      ;;
  esac
done

if [[ -n "$CHART" && ("$ALL" -eq 1 || ${#VERSIONS[@]} -gt 0) ]]; then
  echo "FAIL: --chart cannot be combined with --version or --all" >&2
  exit 1
fi
if [[ -z "$CHART" && "$ALL" -eq 0 && ${#VERSIONS[@]} -eq 0 ]]; then
  echo "FAIL: pass --version, --all, or --chart" >&2
  exit 1
fi

# Exit 0 when the tag exists, 1 when the registry says it is missing,
# and 2 when the check itself failed. A failed check must not be treated
# as "missing" or the script would pin the v-flipped tag by mistake.
tag_exists() {
  local tag="$1"
  local err status
  if [[ -n "$EXISTS_CMD" ]]; then
    "$EXISTS_CMD" "$tag"
    return
  fi
  set +e
  err="$(oras manifest fetch "ghcr.io/attune-io/attune:${tag}" 2>&1 >/dev/null)"
  status=$?
  set -e
  if [[ "$status" -eq 0 ]]; then
    return 0
  fi
  if [[ "$err" == *MANIFEST_UNKNOWN* || "$err" == *"not found"* || "$err" == *"404"* ]]; then
    return 1
  fi
  echo "FAIL: registry check for ${tag} failed: ${err}" >&2
  return 2
}

record_or_run() {
  if [[ -n "${REPUBLISH_RECORD_DIR:-}" ]]; then
    mkdir -p "$REPUBLISH_RECORD_DIR"
    printf '%s\n' "$*" >> "${REPUBLISH_RECORD_DIR}/commands"
    return 0
  fi
  "$@"
}

chart_version_of() {
  awk '/^version:/{print $2; exit}' "$1/Chart.yaml"
}

rewrite_chart() {
  local chart="$1"
  local label="$2"
  local rendered chosen old_new status
  rendered="$(helm template repoint "$chart" --set webhooks.enabled=false | python3 "$PY" rendered)"
  local -a exists=()
  local tag
  while IFS= read -r tag; do
    [[ -n "$tag" ]] || continue
    set +e
    tag_exists "$tag"
    status=$?
    set -e
    if [[ "$status" -eq 0 ]]; then
      exists+=(--exists "$tag")
    elif [[ "$status" -ne 1 ]]; then
      echo "FAIL: tag check for ${tag} exited ${status}" >&2
      return 1
    fi
  done < <(python3 "$PY" candidates --rendered "$rendered")
  if [[ ${#exists[@]} -eq 0 ]]; then
    chosen="$(python3 "$PY" choose --rendered "$rendered")"
  else
    chosen="$(python3 "$PY" choose --rendered "$rendered" "${exists[@]}")"
  fi
  old_new="$(python3 "$PY" apply --file "${chart}/Chart.yaml" --image "$chosen")"
  echo "PLAN: chart=${label} rendered=${rendered} ${old_new//$'\n'/ }" >&2
  printf '%s\n' "$old_new" | awk -F= '/^changed=/{print $2}'
}

push_chart() {
  local chart="$1"
  local version="$2"
  local dest tgz
  dest="$(mktemp -d)"
  tgz="$(helm package "$chart" --destination "$dest" | awk -F': ' '/saved it to/{print $2}')"
  [[ -n "$tgz" && -f "$tgz" ]] || { echo "FAIL: helm package produced no archive for ${version}" >&2; exit 1; }
  record_or_run helm push "$tgz" oci://ghcr.io/attune-io/charts
  rm -rf "$dest"
}

# Sign even when the annotation was already correct. A previous run can
# push the chart and then fail on cosign. Skipping the sign on retry
# would leave the new digest unsigned.
sign_chart() {
  local version="$1"
  record_or_run cosign sign --yes "${GHCR_CHART}:${version}"
  record_or_run oras cp "${GHCR_CHART}:${version}" "registry-1.docker.io/attuneio/attune-chart:${version}"
  record_or_run cosign sign --yes "${DOCKER_CHART}:${version}"
}

process_dir() {
  local chart="$1"
  local label="$2"
  local changed version
  changed="$(rewrite_chart "$chart" "$label")"
  version="$(chart_version_of "$chart")"
  if [[ "$PUSH" -eq 1 && "$changed" == "yes" ]]; then
    echo "DO: push ${version}"
    push_chart "$chart" "$version"
  elif [[ "$PUSH" -eq 1 ]]; then
    echo "OK: ${version} annotation already matches; not repackaging"
  else
    echo "OK: plan only for ${label}"
  fi
  if [[ "$PUSH" -eq 1 ]]; then
    echo "DO: sign ${version}"
    sign_chart "$version"
    echo "OK: signed ${version}"
  fi
}

failures=0

if [[ -n "$CHART" ]]; then
  process_dir "$CHART" "$CHART"
  echo "DONE: ok=true"
  exit 0
fi

if [[ "$ALL" -eq 1 ]]; then
  while IFS= read -r tag; do
    [[ -n "$tag" ]] || continue
    if [[ "$(python3 "$PY" version-ok "$tag")" == "yes" ]]; then
      VERSIONS+=("$tag")
    fi
  done < <(oras repo tags "$GHCR_CHART")
fi

if [[ ${#VERSIONS[@]} -eq 0 ]]; then
  echo "FAIL: no chart versions selected" >&2
  exit 1
fi

for version in "${VERSIONS[@]}"; do
  if [[ "$(python3 "$PY" version-ok "$version")" != "yes" ]]; then
    echo "FAIL: not a chart version: ${version}" >&2
    failures=1
    continue
  fi
  work="$(mktemp -d)"
  echo "DO: pull ${version}"
  if ! helm pull "oci://${GHCR_CHART}" --version "$version" --untar --untardir "$work"; then
    echo "FAIL: helm pull ${version}" >&2
    failures=1
    rm -rf "$work"
    continue
  fi
  if ! process_dir "${work}/attune" "$version"; then
    echo "FAIL: ${version}" >&2
    failures=1
  fi
  rm -rf "$work"
done

if [[ "$failures" -ne 0 ]]; then
  echo "DONE: ok=false"
  exit 1
fi
echo "DONE: ok=true"
exit 0
