#!/usr/bin/env bash
# Starts the disposable supported #96/#102 topology and prints only non-secret access facts.
# Credentials are generated at runtime and written 0600 beneath the ignored artifact tree.
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
runtime_root=${QUOIN_E2E_RUNTIME:-"${repo_root}/.artifacts/e2e-97"}
port=${QUOIN_E2E_PORT:-8445}
origin="https://localhost:${port}"
# Keep #97 separate even when invoked directly rather than through Make.
if [[ "$runtime_root" == "${repo_root}/.artifacts/e2e-97" ]]; then export QUOIN_E2E_PROJECT=${QUOIN_E2E_PROJECT:-quoin-e2e-97}; fi
compose=(docker compose --project-directory "${repo_root}" --env-file /dev/null -f "${repo_root}/deploy/e2e-real.compose.yaml")

require_command() {
	command -v "$1" >/dev/null || {
		printf 'Required command is unavailable: %s\n' "$1" >&2
		exit 1
	}
}
require_command docker
require_command go
require_command openssl

if [[ -f "${runtime_root}/credentials.yaml" ]]; then
	# A running environment is intentionally reused to make browser hand-debugging stable.
	# Repair only the generated #97 fixture-secret boundary on retained runs;
	# deployment identities remain owned by the container runtime and untouched.
	for fixture_secret in metrics-basic-password metrics-bearer-token; do
		if [[ -f "${runtime_root}/secrets/${fixture_secret}" ]]; then
			install -d -m 700 "${runtime_root}/fixture/secrets"
			sudo install -m 600 -o "$(id -u)" -g "$(id -g)" "${runtime_root}/secrets/${fixture_secret}" "${runtime_root}/fixture/secrets/${fixture_secret}"
		fi
	done
	# This distinct self-signed endpoint is a test upstream, not the public
	# gateway certificate. Its DNS SAN must match the Compose service name so both
	# the production adapter and the counter reader verify TLS normally.
	if [[ ! -s "${runtime_root}/fixture/prometheus-tls.crt" || ! -s "${runtime_root}/fixture/prometheus-tls.key" ]] || ! openssl x509 -in "${runtime_root}/fixture/prometheus-tls.crt" -noout -ext subjectAltName 2>/dev/null | grep -q 'DNS:prometheus-tls-fixture'; then
		openssl req -x509 -newkey rsa:2048 -nodes -days 2 \
			-keyout "${runtime_root}/fixture/prometheus-tls.key" \
			-out "${runtime_root}/fixture/prometheus-tls.crt" \
			-subj /CN=prometheus-tls-fixture \
			-addext 'subjectAltName=DNS:prometheus-tls-fixture' >/dev/null 2>&1
		chmod 640 "${runtime_root}/fixture/prometheus-tls.key"
		chmod 644 "${runtime_root}/fixture/prometheus-tls.crt"
	fi
	if "${compose[@]}" ps --status running --services 2>/dev/null | grep -qx gateway; then
		printf 'QUOIN_E2E_URL=%s\nQUOIN_E2E_RUNTIME=%s\n' "$origin" "$runtime_root"
		exit 0
	fi
fi

if [[ -e "${runtime_root}" && ! -f "${runtime_root}/credentials.yaml" ]]; then
	printf 'Refusing to use unrecognized runtime directory: %s\nRun scripts/e2e-real/down.sh --purge or set QUOIN_E2E_RUNTIME.\n' "$runtime_root" >&2
	exit 1
fi

umask 077
mkdir -p "${runtime_root}"/{config,data,backups,secrets,gateway/tls,logs,plinth-state,plinth-workspaces,fixture/secrets}
# #97 fixture credentials do not belong in the deployment secret mount: Quoin
# bootstrap changes that mount to the runtime UID. Keep this user-owned 0600
# fixture-only directory readable by Playwright and mount it read-only only to
# fixture containers. Quoin receives the values exclusively in public API calls.
for fixture_secret in metrics-basic-password metrics-bearer-token; do
	if [[ ! -s "${runtime_root}/fixture/secrets/${fixture_secret}" ]]; then
		openssl rand -base64 36 | tr -d '\n' >"${runtime_root}/fixture/secrets/${fixture_secret}"
	fi
	chmod 600 "${runtime_root}/fixture/secrets/${fixture_secret}"
