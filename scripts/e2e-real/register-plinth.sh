#!/usr/bin/env bash
# Consumes a freshly revealed #102 registration token through the production
# Plinth CLI. The raw token only crosses attached stdin and is never echoed,
# written to disk, passed as an argv/env value, or placed in Compose YAML.
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
runtime_root=${QUOIN_E2E_RUNTIME:-"${repo_root}/.artifacts/e2e-102"}
export QUOIN_E2E_RUNTIME="$runtime_root"
export QUOIN_E2E_PORT="${QUOIN_E2E_PORT:-8444}"
credentials_file="${runtime_root}/credentials.yaml"
compose=(docker compose --project-directory "${repo_root}" --env-file /dev/null -f "${repo_root}/deploy/e2e-real.compose.yaml")

[[ -f "$credentials_file" ]] || { printf 'Start the disposable environment first: scripts/e2e-real/up.sh\n' >&2; exit 1; }
[[ -f "${runtime_root}/config/plinth.yaml" ]] || { printf 'The selected runtime has no #102 Plinth configuration: %s\n' "$runtime_root" >&2; exit 1; }
if [[ "${runtime_root}" == "${repo_root}/.artifacts/e2e-102" ]]; then export QUOIN_E2E_PROJECT=quoin-e2e-102; fi
if [[ -f "${runtime_root}/plinth-state/runtime-token.json" ]]; then
  printf 'Plinth is already registered in this disposable runtime. Use the About UI replacement flow, then clear only its disposable state after stopping Plinth.\n' >&2
  exit 1
fi

# The default interactive read prevents shell history and terminal echo. The
# `--stdin` form is exclusively for the local Playwright process: it also takes
# the token through stdin, never command arguments, environment, or files.
case "${1:-}" in
  "") printf 'Paste the one-time Plinth registration token revealed by Admin About: ' >&2; IFS= read -r -s registration_token; printf '\n' >&2 ;;
  --stdin) IFS= read -r registration_token ;;
  *) printf 'usage: %s [--stdin]\n' "$0" >&2; exit 2 ;;
esac
[[ -n "$registration_token" ]] || { printf 'Registration token is required.\n' >&2; exit 1; }

payload=$(python3 - "$registration_token" <<'PYTHON'
import json, sys
print(json.dumps({"slot": "plinth", "generation": 1, "token": sys.argv[1]}, separators=(",", ":")))
PYTHON
)
# `exec -T` retains stdin while preventing the adapter from observing a TTY;
# RunRegister intentionally requires only its secure attached stdin payload.
printf '%s\n' "$payload" | "${compose[@]}" exec -T plinth /plinth register --config /etc/quoin/component.yaml >/dev/null
unset registration_token payload
printf 'Plinth registered through the production adapter. Wait for About to show connected.\n'
