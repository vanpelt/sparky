#!/usr/bin/env bash
# Host preflight for exposing nested virtualization to sandboxes.
#
# Companion to docs/cloud-hypervisor-feasibility.md (§3, M0). It answers, from
# any Linux shell — a bare host, the DGX, or `kubectl exec -c vmm-helper` into
# the CKS node Pod — the questions that decide whether a Node may run a
# sandbox with `--cpus nested=on`:
#
#   1. Does the CPU virtualize at all, and with second-level paging (ept/npt)?
#      Without EPT/NPT an L2 guest runs on shadow paging and is unusably slow.
#   2. Is /dev/kvm present?
#   3. Is the KVM module's `nested` parameter on? It is the upstream default,
#      but `kvm_intel.nested=0` / `kvm_amd.nested=0` is the published
#      mitigation for the 2026 escapes, so a hardened fleet may have it off.
#   4. Does the running kernel carry the fixes for the 2026 shadow-MMU escapes --
#      CVE-2026-53359 (Januscape), CVE-2026-64561 (Zapscape) and its deferred
#      role.invalid follow-on CVE-2026-80726? All three are guest-to-host bugs in
#      arch/x86/kvm/mmu/mmu.c. Nested virtualization is how a guest forces L0 into
#      that code on a normal ept=1/npt=1 host, but it is not the only way in: with
#      EPT/NPT off every guest runs on the shadow MMU, and CVE-2026-80726 is
#      scored PR:N against a plain guest. A Node without all three fixes must not
#      admit nested sandboxes, whatever the other answers are.
#   5. Is Landlock usable? Cloud Hypervisor pins Landlock ABI v3 and fails
#      vm.create outright without it, so --landlock is a node property too.
#
# Read-only; needs no root. Exit 0 when nested may be admitted, 1 when it may
# not, 2 when something could not be determined (report it, decide manually).
# Output follows `sparkbox doctor`'s [PASS]/[WARN]/[FAIL] shape.
set -uo pipefail

fail=0
warn=0
pass() { printf '[PASS] %-28s %s\n' "$1" "$2"; }
warnf() { printf '[WARN] %-28s %s\n' "$1" "$2"; warn=1; }
failf() { printf '[FAIL] %-28s %s\n' "$1" "$2"; fail=1; }

arch=$(uname -m)
# Overridable so an operator can judge a candidate Node's kernel string without
# shelling into it, and so the version tables below are testable.
kernel="${SPARKBOX_PROBE_KERNEL:-$(uname -r)}"
echo "nested-virtualization preflight on $(hostname) ($arch, kernel $kernel)"

# --- 1. CPU ------------------------------------------------------------------
case "$arch" in
  x86_64)
    flags=$(grep -m1 '^flags' /proc/cpuinfo 2>/dev/null | cut -d: -f2)
    vendor=$(grep -m1 '^vendor_id' /proc/cpuinfo 2>/dev/null | awk '{print $3}')
    model=$(grep -m1 '^model name' /proc/cpuinfo 2>/dev/null | cut -d: -f2- | sed 's/^ *//')
    if grep -qw vmx <<<"$flags"; then
      virt=vmx; slat=ept; module=kvm_intel
    elif grep -qw svm <<<"$flags"; then
      virt=svm; slat=npt; module=kvm_amd
    else
      virt=""; slat=""; module=""
    fi
    if [ -n "$virt" ]; then
      pass "cpu virtualization" "$virt ($vendor, $model)"
      if grep -qw "$slat" <<<"$flags"; then
        # The CPUID flag says the silicon can do it; the module parameter says
        # KVM is using it. With kvm_intel.ept=0 / kvm_amd.npt=0 EVERY guest runs
        # on the shadow MMU -- the code all three 2026 escapes live in.
        slatparam="/sys/module/$module/parameters/$slat"
        if [ -r "$slatparam" ]; then
          case "$(cat "$slatparam")" in
            1|Y|y) pass "second-level paging" "$slat (cpu flag, $module.$slat on)" ;;
            *)     failf "second-level paging" "cpu has $slat but $module.$slat is off: every guest runs on the shadow MMU" ;;
          esac
        else
          warnf "second-level paging" "$slat cpu flag present but $slatparam not readable; confirm $module.$slat=1 from the host"
        fi
      else
        failf "second-level paging" "no $slat flag: L2 guests would run on shadow paging"
      fi
    else
      failf "cpu virtualization" "neither vmx nor svm in /proc/cpuinfo${model:+ ($model)}"
      # In a VM this usually means the outer hypervisor is not exposing it.
      if grep -q '^flags.*hypervisor' /proc/cpuinfo 2>/dev/null; then
        warnf "running under hypervisor" "this is itself a guest; the outer VMM must expose vmx/svm"
      fi
    fi
    ;;
  aarch64)
    # Cloud Hypervisor's `nested` option is x86-64 only and arm64 KVM nested
    # support in mainline is FEAT_NV2-only and recent. Report, do not admit.
    module=kvm
    if [ -c /dev/kvm ]; then
      pass "cpu virtualization" "/dev/kvm present (arm64)"
    fi
    failf "nested on arm64" "Cloud Hypervisor exposes nested only on x86-64; arm64 is parity-only (see docs/cloud-hypervisor-feasibility.md §8.5)"
    ;;
  *)
    failf "architecture" "$arch is not a Sparkbox host architecture"
    ;;
