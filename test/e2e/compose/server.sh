#!/usr/bin/env bash
# Playwright webServer bootstrap: builds the four local images if missing,
# performs the real scripted compose install (attached-TTY first admin over a
# pty), then stays attached to the long-lived services so Playwright can own
# the lifecycle. Teardown happens in teardown.mjs.
set -euo pipefail
repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)
cd "$repo_root"

ticket="${QUOIN_TICKET:-}"
# The exact ticket acceptance block passes only QUOIN_EVIDENCE_DIR to the
# Playwright leg; mirror the config's ticket inference so the webServer boots
# the matching isolated stack instead of the shared fixture.
if [ -z "$ticket" ] && [ -n "${QUOIN_EVIDENCE_DIR:-}" ]; then
  candidate=$(basename "$QUOIN_EVIDENCE_DIR")
  case "$candidate" in
    T[0-9][0-9]) ticket="$candidate" ;;
  esac
fi
browser_ticket=0
if grep -qxF "$ticket" test/e2e/browser-tickets.txt; then
  browser_ticket=1
fi
stack="$repo_root/.artifacts/e2e-stack"
compose_project="${QUOIN_COMPOSE_PROJECT:-quoin}"
browser_fixture=""
if [ "$browser_ticket" = "1" ]; then
  # Browser tickets are isolated from each other and the ordinary Compose fixture.
  ticket_slug=$(printf '%s' "$ticket" | tr '[:upper:]' '[:lower:]')
  stack="$repo_root/.artifacts/e2e-stack-$ticket_slug"
  compose_project="quoin-$ticket_slug"
  browser_fixture="quoin-$ticket_slug-auth-fixture"
  export QUOIN_IMAGE_NAMESPACE="quoin-$ticket_slug"
fi
export QUOIN_COMPOSE_PROJECT="$compose_project"
if [ "$browser_ticket" = "1" ] && [ "${QUOIN_BROWSER_E2E_LOCK_HELD:-}" != "1" ]; then
# Browser tickets use the same loopback TLS port and fixed browser fixture
# names. Serialize direct script invocations as well as Playwright runs so
  # one bootstrap cannot remove another's generated Compose projection.
  mkdir -p "$repo_root/.artifacts/tickets"
  exec 9>"$repo_root/.artifacts/tickets/.browser-e2e.lock"
  flock 9
fi
if [ "$browser_ticket" = "1" ]; then
  # Playwright only runs its global teardown after the webServer became ready.
  # A bootstrap failure happens before then, so it must synchronously clean the
  # isolated project, temporary credentials, private images and host helpers.
  cleanup_failed_browser_bootstrap() {
    status=$?
    trap - EXIT
    if [ "$status" -ne 0 ]; then
      if ! QUOIN_TICKET="$ticket" QUOIN_EVIDENCE_DIR="${QUOIN_EVIDENCE_DIR:-$repo_root/.artifacts/tickets/$ticket}" node test/e2e/compose/teardown.mjs; then
        echo "FATAL: $ticket browser bootstrap cleanup failed; inspect cleanup.json before retrying." >&2
        exit 70
      fi
    fi
    exit "$status"
  }
  trap cleanup_failed_browser_bootstrap EXIT
fi
# A failed prior webServer leaves owned fixtures behind (teardown.mjs only
# runs on successful startup); clear only this test's project. Browser tickets
# deliberately do not remove shared T03/T07 fixtures from another acceptance run.
if [ "$browser_ticket" != "1" ]; then
  docker rm -f e2e-fwd e2e-am quoin-t07-thanos >/dev/null 2>&1 || true
else
  docker rm -f "$browser_fixture" >/dev/null 2>&1 || true
  previous_compose="$stack/state/quoin/compose/generated/compose.yaml"
  if [ -f "$previous_compose" ]; then
    docker compose --project-name "$compose_project" --file "$previous_compose" down --volumes --remove-orphans >/dev/null
  fi
