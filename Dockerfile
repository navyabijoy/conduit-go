# ── Build stage ──are these────────────────────────────────────────────────────────────
FROM golang:1.26 AS builder
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
# modernc.org/sqlite is a pure-Go driver, so a static CGO-free binary works.
RUN CGO_ENABLED=0 go build -o /conduit-gateway ./cmd/gateway
# Distroless has no shell, so the data directory is created here.
RUN mkdir /data

# ── Runtime stage ────────────────────────────────────────────────────────────
FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app

COPY --from=builder /conduit-gateway /app/conduit-gateway
COPY ui ./ui
# 65532 is the "nonroot" user of the base image; it must own the database dir.
COPY --from=builder --chown=65532:65532 /data /data

# 8080: gateway API + UI. 8081: built-in mock provider.
EXPOSE 8080 8081
VOLUME ["/data"]

ENTRYPOINT ["/app/conduit-gateway", "-db", "/data/conduit.db"]
