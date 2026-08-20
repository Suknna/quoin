#!/usr/bin/env bash
# Playwright webServer bootstrap: builds the four local images if missing,
# performs the real scripted compose install (attached-TTY first admin over a
# pty), then stays attached to the long-lived services so Playwright can own
# the lifecycle. Teardown happens in teardown.mjs.
set -euo pipefail
repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)
cd "$repo_root"

stack="$repo_root/.artifacts/e2e-stack"
# A failed prior webServer leaves owned fixtures behind (teardown.mjs only
# runs on successful startup); clear them before recreating.
docker rm -f e2e-fwd e2e-am quoin-t07-thanos >/dev/null 2>&1 || true
docker compose --project-name quoin down --remove-orphans >/dev/null 2>&1 || true
evidence="${QUOIN_EVIDENCE_DIR:-$repo_root/.artifacts/tickets/T03}"
rm -rf "$stack"
mkdir -p "$stack" "$evidence"

# Restricted networks need the module mirror passed into the image build
# (same authority as the Go acceptance runs: QUOIN_IMAGE_GOPROXY falling back
# to `go env GOPROXY`, whose value often lives only in the go env file).
image_proxy="${QUOIN_IMAGE_GOPROXY:-$(go env GOPROXY)}"
QUOIN_IMAGE_GOPROXY="$image_proxy" bash build/package/images.sh
go build -o "$stack/quoin-deploy" ./cmd/quoin-deploy

password="e2e-$(openssl rand -base64 24 | tr -d '/+=' | cut -c1-24)-2026"
printf '%s' "$password" > "$stack/admin-temp-password"
chmod 600 "$stack/admin-temp-password"

sed "s|REPLACE_WITH_STACK_DIR|$stack|" test/e2e/compose/compose-install.yaml > "$stack/install.yaml"

printf 'admin\nE2E Admin\n%s\n%s\n' "$password" "$password" | \
  XDG_STATE_HOME="$stack/state" "$stack/quoin-deploy" compose install --config "$stack/install.yaml" \
  2>&1 | tee "$evidence/playwright-server.log"
exec 2>>"$evidence/playwright-server.log"

# Drive the T03 alert: login, change password, create a source, reveal the
# bearer, and fire a real Alertmanager webhook so the UI shows T03Probe.
# No tracing: the admin/new passwords and the revealed bearer must never
# reach the evidence log (they only travel as curl -d bodies / env vars).
BASE=http://127.0.0.1:18080
ORIGIN='Origin: https://quoin.example.com'
# __Host-quoin-session is Secure; curl will not attach it over plain http, so
# carry the Set-Cookie value manually for the whole session.
LOGIN_HEADERS=$(mktemp)
curl -s -D "$LOGIN_HEADERS" -H "$ORIGIN" -H 'Content-Type: application/json' -X POST \
  -d "{\"username\":\"admin\",\"password\":\"$password\"}" "$BASE/api/v1/auth/login" >/dev/null
SESSION_COOKIE=$(awk 'tolower($1)=="set-cookie:" {print $2}' "$LOGIN_HEADERS" | tr -d '\r' | head -1)
: > "$stack/cj"
echo "$SESSION_COOKIE" > "$stack/session-cookie"
CJ="Cookie: $SESSION_COOKIE"
newpass="e2e-$(openssl rand -base64 18 | tr -d '/+=' | cut -c1-18)-2027"
curl -s -H "$CJ" -H "$ORIGIN" -H 'Content-Type: application/json' -X PUT \
  -d "{\"currentPassword\":\"$password\",\"newPassword\":\"$newpass\"}" "$BASE/api/v1/auth/password" \
  -o "$stack/put.json" -w 'change-password-http=%{http_code}\n' 2>&1 | tee -a "$evidence/playwright-server.log"
cat "$stack/put.json" >> "$evidence/playwright-server.log" 2>/dev/null || true
META=$(curl -s -H "$CJ" -H "$ORIGIN" -H 'Content-Type: application/json' -X POST \
  -d '{"key":"e2e-am","protocol":"alertmanager","clientCommandId":"e2e-cmd-0001"}' "$BASE/api/v1/alert-sources")
