#!/usr/bin/env bash
# Reach a guest the control plane cannot reach.
#
# WHY THIS EXISTS. Every supported way into a sandbox goes through the in-guest
# agent on :8000 — `ssh <name>@gateway`, the browser terminal, the REST API.
# That is correct almost always and useless in the one case where you most need
# a shell: a guest whose boot never finished, so the agent never started. The
# gateway then answers "could not reach the sandbox's shell; it may still be
# starting", the node logs `connection refused` on :8000 every couple of
# seconds, and there is no next step. Measured on the failure this was written
# for: a wedged guest burned four vCPUs for forty minutes and the only evidence
# anyone could get at was systemd's progress spinner scrolling past in a log.
#
# Two things are still reachable when the agent is not, and this script is
# those two:
#
#   console   the guest's serial console, which firecracker writes to the
#             vmm-helper container's stdout. It is the boot log, and it is the
#             ONLY thing that works when the guest has no working network and
#             no sshd — but it is also read-only and interleaved with
#             firecracker's own API chatter, so it is de-noised here.
#
#   shell     the guest's own sshd on :22, bypassing the agent entirely. It
#             lives in the node pod's network namespace, which neither the Mac
#             nor even the machine's root namespace can route to, so `jump`
#             stands up a forwarder: it binds a listener in the root namespace
#             (where the Mac can reach it over vmnet) and only then enters the
#             guest namespace to dial. A socket keeps the namespace it was
#             bound in, which is what lets one process straddle both.
#
# Usage:
#   hack/dev/guest.sh console <name> [lines]   the guest's boot log, de-noised
#   hack/dev/guest.sh console <name> -f        follow it
#   hack/dev/guest.sh shell   <name> [cmd…]    ssh in past the agent
#   hack/dev/guest.sh jump    <name> [port]    leave a forwarder up (default 2223)
#   hack/dev/guest.sh stop                     tear the forwarder down
#
# `shell` brings its own forwarder up and takes it down again; `jump` leaves one
# running so you can attach anything else — scp, a second terminal, a port.
#
# Env:
#   SPARKBOX_DEV_MACHINE     container machine name (default sparkbox)
#   SPARKBOX_DEV_GUEST_USER  the guest account to log in as (default sparky)
#   SPARKBOX_DEV_JUMP_PORT   forwarder port on the machine (default 2223)
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
readonly guest_user="${SPARKBOX_DEV_GUEST_USER:-sparky}"
readonly jump_port="${SPARKBOX_DEV_JUMP_PORT:-2223}"
readonly prefix="${SPARKBOX_DEV_PREFIX:-sparkbox-dev}"
readonly helper="$prefix-vmm-helper"
readonly netns="$prefix-netns"
readonly remote_script=/run/sparkbox-dev-guestjump.py

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

# --- the forwarder ----------------------------------------------------------
# Written into the machine as a file rather than passed as an argument:
# `container machine run -- <argv>` joins its arguments into a shell command
# string, so anything with quotes or newlines in it is re-split and mangled.
install_forwarder() {
  mrun <<'GUEST' > /dev/null
cat > /run/sparkbox-dev-guestjump.py <<'PYEOF'
# Bind in the root netns, then setns() into the pod's before dialing.
#
# The guest lives in the node pod's network namespace: nothing on the Mac, and
# nothing in the machine's root namespace, has a route to it. But a socket
# belongs to the namespace it was BOUND in, not the one its process is in now.
# So bind first, where the Mac can reach us over vmnet, and only then enter the
# namespace where the guest exists. One process, both sides.
import ctypes, os, socket, sys, threading

port, target, tport, nspid = int(sys.argv[1]), sys.argv[2], int(sys.argv[3]), sys.argv[4]

srv = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
srv.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
srv.bind(("0.0.0.0", port))
srv.listen(16)

libc = ctypes.CDLL("libc.so.6", use_errno=True)
fd = os.open("/proc/%s/ns/net" % nspid, os.O_RDONLY)
if libc.setns(fd, 0x40000000) != 0:                      # CLONE_NEWNET
    raise OSError(ctypes.get_errno(), "setns into the pod netns failed")
print("ready :%d -> %s:%d" % (port, target, tport), flush=True)

def pump(a, b):
    try:
        while True:
            chunk = a.recv(65536)
            if not chunk:
                break
            b.sendall(chunk)
    except Exception:
        pass
    for s in (a, b):
        try:
            s.shutdown(socket.SHUT_RDWR)
        except Exception:
            pass

def serve(client):
    try:
        guest = socket.create_connection((target, tport), timeout=10)
    except Exception as err:
        print("dial %s:%d failed: %r" % (target, tport, err), flush=True)
        client.close()
        return
    # The connect timeout must not become a read timeout: ssh's banner
    # exchange is idle for as long as the guest takes to answer, and a wedged
    # guest takes longer than ten seconds. Leaving it set closes the
    # connection mid-handshake, which reads as "sshd refused" and is not.
    guest.settimeout(None)
    threading.Thread(target=pump, args=(client, guest), daemon=True).start()
    pump(guest, client)

while True:
    conn, _ = srv.accept()
    threading.Thread(target=serve, args=(conn,), daemon=True).start()
PYEOF
GUEST
}

