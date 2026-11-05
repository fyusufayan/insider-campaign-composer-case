# syntax=docker/dockerfile:1
#
# One backend image, several entrypoints (ADR-001): /app/api, /app/migrate,
# /app/seed, /app/worker and /app/mockprovider. Compose selects the entrypoint
# per service with `command`.

ARG GO_VERSION=1.25
ARG ALPINE_VERSION=3.22

# ---- build -----------------------------------------------------------------
FROM golang:${GO_VERSION}-alpine AS build
WORKDIR /src
ENV CGO_ENABLED=0 GOFLAGS=-trimpath

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .
# Build caches live in BuildKit cache mounts and never reach an image layer.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go build -ldflags="-s -w" -o /out/ ./cmd/api ./cmd/migrate ./cmd/seed ./cmd/worker ./cmd/mockprovider

# ---- runtime ---------------------------------------------------------------
# Alpine (not scratch/distroless) so the Compose health check can use busybox
# wget; CA certificates are needed for the Anthropic API from Slice 1b on.
FROM alpine:${ALPINE_VERSION}
RUN apk add --no-cache ca-certificates tzdata \
 && adduser -D -H -u 10001 -s /sbin/nologin app \
 && install -d -o 10001 -g 10001 -m 0700 /var/lib/mockprovider
# /var/lib/mockprovider: the mock provider's durable idempotency ledger; a
# named Compose volume is mounted there and inherits this ownership.
COPY --from=build /out/ /app/
USER 10001:10001
WORKDIR /app
EXPOSE 8080
CMD ["/app/api"]
