#!/usr/bin/env bash
# Redeploy the mall-shop monitoring stack (Alertmanager + Prometheus) in
# namespace quoin-lab from the LOCAL lab sources, then repair the one
# environment-dependent piece: the libvirt FORWARD rule that lets the
# CURRENT Prometheus pod IP scrape the nginx VM (192.168.140.50:9100/9113).
#
# Prerequisite: the local lab tree .local-lab/ (manifests, secrets, retained
# PVs). It is a LOCAL-MACHINE asset and gitignored — a clean git clone has
# only scripts/mall-shop and cannot run this script as-is.
#
# Never deletes PVCs/PVs, never touches the preserved
# alertmanager-receiver-auth Secret, never edits foreign firewall rules.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
LAB="${REPO_ROOT}/.local-lab"
RULES_SRC="${REPO_ROOT}/scripts/mall-shop/prometheus-rules.yaml"
PROM_SVC="http://10.43.19.39:9090" # pinned Prometheus ClusterIP

[[ -d "$LAB" ]] || {
	echo "FATAL: $LAB missing (local lab tree required)" >&2
	exit 1
}

# 1) Re-embed the plain YAML sources into the ConfigMap manifests so the
#    local .local-lab files stay the single source of truth on this machine.
python3 - "$LAB" "$RULES_SRC" <<'PY'
import re, sys
lab = sys.argv[1]
def embed(cm, key, src):
    body = open(src).read().rstrip("\n")
    indented = "".join("    " + l + "\n" for l in body.splitlines())
    text = open(cm).read()
    text, n = re.subn(r"(?ms)^  " + re.escape(key) + r": \|\n.*\Z",
                      "  " + key + ": |\n" + indented, text)
    assert n == 1, (cm, n)
    open(cm, "w").write(text)
    print("embedded", key, "->", cm)
embed(f"{lab}/config/prometheus/k8s/30-configmap.yaml", "prometheus.yml",
      f"{lab}/config/prometheus/prometheus.yml")
embed(f"{lab}/config/alertmanager/k8s/30-configmap.yaml", "alertmanager.yml",
      f"{lab}/config/alertmanager/alertmanager.yml")

# Keep the 35- rules ConfigMap manifest in sync with the canonical rules so
# applying it can never resurrect stale content over the live rules.
import subprocess, yaml
gen = yaml.safe_load(subprocess.run(
    ["kubectl", "create", "configmap", "prometheus-mall-shop-rules", "-n", "quoin-lab",
     "--from-file=mall-shop-rules.yaml=" + sys.argv[2],
     "--dry-run=client", "-o", "yaml"],
    check=True, capture_output=True, text=True).stdout)
path = f"{lab}/config/prometheus/k8s/35-rules-configmap.yaml"
static = yaml.safe_load(open(path))
static["data"] = gen["data"]
yaml.safe_dump(static, open(path, "w"), sort_keys=False, allow_unicode=True)
print("synced", path)
PY

# 2) Apply rules + stacks (idempotent; PV/PVC untouched). apply.sh alone does
#    NOT make a running pod re-read changed ConfigMaps — steps 3/4 do that.
kubectl apply -f "$LAB/config/prometheus/k8s/35-rules-configmap.yaml"
bash "$LAB/config/alertmanager/k8s/apply.sh"
bash "$LAB/config/prometheus/k8s/apply.sh"

# 3) Alertmanager: its config is mounted via subPath, which does NOT receive
#    ConfigMap updates, so a restart is the only reliable config reload.
kubectl -n quoin-lab rollout restart statefulset/alertmanager
kubectl -n quoin-lab rollout status statefulset/alertmanager --timeout=180s

# Recreate the pod so both configuration and rule ConfigMaps are current;
# an unchanged glob cannot establish that updated rule contents have arrived.
kubectl -n quoin-lab rollout restart statefulset/prometheus
kubectl -n quoin-lab rollout status statefulset/prometheus --timeout=180s

# 5) REUSABLE pod-IP patch: keep exactly ONE scoped ACCEPT rule in the
#    libvirt FORWARD input chain for the CURRENT Prometheus pod IP. Run AFTER
#    any restart so a changed pod IP is picked up.
#      add:    sudo iptables -I LIBVIRT_FWI 1 -s <PODIP>/32 -d 192.168.140.0/24 \
#                -o virbr-lab -p tcp -m multiport --dports 9100,9113 \
#                -m comment --comment mall-shop-prometheus-to-vm -j ACCEPT
#      remove: same with -D instead of -I
PODIP="$(kubectl -n quoin-lab get pod -l app.kubernetes.io/name=prometheus -o jsonpath='{.items[0].status.podIP}')"
OLDIP="$(sudo iptables -S LIBVIRT_FWI | sed -n 's/.*-s \([0-9.]*\)\/32.*mall-shop-prometheus-to-vm.*/\1/p' | head -1)"
if [[ "$OLDIP" != "$PODIP" ]]; then
	if [[ -n "$OLDIP" ]]; then
		sudo iptables -D LIBVIRT_FWI -s "$OLDIP"/32 -d 192.168.140.0/24 -o virbr-lab \
			-p tcp -m multiport --dports 9100,9113 \
			-m comment --comment mall-shop-prometheus-to-vm -j ACCEPT
		echo "removed stale VM scrape rule for pod IP $OLDIP"
	fi
	sudo iptables -I LIBVIRT_FWI 1 -s "$PODIP"/32 -d 192.168.140.0/24 -o virbr-lab \
		-p tcp -m multiport --dports 9100,9113 \
		-m comment --comment mall-shop-prometheus-to-vm -j ACCEPT
	echo "added VM scrape rule for pod IP $PODIP"
else
	echo "VM scrape rule already current for pod IP $PODIP"
fi

# 6) Gate on the live state: 5 business targets up AND 6 rules loaded.
echo "waiting for business targets and rules (max 120s)..."
for _ in $(seq 1 24); do
	UP=$(curl -s -m 5 --get "$PROM_SVC/api/v1/query" \
		--data-urlencode 'query=count(up{system_id="mall-shop",job!="prometheus-self"} == 1)' |
		python3 -c "import sys,json;d=json.load(sys.stdin)['data']['result'];print(d[0]['value'][1] if d else '0')" 2>/dev/null || echo 0)
	RULES=$(curl -s -m 5 "$PROM_SVC/api/v1/rules" |
		python3 -c "import sys,json;gs=json.load(sys.stdin)['data']['groups'];print(sum(len(g['rules']) for g in gs))" 2>/dev/null || echo 0)
	[[ "$UP" == "5" && "$RULES" == "6" ]] && break
	sleep 5
done
echo "business targets up: ${UP:-0}/5, rules loaded: ${RULES:-0}/6"
[[ "${UP:-0}" == "5" && "${RULES:-0}" == "6" ]] || {
	echo "NOT fully ready — check targets: curl $PROM_SVC/api/v1/targets; rules: curl $PROM_SVC/api/v1/rules; pods: kubectl -n quoin-lab get pods" >&2
	exit 1
}
echo "monitoring stack ready"
