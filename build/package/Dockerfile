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
ENV CGO_ENABLED=0
RUN --mount=type=cache,target=/root/.cache/go-build \
    GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags='-s -w -buildid=' -o /out/quoin ./cmd/quoin && \
    GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags='-s -w -buildid=' -o /out/plinth ./cmd/plinth && \
    GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags='-s -w -buildid=' -o /out/lintel ./cmd/lintel && \
    GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags='-s -w -buildid=' -o /out/stele ./cmd/stele && \
    GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags='-s -w -buildid=' -o /out/quoin-healthcheck ./cmd/quoin-healthcheck

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
