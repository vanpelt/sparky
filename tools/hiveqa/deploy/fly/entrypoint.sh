#!/bin/sh
set -e

echo "=== headscale on Fly.io ==="

# Ensure data directory exists on the persistent volume.
mkdir -p /data/headscale

# Override server_url from env if provided.
if [ -n "$HEADSCALE_SERVER_URL" ]; then
    sed -i "s|server_url:.*|server_url: \"${HEADSCALE_SERVER_URL}\"|" /etc/headscale/config.yaml
    echo "server_url set to $HEADSCALE_SERVER_URL"
fi

echo "Starting headscale..."
exec headscale serve
