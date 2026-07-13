#!/usr/bin/env bash
# Launch a fresh Scaleway Elastic Metal host that provisions itself into the
# sparkbox fleet via cloud-init — no SSH, no build. Renders cloud-init.yaml with
# your secrets + a target artifact release, then creates and installs the server.
#
# Prereqs: scw CLI configured; a published release (hack/build-artifacts.sh); the
# fleet gateway PRIVATE keys whose public halves are baked into that release's
# rootfs. Point GATEWAY_HOST_KEY / GATEWAY_UPSTREAM_KEY at those PEM files.
#
# Usage:
#   GATEWAY_HOST_KEY=secrets/gateway_host_key.pem \
#   GATEWAY_UPSTREAM_KEY=secrets/gateway_upstream_key.pem \
#   ./launch-host.sh
set -euo pipefail
HERE=$(cd "$(dirname "$0")" && pwd)
TEMPLATE="$HERE/cloud-init.yaml"

NAME=${NAME:-sparkbox-$(date -u +%m%d%H%M)}
TYPE=${TYPE:-EM-B220E-NVME}
ZONE=${ZONE:-fr-par-1}
OS_ID=${OS_ID:-7d1914e1-f4ab-47fc-bd8c-b3a23143e87a}   # Ubuntu 24.04 LTS
RELEASE=${RELEASE:-latest}
PROXY_DOMAIN=${PROXY_DOMAIN:-hivemind.tools}
BUCKET_BASE=${BUCKET_BASE:-https://sparkbox-artifacts.s3.fr-par.scw.cloud}
# Optional routed IPv6 /64 (e.g. 2001:bc8:702:1c7::/64) → per-sandbox v6.
SUBNET6=${SUBNET6:-}
SUBNET6_FLAG=""
[ -n "$SUBNET6" ] && SUBNET6_FLAG="--subnet6 $SUBNET6"

# Optional operator console password (enables console.<domain>); empty disables.
CONSOLE_PASSWORD=${CONSOLE_PASSWORD:-}
# Optional extra proxy flags, appended last (a repeated --proxy-addr wins). To
# serve HTTPS on :443 with on-demand Let's Encrypt certs (no Cloudflare token):
#   TLS_FLAGS="--proxy-addr :443 --proxy-tls --tls-provider autocert --tls-email you@example.com"
TLS_FLAGS=${TLS_FLAGS:-}

USER_NAME=${USER_NAME:-$(whoami)}
USER_PUBKEY=${USER_PUBKEY:-$HOME/.ssh/id_ed25519.pub}
GATEWAY_HOST_KEY=${GATEWAY_HOST_KEY:?path to the fleet gateway_host_key.pem}
GATEWAY_UPSTREAM_KEY=${GATEWAY_UPSTREAM_KEY:?path to the fleet gateway_upstream_key.pem}

[ -f "$USER_PUBKEY" ] || { echo "no pubkey at $USER_PUBKEY (set USER_PUBKEY)"; exit 1; }
USERS_CONF="$USER_NAME $(cat "$USER_PUBKEY")"

echo "== rendering cloud-init (release=$RELEASE domain=$PROXY_DOMAIN) =="
RENDERED=$(mktemp)
trap 'rm -f "$RENDERED"' EXIT
USERS_CONF="$USERS_CONF" PROXY_DOMAIN="$PROXY_DOMAIN" RELEASE="$RELEASE" \
BUCKET_BASE="$BUCKET_BASE" GHK="$GATEWAY_HOST_KEY" GUK="$GATEWAY_UPSTREAM_KEY" \
SUBNET6_FLAG="$SUBNET6_FLAG" CONSOLE_PASSWORD="$CONSOLE_PASSWORD" TLS_FLAGS="$TLS_FLAGS" \
python3 - "$TEMPLATE" > "$RENDERED" <<'PY'
import base64, os, sys
t = open(sys.argv[1]).read()
b64 = lambda p: base64.b64encode(open(p, 'rb').read()).decode()
for k, v in {
    '@@USERS_CONF@@': os.environ['USERS_CONF'],
    '@@PROXY_DOMAIN@@': os.environ['PROXY_DOMAIN'],
    '@@RELEASE@@': os.environ['RELEASE'],
    '@@BUCKET_BASE@@': os.environ['BUCKET_BASE'],
    '@@SUBNET6_FLAG@@': os.environ['SUBNET6_FLAG'],
    '@@CONSOLE_PASSWORD@@': os.environ['CONSOLE_PASSWORD'],
    '@@TLS_FLAGS@@': os.environ['TLS_FLAGS'],
    '@@GATEWAY_HOST_KEY_B64@@': b64(os.environ['GHK']),
    '@@GATEWAY_UPSTREAM_KEY_B64@@': b64(os.environ['GUK']),
}.items():
    t = t.replace(k, v)
if '@@' in t:
    sys.exit("unfilled placeholder remains in rendered cloud-init")
sys.stdout.write(t)
PY

# SERVER_ID lets you retry the install against an already-created box (e.g. after
# a transient failure) instead of ordering — and paying for — a second server.
if [ -n "${SERVER_ID:-}" ]; then
  SRV="$SERVER_ID"
  echo "== reusing existing server $SRV =="
else
  echo "== creating server $NAME ($TYPE, $ZONE) — billing starts now =="
  SRV=$(scw baremetal server create name="$NAME" type="$TYPE" zone="$ZONE" -o json \
    | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')
  echo "   server id: $SRV"
fi

# Elastic Metal physically provisions ("delivers") the hardware after create;
# an install issued before the box is delivered fails with "Server is not
# delivered". Block until it's ready.
echo "== waiting for hardware delivery =="
scw baremetal server wait "$SRV" zone="$ZONE" >/dev/null

echo "== installing Ubuntu 24.04 + cloud-init (self-provisions on first boot) =="
# scw stores user-data.content as a protobuf *bytes* field: it base64-decodes
# the CLI value, so we pass base64 of the rendered YAML (a raw multi-line string
# is rejected, and @file isn't supported here).
scw baremetal server install "$SRV" zone="$ZONE" os-id="$OS_ID" hostname="$NAME" \
  all-ssh-keys=true \
  user-data.name=cloud-init \
  user-data.content-type=text/cloud-config \
  user-data.content="$(base64 < "$RENDERED" | tr -d '\n')" >/dev/null

echo
echo "== launched =="
echo "   watch:  scw baremetal server wait $SRV zone=$ZONE"
echo "   ip:     scw baremetal server get $SRV zone=$ZONE -o json | python3 -c 'import json,sys;print([i[\"address\"] for i in json.load(sys.stdin)[\"ips\"] if i[\"version\"]==\"IPv4\"][0])'"
echo "   then:   ssh -p 2222 new@<ip>    # cloud-init finishes ~1-2 min after 'ready'"
echo "   teardown: scw baremetal server delete $SRV zone=$ZONE"
