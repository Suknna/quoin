# Additional analysis fixtures sourced by server.sh after the shared runtime is ready.
# It receives stack/evidence/CJ/ORIGIN/BASE/GW2 from the bootstrap script.

# --- T10 fixtures: an enabled qualified provider + a firing analysis alert -
# The probe runs through the real command path (creation never auto-probes);
# poll the immutable probe-results endpoint, then enable.
curl -s -H "$CJ" -H "$ORIGIN" -H 'Content-Type: application/json' -X POST \
  -d '{"clientCommandId":"e2e-t10-probe-'"$RANDOM"'"}' \
  "$BASE/api/v1/connections/main-openai/probe" >>"$evidence/playwright-server.log"
enable_ok=0
for _ in $(seq 1 60); do
  PROBE_ID=$(curl -s -H "$CJ" -H "$ORIGIN" "$BASE/api/v1/connections/main-openai/probe-results" 2>/dev/null | python3 -c 'import json,sys
try:
    items=json.load(sys.stdin).get("items",[])
    print(next((i["id"] for i in items if i.get("outcome")=="passed"), ""))
except Exception:
    print("")' 2>/dev/null)
  if [ -n "$PROBE_ID" ]; then
    CONN_ROW=$(curl -s -H "$CJ" -H "$ORIGIN" "$BASE/api/v1/connections/main-openai" 2>/dev/null)
    ROW_VER=$(printf '%s' "$CONN_ROW" | python3 -c 'import json,sys; print(json.load(sys.stdin)["rowVersion"])')
    ENABLE=$(curl -s -H "$CJ" -H "$ORIGIN" -H 'Content-Type: application/json' -X POST \
      -d "{\"clientCommandId\":\"e2e-t10-enable-$RANDOM\",\"expectedRowVersion\":$ROW_VER,\"qualifiedProbeResultId\":\"$PROBE_ID\"}" \
      "$BASE/api/v1/connections/main-openai/enable")
    if printf '%s' "$ENABLE" | grep -q '"enabled":true'; then enable_ok=1; break; fi
  fi
  sleep 2
done
if [ "$enable_ok" != "1" ]; then
  { echo "FATAL: main-openai never qualified/enabled for T10"; } | tee -a "$evidence/playwright-server.log" >&2
  exit 1
fi
if ! docker exec e2e-am amtool --alertmanager.url=http://127.0.0.1:9093 alert add alertname=T10Probe severity=critical instance=db-2 job=quoin; then
  { echo "FATAL: amtool could not fire T10Probe"; } | tee -a "$evidence/playwright-server.log" >&2
  exit 1
fi
t10_seen=0
for _ in $(seq 1 30); do
  SNAPSHOT=$(curl -s -H "$CJ" -H "$ORIGIN" "$BASE/api/v1/alerts" 2>/dev/null || true)
  if printf '%s' "$SNAPSHOT" | grep -q T10Probe; then t10_seen=1; break; fi
  sleep 1
done
if [ "$t10_seen" != "1" ]; then
  { echo "FATAL: T10Probe never reached the Quoin alert store"; } | tee -a "$evidence/playwright-server.log" >&2
  exit 1
fi

# --- T11 fixtures: deterministic Thanos target and an enabled provider -----
pkill -f "fixtures/thanos-query" >/dev/null 2>&1 || true
go build -o "$stack/fixture-thanos" ./test/fixtures/thanos-query
("$stack/fixture-thanos" -address "0.0.0.0:18444" >"$evidence/fixture-thanos.log" 2>&1 &)
curl -s -H "$CJ" -H "$ORIGIN" -H 'Content-Type: application/json' -X POST \
  -d '{"clientCommandId":"e2e-t11-create-provider","name":"t11-openai","connection":{"type":"model_provider","baseUrl":"http://'"$GW2"':18443","chatModelId":"fixture-chat-thanos","embeddingModelId":"fixture-embed-1","contextBudgetTokens":8192,"maxOutputTokens":1024,"apiKey":"fixture-api-key-2026"}}' \
  "$BASE/api/v1/connections" >>"$evidence/playwright-server.log"
curl -s -H "$CJ" -H "$ORIGIN" -H 'Content-Type: application/json' -X POST \
  -d '{"clientCommandId":"e2e-t11-probe-provider"}' \
  "$BASE/api/v1/connections/t11-openai/probe" >>"$evidence/playwright-server.log"
MAIN_ROW=$(curl -s -H "$CJ" -H "$ORIGIN" "$BASE/api/v1/connections/main-openai" 2>/dev/null | python3 -c 'import json,sys
print(json.load(sys.stdin).get("rowVersion", ""))' 2>/dev/null)
if [ -n "$MAIN_ROW" ]; then
  curl -s -H "$CJ" -H "$ORIGIN" -H 'Content-Type: application/json' -X POST \
    -d "{\"clientCommandId\":\"e2e-t11-disable-main\",\"expectedRowVersion\":$MAIN_ROW}" \
    "$BASE/api/v1/connections/main-openai/disable" >>"$evidence/playwright-server.log"
fi
t11_provider_ok=0
for _ in $(seq 1 60); do
  PROBE_ID=$(curl -s -H "$CJ" -H "$ORIGIN" "$BASE/api/v1/connections/t11-openai/probe-results" 2>/dev/null | python3 -c 'import json,sys
try:
    items=json.load(sys.stdin).get("items",[])
    print(next((i["id"] for i in items if i.get("outcome")=="passed"), ""))
except Exception:
    print("")' 2>/dev/null)
  if [ -n "$PROBE_ID" ]; then
    CONN_ROW=$(curl -s -H "$CJ" -H "$ORIGIN" "$BASE/api/v1/connections/t11-openai" 2>/dev/null)
    ROW_VER=$(printf '%s' "$CONN_ROW" | python3 -c 'import json,sys; print(json.load(sys.stdin)["rowVersion"])')
    ENABLE=$(curl -s -H "$CJ" -H "$ORIGIN" -H 'Content-Type: application/json' -X POST \
      -d "{\"clientCommandId\":\"e2e-t11-enable-provider\",\"expectedRowVersion\":$ROW_VER,\"qualifiedProbeResultId\":\"$PROBE_ID\"}" \
      "$BASE/api/v1/connections/t11-openai/enable")
    if printf '%s' "$ENABLE" | grep -q '"enabled":true'; then t11_provider_ok=1; break; fi
  fi
  sleep 2
done
if [ "$t11_provider_ok" != "1" ]; then
  { echo "FATAL: t11-openai never qualified/enabled"; } | tee -a "$evidence/playwright-server.log" >&2
  exit 1
fi
curl -s -H "$CJ" -H "$ORIGIN" -H 'Content-Type: application/json' -X POST \
  -d '{"clientCommandId":"e2e-t11-create-thanos","name":"t11-thanos","connection":{"type":"thanos","baseUrl":"http://'"$GW2"':18444"}}' \
  "$BASE/api/v1/connections" >>"$evidence/playwright-server.log"
THANOS_ROW=$(curl -s -H "$CJ" -H "$ORIGIN" "$BASE/api/v1/connections/t11-thanos" | python3 -c 'import json,sys; print(json.load(sys.stdin)["rowVersion"])')
curl -s -H "$CJ" -H "$ORIGIN" -H 'Content-Type: application/json' -X POST \
  -d "{\"clientCommandId\":\"e2e-t11-enable-thanos\",\"expectedRowVersion\":$THANOS_ROW}" \
  "$BASE/api/v1/connections/t11-thanos/enable" >>"$evidence/playwright-server.log"
if ! docker exec e2e-am amtool --alertmanager.url=http://127.0.0.1:9093 alert add alertname=T11Thanosa severity=warning instance=db-3 job=quoin; then
  { echo "FATAL: T11Thanosa never reached the Quoin alert store"; } | tee -a "$evidence/playwright-server.log" >&2
  exit 1
fi
t11_seen=0
for _ in $(seq 1 30); do
  SNAPSHOT=$(curl -s -H "$CJ" -H "$ORIGIN" "$BASE/api/v1/alerts" 2>/dev/null || true)
  if printf '%s' "$SNAPSHOT" | grep -q T11Thanosa; then t11_seen=1; break; fi
  sleep 1
done
if [ "$t11_seen" != "1" ]; then
  { echo "FATAL: T11Thanosa never reached the Quoin alert store"; } | tee -a "$evidence/playwright-server.log" >&2
  exit 1
fi

# --- T12 fixtures: a slow provider alert for the recovery UI scenario ---
if ! docker exec e2e-am amtool --alertmanager.url=http://127.0.0.1:9093 alert add alertname=T12SlowPage severity=warning instance=db-4 job=quoin; then
  { echo "FATAL: T12SlowPage never reached the Quoin alert store"; } | tee -a "$evidence/playwright-server.log" >&2
  exit 1
fi
t12_seen=0
for _ in $(seq 1 30); do
  SNAPSHOT=$(curl -s -H "$CJ" -H "$ORIGIN" "$BASE/api/v1/alerts" 2>/dev/null || true)
  if printf '%s' "$SNAPSHOT" | grep -q T12SlowPage; then t12_seen=1; break; fi
  sleep 1
done
if [ "$t12_seen" != "1" ]; then
  { echo "FATAL: T12SlowPage never reached the Quoin alert store"; } | tee -a "$evidence/playwright-server.log" >&2
  exit 1
fi
