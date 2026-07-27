#!/usr/bin/env bash
# Unit tests for scripts/run-fuzz.sh failure classification.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
CLASSIFY=("$ROOT/scripts/run-fuzz.sh" --classify-log)
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

# Fixture: pure deadline flake from nightly #420
cat >"$tmpdir/deadline.log" <<'LOG'
fuzz: elapsed: 30s, execs: 141714 (4822/sec), new interesting: 0 (total: 13)
fuzz: elapsed: 30s, execs: 141714 (0/sec), new interesting: 0 (total: 13)
--- FAIL: FuzzRecommendationEngine (30.10s)
    context deadline exceeded
FAIL
exit status 1
FAILgithub.com/attune-io/attune/internal/recommendation	30.112s
LOG
got="$("${CLASSIFY[@]}" "$tmpdir/deadline.log")"
assert_eq "deadline_flake" "$got" "pure deadline flake"

# Fixture: real assertion failure with corpus write
cat >"$tmpdir/real_assert.log" <<'LOG'
--- FAIL: FuzzRecommendationEngine (0.12s)
    --- FAIL: FuzzRecommendationEngine (0.00s)
        fuzz_test.go:131: result 5m below minimum bound 10m
        failing input written to testdata/fuzz/FuzzRecommendationEngine/abc123
FAIL
LOG
got="$("${CLASSIFY[@]}" "$tmpdir/real_assert.log")"
assert_eq "real_failure" "$got" "assertion + failing input"

# Fixture: panic
cat >"$tmpdir/panic.log" <<'LOG'
--- FAIL: FuzzPercentileEstimator (1.00s)
panic: runtime error: integer divide by zero [recovered]
FAIL
LOG
got="$("${CLASSIFY[@]}" "$tmpdir/panic.log")"
assert_eq "real_failure" "$got" "panic is real"

# Fixture: deadline text PLUS real assert must stay real
cat >"$tmpdir/mixed.log" <<'LOG'
--- FAIL: FuzzRecommendationEngine (30.10s)
    fuzz_test.go:131: result 5m below minimum bound 10m
    context deadline exceeded
    failing input written to testdata/fuzz/FuzzRecommendationEngine/xyz
FAIL
LOG
got="$("${CLASSIFY[@]}" "$tmpdir/mixed.log")"
assert_eq "real_failure" "$got" "mixed deadline+assert is real"

# Fixture: empty log
: >"$tmpdir/empty.log"
got="$("${CLASSIFY[@]}" "$tmpdir/empty.log")"
assert_eq "real_failure" "$got" "empty log is real"

# Fixture: generic non-zero without deadline
cat >"$tmpdir/buildfail.log" <<'LOG'
# github.com/attune-io/attune/internal/recommendation [github.com/attune-io/attune/internal/recommendation.test]
internal/recommendation/engine.go:10:2: undefined: foo
FAIL	github.com/attune-io/attune/internal/recommendation [build failed]
LOG
got="$("${CLASSIFY[@]}" "$tmpdir/buildfail.log")"
assert_eq "real_failure" "$got" "build failure is real"

echo "All run-fuzz classifier tests passed"
