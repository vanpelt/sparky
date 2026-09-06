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
#                             [--namespace parity] [--context <ctx>] [--helper]
#
# --pkg is what points it at a driver other than firecracker:
#
#   hack/parity/run-on-cks.sh --pkg ./internal/vmm/qemu --run TestQEMUParity
#
# --helper runs that same suite through the privileged helper -- the launcher a
# hardened node uses, where the VMM is execed by a root process on the far side
# of a Unix socket and confines itself. Without it the suite exercises the
# direct launcher, which is a dev box, not CKS:
#
#   hack/parity/run-on-cks.sh --pkg ./internal/vmm/qemu --run TestQEMUParity --helper
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
# --helper drives the suite through the PRIVILEGED HELPER instead of the direct
# launcher, which is the configuration a hardened node actually runs and the one
# nothing had ever booted a guest through. It changes four things at once, and
# they are a package rather than a menu:
#
#   - a `sparkbox-vmm-helper serve --backend qemu` runs in the Pod as root; it
#     builds the QEMU argv, creates the tap, and execs a QEMU that confines
#     ITSELF (-run-with chroot= -runas <uid>:<uid> -sandbox on);
#   - the test binary runs as an UNPRIVILEGED uid, because the helper refuses a
#     controller uid of 0 -- that refusal is the boundary's whole point;
#   - which means the driver cannot loop-mount a rootfs to inject an SSH key
#     (MEASURED: mount(8) will not set up a loop device for a non-root uid even
#     with ambient CAP_SYS_ADMIN and CAP_DAC_OVERRIDE), so the suite runs with
#     --disable-host-rootfs-mounts and this script bakes one key into the
#     template first -- exactly how a real sandbox gets its key;
#   - and the kernel is COPIED onto the scratch filesystem, because the helper
#     hardlinks it into each jail and chmods it 0444, neither of which works
#     against the read-only /nodedata mount it lives on.
helper=0
controller_uid=65532
controller_gid=65532

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
    --helper)    helper=1; shift ;;
    # Print the header block, whatever length it has grown to, rather than a
    # hardcoded line count that silently truncates the usage the day someone
    # documents a new flag. (It did.)
    -h|--help)   sed -n '2,/^set -uo/p' "$0" | sed '$d'; exit 0 ;;
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
if [ "$helper" = 1 ]; then
  [ "$pkg" = ./internal/vmm/qemu ] \
    || die "--helper is only wired for --pkg ./internal/vmm/qemu"
  echo "   plus cmd/sparkbox-vmm-helper"
  ( cd "$sparkbox" && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
      go build -o "$out/sparkbox-vmm-helper" ./cmd/sparkbox-vmm-helper ) \
    || die "go build sparkbox-vmm-helper failed"
fi

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
# 0755, not `+x`: the suite runs as the controller uid under --helper, and a
# umask that dropped the other-execute bit would fail at exec with a message
# about the file, not about the uid.
"${k[@]}" -n "$namespace" exec parity -- chmod 0755 /work/parity.test
if [ "$helper" = 1 ]; then
  "${k[@]}" cp "$out/sparkbox-vmm-helper" "$namespace/parity:/work/sparkbox-vmm-helper" \
    || die "kubectl cp of the helper failed"
  "${k[@]}" -n "$namespace" exec parity -- chmod 0755 /work/sparkbox-vmm-helper
fi

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
for t in mkfs.xfs e2fsck zerofree zstd ip setpriv ssh-keygen; do
  command -v "$t" >/dev/null 2>&1 || { echo "missing $t: wrong image?" >&2; exit 1; }
done
truncate -s 30G /work/scratch.xfs
mkfs.xfs -q -m reflink=1 /work/scratch.xfs
mkdir -p /work/scratch
mount -o loop /work/scratch.xfs /work/scratch
# jailer/ sits beside state/ ON THE SAME FILESYSTEM, and that is not tidiness.
# The helper hardlinks the rootfs, the kernel and each snapshot output from the
# VM directory into the per-slot jail, and link(2) is EXDEV across filesystems.
# Production gets this for free (both are under $hot_dir); here it has to be
# arranged. assets/ is the same story for the kernel -- see step 5b.
mkdir -p /work/scratch/images /work/scratch/state /work/scratch/jailer /work/scratch/assets

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

