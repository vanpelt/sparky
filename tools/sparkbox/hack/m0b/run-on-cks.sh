#!/usr/bin/env bash
# M0b on the CKS node, in a throwaway Pod that touches nothing the sparkbox
# deployment owns. See docs/cloud-hypervisor-feasibility.md section 9 (M0b).
#
# Why a Pod and not the existing node Pod: the experiment needs /dev/kvm, and
# `sparkbox.dev/kvm` is allocatable 1 and already held by vmm-helper, so a
# second Pod cannot get it from the device plugin. It mounts the device by
# hostPath instead, which needs `privileged: true` -- Kubernetes has no narrower
# way to pass a device through without a device plugin.
#
# What it does NOT do: read or write anything under /var/lib/sparkbox, touch any
# existing object, or take a device-plugin allocation. It runs in its own
# namespace so no selector in sparkbox-poc can reach it, and both CPU and memory
# are capped so a runaway build cannot pressure the running sandboxes. There is
# only one node in this cluster, so the Pod necessarily lands beside them.
#
# Usage:
#   hack/m0b/run-on-cks.sh [--keep] [--namespace nested-m0b] [--context <ctx>]
#
#   --keep  leave the namespace up afterwards for poking at it by hand.
#           Otherwise it is deleted on exit, including on failure or Ctrl-C.
set -uo pipefail

here=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
hackdir=$(dirname "$here")

namespace=nested-m0b
context=""
keep=0
while [ "$#" -gt 0 ]; do
  case "$1" in
    --keep)      keep=1; shift ;;
    --namespace) namespace=${2:?--namespace needs a value}; shift 2 ;;
    --context)   context=${2:?--context needs a value}; shift 2 ;;
    -h|--help)   sed -n '2,28p' "$0"; exit 0 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

k=(kubectl)
[ -n "$context" ] && k+=(--context "$context")
say() { printf '\n\033[1m%s\033[0m\n' "$1"; }

command -v kubectl >/dev/null 2>&1 || { echo "kubectl not installed" >&2; exit 2; }
if ! "${k[@]}" get --raw /api >/dev/null 2>&1; then
  echo "cannot reach the API server -- a bare 403 here is an expired CoreWeave token, not RBAC" >&2
  exit 1
fi

cleanup() {
  if [ "$keep" = 1 ]; then
    say "leaving namespace/$namespace up (--keep). Delete with: kubectl delete ns $namespace"
    return
  fi
  say "cleaning up namespace/$namespace"
  "${k[@]}" delete ns "$namespace" --wait=false >/dev/null 2>&1
}
trap cleanup EXIT INT TERM

say "1. namespace/$namespace and the build Pod"
"${k[@]}" get ns "$namespace" >/dev/null 2>&1 || "${k[@]}" create ns "$namespace" >/dev/null

# Requests are deliberately modest against a node that is already 83% requested;
# limits are what actually protect the neighbours. Memory is the dangerous one
# -- an OOM inside this cap kills only this Pod -- so it is capped hard, and
# ephemeral-storage is capped too because a kernel tree is ~15GB and the node's
# disk is shared with every sandbox rootfs.
"${k[@]}" -n "$namespace" apply -f - >/dev/null <<'YAML'
apiVersion: v1
kind: Pod
metadata:
  name: m0b
  labels: { app: m0b-nested-probe }
spec:
  restartPolicy: Never
  terminationGracePeriodSeconds: 5
  containers:
    - name: build
      image: ubuntu:24.04
      command: ["sleep", "infinity"]
      securityContext:
        privileged: true            # the only way to pass /dev/kvm without a device plugin
      resources:
        requests: { cpu: "4",  memory: "8Gi",  ephemeral-storage: "20Gi" }
        limits:   { cpu: "16", memory: "16Gi", ephemeral-storage: "40Gi" }
      volumeMounts:
        - { name: kvm,  mountPath: /dev/kvm }
        - { name: work, mountPath: /work }
  volumes:
    - name: kvm
      hostPath: { path: /dev/kvm, type: CharDevice }
    - name: work
      emptyDir: {}
YAML

say "2. waiting for the Pod"
if ! "${k[@]}" -n "$namespace" wait --for=condition=Ready pod/m0b --timeout=180s; then
  "${k[@]}" -n "$namespace" describe pod m0b | tail -25
  exit 1
fi
"${k[@]}" -n "$namespace" get pod m0b -o wide

say "3. copying hack/ into the Pod"
# build-kernel.sh, kernel-config.fragment and m0b/ all travel together; the
# experiment must run the same builder the fleet does.
"${k[@]}" -n "$namespace" exec m0b -- mkdir -p /work/hack
"${k[@]}" cp "$hackdir" "$namespace/m0b:/work/" || { echo "kubectl cp failed" >&2; exit 1; }

say "4. running the experiment (kernel build is the slow part)"
"${k[@]}" -n "$namespace" exec m0b -- bash /work/hack/m0b/inside-pod.sh 2>&1 | tee /tmp/m0b-run.log
rc=${PIPESTATUS[0]}

say "5. result"
grep -E '^M0B_|^VERDICT' /tmp/m0b-run.log || echo "no markers -- see the full log above"
echo
echo "full log: /tmp/m0b-run.log"
exit "$rc"