fi
if [ "$browser_ticket" != "1" ]; then
  docker compose --project-name "$compose_project" down --remove-orphans >/dev/null 2>&1 || true
fi
# The TLS proxy is a host-side test process, so it is not owned by Compose.
# Only terminate the process launched from this fixed test-stack path.
pkill -f "$stack/tls-proxy.mjs" >/dev/null 2>&1 || true
evidence="${QUOIN_EVIDENCE_DIR:-$repo_root/.artifacts/tickets/T03}"
rm -rf "$stack"
mkdir -p "$stack" "$evidence"

# Restricted networks need the module mirror passed into the image build
# (same authority as the Go acceptance runs: QUOIN_IMAGE_GOPROXY falling back
# to `go env GOPROXY`, whose value often lives only in the go env file).
image_proxy="${QUOIN_IMAGE_GOPROXY:-$(go env GOPROXY)}"
# Always rebuild: the web dist and Go sources may both have changed since
# the images were last built, and a stale image would test old code.
QUOIN_IMAGE_GOPROXY="$image_proxy" QUOIN_FORCE_IMAGE_BUILD=1 bash build/package/images.sh
go build -o "$stack/quoin-deploy" ./cmd/quoin-deploy

password="e2e-$(openssl rand -base64 24 | tr -d '/+=' | cut -c1-24)-2026"
printf '%s' "$password" > "$stack/admin-temp-password"
chmod 600 "$stack/admin-temp-password"

sed -e "s|REPLACE_WITH_STACK_DIR|$stack|" -e 's|https://quoin.example.com|https://127.0.0.1:18480|' test/e2e/compose/compose-install.yaml > "$stack/install.yaml"

printf 'admin\nE2E Admin\n%s\n%s\n' "$password" "$password" | \
  XDG_STATE_HOME="$stack/state" "$stack/quoin-deploy" compose install --config "$stack/install.yaml" \
  2>&1 | tee "$evidence/playwright-server.log"
exec 2>>"$evidence/playwright-server.log"

# Drive the T03 alert: login, change password, create a source, reveal the
# bearer, and fire a real Alertmanager webhook so the UI shows T03Probe.
# No tracing: the admin/new passwords and the revealed bearer must never
# reach the evidence log (they only travel as curl -d bodies / env vars).
BASE=http://127.0.0.1:18080
ORIGIN='Origin: https://127.0.0.1:18480'
# The public-origin contract is HTTPS and noVNC verifies WebSocket Origin.
# Terminate an ephemeral self-signed TLS edge for the browser E2E rather than
# weakening the production Origin check to accommodate plain-loopback tests.
openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj '/CN=127.0.0.1' \
  -addext 'subjectAltName=IP:127.0.0.1' -keyout "$stack/tls.key" -out "$stack/tls.crt" >/dev/null 2>&1
cat > "$stack/tls-proxy.mjs" <<'EOF'
import fs from 'node:fs'
import http from 'node:http'
import https from 'node:https'
import net from 'node:net'
const options = { key: fs.readFileSync(process.env.TLS_KEY), cert: fs.readFileSync(process.env.TLS_CERT) }
const target = { host: '127.0.0.1', port: 18080 }
const proxy = https.createServer(options, (request, response) => {
  const upstream = http.request({ ...target, method: request.method, path: request.url, headers: { ...request.headers, host: request.headers.host } }, (upstreamResponse) => {
    response.writeHead(upstreamResponse.statusCode, upstreamResponse.headers)
    upstreamResponse.pipe(response)
  })
  upstream.on('error', () => response.writeHead(502).end())
  request.pipe(upstream)
})
proxy.on('upgrade', (request, socket, head) => {
  const upstream = net.connect(target.port, target.host, () => {
    upstream.write(`${request.method} ${request.url} HTTP/${request.httpVersion}\r\n`)
    for (const [name, value] of Object.entries(request.headers)) upstream.write(`${name}: ${value}\r\n`)
    upstream.write('\r\n')
    if (head.length) upstream.write(head)
    socket.pipe(upstream).pipe(socket)
  })
  upstream.on('error', () => socket.destroy())
})
proxy.listen(18480, '127.0.0.1')
EOF
TLS_KEY="$stack/tls.key" TLS_CERT="$stack/tls.crt" node "$stack/tls-proxy.mjs" >>"$evidence/playwright-server.log" 2>&1 &
tls_proxy_pid=$!
printf '%s' "$tls_proxy_pid" > "$stack/tls-proxy.pid"
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
start_ready_server() {
  printf '%s' "$newpass" > "$stack/admin-new-password"
  chmod 600 "$stack/admin-new-password"
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
  printf '%s' "$!" > "$stack/ready.pid"
}
curl -s -H "$CJ" -H "$ORIGIN" -H 'Content-Type: application/json' -X PUT \
  -d "{\"currentPassword\":\"$password\",\"newPassword\":\"$newpass\"}" "$BASE/api/v1/auth/password" \
  -o "$stack/put.json" -w 'change-password-http=%{http_code}\n' 2>&1 | tee -a "$evidence/playwright-server.log"
