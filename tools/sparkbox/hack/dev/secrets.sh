#!/usr/bin/env bash
# Persist the dev box's generated identity in 1Password, so a fresh checkout
# does not mint a new one.
#
# WHY THIS EXISTS. `.dev/` is disposable and its keys are not. `gateway.sh`
# mints a local fleet identity the first time it starts, and it mints a NEW one
# in a checkout that has no `.dev/` — a different clone, a fresh worktree, a
# `rm -rf .dev`. Everything downstream has already pinned the old one:
#
#   - the node copied the gateway's host key into its control dir on its first
#     successful link and trusts THAT from then on, so it refuses every link
#     afterwards with a host-key mismatch;
#   - the rootfs template carries the gateway's UPSTREAM public key as the login
#     user's authorized_keys, baked in by prepare-vm-assets, so the gateway can
#     no longer ssh into any sandbox created before the re-mint;
#   - the OIDC signing key derives the KEK for every user secret in the
#     database, so re-minting it silently orphans all of them.
#
# None of that announces itself as a key problem. The node links or does not,
# sandboxes boot and report `running`, and the routes that need a key answer
# "could not reach the sandbox's shell". That failure cost a full day once
# already — see hack/dev/README.md, "the guest booted fine and still will not
# answer".
#
#   push    local keys -> 1Password, generating nothing that does not exist
#   pull    1Password -> .dev/gateway/keys (0600), then re-derive the .pub halves
#   status  what exists where, and whether the two agree
#
# This is a thin wrapper around deploy/sync-fleet-secrets.sh, which already does
# exactly this for a real fleet and has the parts that are easy to get wrong:
# values travel in a template rather than argv, every write is proved by reading
# it back, and a hung `op` is reported once instead of eleven times.
#
# Usage:
#   hack/dev/secrets.sh push|pull|status
#
# Env:
#   SPARKBOX_DEV_OP_VAULT    vault to use (default Sparkbox-Dev)
#   SPARKBOX_DEV_OP_ACCOUNT  1Password account (default: op's default)
#   SPARKBOX_DEV_KEY_DIR     override the key directory
#   SPARKBOX_DEV_STATE_DIR   override the state directory (holds node_ca_cert.pem)
set -euo pipefail

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
module_dir=$(cd "$script_dir/../.." && pwd)

readonly gw_dir="$module_dir/.dev/gateway"
readonly key_dir="${SPARKBOX_DEV_KEY_DIR:-$gw_dir/keys}"
# Overridable alongside the key dir: node_ca_cert.pem lives in the state dir
# while its key lives in the key dir, and the fleet script requires the two to
# be both present or both absent. Moving one without the other reads as a
# half-written CA — which is exactly what a wrong default produces here, since
# the key is always found and the cert never is. So this path is gateway.sh's
# own `state_dir`, spelled the same way (durable/gateway/control, the
# entrypoint's default), not a plausible-looking .dev/gateway/state.
readonly state_dir="${SPARKBOX_DEV_STATE_DIR:-$gw_dir/durable/gateway/control}"
readonly sync="$module_dir/deploy/sync-fleet-secrets.sh"

# A vault of its own, and NOT the fleet's. `push` overwrites whatever is in the
# vault with what is on this laptop, so aiming this at the fleet's vault would
# replace the production gateway host key, upstream key and OIDC signing key
# with a dev box's — the OIDC key being the one with no recovery path, because
# it derives the KEK for every user secret in the fleet's database.
#
# The fleet default is `Sparkbox` (deploy/sync-fleet-secrets.sh), so the two
# names differ by four characters and one of them is reached by forgetting to
# set a variable. Hence the refusal below rather than a comment.
readonly vault="${SPARKBOX_DEV_OP_VAULT:-Sparkbox-Dev}"
readonly fleet_vault=Sparkbox

