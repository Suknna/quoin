# syntax=docker/dockerfile:1.7
# Formal Plinth image. The base is the digest-locked Debian 13 slim index from
# contracts/release-inputs.yaml and every directly installed package carries
# the full per-architecture version locked in contracts/plinth-worker-tools.yaml
# (OPS-IMAGE-003 / OPS-IMAGE-004 / OPS-IMAGE-006). ca-certificates is pinned
# outside that catalog because the supervisor needs a system trust store for
# outbound public TLS (model providers, Thanos, Kubernetes APIs); folding it
# into the machine lock belongs to the release-qualification lock amendment.
ARG DEBIAN_REPO=docker.io/library/debian
ARG DEBIAN_INDEX_DIGEST=sha256:3a39a0592364683e6bab97937b72cad5a8fa6dcbbee90edb3bb48c7f8e94f258
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
    GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags='-s -w -buildid=' -o /out/plinth ./cmd/plinth && \
    GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags='-s -w -buildid=' -o /out/quoin-healthcheck ./cmd/quoin-healthcheck
FROM ${DEBIAN_REPO}@${DEBIAN_INDEX_DIGEST}
ENV LANG=C.UTF-8 LC_ALL=C.UTF-8
ARG TARGETARCH
# The two install branches are the per-architecture projections of the frozen
# tool catalog; test/release/compose proves both equal the machine lock.
RUN apt-get update \
 && if [ "$TARGETARCH" = "arm64" ]; then apt-get install -y --no-install-recommends \
        bash=5.2.37-2+b9 coreutils=9.7-3 findutils=4.10.0-3 grep=3.11-4+b1 sed=4.9-2+deb13u1 \
        gawk=1:5.2.1-2+b2 jq=1.7.1-6+deb13u3 file=1:5.46-5 tar=1.35+dfsg-3.1 gzip=1.13-1 \
        xz-utils=5.8.1-1+deb13u1 unzip=6.0-29+deb13u1 ripgrep=14.1.1-1+b4 diffutils=1:3.10-4 \
        ca-certificates=20250419; \
    else apt-get install -y --no-install-recommends \
        bash=5.2.37-2+b9 coreutils=9.7-3 findutils=4.10.0-3 grep=3.11-4 sed=4.9-2+deb13u1 \
        gawk=1:5.2.1-2+b1 jq=1.7.1-6+deb13u3 file=1:5.46-5 tar=1.35+dfsg-3.1 gzip=1.13-1 \
        xz-utils=5.8.1-1+deb13u1 unzip=6.0-29+deb13u1 ripgrep=14.1.1-1+b4 diffutils=1:3.10-4 \
        ca-certificates=20250419; \
    fi \
    && rm -rf /var/lib/apt/lists/* \
    && test -x /bin/bash && test -x /usr/bin/awk && test -x /usr/bin/jq \
    && test -x /usr/bin/rg && test -x /usr/bin/file && test -x /usr/bin/unzip
USER 65532:65532
COPY --from=build /out/plinth /plinth
COPY --from=build /out/quoin-healthcheck /quoin-healthcheck
ENTRYPOINT ["/plinth"]
