#!/usr/bin/env bash
# Real-response regression for the frontend static config (web-caddy.yaml).
#
# It replays the exact production pipeline from deploy/images/frontend/Dockerfile
# (yaml -> Caddyfile -> caddy adapt) inside the same caddy:2.10.2-alpine image,
# serves a minimal dist fixture with the adapted config, and asserts the three
# serving contracts:
#   1. a SPA deep link still returns the document: 200 + text/html + no-cache
#   2. an existing hashed asset serves immutable
#   3. a missing asset 404s uncached and never falls back to the SPA document
#      (a stale tab after a deployment update must fail the module load as a
#      plain 404, not cache an HTML body under an immutable asset URL)
#
# Requires docker (the same tool deploy/images/build.sh uses). Not wired into
# `make test` on purpose: the Go gate stays docker-free.
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)
caddy_image="caddy:2.10.2-alpine"
listen_port="${QUOIN_CADDY_TEST_PORT:-18098}"

command -v docker >/dev/null || { echo "docker is required" >&2; exit 2; }
docker image inspect "$caddy_image" >/dev/null 2>&1 || docker pull "$caddy_image"

work=$(mktemp -d)
container="quoin-web-caddy-response-test-$$"
cleanup() {
  docker rm -f "$container" >/dev/null 2>&1 || true
  rm -rf "$work"
}
trap cleanup EXIT

# Fixture document root: one document and one hashed chunk, like a real dist.
mkdir -p "$work/srv/assets"
printf '%s\n' '<!doctype html><title>quoin-fixture</title>' >"$work/srv/index.html"
printf '%s\n' 'console.log("fixture chunk");' >"$work/srv/assets/app-abc123.js"

# Same yaml->Caddyfile extraction and caddy adapt conversion as the Dockerfile.
# The frontend directory is mounted read-only (single-file binds are not
# portable across docker setups); the container only reads web-caddy.yaml.
docker run --rm -i \
  -v "$repo_root/deploy/images/frontend:/src:ro" \
  -v "$work:/out" \
  "$caddy_image" sh -c '
    apk add --no-cache py3-yaml >/dev/null 2>&1
    python3 - <<PY
import pathlib, subprocess, yaml
source = yaml.safe_load(pathlib.Path("/src/web-caddy.yaml").read_text())
caddyfile = source.get("caddyfile")
if not isinstance(caddyfile, str) or not caddyfile.strip():
    raise SystemExit("web-caddy.yaml must contain a non-empty caddyfile string")
pathlib.Path("/out/Caddyfile").write_text(caddyfile)
with pathlib.Path("/out/caddy.json").open("w") as output:
    subprocess.run(["caddy", "adapt", "--config", "/out/Caddyfile",
                    "--adapter", "caddyfile", "--pretty"], check=True, stdout=output)
PY
  '
test -s "$work/caddy.json" || { echo "caddy adapt produced no config" >&2; exit 1; }
echo "caddy adapt: ok"

# Serve the fixture with the adapted config exactly as the image would.
docker run -d --name "$container" \
  -p "127.0.0.1:${listen_port}:8080" \
  -v "$work/caddy.json:/etc/caddy/caddy.json:ro" \
  -v "$work/srv:/srv:ro" \
  "$caddy_image" caddy run --config /etc/caddy/caddy.json >/dev/null

base="http://127.0.0.1:${listen_port}"
ready=""
for _ in $(seq 1 40); do
  if curl -sf -o /dev/null "$base/"; then ready=1; break; fi
  sleep 0.25
done
test -n "$ready" || { echo "caddy did not become ready" >&2; docker logs "$container" >&2 || true; exit 1; }

failures=0
assert_contains() {
  local label=$1 haystack=$2 needle=$3
  if [[ "$haystack" == *"$needle"* ]]; then
    echo "pass: $label"
  else
    echo "FAIL: $label (wanted '$needle' in: $haystack)" >&2
    failures=$((failures + 1))
  fi
}
assert_equals() {
  local label=$1 actual=$2 expected=$3
  if [[ "$actual" == "$expected" ]]; then
    echo "pass: $label"
  else
    echo "FAIL: $label (wanted '$expected', got '$actual')" >&2
    failures=$((failures + 1))
  fi
}

# 1. SPA deep link survives as a document and revalidates.
deep_status=$(curl -sS -D "$work/deep-headers" -o "$work/deep-body" -w '%{http_code}' "$base/knowledge/candidates/5")
deep_headers=$(cat "$work/deep-headers")
deep_body=$(cat "$work/deep-body")
assert_equals "deep link returns 200" "$deep_status" "200"
assert_contains "deep link is the SPA document" "$deep_body" "quoin-fixture"
assert_contains "deep link revalidates (no-cache)" "$deep_headers" "Cache-Control: no-cache"
assert_contains "deep link is html" "$deep_headers" "Content-Type: text/html"

# 2. An existing hashed asset is immutable.
asset_status=$(curl -sS -D "$work/asset-headers" -o /dev/null -w '%{http_code}' "$base/assets/app-abc123.js")
asset_headers=$(cat "$work/asset-headers")
assert_equals "existing asset returns 200" "$asset_status" "200"
assert_contains "existing asset is immutable" "$asset_headers" "Cache-Control: public, max-age=31536000, immutable"
assert_contains "existing asset is javascript" "$asset_headers" "javascript"

# 3. A missing asset 404s uncached and never serves the SPA document.
missing_status=$(curl -sS -D "$work/missing-headers" -o "$work/missing-body" -w '%{http_code}' "$base/assets/stale-deadbeef.js")
missing_headers=$(cat "$work/missing-headers")
missing_body=$(cat "$work/missing-body")
assert_equals "missing asset returns 404" "$missing_status" "404"
assert_contains "missing asset is not cached" "$missing_headers" "Cache-Control: no-store"
if [[ "$missing_headers" == *"Content-Type: text/html"* ]]; then
  echo "FAIL: missing asset fell back to the SPA document" >&2
  failures=$((failures + 1))
else
  echo "pass: missing asset does not serve the SPA document"
fi
if [[ "$missing_body" == *quoin-fixture* ]]; then
  echo "FAIL: missing asset body is the SPA document" >&2
  failures=$((failures + 1))
else
  echo "pass: missing asset body is not the SPA document"
fi

# API dispatch keeps failing closed instead of falling through to the SPA.
api_status=$(curl -sS -o /dev/null -w '%{http_code}' "$base/api/v1/auth/config")
assert_equals "API path stays 404 at the frontend" "$api_status" "404"

if [[ "$failures" -ne 0 ]]; then
  echo "$failures assertion(s) failed" >&2
  exit 1
fi
echo "all frontend caddy response assertions passed"