esac

# --- 2. /dev/kvm --------------------------------------------------------------
if [ -c /dev/kvm ]; then
  pass "/dev/kvm" "character device"
elif [ -e /dev/kvm ]; then
  failf "/dev/kvm" "exists but is not a character device (a placeholder file?)"
else
  failf "/dev/kvm" "absent (on CKS: check the sparkbox.dev/kvm allocation)"
fi

# --- 3. kvm module nested parameter ------------------------------------------
if [ -n "${module:-}" ]; then
  param="/sys/module/$module/parameters/nested"
  if [ -r "$param" ]; then
    nested=$(cat "$param")
    case "$nested" in
      1|Y|y) pass "$module.nested" "$nested" ;;
      *)     failf "$module.nested" "$nested — nested virtualization is OFF on this host. KVM then advertises no VMX/SVM to any guest and faults every VMX instruction with #UD, so no VMM can expose nested here. module_param(nested, bool, 0444) is read-only at runtime: changing it needs a modprobe.d drop-in or kernel cmdline plus a module reload or reboot. On CKS that is CoreWeave's operation, not ours." ;;
    esac
  else
    warnf "$module.nested" "$param not readable; module not loaded or sysfs hidden — check from the host"
  fi
fi

# --- 3b. what KVM actually offers a guest -------------------------------------
# The module parameter is the cause; this is the effect, and the effect is what
# a VMM can pass on. KVM sets X86_FEATURE_VMX / X86_FEATURE_SVM in
# KVM_GET_SUPPORTED_CPUID only `if (nested)`, so reading the bit back settles in
# one ioctl what the parameter only implies -- and it is the number to compare
# against a guest's own CPUID when the two disagree. Read-only: the ioctl is on
# the /dev/kvm fd and creates no VM.
if [ "$arch" = x86_64 ] && [ -c /dev/kvm ] && command -v python3 >/dev/null 2>&1; then
  kvm_offers=$(python3 - <<'PY' 2>&1
import fcntl, struct, array, os
KVM_GET_SUPPORTED_CPUID = 0xC008AE05  # _IOWR(0xAE, 0x05, struct kvm_cpuid2)
NENT = 256
try:
    fd = os.open("/dev/kvm", os.O_RDWR | os.O_CLOEXEC)
except OSError as e:
    print("err:cannot open /dev/kvm: %s" % e.strerror); raise SystemExit
buf = array.array("B", b"\x00" * (8 + NENT * 40))
struct.pack_into("<II", buf, 0, NENT, 0)
try:
    fcntl.ioctl(fd, KVM_GET_SUPPORTED_CPUID, buf, True)
except OSError as e:
    print("err:KVM_GET_SUPPORTED_CPUID failed: %s" % e.strerror); raise SystemExit
nent = struct.unpack_from("<I", buf, 0)[0]
leaves = {}
for i in range(nent):
    f, ix, fl, a, b, c, d = struct.unpack_from("<IIIIIII", buf, 8 + i * 40)
    leaves.setdefault(f, (a, b, c, d))
vmx = (leaves.get(1, (0, 0, 0, 0))[2] >> 5) & 1
svm = (leaves.get(0x80000001, (0, 0, 0, 0))[2] >> 2) & 1
print("ok:%d:%d:0x%08x" % (vmx, svm, leaves.get(1, (0, 0, 0, 0))[2]))
PY
)
  IFS=: read -r ko_status ko_vmx ko_svm ko_ecx <<<"$kvm_offers"
  if [ "$ko_status" = ok ]; then
    ko_have=""
    [ "$ko_vmx" = 1 ] && ko_have="VMX"
    [ "$ko_svm" = 1 ] && ko_have="${ko_have:+$ko_have and }SVM"
    if [ -n "$ko_have" ]; then
      pass "KVM offers guests" "$ko_have in KVM_GET_SUPPORTED_CPUID (leaf 1 ECX $ko_ecx) -- a VMM can pass nested through"
    else
      failf "KVM offers guests" "neither VMX nor SVM in KVM_GET_SUPPORTED_CPUID (leaf 1 ECX $ko_ecx) -- no VMM can expose nested here whatever it is asked to do"
    fi
  elif [ "$ko_status" = err ]; then
    warnf "KVM offers guests" "${kvm_offers#err:}"
  else
    warnf "KVM offers guests" "could not read KVM_GET_SUPPORTED_CPUID"
  fi
