## hiveqa — single deployment with headscale
## Packages hiveqad, hiveqactl, and headscale in one image.
##
## Usage:
##   docker build -t hiveqa .
##   docker run -e TS_AUTHKEY=... -e HIVEQA_CONTROL_URL=http://localhost:8080 \
##     -v /var/run/docker.sock:/var/run/docker.sock \
##     -v hiveqa-state:/data \
##     hiveqa
##
## For K8s / Cloud Run, see deploy/ directory.

# ---------- Stage 1: build hiveqa binaries ----------
FROM golang:1.24-alpine AS build-hiveqa
RUN apk add --no-cache git
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/hiveqad  ./cmd/hiveqad \
 && CGO_ENABLED=0 go build -o /out/hiveqactl ./cmd/hiveqactl

# ---------- Stage 2: fetch headscale binary ----------
FROM alpine:3.21 AS fetch-headscale
ARG HEADSCALE_VERSION=0.28.0
ARG TARGETARCH
RUN apk add --no-cache curl \
 && curl -fsSL "https://github.com/juanfont/headscale/releases/download/v${HEADSCALE_VERSION}/headscale_${HEADSCALE_VERSION}_linux_${TARGETARCH}" \
    -o /headscale \
 && chmod +x /headscale

# ---------- Stage 3: runtime ----------
FROM alpine:3.21

RUN apk add --no-cache \
    ca-certificates \
    docker-cli \
    docker-cli-compose \
    tini

COPY --from=build-hiveqa /out/hiveqad  /usr/local/bin/hiveqad
COPY --from=build-hiveqa /out/hiveqactl /usr/local/bin/hiveqactl
COPY --from=fetch-headscale /headscale /usr/local/bin/headscale

# Default headscale config — can be overridden via mount.
COPY deploy/headscale.yaml /etc/headscale/config.yaml

# Entrypoint script manages headscale + hiveqad lifecycle.
COPY deploy/entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

# State volumes.
VOLUME /data/headscale
VOLUME /data/hiveqa

ENV HIVEQA_STATE_DIR=/data/hiveqa
ENV HEADSCALE_CONFIG=/etc/headscale/config.yaml

# Headscale control port + DERP + metrics.
EXPOSE 8080 3478/udp 9090

ENTRYPOINT ["tini", "--", "/entrypoint.sh"]
