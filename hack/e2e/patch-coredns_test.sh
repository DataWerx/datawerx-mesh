#!/usr/bin/env bash
# Unit tests for the pure Corefile-rewrite helpers in patch-coredns.sh.
# No cluster required. Run: bash hack/e2e/patch-coredns_test.sh
set -euo pipefail

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=/dev/null
source "${HERE}/patch-coredns.sh"

fail=0
check() { # check <name> <expected> <actual>
  if [[ "$2" == "$3" ]]; then
    echo "ok   - $1"
  else
    echo "FAIL - $1"
    echo "  expected: |$2|"
    echo "  actual:   |$3|"
    fail=1
  fi
}

BASE='.:53 {
    errors
    forward . /etc/resolv.conf
}'

STALE="${BASE}
clusterset.local:53 {
    errors
    cache 5
    forward . 10.96.0.99
}"

# 1. No existing block -> not current.
if corefile_is_current "${BASE}" "10.96.0.42"; then
  echo "FAIL - is_current should be false when no zone present"; fail=1
else
  echo "ok   - is_current false when zone absent"
fi

# 2. Stale IP present -> not current (the core bug this fixes).
if corefile_is_current "${STALE}" "10.96.0.42"; then
  echo "FAIL - is_current should be false when the forward IP is stale"; fail=1
else
  echo "ok   - is_current false on stale IP"
fi

# 3. Matching IP present -> current (idempotent skip).
if corefile_is_current "${STALE}" "10.96.0.99"; then
  echo "ok   - is_current true when IP matches"
else
  echo "FAIL - is_current should be true when the forward IP matches"; fail=1
fi

# 4. ensure_zone rewrites a stale block to the new IP and keeps the base block.
OUT=$(corefile_ensure_zone "${STALE}" "10.96.0.42")
grep -qF "forward . 10.96.0.42" <<<"${OUT}" || { echo "FAIL - new IP missing"; fail=1; }
grep -qF "forward . 10.96.0.99" <<<"${OUT}" && { echo "FAIL - stale IP still present"; fail=1; } || true
grep -qF "forward . /etc/resolv.conf" <<<"${OUT}" || { echo "FAIL - base block dropped"; fail=1; }
# Exactly one clusterset.local block after rewrite.
n=$(grep -c "^clusterset.local:53 {" <<<"${OUT}")
check "exactly one clusterset.local block after rewrite" "1" "${n}"

# 5. ensure_zone is convergent: applying it to its own output keeps one block,
#    pointing at the new IP.
OUT2=$(corefile_ensure_zone "${OUT}" "10.96.0.42")
n2=$(grep -c "^clusterset.local:53 {" <<<"${OUT2}")
check "still one block after re-applying ensure_zone" "1" "${n2}"
if corefile_is_current "${OUT2}" "10.96.0.42"; then
  echo "ok   - output is_current for the new IP"
else
  echo "FAIL - rewritten Corefile is not current"; fail=1
fi

# 6. Adds a block when none existed, leaving the base intact.
OUT3=$(corefile_ensure_zone "${BASE}" "10.96.0.7")
grep -qF "forward . 10.96.0.7" <<<"${OUT3}" || { echo "FAIL - zone not added to base"; fail=1; }
grep -qF "forward . /etc/resolv.conf" <<<"${OUT3}" || { echo "FAIL - base block lost"; fail=1; }

if [[ "${fail}" -eq 0 ]]; then
  echo "PASS"
else
  echo "FAILED"
  exit 1
fi
