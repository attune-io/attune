#!/usr/bin/env bash
#
# run-fuzz.sh
#
# Run Attune's Go native fuzz targets with hardening against a known
# toolchain flake: when -fuzztime expires, go test occasionally fails
# with only "context deadline exceeded" and no failing input
# (golang/go#75804 / attune-io/attune#420).
#
# Strategy (strict, does not mask real bugs):
# 1. Run each fuzz target independently so one flake cannot hide others.
# 2. On failure, classify the log:
#    - deadline flake: "context deadline exceeded" AND no crash corpus /
#      panic / t.Error lines → retry once.
#    - anything else → fail that target immediately (no retry).
# 3. Aggregate: exit non-zero if any target still failed after retry.
#
# Usage:
#   scripts/run-fuzz.sh
#   FUZZTIME=5s scripts/run-fuzz.sh
#   scripts/run-fuzz.sh --classify-log /path/to/log   # for tests
#
# Env:
#   FUZZTIME          per-target duration (default: 30s)
#   FUZZ_MAX_RETRIES  retries for deadline flakes only (default: 1)
#
set -euo pipefail

FUZZTIME="${FUZZTIME:-30s}"
FUZZ_MAX_RETRIES="${FUZZ_MAX_RETRIES:-1}"

# package|target
TARGETS=(
  "./internal/recommendation/...|FuzzPercentileEstimator"
  "./internal/recommendation/...|FuzzRecommendationEngine"
  "./internal/webhook/...|FuzzValidateFloatFields"
)

# Classify a go test -fuzz failure log.
# Prints: deadline_flake | real_failure
classify_fuzz_failure() {
  local log_file="$1"

  if [[ ! -s "$log_file" ]]; then
    echo "real_failure"
    return 0
  fi

  # Real crash corpus or assertion path — never treat as flake.
  if grep -qE 'failing input written to|panic:|[[:alnum:]_./-]+\.go:[0-9]+:' "$log_file"; then
    echo "real_failure"
    return 0
  fi

  # Known toolchain race at -fuzztime boundary (no corpus, no assert).
  if grep -q 'context deadline exceeded' "$log_file"; then
    echo "deadline_flake"
    return 0
  fi

  echo "real_failure"
  return 0
}

run_one_fuzz() {
  local pkg="$1"
  local target="$2"
  local attempt="$3"
  local log_file="$4"
  local rc

  echo "==> fuzz ${target} (pkg=${pkg}, fuzztime=${FUZZTIME}, attempt=${attempt})"
  # Stream output and capture for classification. PIPESTATUS[0] is go test.
  set +e
  set -o pipefail
  go test "${pkg}" -run='^$' -fuzz="${target}" -fuzztime="${FUZZTIME}" 2>&1 | tee "${log_file}"
  rc=${PIPESTATUS[0]}
  set +o pipefail
  set -e
  return "${rc}"
}

run_target_with_retry() {
  local pkg="$1"
  local target="$2"
  local log_dir="$3"
  local attempt=1
  local max_attempts=$((FUZZ_MAX_RETRIES + 1))
  local log_file
  local kind

  while (( attempt <= max_attempts )); do
    log_file="${log_dir}/${target}.attempt${attempt}.log"
    if run_one_fuzz "${pkg}" "${target}" "${attempt}" "${log_file}"; then
      echo "OK: ${target}"
      return 0
    fi
    kind="$(classify_fuzz_failure "${log_file}")"
    echo "FAIL: ${target} (class=${kind})"

    if [[ "${kind}" == "deadline_flake" ]] && (( attempt < max_attempts )); then
      echo "WARN: ${target} hit Go fuzz deadline flake; retrying once (see golang/go#75804)"
      attempt=$((attempt + 1))
      continue
    fi

    if [[ "${kind}" == "deadline_flake" ]]; then
      echo "ERROR: ${target} still deadline-flaking after ${FUZZ_MAX_RETRIES} retry(ies)"
    else
      echo "ERROR: ${target} real fuzz failure (no deadline-only retry)"
    fi
    return 1
  done

  return 1
}

if [[ "${1:-}" == "--classify-log" ]]; then
  if [[ -z "${2:-}" ]]; then
    echo "Usage: $0 --classify-log <path>" >&2
    exit 2
  fi
  classify_fuzz_failure "$2"
  exit 0
fi

if [[ "${1:-}" == "-h" || "${1:-}" == "--help" ]]; then
  sed -n '2,30p' "$0"
  exit 0
fi

tmp_dir="$(mktemp -d)"
trap 'rm -rf "${tmp_dir}"' EXIT

failed=0
failed_targets=()

for entry in "${TARGETS[@]}"; do
  pkg="${entry%%|*}"
  target="${entry##*|}"
  if ! run_target_with_retry "${pkg}" "${target}" "${tmp_dir}"; then
    failed=1
    failed_targets+=("${target}")
  fi
done

if (( failed )); then
  echo "Fuzz suite failed: ${failed_targets[*]}"
  exit 1
fi

echo "All fuzz targets passed (fuzztime=${FUZZTIME})"
