#!/bin/sh
set -e

echo "=== hiveqa single-deployment entrypoint ==="

# --- Headscale setup ---
HEADSCALE_DATA="/data/headscale"
mkdir -p "$HEADSCALE_DATA"

# Start headscale in the background.
echo "Starting headscale..."
headscale serve &
HEADSCALE_PID=$!

# Wait for headscale to be ready.
echo "Waiting for headscale to be ready..."
for i in $(seq 1 30); do
    if headscale nodes list >/dev/null 2>&1; then
        echo "Headscale is ready."
        break
    fi
    if [ "$i" -eq 30 ]; then
        echo "ERROR: headscale did not become ready in 30s"
        exit 1
    fi
    sleep 1
done

# Create a default user if it doesn't exist.
headscale users create default 2>/dev/null || true

# Generate an auth key for hiveqa if TS_AUTHKEY is not already set.
if [ -z "$TS_AUTHKEY" ]; then
    echo "No TS_AUTHKEY set, generating one from headscale..."
    TS_AUTHKEY=$(headscale preauthkeys create --user default --reusable --ephemeral --expiration 24h 2>/dev/null)
    export TS_AUTHKEY
    echo "Generated ephemeral auth key."
fi

# Set control URL to local headscale if not overridden.
if [ -z "$HIVEQA_CONTROL_URL" ]; then
    HIVEQA_CONTROL_URL="http://localhost:8080"
    export HIVEQA_CONTROL_URL
fi

# --- hiveqad setup ---
echo "Starting hiveqad..."
echo "  Control URL: $HIVEQA_CONTROL_URL"
echo "  TLS mode:    ${HIVEQA_TLS:-tailscale}"
echo "  State dir:   ${HIVEQA_STATE_DIR:-/data/hiveqa}"

# If using headscale locally, default to TLS=none (headscale can't do cert provisioning).
# The external load balancer (Cloud Run / K8s ingress) handles TLS termination.
if [ -z "$HIVEQA_TLS" ] && [ "$HIVEQA_CONTROL_URL" = "http://localhost:8080" ]; then
    HIVEQA_TLS="none"
    export HIVEQA_TLS
    echo "  Auto-set TLS=none (headscale mode, use external TLS termination)"
fi

hiveqad --control-url="$HIVEQA_CONTROL_URL" &
HIVEQAD_PID=$!

# --- Signal handling ---
cleanup() {
    echo "Shutting down..."
    kill "$HIVEQAD_PID" 2>/dev/null || true
    kill "$HEADSCALE_PID" 2>/dev/null || true
    wait "$HIVEQAD_PID" 2>/dev/null || true
    wait "$HEADSCALE_PID" 2>/dev/null || true
    echo "Goodbye."
}
trap cleanup TERM INT

# Wait for either process to exit.
wait -n "$HEADSCALE_PID" "$HIVEQAD_PID" 2>/dev/null
EXIT_CODE=$?
echo "A process exited with code $EXIT_CODE, shutting down..."
cleanup
exit $EXIT_CODE
