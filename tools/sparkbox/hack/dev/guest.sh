#!/usr/bin/env bash
# Read a guest's serial console, for a guest the control plane cannot reach.
#
# WHY THIS EXISTS. Every supported way into a sandbox goes through the in-guest
# agent on :8000 — `ssh <name>@gateway`, the browser terminal, the REST API.
# That is correct almost always and useless in the one case where you most need
# to see something: a guest whose boot never finished, so the agent never
# started. The gateway answers "could not reach the sandbox's shell", the node
# logs `connection refused` on :8000 every couple of seconds, and there is no
# next step.
#
# The console asks nothing of the guest. firecracker writes ttyS0 to the
# vmm-helper container's stdout, so it works with no network, no sshd and no
# agent — the one view that survives when everything else is gone. Raw it is
# unreadable, so it is de-noised here.
#
# There is deliberately no `shell` mode. One lived here and was deleted: it
# stood up a bind-then-setns forwarder to reach the guest's own sshd, and it
# never worked — it dialled with the operator's ssh identities while a guest's
# authorized_keys holds only the gateway's key, and its reused listener port
# could answer for the wrong guest, which is indistinguishable from the bug you
# would be debugging. If you need a shell past the agent, the node container is
# already in the guest's network namespace and already has an ssh client:
#
#   container machine run -i --root --name sparkbox -- \
#     docker exec -i sparkbox-dev-sparkbox-node \
#     ssh -i /run/sparkbox/trust/... sparky@<guest-ip>
#
# Usage:
#   hack/dev/guest.sh console <name> [lines]   the guest's boot log, de-noised
#   hack/dev/guest.sh console <name> -f        follow it
#
# Env:
#   SPARKBOX_DEV_MACHINE  container machine name (default sparkbox)
set -euo pipefail

# Same bash floor and the same reason as up.sh and gateway.sh.
if [ -z "${BASH_VERSINFO:-}" ] ||
   [ "${BASH_VERSINFO[0]}" -lt 4 ] ||
   { [ "${BASH_VERSINFO[0]}" -eq 4 ] && [ "${BASH_VERSINFO[1]}" -lt 4 ]; }; then
  for candidate in /opt/homebrew/bin/bash /usr/local/bin/bash /opt/local/bin/bash; do
    [ -x "$candidate" ] && exec "$candidate" "$0" "$@"
  done
  echo "hack/dev/guest.sh needs bash >= 4.4; this is ${BASH_VERSION:-unknown}" >&2
  echo "fix: brew install bash" >&2
  exit 1
fi

readonly machine="${SPARKBOX_DEV_MACHINE:-sparkbox}"
readonly prefix="${SPARKBOX_DEV_PREFIX:-sparkbox-dev}"
readonly helper="$prefix-vmm-helper"

note() { printf '    %s\n' "$*"; }
die()  { printf 'guest.sh: %s\n' "$*" >&2; exit 1; }

# The one transport into the machine. -i is mandatory: without it stdin is
# discarded and bash exits 0 having run nothing. See machine.sh's header.
mrun() { container machine run -i --root --name "$machine" -- bash -s; }

command -v container > /dev/null 2>&1 || die "no \`container\` CLI on this Mac"

# --- which VM is which ------------------------------------------------------
# A sandbox's guest address comes from the slot the node gave it. Rather than
# reimplement that arithmetic, read the address firecracker was actually booted
# with: every VM's boot_args is logged verbatim on the /boot-source PUT, and it
# carries both `sparkbox_host=<name>` and `ip=<guest>::<gateway>:...`.
#
# The two are matched on the same line rather than in one pattern, because
# boot_args is a set and not a sequence: sparkbox_host comes after ip= for one
# VM and before it for the next, so any single expression spanning them matches
# only whichever order happened to be sampled while it was written.
guest_ip() {
  local name=$1
  mrun <<GUEST 2>/dev/null | tr -d '\r' | tail -1
docker logs $helper 2>&1 |
  grep -F '/boot-source' |
  grep -E 'sparkbox_host=$name([^A-Za-z0-9._-]|\$)' |
  tail -1 |
  grep -oE 'ip=[0-9.]+::' |
  head -1 |
  sed -e 's/^ip=//' -e 's/:://'
GUEST
}

