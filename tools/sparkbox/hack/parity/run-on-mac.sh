#!/usr/bin/env bash
# The VMM parity suite on the Mac dev box's Apple container machine.
#
# The suite needs /dev/kvm, a guest kernel, a rootfs template and a
# reflink-capable filesystem. On a Mac all four live one level down, inside the
# Linux VM `container machine` boots — the same machine hack/dev/ uses — so this
# script builds a linux/arm64 test binary here, gets it in there, and runs it in
# a THROWAWAY container beside the dev pod rather than inside it.
#
# Beside, not inside, for three reasons:
#   - its own network namespace, so the tap devices the driver creates (sbtap0,
#     sbtap1, ...) cannot collide with the node's;
#   - its own VMStateDir, so nothing it does is visible to a sandbox somebody is
#     working in;
#   - its own driver Options, so it runs the direct launcher and lets the driver
#     do its own rootfs key injection — which the node, running the privileged
#     helper with host rootfs mounts disabled, never exercises.
#
# It reads the dev pod's assets and image template read-only. That is the point:
# a parity run should boot the artifact the fleet boots, not a fixture built for
# the occasion.
#
# Usage:
#   hack/parity/run-on-mac.sh [--run <regex>] [--keep] [--timeout 60m]
#                             [--machine sparkbox]
set -uo pipefail

here=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
sparkbox=$(dirname "$(dirname "$here")")

machine=sparkbox
runre='TestFirecrackerParity'
timeout=60m
keep=0
# 1 GiB, not the 2 GiB a real sandbox gets. The suite only needs sshd, and this
# machine has a measured first-touch memory cliff above ~1 GiB per guest
# (docs/... sparkbox-devbox-thp-memory-faults): with the dev pod holding
# sandboxes of its own on a 12 GiB VM, 2 GiB guests took 10-30x longer to boot
# and the later cases timed out. Halving it is the difference between a 4-minute
# run and one that fails on the clock.
mem_mb=1024
boot_timeout_s=240
base_ref=127.0.0.1:5001/sparkbox-cks:dev
push_ref=127.0.0.1:5001/sparkbox-parity:dev
pull_ref=192.168.64.1:5001/sparkbox-parity:dev
cname=sparkbox-parity

while [ "$#" -gt 0 ]; do
  case "$1" in
    --run)      runre=${2:?--run needs a regex}; shift 2 ;;
    --keep)     keep=1; shift ;;
    --timeout)  timeout=${2:?--timeout needs a value}; shift 2 ;;
    --machine)  machine=${2:?--machine needs a name}; shift 2 ;;
    --mem)      mem_mb=${2:?--mem needs MiB}; shift 2 ;;
    --boot-timeout) boot_timeout_s=${2:?--boot-timeout needs seconds}; shift 2 ;;
    --base)     base_ref=${2:?--base needs a ref}; shift 2 ;;
    -h|--help)  sed -n '2,26p' "$0"; exit 0 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

say() { printf '\n\033[1m%s\033[0m\n' "$1"; }
die() { echo "error: $*" >&2; exit 1; }

command -v container >/dev/null 2>&1 || die "Apple 'container' CLI not installed"
command -v docker    >/dev/null 2>&1 || die "docker (OrbStack or Desktop) not installed"
container machine ls 2>/dev/null | grep -E "^$machine[[:space:]]" | grep -q running \
  || die "container machine '$machine' is not running (hack/dev/machine.sh status)"

# `-i` is mandatory: without it stdin is discarded, bash reads EOF, and the call
# exits 0 having run nothing. The trailing EXIT line is what decides, so a
# transport that swallows a status cannot turn a red run green.
inmachine() { container machine run -i --root --name "$machine" -- bash -s; }
run_guest() {
  local outp status
  outp=$(inmachine) || true
  printf '%s\n' "$outp" | sed '$d'
  status=$(printf '%s\n' "$outp" | tail -1 | sed -n 's/^EXIT //p')
  [ -n "$status" ] || { echo "transport produced no EXIT line" >&2; return 1; }
  return "$status"
}

out="$sparkbox/.dev/parity"
mkdir -p "$out"

# --- 1. build the test binary -------------------------------------------------
# `go test -c` so the container needs no Go toolchain and no source tree: the
# binary is the whole payload.
say "1. building the linux/arm64 parity test binary"
( cd "$sparkbox" && GOOS=linux GOARCH=arm64 CGO_ENABLED=0 \
    go test -c -o "$out/parity.test" ./internal/vmm/firecracker ) || die "go test -c failed"
ls -lh "$out/parity.test" | awk '{print "   " $5, $9}'

