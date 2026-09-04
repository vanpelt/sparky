#!/usr/bin/env bash
# Bring the whole local dev environment up, from a fresh checkout to a gateway
# that can boot real microVMs, in one idempotent command.
#
# WHY THIS EXISTS. Every piece of this already had a script; what nobody had was
# the order and the three joins between them. A new developer had to know that
# the gateway's SSH listener must leave loopback before a node can link, that
# the node verifies a host key which has to be copied into the machine first,
# and that the two halves reserve guest subnets which must not overlap. None of
# that is discoverable from the individual scripts, and each failure surfaces
# far from its cause: an overlap is refused at `node approve`, a missing host
# key looks like a node that just never comes online, and a loopback listener
# looks like a gateway with no nodes at all.
#
# WHAT IT DELEGATES, rather than reimplements:
#   machine.sh ensure   the Apple container machine's devices, docker, XFS, registry trust
#   image.sh all        registry up, build from the working tree, push, pull
#   gateway.sh start    the real production gateway entrypoint, natively
#   sparkbox devpod up  the five-container node Pod, rendered from deployment.yaml
#
# WHAT IT WILL NOT DO: create the container machine (see machine.sh's header —
# it is a ~27GB one-way ratchet and making one is `sparkbox setup`'s job), or
# complete anything that needs a human at a browser. Those are reported.
#
# Usage:
#   hack/dev/up.sh              everything, idempotent; safe to re-run
#   hack/dev/up.sh status       what is up and what is missing, read-only
#   hack/dev/up.sh down         stop the gateway and the node Pod; keep the
#                               machine, the image, and the node's data volume
#
# Env: everything the delegated scripts take, plus
#   SPARKBOX_DEV_NODE_NAME      the node's name in the roster (default macdev)
#   SPARKBOX_DEV_SKIP_IMAGE     1 to skip the build/push/pull (default 0)
set -euo pipefail

# --- bash floor -------------------------------------------------------------
# Same floor and same reason as gateway.sh: the production entrypoints expand
# possibly-empty arrays under `set -u`, which bash 3.2 (all macOS ships) treats
# as an unbound variable. Re-exec into a newer bash rather than fail late.
if [ -z "${BASH_VERSINFO:-}" ] ||
   [ "${BASH_VERSINFO[0]}" -lt 4 ] ||
   { [ "${BASH_VERSINFO[0]}" -eq 4 ] && [ "${BASH_VERSINFO[1]}" -lt 4 ]; }; then
  for candidate in /opt/homebrew/bin/bash /usr/local/bin/bash /opt/local/bin/bash; do
    [ -x "$candidate" ] && exec "$candidate" "$0" "$@"
  done
  echo "hack/dev/up.sh needs bash >= 4.4; this is ${BASH_VERSION:-unknown}" >&2
  echo "fix: brew install bash" >&2
  exit 1
fi

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
readonly script_dir
readonly module_dir=$(cd -- "$script_dir/../.." && pwd)

readonly machine="${SPARKBOX_DEV_MACHINE:-sparkbox}"
readonly host_ip="${SPARKBOX_DEV_HOST_IP:-192.168.64.1}"
readonly reg_port="${SPARKBOX_DEV_REGISTRY_PORT:-5001}"
readonly image="${SPARKBOX_DEV_IMAGE:-sparkbox-cks:dev}"
readonly pull_ref="$host_ip:$reg_port/$image"
readonly data_root="${SPARKBOX_DEV_DATA_DIR:-/srv/sparkbox/data}"
readonly pod_data="$data_root/devpod"
readonly pod_trust="$data_root/devpod-trust"
readonly node_name="${SPARKBOX_DEV_NODE_NAME:-macdev}"
readonly ssh_port="${SPARKBOX_DEV_SSH_PORT:-2222}"
readonly key_dir="$module_dir/.dev/gateway/keys"
readonly host_key_pub="$key_dir/gateway_host_key.pub"