require_guest() {
  local name=$1 ip
  [ -n "$name" ] || die "which sandbox? usage: guest.sh $2 <name>"
  ip=$(guest_ip "$name")
  [ -n "$ip" ] || die "no VM named $name has booted on this node.
     \`ssh -p 2222 ctl@127.0.0.1 list\` shows what exists; a sandbox that is
     paused has no address until something resumes it."
  printf '%s' "$ip"
}

# --- console ----------------------------------------------------------------
# firecracker writes the guest's ttyS0 to its own stdout, so the console is
# interleaved with the VMM's API log and with systemd's ANSI progress spinner
# rewriting one line thousands of times. Both are stripped: what is left is the
# boot log you would have seen on a physical console.
cmd_console() {
  local name=${1:-} arg=${2:-200} follow=""
  [ -n "$name" ] || die "which sandbox? usage: guest.sh console <name> [lines|-f]"
  local lines=200
  case "$arg" in
    -f|--follow) follow="--follow" ;;
    ''|*[!0-9]*) die "console takes a line count or -f, not $arg" ;;
    *) lines=$arg ;;
  esac
  note "serial console of $name (firecracker's own API log filtered out)"
  # Three passes, each removing something that is not the guest talking:
  #   sed   ANSI. Both forms: CSI (ESC [ … letter) and the two-character
  #         escapes systemd's progress bar uses, which leave a bare "M" on
  #         every line if you only strip the first form.
  #   grep  firecracker's own API log, which shares this stdout.
  #   awk   systemd's progress spinner. It rewrites one line several times a
  #         second, so a stuck boot buries its own evidence under thousands of
  #         redraws. Held back and reported once at the end instead — which
  #         only works for a bounded read, so -f passes them through.
  mrun <<GUEST
docker logs ${follow:-} --tail $lines $helper 2>&1 |
  sed -e 's/\\x1b\\[[0-9;?]*[a-zA-Z]//g' -e 's/\\x1b[@-Z\\\\-_]//g' -e 's/\\r/\\n/g' |
  grep -vE 'anonymous-instance:(fc_api|main)' |
  grep -vE '^[[:space:]]*\$' |
  awk -v follow="${follow:-}" '
    # Both sides of the bookkeeping must reduce a unit name the same way, and
    # systemd truncates it to the terminal width in BOTH the progress line and
    # the completion line — at different points. A fixed-length prefix of the
    # unit name (never its /start suffix) is the only stable key.
    function unitkey(t,   k) {
      k = t
      sub(/[^A-Za-z0-9._@-].*/, "", k)
      return substr(k, 1, 16)
    }
    /Job .*running \\(/ {
      if (follow != "") { print; next }
      line = \$0
      sub(/^.*Job /, "", line)
      # Key on the unit name alone. systemd truncates it to the terminal width,
      # so the SAME job appears as "sparkbox-env-setup.ser…" in one frame and
      # "sparkbox-env-setup.ser…art" in the next; keying on the whole line would
      # report one stuck unit as several.
      waiting[unitkey(line)] = line   # last frame wins: the newest elapsed time
      next
    }
    # A unit that later reports Finished/Started/Failed is no longer waiting.
    # Without this the summary lists everything that was EVER pending in the
    # window, which on a slow boot is most of the boot and reads as nine stuck
    # units when eight of them completed.
    /(Finished|Started|Failed to start) / {
      done = \$0
      sub(/^.*(Finished|Started|Failed to start) /, "", done)
      delete waiting[unitkey(done)]
      print
      next
    }
    # Spinner debris. firecracker writes its own log to this same stdout, so a
    # progress line is regularly cut in half by one, and the tail arrives as its
    # own line: "54s / 1h 32min 31s)". Whole progress lines were consumed above,
    # so anything still carrying a progress timer here is a fragment.
    / \/ (no limit|[0-9]+h|[0-9]+min|[0-9]+s)/ { next }
    /^\\[[ *]*\\]\$/ { next }
    { print }
    END {
      n = 0
      for (job in waiting) n++
      if (n > 0) {
        print ""
        print "--- still waiting, as of the last console frame ---"
        for (job in waiting) print "    " waiting[job]
      }
    }'
GUEST
}

case "${1:-}" in
  console) shift; cmd_console "$@" ;;
  *)
    echo "usage: hack/dev/guest.sh console <name> [lines|-f]" >&2
    exit 2
    ;;
esac
