#!/usr/bin/env bash
# Stops the disposable #96/#102 environment. --purge also deletes generated users,
# database, TLS material, and local-only credentials.
set -euo pipefail
repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
runtime_root=${QUOIN_E2E_RUNTIME:-"${repo_root}/.artifacts/e2e-97"}
compose=(docker compose --project-directory "${repo_root}" --env-file /dev/null -f "${repo_root}/deploy/e2e-real.compose.yaml")
export QUOIN_E2E_RUNTIME="$runtime_root" QUOIN_E2E_PORT="${QUOIN_E2E_PORT:-8445}"
if [[ "$runtime_root" == "${repo_root}/.artifacts/e2e-97" ]]; then export QUOIN_E2E_PROJECT=quoin-e2e-97; fi
"${compose[@]}" down --remove-orphans
if [[ "${1:-}" == "--purge" ]]; then
  rm -rf -- "${runtime_root}"
fi
