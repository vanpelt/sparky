#!/usr/bin/env bash
# Launch a fresh Scaleway Elastic Metal host that provisions itself into the
# sparkbox fleet via cloud-init — no SSH, no build. Unlike the old flow, fleet
# secrets do NOT ride in user-data: they live in Secret Manager (upload once with
# deploy/upload-fleet-secrets.sh) and the host fetches them at boot. The only
# secret this script injects is a per-host, IP-pinned, expiring Secret Manager
# API key it mints after the box is delivered (deploy/mint-host-identity.sh).
#
# Order matters: the IP pin needs the box's real address, so we create + wait for
# delivery FIRST, then mint the key against that IP, then render + install.
#
# Prereqs: scw CLI configured; a published release (hack/build-artifacts.sh) whose
# rootfs bakes in the gateway upstream pubkey matching the key in Secret Manager;
# the fleet secrets uploaded (deploy/upload-fleet-secrets.sh). A plain
# `./launch-host.sh` then launches a fully-wired host.
set -euo pipefail
HERE=$(cd "$(dirname "$0")" && pwd)
TEMPLATE="$HERE/cloud-init.yaml"

NAME=${NAME:-sparkbox-$(date -u +%m%d%H%M)}
TYPE=${TYPE:-EM-B220E-NVME}
ZONE=${ZONE:-fr-par-1}
REGION=${REGION:-$(scw config get default-region 2>/dev/null || echo fr-par)}
PROJECT_ID=${PROJECT_ID:-$(scw config get default-project-id)}
OS_ID=${OS_ID:-7d1914e1-f4ab-47fc-bd8c-b3a23143e87a}   # Ubuntu 24.04 LTS
RELEASE=${RELEASE:-latest}
PROXY_DOMAIN=${PROXY_DOMAIN:-hivemind.tools}
BUCKET_BASE=${BUCKET_BASE:-https://sparkbox-artifacts.s3.fr-par.scw.cloud}
KEY_TTL_DAYS=${KEY_TTL_DAYS:-30}
# Optional routed IPv6 /64 (e.g. 2001:bc8:702:1c7::/64) → per-sandbox v6.
SUBNET6=${SUBNET6:-}
SUBNET6_FLAG=""
[ -n "$SUBNET6" ] && SUBNET6_FLAG="--subnet6 $SUBNET6"

# Optional live-overcommit flags. Empty by default (count the full memory
# ceiling per VM, the safe baseline). To pack more warm VMs on a box, set the
# working-set floor measured with hack/measure-density.py, e.g.:
#   OVERCOMMIT_FLAGS="--mem-reserve-mb 1024"
# It can also be flipped on a running host by editing /srv/sparkbox/sparkbox.env
# and `systemctl restart sparkbox` — no relaunch needed.
OVERCOMMIT_FLAGS=${OVERCOMMIT_FLAGS:-}

# Optional extra proxy flags, appended last. For HTTPS on :443 with on-demand
# Let's Encrypt (no Cloudflare token):
#   TLS_FLAGS="--proxy-addr :443 --proxy-tls --tls-provider autocert --tls-email you@example.com"
TLS_FLAGS=${TLS_FLAGS:-}

# Optional reserved (flexible) IPs so DNS points at a stable address across host
# rebuilds. FLEXIBLE_FIP_IDS is a comma-separated list of `scw fip ip` IDs to move
# onto the new server; FLEXIBLE_ADDRS is the matching space-separated CIDR host
# addresses the guest pins on its primary NIC.
FLEXIBLE_FIP_IDS=${FLEXIBLE_FIP_IDS:-}
FLEXIBLE_ADDRS=${FLEXIBLE_ADDRS:-}

USER_NAME=${USER_NAME:-$(whoami)}
USER_PUBKEY=${USER_PUBKEY:-$HOME/.ssh/id_ed25519.pub}
[ -f "$USER_PUBKEY" ] || { echo "no pubkey at $USER_PUBKEY (set USER_PUBKEY)"; exit 1; }
USERS_CONF="$USER_NAME $(cat "$USER_PUBKEY")"

# --- create / deliver -------------------------------------------------------
# SERVER_ID lets you retry the install against an already-created box instead of
# ordering — and paying for — a second server.
if [ -n "${SERVER_ID:-}" ]; then
  SRV="$SERVER_ID"
  echo "== reusing existing server $SRV =="
else
  echo "== creating server $NAME ($TYPE, $ZONE) — billing starts now =="
  SRV=$(scw baremetal server create name="$NAME" type="$TYPE" zone="$ZONE" -o json \
    | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')
  echo "   server id: $SRV"
fi

# Elastic Metal physically provisions ("delivers") the hardware after create; an
# install issued before delivery fails with "Server is not delivered". Block.
echo "== waiting for hardware delivery =="
scw baremetal server wait "$SRV" zone="$ZONE" >/dev/null

# Move any reserved flexible IPs onto this server before install so routing is
# live by the time the box boots.
if [ -n "$FLEXIBLE_FIP_IDS" ]; then
  echo "== attaching flexible IPs ($FLEXIBLE_FIP_IDS) to $SRV =="
  fip_args=""; i=0
  IFS=',' read -ra _fips <<< "$FLEXIBLE_FIP_IDS"
  for f in "${_fips[@]}"; do fip_args="$fip_args fips-ids.$i=$f"; i=$((i+1)); done
  # shellcheck disable=SC2086
  scw fip ip attach $fip_args server-id="$SRV" zone="$ZONE" >/dev/null
fi

# --- mint the host's IP-pinned Secret Manager key ---------------------------
# The box's native public IPv4 is its outbound source address (DHCP owns the
# default route even when flexible IPs are attached as extras). Exclude any
# attached flexible address so we pin to the native IP, not the flexible one.
HOST_IP=$(scw baremetal server get "$SRV" zone="$ZONE" -o json \
  | FLEXIBLE_ADDRS="$FLEXIBLE_ADDRS" python3 -c 'import json,sys,os
d=json.load(sys.stdin)
flex=set(a.split("/")[0] for a in os.environ.get("FLEXIBLE_ADDRS","").split())
v4=[i["address"] for i in d["ips"] if i["version"]=="IPv4" and i["address"] not in flex]
print(v4[0] if v4 else "")')
[ -n "$HOST_IP" ] || { echo "could not determine native public IPv4"; exit 1; }
echo "== host native public IPv4: $HOST_IP ${SUBNET6:+(+ v6 $SUBNET6)} =="

eval "$(NAME="$NAME" HOST_IP="$HOST_IP" REGION="$REGION" PROJECT_ID="$PROJECT_ID" \
        KEY_TTL_DAYS="$KEY_TTL_DAYS" SUBNET6="$SUBNET6" "$HERE/mint-host-identity.sh")"
# eval brought APP_ID / POLICY_ID / ACCESS_KEY / SECRET_KEY into scope.
[ -n "${SECRET_KEY:-}" ] || { echo "failed to mint host identity"; exit 1; }

# --- render cloud-init ------------------------------------------------------
echo "== rendering cloud-init (release=$RELEASE domain=$PROXY_DOMAIN) =="
RENDERED=$(mktemp)
trap 'rm -f "$RENDERED"' EXIT
USERS_CONF="$USERS_CONF" PROXY_DOMAIN="$PROXY_DOMAIN" RELEASE="$RELEASE" \
BUCKET_BASE="$BUCKET_BASE" \
SCW_SECRET_KEY="$SECRET_KEY" SCW_PROJECT_ID="$PROJECT_ID" SCW_REGION="$REGION" \
REFRESH_TOOLS="$HERE/refresh-agent-tools.sh" \
GUEST_IDENTITY="$HERE/install-guest-identity.sh" \
NET_SETUP="$HERE/sparkbox-net.sh" \
SUBNET6_FLAG="$SUBNET6_FLAG" TLS_FLAGS="$TLS_FLAGS" OVERCOMMIT_FLAGS="$OVERCOMMIT_FLAGS" \
FLEXIBLE_ADDRS="$FLEXIBLE_ADDRS" \
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
    '@@TLS_FLAGS@@': os.environ['TLS_FLAGS'],
    '@@OVERCOMMIT_FLAGS@@': os.environ['OVERCOMMIT_FLAGS'],
    '@@FLEXIBLE_ADDRS@@': os.environ['FLEXIBLE_ADDRS'],
    '@@SCW_SECRET_KEY@@': os.environ['SCW_SECRET_KEY'],
    '@@SCW_PROJECT_ID@@': os.environ['SCW_PROJECT_ID'],
    '@@SCW_REGION@@': os.environ['SCW_REGION'],
    '@@REFRESH_TOOLS_B64@@': b64(os.environ['REFRESH_TOOLS']),
    '@@GUEST_IDENTITY_B64@@': b64(os.environ['GUEST_IDENTITY']),
    '@@NET_SETUP_B64@@': b64(os.environ['NET_SETUP']),
}.items():
    t = t.replace(k, v)
if '@@' in t:
    sys.exit("unfilled placeholder remains in rendered cloud-init")
sys.stdout.write(t)
PY

# --- install ----------------------------------------------------------------
echo "== installing Ubuntu 24.04 + cloud-init (self-provisions on first boot) =="
# scw stores user-data.content as a protobuf bytes field (base64-decoded from the
# CLI value), so we pass base64 of the rendered YAML.
scw baremetal server install "$SRV" zone="$ZONE" os-id="$OS_ID" hostname="$NAME" \
  all-ssh-keys=true \
  user-data.name=cloud-init \
  user-data.content-type=text/cloud-config \
  user-data.content="$(base64 < "$RENDERED" | tr -d '\n')" >/dev/null

echo
echo "== launched =="
echo "   host identity: app $APP_ID (key expires in ${KEY_TTL_DAYS}d, pinned to $HOST_IP)"
echo "   watch:  scw baremetal server wait $SRV zone=$ZONE"
echo "   ip:     $HOST_IP"
echo "   then:   ssh new@$HOST_IP           # port 22; cloud-init finishes ~1-2 min after 'ready'"
echo "   admin:  ssh -p 2222 ubuntu@$HOST_IP  # host admin sshd (gateway owns :22)"
echo "   secrets:ssh -p 2222 ubuntu@$HOST_IP journalctl -u sparkbox-secrets  # boot-time fetch log"
echo "   teardown: scw baremetal server delete $SRV zone=$ZONE; scw iam application delete $APP_ID"
