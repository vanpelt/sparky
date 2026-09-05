#!/usr/bin/env bash
# The VMM parity suite on the CKS node, x86_64, in a throwaway Pod.
#
# Same discipline as hack/m0b/run-on-cks.sh, and for the same reason: this is a
# production cluster with one node, and the sandboxes on it belong to people.
#
#   - its own namespace, so no selector in sparkbox-poc can reach it;
#   - hostPath /dev/kvm and `privileged: true`, because the device plugin CANNOT
#     help — sparkbox.dev/kvm is allocatable 1 and held by vmm-helper, and
#     Kubernetes has no narrower way to pass a device through without one;
#   - CPU, memory and ephemeral-storage all capped, so nothing here can pressure
#     the sandboxes sharing the node;
#   - deleted on exit, including on failure and Ctrl-C.
#
# WHAT IT READS FROM THE NODE, AND WHY THAT IS NOT A WRITE
#
# /var/lib/sparkbox is mounted READ-ONLY, for the guest kernel and the rootfs
# template only. The suite should boot the artifact the fleet boots; building a
# fixture for the occasion would test the fixture. Nothing under that path is
# written, and no device-plugin allocation is taken.
#
# WHY THERE IS A LOOPBACK XFS IN HERE
#
# The driver reflink-clones the template and REFUSES to fall back to a full
# 25 GiB copy — an incompatible mount is a configuration error, not a reason for
# sandbox creation to become unexpectedly huge. So VMStateDir has to be on a
# reflink-capable filesystem, and it must not be the node's. The Pod therefore
# makes its own: an XFS image inside its emptyDir, mounted loopback. The template
# is copied into it ONCE (sparse, so it costs its used size and not 25 GiB), and
# every VM the suite creates is a reflink clone within that image.
#
# WHERE THE TOOLCHAIN COMES FROM
#
# ghcr.io/<owner>/sparkbox-parity, built by .github/workflows/sparkbox-parity-image.yml
# on a native runner per architecture. It used to be `ubuntu:24.04` plus an
# `apt-get install` inside the Pod, which made every run depend on the cluster
# having egress to an Ubuntu mirror and on that mirror serving the same versions
# twice. The test BINARY is still copied in per run, so iterating on the suite
# costs a `go test -c` rather than a CI round trip.
#
# Usage:
#   hack/parity/run-on-cks.sh [--run <regex>] [--pkg <import path>] [--image <ref>]
#                             [--keep] [--timeout 90m] [--machine-type <name>]
#                             [--namespace parity] [--context <ctx>]
#
# --pkg is what points it at a driver other than firecracker:
#
#   hack/parity/run-on-cks.sh --pkg ./internal/vmm/qemu --run TestQEMUParity
set -uo pipefail

here=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
sparkbox=$(dirname "$(dirname "$here")")

namespace=parity
context=""
runre='TestFirecrackerParity'
pkg=./internal/vmm/firecracker
# Pinned by branch, not `edge`: a parity result should name the toolchain that
# produced it, and the branch tag is what CI just pushed for this checkout.
image=""
machine_type=""
timeout=90m
# Lower than the suite's own 180s default. This node boots a guest in seconds;
# a three-minute ceiling here only decides how long a MISCONFIGURED run takes to
# tell you, and at nineteen cases that is the difference between four minutes
# and an hour.
boot_timeout_s=60
keep=0
nodedata=
scratch_gb=40
login_user=""

while [ "$#" -gt 0 ]; do
  case "$1" in
    --run)       runre=${2:?--run needs a regex}; shift 2 ;;
    --pkg)       pkg=${2:?--pkg needs an import path}; shift 2 ;;
    --image)     image=${2:?--image needs a ref}; shift 2 ;;
    --machine-type) machine_type=${2:?--machine-type needs a name}; shift 2 ;;
    --keep)      keep=1; shift ;;
    --timeout)   timeout=${2:?--timeout needs a value}; shift 2 ;;
    --boot-timeout) boot_timeout_s=${2:?--boot-timeout needs seconds}; shift 2 ;;
    --namespace) namespace=${2:?--namespace needs a value}; shift 2 ;;
    --context)   context=${2:?--context needs a value}; shift 2 ;;
    --node-data)  nodedata=${2:?--node-data needs a path}; shift 2 ;;
    --login-user) login_user=${2:?--login-user needs a name}; shift 2 ;;
    -h|--help)   sed -n '2,55p' "$0"; exit 0 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

