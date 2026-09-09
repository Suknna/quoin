# syntax=docker/dockerfile:1.7
# The frontend is independently built from the repository-root context so its
# workspace metadata and web sources retain the same paths as local builds.
FROM node:24-bookworm-slim AS build
WORKDIR /src
COPY package.json pnpm-lock.yaml pnpm-workspace.yaml ./
COPY web/package.json web/package.json
RUN corepack enable && pnpm install --filter @quoin/web --frozen-lockfile
COPY web web
RUN pnpm --dir web build

FROM caddy:2.10.2-alpine AS config
# Caddy's official image accepts JSON at runtime. Keep YAML as the only checked-
# in source, then convert the embedded Caddyfile at build time with PyYAML rather
# than introducing a bespoke static-server framework.
COPY deploy/images/frontend/web-caddy.yaml /tmp/web-caddy.yaml
RUN apk add --no-cache py3-yaml && \
    python3 - <<'PY'
import pathlib
import subprocess
import yaml

source = yaml.safe_load(pathlib.Path('/tmp/web-caddy.yaml').read_text())
caddyfile = source.get('caddyfile')
if not isinstance(caddyfile, str) or not caddyfile.strip():
    raise SystemExit('web-caddy.yaml must contain a non-empty caddyfile string')
pathlib.Path('/tmp/Caddyfile').write_text(caddyfile)
with pathlib.Path('/out-caddy.json').open('w') as output:
    subprocess.run(['caddy', 'adapt', '--config', '/tmp/Caddyfile', '--adapter', 'caddyfile', '--pretty'], check=True, stdout=output)
PY

FROM caddy:2.10.2-alpine AS capless
# Caddy's stock binary carries cap_net_bind_service for :80/:443. This frontend
# only listens on 8080; retaining the file capability makes exec fail when the
# Pod drops all bounding capabilities and forbids privilege escalation.
RUN setcap -r /usr/bin/caddy

FROM caddy:2.10.2-alpine
# Copying the stripped executable into the fresh runtime image leaves the
# libcap tooling behind and avoids carrying an executable file capability.
COPY --from=capless /usr/bin/caddy /usr/bin/caddy
COPY --from=config /out-caddy.json /etc/caddy/caddy.json
COPY --from=build --chown=65532:65532 /src/web/dist /srv
# The official Alpine Caddy image deliberately has no named unprivileged user.
# Use the same fixed non-root UID as the other product images and grant it only
# Caddy's runtime state directories plus the immutable static document root.
RUN chown -R 65532:65532 /data /config
EXPOSE 8080
USER 65532:65532
CMD ["caddy", "run", "--config", "/etc/caddy/caddy.json"]
