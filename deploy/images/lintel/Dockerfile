# syntax=docker/dockerfile:1.7
# Formal Lintel image. The base is the digest-locked Debian 13 slim index and
# every installed package carries the full per-architecture version locked in
# contracts/release-inputs.yaml lintel_runtime (OPS-IMAGE-003/006). The
# Playwright Chromium is downloaded at build time from the locked
# cdn.playwright.dev artifacts and SHA-256 verified before unpacking
# (OPS-IMAGE-002); nothing is installed or downloaded at container start.
# The Node runtime and playwright-core come from the frozen pnpm lock; the
# journey runner lives at the fixed /web/journey-runner.mjs path the Lintel
# binary resolves.
ARG DEBIAN_REPO=docker.io/library/debian
ARG DEBIAN_INDEX_DIGEST=sha256:3a39a0592364683e6bab97937b72cad5a8fa6dcbbee90edb3bb48c7f8e94f258
ARG CHROMIUM_AMD64_URL=https://cdn.playwright.dev/builds/cft/151.0.7922.34/linux64/chrome-linux64.zip
ARG CHROMIUM_AMD64_SHA256=ae8736ac28bc69278551500f219fc749575648263c43ec5990749eff43b9fcf8
ARG CHROMIUM_ARM64_URL=https://cdn.playwright.dev/builds/chromium/1234/chromium-linux-arm64.zip
ARG CHROMIUM_ARM64_SHA256=b5ad7d8fe70f230b34198ddb5626d717c016db2f627cb44b922babbcaf3479b9

FROM --platform=$BUILDPLATFORM golang:1.26.1-alpine3.22 AS build
WORKDIR /src
COPY go.mod go.sum ./
ARG GOPROXY=https://proxy.golang.org,direct
ENV GOPROXY=${GOPROXY}
RUN go mod download
COPY . .
ARG TARGETOS=linux
ARG TARGETARCH
ENV CGO_ENABLED=0
RUN --mount=type=cache,target=/root/.cache/go-build \
    GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags='-s -w -buildid=' -o /out/lintel ./cmd/lintel && \
    GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags='-s -w -buildid=' -o /out/quoin-healthcheck ./cmd/quoin-healthcheck

