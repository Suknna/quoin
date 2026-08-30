# syntax=docker/dockerfile:1.7
# Formal Quoin image. The base is the digest-locked distroless index from
# contracts/release-inputs.yaml (OPS-IMAGE-003). The repository ARG defaults
# to the locked gcr.io authority; a digest-identical mirror may be substituted
# on constrained networks because the digest, never the repository name, is
# the authority. The build stage runs on the build platform and
# cross-compiles, so foreign-architecture builds only emulate the final
# COPY-only stage.
ARG DISTROLESS_REPO=gcr.io/distroless/static-debian13
ARG DISTROLESS_INDEX_DIGEST=sha256:f7f8f729987ad0fdf6b05eeeae94b26e6a0f613bdf46feea7fc40f7bd72953e6
FROM --platform=$BUILDPLATFORM golang:1.26.1-alpine3.22 AS build
WORKDIR /src
COPY go.mod go.sum ./
# GOPROXY is a build argument so restricted networks can point the image
# build at their module mirror; the default matches Go's own default.
ARG GOPROXY=https://proxy.golang.org,direct
ENV GOPROXY=${GOPROXY}
RUN go mod download
COPY . .
ARG TARGETOS=linux
ARG TARGETARCH
ENV CGO_ENABLED=0
RUN --mount=type=cache,target=/root/.cache/go-build \
    GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags='-s -w -buildid=' -o /out/quoin ./cmd/quoin && \
    GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags='-s -w -buildid=' -o /out/quoin-healthcheck ./cmd/quoin-healthcheck
FROM ${DISTROLESS_REPO}@${DISTROLESS_INDEX_DIGEST}
USER 65532:65532
COPY --from=build /out/quoin /quoin
COPY --from=build /out/quoin-healthcheck /quoin-healthcheck
ENTRYPOINT ["/quoin"]