HANDLE=$(printf '%s' "$META" | python3 -c "import json,sys; print(json.load(sys.stdin)['revealHandle'])")
BEARER=$(curl -s -H "$CJ" -H "$ORIGIN" -H 'Content-Type: application/json' -X POST \
  -d "{\"revealHandle\":\"$HANDLE\"}" "$BASE/api/v1/alert-sources/credentials/reveal" | python3 -c "import json,sys; print(json.load(sys.stdin)['bearerToken'])")
# Forwarder + Alertmanager live on quoin_internal (plain Linux dockerd
# provides no host.docker.internal): the forwarder attaches the bearer and
# posts straight to stele:8080 (the same webhook listener Stele exposes
# inside the deployment network), and Alertmanager reaches the forwarder by
# container DNS name. Same approach as the T04 fixture.
mkdir -p "$stack/am"
cat > "$stack/am/forwarder.py" <<'PYEOF'
import http.server, urllib.request, urllib.error, os
class S(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        n = int(self.headers.get('Content-Length', 0))
        body = self.rfile.read(n)
        req = urllib.request.Request(os.environ['STELE_URL'], data=body, method='POST', headers={'Content-Type': self.headers.get('Content-Type','application/json'), 'Authorization': 'Bearer ' + os.environ['STELE_BEARER']})
        try:
            with urllib.request.urlopen(req, timeout=10) as resp:
                self.send_response(resp.status); self.end_headers()
        except urllib.error.HTTPError as e:
            self.send_response(e.code); self.end_headers()
        except Exception as e:
            self.send_response(502); self.end_headers()
    def log_message(self, *a): pass
http.server.HTTPServer(('0.0.0.0', 8099), S).serve_forever()
PYEOF
docker run -d --name e2e-fwd \
  -p 127.0.0.1:18082:8099 \
  -e "STELE_URL=http://stele:8080/" -e "STELE_BEARER=$BEARER" \
  -v "$stack/am/forwarder.py:/forwarder.py:ro" python:3.12-slim python /forwarder.py >/dev/null
docker network connect quoin_internal e2e-fwd
cat > "$stack/am/alertmanager.yml" <<'EOF'
route:
  receiver: sink
  group_wait: 0s
  group_interval: 1s
  repeat_interval: 1h
receivers:
- name: sink
  webhook_configs:
  - url: http://e2e-fwd:8099/
    send_resolved: true
EOF
docker run -d --name e2e-am -p 127.0.0.1:19093:9093 \
  -v "$stack/am/alertmanager.yml:/etc/alertmanager/alertmanager.yml:ro" prom/alertmanager:v0.28.1 >/dev/null
docker network connect quoin_internal e2e-am
# Wait for Alertmanager to be ready (first pull can take a while), then fire
# the probe alert; a failure here is loud (the fixture never flips to ready).
am_ready=0
for _ in $(seq 1 30); do
  if curl -sf http://127.0.0.1:19093/-/healthy >/dev/null 2>&1; then am_ready=1; break; fi
  sleep 1
done
if [ "$am_ready" != "1" ]; then
  { echo "FATAL: Alertmanager never became ready"; } | tee -a "$evidence/playwright-server.log" >&2
  exit 1
fi
if ! docker exec e2e-am amtool --alertmanager.url=http://127.0.0.1:9093 alert add alertname=T03Probe severity=critical instance=db-1 job=quoin; then
  { echo "FATAL: amtool could not fire T03Probe"; } | tee -a "$evidence/playwright-server.log" >&2
  exit 1
fi
# The ready fixture must not flip until the alert has actually reached the
# Quoin store: poll the authenticated snapshot until T03Probe is visible.
probe_seen=0
for _ in $(seq 1 30); do
  SNAPSHOT=$(curl -s -H "$CJ" -H "$ORIGIN" "$BASE/api/v1/alerts" 2>/dev/null || true)
  if printf '%s' "$SNAPSHOT" | grep -q T03Probe; then probe_seen=1; break; fi
  sleep 1
done
if [ "$probe_seen" != "1" ]; then
  { echo "FATAL: T03Probe never reached the Quoin alert store (last snapshot: ${SNAPSHOT:-none})"; } | tee -a "$evidence/playwright-server.log" >&2
  exit 1
fi
# --- T07 fixtures: a real Thanos target and a registered Plinth ----------
image_proxy2="${QUOIN_IMAGE_GOPROXY:-$(go env GOPROXY)}"
docker rm -f quoin-t07-thanos >/dev/null 2>&1 || true
NET=$(docker compose --project-name quoin --file "$stack/state/quoin/compose/generated/compose.yaml" ps -q quoin | xargs docker inspect --format '{{range $k, $v := .NetworkSettings.Networks}}{{$k}} {{end}}' | tr ' ' '\n' | grep 'quoin_internal$' | head -1)
docker run -d --name quoin-t07-thanos --network "$NET" thanosio/thanos:v0.36.0 query --http-address=0.0.0.0:9090 --log.level=warn >>"$evidence/playwright-server.log" 2>&1
# Register plinth so supervisor probes dispatch live (attached stdin keeps
# the token out of argv and logs).
PLINTH_ROW=$(curl -s -H "$CJ" -H "$ORIGIN" "$BASE/api/v1/runtime" | python3 -c 'import json,sys; print(int(json.load(sys.stdin)["plinth"]["rowVersion"]))')
PREP=$(curl -s -H "$CJ" -H "$ORIGIN" -H 'Content-Type: application/json' -X POST -d "{\"clientCommandId\":\"e2e-t07-prepare-$RANDOM\",\"expectedRowVersion\":$PLINTH_ROW}" "$BASE/api/v1/runtime-slots/plinth/registration/prepare")
HANDLE=$(printf '%s' "$PREP" | python3 -c 'import json,sys; print(json.load(sys.stdin)["registrationTokenHandle"])')
REVEAL=$(curl -s -H "$CJ" -H "$ORIGIN" -H 'Content-Type: application/json' -X POST -d "{\"registrationTokenHandle\":\"$HANDLE\"}" "$BASE/api/v1/runtime-slots/registration-token/reveal")
printf '%s\n' "$REVEAL" | python3 -c 'import json,sys; d=json.load(sys.stdin); print(json.dumps({"slot":d["slot"],"generation":d["generation"],"token":d["registrationToken"]}))' | docker compose --project-name quoin --file "$stack/state/quoin/compose/generated/compose.yaml" run --rm --no-deps -i -T plinth register --config /etc/quoin/component.yaml >>"$evidence/playwright-server.log" 2>&1
# Create the UI-visible connection (one-time secret in the request body only).
curl -s -H "$CJ" -H "$ORIGIN" -H 'Content-Type: application/json' -X POST \
  -d '{"clientCommandId":"e2e-t07-create-1","name":"main-thanos","connection":{"type":"thanos","baseUrl":"http://quoin-t07-thanos:9090","password":"e2e-thanos-secret"}}' \
  "$BASE/api/v1/connections" >>"$evidence/playwright-server.log"

# The fixture doubles as the ready marker: tests only run once every step
# (login/change/source/reveal/AM alert) has completed.
printf '%s' "$newpass" > "$stack/admin-new-password"
chmod 600 "$stack/admin-new-password"

set +x
# Readiness endpoint for Playwright's webServer probe: 200 only after every
# step (incl. the AM alert) is done.
cat > "$stack/ready.py" <<PYEOF
import http.server, os
class S(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        if os.path.exists('$stack/admin-new-password'):
            self.send_response(200); self.end_headers()
        else:
            self.send_response(503); self.end_headers()
    def log_message(self, *a): pass
http.server.HTTPServer(('0.0.0.0', 18083), S).serve_forever()
PYEOF
python3 "$stack/ready.py" &
exec docker compose --project-name quoin --file "$stack/state/quoin/compose/generated/compose.yaml" logs -f quoin plinth lintel stele