# The locked Chromium artifact for the target architecture is fetched and
# hash-verified on the build platform; it is only ever extracted, never
# executed, on this stage.
FROM --platform=$BUILDPLATFORM debian:13-slim AS chromium
ARG CHROMIUM_AMD64_URL
ARG CHROMIUM_AMD64_SHA256
ARG CHROMIUM_ARM64_URL
ARG CHROMIUM_ARM64_SHA256
ARG TARGETARCH
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl unzip \
 && rm -rf /var/lib/apt/lists/* \
 && if [ "$TARGETARCH" = "arm64" ]; then url="$CHROMIUM_ARM64_URL"; sha256="$CHROMIUM_ARM64_SHA256"; \
    else url="$CHROMIUM_AMD64_URL"; sha256="$CHROMIUM_AMD64_SHA256"; fi \
 && curl -fsSL "$url" -o /tmp/chromium.zip \
 && printf '%s  %s\n' "$sha256" /tmp/chromium.zip | sha256sum -c - \
 && mkdir -p /browser \
 && unzip -q /tmp/chromium.zip -d /browser \
 && rm /tmp/chromium.zip \
 && entry=$(find /browser -maxdepth 2 -type f \( -name chrome -o -name chromium \) | head -n 1) \
 && test -n "$entry"

# playwright-core is installed from the frozen pnpm workspace lock with the
# browser download explicitly skipped: Lintel owns the one Chromium above.
FROM --platform=$BUILDPLATFORM node:24-bookworm-slim AS web-deps
WORKDIR /src
COPY package.json pnpm-lock.yaml pnpm-workspace.yaml ./
COPY web/package.json web/package.json
RUN corepack enable && PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD=1 pnpm install --filter @quoin/web --prod --frozen-lockfile

FROM ${DEBIAN_REPO}@${DEBIAN_INDEX_DIGEST}
ENV LANG=C.UTF-8 LC_ALL=C.UTF-8
ARG TARGETARCH
# The two install branches are the per-architecture projections of the frozen
# lintel_runtime package lock; test/release/subjects proves both equal the
# machine lock, and the locked key executables are verified in place.
RUN apt-get update \
 && if [ "$TARGETARCH" = "arm64" ]; then apt-get install -y --no-install-recommends \
        ca-certificates=20250419 \
        fonts-freefont-ttf=20211204+svn4273-2 \
        fonts-ipafont-gothic=00303-23 \
        fonts-liberation=1:2.1.5-3 \
        fonts-noto-color-emoji=2.051-0+deb13u1 \
        fonts-tlwg-loma-otf=1:0.7.3-1 \
        fonts-unifont=1:15.1.01-1 \
        fonts-wqy-zenhei=0.9.45-8 \
        libasound2t64=1.2.14-1 \
        libatk-bridge2.0-0t64=2.56.2-1+deb13u1 \
        libatk1.0-0t64=2.56.2-1+deb13u1 \
        libatspi2.0-0t64=2.56.2-1+deb13u1 \
        libcairo2=1.18.4-1+b1 \
        libcups2t64=2.4.10-3+deb13u2 \
        libdbus-1-3=1.16.2-2 \
        libdrm2=2.4.124-2 \
        libfontconfig1=2.15.0-2.3 \
        libfreetype6=2.13.3+dfsg-1+deb13u1 \
        libgbm1=25.0.7-2+deb13u1 \
        libglib2.0-0t64=2.84.4-3~deb13u3 \
        libnspr4=2:4.36-1 \
        libnss3=2:3.110-1+deb13u4 \
        libpango-1.0-0=1.56.3-1 \
        libx11-6=2:1.8.12-1 \
        libxcb1=1.17.0-2+b1 \
        libxcomposite1=1:0.4.6-1 \
        libxdamage1=1:1.1.6-1+b2 \
        libxext6=2:1.3.4-1+b3 \
        libxfixes3=1:6.0.0-2+b4 \
        libxkbcommon0=1.7.0-2 \
        libxrandr2=2:1.5.4-1+b3 \
        nodejs=20.19.2+dfsg-1+deb13u2 \
        tigervnc-scraping-server=1.15.0+dfsg-2.1~deb13u1 \
        xfonts-scalable=1:1.0.3-1.3 \
        xvfb=2:21.1.16-1.3+deb13u3; \
    else apt-get install -y --no-install-recommends \
        ca-certificates=20250419 \
        fonts-freefont-ttf=20211204+svn4273-2 \
        fonts-ipafont-gothic=00303-23 \
        fonts-liberation=1:2.1.5-3 \
        fonts-noto-color-emoji=2.051-0+deb13u1 \
        fonts-tlwg-loma-otf=1:0.7.3-1 \
        fonts-unifont=1:15.1.01-1 \
        fonts-wqy-zenhei=0.9.45-8 \
        libasound2t64=1.2.14-1 \
        libatk-bridge2.0-0t64=2.56.2-1+deb13u1 \
        libatk1.0-0t64=2.56.2-1+deb13u1 \
        libatspi2.0-0t64=2.56.2-1+deb13u1 \
        libcairo2=1.18.4-1+b1 \
        libcups2t64=2.4.10-3+deb13u2 \
        libdbus-1-3=1.16.2-2 \
        libdrm2=2.4.124-2 \
        libfontconfig1=2.15.0-2.3 \
        libfreetype6=2.13.3+dfsg-1+deb13u1 \
        libgbm1=25.0.7-2+deb13u1 \
        libglib2.0-0t64=2.84.4-3~deb13u3 \
        libnspr4=2:4.36-1 \
        libnss3=2:3.110-1+deb13u4 \
        libpango-1.0-0=1.56.3-1 \
        libx11-6=2:1.8.12-1 \
        libxcb1=1.17.0-2+b1 \
        libxcomposite1=1:0.4.6-1 \
        libxdamage1=1:1.1.6-1+b2 \
        libxext6=2:1.3.4-1+b3 \
        libxfixes3=1:6.0.0-2+b4 \
        libxkbcommon0=1.7.0-2 \
        libxrandr2=2:1.5.4-1+b3 \
        nodejs=20.19.2+dfsg-1+deb13u2 \
        tigervnc-scraping-server=1.15.0+dfsg-2.1~deb13u1 \
        xfonts-scalable=1:1.0.3-1.3 \
        xvfb=2:21.1.16-1.3+deb13u3; \
    fi \
    && rm -rf /var/lib/apt/lists/* \
    && test -x /usr/bin/Xvfb && test -x /usr/bin/x0vncserver && test -x /usr/bin/node
COPY --from=chromium /browser /usr/lib/quoin/chromium
RUN entry=$(find /usr/lib/quoin/chromium -maxdepth 2 -type f \( -name chrome -o -name chromium \) | head -n 1) \
    && test -n "$entry" \
    && ln -s "$entry" /usr/bin/chromium \
    && test -x /usr/bin/chromium
# pnpm's workspace links in /web/node_modules resolve through the workspace
# store at /node_modules; retain both ends instead of flattening dependencies.
COPY --from=web-deps /src/node_modules /node_modules
COPY --from=web-deps /src/web/node_modules /web/node_modules
COPY internal/lintel/browser/journey/playwright-runner.mjs /web/journey-runner.mjs
USER 65532:65532
COPY --from=build /out/lintel /lintel
COPY --from=build /out/quoin-healthcheck /quoin-healthcheck
ENTRYPOINT ["/lintel"]
