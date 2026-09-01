#!/usr/bin/env bash
# Sync the fleet's secrets between this machine and a 1Password vault, one item
# per secret, so the fleet's identity survives any single box and a new host or
# node can be hydrated without hand-carrying PEMs.
#
#   push    local files -> 1Password (generating anything that doesn't exist yet)
#   pull    1Password -> local files ($SECRETS_DIR, 0600)
#   status  what exists where, and whether the two agree
#
# Each secret is a Password item whose `password` field holds the whole payload,
# read back as op://$OP_VAULT/<name>/password. That one shape covers PEMs and
# tokens alike, and it is what `sparkbox fetch-secrets --provider op` expects.
#
#   gateway-host-key      $SECRETS_DIR/gateway_host_key.pem      (required)
#   gateway-upstream-key  $SECRETS_DIR/gateway_upstream_key.pem  (required)
#   oidc-signing-key      $SECRETS_DIR/oidc_signing_key.pem      (generated if absent)
#   node-control-ca-cert  $SECRETS_DIR/node_ca_cert.pem          (generated with its key)
#   node-control-ca-key   $SECRETS_DIR/node_ca_key.pem           (generated if absent)
#   gateway-control-key   $SECRETS_DIR/gateway_control_key.pem   (generated if absent)
#   github-app-key        $SECRETS_DIR/github_app_key.pem        (NEVER generated — GitHub issues it)
#   github-app-client-secret $SECRETS_DIR/github_app_client_secret (NEVER generated — GitHub issues it)
#   github-webhook-secret $SECRETS_DIR/github_webhook_secret     (generated if absent — we pick it)
#   cloudflare-api-token  $SECRETS_DIR/.env CLOUDFLARE_API_TOKEN (optional)
#   console-password      $CONSOLE_PASSWORD or generated         (optional)
#
# Unlike Scaleway's ssh_key type, 1Password has no notion of secret types: an
# SSH key is stored as a bare PEM, not wrapped in JSON. bootsecrets handles both.
#
# Prereq: `op` signed in (desktop app, or OP_SERVICE_ACCOUNT_TOKEN). Run from a
# trusted machine — this is the one place the whole fleet identity is in plaintext.
set -euo pipefail

SECRETS_DIR=${SECRETS_DIR:-$HOME/.sparkbox/secrets}
OP_VAULT=${OP_VAULT:-Sparkbox}
OP_FIELD=${OP_FIELD:-password}
FLEET_CLUSTER_ID=${FLEET_CLUSTER_ID:-prod}

GATEWAY_HOST_KEY=${GATEWAY_HOST_KEY:-$SECRETS_DIR/gateway_host_key.pem}
GATEWAY_UPSTREAM_KEY=${GATEWAY_UPSTREAM_KEY:-$SECRETS_DIR/gateway_upstream_key.pem}
OIDC_SIGNING_KEY=${OIDC_SIGNING_KEY:-$SECRETS_DIR/oidc_signing_key.pem}
NODE_CA_CERT=${NODE_CA_CERT:-$SECRETS_DIR/node_ca_cert.pem}
NODE_CA_KEY=${NODE_CA_KEY:-$SECRETS_DIR/node_ca_key.pem}
GATEWAY_CONTROL_KEY=${GATEWAY_CONTROL_KEY:-$SECRETS_DIR/gateway_control_key.pem}
GITHUB_APP_KEY=${GITHUB_APP_KEY:-$SECRETS_DIR/github_app_key.pem}
GITHUB_APP_CLIENT_SECRET_FILE=${GITHUB_APP_CLIENT_SECRET_FILE:-$SECRETS_DIR/github_app_client_secret}
GITHUB_WEBHOOK_SECRET_FILE=${GITHUB_WEBHOOK_SECRET_FILE:-$SECRETS_DIR/github_webhook_secret}