fi

# --- 4. kernel fixes for the 2026 nested-virt escapes -------------------------
# Stable series -> first release carrying the fix, transcribed from the kernel.org
# CVE records themselves (git.kernel.org/pub/scm/linux/security/vulns.git,
# cve/published/2026) and not from press coverage, which got both of the first
# two wrong: it reported 5.15.211/5.10.260 for Januscape (those releases carry no
# such fix) and omitted Zapscape's 5.15.218/6.1.183 (which made the whole 6.1 LTS
# series -- the likeliest CKS node series -- unjudgeable).
#
# All three are also fixed in mainline 7.2. A series absent from a list means the
# record names no fix for it: treat absence as UNFIXED, not as undetermined.
#
# CVE-2026-80726 is the role.invalid follow-on that CVE-2026-64561's own record
# defers ("that flaw will be addressed separately"). Its fix lands 3-4 stable
# releases after Zapscape's in every series but 6.1, so it -- not Zapscape -- is
# the binding gate.
declare -A januscape=( [6.1]=177 [6.6]=144 [6.12]=95  [6.18]=38 [7.1]=3 )
declare -A zapscape=(  [5.15]=218 [6.1]=183 [6.6]=148 [6.12]=101 [6.18]=42 [7.1]=6 )
declare -A rolescape=( [6.1]=183 [6.6]=152 [6.12]=104 [6.18]=45 [7.1]=9 )

check_cve() { # <label> <assoc-array-name>
  local label=$1; local -n table=$2
  local base patch series
  # 6.12.101-foo -> series 6.12, patch 101. An -rc or distro kernel with a
  # rewritten version string lands in WARN, which is the honest answer.
  if [[ $kernel =~ ^([0-9]+)\.([0-9]+)\.([0-9]+) ]]; then
    series="${BASH_REMATCH[1]}.${BASH_REMATCH[2]}"; patch="${BASH_REMATCH[3]}"
  elif [[ $kernel =~ ^([0-9]+)\.([0-9]+) ]]; then
    series="${BASH_REMATCH[1]}.${BASH_REMATCH[2]}"; patch=0
  else
    warnf "$label" "cannot parse kernel version $kernel"; return
  fi
  if [ -z "${table[$series]+x}" ]; then
    # The kernel.org records set defaultStatus "affected" and enumerate only the
    # series that received a backport, so a series that is absent is unfixed --
    # not merely unknown. Mainline 7.2 and later carry all three.
    if [ "${BASH_REMATCH[1]}" -ge 8 ] || { [ "${BASH_REMATCH[1]}" = 7 ] && [ "${BASH_REMATCH[2]}" -ge 2 ]; }; then
      pass "$label" "kernel $kernel is past mainline 7.2"
    else
      failf "$label" "the $series series has no fix on record (kernel $kernel); if this is a distro kernel, verify the backport against the vendor advisory before overriding"
    fi
    return
  fi
  if [ "$patch" -ge "${table[$series]}" ]; then
    pass "$label" "kernel $kernel >= $series.${table[$series]}"
  else
    failf "$label" "kernel $kernel < $series.${table[$series]} — a nested guest can reach the vulnerable path; do not admit nested sandboxes"
  fi
}
check_cve "CVE-2026-53359 Januscape"   januscape
check_cve "CVE-2026-64561 Zapscape"    zapscape
check_cve "CVE-2026-80726 role.invalid" rolescape

# Distro kernels backport without bumping the upstream patch level. Give the
# operator the string they need to check against the vendor advisory.
if [ -r /proc/version ]; then
  echo "       /proc/version: $(cut -c1-120 /proc/version)"
fi

