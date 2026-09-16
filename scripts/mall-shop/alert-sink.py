#!/usr/bin/env python3
"""Local Alertmanager webhook sink for the mall-shop lab rehearsal.

Records every webhook POST as one JSON line (deliveries.jsonl) so firing and
resolved notifications have on-disk delivery proof. No external email/webhook
is configured in this lab; this sink IS the verification receiver.

Intended to run as a docker container (see alert-sink.sh). The container
publishes ONLY on the host private LAN address (192.168.1.200:19094) — never
on 0.0.0.0 or any public interface. Deliveries may contain alert payloads and
are written to a gitignored .artifacts directory only.
"""

import json
import os
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PORT = int(os.environ.get("SINK_PORT", "19094"))
DATA_FILE = os.environ.get("SINK_DATA_FILE", "/data/deliveries.jsonl")


class SinkHandler(BaseHTTPRequestHandler):
    # Bounded requests only: drop stalled/malformed LAN clients after 5s.
    timeout = 5
    MAX_BODY = 1024 * 1024  # 1MiB is far above any real Alertmanager payload

    def _respond(self, code: int, body: bytes = b"{}\n") -> None:
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        # Health only. Deliveries are NOT readable over the network: the JSONL
        # lives on the host bind mount and in-container via docker exec.
        if self.path in ("/healthz", "/health"):
            self._respond(200, b'{"status":"ok"}\n')
        else:
            self._respond(404, b'{"error":"not found"}\n')

    def do_POST(self):
        if self.path != "/alerts":
            self._respond(404, b'{"error":"not found"}\n')
            return
        try:
            length = int(self.headers.get("Content-Length") or "")
        except ValueError:
            self._respond(400, b'{"error":"missing or invalid Content-Length"}\n')
            return
        if not 1 <= length <= self.MAX_BODY:
            self._respond(400, b'{"error":"Content-Length out of range"}\n')
            return
        raw = self.rfile.read(length)
        try:
            payload = json.loads(raw)
        except json.JSONDecodeError:
            self._respond(400, b'{"error":"invalid JSON"}\n')
            return
        record = {
            "received_at": time.strftime("%Y-%m-%dT%H:%M:%S%z"),
            "remote": self.client_address[0],
            "payload": payload,
        }
        os.makedirs(os.path.dirname(DATA_FILE), exist_ok=True)
        with open(DATA_FILE, "a", encoding="utf-8") as f:
            f.write(json.dumps(record, ensure_ascii=False) + "\n")
        self._respond(200)

    def log_message(self, fmt, *args):  # keep container logs quiet; JSONL is authoritative
        pass


if __name__ == "__main__":
    ThreadingHTTPServer(("0.0.0.0", PORT), SinkHandler).serve_forever()
