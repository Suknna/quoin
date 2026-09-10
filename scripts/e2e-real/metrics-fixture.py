#!/usr/bin/env python3
"""Controlled Prometheus-compatible HTTP endpoint for #97 real E2E.

The fixture is deliberately narrow: it accepts only the production probe query
and the business-scoped query used by the acceptance test.  It proves which
named endpoint received a request without seeding Quoin state or imitating any
Quoin API.  Authentication values are loaded from mounted runtime-only files,
never from source control or process arguments.
"""

from __future__ import annotations

import argparse
import base64
import json
import os
import ssl
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from urllib.parse import parse_qs, urlparse


def read_secret(environment_name: str) -> str:
    """Read a disposable credential only when this fixture mode needs one."""
    filename = os.environ.get(environment_name, "")
    if not filename:
        return ""
    return Path(filename).read_text(encoding="utf-8").strip()


class MetricsFixture(BaseHTTPRequestHandler):
    fixture_name = ""
    auth_mode = "none"
    basic_username = ""
    basic_password = ""
    bearer_token = ""
    hits: list[str] = []

    def log_message(self, _format: str, *_args: object) -> None:
        # Request contents may contain an Authorization header. Keep container
        # logs intentionally silent so E2E secrets cannot escape diagnostics.
        return

    def send_json(self, status: int, value: object) -> None:
        encoded = json.dumps(value, separators=(",", ":")).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(encoded)))
        self.end_headers()
        self.wfile.write(encoded)

    def authorized(self) -> bool:
        authorization = self.headers.get("Authorization", "")
        if self.auth_mode == "none":
            return authorization == ""
        if self.auth_mode == "bearer":
            return authorization == f"Bearer {self.bearer_token}"
        if self.auth_mode == "basic":
            expected = base64.b64encode(
                f"{self.basic_username}:{self.basic_password}".encode("utf-8")
            ).decode("ascii")
            return authorization == f"Basic {expected}"
        return False

    def do_GET(self) -> None:  # noqa: N802 - BaseHTTPRequestHandler API
        parsed = urlparse(self.path)
        if parsed.path == "/-/ready":
            self.send_json(200, {"status": "ready", "fixture": self.fixture_name})
            return
        if parsed.path == "/fixture/hits":
            # This endpoint is reachable only on the internal Compose network;
            # it is an assertion aid, not a production adapter surface.
            self.send_json(200, {"fixture": self.fixture_name, "queries": self.hits})
            return
        if parsed.path != "/api/v1/query":
            self.send_json(404, {"status": "error", "error": "not found"})
            return
        if not self.authorized():
            self.send_json(401, {"status": "error", "error": "unauthorized"})
            return

        query = parse_qs(parsed.query).get("query", [""])[0]
        self.hits.append(query)
        if query == "vector(1)":
            self.send_json(200, {"status": "success", "data": {"resultType": "vector", "result": [{"metric": {"__name__": "vector", "fixture_endpoint": self.fixture_name}, "value": [1760000000, "1"]}]}})
            return
        if query.startswith('up{system_id="e2e-metrics-'):
            self.send_json(200, {"status": "success", "data": {"resultType": "vector", "result": [{"metric": {"__name__": "up", "system_id": query.split('"')[1], "fixture_endpoint": self.fixture_name}, "value": [1760000000, "1"]}]}})
            return
        if query.startswith('fixture_resource{system_id="e2e-metrics-'):
            self.send_json(200, {"status": "success", "data": {"resultType": "vector", "result": [{"metric": {"__name__": "fixture_resource", "system_id": query.split('"')[1], "instance": "fixture-identity", "fixture_endpoint": self.fixture_name}, "value": [1760000000, "1"]}]}})
            return
        self.send_json(422, {"status": "error", "errorType": "bad_data", "error": "fixture rejects unscoped query"})


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--name", required=True)
    parser.add_argument("--auth", choices=("none", "basic", "bearer"), required=True)
    parser.add_argument("--basic-username", default="")
    parser.add_argument("--tls-cert", default="")
    parser.add_argument("--tls-key", default="")
    args = parser.parse_args()

    MetricsFixture.fixture_name = args.name
    MetricsFixture.auth_mode = args.auth
    MetricsFixture.basic_username = args.basic_username
    MetricsFixture.basic_password = read_secret("METRICS_FIXTURE_BASIC_PASSWORD_FILE")
    MetricsFixture.bearer_token = read_secret("METRICS_FIXTURE_BEARER_TOKEN_FILE")
    if args.auth == "basic" and (not args.basic_username or not MetricsFixture.basic_password):
        raise SystemExit("basic fixture credentials are required")
    if args.auth == "bearer" and not MetricsFixture.bearer_token:
        raise SystemExit("bearer fixture token is required")

    server = ThreadingHTTPServer(("0.0.0.0", 8080), MetricsFixture)
    if bool(args.tls_cert) != bool(args.tls_key):
        raise SystemExit("both TLS certificate and key are required")
    if args.tls_cert:
        context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        context.load_cert_chain(args.tls_cert, args.tls_key)
        server.socket = context.wrap_socket(server.socket, server_side=True)
    server.serve_forever()


if __name__ == "__main__":
    main()