# The dev box's identity, and only that. Everything omitted here is omitted for
# a reason:
#
#   github-app-key           already escrowed, in a DIFFERENT vault and account
#                            (op://Hivemind-Dev/github-app-key/password, see
#                            hack/dev/gateway.sh). Two sources for one secret,
#                            silently preferring one, is how you end up
#                            debugging an App that is not the App you edited.
#   github-app-client-secret the dev App uses none — minting is outbound-only.
#   github-webhook-secret    no public URL here, so no webhooks.
#   cloudflare-api-token     no public DNS: the dev gateway runs TLS off.
#   console-password         no console auth on a loopback gateway.
readonly exclude="github-app-key github-app-client-secret github-webhook-secret cloudflare-api-token console-password"

die() { echo "secrets.sh: $*" >&2; exit 1; }
note() { printf '    %s\n' "$*"; }

[ -x "$sync" ] || die "missing $sync"

if [ "$vault" = "$fleet_vault" ]; then
  die "refusing to use the fleet vault '$fleet_vault' for dev secrets.
     push would overwrite the fleet's gateway host key, upstream key and OIDC
     signing key with this laptop's. Pick another vault:
       SPARKBOX_DEV_OP_VAULT=Sparkbox-Dev $0 $*
     To sync the real fleet, run deploy/sync-fleet-secrets.sh directly."
fi

# The public halves are derived, never stored: ssh-keygen -y regenerates them
# from the .pem byte for byte, so escrowing them would be a second copy of the
# same fact that can disagree with the first.
#
# They must exist after a pull, though, and nothing else will make them.
# gateway.sh derives them only inside mint_identity, which a complete restored
# identity skips by definition — so a pull without this leaves up.sh's
# seed_trust with no gateway_upstream_key.pub and the node with nothing to
# trust.
derive_pubs() {
  local name
  for name in gateway_host_key gateway_upstream_key; do
    [ -f "$key_dir/$name.pem" ] || continue
    ssh-keygen -y -f "$key_dir/$name.pem" > "$key_dir/$name.pub"
    chmod 0644 "$key_dir/$name.pub"
    note "derived $name.pub"
  done
}

# `env` with an array, not a chain of assignment prefixes: an empty
# ${VAR:+NAME=value} in such a chain expands to nothing, and bash then reads the
# NEXT assignment as the command name ("SECRETS_EXCLUDE=...: command not found").
run_sync() {
  local -a vars=(
    SECRETS_DIR="$key_dir"
    OP_VAULT="$vault"
    SECRETS_EXCLUDE="$exclude"
    NODE_CA_CERT="$state_dir/node_ca_cert.pem"
    FLEET_CLUSTER_ID="${SPARKBOX_DEV_NODE_NAME:-dev-gateway}"
  )
  [ -n "${SPARKBOX_DEV_OP_ACCOUNT:-}" ] &&
    vars+=(OP_ACCOUNT="$SPARKBOX_DEV_OP_ACCOUNT")
  env "${vars[@]}" "$sync" "$1"
}

case "${1:-}" in
  push)
    [ -d "$key_dir" ] ||
      die "no $key_dir — start the gateway once so it mints an identity:
     hack/dev/gateway.sh start"
    run_sync push
    ;;
  pull)
    mkdir -p "$key_dir"; chmod 0700 "$key_dir"
    run_sync pull
    derive_pubs
    note ""
    note "the node pinned the OLD gateway host key on its first link; up.sh"
    note "clears that pin when it disagrees, so re-run: hack/dev/up.sh node"
    ;;
  status)
    run_sync status
    ;;
  *)
    echo "usage: hack/dev/secrets.sh {push|pull|status}"
    echo
    echo "  push    .dev/gateway/keys -> 1Password vault '$vault'"
    echo "  pull    that vault -> .dev/gateway/keys, then derive the .pub halves"
    echo "  status  compare both sides"
    echo
    echo "env: SPARKBOX_DEV_OP_VAULT (=$vault)  SPARKBOX_DEV_OP_ACCOUNT"
    exit 2
    ;;
esac
