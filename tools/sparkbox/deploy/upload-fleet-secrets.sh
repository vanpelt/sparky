#!/usr/bin/env bash
# Upload the fleet's secrets into Scaleway Secret Manager under /sparkbox/fleet,
# so hosts fetch them at boot (`sparkbox fetch-secrets`) instead of carrying them
# in cloud-init user-data. Idempotent: creates each secret once, then adds a new
# version only when the payload has changed.
#
#   gateway-host-key      ssh_key   $SECRETS_DIR/gateway_host_key.pem
#   gateway-upstream-key  ssh_key   $SECRETS_DIR/gateway_upstream_key.pem
#   oidc-signing-key      opaque    $SECRETS_DIR/oidc_signing_key.pem  (generated if absent)
#   node-control-ca-cert  opaque    $SECRETS_DIR/node_ca_cert.pem       (generated with its key)
#   node-control-ca-key   opaque    $SECRETS_DIR/node_ca_key.pem        (generated if absent)
#   gateway-control-key   opaque    $SECRETS_DIR/gateway_control_key.pem (generated if absent)
#   cloudflare-api-token  opaque    $SECRETS_DIR/.env  CLOUDFLARE_API_TOKEN  (optional)
#   console-password      opaque    $CONSOLE_PASSWORD or generated + saved  (optional)
#
# Prereq: scw CLI configured (project + secret key). Run from a trusted machine.
set -euo pipefail
HERE=$(cd "$(dirname "$0")" && pwd)
SECRETS_DIR=${SECRETS_DIR:-$HOME/.sparkbox/secrets}
FLEET_PATH=${FLEET_PATH:-/sparkbox/fleet}
REGION=${REGION:-$(scw config get default-region 2>/dev/null || echo fr-par)}

GATEWAY_HOST_KEY=${GATEWAY_HOST_KEY:-$SECRETS_DIR/gateway_host_key.pem}
GATEWAY_UPSTREAM_KEY=${GATEWAY_UPSTREAM_KEY:-$SECRETS_DIR/gateway_upstream_key.pem}
OIDC_SIGNING_KEY=${OIDC_SIGNING_KEY:-$SECRETS_DIR/oidc_signing_key.pem}
NODE_CA_CERT=${NODE_CA_CERT:-$SECRETS_DIR/node_ca_cert.pem}
NODE_CA_KEY=${NODE_CA_KEY:-$SECRETS_DIR/node_ca_key.pem}
GATEWAY_CONTROL_KEY=${GATEWAY_CONTROL_KEY:-$SECRETS_DIR/gateway_control_key.pem}
FLEET_CLUSTER_ID=${FLEET_CLUSTER_ID:-prod}

for k in "$GATEWAY_HOST_KEY" "$GATEWAY_UPSTREAM_KEY"; do
  [ -f "$k" ] || { echo "missing fleet gateway key: $k"; exit 1; }
done

# The OIDC signing key is fleet state but has no rootfs-baked counterpart, so we
# can mint it here on first run. Keep the file: it must stay stable across hosts,
# or every relying party re-onboards against a new JWKS.
if [ ! -f "$OIDC_SIGNING_KEY" ]; then
  echo "== generating fleet OIDC signing key at $OIDC_SIGNING_KEY =="
  mkdir -p "$(dirname "$OIDC_SIGNING_KEY")"
  ( umask 077; openssl ecparam -name prime256v1 -genkey -noout -out "$OIDC_SIGNING_KEY" )
fi

# The node-control CA is a fleet identity, not disposable host state. Generate
# it once on the trusted upload machine, upload both halves, and keep the local
# copies for disaster recovery. The gateway leaf key is likewise stable while
# its public certificate is short-lived and reissued into durable state.
if [ ! -f "$NODE_CA_KEY" ] || [ ! -f "$NODE_CA_CERT" ]; then
  if [ -f "$NODE_CA_KEY" ] || [ -f "$NODE_CA_CERT" ]; then
    echo "incomplete node-control CA: $NODE_CA_KEY and $NODE_CA_CERT must both exist or both be absent"
    exit 1
  fi
  echo "== generating node-control CA for cluster $FLEET_CLUSTER_ID =="
  mkdir -p "$(dirname "$NODE_CA_KEY")"
  ( umask 077
    openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -out "$NODE_CA_KEY"
    openssl req -x509 -new -sha256 -days 3650 \
      -key "$NODE_CA_KEY" -out "$NODE_CA_CERT" \
      -subj "/CN=Sparkbox $FLEET_CLUSTER_ID internal CA" \
      -addext "basicConstraints=critical,CA:TRUE,pathlen:0" \
      -addext "keyUsage=critical,digitalSignature,keyCertSign,cRLSign"
  )
fi
if [ ! -f "$GATEWAY_CONTROL_KEY" ]; then
  echo "== generating gateway control key at $GATEWAY_CONTROL_KEY =="
  mkdir -p "$(dirname "$GATEWAY_CONTROL_KEY")"
  ( umask 077
    openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 \
      -out "$GATEWAY_CONTROL_KEY"
  )