cat "$stack/put.json" >> "$evidence/playwright-server.log" 2>/dev/null || true
# Changing a password revokes every existing Session, including CJ above. Log
# in again before the browser fixture provisions Runtime connections.
: > "$LOGIN_HEADERS"
curl -sf -D "$LOGIN_HEADERS" -H "$ORIGIN" -H 'Content-Type: application/json' -X POST \
  -d "{\"username\":\"admin\",\"password\":\"$newpass\"}" "$BASE/api/v1/auth/login" >/dev/null
SESSION_COOKIE=$(awk 'tolower($1)=="set-cookie:" {print $2}' "$LOGIN_HEADERS" | tr -d '\r' | head -1)
if [ -z "$SESSION_COOKIE" ]; then
  echo 'FATAL: login after required password change returned no session cookie' | tee -a "$evidence/playwright-server.log" >&2
  exit 1
fi
CJ="Cookie: $SESSION_COOKIE"

# Browser-ticket stacks intentionally provision only the Quoin/Lintel path.
# They leave alert/model/Thanos fixtures owned by other ticket suites alone.
if [ "$browser_ticket" = "1" ]; then
  cat > "$stack/browser-auth-fixture.py" <<'PYEOF'
import http.server

def increment(name):
    path = '/state/' + name
    try:
        value = int(open(path).read())
    except (FileNotFoundError, ValueError):
        value = 0
    open(path, 'w').write(str(value + 1) + '\n')

