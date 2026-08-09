# ── Build stage ──────────────────────────────────────────────────────────────
FROM golang:1.26 AS builder
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
# modernc.org/sqlite is a pure-Go driver, so a static CGO-free binary works.
RUN CGO_ENABLED=0 go build -o /conduit-gateway ./cmd/gateway

# ── Runtime stage ────────────────────────────────────────────────────────────
FROM gcr.io/distroless/static-debian12
WORKDIR /app

COPY --from=builder /conduit-gateway /app/conduit-gateway
COPY ui ./ui

# 8080: gateway API + UI. 8081: built-in mock provider (used when -mock=true).
EXPOSE 8080 8081
VOLUME ["/data"]

ENTRYPOINT ["/app/conduit-gateway", "-db", "/data/conduit.db"]
