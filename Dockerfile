# syntax=docker/dockerfile:1

# Build stage: compile the Go service with the pinned toolchain.
FROM golang:1.23-alpine AS build

WORKDIR /src

# Cache module downloads first so dependency resolution is not re-run on every
# source change.
COPY go.mod go.sum ./
RUN go mod download

COPY cmd/ ./cmd/
COPY internal/ ./internal/

# Build a static binary. CGO is disabled so the binary runs on a minimal
# scratch/alpine runtime without a C toolchain.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/pii-service ./cmd/pii-service

# Runtime stage: minimal non-root image. Alpine provides busybox wget for the
# healthcheck and a shell for the entrypoint.
FROM alpine:3.20

RUN addgroup -S -g 10001 pii && adduser -S -D -H -u 10001 -G pii pii

COPY --from=build /out/pii-service /usr/local/bin/pii-service

USER pii

EXPOSE 8080 9464

# wget is present in the base alpine image (busybox). The healthcheck probes
# the live endpoint; it never sends or logs request/response bodies.
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD wget -q -O /dev/null http://127.0.0.1:8080/health/live || exit 1

ENTRYPOINT ["/usr/local/bin/pii-service"]