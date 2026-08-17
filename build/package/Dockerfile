# syntax=docker/dockerfile:1.7
FROM golang:1.26.1-alpine3.22 AS build
WORKDIR /src
RUN apk add --no-cache ca-certificates
COPY go.mod go.sum ./
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

FROM scratch AS plinth
USER 65532:65532
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/plinth /plinth
COPY --from=build /out/quoin-healthcheck /quoin-healthcheck
ENTRYPOINT ["/plinth"]

FROM scratch AS lintel
USER 65532:65532
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/lintel /lintel
COPY --from=build /out/quoin-healthcheck /quoin-healthcheck
ENTRYPOINT ["/lintel"]

FROM scratch AS stele
USER 65532:65532
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/stele /stele
COPY --from=build /out/quoin-healthcheck /quoin-healthcheck
ENTRYPOINT ["/stele"]
