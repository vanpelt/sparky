#!/usr/bin/env bash
# test-secrets.sh — exercise hack/dev/secrets.sh without a 1Password account.
#
# The thing being tested is a backup: the failure it exists to prevent is a key
# you believe is escrowed and is not, discovered when the identity is already
# gone. That is not something to find out on a real vault, and a real vault
# cannot run in CI anyway — `op` needs an authorized desktop app or a service
# account token. So the CLI is replaced with a stub on PATH that stores items in
# a directory, and the round trip is checked byte for byte.
#
# What this does NOT prove: that a real `op` accepts these exact arguments. The
# invocations under test are the ones deploy/sync-fleet-secrets.sh already runs
# against the live CLI (op 2.33.0, verified 2026-07-27, including the
# `op item edit --template` update path). This harness pins the wiring around
# them — which vault, which files, what is excluded, what happens on the paths
# that must refuse.
#
# usage: ./hack/dev/test-secrets.sh
set -uo pipefail

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
module_dir=$(cd "$script_dir/../.." && pwd)
secrets_sh="$script_dir/secrets.sh"

WORK=$(mktemp -d "${TMPDIR:-/tmp}/sparkbox-secrets-test.XXXXXX")
trap 'rm -rf -- "$WORK"' EXIT

CASES=0
FAILURES=0
ok()  { CASES=$((CASES + 1)); printf '  [PASS] %s\n' "$1"; }
bad() { CASES=$((CASES + 1)); FAILURES=$((FAILURES + 1)); printf '  [FAIL] %s\n' "$1"; }

# --- the stub -----------------------------------------------------------------
# Stores one file per item under $OP_STORE/<vault>/<name>, which is the whole
# model `op` presents to this script: an item is a name in a vault holding one
# concealed field. Failure modes it can be told to produce are the ones that
# actually happen — a vault that cannot be reached, and a write that is accepted
# and silently discarded (see put_secret's read-back).
mkdir -p "$WORK/bin"
cat > "$WORK/bin/op" <<'STUB'
#!/usr/bin/env bash
set -uo pipefail
store="${OP_STORE:?}"

# Pull the value out of --vault/--account/--template wherever they land.
vault=""; template=""; title=""
args=()
while [ $# -gt 0 ]; do
  case "$1" in
    --vault)    vault=$2; shift 2 ;;
    --account)  shift 2 ;;
    --template) template=$2; shift 2 ;;
    --title)    title=$2; shift 2 ;;
    --no-newline) shift ;;
    *) args+=("$1"); shift ;;
  esac
done
set -- "${args[@]:-}"

[ "${OP_FAIL_VAULT:-}" = 1 ] && { echo "cannot reach vault" >&2; exit 1; }

