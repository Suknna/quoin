#!/usr/bin/env bash
# mall-shop REAL alert trigger/recover drill (reversible, no fake alerts).
#
# Injects one real, minimal fault: scales the mall-redis-exporter Deployment
# in namespace mall-lab to 0, so Prometheus genuinely loses the target. The
# MallShopMiddlewareTargetDown rule (up == 0 for 40s) must go pending ->
# firing, Alertmanager must route it, and the local sink must record a NEW
# firing delivery (baseline-counted, so earlier deliveries never count).
# Then restores the original replica count and verifies the resolved
# delivery the same way.
#
# Nothing is modified unless the precondition check passes; the EXIT trap
# restores the original replica count only after a real injection.
# Evidence lines go to stdout and --evidence file if given.
set -euo pipefail

PROM="http://10.43.19.39:9090" # pinned prometheus ClusterIP
AM="http://10.43.100.206:9093" # pinned alertmanager ClusterIP
NS="mall-lab"
DEPLOY="mall-redis-exporter"
RULE="MallShopMiddlewareTargetDown"
JOB="mall-redis-exporter"
SINK_FILE="/home/suknna/code/quoin/.artifacts/mall-shop-20260916/alertsink/deliveries.jsonl"
EVID="${2:-/dev/null}"

ts() { date -u +%Y-%m-%dT%H:%M:%SZ; }
log() {
	local line
	line="[$(ts)] $*"
	echo "$line"
	if [[ "$EVID" != "/dev/null" ]]; then echo "$line" >>"$EVID"; fi
}

promq() { curl -s -m 5 --get "$PROM/api/v1/query" --data-urlencode "query=$1"; }
# Evaluates PY against the JSON on stdin loaded as `d`; failures print empty.
json_field() { python3 -c "
import sys, json
try:
    d = json.load(sys.stdin)
    print(eval(sys.argv[1]))
except Exception:
    print('')
" "$1" 2>/dev/null; }

up_value() { promq "up{job=\"$JOB\",system_id=\"mall-shop\"}" | json_field "d['data']['result'][0]['value'][1] if d['data']['result'] else 'absent'"; }
rule_state() { promq "ALERTS{alertname=\"$RULE\",system_id=\"mall-shop\"}" | json_field "d['data']['result'][0]['metric'].get('alertstate','') if d['data']['result'] else ''"; }
am_active() { curl -s -m 5 "$AM/api/v2/alerts" | json_field "int(any(a['labels'].get('alertname')=='$RULE' and a['status']['state']=='active' for a in d))"; }
sink_count() {
	python3 - "$1" "$SINK_FILE" <<'PY'
import json, sys
want, path = sys.argv[1], sys.argv[2]
n = 0
try:
    with open(path) as f:
        for line in f:
            try:
                rec = json.loads(line)
            except json.JSONDecodeError:
                continue
            payload = rec.get("payload")
            if not isinstance(payload, dict):
                continue
            alerts = payload.get("alerts") or []
            if any(a.get("labels", {}).get("alertname") == "MallShopMiddlewareTargetDown"
                   and a.get("labels", {}).get("system_id") == "mall-shop"
                   and a.get("status") == want for a in alerts):
                n += 1
except FileNotFoundError:
    pass
print(n)
PY
}

ORIG_REPLICAS="$(kubectl -n "$NS" get deploy "$DEPLOY" -o jsonpath='{.spec.replicas}')"
INJECTED=0
cleanup() {
	if [[ "$INJECTED" == "1" ]]; then
		log "cleanup: restoring $NS/deploy/$DEPLOY replicas=$ORIG_REPLICAS"
		kubectl -n "$NS" scale deploy "$DEPLOY" --replicas="$ORIG_REPLICAS" >/dev/null 2>&1 || true
	fi
}
trap cleanup EXIT

log "drill start: target=$NS/deploy/$DEPLOY rule=$RULE original_replicas=$ORIG_REPLICAS"
pre_up="$(up_value)"
[[ "$pre_up" == "1" ]] || {
	log "FATAL: up{job=$JOB}=$pre_up before drill; aborting with NO fault injected"
	exit 1
}
BASE_FIRING="$(sink_count firing)"
BASE_RESOLVED="$(sink_count resolved)"
log "precondition: up=$pre_up sink_baseline firing=$BASE_FIRING resolved=$BASE_RESOLVED (only NEW deliveries count)"

log "inject: kubectl -n $NS scale deploy $DEPLOY --replicas=0"
kubectl -n "$NS" scale deploy "$DEPLOY" --replicas=0 >/dev/null
INJECTED=1

FIRED=""
for i in $( # up to ~3min
	seq 1 60
); do
	ST="$(rule_state)"
	AMV="$(am_active)"
	SK="$(sink_count firing)"
	if [[ "$ST" == "firing" && "$AMV" == "1" && "$SK" -gt "$BASE_FIRING" ]]; then
		FIRED="$(ts)"
		log "FIRED at $FIRED: Prom rule=firing, AM active=1, sink NEW firing deliveries=$((SK - BASE_FIRING))"
		break
	fi
	if [[ $((i % 4)) -eq 0 ]]; then log "  t+${i}x3s rule=${ST:-none} am_active=$AMV sink_firing=$SK/$BASE_FIRING"; fi
	sleep 3
done
if [[ -z "$FIRED" ]]; then
	log "FATAL: no verified firing (Prom + AM + NEW sink delivery) within 3min"
	exit 1
fi

log "recover: kubectl -n $NS scale deploy $DEPLOY --replicas=$ORIG_REPLICAS"
kubectl -n "$NS" scale deploy "$DEPLOY" --replicas="$ORIG_REPLICAS" >/dev/null

RESOLVED=""
for i in $(seq 1 60); do
	ST="$(rule_state)"
	SKR="$(sink_count resolved)"
	UP="$(up_value)"
	if [[ "$UP" == "1" && -z "$ST" && "$SKR" -gt "$BASE_RESOLVED" ]]; then
		RESOLVED="$(ts)"
		log "RESOLVED at $RESOLVED: up=1, rule inactive, sink NEW resolved deliveries=$((SKR - BASE_RESOLVED))"
		break
	fi
	if [[ $((i % 4)) -eq 0 ]]; then log "  t+${i}x3s up=$UP rule=${ST:-inactive} sink_resolved=$SKR/$BASE_RESOLVED"; fi
	sleep 3
done
if [[ -z "$RESOLVED" ]]; then
	log "FATAL: no verified recovery (up=1 + resolved delivery) within 3min"
	exit 1
fi

log "drill PASSED: real fault -> Prom firing -> AM delivery -> recovery -> resolved delivery; replicas restored to $ORIG_REPLICAS"
