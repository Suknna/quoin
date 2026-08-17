#!/usr/bin/env bash
# Playwright webServer bootstrap: builds the four local images if missing,
# performs the real scripted compose install (attached-TTY first admin over a
# pty), then stays attached to the long-lived services so Playwright can own
# the lifecycle. Teardown happens in teardown.mjs.
set -euo pipefail
repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)
cd "$repo_root"

stack="$repo_root/.artifacts/e2e-stack"
evidence="${QUOIN_EVIDENCE_DIR:-$repo_root/.artifacts/tickets/T01}"
rm -rf "$stack"
mkdir -p "$stack" "$evidence"

bash build/package/images.sh
go build -o "$stack/quoin-deploy" ./cmd/quoin-deploy

password="e2e-$(openssl rand -base64 24 | tr -d '/+=' | cut -c1-24)-2026"
printf '%s' "$password" > "$stack/admin-temp-password"
chmod 600 "$stack/admin-temp-password"

sed "s|REPLACE_WITH_STACK_DIR|$stack|" test/e2e/compose/compose-install.yaml > "$stack/install.yaml"

printf 'admin\nE2E Admin\n%s\n%s\n' "$password" "$password" | \
  XDG_STATE_HOME="$stack/state" "$stack/quoin-deploy" compose install --config "$stack/install.yaml" \
  2>&1 | tee "$evidence/playwright-server.log"

exec docker compose --project-name quoin --file "$stack/state/quoin/compose/generated/compose.yaml" logs -f quoin plinth lintel stele
