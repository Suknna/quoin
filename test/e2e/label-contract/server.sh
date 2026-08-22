#!/usr/bin/env bash
# T17 Playwright fixture: a real but self-owned Compose stack. Every run gets
# an unguessable project, state directory, containers and image tags; no caller
# can redirect it at another Compose project through an environment variable.
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)
cd "$repo_root"
evidence="${QUOIN_EVIDENCE_DIR:-$repo_root/.artifacts/tickets/T17}"
mkdir -p "$evidence"

stack=$(mktemp -d "$repo_root/.artifacts/e2e-t17-XXXXXX")
run_id=$(basename "$stack" | tr '[:upper:]' '[:lower:]')
normalized_stack="$repo_root/.artifacts/$run_id"
if [[ -e "$normalized_stack" ]]; then
  echo "refusing to reuse pre-existing ticket fixture directory: $normalized_stack" >&2
  exit 1
fi
mv "$stack" "$normalized_stack"
stack="$normalized_stack"
project="quoin-t17-e2e-${run_id#e2e-t17-}"
namespace="${project}-image"
compose_file="$stack/state/quoin/compose/generated/compose.yaml"
override_file="$stack/fixture-images.compose.yaml"
internal_network="${project}_internal"
forwarder="${project}-forwarder"
alertmanager="${project}-alertmanager"
readiness="${project}-ready"
fixture_label="com.quoin.fixture"
fixture_value="$run_id"
fixture_manifest="$evidence/ticket17-browser-fixture.json"
created_images=()

# Shell cleanup only runs while startup has not handed the running stack to
# Playwright global teardown. A name match is never ownership: every container
# must carry this exact run's fixture label before it can be stopped or removed.
owned_container() {
  local name=$1 value
  value=$(docker inspect --format "{{ index .Config.Labels \"$fixture_label\" }}" "$name" 2>/dev/null) || return 1
  [[ "$value" == "$fixture_value" ]]
}

remove_owned_container() {
  local name=$1
  if docker inspect "$name" >/dev/null 2>&1; then
    if owned_container "$name"; then
      docker rm -f "$name" >/dev/null 2>&1 || true
    else
      echo "refusing to remove non-fixture container $name" >&2
    fi
  fi
}