# Address of the machine on the vmnet the Mac shares with it. Read rather than
# assumed: it is not always 192.168.64.2, and a wrong guess here looks exactly
# like a guest that is not listening.
machine_ip() {
  mrun <<'GUEST' 2>/dev/null | tr -d '\r' | tail -1
ip -4 -o addr show dev eth0 | awk '{print $4}' | cut -d/ -f1
GUEST
}

# Kill any previous forwarder and start one for THIS sandbox, in a SINGLE guest
# shell so the kill is complete before the bind.
#
# Both halves of that matter and both were learned the hard way. Killing from a
# separate `container machine run` raced the new process to the port: the bind
# lost with EADDRINUSE while the OLD forwarder kept serving — still pointed at a
# previous sandbox's address, possibly one that no longer exists. And the
# readiness check has to name the TARGET, not just the word "ready", or the
# survivor's own banner satisfies it and `shell` connects you to the wrong
# guest. That failure looks like `Connection reset by peer` from a sandbox that
# is perfectly healthy, which is a long way from its cause.
start_forwarder() {
  local ip=$1 port=$2
  install_forwarder
  local out want="ready :$port -> $ip:22"
  out=$(mrun <<GUEST
pkill -f "$remote_script" 2>/dev/null || true
for _ in 1 2 3 4 5 6 7 8 9 10; do
  pgrep -f "$remote_script" > /dev/null 2>&1 || break
  sleep 0.5
done
if pgrep -f "$remote_script" > /dev/null 2>&1; then
  echo "a previous forwarder will not die; kill it by hand in the machine"
  exit 1
fi

nspid=\$(docker inspect -f '{{.State.Pid}}' $netns 2>/dev/null) || true
if [ -z "\${nspid:-}" ]; then
  echo "the node pod's network container ($netns) is not running"
  exit 1
fi

log=/run/sparkbox-dev-guestjump.log
: > "\$log"
nohup python3 -u $remote_script $port $ip 22 "\$nspid" > "\$log" 2>&1 &
fwd=\$!
# Wait for the ready LINE, not merely for the file to be non-empty. The old
# check gave python 5 seconds to produce any output at all, which it loses on a
# machine whose cores are all inside a guest — and an empty file then read as
# "the pod is not running", which is a long way from "the box is busy".
for _ in \$(seq 1 120); do
  grep -q 'ready :' "\$log" 2>/dev/null && break
  kill -0 "\$fwd" 2>/dev/null || break   # it died; the log has the reason
  sleep 0.5
done
cat "\$log"
GUEST
)
  case "$out" in
    *"$want"*) return 0 ;;
    *) die "could not forward to $ip:22 in the machine:
     ${out:-(no output — is the node pod running? hack/dev/up.sh status)}" ;;
  esac
}

cmd_stop() {
  mrun <<GUEST > /dev/null 2>&1 || true
pkill -f "$remote_script" || true
GUEST
  note "forwarder stopped"
}

cmd_jump() {
  local name=${1:-} port=${2:-$jump_port} ip mip
  ip=$(require_guest "$name" jump)
  start_forwarder "$ip" "$port"
  mip=$(machine_ip)
  note "$name is $ip inside the pod; forwarding $mip:$port to its sshd"
  note "  ssh -p $port $guest_user@$mip"
  note "stop it with: hack/dev/guest.sh stop"
}

cmd_shell() {
  local name=${1:-} ip mip
  ip=$(require_guest "$name" shell)
  shift || true
  start_forwarder "$ip" "$jump_port"
  mip=$(machine_ip)
  note "$name is $ip inside the pod; going in past the agent"
  # StrictHostKeyChecking is off and the known_hosts file is /dev/null on
  # purpose: the port is reused for whichever guest is being debugged, so a
  # pinned key would be a mismatch warning every single time.
  local rc=0
  ssh -p "$jump_port" \
      -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
      -o LogLevel=ERROR -o ConnectTimeout=20 \
      "$guest_user@$mip" "$@" || rc=$?
  cmd_stop > /dev/null 2>&1 || true
  if [ "$rc" -ne 0 ]; then
    note ""
    note "if that timed out during the banner exchange, sshd is running but the"
    note "guest is too starved to answer. \`guest.sh console $name\` is then the"
    note "only view left, and it does not need the guest to be healthy."
  fi
  return "$rc"
}

case "${1:-}" in
  console) shift; cmd_console "$@" ;;
  shell)   shift; cmd_shell "$@" ;;
  jump)    shift; cmd_jump "$@" ;;
  stop)    shift; cmd_stop ;;
  *)
    sed -n '/^# Usage:/,/^# Env:/p' "$0" | sed -e 's/^# \{0,1\}//' -e '$d' >&2
    exit 2
    ;;
esac