k=(kubectl)
[ -n "$context" ] && k+=(--context "$context")
say() { printf '\n\033[1m%s\033[0m\n' "$1"; }
die() { echo "error: $*" >&2; exit 1; }

command -v kubectl >/dev/null 2>&1 || die "kubectl not installed"
"${k[@]}" get --raw /api >/dev/null 2>&1 \
  || die "cannot reach the API server — a bare 403 here is an expired CoreWeave token, not RBAC"

cleanup() {
  if [ "$keep" = 1 ]; then
    say "leaving namespace/$namespace up (--keep). Delete with: kubectl delete ns $namespace"
    return
  fi
  say "cleaning up namespace/$namespace"
  "${k[@]}" delete ns "$namespace" --wait=false >/dev/null 2>&1
}
trap cleanup EXIT INT TERM

out="$sparkbox/.dev/parity"
mkdir -p "$out"

# Default the image to this checkout's branch tag. Slashes are legal in a branch
# name and not in a tag, which is the same substitution the workflow makes.
if [ -z "$image" ]; then
  branch=$(cd "$sparkbox" && git rev-parse --abbrev-ref HEAD 2>/dev/null | tr '/' '-')
  owner=$(cd "$sparkbox" && git remote get-url origin 2>/dev/null \
            | sed -E 's#.*[:/]([^/]+)/[^/]+(\.git)?$#\1#')
  image="ghcr.io/${owner:-vanpelt}/sparkbox-parity:${branch:-edge}"
fi