done
# Only deployment credentials are private. Containerized services must traverse
# generated config/gateway paths; their contents deliberately contain no secrets.
chmod 755 "${runtime_root}" "${runtime_root}/config" "${runtime_root}/data" "${runtime_root}/backups" "${runtime_root}/gateway" "${runtime_root}/gateway/tls" "${runtime_root}/fixture"
install -m 755 "${repo_root}/scripts/e2e-real/metrics-fixture.py" "${runtime_root}/fixture/metrics-fixture.py"
chmod 700 "${runtime_root}/plinth-state" "${runtime_root}/plinth-workspaces"
chmod 700 "${runtime_root}/secrets"

cat >"${runtime_root}/config/quoin.yaml" <<EOF
component: quoin
publicOrigin: ${origin}
stelePublicURL: ${origin}/stele/alerts
dataDirectory: /var/lib/quoin/data
backupDirectory: /var/lib/quoin/backups
rootKeyFile: /run/quoin-secrets/root-key
runtimeTlsCertificateFile: /run/quoin-secrets/runtime-tls.crt
runtimeTlsPrivateKeyFile: /run/quoin-secrets/runtime-tls.key
steleServiceTokenFile: /run/quoin-secrets/stele-service-token
EOF
cat >"${runtime_root}/config/stele.yaml" <<'EOF'
component: stele
quoinRuntimeEndpoint: https://quoin:8443
quoinRuntimeCaFile: /run/quoin-secrets/runtime-ca.pem
serviceTokenFile: /run/quoin-secrets/stele-service-token
EOF
cat >"${runtime_root}/config/plinth.yaml" <<'EOF'
component: plinth
stateDirectory: /var/lib/plinth
workspaceDirectory: /var/lib/plinth/workspaces
quoinRuntimeEndpoint: https://quoin:8443
quoinRuntimeCaFile: /run/quoin-secrets/runtime-ca.pem
EOF
cat >"${runtime_root}/gateway/Caddyfile" <<'EOF'
:8443 {
  tls /etc/caddy/tls/tls.crt /etc/caddy/tls/tls.key
  handle /api/* {
    reverse_proxy quoin:8080
  }
  handle_path /stele/* {
    reverse_proxy stele:8080
  }
  handle {
    reverse_proxy frontend:8080
  }
}
EOF
if [[ ! -s "${runtime_root}/gateway/tls/tls.crt" ]]; then
	openssl req -x509 -newkey rsa:2048 -nodes -days 2 \
		-keyout "${runtime_root}/gateway/tls/tls.key" \
		-out "${runtime_root}/gateway/tls/tls.crt" \
		-subj /CN=localhost \
		-addext 'subjectAltName=DNS:localhost,IP:127.0.0.1' >/dev/null 2>&1
fi
# The no-auth Prometheus fixture has a dedicated DNS-SAN certificate. Its CA
# chain is passed through the normal connection tlsCaPem field; no skip-verify
# exception is used in either the production adapter or counter-reader path.
if [[ ! -s "${runtime_root}/fixture/prometheus-tls.crt" || ! -s "${runtime_root}/fixture/prometheus-tls.key" ]]; then
	openssl req -x509 -newkey rsa:2048 -nodes -days 2 \
		-keyout "${runtime_root}/fixture/prometheus-tls.key" \
		-out "${runtime_root}/fixture/prometheus-tls.crt" \
		-subj /CN=prometheus-tls-fixture \
		-addext 'subjectAltName=DNS:prometheus-tls-fixture' >/dev/null 2>&1
fi
chmod 644 "${runtime_root}/config"/*.yaml "${runtime_root}/gateway/Caddyfile" "${runtime_root}/gateway/tls/tls.crt" "${runtime_root}/fixture/prometheus-tls.crt"
chmod 640 "${runtime_root}/gateway/tls/tls.key" "${runtime_root}/fixture/prometheus-tls.key"

# Build the real visible services and the unmodified production Plinth adapter.
# Lintel stays out because #102 only proves the Plinth lifecycle.
QUOIN_IMAGE_COMPONENTS=frontend,quoin,plinth,stele bash "${repo_root}/deploy/images/build.sh"
export QUOIN_E2E_RUNTIME="$runtime_root" QUOIN_E2E_PORT="$port"
# #97 always uses its own Compose project, so it cannot reuse or disturb an
# existing #96/#102/manual environment unless a caller explicitly overrides it.
if [[ "$runtime_root" == "${repo_root}/.artifacts/e2e-97" ]]; then export QUOIN_E2E_PROJECT=quoin-e2e-97; fi

# Bootstrap deployment secrets and the first administrator before the Quoin
# server opens its exclusive data-directory lock. Both steps use the supported
# product CLI inside the production image, never SQL or an application backdoor.
# The bootstrap container receives the only writable secret mount. Long-lived
# Quoin and Stele retain their read-only deployment secret mounts from Compose.
docker run --rm --user 0:0 \
	--mount "type=bind,source=${runtime_root}/config/quoin.yaml,target=/etc/quoin/component.yaml,readonly" \
	--mount "type=bind,source=${runtime_root}/secrets,target=/run/quoin-secrets" \
	"${QUOIN_IMAGE:-quoin/quoin:v0.1.0-dev}" secrets bootstrap --config /etc/quoin/component.yaml
chmod 700 "${runtime_root}/secrets"
sudo chmod 600 "${runtime_root}/secrets"/*
sudo chown -R 65532:65532 "${runtime_root}/data" "${runtime_root}/backups" "${runtime_root}/secrets" "${runtime_root}/plinth-state" "${runtime_root}/plinth-workspaces"
admin_user="admin96"
admin_password=$(openssl rand -base64 36 | tr -d '\n' | cut -c1-32)
admin_final_password=$(openssl rand -base64 36 | tr -d '\n' | cut -c1-32)

# The CLI requires a real terminal. This PTY driver waits for each prompt before
# writing input and never records or mirrors terminal output, avoiding password
# echo/transcripts while retaining the supported interactive command.
REPO_ROOT="$repo_root" COMPOSE_FILE="${repo_root}/deploy/e2e-real.compose.yaml" \
	ADMIN_USER="$admin_user" ADMIN_PASSWORD="$admin_password" python3 - <<'PY'
import os
import pty
import select
import subprocess
import sys

command = ["docker", "compose", "--project-directory", os.environ["REPO_ROOT"], "--env-file", "/dev/null", "-f", os.environ["COMPOSE_FILE"], "run", "--rm", "--no-deps", "quoin", "admin", "create", "--config", "/etc/quoin/component.yaml"]
pid, fd = pty.fork()
if pid == 0:
    os.execvp(command[0], command)
prompts = [(b"Username: ", os.environ["ADMIN_USER"]), (b"Display name: ", "E2E Administrator"), (b"Temporary password: ", os.environ["ADMIN_PASSWORD"]), (b"Confirm temporary password: ", os.environ["ADMIN_PASSWORD"])]
buffer = b""
for prompt, answer in prompts:
    while prompt not in buffer:
        ready, _, _ = select.select([fd], [], [], 60)
        if not ready:
            os.kill(pid, 15)
            raise SystemExit("timed out waiting for supported admin CLI prompt")
        try:
            chunk = os.read(fd, 1024)
        except OSError:
            chunk = b""
        if not chunk:
            raise SystemExit("admin CLI ended before all prompts")
        buffer = (buffer + chunk)[-4096:]
    os.write(fd, answer.encode() + b"\n")
_, status = os.waitpid(pid, 0)
if not os.WIFEXITED(status) or os.WEXITSTATUS(status) != 0:
    raise SystemExit("supported admin bootstrap command failed")
PY

cat >"${runtime_root}/credentials.yaml" <<EOF
url: ${origin}
admin:
  username: ${admin_user}
  temporaryPassword: ${admin_password}
  password: ${admin_final_password}
EOF
chmod 600 "${runtime_root}/credentials.yaml"
"${compose[@]}" up -d --wait --wait-timeout 90
printf 'QUOIN_E2E_URL=%s\nQUOIN_E2E_RUNTIME=%s\nQUOIN_E2E_CREDENTIALS_FILE=%s\nQUOIN_E2E_METRICS_FIXTURE_PROMETHEUS=http://prometheus-fixture:8080\nQUOIN_E2E_METRICS_FIXTURE_THANOS=http://thanos-fixture:8080\n' "$origin" "$runtime_root" "${runtime_root}/credentials.yaml"
