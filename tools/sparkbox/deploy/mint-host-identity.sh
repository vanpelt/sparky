#!/usr/bin/env bash
# Mint a deliberately-crippled Scaleway identity for one sparkbox host: an IAM
# application, a policy that grants ONLY SecretManagerSecretAccess and ONLY from
# this host's IP, and an expiring API key. The key is the one secret cloud-init
# still carries — this makes it worth as close to nothing as possible off-host.
#
# The IP pin is the point: a key exfiltrated off the box is inert anywhere else,
# because request.ip won't match. So HOST_IP must be the box's OUTBOUND source
# address (its native public IPv4 — DHCP owns the default route even when
# flexible IPs are attached as extra addresses).
#
# Prints machine-readable APP_ID/POLICY_ID/ACCESS_KEY/SECRET_KEY lines to stdout
# (the secret key is shown once, by Scaleway, ever); human logs go to stderr.
set -euo pipefail

NAME=${NAME:?set NAME to the host name}
HOST_IP=${HOST_IP:?set HOST_IP to the host outbound public IPv4}
REGION=${REGION:-$(scw config get default-region 2>/dev/null || echo fr-par)}
PROJECT_ID=${PROJECT_ID:-$(scw config get default-project-id)}
KEY_TTL_DAYS=${KEY_TTL_DAYS:-30}
# Scope the policy to the whole /sparkbox/ tree so per-host secret paths added
# later (e.g. /sparkbox/host-<id>/) stay covered without touching the policy.
FLEET_PREFIX=${FLEET_PREFIX:-/sparkbox/}

log() { echo "$@" >&2; }

log "== minting host identity for $NAME (pinned to $HOST_IP) =="

APP_ID=$(scw iam application create name="sparkbox-host-$NAME" \
  description="sparkbox host $NAME — Secret Manager access, IP-pinned" -o json \
  | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')
log "   application: $APP_ID"

# request.ip pins the source address — a key stolen off the box is inert
# anywhere else. We do NOT scope by secret path here: despite Scaleway's docs
# (resource.name.startsWith("/folder/")), for AccessSecretVersionByPath
# `resource.name` is the BARE secret name, not the "/sparkbox/fleet/..." path —
# verified live, a path prefix denies every read. The isolation boundary that
# actually works for Secret Manager is the Project (this rule is project-scoped);
# for per-box isolation, give a box its own Project. IP pin + read-only
# (SecretManagerSecretAccess) + expiry are the load-bearing controls.
COND="request.ip == '$HOST_IP'"
POLICY_ID=$(scw iam policy create name="sparkbox-host-$NAME" application-id="$APP_ID" \
  rules.0.project-ids.0="$PROJECT_ID" \
  rules.0.permission-set-names.0=SecretManagerSecretAccess \
  rules.0.condition="$COND" -o json \
  | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')
log "   policy:      $POLICY_ID (SecretManagerSecretAccess, project-scoped, ip==$HOST_IP)"

EXPIRES=$(python3 -c "import datetime; print((datetime.datetime.now(datetime.timezone.utc)+datetime.timedelta(days=$KEY_TTL_DAYS)).strftime('%Y-%m-%dT%H:%M:%SZ'))")
KEY_JSON=$(scw iam api-key create application-id="$APP_ID" expires-at="$EXPIRES" \
  default-project-id="$PROJECT_ID" description="sparkbox host $NAME" -o json)
ACCESS_KEY=$(echo "$KEY_JSON" | python3 -c 'import json,sys; print(json.load(sys.stdin)["access_key"])')
SECRET_KEY=$(echo "$KEY_JSON" | python3 -c 'import json,sys; print(json.load(sys.stdin)["secret_key"])')
log "   api key:     $ACCESS_KEY (expires $EXPIRES)"

printf 'APP_ID=%s\n' "$APP_ID"
printf 'POLICY_ID=%s\n' "$POLICY_ID"
printf 'ACCESS_KEY=%s\n' "$ACCESS_KEY"
printf 'SECRET_KEY=%s\n' "$SECRET_KEY"
