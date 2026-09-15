#!/usr/bin/env bash
# Starts the disposable supported e2e-real topology and prints only non-secret
# access facts. No administrator is created here: first Quoin startup auto-seeds
# the pending built-in administrator; the browser initialization runs later
# through the real web frontend using the public default credentials directly.
# Test credentials are generated at runtime and written 0600 beneath the
# ignored artifact tree.
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
runtime_root=${QUOIN_E2E_RUNTIME:-"${repo_root}/.artifacts/e2e-97"}
port=${QUOIN_E2E_PORT:-8445}
origin="https://localhost:${port}"
# Keep #97 separate even when invoked directly rather than through Make.
if [[ "$runtime_root" == "${repo_root}/.artifacts/e2e-97" ]]; then export QUOIN_E2E_PROJECT=${QUOIN_E2E_PROJECT:-quoin-e2e-97}; fi
# The OTP receiver runs under the invoking user so its 0600 delivery record
# stays owned and readable by the browser harness on the host.
export QUOIN_E2E_OTP_UID=${QUOIN_E2E_OTP_UID:-$(id -u)} QUOIN_E2E_OTP_GID=${QUOIN_E2E_OTP_GID:-$(id -g)}
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

# ensure_otp_fixture prepares the test OTP receiver's TLS identity and record
# directory idempotently. The certificate is a dedicated self-signed test
# certificate whose DNS SAN matches the Compose service name, so Quoin's
# delivery senders validate TLS normally against the mounted CA copy; no
# skip-verify exception exists in the product. The receiver runs as the
# invoking user, so its TLS key and record directory are owned by that user
# and the 0600 record stays readable by the harness.
ensure_otp_fixture() {
	install -d -m 700 "${runtime_root}/fixture/otp-tls" "${runtime_root}/fixture/otp"
	if [[ ! -s "${runtime_root}/fixture/otp-tls/tls.crt" || ! -s "${runtime_root}/fixture/otp-tls/tls.key" ]] || ! openssl x509 -in "${runtime_root}/fixture/otp-tls/tls.crt" -noout -ext subjectAltName 2>/dev/null | grep -q 'DNS:otpdelivery'; then
		openssl req -x509 -newkey rsa:2048 -nodes -days 2 \
			-keyout "${runtime_root}/fixture/otp-tls/tls.key" \
			-out "${runtime_root}/fixture/otp-tls/tls.crt" \
			-subj /CN=otpdelivery \
			-addext 'subjectAltName=DNS:otpdelivery' >/dev/null 2>&1
	fi
	sudo chown "$(id -u):$(id -g)" "${runtime_root}/fixture/otp-tls/tls.key" "${runtime_root}/fixture/otp-tls/tls.crt" "${runtime_root}/fixture/otp"
	chmod 640 "${runtime_root}/fixture/otp-tls/tls.key"
	chmod 644 "${runtime_root}/fixture/otp-tls/tls.crt"
	chmod 700 "${runtime_root}/fixture/otp"
}

if [[ -f "${runtime_root}/credentials.yaml" ]]; then
	# A running environment is intentionally reused to make browser hand-debugging stable.
	# Repair only the generated fixture-secret boundary on retained runs;
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
	ensure_otp_fixture
	if "${compose[@]}" ps --status running --services 2>/dev/null | grep -qx gateway; then
		printf 'QUOIN_E2E_URL=%s\nQUOIN_E2E_RUNTIME=%s\nQUOIN_E2E_CREDENTIALS_FILE=%s\nQUOIN_E2E_OTP_RECORD_FILE=%s\nQUOIN_E2E_OTP_CA_FILE=%s\n' \
			"$origin" "$runtime_root" "${runtime_root}/credentials.yaml" \
			"${runtime_root}/fixture/otp/deliveries.jsonl" "${runtime_root}/fixture/otp-tls/tls.crt"
		exit 0
	fi
fi

if [[ -e "${runtime_root}" && ! -f "${runtime_root}/credentials.yaml" ]]; then
	printf 'Refusing to use unrecognized runtime directory: %s\nRun scripts/e2e-real/down.sh --purge or set QUOIN_E2E_RUNTIME.\n' "$runtime_root" >&2
	exit 1
fi