# name:file:required — the manifest, mirroring internal/bootsecrets.
SECRETS=(
  "gateway-host-key:$GATEWAY_HOST_KEY:1"
  "gateway-upstream-key:$GATEWAY_UPSTREAM_KEY:1"
  "oidc-signing-key:$OIDC_SIGNING_KEY:1"
  "node-control-ca-cert:$NODE_CA_CERT:0"
  "node-control-ca-key:$NODE_CA_KEY:0"
  "gateway-control-key:$GATEWAY_CONTROL_KEY:0"
  "github-app-key:$GITHUB_APP_KEY:0"
  "github-app-client-secret:$GITHUB_APP_CLIENT_SECRET_FILE:0"
  "github-webhook-secret:$GITHUB_WEBHOOK_SECRET_FILE:0"
  "cloudflare-api-token:$SECRETS_DIR/cloudflare_api_token:0"
  "console-password:$SECRETS_DIR/console_password:0"
)

TMP=$(mktemp -d); trap 'rm -rf "$TMP"' EXIT
chmod 700 "$TMP"

op_args=(--vault "$OP_VAULT")
[ -n "${OP_ACCOUNT:-}" ] && op_args+=(--account "$OP_ACCOUNT")
read_args=()
[ -n "${OP_ACCOUNT:-}" ] && read_args+=(--account "$OP_ACCOUNT")

# run_bounded runs a command under a time limit where one is available. macOS
# has no `timeout` unless coreutils is installed, and this script's whole point
# is to run on the operator's laptop, so its absence must not be fatal.
run_bounded() {
  local secs=$1; shift
  if command -v timeout >/dev/null 2>&1; then
    timeout "$secs" "$@"
  else
    "$@"
  fi
}

need_op() {
  command -v op >/dev/null || { echo "the 1Password CLI (op) is not installed: https://1password.com/downloads/command-line/"; exit 1; }
  # Fail here, with a useful message, rather than once per secret. A hung `op`
  # means the desktop app is waiting on approval it will never get.
  if ! run_bounded 60 op vault get "$OP_VAULT" "${read_args[@]}" >/dev/null 2>"$TMP/oerr"; then
    echo "cannot reach vault '$OP_VAULT': $(head -1 "$TMP/oerr" 2>/dev/null)"
    echo "  sign in first (op signin), or create the vault: op vault create $OP_VAULT"
    exit 1
  fi
}

# op_read <name> -> payload on stdout; returns 1 if the item is absent.
op_read() {
  op read --no-newline "op://$OP_VAULT/$1/$OP_FIELD" "${read_args[@]}" 2>/dev/null
}

# json_item <title> <file> -> an item template on stdout. Values travel in a
# template, never in argv, where any process on the machine could read them.
json_item() {
  python3 -c '
import json, sys
title, path = sys.argv[1], sys.argv[2]
print(json.dumps({
    "title": title,
    "category": "PASSWORD",
    "fields": [{"id": "password", "type": "CONCEALED", "purpose": "PASSWORD",
                "label": "password", "value": open(path).read()}],
}))' "$1" "$2"
}

# put_secret <name> <file>: create or update, then PROVE it by reading back.
#
# The read-back is not paranoia. `op item create --category ... --template ...`
# silently stores an item with every field empty, and there is no reason to
# assume that is the only such case — a secret you believe is backed up but
# isn't is the worst possible failure here, because you find out when the box
# is already gone. Never report success without verifying.
put_secret() {
  local name=$1 file=$2 current
  if current=$(op_read "$name"); then
    if [ "$current" = "$(cat "$file")" ]; then
      echo "   unchanged  $name"
      return
    fi
    json_item "$name" "$file" > "$TMP/item.json"
    # A full template would drop any other field's value; --template on an edit
    # only touches what it names.
    op item edit "$name" "${op_args[@]}" --template "$TMP/item.json" >/dev/null
    echo "   updated    $name"
  else
    json_item "$name" "$file" > "$TMP/item.json"
    op item create --title "$name" "${op_args[@]}" --template "$TMP/item.json" >/dev/null
    echo "   created    $name"
  fi
  rm -f "$TMP/item.json"

  if ! current=$(op_read "$name"); then
    echo "   FAILED     $name: wrote it, but it cannot be read back"; exit 1
  fi
  if [ "$current" != "$(cat "$file")" ]; then
    echo "   FAILED     $name: stored value does not match $file"
    echo "              (op accepted the write and discarded the value — do not trust this as a backup)"
    exit 1
  fi
}