# --- 5. Landlock ---------------------------------------------------------------
# Cloud Hypervisor's --landlock requires Landlock ABI v3 and fails vm.create
# outright without it, so this is a node property exactly like kvm.nested.
#
# Ask the kernel directly first: landlock_create_ruleset(NULL, 0,
# LANDLOCK_CREATE_RULESET_VERSION) returns the ABI version and needs no
# privilege, no securityfs mount and no readable dmesg. That matters because the
# way we actually run this probe is `kubectl exec -c vmm-helper`, and the node
# Pod has no /sys/kernel/security -- the securityfs path below answered WARN on
# CKS for a Node whose real answer is ABI 7. The syscall number is 444 on every
# Linux architecture that has it.
landlock_abi=""
if command -v python3 >/dev/null 2>&1; then
  landlock_abi=$(python3 - <<'PY' 2>/dev/null
import ctypes
libc = ctypes.CDLL(None, use_errno=True)
r = libc.syscall(ctypes.c_long(444), None, ctypes.c_size_t(0), ctypes.c_uint(1))
print(r if r > 0 else "")
PY
)
elif command -v perl >/dev/null 2>&1; then
  landlock_abi=$(perl -e 'my $r = syscall(444, 0, 0, 1); print $r > 0 ? $r : ""' 2>/dev/null)
fi
if [ -n "$landlock_abi" ]; then
  if [ "$landlock_abi" -lt 3 ]; then
    warnf "landlock" "ABI v$landlock_abi; cloud-hypervisor --landlock needs v3 and fails VM creation below it"
  else
    pass "landlock" "ABI v$landlock_abi (>= v3, what cloud-hypervisor --landlock pins)"
  fi
elif [ -r /sys/kernel/security/lsm ]; then
  if grep -qw landlock /sys/kernel/security/lsm; then
    abi=$(dmesg 2>/dev/null | sed -n 's/.*landlock: Up and running.*ABI version \([0-9]*\).*/\1/p' | tail -1)
    if [ -n "$abi" ] && [ "$abi" -lt 3 ]; then
      warnf "landlock" "ABI v$abi; cloud-hypervisor --landlock needs v3 and fails VM creation below it"
    else
      pass "landlock" "enabled in the active LSM list${abi:+ (ABI v$abi)}"
    fi
  else
    warnf "landlock" "not in the active LSM list; cloud-hypervisor --landlock would fail VM creation (chroot and seccomp are unaffected)"
  fi
else
  warnf "landlock" "landlock_create_ruleset() gave no version (no python3/perl, or the syscall is unavailable) and /sys/kernel/security/lsm is not readable; confirm landlock is in CONFIG_LSM from the host"
fi

# --- 6. optional: the VMM itself ----------------------------------------------
# CLOUD_HYPERVISOR=/path/to/binary reports its version and whether `nested`
# is a recognised --cpus parameter (it arrived in v50.0).
if [ -n "${CLOUD_HYPERVISOR:-}" ]; then
  if [ -x "$CLOUD_HYPERVISOR" ]; then
    ver=$("$CLOUD_HYPERVISOR" --version 2>/dev/null | head -1)
    if "$CLOUD_HYPERVISOR" --help 2>/dev/null | grep -q 'nested=on|off'; then
      # v50.0 introduced the option, but on AMD `nested=off` was a silent no-op
      # until v52.0 (the CPUID loop broke on leaf 1 before reaching the leaf that
      # carries SVM), and on arm64 `nested=off` was a hard parse error before
      # v52.0. Both make v52.0 the floor. See docs/cloud-hypervisor-feasibility.md.
      major=$(printf '%s' "$ver" | sed -n 's/.*v\([0-9][0-9]*\)\..*/\1/p')
      if [ -n "$major" ] && [ "$major" -lt 52 ]; then
        failf "cloud-hypervisor" "${ver}: --cpus nested=off is a no-op on AMD and a parse error on arm64 below v52.0"
      else
        pass "cloud-hypervisor" "${ver:-unknown version}, --cpus nested= supported"
      fi
    else
      failf "cloud-hypervisor" "${ver:-unknown version} does not document --cpus nested= (needs >= v52.0)"
    fi
  else
    warnf "cloud-hypervisor" "$CLOUD_HYPERVISOR is not executable"
  fi
fi

echo
if [ "$fail" -ne 0 ]; then
  echo "verdict: do NOT admit nested-virtualization sandboxes on this host"
  exit 1
elif [ "$warn" -ne 0 ]; then
  echo "verdict: undetermined — resolve the WARN lines before admitting nested sandboxes"
  exit 2
else
  echo "verdict: this host may admit nested-virtualization sandboxes (Cloud Hypervisor --cpus nested=on)"
  exit 0
fi
