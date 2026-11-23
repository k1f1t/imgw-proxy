# syntax=docker/dockerfile:1

FROM golang:1.26-alpine AS builder
WORKDIR /src

RUN apk add --no-cache ca-certificates tzdata

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/imgw-openmeteo-proxy ./cmd/server

FROM alpine:3.21
RUN apk add --no-cache ca-certificates tzdata wget \
    && adduser -D -H -u 10001 appuser \
    && mkdir -p /data \
    && chown appuser:appuser /data

COPY --from=builder /out/imgw-openmeteo-proxy /usr/local/bin/imgw-openmeteo-proxy

USER appuser
WORKDIR /data

ENV LISTEN_ADDR=0.0.0.0:8080 \
    DB_PATH=/data/imgw.db \
    TZ=Europe/Warsaw

EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
    CMD wget -qO- http://127.0.0.1:8080/healthz >/dev/null 2>&1 || exit 1

ENTRYPOINT ["/usr/local/bin/imgw-openmeteo-proxy"]
