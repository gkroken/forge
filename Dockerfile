# syntax=docker/dockerfile:1

# ── Stage 1: build ──────────────────────────────────────────────────────────
FROM golang:1.25-alpine AS builder
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

EXPOSE 8080
VOLUME ["/data"]

ENTRYPOINT ["/forge"]
CMD ["-addr", ":8080", "-data", "/data"]