fi

# Cloudflare token (optional): from env or $SECRETS_DIR/.env.
if [ -z "${CLOUDFLARE_API_TOKEN:-}" ] && [ -f "$SECRETS_DIR/.env" ]; then
  CLOUDFLARE_API_TOKEN=$(sed -n 's/^CLOUDFLARE_API_TOKEN=//p' "$SECRETS_DIR/.env")
fi
CLOUDFLARE_API_TOKEN=${CLOUDFLARE_API_TOKEN:-}

# Console password (optional): from env, else the saved value in .env, else
# generate one and save it — so the operator can log into console.<domain> and
# re-running this script doesn't rotate the password (or burn a version) each time.
if [ -z "${CONSOLE_PASSWORD:-}" ] && [ -f "$SECRETS_DIR/.env" ]; then
  CONSOLE_PASSWORD=$(sed -n 's/^SPARKBOX_CONSOLE_PASSWORD=//p' "$SECRETS_DIR/.env")
fi
if [ -z "${CONSOLE_PASSWORD:-}" ]; then
  CONSOLE_PASSWORD=$(openssl rand -base64 18)
  echo "== generated console password (saved to $SECRETS_DIR/.env as SPARKBOX_CONSOLE_PASSWORD) =="
  echo "   $CONSOLE_PASSWORD"
  touch "$SECRETS_DIR/.env"; chmod 600 "$SECRETS_DIR/.env"
  echo "SPARKBOX_CONSOLE_PASSWORD=$CONSOLE_PASSWORD" >> "$SECRETS_DIR/.env"
fi

TMP=$(mktemp -d); trap 'rm -rf "$TMP"' EXIT

# ensure_secret <name> <type> -> prints the secret id, creating it if absent.
ensure_secret() {
  local name=$1 type=$2 id
  id=$(scw secret secret list name="$name" -o json 2>/dev/null \
    | python3 -c "import json,sys; name,path=sys.argv[1],sys.argv[2]; print(next((s['id'] for s in json.load(sys.stdin) if s['name']==name and s['path']==path), ''))" "$name" "$FLEET_PATH" 2>/dev/null || true)
  if [ -z "$id" ]; then
    id=$(scw secret secret create name="$name" path="$FLEET_PATH" type="$type" \
           description="sparkbox fleet secret" region="$REGION" -o json \
         | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')
    echo "   created $FLEET_PATH/$name ($type) -> $id" >&2
  else
    echo "   exists  $FLEET_PATH/$name -> $id" >&2
  fi
  printf '%s' "$id"
}

# put_version <id> <datafile>: add a new version only if the payload changed
# from the latest, so re-running the script doesn't churn versions (cap is 20).
put_version() {
  local id=$1 file=$2 current
  current=$(scw secret version access "$id" revision=latest_enabled region="$REGION" -o json 2>/dev/null \
    | python3 -c "import json,sys,base64; d=json.load(sys.stdin); sys.stdout.buffer.write(base64.b64decode(d['data']))" 2>/dev/null || true)
  if [ "$current" = "$(cat "$file")" ]; then
    echo "   unchanged, no new version" >&2
    return
  fi
  scw secret version create "$id" data=@"$file" region="$REGION" -o json >/dev/null
  echo "   uploaded new version" >&2
}

wrap_ssh() { python3 -c 'import json,sys; print(json.dumps({"ssh_private_key": open(sys.argv[1]).read()}))' "$1"; }

echo "== uploading fleet secrets to $FLEET_PATH ($REGION) =="

id=$(ensure_secret gateway-host-key ssh_key)
wrap_ssh "$GATEWAY_HOST_KEY" > "$TMP/host.json";   put_version "$id" "$TMP/host.json"

id=$(ensure_secret gateway-upstream-key ssh_key)
wrap_ssh "$GATEWAY_UPSTREAM_KEY" > "$TMP/up.json"; put_version "$id" "$TMP/up.json"

id=$(ensure_secret oidc-signing-key opaque)
put_version "$id" "$OIDC_SIGNING_KEY"

id=$(ensure_secret node-control-ca-cert opaque)
put_version "$id" "$NODE_CA_CERT"

id=$(ensure_secret node-control-ca-key opaque)
put_version "$id" "$NODE_CA_KEY"

id=$(ensure_secret gateway-control-key opaque)
put_version "$id" "$GATEWAY_CONTROL_KEY"

if [ -n "$CLOUDFLARE_API_TOKEN" ]; then
  id=$(ensure_secret cloudflare-api-token opaque)
  printf '%s' "$CLOUDFLARE_API_TOKEN" > "$TMP/cf"; put_version "$id" "$TMP/cf"
fi

id=$(ensure_secret console-password opaque)
printf '%s' "$CONSOLE_PASSWORD" > "$TMP/cp"; put_version "$id" "$TMP/cp"

echo "== done =="