class S(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path == '/healthz':
            self.send_response(204); self.end_headers()
        elif self.path == '/authenticated' and 't20_auth=1' in self.headers.get('Cookie', ''):
            open('/state/authenticated-page', 'w').write('authenticated-page\n')
            self.send_response(200); self.end_headers(); self.wfile.write(b'authenticated')
        elif self.path == '/login' and 't20_auth=1' in self.headers.get('Cookie', ''):
            # A published profile revisits its frozen login URL. A real
            # authenticated endpoint redirects that request to its landing
            # page; without this, the fixture falsely declares the persisted
            # cookie unauthenticated during an Exploration admission probe.
            self.send_response(303); self.send_header('Location', '/authenticated'); self.end_headers()
        elif self.path == '/status' and 't20_auth=1' in self.headers.get('Cookie', ''):
            # The status-marker journey's happy target: a real authenticated
            # page whose marker element carries the typed output text.
            increment('status-seq')
            self.send_response(200); self.send_header('Content-Type', 'text/html'); self.end_headers()
            self.wfile.write(b'<!doctype html><html><body><main data-quoin-status="ok">SYSTEM OK</main></body></html>')
        elif self.path == '/status-broken' and 't20_auth=1' in self.headers.get('Cookie', ''):
            # The journey failure case: an authenticated page that never shows
            # the frozen selector, so the bounded wait ends in journey_failed.
            increment('status-broken-seq')
            self.send_response(200); self.send_header('Content-Type', 'text/html'); self.end_headers()
            self.wfile.write(b'<!doctype html><html><body><main>no marker here</main></body></html>')
        elif self.path.startswith('/ready'):
            increment('ready-seq')
            self.send_response(204); self.end_headers()
        elif self.path == '/complete':
            increment('submit-seq')
            open('/state/authenticated', 'w').write('authenticated\n')
            self.send_response(303); self.send_header('Set-Cookie', 't20_auth=1; Path=/; Max-Age=3600'); self.send_header('Location', '/authenticated'); self.end_headers()
        else:
            increment('login-get-seq')
            self.send_response(200); self.send_header('Content-Type', 'text/html'); self.end_headers()
            # This fixture records only event kinds/counts. It never records an
            # input value, key, pointer coordinate, cookie, or page content.
            self.wfile.write(b'<style>html,body,button{width:100%;height:100%;margin:0;border:0}button{font:32px sans-serif}</style><button type="button" autofocus>Press Enter to sign in</button><script>document.addEventListener("pointerdown",()=>navigator.sendBeacon("/input/pointer"));document.addEventListener("keydown",()=>navigator.sendBeacon("/input/key"));document.addEventListener("keyup",()=>location.assign("/complete"));fetch("/ready?"+Date.now(),{cache:"no-store"});</script>')
    def do_POST(self):
        if self.path == '/input/pointer':
            increment('pointerdown-seq'); self.send_response(204); self.end_headers(); return
        if self.path == '/input/key':
            # Record the actual RFB key.  Navigation occurs on the same RFB
            # press's key-up event, after the key-down input handoff completes;
            # the old `fetch(...).then(location.assign)` continuation could be
            # discarded after this request was already observed. /complete
            # remains the only authority that writes auth state/cookie.
            increment('keydown-seq'); self.send_response(204); self.end_headers(); return
        self.send_response(404); self.end_headers()
    def log_message(self, *a): pass
http.server.HTTPServer(('0.0.0.0', 8081), S).serve_forever()
PYEOF
  NET=$(docker compose --project-name "$compose_project" --file "$stack/state/quoin/compose/generated/compose.yaml" ps -q quoin | xargs docker inspect --format '{{range $k, $v := .NetworkSettings.Networks}}{{$k}} {{end}}' | tr ' ' '\n' | grep '_internal$' | head -1)
  mkdir -p "$stack/browser-auth-fixture-state"
  docker run -d --name "$browser_fixture" --network "$NET" --network-alias "$browser_fixture" -v "$stack/browser-auth-fixture.py:/app.py:ro" -v "$stack/browser-auth-fixture-state:/state" python:3.12-slim python /app.py >>"$evidence/playwright-server.log" 2>&1
  fixture_ready=0
  for _ in $(seq 1 30); do
    if docker exec "$browser_fixture" python -c 'import urllib.request; urllib.request.urlopen("http://127.0.0.1:8081/healthz", timeout=1)' >/dev/null 2>&1; then fixture_ready=1; break; fi
    sleep 1
  done
  if [ "$fixture_ready" != "1" ]; then
    echo "FATAL: $ticket authentication fixture did not become ready" | tee -a "$evidence/playwright-server.log" >&2
    exit 1
  fi
  LINTEL_ROW=$(curl -s -H "$CJ" -H "$ORIGIN" "$BASE/api/v1/runtime" | python3 -c 'import json,sys; print(int(json.load(sys.stdin)["lintel"]["rowVersion"]))')
  PREP=$(curl -s -H "$CJ" -H "$ORIGIN" -H 'Content-Type: application/json' -X POST -d "{\"clientCommandId\":\"e2e-t20-prepare-$RANDOM\",\"expectedRowVersion\":$LINTEL_ROW}" "$BASE/api/v1/runtime-slots/lintel/registration/prepare")
  HANDLE=$(printf '%s' "$PREP" | python3 -c 'import json,sys; print(json.load(sys.stdin)["registrationTokenHandle"])')
  REVEAL=$(curl -s -H "$CJ" -H "$ORIGIN" -H 'Content-Type: application/json' -X POST -d "{\"registrationTokenHandle\":\"$HANDLE\"}" "$BASE/api/v1/runtime-slots/registration-token/reveal")
  printf '%s\n' "$REVEAL" | python3 -c 'import json,sys; d=json.load(sys.stdin); print(json.dumps({"slot":d["slot"],"generation":d["generation"],"token":d["registrationToken"]}))' | docker compose --project-name "$compose_project" --file "$stack/state/quoin/compose/generated/compose.yaml" run --rm --no-deps -i -T lintel register --config /etc/quoin/component.yaml >>"$evidence/playwright-server.log" 2>&1
  lintel_connected=0
  for _ in $(seq 1 45); do
    if docker compose --project-name "$compose_project" --file "$stack/state/quoin/compose/generated/compose.yaml" logs lintel 2>&1 | grep -q 'runtime.connected'; then lintel_connected=1; break; fi
    sleep 1
  done
  if [ "$lintel_connected" != "1" ]; then
    docker compose --project-name "$compose_project" --file "$stack/state/quoin/compose/generated/compose.yaml" logs lintel >>"$evidence/playwright-server.log" 2>&1 || true
    echo 'FATAL: Lintel did not establish its Runtime control stream after registration' | tee -a "$evidence/playwright-server.log" >&2
    exit 1
  fi
  {
    echo 'lintel-image:'
    docker image inspect "${QUOIN_IMAGE_NAMESPACE:-quoin}/lintel:v0.1.0-dev" --format '{{.Id}}'
    echo 'chromium:'
    docker compose --project-name "$compose_project" --file "$stack/state/quoin/compose/generated/compose.yaml" exec -T lintel chromium --version
    echo 'tigervnc-scraping-server:'
    docker compose --project-name "$compose_project" --file "$stack/state/quoin/compose/generated/compose.yaml" exec -T lintel dpkg-query -W -f='${Version}\n' tigervnc-scraping-server
    echo 'xvfb:'
    docker compose --project-name "$compose_project" --file "$stack/state/quoin/compose/generated/compose.yaml" exec -T lintel dpkg-query -W -f='${Version}\n' xvfb
  } >"$evidence/${ticket,,}-components.log" 2>&1
  if [ "$ticket" = "T22" ] || [ "$ticket" = "T24" ]; then
    # T22 requires the real Plinth model turn in addition to the common
    # browser-ticket Runtime/Lintel path.
    go build -o "$stack/fixture-provider" ./test/fixtures/model-provider
    "$stack/fixture-provider" -address "0.0.0.0:18443" >"$evidence/fixture-provider.log" 2>&1 &
    printf '%s' "$!" >"$stack/fixture-provider.pid"
    if [ "$ticket" = "T24" ]; then
      go build -o "$stack/fixture-thanos" ./test/fixtures/thanos-query
      "$stack/fixture-thanos" -address "0.0.0.0:18444" >"$evidence/fixture-thanos.log" 2>&1 &
      printf '%s' "$!" >"$stack/fixture-thanos.pid"
    fi
    GW=$(docker compose --project-name "$compose_project" --file "$stack/state/quoin/compose/generated/compose.yaml" ps -q quoin | xargs docker inspect --format '{{range $k, $v := .NetworkSettings.Networks}}{{$k}} {{end}}' | tr ' ' '\n' | grep '_internal$' | head -1 | xargs docker network inspect --format '{{(index .IPAM.Config 0).Gateway}}')
    provider_name="t22-openai"
    if [ "$ticket" = "T24" ]; then provider_name="t24-openai"; fi
    curl -sf -H "$CJ" -H "$ORIGIN" -H 'Content-Type: application/json' -X POST \
      -d '{"clientCommandId":"e2e-'"${ticket,,}"'-create-provider","name":"'"$provider_name"'","connection":{"type":"model_provider","baseUrl":"http://'"$GW"':18443","chatModelId":"fixture-chat-1","embeddingModelId":"fixture-embed-1","contextBudgetTokens":8192,"maxOutputTokens":1024,"apiKey":"fixture-api-key-2026"}}' \
      "$BASE/api/v1/connections" >>"$evidence/playwright-server.log"
    if [ "$ticket" = "T24" ]; then
      curl -sf -H "$CJ" -H "$ORIGIN" -H 'Content-Type: application/json' -X POST \
        -d '{"clientCommandId":"e2e-t24-create-thanos","name":"t24-thanos","connection":{"type":"thanos","baseUrl":"http://'"$GW"':18444"}}' \
        "$BASE/api/v1/connections" >>"$evidence/playwright-server.log"
    fi
    PLINTH_ROW=$(curl -sf -H "$CJ" -H "$ORIGIN" "$BASE/api/v1/runtime" | python3 -c 'import json,sys; print(int(json.load(sys.stdin)["plinth"]["rowVersion"]))')
    PREP=$(curl -sf -H "$CJ" -H "$ORIGIN" -H 'Content-Type: application/json' -X POST -d "{\"clientCommandId\":\"e2e-t22-prepare-plinth-$RANDOM\",\"expectedRowVersion\":$PLINTH_ROW}" "$BASE/api/v1/runtime-slots/plinth/registration/prepare")
    HANDLE=$(printf '%s' "$PREP" | python3 -c 'import json,sys; print(json.load(sys.stdin)["registrationTokenHandle"])')
    REVEAL=$(curl -sf -H "$CJ" -H "$ORIGIN" -H 'Content-Type: application/json' -X POST -d "{\"registrationTokenHandle\":\"$HANDLE\"}" "$BASE/api/v1/runtime-slots/registration-token/reveal")
    printf '%s\n' "$REVEAL" | python3 -c 'import json,sys; d=json.load(sys.stdin); print(json.dumps({"slot":d["slot"],"generation":d["generation"],"token":d["registrationToken"]}))' | docker compose --project-name "$compose_project" --file "$stack/state/quoin/compose/generated/compose.yaml" run --rm --no-deps -i -T plinth register --config /etc/quoin/component.yaml >>"$evidence/playwright-server.log" 2>&1
    curl -sf -H "$CJ" -H "$ORIGIN" -H 'Content-Type: application/json' -X POST -d '{"clientCommandId":"e2e-'"${ticket,,}"'-probe-provider"}' "$BASE/api/v1/connections/$provider_name/probe" >>"$evidence/playwright-server.log"
    qualified=""
    for _ in $(seq 1 60); do
      qualified=$(curl -sf -H "$CJ" -H "$ORIGIN" "$BASE/api/v1/connections/$provider_name/probe-results" | python3 -c 'import json,sys; print(next((x["id"] for x in json.load(sys.stdin).get("items",[]) if x.get("outcome")=="passed"), ""))' 2>/dev/null || true)
      [ -n "$qualified" ] && break
      sleep 2
    done
    if [ -z "$qualified" ]; then
      echo 'FATAL: T22 model provider did not qualify' | tee -a "$evidence/playwright-server.log" >&2
      exit 1
    fi
    PROVIDER_ROW=$(curl -sf -H "$CJ" -H "$ORIGIN" "$BASE/api/v1/connections/$provider_name" | python3 -c 'import json,sys; print(json.load(sys.stdin)["rowVersion"])')
    curl -sf -H "$CJ" -H "$ORIGIN" -H 'Content-Type: application/json' -X POST -d "{\"clientCommandId\":\"e2e-${ticket,,}-enable-provider\",\"expectedRowVersion\":$PROVIDER_ROW,\"qualifiedProbeResultId\":\"$qualified\"}" "$BASE/api/v1/connections/$provider_name/enable" >>"$evidence/playwright-server.log"
    if [ "$ticket" = "T24" ]; then
      # Manual Inspection's PromQL child is admitted only through an enabled,
      # qualified deployment-global Thanos connection. Probe/qualify/enable it
      # through the same real Plinth Runtime path as the model provider.
      curl -sf -H "$CJ" -H "$ORIGIN" -H 'Content-Type: application/json' -X POST \
        -d '{"clientCommandId":"e2e-t24-probe-thanos"}' "$BASE/api/v1/connections/t24-thanos/probe" >>"$evidence/playwright-server.log"
      thanos_qualified=""
      for _ in $(seq 1 60); do
        thanos_qualified=$(curl -sf -H "$CJ" -H "$ORIGIN" "$BASE/api/v1/connections/t24-thanos/probe-results" | python3 -c 'import json,sys; print(next((x["id"] for x in json.load(sys.stdin).get("items",[]) if x.get("outcome")=="passed"), ""))' 2>/dev/null || true)
        [ -n "$thanos_qualified" ] && break
        sleep 2
      done
      if [ -z "$thanos_qualified" ]; then
        echo 'FATAL: T24 Thanos connection did not qualify' | tee -a "$evidence/playwright-server.log" >&2
        exit 1
      fi
      THANOS_ROW=$(curl -sf -H "$CJ" -H "$ORIGIN" "$BASE/api/v1/connections/t24-thanos" | python3 -c 'import json,sys; print(json.load(sys.stdin)["rowVersion"])')
      curl -sf -H "$CJ" -H "$ORIGIN" -H 'Content-Type: application/json' -X POST \
        -d "{\"clientCommandId\":\"e2e-t24-enable-thanos\",\"expectedRowVersion\":$THANOS_ROW,\"qualifiedProbeResultId\":\"$thanos_qualified\"}" "$BASE/api/v1/connections/t24-thanos/enable" >>"$evidence/playwright-server.log"
    fi
  fi
  start_ready_server
  if [ "${QUOIN_BROWSER_TEST_BOOTSTRAP_FAILURE:-}" = "1" ]; then
    if [ "${QUOIN_BROWSER_TEST_FAILURE_ARTIFACT:-}" = "1" ]; then
      mkdir -p "$stack/playwright-output"
      printf 'synthetic failure artifact for teardown verification\n' >"$stack/playwright-output/error-context.md"
    fi
    echo "intentional $ticket browser bootstrap failure after owned resources were created" >&2
    exit 97
  fi
# Every browser ticket stops here. Falling through into T03/T07/T10 fixture
# provisioning would mix an ordinary model provider and compose project into
# an isolated browser-runtime proof.
  exec docker compose --project-name "$compose_project" --file "$stack/state/quoin/compose/generated/compose.yaml" logs -f quoin plinth lintel stele 2>&1 | tee -a "$evidence/runtime-process.log"
fi

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
# --- T08 fixtures: the deterministic model provider + UI connections -----
pkill -f "fixtures/model-provider" >/dev/null 2>&1 || true
go build -o "$stack/fixture-provider" ./test/fixtures/model-provider
("$stack/fixture-provider" -address "0.0.0.0:18443" >"$evidence/fixture-provider.log" 2>&1 &)
GW2=$(docker compose --project-name "$compose_project" --file "$stack/state/quoin/compose/generated/compose.yaml" ps -q quoin | xargs docker inspect --format '{{range $k, $v := .NetworkSettings.Networks}}{{$k}} {{end}}' | tr ' ' '\n' | grep 'quoin_internal$' | head -1 | xargs docker network inspect --format '{{(index .IPAM.Config 0).Gateway}}')
curl -s -H "$CJ" -H "$ORIGIN" -H 'Content-Type: application/json' -X POST \
  -d '{"clientCommandId":"e2e-t08-create-1","name":"main-openai","connection":{"type":"model_provider","baseUrl":"http://'"$GW2"':18443","chatModelId":"fixture-chat-1","embeddingModelId":"fixture-embed-1","contextBudgetTokens":8192,"maxOutputTokens":1024,"apiKey":"fixture-api-key-2026"}}' \
  "$BASE/api/v1/connections" >>"$evidence/playwright-server.log"
curl -s -H "$CJ" -H "$ORIGIN" -H 'Content-Type: application/json' -X POST \
  -d '{"clientCommandId":"e2e-t08-create-2","name":"broken-openai","connection":{"type":"model_provider","baseUrl":"http://'"$GW2"':18443","chatModelId":"fixture-broken-stream","embeddingModelId":"fixture-embed-1","contextBudgetTokens":8192,"maxOutputTokens":1024,"apiKey":"fixture-api-key-2026"}}' \
  "$BASE/api/v1/connections" >>"$evidence/playwright-server.log"

# --- T07 fixtures: a real Thanos target and a registered Plinth ----------
docker rm -f quoin-t07-thanos >/dev/null 2>&1 || true
NET=$(docker compose --project-name "$compose_project" --file "$stack/state/quoin/compose/generated/compose.yaml" ps -q quoin | xargs docker inspect --format '{{range $k, $v := .NetworkSettings.Networks}}{{$k}} {{end}}' | tr ' ' '\n' | grep 'quoin_internal$' | head -1)
docker run -d --name quoin-t07-thanos --network "$NET" thanosio/thanos:v0.36.0 query --http-address=0.0.0.0:9090 --log.level=warn >>"$evidence/playwright-server.log" 2>&1
# Register plinth so supervisor probes dispatch live (attached stdin keeps
# the token out of argv and logs).
PLINTH_ROW=$(curl -s -H "$CJ" -H "$ORIGIN" "$BASE/api/v1/runtime" | python3 -c 'import json,sys; print(int(json.load(sys.stdin)["plinth"]["rowVersion"]))')
PREP=$(curl -s -H "$CJ" -H "$ORIGIN" -H 'Content-Type: application/json' -X POST -d "{\"clientCommandId\":\"e2e-t07-prepare-$RANDOM\",\"expectedRowVersion\":$PLINTH_ROW}" "$BASE/api/v1/runtime-slots/plinth/registration/prepare")
HANDLE=$(printf '%s' "$PREP" | python3 -c 'import json,sys; print(json.load(sys.stdin)["registrationTokenHandle"])')
REVEAL=$(curl -s -H "$CJ" -H "$ORIGIN" -H 'Content-Type: application/json' -X POST -d "{\"registrationTokenHandle\":\"$HANDLE\"}" "$BASE/api/v1/runtime-slots/registration-token/reveal")
printf '%s\n' "$REVEAL" | python3 -c 'import json,sys; d=json.load(sys.stdin); print(json.dumps({"slot":d["slot"],"generation":d["generation"],"token":d["registrationToken"]}))' | docker compose --project-name "$compose_project" --file "$stack/state/quoin/compose/generated/compose.yaml" run --rm --no-deps -i -T plinth register --config /etc/quoin/component.yaml >>"$evidence/playwright-server.log" 2>&1

# Create the UI-visible connection (one-time secret in the request body only).
curl -s -H "$CJ" -H "$ORIGIN" -H 'Content-Type: application/json' -X POST \
  -d '{"clientCommandId":"e2e-t07-create-1","name":"main-thanos","connection":{"type":"thanos","baseUrl":"http://quoin-t07-thanos:9090","password":"e2e-thanos-secret"}}' \
  "$BASE/api/v1/connections" >>"$evidence/playwright-server.log"

# The UI analysis ticket fixtures are maintained separately so this bootstrap
# stays focused on stack lifecycle and shared runtime setup.
source test/e2e/compose/fixtures-analysis.sh

# The fixture doubles as the ready marker: tests only run once every step
# (login/change/source/reveal/AM alert) has completed.
set +x
start_ready_server
exec docker compose --project-name "$compose_project" --file "$stack/state/quoin/compose/generated/compose.yaml" logs -f quoin plinth lintel stele 2>&1 | tee -a "$evidence/runtime-process.log"