# --- 2. ship it through the registry ------------------------------------------
# NOT over HTTP from a listener on the Mac: the machine connects, the Mac's
# firewall drops it after the handshake, and curl reports "Empty reply from
# server" — measured. The registry path is the one hack/dev/image.sh already
# proves works in this direction, so the binary rides a layer.
say "2. baking the binary into an image and pushing it to the local registry"
"$sparkbox/hack/dev/registry.sh" up >/dev/null || die "local registry not available"
docker pull -q "$base_ref" >/dev/null 2>&1 || true
docker build -q -t "$push_ref" -f - "$out" >/dev/null <<DOCKERFILE || die "docker build failed"
FROM $base_ref
COPY parity.test /parity.test
DOCKERFILE
docker push -q "$push_ref" >/dev/null || die "docker push failed"

# What else is on this machine decides how long a guest takes to come up here,
# so print it rather than leaving a slow run looking like a driver bug.
say "3. pulling it into the machine, and what else is running on it"
run_guest <<GUEST
set -eu
docker pull -q $pull_ref >/dev/null
rm -rf /srv/sparkbox/data/parity
mkdir -p /srv/sparkbox/data/parity/scratch
# Reflink is not optional: the driver refuses to fall back to a full 25 GiB copy,
# so a scratch dir on the wrong filesystem fails Create rather than being slow.
cp --reflink=always /srv/sparkbox/data/devpod/images/universal.ext4 \
   /srv/sparkbox/data/parity/scratch/.reflink-probe
rm -f /srv/sparkbox/data/parity/scratch/.reflink-probe
echo "reflink on /srv/sparkbox/data: ok"
free -m | sed -n '1,2p'
n=$(pgrep -c firecracker || true)
echo "firecracker processes already running on this machine: ${n:-0} (the dev pod's sandboxes)"
echo "EXIT 0"
GUEST
[ $? -eq 0 ] || die "staging failed"

# --- 4. run it ----------------------------------------------------------------
# Detached, then polled: a docker run streamed down the stdin transport for forty
# minutes is one dropped connection away from losing the run, and a container
# that outlives the transport is the recovery path.
say "4. running the suite (detached; polling for output)"
run_guest <<GUEST
set -eu
docker rm -f $cname >/dev/null 2>&1 || true
docker run -d --name $cname \
  --privileged \
  --device /dev/kvm --device /dev/net/tun \
  -v /srv/sparkbox/data/parity:/parity \
  -v /srv/sparkbox/data/devpod/images:/images:ro \
  -v /srv/sparkbox/data/devpod/assets:/assets:ro \
  -e SPARKBOX_VMM_PARITY=1 \
  -e SPARKBOX_PARITY_KERNEL=/assets/vmlinux \
  -e SPARKBOX_PARITY_IMAGE_DIR=/images \
  -e SPARKBOX_PARITY_IMAGE=universal \
  -e SPARKBOX_PARITY_STATE_DIR=/parity/scratch \
  -e SPARKBOX_PARITY_FIRECRACKER=/assets/firecracker \
  -e SPARKBOX_PARITY_LOGIN_USER=sparky \
  -e SPARKBOX_PARITY_SUBNET=172.31.0.0/24 \
  -e SPARKBOX_PARITY_VCPUS=2 \
  -e SPARKBOX_PARITY_MEM_MB=$mem_mb \
  -e SPARKBOX_PARITY_BOOT_TIMEOUT_S=$boot_timeout_s \
  --entrypoint /parity.test \
  $pull_ref \
  -test.run '$runre' -test.v -test.timeout $timeout >/dev/null
echo "EXIT 0"
GUEST
[ $? -eq 0 ] || die "could not start the parity container"

log="$out/parity-$(date +%Y%m%d-%H%M%S).log"
printed=0
body=""
while :; do
  snapshot=$(printf 'docker logs %s 2>&1; echo "STATE $(docker inspect -f "{{.State.Running}}:{{.State.ExitCode}}" %s)"\n' \
               "$cname" "$cname" | inmachine)
  state=$(printf '%s\n' "$snapshot" | tail -1)
  body=$(printf '%s\n' "$snapshot" | sed '$d')
  total=$(printf '%s\n' "$body" | wc -l | tr -d ' ')
  if [ "$total" -gt "$printed" ]; then
    printf '%s\n' "$body" | tail -n +$((printed + 1))
    printed=$total
  fi
  case "$state" in
    "STATE false:"*) break ;;
    "STATE true:"*)  ;;
    *) echo "unexpected state line: $state" >&2 ;;
  esac
  sleep 10
done
printf '%s\n' "$body" >"$log"
rc=${state##*:}

say "5. result"
grep -E '^(ok|FAIL|PASS|--- (PASS|FAIL|SKIP))' "$log" | tail -40
echo
echo "full log: $log"

if [ "$keep" = 1 ]; then
  echo "leaving container/$cname and /srv/sparkbox/data/parity in place (--keep)"
else
  printf 'docker rm -f %s >/dev/null 2>&1; rm -rf /srv/sparkbox/data/parity; echo "EXIT 0"\n' \
    "$cname" | inmachine >/dev/null
fi
exit "${rc:-1}"