case "${1:-}" in
  vault)
    # `op vault get <name>` — the reachability probe.
    [ -d "$store/${vault:-$3}" ] || [ "${OP_VAULT_EXISTS:-1}" = 1 ] || exit 1
    exit 0
    ;;
  read)
    # op read op://<vault>/<item>/<field>
    ref=${2#op://}
    v=${ref%%/*}; rest=${ref#*/}; item=${rest%%/*}
    [ -f "$store/$v/$item" ] || { echo "item not found" >&2; exit 1; }
    cat "$store/$v/$item"
    exit 0
    ;;
  item)
    case "${2:-}" in
      create|edit)
        name=$title
        [ -n "$name" ] || name=$3
        mkdir -p "$store/$vault"
        # OP_DISCARD_WRITES reproduces the documented failure where op accepts
        # the write and stores an empty field.
        if [ "${OP_DISCARD_WRITES:-}" = 1 ]; then
          : > "$store/$vault/$name"
        else
          python3 -c '
import json,sys
d=json.load(open(sys.argv[1]))
sys.stdout.write(d["fields"][0]["value"])' "$template" > "$store/$vault/$name"
        fi
        exit 0
        ;;
    esac
    ;;
esac
echo "stub op: unhandled: $*" >&2
exit 64
STUB
chmod +x "$WORK/bin/op"

export PATH="$WORK/bin:$PATH"
export OP_STORE="$WORK/store"
mkdir -p "$OP_STORE/Sparkbox-Dev"

# --- a dev key directory ------------------------------------------------------
# Real ed25519 keys, because derive_pubs runs ssh-keygen -y over them and a
# placeholder would not survive it.
keys="$WORK/keys"
mkdir -p "$keys" "$WORK/state"
for name in gateway_host_key gateway_upstream_key; do
  ssh-keygen -q -t ed25519 -N '' -C "sparkbox $name" -f "$keys/$name.pem" 2>/dev/null
  rm -f "$keys/$name.pem.pub"
done
openssl ecparam -name prime256v1 -genkey -noout -out "$keys/oidc_signing_key.pem" 2>/dev/null
openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -out "$keys/node_ca_key.pem" 2>/dev/null
openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -out "$keys/gateway_control_key.pem" 2>/dev/null
# The CA cert lives in the state dir, not the key dir, and the fleet script
# refuses a half-written CA — both or neither. A real dev box has both.
openssl req -x509 -new -key "$keys/node_ca_key.pem" -days 1 -subj "/CN=Sparkbox dev test CA" \
  -out "$WORK/state/node_ca_cert.pem" 2>/dev/null
# The one that must NOT travel: already escrowed in another vault entirely.
echo "FAKE GITHUB APP KEY" > "$keys/github_app_key.pem"

dev() {
  SPARKBOX_DEV_KEY_DIR="$keys" \
  SPARKBOX_DEV_STATE_DIR="$WORK/state" \
  SPARKBOX_DEV_OP_VAULT="${VAULT:-Sparkbox-Dev}" \
    "$secrets_sh" "$@" 2>&1
}

echo "== hack/dev/secrets.sh =="

# --- push ---------------------------------------------------------------------
out=$(dev push)
if [ $? -eq 0 ] && grep -q 'created    gateway-host-key' <<<"$out"; then
  ok "push creates the identity items"
else
  bad "push failed: $out"
fi

for item in gateway-host-key gateway-upstream-key oidc-signing-key node-control-ca-key gateway-control-key; do
  if [ -s "$OP_STORE/Sparkbox-Dev/$item" ]; then
    ok "push escrowed $item"
  else
    bad "push did not escrow $item"
  fi
done

# The whole point of the exclude list.
if [ -e "$OP_STORE/Sparkbox-Dev/github-app-key" ]; then
  bad "push escrowed github-app-key, which lives in a different vault"
else
  ok "push left github-app-key alone (owned by another vault)"
fi
for item in console-password github-webhook-secret cloudflare-api-token; do
  if [ -e "$OP_STORE/Sparkbox-Dev/$item" ]; then
    bad "push escrowed $item, which the dev box does not use"
  else
    ok "push skipped $item"
  fi
done
# Excluded secrets must not be MINTED either — an unused generated value in the
# key dir is indistinguishable from one that matters.
if [ -e "$keys/console_password" ] || [ -e "$keys/github_webhook_secret" ]; then
  bad "push generated an excluded secret into the key dir"
else
  ok "push generated no excluded secrets locally"
fi

# --- idempotence --------------------------------------------------------------
out=$(dev push)
if grep -q 'unchanged  gateway-host-key' <<<"$out"; then
  ok "a second push is a no-op"
else
  bad "second push was not a no-op: $out"
fi

# --- status -------------------------------------------------------------------
out=$(dev status)
if grep -qE '^gateway-host-key +present +present +yes' <<<"$out"; then
  ok "status reports both sides matching"
else
  bad "status did not report a match: $out"
fi
if grep -q 'github-app-key' <<<"$out"; then
  bad "status listed an excluded secret"
else
  ok "status omits excluded secrets"
fi

# --- pull: the fresh-checkout case -------------------------------------------
# This is what the whole thing is for: the identity is gone and must come back
# byte for byte, or the node's pin and the rootfs template's baked-in key both
# stop matching.
before_host=$(cat "$keys/gateway_host_key.pem")
before_up=$(cat "$keys/gateway_upstream_key.pem")
before_oidc=$(cat "$keys/oidc_signing_key.pem")
rm -f "$keys"/*.pem "$keys"/*.pub

out=$(dev pull)
if [ "$(cat "$keys/gateway_host_key.pem" 2>/dev/null)" = "$before_host" ]; then
  ok "pull restores the host key byte for byte"
else
  bad "pull did not restore the host key: $out"
fi
if [ "$(cat "$keys/gateway_upstream_key.pem" 2>/dev/null)" = "$before_up" ]; then
  ok "pull restores the upstream key byte for byte"
else
  bad "pull did not restore the upstream key"
fi
if [ "$(cat "$keys/oidc_signing_key.pem" 2>/dev/null)" = "$before_oidc" ]; then
  ok "pull restores the OIDC signing key (the KEK for every user secret)"
else
  bad "pull did not restore the OIDC signing key"
fi

# The .pub halves are derived, not stored, and pull must derive them itself:
# gateway.sh's derive_trust_pubs skips a .pub that already exists, so it would
# leave the previous identity's public half sitting next to the newly restored
# .pem. Without these, up.sh's seed_trust has nothing to give the node.
for name in gateway_host_key gateway_upstream_key; do
  if [ -s "$keys/$name.pub" ]; then
    ok "pull derived $name.pub"
  else
    bad "pull left no $name.pub — seed_trust would have nothing to copy"
  fi
done
# Derived, so it must agree with the restored private half. Both files must
# exist for the comparison to mean anything — two missing files compare equal.
if [ -s "$keys/gateway_upstream_key.pub" ] && [ -s "$keys/gateway_upstream_key.pem" ] &&
   [ "$(cat "$keys/gateway_upstream_key.pub")" = "$(ssh-keygen -y -f "$keys/gateway_upstream_key.pem")" ]; then
  ok "the derived public half matches its private key"
else
  bad "derived public half does not match its private key"
fi
# github_app_key was never escrowed, so a pull must not resurrect a stale one.
if [ -e "$keys/github_app_key.pem" ]; then
  bad "pull wrote a github_app_key.pem from the dev vault"
else
  ok "pull did not invent a github-app key"
fi

# --- refusals -----------------------------------------------------------------
out=$(VAULT=Sparkbox dev status)
if [ $? -ne 0 ] && grep -q 'refusing to use the fleet vault' <<<"$out"; then
  ok "refuses the fleet vault (push there would overwrite the fleet identity)"
else
  bad "did not refuse the fleet vault: $out"
fi

# A write that is accepted and silently discarded is the failure put_secret's
# read-back exists to catch. It must be loud, and it must not exit 0.
rm -f "$OP_STORE/Sparkbox-Dev/gateway-host-key"
out=$(OP_DISCARD_WRITES=1 dev push)
if [ $? -ne 0 ] && grep -q 'FAILED' <<<"$out"; then
  ok "a silently-discarded write fails loudly instead of reporting a backup"
else
  bad "a discarded write was reported as success: $out"
fi

# An unreachable vault must be reported once, not once per secret.
out=$(OP_FAIL_VAULT=1 dev status)
if [ $? -ne 0 ] && [ "$(grep -c 'cannot reach vault' <<<"$out")" -le 1 ]; then
  ok "an unreachable vault is reported once and stops"
else
  bad "unreachable vault handling: $out"
fi

# --- the shared script, unchanged by the dev wrapper --------------------------
# SECRETS_EXCLUDE was added to deploy/sync-fleet-secrets.sh for the dev box's
# sake, and that script syncs the real fleet. Prove the default is still "sync
# everything": an empty exclude list must exclude nothing.
fleet_keys="$WORK/fleet"
mkdir -p "$fleet_keys" "$OP_STORE/Fleet-Test"
ssh-keygen -q -t ed25519 -N '' -f "$fleet_keys/gateway_host_key.pem" 2>/dev/null
ssh-keygen -q -t ed25519 -N '' -f "$fleet_keys/gateway_upstream_key.pem" 2>/dev/null
rm -f "$fleet_keys"/*.pem.pub
out=$(SECRETS_DIR="$fleet_keys" OP_VAULT=Fleet-Test CONSOLE_PASSWORD=hunter2         "$module_dir/deploy/sync-fleet-secrets.sh" push 2>&1)
if [ $? -eq 0 ] && [ -s "$OP_STORE/Fleet-Test/console-password" ] &&
   [ -s "$OP_STORE/Fleet-Test/github-webhook-secret" ]; then
  ok "an empty SECRETS_EXCLUDE still syncs the full fleet manifest"
else
  bad "the fleet default regressed: $out"
fi

# --- the default state dir ----------------------------------------------------
# The one path every other check in this file steps over: `dev()` above always
# passes SPARKBOX_DEV_STATE_DIR, so the DEFAULT was never exercised — and it was
# wrong. It named .dev/gateway/state while gateway.sh puts the state dir at
# .dev/gateway/durable/gateway/control, so node_ca_key.pem was always found in
# the key dir and node_ca_cert.pem never was, and the fleet script refuses that
# combination as a half-written CA. Every real run would have died there; only a
# run with the override could pass, which is to say only this file's runs.
#
# Asserted against gateway.sh's own definition rather than a literal, because
# the invariant is that the two agree — a path spelled correctly here and then
# moved there is the same outage. Source-level rather than behavioural for the
# reason the bug survived: taking the default means writing into the checkout's
# real .dev/, which a test must not do.
resolve_state_dir() {
  (
    unset SPARKBOX_DEV_STATE_DIR
    gw_dir=GW
    # Only the assignments, without `readonly`, so both can be evaluated in one
    # shell and neither script's other side effects run.
    eval "$(grep -E '^readonly (durable_dir|state_dir)=' "$1" | sed 's/^readonly //')"
    printf '%s' "${state_dir:-}"
  )
}
gw_state=$(resolve_state_dir "$script_dir/gateway.sh")
dev_state=$(resolve_state_dir "$secrets_sh")
if [ -z "$gw_state" ] || [ -z "$dev_state" ]; then
  # Not "they differ": one of the scripts stopped declaring a state_dir the way
  # this check reads it, which would otherwise pass as two empty strings.
  bad "could not resolve a state dir (gateway.sh=$gw_state secrets.sh=$dev_state)"
elif [ "$gw_state" = "$dev_state" ]; then
  ok "secrets.sh's default state dir is gateway.sh's (${dev_state#GW/})"
else
  bad "secrets.sh defaults the state dir to ${dev_state#GW/} but gateway.sh writes it to ${gw_state#GW/};
       node_ca_cert.pem will never be found and every push will refuse a half-written CA"
fi

echo
if [ "$FAILURES" -eq 0 ]; then
  echo "all $CASES checks passed"
  exit 0
fi
echo "$FAILURES of $CASES checks FAILED"
exit 1