# QEMU only.
#
# THE MACHINE TYPE IS DELIBERATELY LEFT EMPTY unless an operator names one, and
# that is a correction rather than a simplification. This used to resolve the
# newest versioned pc-q35-* out of the binary, because the driver refused to
# default one on amd64 at all. The driver now has a pinned default and it is
# NOT a bare version -- it is `pc-q35-8.2,sata=off,vmport=off`, where the two
# suffixes remove ich9-ahci and the VMware backdoor ports (measured with
# `info qtree`). Resolving one here OVERRODE that, so a green run validated a
# machine model no production node will ever boot -- and a different device set
# is a different migration stream, which is the one thing a snapshot cannot
# survive.
#
# Empty means the driver and the helper each take the SAME pinned default from
# internal/vmm/qemuargs, which is the configuration actually being tested. Step
# 6c then proves the two processes agreed rather than assuming it.
qemu_bin=""
if [ "$pkg" != "./internal/vmm/firecracker" ]; then
  qemu_bin=$("${k[@]}" -n "$namespace" exec parity -- \
    bash -c 'command -v qemu-system-x86_64' 2>/dev/null || true)
  [ -n "$qemu_bin" ] || die "qemu-system-x86_64 not in the image ($image)"
  echo "   qemu=$qemu_bin machine-type=${machine_type:-(the driver pinned default)}"
fi

# The QEMU driver's own default (SPARKBOX_PARITY_QEMU_SUBNET), stated here
# because helper mode has to hand the SAME value to two processes: the driver
# derives each guest's ip= kernel argument from it and the helper addresses the
# tap from it. Disagree and the guest comes up with an address its gateway is
# not on, which presents as a boot that never answers SSH.
qemu_subnet=172.31.1.0/24

if [ "$helper" = 1 ]; then
  say "5b. baking the parity key into the template, and starting the helper"
  rm -f "$out/parity_key" "$out/parity_key.pub"
  ssh-keygen -q -t ed25519 -N '' -C parity -f "$out/parity_key" \
    || die "ssh-keygen failed"
  "${k[@]}" cp "$out/parity_key" "$namespace/parity:/work/parity_key" || die "kubectl cp of the key failed"
  "${k[@]}" cp "$out/parity_key.pub" "$namespace/parity:/work/parity_key.pub" || die "kubectl cp of the pubkey failed"
  "${k[@]}" -n "$namespace" exec -i parity -- env \
      LOGIN_USER="$login_user" KERNEL="$kernel" QEMU_BIN="$qemu_bin" \
      MACHINE_TYPE="$machine_type" SUBNET="$qemu_subnet" \
      CONTROLLER_UID="$controller_uid" CONTROLLER_GID="$controller_gid" \
      bash -s <<'HELPER' || die "helper preparation failed"
set -eu