# The node links out to this Mac, so the listener cannot be on loopback. Forced
# rather than defaulted: the entire purpose of this script is a gateway with a
# VM node, and a run that quietly produced one without is the failure this file
# exists to prevent.
export SPARKBOX_DEV_SSH_BIND="${SPARKBOX_DEV_SSH_BIND:-0.0.0.0}"

step()  { printf '\n\033[1m==> %s\033[0m\n' "$*"; }
note()  { printf '    %s\n' "$*"; }
die()   { printf 'up.sh: %s\n' "$*" >&2; exit 1; }

# The one transport into the machine. -i is mandatory; see machine.sh's header.
mrun() { container machine run -i --root --name "$machine" -- bash -s; }

# --- preflight --------------------------------------------------------------
preflight() {
  local missing=()
  command -v container > /dev/null 2>&1 || missing+=("container (Apple's container CLI)")
  command -v docker    > /dev/null 2>&1 || missing+=("docker (for the local registry and the image build)")
  command -v go        > /dev/null 2>&1 || missing+=("go (the gateway is built from this working tree)")
  if [ "${#missing[@]}" -gt 0 ]; then
    printf 'up.sh: missing prerequisites:\n' >&2
    printf '  - %s\n' "${missing[@]}" >&2
    exit 1
  fi
}

# --- the node's trust bundle ------------------------------------------------
# ONE file, never the directory: $key_dir also holds five private keys, and the
# node has no business with any of them. It verifies the gateway's host key on
# every link and needs exactly that public half.
#
# Re-copied on every run rather than only when absent, because the failure it
# prevents is silent: re-minting the gateway identity leaves a stale .pub here
# and the node then refuses to link, with the mismatch visible only in its logs.
seed_trust() {
  [ -f "$host_key_pub" ] || die "no $host_key_pub — start the gateway once so it mints its identity"
  {
    printf "mkdir -p %q\ncat > %q <<'PUBKEY'\n" "$pod_trust" "$pod_trust/gateway_host_key.pub"
    cat "$host_key_pub"
    printf "PUBKEY\necho SEEDED\n"
  } | mrun | grep -q SEEDED || die "could not write the trust bundle into the machine"
}

# --- the node Pod -----------------------------------------------------------
# Whether this node is already linked and carrying work. Everything below is
# gated on it, because `up` is meant to be re-runnable and re-running it must
# not cost somebody their running sandboxes: `devpod down` stops the node, and
# the guests on it go with it. A converged environment is left alone.
node_online() {
  case "$(ctl node ls 2>/dev/null | grep -F "$node_name" || true)" in
    *online*) return 0 ;;
  esac
  return 1
}

# How many sandboxes the roster says this node is carrying, so a restart that IS
# necessary can say what it is about to stop rather than doing it silently.
node_sandbox_count() {
  ctl node ls 2>/dev/null | grep -F "$node_name" |
    grep -o '[0-9]* sandbox' | head -1 | awk '{print $1}'
}

# The devpod binary comes out of the image rather than being cross-compiled,
# so the translator that renders the Pod is the same build the Pod itself runs.
start_node() {
  mrun <<GUEST
set -euo pipefail
IMG=$pull_ref
SB=/usr/local/bin/sparkbox-dev
cid=\$(docker create "\$IMG")
docker cp "\$cid:/usr/local/bin/sparkbox" "\$SB" > /dev/null
docker rm -f "\$cid" > /dev/null
chmod 0755 "\$SB"

# Down first so a re-run re-reads the trust bundle and re-links; the data volume
# is kept (no -purge-data), so the rootfs template and node identity survive.
\$SB devpod down -image "\$IMG" -data $pod_data > /dev/null 2>&1 || true
\$SB devpod up -image "\$IMG" -data $pod_data -trust-dir $pod_trust \\
  -gateway $host_ip:$ssh_port -driver firecracker -node-name $node_name 2>&1 | tail -3
GUEST
}

