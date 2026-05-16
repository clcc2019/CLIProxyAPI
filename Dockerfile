FROM golang:1.26.2-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./

RUN go mod download

COPY . .

ARG VERSION=dev
ARG COMMIT=none
ARG BUILD_DATE=unknown

# Include all optional backends (Postgres / Minio / Git / Redis / TUI) in
# Docker images by default. Operators who want smaller images can override
# BUILD_TAGS at build time, e.g.:
#   docker build --build-arg BUILD_TAGS=has_postgres
#   docker build --build-arg BUILD_TAGS=             # truly minimal
# -trimpath strips the absolute build path for reproducibility; -s -w drop
# the DWARF + symbol table (pclntab for stack traces is kept).
ARG BUILD_TAGS=has_postgres,has_minio,has_git,has_redis,has_tui

RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -tags="${BUILD_TAGS}" \
    -ldflags="-s -w -X 'main.Version=${VERSION}' -X 'main.Commit=${COMMIT}' -X 'main.BuildDate=${BUILD_DATE}'" \
    -o ./CLIProxyAPI ./cmd/server/

FROM bitnami/minideb:latest

# RUN apk add --no-cache tzdata
RUN install_packages ca-certificates curl wget

RUN mkdir /CLIProxyAPI

COPY --from=builder ./app/CLIProxyAPI /CLIProxyAPI/CLIProxyAPI

COPY config.example.yaml /CLIProxyAPI/config.example.yaml

# Run as a dedicated non-root user. The proxy needs no privileged operations,
# and this limits the blast radius of any future RCE-class bug. /CLIProxyAPI
# is owned by the new user so config & auth files written at runtime work.
RUN groupadd --system --gid 10001 cliproxy \
 && useradd --system --uid 10001 --gid cliproxy --home-dir /CLIProxyAPI --shell /usr/sbin/nologin cliproxy \
 && chown -R cliproxy:cliproxy /CLIProxyAPI

WORKDIR /CLIProxyAPI

EXPOSE 8317

ENV TZ=Asia/Tokyo

# RUN cp /usr/share/zoneinfo/${TZ} /etc/localtime && echo "${TZ}" > /etc/timezone

# Liveness probe at /healthz (always 200 once the listener is bound).
# Readiness is /readyz; orchestrators should prefer that for routing.
HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
    CMD curl --fail --silent --max-time 4 http://127.0.0.1:8317/healthz || exit 1

USER cliproxy

CMD ["./CLIProxyAPI"]