umask 077
mkdir -p "${runtime_root}"/{config,data,backups,secrets,gateway/tls,logs,plinth-state,plinth-workspaces,fixture/secrets}
# Fixture credentials do not belong in the deployment secret mount: Quoin
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
ensure_otp_fixture

# Build the real visible services, the unmodified production Plinth adapter and
# the test-only OTP receiver. Lintel stays out because #102 only proves the
# Plinth lifecycle.
QUOIN_IMAGE_COMPONENTS=frontend,quoin,plinth,stele,otp-test bash "${repo_root}/deploy/images/build.sh"
export QUOIN_E2E_RUNTIME="$runtime_root" QUOIN_E2E_PORT="$port"
# #97 always uses its own Compose project, so it cannot reuse or disturb an
# existing #96/#102/manual environment unless a caller explicitly overrides it.
if [[ "$runtime_root" == "${repo_root}/.artifacts/e2e-97" ]]; then export QUOIN_E2E_PROJECT=quoin-e2e-97; fi

# Bootstrap deployment secrets before the Quoin server opens its exclusive
# data-directory lock, using the supported product CLI inside the production
# image. The bootstrap container receives the only writable secret mount;
# long-lived Quoin and Stele retain their read-only deployment secret mounts.
docker run --rm --user 0:0 \
	--mount "type=bind,source=${runtime_root}/config/quoin.yaml,target=/etc/quoin/component.yaml,readonly" \
	--mount "type=bind,source=${runtime_root}/secrets,target=/run/quoin-secrets" \
	"${QUOIN_IMAGE:-quoin/quoin:v0.1.0-dev}" secrets bootstrap --config /etc/quoin/component.yaml
chmod 700 "${runtime_root}/secrets"
sudo chmod 600 "${runtime_root}/secrets"/*
sudo chown -R 65532:65532 "${runtime_root}/data" "${runtime_root}/backups" "${runtime_root}/secrets" "${runtime_root}/plinth-state" "${runtime_root}/plinth-workspaces"

# First startup seeds the pending built-in administrator (default credentials
# admin/admin, unusable beyond initialization). No administrator is created by
# this script and no install credential exists: the browser drives the unified
# initialization wizard directly with the default credentials.
"${compose[@]}" up -d --wait --wait-timeout 90

# Materialize the 0600 credentials file for the browser harness. The only
# generated secret values are the test-only replacement password and the SMTP
# AUTH value the fixture accepts; the initialization password is the public
# default. Nothing is echoed to logs or the environment. On a retained run the
# existing credentials file stays authoritative untouched.
if [[ ! -s "${runtime_root}/credentials.yaml" ]]; then
	new_password=$(openssl rand -base64 36 | tr -d '\n' | cut -c1-32)
	smtp_password=$(openssl rand -base64 24 | tr -d '\n' | cut -c1-24)
	cat >"${runtime_root}/credentials.yaml" <<EOF
url: ${origin}
username: admin
defaultPassword: admin
newPassword: ${new_password}
contactEmail: admin@quoin.test
smtpHost: otpdelivery
smtpPort: "8587"
smtpUsername: quoin-e2e
smtpFrom: quoin-e2e@quoin.test
smtpPassword: ${smtp_password}
webhookUrl: https://otpdelivery:8445/
otpCaCertFile: ${runtime_root}/fixture/otp-tls/tls.crt
otpRecordFile: ${runtime_root}/fixture/otp/deliveries.jsonl
EOF
	chmod 600 "${runtime_root}/credentials.yaml"
fi
printf 'QUOIN_E2E_URL=%s\nQUOIN_E2E_RUNTIME=%s\nQUOIN_E2E_CREDENTIALS_FILE=%s\nQUOIN_E2E_OTP_RECORD_FILE=%s\nQUOIN_E2E_OTP_CA_FILE=%s\nQUOIN_E2E_METRICS_FIXTURE_PROMETHEUS=http://prometheus-fixture:8080\nQUOIN_E2E_METRICS_FIXTURE_THANOS=http://thanos-fixture:8080\n' \
	"$origin" "$runtime_root" "${runtime_root}/credentials.yaml" \
	"${runtime_root}/fixture/otp/deliveries.jsonl" "${runtime_root}/fixture/otp-tls/tls.crt"
