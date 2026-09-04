#!/usr/bin/env bash
# M0 of the Cloud Hypervisor spike: answer, against the LIVE CKS deployment,
# every question that decides whether nested virtualization is available to us
# at all. See docs/cloud-hypervisor-feasibility.md §9 M0.
#
# Read-only. It runs `kubectl get`/`exec` and an optional `ssh` into one sandbox.
# It creates nothing, changes nothing, and restarts nothing. The one thing it
# executes remotely is hack/probe-nested-virt.sh, piped in over stdin — nothing
# is written to the node.
#
# Three questions, in the order they matter:
#
#   A. Can this Node host nested guests at all? CPU flags, /dev/kvm, the KVM
#      module's nested and ept/npt parameters, Landlock, and the three 2026
#      shadow-MMU escapes (CVE-2026-53359, CVE-2026-64561, CVE-2026-80726).
#      Delegated to hack/probe-nested-virt.sh, run inside the vmm-helper
#      container so the answer describes the process that would exec the VMM.
#
#   B. Does a Firecracker guest ALREADY see the VMX/SVM bit today? The spike
#      argues from source that it does — Firecracker's CPUID normaliser never
#      clears the bit and Sparkbox sets no CPU template — but that is an
#      inference, and this settles it by reading /proc/cpuinfo in a real
#      sandbox. No kernel rebuild is needed: X86_FEATURE_VMX is CPUID.1:ECX[5],
#      printed by the kernel's generic cpuinfo flag loop, entirely independent
#      of CONFIG_KVM (which our guest kernel lacks). Seeing the flag does NOT
#      mean a guest can run an inner VM — that still needs CONFIG_KVM — it means
#      our current "guests see no VMX" posture is a property of CoreWeave's
#      module parameters rather than of Sparkbox, and that a masking CPU
#      template is worth pinning whatever we decide about the port.
#
#   C. What is the fleet's shape for the decisions that follow? Node kernel and
#      OS, CPU vendor (Intel and AMD differ in both the CVE and the
#      Cloud-Hypervisor-version story), and whether a second node pool exists
#      to isolate nested-enabled sandboxes onto.
#
# Usage:
#   hack/probe-cks-nested.sh [--sandbox <name>] [--domain catnip.sh]
#                            [--namespace sparkbox-poc] [--context <ctx>]
#
#   --sandbox  a live sandbox to read /proc/cpuinfo from, for question B.
#              Omitted, B is skipped and the command to run is printed instead.
#
# Exit 0 when every question was answered and the Node may admit nested
# sandboxes; 1 when a hard blocker was found; 2 when something could not be
# determined.
set -uo pipefail

here=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
probe="$here/probe-nested-virt.sh"

namespace=sparkbox-poc
context=""
sandbox=""
domain=catnip.sh
while [ "$#" -gt 0 ]; do
  case "$1" in
    --sandbox)   sandbox=${2:?--sandbox needs a value}; shift 2 ;;
    --domain)    domain=${2:?--domain needs a value}; shift 2 ;;
    --namespace) namespace=${2:?--namespace needs a value}; shift 2 ;;
    --context)   context=${2:?--context needs a value}; shift 2 ;;
    -h|--help)   sed -n '2,40p' "$0"; exit 0 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

k=(kubectl)
[ -n "$context" ] && k+=(--context "$context")

fail=0; warn=0
say()   { printf '\n\033[1m%s\033[0m\n' "$1"; }
pass()  { printf '[PASS] %-26s %s\n' "$1" "$2"; }
warnf() { printf '[WARN] %-26s %s\n' "$1" "$2"; warn=1; }
failf() { printf '[FAIL] %-26s %s\n' "$1" "$2"; fail=1; }

command -v kubectl >/dev/null 2>&1 || {
  echo "kubectl is not installed. This script must run where the CKS kubeconfig lives." >&2
  exit 2
}
[ -r "$probe" ] || { echo "cannot read $probe" >&2; exit 2; }

say "0. kubeconfig"
ctx=$("${k[@]}" config current-context 2>/dev/null) || ctx=""
[ -n "$ctx" ] || { failf "context" "no current kubectl context"; exit 1; }
pass "context" "$ctx"
# A bare 403 on /api is an expired CoreWeave token, not RBAC — it looks exactly
# like a permissions problem and is not. See .claude/skills/deploy/SKILL.md §1.
if ! api=$("${k[@]}" get --raw /api 2>&1); then
  case "$api" in
    *403*|*Forbidden*) failf "token" "403 on /api — the CoreWeave token is expired; refresh it in the console" ;;
    *)                 failf "token" "cannot reach the API server: $(printf '%s' "$api" | head -1)" ;;
  esac
  exit 1
fi
pass "token" "API server reachable"

say "1. The VM node Pod"
pod=$("${k[@]}" -n "$namespace" get pod \
  -l app.kubernetes.io/name=sparkbox,app.kubernetes.io/component=vm-node \
  --field-selector status.phase=Running \
  -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
[ -n "$pod" ] || { failf "vm-node pod" "no Running vm-node Pod in namespace $namespace"; exit 1; }
node=$("${k[@]}" -n "$namespace" get pod "$pod" -o jsonpath='{.spec.nodeName}')
pass "vm-node pod" "$pod on $node"

say "2. Node facts"
# Fetched one at a time on purpose: osImage contains spaces ("Ubuntu 24.04.1
# LTS"), so a single jsonpath split by `read` silently shifts every later field.
nodefact() { "${k[@]}" get node "$node" -o "jsonpath={.status.nodeInfo.$1}" 2>/dev/null; }
kernel=$(nodefact kernelVersion)
os=$(nodefact osImage)
runtime=$(nodefact containerRuntimeVersion)
arch=$(nodefact architecture)
pass "kernel" "${kernel:-unknown}"
pass "os / runtime" "${os:-unknown} / ${runtime:-unknown}"
pass "arch" "${arch:-unknown}"
for res in sparkbox.dev/kvm sparkbox.dev/tun sparkbox.dev/loop; do
  n=$("${k[@]}" get node "$node" -o "go-template={{index .status.allocatable \"$res\"}}" 2>/dev/null)
  case "$n" in
    1) pass "allocatable $res" "1" ;;
    *) warnf "allocatable $res" "${n:-<none>} — the device plugin may not be advertising it" ;;
  esac