owned_image() {
  [[ "$(docker image inspect --format "{{ index .Config.Labels \"$fixture_label\" }}" "$1" 2>/dev/null)" == "$fixture_value" ]]
}

owned_compose_projection() {
  local containers container
  containers=$(docker compose --project-name "$project" --file "$compose_file" --file "$override_file" ps -aq 2>/dev/null) || return 1
  for container in $containers; do
    owned_container "$container" || return 1
  done
}

cleanup_startup_failure() {
  status=$?
  if [[ "$status" -ne 0 ]]; then
    echo "T17 browser fixture startup failed (exit=$status); removing only this run's resources" >&2
    remove_owned_container "$forwarder"
    remove_owned_container "$alertmanager"
    remove_owned_container "$readiness"
    if [[ -f "$compose_file" ]]; then
      if owned_compose_projection; then
        docker compose --project-name "$project" --file "$compose_file" --file "$override_file" down --remove-orphans >/dev/null 2>&1 || true
      else
        echo "refusing to tear down Compose project with non-fixture containers" >&2
      fi
    fi
    for image in "${created_images[@]}"; do
      if owned_image "$image"; then
        docker image rm "$image" >/dev/null 2>&1 || true
      else
        echo "refusing to remove image without this fixture label: $image" >&2
      fi
    done
    rm -rf "$stack"
  fi
  exit "$status"
}
trap cleanup_startup_failure EXIT

# Retain the exact pre-state of the generic network. The fixture never joins it;
# teardown compares this snapshot after Chromium has finished.
docker network inspect quoin_internal --format '{{range $id, $container := .Containers}}{{$id}}={{$container.Name}}{{println}}{{end}}' >"$stack/shared-network-before" 2>&1 || true
docker network inspect quoin_internal >/dev/null 2>&1 && echo 0 >"$stack/shared-network-before.exit" || echo 1 >"$stack/shared-network-before.exit"

# The application image is part of what Chromium verifies. Build it under a
# fresh ticket-owned namespace rather than overwriting/deleting quoin/* tags
# that may belong to another local stack.
image_proxy="${QUOIN_IMAGE_GOPROXY:-$(go env GOPROXY)}"
for target in quoin plinth lintel stele; do
  image="$namespace/$target:v0.1.0-dev"
  if docker image inspect "$image" >/dev/null 2>&1; then
    echo "refusing to replace pre-existing ticket fixture image $image" >&2
    exit 1
  fi
  docker build -f build/package/Dockerfile --target "$target" \
    --label "$fixture_label=$fixture_value" \
    ${image_proxy:+--build-arg "GOPROXY=$image_proxy"} \
    -t "$image" .
  created_images+=("$image")
done

if docker network inspect "$internal_network" >/dev/null 2>&1; then
  echo "refusing to reuse pre-existing ticket fixture network $internal_network" >&2
  exit 1
fi

cat >"$override_file" <<EOF
services:
  secret-bootstrap:
    image: $namespace/quoin:v0.1.0-dev
    labels: { $fixture_label: "$fixture_value" }
  admin-bootstrap:
    image: $namespace/quoin:v0.1.0-dev
    labels: { $fixture_label: "$fixture_value" }
  quoin:
    image: $namespace/quoin:v0.1.0-dev
    labels: { $fixture_label: "$fixture_value" }
  plinth:
    image: $namespace/plinth:v0.1.0-dev
    labels: { $fixture_label: "$fixture_value" }
  lintel:
    image: $namespace/lintel:v0.1.0-dev
    labels: { $fixture_label: "$fixture_value" }
  stele:
    image: $namespace/stele:v0.1.0-dev
    labels: { $fixture_label: "$fixture_value" }
EOF

# quoin-deploy deliberately owns production project name `quoin`. This test
# local shim maps only its exact known invocation and fails closed if it ever
# observes an unexpected `quoin` Compose form; it cannot be redirected by the
# caller. The image override is appended after the generated projection.
real_docker=$(command -v docker)
mkdir -p "$stack/docker-bin" "$stack/am"
cat >"$stack/docker-bin/docker" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
if [[ "$#" -ge 6 && "$1" == "compose" && "$2" == "--project-name" && "$3" == "quoin" ]]; then
  if [[ "$4" != "--file" ]]; then
    echo "T17 fixture rejected unexpected quoin-deploy Compose invocation" >&2
    exit 70
  fi
  exec "${QUOIN_T17_REAL_DOCKER:?}" compose --project-name "${QUOIN_T17_COMPOSE_PROJECT:?}" \
    --file "$5" --file "${QUOIN_T17_IMAGE_OVERRIDE:?}" "${@:6}"
fi
exec "${QUOIN_T17_REAL_DOCKER:?}" "$@"
EOF
chmod 700 "$stack/docker-bin/docker"

# From here, retain raw non-secret command evidence. No shell tracing is ever
# enabled: passwords and revealed alert bearer remain in this 0700 stack only.
exec > >(tee -a "$evidence/playwright-t17-server.log") 2>&1
echo "T17 browser fixture run=$run_id project=$project internalNetwork=$internal_network"

go build -trimpath -o "$stack/quoin-deploy" ./cmd/quoin-deploy
password="e2e-$(openssl rand -base64 24 | tr -d '/+=' | cut -c1-24)-2026"
printf '%s' "$password" >"$stack/admin-temp-password"
chmod 600 "$stack/admin-temp-password"
sed "s|REPLACE_WITH_STACK_DIR|$stack|" test/e2e/compose/compose-install.yaml >"$stack/install.yaml"
printf 'admin\nT17 E2E Admin\n%s\n%s\n' "$password" "$password" | \
  PATH="$stack/docker-bin:$PATH" \
  QUOIN_T17_REAL_DOCKER="$real_docker" \
  QUOIN_T17_COMPOSE_PROJECT="$project" \
  QUOIN_T17_IMAGE_OVERRIDE="$override_file" \
  QUOIN_DEPLOY_SCRIPTED=1 \
  XDG_STATE_HOME="$stack/state" \
  "$stack/quoin-deploy" compose install --config "$stack/install.yaml"

base=http://127.0.0.1:18080
origin='Origin: https://quoin.example.com'
login_headers="$stack/login.headers"
curl -sS -D "$login_headers" -H "$origin" -H 'Content-Type: application/json' -X POST \
  -d "{\"username\":\"admin\",\"password\":\"$password\"}" "$base/api/v1/auth/login" >/dev/null
session_cookie=$(awk 'tolower($1)=="set-cookie:" {print $2}' "$login_headers" | tr -d '\r' | head -1)
if [[ -z "$session_cookie" ]]; then
  echo "FATAL: login did not return a session cookie" >&2
  exit 1
fi
cookie_header="Cookie: $session_cookie"
new_password="e2e-$(openssl rand -base64 18 | tr -d '/+=' | cut -c1-18)-2027"
password_status=$(curl -sS -o /dev/null -w '%{http_code}' -H "$cookie_header" -H "$origin" -H 'Content-Type: application/json' -X PUT \
  -d "{\"currentPassword\":\"$password\",\"newPassword\":\"$new_password\"}" "$base/api/v1/auth/password")
if [[ "$password_status" != "204" ]]; then
  echo "FATAL: password change returned HTTP $password_status" >&2
  exit 1
fi
echo "password-change-http=$password_status"

source_meta=$(curl -sS -H "$cookie_header" -H "$origin" -H 'Content-Type: application/json' -X POST \
  -d '{"key":"t17-e2e-am","protocol":"alertmanager","clientCommandId":"t17-e2e-source-1"}' "$base/api/v1/alert-sources")
reveal_handle=$(printf '%s' "$source_meta" | python3 -c 'import json,sys; print(json.load(sys.stdin)["revealHandle"])')
bearer=$(curl -sS -H "$cookie_header" -H "$origin" -H 'Content-Type: application/json' -X POST \
  -d "{\"revealHandle\":\"$reveal_handle\"}" "$base/api/v1/alert-sources/credentials/reveal" | python3 -c 'import json,sys; print(json.load(sys.stdin)["bearerToken"])')
if [[ -z "$bearer" ]]; then
  echo "FATAL: alert source credential reveal returned no bearer" >&2
  exit 1
fi

cat >"$stack/am/forwarder.py" <<'PYEOF'
import http.server, os, urllib.error, urllib.request
class Handler(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        body = self.rfile.read(int(self.headers.get('Content-Length', '0')))
        request = urllib.request.Request(
            os.environ['STELE_URL'], data=body, method='POST',
            headers={'Content-Type': self.headers.get('Content-Type', 'application/json'), 'Authorization': 'Bearer ' + os.environ['STELE_BEARER']},
        )
        try:
            with urllib.request.urlopen(request, timeout=10) as response:
                self.send_response(response.status)
                self.end_headers()
        except urllib.error.HTTPError as error:
            self.send_response(error.code)
            self.end_headers()
        except Exception:
            self.send_response(502)
            self.end_headers()
    def log_message(self, *args):
        pass
http.server.HTTPServer(('0.0.0.0', 8099), Handler).serve_forever()
PYEOF
cat >"$stack/am/alertmanager.yml" <<EOF
route:
  receiver: sink
  group_wait: 0s
  group_interval: 1s
  repeat_interval: 1h
receivers:
- name: sink
  webhook_configs:
  - url: http://$forwarder:8099/
    send_resolved: true
EOF

docker run -d --name "$forwarder" --label "$fixture_label=$fixture_value" -p 127.0.0.1:18082:8099 \
  -e 'STELE_URL=http://stele:8080/' -e "STELE_BEARER=$bearer" \
  -v "$stack/am/forwarder.py:/forwarder.py:ro" python:3.12-slim python /forwarder.py >/dev/null
docker network connect "$internal_network" "$forwarder"
docker run -d --name "$alertmanager" --label "$fixture_label=$fixture_value" -p 127.0.0.1:19093:9093 \
  -v "$stack/am/alertmanager.yml:/etc/alertmanager/alertmanager.yml:ro" prom/alertmanager:v0.28.1 >/dev/null
docker network connect "$internal_network" "$alertmanager"

alertmanager_ready=0
for _ in $(seq 1 30); do
  if curl -sf http://127.0.0.1:19093/-/healthy >/dev/null 2>&1; then
    alertmanager_ready=1
    break
  fi
  sleep 1
done
if [[ "$alertmanager_ready" != "1" ]]; then
  echo "FATAL: Alertmanager never became ready" >&2
  exit 1
fi

printf '%s' "$new_password" >"$stack/admin-new-password"
chmod 600 "$stack/admin-new-password"
python3 - "$fixture_manifest" <<EOF
import json, sys
json.dump({
  "runId": "$run_id",
  "project": "$project",
  "stack": "$stack",
  "composeFile": "$compose_file",
  "imageOverride": "$override_file",
  "internalNetwork": "$internal_network",
  "forwarder": "$forwarder",
  "alertmanager": "$alertmanager",
  "readiness": "$readiness",
  "images": ["$namespace/quoin:v0.1.0-dev", "$namespace/plinth:v0.1.0-dev", "$namespace/lintel:v0.1.0-dev", "$namespace/stele:v0.1.0-dev"],
  "verified": ["real compose install", "admin password change", "alert source credential relay", "Alertmanager health"]
}, open(sys.argv[1], "w"), indent=2)
EOF

cat >"$stack/ready.py" <<PYEOF
import http.server, os
class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(200 if os.path.exists('/fixture/admin-new-password') else 503)
        self.end_headers()
    def log_message(self, *args):
        pass
http.server.HTTPServer(('0.0.0.0', 18083), Handler).serve_forever()
PYEOF
docker run -d --name "$readiness" --label "$fixture_label=$fixture_value" -p 127.0.0.1:18083:18083 \
  -v "$stack:/fixture:ro" python:3.12-slim python /fixture/ready.py >/dev/null
trap - EXIT
exec docker compose --project-name "$project" --file "$compose_file" --file "$override_file" logs -f quoin plinth lintel stele
