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
# Usage:
#   hack/parity/run-on-cks.sh [--run <regex>] [--keep] [--timeout 90m]
#                             [--namespace parity] [--context <ctx>]
set -uo pipefail

here=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
sparkbox=$(dirname "$(dirname "$here")")

namespace=parity
context=""
runre='TestFirecrackerParity'
timeout=90m
keep=0
nodedata=/var/lib/sparkbox
scratch_gb=40
login_user=""

while [ "$#" -gt 0 ]; do
  case "$1" in
    --run)       runre=${2:?--run needs a regex}; shift 2 ;;
    --keep)      keep=1; shift ;;
    --timeout)   timeout=${2:?--timeout needs a value}; shift 2 ;;
    --namespace) namespace=${2:?--namespace needs a value}; shift 2 ;;
    --context)   context=${2:?--context needs a value}; shift 2 ;;
    --node-data)  nodedata=${2:?--node-data needs a path}; shift 2 ;;
    --login-user) login_user=${2:?--login-user needs a name}; shift 2 ;;
    -h|--help)   sed -n '2,32p' "$0"; exit 0 ;;
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

say "1. building the linux/amd64 parity test binary"
( cd "$sparkbox" && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
    go test -c -o "$out/parity-amd64.test" ./internal/vmm/firecracker ) || die "go test -c failed"
ls -lh "$out/parity-amd64.test" | awk '{print "   " $5, $9}'

say "2. namespace/$namespace and the runner Pod"
"${k[@]}" get ns "$namespace" >/dev/null 2>&1 || "${k[@]}" create ns "$namespace" >/dev/null
"${k[@]}" -n "$namespace" apply -f - >/dev/null <<YAML
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
      # Stock ubuntu, and step 5 installs what the suite shells out to
      # (e2fsprogs, zerofree, zstd, iproute2, xfsprogs). Deliberately NOT the
      # sparkbox node image: this Pod should not be able to be confused for part
      # of the deployment, in a `kubectl get pods -A` or by anything else.
      image: ubuntu:24.04
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
apt-get update -qq
apt-get install -y -qq xfsprogs e2fsprogs zerofree zstd iproute2 iputils-ping >/dev/null
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
PREP
[ $? -eq 0 ] || die "scratch preparation failed"

image_name=$("${k[@]}" -n "$namespace" exec parity -- \
  bash -c 'basename "$(ls /work/scratch/images/*.ext4 | head -1)" .ext4')
if [ -z "$login_user" ]; then
  login_user=$("${k[@]}" -n "$namespace" exec parity -- \
    bash -c 'cat /nodedata/images/*.login-user 2>/dev/null | head -1' || true)
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
  /work/parity.test -test.run "$runre" -test.v -test.timeout "$timeout" \
  2>&1 | tee "$out/parity-cks-$(date +%Y%m%d-%H%M%S).log"
rc=${PIPESTATUS[0]}

say "7. the neighbours"
# The point of the whole namespace/limits/hostPath dance is that this line is
# boring. Check it, every time.
"${k[@]}" -n sparkbox-poc get pods -o wide

exit "$rc"