done
pools=$("${k[@]}" get nodes -L compute.coreweave.com/node-pool --no-headers 2>/dev/null |
  awk '{print $NF}' | sort | uniq -c | awk '{printf "%s(%s) ", $2, $1}')
if [ -n "$pools" ]; then
  pass "node pools" "$pools"
  # §7: isolating nested-enabled sandboxes needs a pool of their own, and that
  # is a second full deployment, not a nodeSelector — but it needs a pool first.
  [ "$(printf '%s\n' "$pools" | wc -w)" -lt 2 ] &&
    warnf "nested isolation" "only one node pool visible; a dedicated nested pool would have to be created"
else
  warnf "node pools" "could not list node pools"
fi

say "3. Question A — can this Node host nested guests? (probe inside vmm-helper)"
probe_out=$("${k[@]}" -n "$namespace" exec -i "$pod" -c vmm-helper -- bash -s 2>&1 <"$probe")
probe_rc=$?
printf '%s\n' "$probe_out" | sed 's/^/    /'
case "$probe_rc" in
  0) pass "node preflight" "may admit nested sandboxes" ;;
  1) failf "node preflight" "hard blocker — see the probe output above" ;;
  *) warnf "node preflight" "undetermined (exit $probe_rc)" ;;
esac

say "4. Question B — does a Firecracker guest already see VMX/SVM?"
if [ -z "$sandbox" ]; then
  warnf "guest cpuid" "no --sandbox given; skipped"
  cat <<EOF
    Run this against any live sandbox to settle it — it is read-only and takes
    seconds. The guest kernel has no CONFIG_KVM, but the flag is a CPUID bit the
    kernel prints regardless, so its presence is the whole answer:

      ssh $USER-box@$domain 'grep -o -m1 -E "\\b(vmx|svm)\\b" /proc/cpuinfo; ls /dev/kvm 2>&1'

    Then re-run this script with --sandbox <name> to record it.
EOF
else
  guest=$(ssh -o BatchMode=yes -o StrictHostKeyChecking=accept-new -o ConnectTimeout=15 \
    "$sandbox@$domain" 'grep -o -m1 -E "\b(vmx|svm)\b" /proc/cpuinfo || true; echo "---"; uname -r; echo "---"; ls /dev/kvm 2>&1 || true' 2>&1)
  if [ $? -ne 0 ]; then
    warnf "guest cpuid" "could not ssh to $sandbox@$domain: $(printf '%s' "$guest" | head -1)"
  else
    flag=$(printf '%s' "$guest" | sed -n '1p')
    gkernel=$(printf '%s' "$guest" | sed -n '3p')
    case "$flag" in
      vmx|svm)
        # Not a vulnerability by itself: without CONFIG_KVM the guest cannot use
        # it. It does mean the absence of nested exposure is CoreWeave's module
        # parameter, not our doing.
        warnf "guest cpuid" "guest sees '$flag' (kernel $gkernel) — as the spike predicted. A masking CPU template (T2CL/T2/C3 on Intel, T2A on AMD) would make this OUR property; today it is the Node's." ;;
      "")
        # Measured on 2026-09-04 and expected: KVM only advertises VMX/SVM in
        # KVM_GET_SUPPORTED_CPUID when the module's nested parameter is on, so an
        # empty answer here is almost always a reading of the NODE. Section 3
        # above has the direct check.
        pass "guest cpuid" "guest sees neither vmx nor svm (kernel $gkernel) — expected when $([ -n "${node:-}" ] && echo "$node" || echo "the node") has kvm_*.nested=0; see the module parameter in section 3" ;;
      *)
        warnf "guest cpuid" "unexpected output: $flag" ;;
    esac
    printf '%s\n' "$guest" | sed 's/^/    /'
  fi
fi

say "5. Record these answers"
cat <<EOF
Paste into docs/cloud-hypervisor-feasibility.md §10 (M0 results):

| node | $node |
| kernel | ${kernel:-?} |
| os / runtime / arch | ${os:-?} / ${runtime:-?} / ${arch:-?} |
| node preflight | exit $probe_rc (see above) |
| guest sees vmx/svm | ${flag:-not measured} |
| node pools | ${pools:-?} |

Still to ask CoreWeave, because no probe answers them:
  1. Is kvm_{intel,amd}.nested deliberately set anywhere in this fleet, and can
     we rely on it staying that way?
  2. What is the kernel patch cadence per CPU pool? The binding gate is
     CVE-2026-80726 (6.1.183 / 6.6.152 / 6.12.104 / 6.18.45 / 7.1.9).
  3. Is landlock in the node kernel's CONFIG_LSM? Cloud Hypervisor pins ABI v3
     and fails vm.create without it.
  4. Can we get a second CPU node pool, to isolate nested-enabled sandboxes?
EOF

echo
if [ "$fail" -ne 0 ]; then
  echo "verdict: a hard blocker was found — nested is not available on this Node as it stands"
  exit 1
elif [ "$warn" -ne 0 ]; then
  echo "verdict: undetermined — resolve the WARN lines above"
  exit 2
fi
echo "verdict: this Node may admit nested-virtualization sandboxes"