# The key, into the TEMPLATE. This is how a real sandbox gets one -- the CKS
# deployment runs with --disable-host-rootfs-mounts and its templates carry a
# baked authorized_keys -- and here it is also the only way, because the test
# process is about to stop being root. Done before any VM exists, on our own
# copy of the image; /nodedata is mounted read-only and is never touched.
img=$(ls /work/scratch/images/*.ext4 | head -1)
mnt=$(mktemp -d)
mount -o loop "$img" "$mnt"
home=/root
[ "$LOGIN_USER" = root ] || home="/home/$LOGIN_USER"
[ -d "$mnt$home" ] || { echo "no $home in the template for login user $LOGIN_USER" >&2; umount "$mnt"; exit 1; }
mkdir -p "$mnt$home/.ssh"
cat /work/parity_key.pub >> "$mnt$home/.ssh/authorized_keys"
chmod 700 "$mnt$home/.ssh"; chmod 600 "$mnt$home/.ssh/authorized_keys"
# Ownership INSIDE the guest, which is not this container's idea of the user.
owner=$(stat -c '%u:%g' "$mnt$home")
chown -R "$owner" "$mnt$home/.ssh"
umount "$mnt"; rmdir "$mnt"
echo "baked $(wc -l < /work/parity_key.pub) key into $(basename "$img"):$home/.ssh/authorized_keys"

# The kernel has to live on the scratch filesystem. The helper hardlinks it into
# every jail and chmods it 0444; /nodedata is read-only (EROFS) and a different
# filesystem (EXDEV), so both fail there.
cp "$KERNEL" /work/scratch/assets/vmlinux

# Everything the unprivileged controller reads or writes. The jail base is left
# alone: the helper creates it 0711 root-owned, which is what makes a slot's
# jail traversable-but-not-listable to this group.
chown -R "$CONTROLLER_UID:$CONTROLLER_GID" /work/scratch/images /work/scratch/state /work/scratch/assets
chmod 0755 /work/scratch
# /work/vmm is deliberately NOT pre-created: RunServer makes its socket's
# directory itself and then chowns it root:<controller-gid> 0750, so anything
# arranged here would just be overwritten.
chown "$CONTROLLER_UID:$CONTROLLER_GID" /work/parity_key && chmod 0400 /work/parity_key

# --firecracker is deliberately not passed: a qemu backend does not validate it,
# which is the point of checking the binary by backend rather than both ways.
setsid /work/sparkbox-vmm-helper serve   --socket /work/vmm/helper.sock   --backend qemu   --qemu-bin "$QEMU_BIN"   --machine-type "$MACHINE_TYPE"   --kernel /work/scratch/assets/vmlinux   --vm-state-dir /work/scratch/state   --chroot-base /work/scratch/jailer   --subnet "$SUBNET"   --jailer-uid-base 100000   --controller-uid "$CONTROLLER_UID"   --controller-gid "$CONTROLLER_GID"   >/work/helper.log 2>&1 &

for _ in $(seq 1 50); do
  [ -S /work/vmm/helper.sock ] && break
  sleep 0.2
done
[ -S /work/vmm/helper.sock ] || { echo "helper never created its socket:" >&2; cat /work/helper.log >&2; exit 1; }
# Prove the boundary from the far side: the ping must succeed AS THE CONTROLLER
# UID, because that is the only uid the helper accepts. A root ping would pass
# here and tell us nothing about the run that follows.
setpriv --reuid "$CONTROLLER_UID" --regid "$CONTROLLER_GID" --clear-groups   /work/sparkbox-vmm-helper ping --socket /work/vmm/helper.sock   || { echo "helper refused the controller uid:" >&2; cat /work/helper.log >&2; exit 1; }
echo "helper up: $(head -1 /work/helper.log)"
HELPER
fi

run_log="$out/parity-cks-$(date +%Y%m%d-%H%M%S).log"

say "6. running the suite"
# Under --helper the suite runs as the CONTROLLER UID, not root, because the
# helper refuses uid 0 and because an unprivileged controller is the thing being
# tested. setpriv rather than su: no PAM, no login shell, no environment
# rewriting between here and the test binary.
runner=()
helper_env=()
if [ "$helper" = 1 ]; then
  runner=(setpriv --reuid "$controller_uid" --regid "$controller_gid" --clear-groups --)
  helper_env=(
    SPARKBOX_PARITY_HELPER_SOCKET=/work/vmm/helper.sock
    SPARKBOX_PARITY_HELPER_BIN=/work/sparkbox-vmm-helper
    SPARKBOX_PARITY_HELPER_CHROOT_BASE=/work/scratch/jailer
    SPARKBOX_PARITY_HELPER_GID="$controller_gid"
    SPARKBOX_PARITY_SSH_KEY_FILE=/work/parity_key
    # The kernel the HELPER was started with. Passing the /nodedata path here
    # would have the driver stat one file while the helper links another --
    # which would work right up until they differed.
    SPARKBOX_PARITY_KERNEL=/work/scratch/assets/vmlinux
  )
fi

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
  SPARKBOX_PARITY_QEMU_SUBNET="$qemu_subnet" \
  SPARKBOX_PARITY_MACHINE_TYPE="$machine_type" \
  ${helper_env[@]+"${helper_env[@]}"} \
  ${runner[@]+"${runner[@]}"} /work/parity.test -test.run "$runre" -test.v -test.timeout "$timeout" \
  2>&1 | tee "$run_log"
rc=${PIPESTATUS[0]}

if [ "$helper" = 1 ]; then
  say "6c. did the two processes describe the same machine?"
  # The driver decides what machine a sandbox BOOTS as and the helper decides
  # what machine its snapshot is RESTORED onto. They are separate processes
  # taking the same pinned default, and QEMU matches a migration stream
  # positionally against the machine the command line describes -- so a
  # disagreement here does not fail now, it fails on a resume, an hour later,
  # on a node where nothing about the sandbox has changed. Cheap to check once
  # per run; impossible to diagnose from the eventual symptom.
  helper_machine=$("${k[@]}" -n "$namespace" exec parity -- \
    sed -n 's/.*machine type //p' /work/helper.log | head -1 | tr -d '\r')
  driver_machine=$(sed -n 's/.* machine=\([^ ]*\) .*/\1/p' "$run_log" | head -1 | tr -d '\r')
  echo "   helper: ${helper_machine:-(none)}"
  echo "   driver: ${driver_machine:-(none)}"
  if [ -z "$helper_machine" ] || [ "$helper_machine" != "$driver_machine" ]; then
    echo "   MISMATCH: a sandbox paused by one of these cannot be resumed by the other" >&2
    rc=1
  else
    echo "   agreed"
  fi

  say "6b. the helper's log"
  # Always, not only on failure. The helper is where a launch is refused and
  # where a VMM's exit status is reported, and a green run's log is the baseline
  # that makes a red one readable.
  "${k[@]}" -n "$namespace" exec parity -- tail -40 /work/helper.log || true
fi

say "7. the neighbours"
# The point of the whole namespace/limits/hostPath dance is that this line is
# boring. Check it, every time.
"${k[@]}" -n sparkbox-poc get pods -o wide

exit "$rc"