# What the node itself printed at startup: its fingerprint, and the guest subnet
# it will report. Read from the machine, NOT from the gateway's roster — that
# distinction is the whole ceremony. See approve_node.
node_says() {
  mrun <<GUEST 2>/dev/null
docker logs sparkbox-dev-sparkbox-node 2>&1 |
  grep -o 'node approve SHA256:[A-Za-z0-9+/]* --guest-subnet [0-9./]*' | tail -1
GUEST
}

# --- approval ---------------------------------------------------------------
# A node picks its own name and the gateway has nothing to check it against, so
# approving a NAME means trusting a string a stranger chose. Approving a
# FINGERPRINT cannot be claimed that way — see ctlops.ApproveNode, which argues
# it at length. The documented ceremony is: read the fingerprint off the
# machine's own console, compare it against `node ls`, approve what matches.
#
# That is exactly what happens here, and it is automated rather than skipped.
# This script started the machine's half, so it can read that console directly;
# the comparison against the roster is still made, and a mismatch still refuses.
# What it does NOT do is approve whatever the roster happens to be offering,
# which is the shortcut that would make the ceremony theatre.
approve_node() {
  local claim fp subnet rostered
  claim=$(node_says | tr -d '\r')
  [ -n "$claim" ] || die "the node has not printed a fingerprint yet; try again in a few seconds"
  fp=$(printf '%s' "$claim" | awk '{print $3}')
  subnet=$(printf '%s' "$claim" | awk '{print $5}')
  [ -n "$fp" ] && [ -n "$subnet" ] || die "could not parse the node's fingerprint from: $claim"

  rostered=$(ctl node ls 2>/dev/null | grep -F "$fp" || true)
  if [ -z "$rostered" ]; then
    die "the node reports $fp but the gateway's roster does not list it — it has not enrolled yet"
  fi
  case "$rostered" in
    *approved*)
      note "already approved: $fp"
      return 0
      ;;
  esac
  note "node console says:  $fp"
  note "roster agrees; approving with the subnet it reports ($subnet)"
  ctl node approve "$fp" --guest-subnet "$subnet"
}

# --- the gateway's listener -------------------------------------------------
# A gateway that is already running may have been started by gateway.sh without
# the wider bind, in which case it is healthy, answers every ctl command, and
# silently cannot accept a node. Read the actual listen address rather than
# assuming: the symptom otherwise is "this gateway has no VM nodes" from a node
# that is running perfectly well a few feet away.
gateway_accepts_nodes() {
  local listeners
  command -v lsof > /dev/null 2>&1 || return 0 # cannot tell; assume the caller knows
  listeners=$(lsof -nP -iTCP:"$ssh_port" -sTCP:LISTEN 2>/dev/null | tail -n +2) || true
  [ -n "$listeners" ] || return 1
  # Any listener not bound to loopback will do: 0.0.0.0, or the vmnet address.
  printf '%s\n' "$listeners" | grep -qv '127\.0\.0\.1:'
}

# --- talking to the gateway -------------------------------------------------
# Always 127.0.0.1 even though the listener is wider: this Mac is the operator.
ctl() {
  ssh -p "$ssh_port" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
      -o ConnectTimeout=8 -o LogLevel=ERROR ctl@127.0.0.1 "$@"
}

# The node retries the link with growing backoff while it is pending, so there
# is real latency between approval and `online`. Waiting here rather than
# leaving the developer to poll is most of the point of this script.
wait_online() {
  local deadline=$((SECONDS + 150))
  while [ "$SECONDS" -lt "$deadline" ]; do
    case "$(ctl node ls 2>/dev/null | grep -F "$node_name" || true)" in
      *online*) note "node $node_name is online"; return 0 ;;
    esac
    sleep 5
  done
  note "node $node_name did not come online within 150s."
  note "its link retries with growing backoff, so give it another minute before"
  note "digging; the reason is in the node's own log:"
  note "  hack/dev/machine.sh shell   then: docker logs --tail 20 sparkbox-dev-sparkbox-node"
  return 1
}

