# syntax=docker/dockerfile:1.7
# Test-only OTP delivery receiver image (deploy/images/otp-test): packages the
# scripts/test support fixture binary from test/support/otpdelivery. It serves
# the disposable e2e-real topology exclusively — an HTTPS webhook receiver and
# an implicit-TLS SMTP server that record every accepted delivery as JSONL so
# the harness can assert on codes. It never ships to production deployments and
# never contacts a real provider; the release build pipeline does not build it.
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
    GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags="-s -w" -o /out/otpdelivery ./test/support/otpdelivery

FROM alpine:3.22
RUN apk add --no-cache ca-certificates && \
    addgroup -g 65532 fixture && adduser -D -u 65532 -G fixture fixture
USER 65532:65532
COPY --from=build /out/otpdelivery /otpdelivery
EXPOSE 8445 8587
ENTRYPOINT ["/otpdelivery"]
