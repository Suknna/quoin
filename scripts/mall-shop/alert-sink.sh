#!/usr/bin/env bash
# mall-shop local Alertmanager sink lifecycle (docker, persistent via
# --restart unless-stopped; survives reboots independent of any agent session).
#
#   alert-sink.sh up      ensure container running and healthy (idempotent)
#   alert-sink.sh status  print container + delivery summary (no payload values)
#   alert-sink.sh down    stop container (recorded deliveries are kept)
#
# The container publishes ONLY on the host private LAN IP 192.168.1.200:19094
# (never 0.0.0.0 / never public). Deliveries are appended to a gitignored
# .artifacts JSONL; that file may contain alert payloads — do not commit it.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
NAME="mall-shop-alert-sink"
HOST_IP="192.168.1.200"
PORT="19094"
DATA_DIR="${REPO_ROOT}/.artifacts/mall-shop-20260916/alertsink"
IMAGE="python:3.13-alpine"

case "${1:-up}" in
  up)
    mkdir -p "${DATA_DIR}"
    if ! docker image inspect "${IMAGE}" >/dev/null 2>&1; then
      echo "FATAL: cached image ${IMAGE} not present; sink must run fully offline" >&2
      exit 1
    fi
    if docker ps --format '{{.Names}}' | grep -qx "${NAME}"; then
      echo "sink already running"
    elif docker ps -a --format '{{.Names}}' | grep -qx "${NAME}"; then
      docker start "${NAME}" >/dev/null
      echo "sink container restarted"
    else
      docker run -d --name "${NAME}" --restart unless-stopped \
        -p "${HOST_IP}:${PORT}:19094" \
        -v "${DATA_DIR}:/data" \
        -v "${REPO_ROOT}/scripts/mall-shop/alert-sink.py:/app/sink.py:ro" \
        "${IMAGE}" python3 -u /app/sink.py >/dev/null
      echo "sink container created (published on ${HOST_IP}:${PORT} only)"
    fi
    for _ in $(seq 1 20); do
      if curl -fsS -m 2 "http://${HOST_IP}:${PORT}/healthz" >/dev/null 2>&1; then
        echo "sink healthy: http://${HOST_IP}:${PORT}/healthz"
        echo "deliveries file: ${DATA_DIR}/deliveries.jsonl"
        exit 0
      fi
      sleep 0.5
    done
    echo "FATAL: sink did not become healthy; check: docker logs ${NAME}" >&2
    exit 1
    ;;
  status)
    if docker ps --format '{{.Names}}' | grep -qx "${NAME}"; then
      echo "container: running (${NAME}, ${HOST_IP}:${PORT})"
    elif docker ps -a --format '{{.Names}}' | grep -qx "${NAME}"; then
      echo "container: exists but stopped (${NAME})"
    else
      echo "container: absent (${NAME})"
    fi
    if [[ -f "${DATA_DIR}/deliveries.jsonl" ]]; then
      echo "deliveries: $(grep -c . "${DATA_DIR}/deliveries.jsonl") POSTs recorded"
    else
      echo "deliveries: none yet"
    fi
    ;;
  down)
    if docker ps -a --format '{{.Names}}' | grep -qx "${NAME}"; then
      docker stop "${NAME}" >/dev/null
      echo "sink stopped (deliveries kept in ${DATA_DIR})"
    else
      echo "sink not present"
    fi
    ;;
  *)
    echo "usage: $0 {up|status|down}" >&2
    exit 2
    ;;
esac