cmd_up() {
  preflight
  [ "$SPARKBOX_DEV_SSH_BIND" = "127.0.0.1" ] &&
    die "SPARKBOX_DEV_SSH_BIND=127.0.0.1 cannot accept a node link: the container machine
     cannot reach loopback on this Mac. Unset it, or use hack/dev/gateway.sh directly
     for a gateway-only run."

  step "Apple container machine"
  "$script_dir/machine.sh" ensure

  if [ "${SPARKBOX_DEV_SKIP_IMAGE:-0}" = 1 ]; then
    step "image (skipped: SPARKBOX_DEV_SKIP_IMAGE=1)"
  else
    step "node image: build from this working tree, push, pull"
    "$script_dir/image.sh" all
  fi

  step "gateway"
  if "$script_dir/gateway.sh" status > /dev/null 2>&1; then
    if gateway_accepts_nodes; then
      note "already running and accepting node links"
    else
      note "running, but its SSH listener is on loopback and no node can reach it"
      note "restarting it on $SPARKBOX_DEV_SSH_BIND"
      "$script_dir/gateway.sh" restart > /dev/null
    fi
  else
    "$script_dir/gateway.sh" start > /dev/null
    note "started"
  fi

  step "trust bundle"
  seed_trust
  note "copied gateway_host_key.pub into $pod_trust"

  step "node Pod"
  if node_online; then
    # The converged case, and the common one on a re-run. Stopping the Pod here
    # would stop every guest on it — including a build somebody kicked off two
    # minutes ago — to reach a state it is already in.
    note "$node_name is already linked and online, carrying $(node_sandbox_count) sandbox(es); leaving it alone"
    note "to rebuild it anyway: hack/dev/up.sh down && hack/dev/up.sh"
  else
    local carrying
    carrying=$(node_sandbox_count)
    if [ -n "$carrying" ] && [ "$carrying" != 0 ]; then
      note "restarting the node Pod; the $carrying sandbox(es) it holds will stop"
    fi
    start_node

    step "approval"
    approve_node
    wait_online || true
  fi

  step "ready"
  "$script_dir/gateway.sh" status
  echo
  note "boot a sandbox:  ssh -p $ssh_port new+mybox@127.0.0.1"
  human_steps
}

# The things this cannot finish, stated plainly rather than left to be
# discovered when a clone fails inside a VM.
human_steps() {
  cat <<EOF

    Two things still need a human, and both are optional:

      GitHub repo attachments need the App key out of 1Password, and a GitHub
      account linked to your handle:
        op signin --account coreweave.1password.com   # if 'op vault list' fails
        ssh -p $ssh_port ctl@127.0.0.1 github link
        ssh -p $ssh_port ctl@127.0.0.1 repo add <owner>/<name>

      The App must also be installed on those repositories:
        ssh -p $ssh_port ctl@127.0.0.1 github install
EOF
}

cmd_down() {
  step "node Pod"
  mrun <<GUEST 2>&1 | tail -2 || true
/usr/local/bin/sparkbox-dev devpod down -image $pull_ref -data $pod_data 2>&1 | tail -2
GUEST
  step "gateway"
  "$script_dir/gateway.sh" stop
  note "the machine, the image and $pod_data are kept."
  note "the node's identity and rootfs template live in that volume, so the next"
  note "\`up\` re-links the same node rather than enrolling a second one."
}

cmd_status() {
  step "gateway"
  "$script_dir/gateway.sh" status || true
  step "nodes"
  ctl node ls 2>&1 || note "the gateway is not answering on 127.0.0.1:$ssh_port"
  step "machine"
  "$script_dir/machine.sh" status || true
}

case "${1:-up}" in
  up) cmd_up ;;
  down) cmd_down ;;
  status) cmd_status ;;
  *)
    echo "usage: hack/dev/up.sh [up|down|status]" >&2
    exit 2
    ;;
esac
