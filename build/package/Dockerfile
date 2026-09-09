# syntax=docker/dockerfile:1.7
FROM golang:1.26.1-alpine3.22 AS build
WORKDIR /src
RUN apk add --no-cache ca-certificates
COPY go.mod go.sum ./
# GOPROXY is a build argument so restricted networks can point the image
# build at their module mirror (e.g. --build-arg GOPROXY=https://goproxy.cn,direct);
# the default matches Go's own default.
ARG GOPROXY=https://proxy.golang.org,direct
ENV GOPROXY=${GOPROXY}
RUN go mod download
COPY . .
ARG TARGETOS=linux
ARG TARGETARCH
# Release identity is build provenance, not a runtime compatibility check.
ARG RELEASE_VERSION=v0.1.0-dev
ENV CGO_ENABLED=0
RUN --mount=type=cache,target=/root/.cache/go-build \
    GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags="-s -w -buildid= -X github.com/Suknna/quoin/internal/buildinfo.Release=${RELEASE_VERSION}" -o /out/quoin ./cmd/quoin && \
    GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags="-s -w -buildid= -X github.com/Suknna/quoin/internal/buildinfo.Release=${RELEASE_VERSION}" -o /out/plinth ./cmd/plinth && \
    GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags="-s -w -buildid= -X github.com/Suknna/quoin/internal/buildinfo.Release=${RELEASE_VERSION}" -o /out/lintel ./cmd/lintel && \
    GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags="-s -w -buildid= -X github.com/Suknna/quoin/internal/buildinfo.Release=${RELEASE_VERSION}" -o /out/stele ./cmd/stele && \
    GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags="-s -w -buildid= -X github.com/Suknna/quoin/internal/buildinfo.Release=${RELEASE_VERSION}" -o /out/quoin-healthcheck ./cmd/quoin-healthcheck

FROM scratch AS quoin
USER 65532:65532
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/quoin /quoin
COPY --from=build /out/quoin-healthcheck /quoin-healthcheck
ENTRYPOINT ["/quoin"]

FROM debian:13-slim AS plinth-base
# The frozen worker tool catalog (contracts/plinth-worker-tools.yaml, owner
# OPS-IMAGE-004): the sandboxed agent worker executes this exact tool set
# inside the image; the Landlock read-only rules anchor the same paths.
RUN apt-get update && apt-get install -y --no-install-recommends \
      bash coreutils findutils grep sed gawk jq file tar gzip xz-utils unzip ripgrep diffutils ca-certificates \
    && rm -rf /var/lib/apt/lists/*

FROM plinth-base AS plinth
USER 65532:65532
COPY --from=build /out/plinth /plinth
COPY --from=build /out/quoin-healthcheck /quoin-healthcheck
ENTRYPOINT ["/plinth"]

FROM node:24-bookworm-slim AS journey-node
WORKDIR /src
COPY package.json pnpm-lock.yaml pnpm-workspace.yaml ./
COPY web/package.json web/package.json
# The Lintel-owned Chromium remains the only browser binary. Playwright's
# library controls its loopback CDP endpoint, so installation must not download
# a second browser distribution (https://playwright.dev/docs/library).
RUN corepack enable && PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD=1 pnpm install --filter @quoin/web --prod --frozen-lockfile

FROM node:24-bookworm-slim AS lintel
# Chromium's dependency set is large; tolerate bounded transient mirror resets
# without accepting missing packages or weakening APT signature verification.
RUN apt-get -o Acquire::Retries=3 update && \
    apt-get -o Acquire::Retries=3 install -y --no-install-recommends \
      chromium xvfb tigervnc-scraping-server ca-certificates \
    && rm -rf /var/lib/apt/lists/*
# pnpm's workspace links in /web/node_modules resolve through the workspace
# store at /node_modules; retain both ends instead of flattening dependencies.
COPY --from=journey-node /src/node_modules /node_modules
COPY --from=journey-node /src/web/node_modules /web/node_modules
COPY internal/lintel/browser/journey/playwright-runner.mjs /web/journey-runner.mjs
USER 65532:65532
COPY --from=build /out/lintel /lintel
COPY --from=build /out/quoin-healthcheck /quoin-healthcheck
ENTRYPOINT ["/lintel"]

FROM scratch AS stele
USER 65532:65532
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/stele /stele
COPY --from=build /out/quoin-healthcheck /quoin-healthcheck
ENTRYPOINT ["/stele"]

FROM node:24-bookworm-slim AS web-build
WORKDIR /src
COPY package.json pnpm-lock.yaml pnpm-workspace.yaml ./
COPY web/package.json web/package.json
RUN corepack enable && pnpm install --filter @quoin/web --frozen-lockfile
COPY web web
RUN pnpm --dir web build

FROM caddy:2.10.2-alpine AS web-config
# Caddy's official image accepts JSON at runtime. Keep YAML as the only checked-
# in source, then convert the embedded Caddyfile at build time with PyYAML rather
# than introducing a bespoke static-server framework.
COPY build/package/web-caddy.yaml /tmp/web-caddy.yaml
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

FROM caddy:2.10.2-alpine AS web-capless
# Caddy's stock binary carries cap_net_bind_service for :80/:443. This frontend
# only listens on 8080; retaining the file capability makes exec fail when the
# Pod drops all bounding capabilities and forbids privilege escalation.
RUN setcap -r /usr/bin/caddy

FROM caddy:2.10.2-alpine AS web
# Copying the stripped executable into the fresh runtime image leaves the
# libcap tooling behind and avoids carrying an executable file capability.
COPY --from=web-capless /usr/bin/caddy /usr/bin/caddy
COPY --from=web-config /out-caddy.json /etc/caddy/caddy.json
COPY --from=web-build --chown=65532:65532 /src/web/dist /srv
# The official Alpine Caddy image deliberately has no named unprivileged user.
# Use the same fixed non-root UID as the other product images and grant it only
# Caddy's runtime state directories plus the immutable static document root.
RUN chown -R 65532:65532 /data /config
EXPOSE 8080
USER 65532:65532
CMD ["caddy", "run", "--config", "/etc/caddy/caddy.json"]
