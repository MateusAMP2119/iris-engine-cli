#!/bin/bash
# Orchestrator helper: block until PR $1's checks exist and finish, then merge.
# Exits nonzero (and does NOT merge) if any check fails.
set -euo pipefail
pr="$1"

# Wait for checks to register (gh prints "no checks reported" to stderr and exits 8).
for _ in $(seq 1 80); do
  out=$(gh pr checks "$pr" 2>/dev/null || true)
  if [ -n "$out" ]; then break; fi
  sleep 15
done
out=$(gh pr checks "$pr" 2>/dev/null || true)
if [ -z "$out" ]; then
  echo "NO_CHECKS_REGISTERED after 20m" >&2
  exit 1
fi

# Wait for all checks to leave pending.
while true; do
  out=$(gh pr checks "$pr" 2>/dev/null || true)
  case "$out" in *pending*) sleep 20 ;; *) break ;; esac
done

bad=$(gh pr checks "$pr" | awk -F'\t' '$2!="pass"{print $1" -> "$2}')
if [ -n "$bad" ]; then
  echo "CI_FAILING: $bad" >&2
  exit 1
fi

gh pr merge "$pr" --merge
echo "MERGED_$pr"
