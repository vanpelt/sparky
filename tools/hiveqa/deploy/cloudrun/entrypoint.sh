#!/bin/sh
set -e

echo "=== headscale on Cloud Run ==="

# Cloud Run sets PORT; headscale needs to listen on it.
LISTEN_PORT="${PORT:-8080}"

# Patch listen_addr if PORT differs from default.
if [ "$LISTEN_PORT" != "8080" ]; then
    sed -i "s/listen_addr: .*/listen_addr: \"0.0.0.0:${LISTEN_PORT}\"/" /etc/headscale/config.yaml
fi

# Headscale needs a writable directory for noise keys.
# Cloud Run provides /tmp as writable.
mkdir -p /tmp/headscale
export HEADSCALE_NOISE_PRIVATE_KEY_PATH="/tmp/headscale/noise_private.key"

# If keys are provided via secrets, write them.
if [ -n "$HEADSCALE_NOISE_KEY" ]; then
    echo "$HEADSCALE_NOISE_KEY" > "$HEADSCALE_NOISE_PRIVATE_KEY_PATH"
fi

echo "Starting headscale on :${LISTEN_PORT}"
exec headscale serve
