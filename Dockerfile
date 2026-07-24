# syntax=docker/dockerfile:1
ARG VERSION=0.1.5
FROM node:24.18.0-bookworm-slim AS web-build

WORKDIR /src/frontend
COPY frontend/package.json frontend/package-lock.json ./
RUN npm ci
COPY frontend/ ./
RUN npm run build

FROM golang:1.26-bookworm AS build

ARG VERSION
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web-build /src/internal/control/web/dist ./internal/control/web/dist
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w -X simple_cdn/internal/version.Version=${VERSION}" -o /out/cdn-control ./cmd/control \
    && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w -X simple_cdn/internal/version.Version=${VERSION}" -o /out/cdn-edge-agent-linux-amd64 ./cmd/edge-agent

FROM debian:12-slim

ARG VERSION
LABEL org.opencontainers.image.title="simple_cdn" \
      org.opencontainers.image.version="${VERSION}"

RUN apt-get update \
    && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        ca-certificates certbot curl python3-certbot-dns-cloudflare restic sqlite3 tzdata util-linux \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --gid 10001 cdn-platform \
    && useradd --uid 10001 --gid 10001 --home-dir /var/lib/cdn-platform --shell /usr/sbin/nologin cdn-platform

COPY --from=build /out/cdn-control /usr/local/bin/cdn-control
COPY --from=build /out/cdn-edge-agent-linux-amd64 /usr/local/lib/cdn-platform/cdn-edge-agent-linux-amd64
COPY scripts/compose-*.sh /usr/local/lib/cdn-platform/
RUN chmod 0755 /usr/local/lib/cdn-platform/compose-*.sh

USER 10001:10001
ENTRYPOINT ["/usr/local/bin/cdn-control"]
