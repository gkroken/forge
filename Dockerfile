# syntax=docker/dockerfile:1

# ── Stage 1: build ──────────────────────────────────────────────────────────
FROM golang:1.26-alpine AS builder
WORKDIR /src

# Cache module downloads separately from source
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build \
      -trimpath \
      -ldflags="-s -w" \
      -o /forge \
      ./cmd/forge

# Create /data owned by the nonroot uid so Docker initialises named volumes
# with the right permissions on first use.
RUN mkdir /data

# ── Stage 2: runtime ────────────────────────────────────────────────────────
# Distroless static: no shell, no package manager, non-root (uid 65532).
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /forge /forge
# /data must be owned by nonroot before the VOLUME declaration so that Docker
# propagates this ownership when it initialises an empty named volume.
COPY --from=builder --chown=nonroot:nonroot /data /data

# The data dir comes from the environment, not CMD: any `docker run` argument
# replaces CMD wholesale, and `-addr :80` alone would otherwise drop the data dir
# back to the flag default ./data (read-only under --read-only).
ENV FORGE_DATA=/data

EXPOSE 8080
VOLUME ["/data"]

ENTRYPOINT ["/forge"]