# --- generation: state that has no rootfs-baked counterpart ------------------

generate_missing() {
  for k in "$GATEWAY_HOST_KEY" "$GATEWAY_UPSTREAM_KEY"; do
    [ -f "$k" ] || { echo "missing fleet gateway key: $k"; exit 1; }
  done

  # The OIDC signing key must stay stable across hosts: it signs workload
  # identity tokens AND derives the KEK for every user secret in the database.
  # Mint it once, here, and never let a host generate its own.
  if [ ! -f "$OIDC_SIGNING_KEY" ]; then
    echo "== generating fleet OIDC signing key at $OIDC_SIGNING_KEY =="
    mkdir -p "$(dirname "$OIDC_SIGNING_KEY")"
    ( umask 077; openssl ecparam -name prime256v1 -genkey -noout -out "$OIDC_SIGNING_KEY" )
  fi

  # The node-control CA is a fleet identity, not disposable host state: it is
  # what lets the mTLS control plane trust a node at all.
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
      openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -out "$GATEWAY_CONTROL_KEY" )
  fi

  # github-app-key is DELIBERATELY EXEMPT from generation, and it is the only
  # PEM in the manifest that is. Every other one is minted here, so absence
  # means "not made yet" and minting it is the right repair. This one is issued
  # by GitHub and displayed exactly once, when you click "Generate a private
  # key" — nothing on this side can recreate it. A locally generated RSA key
  # would be indistinguishable to this script: it would push cleanly, verify on
  # read-back, deliver to the host, and then fail every installation-token mint
  # with a bare 401 that names none of this. So the key is pushed only if the
  # operator dropped the downloaded .pem at $GITHUB_APP_KEY; otherwise the
  # manifest loop skips it like any other absent optional secret, which is
  # exactly right for a fleet with no App. See docs/github-app-setup.md.

  # github-webhook-secret IS generated here, one line below the key that is
  # deliberately not, and the difference between them is who has to know the
  # value. GitHub issues the private key and shows it exactly once, so nothing
  # on this side can recreate it. The webhook secret is the opposite: GitHub
  # never generates it and never learns it from us — it is a random string both
  # ends must agree on, which an operator pastes into the App's Webhook
  # settings. Minting it here is therefore the whole workflow rather than a
  # trap, and it is echoed for the same reason the console password is: a
  # secret nobody can read is a secret nobody can paste. Rotating it is one
  # edit on each side, with no re-consent and no flag day.
  local ws=${GITHUB_WEBHOOK_SECRET:-}
  if [ -z "$ws" ] && [ -f "$SECRETS_DIR/.env" ]; then
    ws=$(sed -n 's/^SPARKBOX_GITHUB_WEBHOOK_SECRET=//p' "$SECRETS_DIR/.env")
  fi
  if [ -z "$ws" ] && [ -f "$GITHUB_WEBHOOK_SECRET_FILE" ]; then
    ws=$(cat "$GITHUB_WEBHOOK_SECRET_FILE")
  fi
  if [ -z "$ws" ]; then
    # Hex, not base64: this value is typed or pasted into a web form and then
    # compared byte for byte, and hex has no characters a form, a shell or a
    # copy-paste can mangle.
    ws=$(openssl rand -hex 32)
    echo "== generated github webhook secret (stored as github-webhook-secret) =="
    echo "   $ws"
    echo "   paste it into the App's Settings -> Webhook -> Secret"
  fi
  ( umask 077; printf '%s' "$ws" > "$GITHUB_WEBHOOK_SECRET_FILE" )

  # Cloudflare token and console password are values, not files: stage them as
  # files so the manifest loop stays uniform.
  local cf=${CLOUDFLARE_API_TOKEN:-}
  if [ -z "$cf" ] && [ -f "$SECRETS_DIR/.env" ]; then
    cf=$(sed -n 's/^CLOUDFLARE_API_TOKEN=//p' "$SECRETS_DIR/.env")
  fi
  if [ -n "$cf" ]; then
    ( umask 077; printf '%s' "$cf" > "$SECRETS_DIR/cloudflare_api_token" )
  fi

  local cp=${CONSOLE_PASSWORD:-}
  if [ -z "$cp" ] && [ -f "$SECRETS_DIR/.env" ]; then
    cp=$(sed -n 's/^SPARKBOX_CONSOLE_PASSWORD=//p' "$SECRETS_DIR/.env")
  fi
  if [ -z "$cp" ] && [ -f "$SECRETS_DIR/console_password" ]; then
    cp=$(cat "$SECRETS_DIR/console_password")
  fi
  if [ -z "$cp" ]; then
    cp=$(openssl rand -base64 18)
    echo "== generated console password (stored as console-password) =="
    echo "   $cp"
  fi
  ( umask 077; printf '%s' "$cp" > "$SECRETS_DIR/console_password" )
}

