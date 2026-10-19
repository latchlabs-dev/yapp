# Build stage — pinned to the BUILD host's native arch; Go cross-compiles to the
# target arch via GOARCH, so multi-arch builds need no QEMU emulation.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS builder

RUN apk add --no-cache git ca-certificates

WORKDIR /src
COPY go.mod go.sum ./
COPY third_party/duffel/go.mod third_party/duffel/go.sum ./third_party/duffel/
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal
COPY third_party ./third_party
COPY migrations ./migrations
COPY extensions/corp-proxy.ts ./extensions/corp-proxy.ts

ARG VERSION=dev
ARG COMMIT=unknown
ARG DATE=unknown
# Provided automatically by buildx for the image being produced.
ARG TARGETOS
ARG TARGETARCH

RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build \
    -ldflags "-X github.com/earlgrey/piproxy/internal/config.Version=${VERSION} \
              -X github.com/earlgrey/piproxy/internal/config.Commit=${COMMIT} \
              -X github.com/earlgrey/piproxy/internal/config.BuildDate=${DATE}" \
    -o /usr/local/bin/proxy ./cmd/proxy

RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build \
    -ldflags "-X github.com/earlgrey/piproxy/internal/config.Version=${VERSION} \
              -X github.com/earlgrey/piproxy/internal/config.Commit=${COMMIT} \
              -X github.com/earlgrey/piproxy/internal/config.BuildDate=${DATE}" \
    -o /usr/local/bin/admin ./cmd/admin

# Runtime stage
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -o /usr/local/bin/subscription-reconcile ./cmd/subscription-reconcile
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -o /usr/local/bin/sessionarchive ./cmd/sessionarchive
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -o /usr/local/bin/pdfextract ./cmd/pdfextract

FROM alpine:3.21

RUN apk add --no-cache ca-certificates tzdata

RUN addgroup -S proxy && adduser -S proxy -G proxy && \
    addgroup -S -g 10001 hooks && addgroup proxy hooks && \
    install -d -m 0750 -o proxy -g hooks /var/lib/piproxy/hooks

COPY --from=builder /usr/local/bin/proxy /usr/local/bin/proxy
COPY --from=builder /usr/local/bin/admin /usr/local/bin/admin
COPY --from=builder /usr/local/bin/subscription-reconcile /usr/local/bin/subscription-reconcile
COPY --from=builder /usr/local/bin/sessionarchive /usr/local/bin/sessionarchive
COPY --from=builder /usr/local/bin/pdfextract /usr/local/bin/pdfextract
COPY --from=builder /src/migrations /migrations
COPY --from=builder /src/extensions /app/extensions

USER proxy

EXPOSE 8080 8081 9090

ENTRYPOINT ["/usr/local/bin/proxy"]
