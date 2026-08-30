# syntax=docker/dockerfile:1.7
# Formal Stele image. The base is the digest-locked distroless index from
# contracts/release-inputs.yaml (OPS-IMAGE-003); see deploy/images/quoin for
# the repository-ARG and cross-compile rationale.
ARG DISTROLESS_REPO=gcr.io/distroless/static-debian13
ARG DISTROLESS_INDEX_DIGEST=sha256:f7f8f729987ad0fdf6b05eeeae94b26e6a0f613bdf46feea7fc40f7bd72953e6
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
    GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags='-s -w -buildid=' -o /out/stele ./cmd/stele && \
    GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags='-s -w -buildid=' -o /out/quoin-healthcheck ./cmd/quoin-healthcheck
FROM ${DISTROLESS_REPO}@${DISTROLESS_INDEX_DIGEST}
USER 65532:65532
COPY --from=build /out/stele /stele
COPY --from=build /out/quoin-healthcheck /quoin-healthcheck
ENTRYPOINT ["/stele"]