# --- commands ---------------------------------------------------------------

cmd_push() {
  mkdir -p "$SECRETS_DIR"; chmod 700 "$SECRETS_DIR"
  generate_missing
  need_op
  echo "== pushing fleet secrets to 1Password vault '$OP_VAULT' =="
  local entry name file required
  for entry in "${SECRETS[@]}"; do
    IFS=: read -r name file required <<<"$entry"
    if [ ! -f "$file" ]; then
      [ "$required" = "1" ] && { echo "   MISSING    $name ($file)"; exit 1; }
      echo "   skipped    $name (no $file)"
      continue
    fi
    put_secret "$name" "$file"
  done
  echo "== done =="
}

cmd_pull() {
  need_op
  mkdir -p "$SECRETS_DIR"; chmod 700 "$SECRETS_DIR"
  echo "== pulling fleet secrets from 1Password vault '$OP_VAULT' =="
  local entry name file required
  for entry in "${SECRETS[@]}"; do
    IFS=: read -r name file required <<<"$entry"
    if ! op_read "$name" > "$TMP/payload" 2>/dev/null; then
      [ "$required" = "1" ] && { echo "   MISSING    $name (required)"; exit 1; }
      echo "   absent     $name"
      continue
    fi
    mkdir -p "$(dirname "$file")"
    ( umask 077; cp "$TMP/payload" "$file" )
    chmod 600 "$file"
    echo "   wrote      $file"
  done
  rm -f "$TMP/payload"
  echo "== done =="
}

cmd_status() {
  need_op
  printf '%-24s %-10s %-10s %s\n' SECRET LOCAL 1PASSWORD MATCH
  local entry name file required local_state remote_state match
  for entry in "${SECRETS[@]}"; do
    IFS=: read -r name file required <<<"$entry"
    local_state=absent; [ -f "$file" ] && local_state=present
    if op_read "$name" > "$TMP/payload" 2>/dev/null; then
      remote_state=present
    else
      remote_state=absent
    fi
    match=-
    if [ "$local_state" = present ] && [ "$remote_state" = present ]; then
      if cmp -s "$file" "$TMP/payload"; then match=yes; else match=NO; fi
    fi
    printf '%-24s %-10s %-10s %s\n' "$name" "$local_state" "$remote_state" "$match"
  done
  rm -f "$TMP/payload"
}

case "${1:-}" in
  push)   cmd_push ;;
  pull)   cmd_pull ;;
  status) cmd_status ;;
  *)
    echo "usage: $(basename "$0") {push|pull|status}"
    echo
    echo "  push    local files -> 1Password (generates missing fleet keys first)"
    echo "  pull    1Password -> \$SECRETS_DIR ($SECRETS_DIR)"
    echo "  status  compare both sides"
    echo
    echo "env: OP_VAULT (=$OP_VAULT)  OP_ACCOUNT  SECRETS_DIR (=$SECRETS_DIR)"
    exit 2
    ;;
esac