# Where the node's assets live ON THE HOST. Not /var/lib/sparkbox: that is the
# path the deployment mounts them AT, inside its containers. The host path is a
# hostPath volume on the sparkbox-node Deployment (/mnt/local/sparkbox-poc as of
# this writing), so read it from there rather than hardcoding a value that goes
# stale the first time the namespace or the mount moves. Getting this wrong does
# not fail loudly -- the Pod simply never becomes Ready, with the reason three
# screens deep in `describe`.
if [ -z "$nodedata" ]; then
  nodedata=$("${k[@]}" -n sparkbox-poc get deploy sparkbox-node -o json 2>/dev/null \
    | python3 -c '
import json,sys
try: sp = json.load(sys.stdin)["spec"]["template"]["spec"]
except Exception: sys.exit(0)
want = {m["name"] for c in sp["containers"] for m in c.get("volumeMounts", [])
        if m.get("mountPath") == "/var/lib/sparkbox"}
for v in sp.get("volumes", []):
    if v["name"] in want and "hostPath" in v:
        print(v["hostPath"]["path"]); break
' || true)
  [ -n "$nodedata" ] || die "could not read the node data hostPath from deploy/sparkbox-node; pass --node-data"
  echo "node data on the host: $nodedata"
fi

say "1. building the linux/amd64 parity test binary"
echo "   package: $pkg"
( cd "$sparkbox" && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
    go test -c -o "$out/parity-amd64.test" "$pkg" ) || die "go test -c failed"
ls -lh "$out/parity-amd64.test" | awk '{print "   " $5, $9}'

say "2. namespace/$namespace and the runner Pod"
# A leftover namespace is almost always a previous --keep run, and reusing its
# Pod does not work: `apply` reconfigures it in place, the emptyDir still holds
# the old scratch.xfs, and step 5 dies on "appears to contain an existing
# filesystem". Delete and WAIT -- an async delete loses the same race a beat
# later, against a Pod that is half gone.
if "${k[@]}" get ns "$namespace" >/dev/null 2>&1; then
  say "removing the existing namespace/$namespace (a previous --keep?)"
  "${k[@]}" delete ns "$namespace" --wait=true --timeout=120s >/dev/null \
    || die "could not delete the existing namespace/$namespace"
fi
"${k[@]}" create ns "$namespace" >/dev/null
"${k[@]}" -n "$namespace" apply -f - <<YAML || die "pod manifest rejected (see the error above)"
apiVersion: v1
kind: Pod
metadata:
  name: parity
  labels: { app: vmm-parity }
spec:
  restartPolicy: Never
  terminationGracePeriodSeconds: 5
  containers:
    - name: runner
      # The parity toolchain image (hack/parity/Dockerfile): QEMU plus the
      # filesystem tools the driver shells out to, and nothing else.
      #
      # Deliberately NOT the sparkbox node image. This Pod should not be
      # confusable with part of the deployment by someone reading
      # 'kubectl get pods -A' on a live cluster -- and note the quoting there:
      # this heredoc is unquoted, so a backtick would run a command and paste
      # its output into the manifest. That is not hypothetical; it is the bug
      # that made this script fail the first time it was ever run.
      image: ${image}
      command: ["sleep", "infinity"]
      securityContext:
        privileged: true          # the only way to pass /dev/kvm without a device plugin
      resources:
        requests: { cpu: "2", memory: "4Gi",  ephemeral-storage: "20Gi" }
        limits:   { cpu: "8", memory: "12Gi", ephemeral-storage: "${scratch_gb}Gi" }
      volumeMounts:
        - { name: kvm,      mountPath: /dev/kvm }
        - { name: nodedata, mountPath: /nodedata, readOnly: true }
        - { name: work,     mountPath: /work }
  volumes:
    - name: kvm
      hostPath: { path: /dev/kvm, type: CharDevice }
    - name: nodedata
      hostPath: { path: ${nodedata}, type: Directory }
    - name: work
      emptyDir: {}
YAML

say "3. waiting for the Pod"
if ! "${k[@]}" -n "$namespace" wait --for=condition=Ready pod/parity --timeout=180s; then
  "${k[@]}" -n "$namespace" describe pod parity | tail -25
  exit 1
fi
"${k[@]}" -n "$namespace" get pod parity -o wide

say "4. copying the test binary in"
"${k[@]}" cp "$out/parity-amd64.test" "$namespace/parity:/work/parity.test" || die "kubectl cp failed"
"${k[@]}" -n "$namespace" exec parity -- chmod +x /work/parity.test

say "5. preparing a reflink-capable scratch filesystem"
# `-i` is mandatory: without it kubectl closes stdin, bash reads EOF, and the
# exec exits 0 having run nothing — a green step that did nothing.
"${k[@]}" -n "$namespace" exec -i parity -- bash -s <<'PREP'
set -eu
export DEBIAN_FRONTEND=noninteractive
# No apt-get: xfsprogs, e2fsprogs, zerofree, zstd and iproute2 are baked into
# the image by hack/parity/Dockerfile, so a run does not depend on the cluster
# reaching an Ubuntu mirror. Fail here rather than three minutes later if the
# image is somehow the wrong one.
for t in mkfs.xfs e2fsck zerofree zstd ip; do
  command -v "$t" >/dev/null 2>&1 || { echo "missing $t: wrong image?" >&2; exit 1; }
done
truncate -s 30G /work/scratch.xfs
mkfs.xfs -q -m reflink=1 /work/scratch.xfs
mkdir -p /work/scratch
mount -o loop /work/scratch.xfs /work/scratch
mkdir -p /work/scratch/images /work/scratch/state

# One sparse copy in, then every VM is a reflink clone of it. --sparse=always so
# the 25 GiB ceiling costs its used size and not the ceiling.
img=$(ls /nodedata/images/*.ext4 2>/dev/null | head -1)
[ -n "$img" ] || { echo "no rootfs template under /nodedata/images" >&2; exit 1; }
cp --sparse=always "$img" "/work/scratch/images/$(basename "$img")"
cp --reflink=always "/work/scratch/images/$(basename "$img")" /work/scratch/state/.probe
rm -f /work/scratch/state/.probe
echo "template: $(basename "$img")  scratch: $(df -h /work/scratch | tail -1)"
# The Mac runner learned this the hard way: a machine was resized between two
# runs and the two logs were compared as if they came from the same box, which
# moved Firecracker's total by 40%. A timing is comparable to another timing
# only if this line matches. See docs/vmm-parity-harness.md.
echo "pod sees: $(nproc) CPUs, $(awk '/MemTotal/ {printf "%.1f GiB", $2/1048576}' /proc/meminfo)"
# nproc reports the HOST's 64 CPUs regardless of this Pod's cgroup cap -- the
# trap hack/m0b hit when it used nproc for -j. cpu.max is the real limit.
echo "cgroup cpu.max: $(cat /sys/fs/cgroup/cpu.max 2>/dev/null || echo unknown)"
PREP
[ $? -eq 0 ] || die "scratch preparation failed"

image_name=$("${k[@]}" -n "$namespace" exec parity -- \
  bash -c 'basename "$(ls /work/scratch/images/*.ext4 | head -1)" .ext4')
if [ -z "$login_user" ]; then
  # The sidecar sits next to the TEMPLATE, not next to the base image: on a live
  # node /nodedata/images holds universal.ext4 and a .ready marker and nothing
  # else, while /nodedata/templates carries a .login-user beside each snapshot.
  # Look in both. Getting this wrong is expensive and silent -- the driver falls
  # back to root, every SSH handshake fails as the wrong user, and each case
  # burns the FULL boot timeout before failing. That is what the first real run
  # of this script did: nineteen cases, exactly three minutes apart.
  login_user=$("${k[@]}" -n "$namespace" exec parity -- bash -c \
    'cat /nodedata/images/*.login-user /nodedata/templates/*.login-user 2>/dev/null | head -1' || true)
fi
# root is the driver's own default, and it is the wrong guess for our images
# (hack/images/Dockerfile labels them sparky). Getting it wrong fails at the SSH
# handshake, a long way from here, so say which one is in play.
[ -n "$login_user" ] || login_user=root
kernel=$("${k[@]}" -n "$namespace" exec parity -- \
  bash -c 'ls /nodedata/assets/vmlinux 2>/dev/null || ls /nodedata/vmlinux 2>/dev/null' || true)
[ -n "$kernel" ] || die "no guest kernel found under $nodedata"
fcbin=$("${k[@]}" -n "$namespace" exec parity -- \
  bash -c 'ls /nodedata/assets/firecracker 2>/dev/null || command -v firecracker' || true)
[ -n "$fcbin" ] || die "no firecracker binary found under $nodedata"
echo "   image=$image_name login=$login_user kernel=$kernel firecracker=$fcbin"

# QEMU only. The driver REFUSES to default a machine type off arm64
# (internal/vmm/qemu/qemu.go), and it insists on a VERSIONED name, because the
# machine model is baked into every migration stream a sandbox pauses into: an
# unversioned `q35` silently means "whatever this QEMU calls newest", so a QEMU
# upgrade would strand every existing snapshot. Ask the binary what it has
# rather than hardcoding a version that expires.
qemu_bin=""
if [ "$pkg" != "./internal/vmm/firecracker" ]; then
  qemu_bin=$("${k[@]}" -n "$namespace" exec parity -- \
    bash -c 'command -v qemu-system-x86_64' 2>/dev/null || true)
  [ -n "$qemu_bin" ] || die "qemu-system-x86_64 not in the image ($image)"
  if [ -z "$machine_type" ]; then
    machine_type=$("${k[@]}" -n "$namespace" exec parity -- \
      bash -c "$qemu_bin -M help | awk '{print \$1}' | grep -E '^pc-q35-[0-9]+\\.[0-9]+$' | sort -V | tail -1")
  fi
  [ -n "$machine_type" ] || die "could not resolve a versioned pc-q35-* machine type"
  echo "   qemu=$qemu_bin machine-type=$machine_type"
fi

say "6. running the suite"
"${k[@]}" -n "$namespace" exec parity -- env \
  SPARKBOX_VMM_PARITY=1 \
  SPARKBOX_PARITY_KERNEL="$kernel" \
  SPARKBOX_PARITY_IMAGE_DIR=/work/scratch/images \
  SPARKBOX_PARITY_IMAGE="$image_name" \
  SPARKBOX_PARITY_STATE_DIR=/work/scratch/state \
  SPARKBOX_PARITY_FIRECRACKER="$fcbin" \
  SPARKBOX_PARITY_LOGIN_USER="$login_user" \
  SPARKBOX_PARITY_SUBNET=172.31.0.0/24 \
  SPARKBOX_PARITY_VCPUS=2 \
  SPARKBOX_PARITY_MEM_MB=2048 \
  SPARKBOX_PARITY_BOOT_TIMEOUT_S="$boot_timeout_s" \
  SPARKBOX_PARITY_QEMU="$qemu_bin" \
  SPARKBOX_PARITY_MACHINE_TYPE="$machine_type" \
  /work/parity.test -test.run "$runre" -test.v -test.timeout "$timeout" \
  2>&1 | tee "$out/parity-cks-$(date +%Y%m%d-%H%M%S).log"
rc=${PIPESTATUS[0]}

say "7. the neighbours"
# The point of the whole namespace/limits/hostPath dance is that this line is
# boring. Check it, every time.
"${k[@]}" -n sparkbox-poc get pods -o wide

exit "$rc"
