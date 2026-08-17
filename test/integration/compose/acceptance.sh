#!/usr/bin/env bash
# Verbatim acceptance run for ticket #24 (T01). Preserves pipeline exit codes
# and writes all evidence under .artifacts/tickets/T01.
set -euo pipefail
repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)
cd "$repo_root"
EVIDENCE=.artifacts/tickets/T01
rm -rf "$EVIDENCE"
mkdir -p "$EVIDENCE"

run() {
  local name="$1"
  shift
  set +e
  "$@" 2>&1 | tee "$EVIDENCE/$name.log"
  local code=${PIPESTATUS[0]}
  set -e
  printf '%s\t%s\n' "$name" "$code" >> "$EVIDENCE/exit-codes.tsv"
  return "$code"
}

git status --short | tee "$EVIDENCE/git-status-before.txt"
run verify-contracts ./ci/verify-contracts
run go-test-all go test -json ./... -count=1
run go-vet go vet ./...
run ticket-acceptance env QUOIN_EVIDENCE_DIR="$PWD/$EVIDENCE" \
  go test -json ./... -run '^TestTicket01' -count=1

run pnpm-install pnpm --dir web install --frozen-lockfile
run web-typecheck pnpm --dir web typecheck
run web-lint pnpm --dir web lint
run web-test pnpm --dir web test
run web-build pnpm --dir web build
run playwright env QUOIN_EVIDENCE_DIR="$PWD/$EVIDENCE" \
  pnpm --dir web exec playwright test --grep '@ticket-01' --project=chromium

test -s "$EVIDENCE/runtime-evidence.json"
test -s "$EVIDENCE/cleanup.json"
git status --short | tee "$EVIDENCE/git-status-after.txt"
