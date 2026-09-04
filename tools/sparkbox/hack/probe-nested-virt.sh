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
#   4. Does the running kernel carry the fixes for CVE-2026-53359 (Januscape)
#      and CVE-2026-64561 (Zapscape)? Both are guest-to-host escapes reachable
#      only through nested virtualization. A Node without both fixes must not
#      admit nested sandboxes, whatever the other answers are.
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
kernel=$(uname -r)
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
        pass "second-level paging" "$slat"
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
      *)     failf "$module.nested" "$nested — nested virtualization is off on this host (mitigation posture, or deliberate); it is not ours to flip on CKS" ;;
    esac
  else
    warnf "$module.nested" "$param not readable; module not loaded or sysfs hidden — check from the host"
  fi
fi

# --- 4. kernel fixes for the 2026 nested-virt escapes -------------------------
# Stable series -> first release carrying the fix, per the kernel.org CVE
# records quoted in docs/cloud-hypervisor-feasibility.md. A series absent from
# a list means the record we have names no fix for it: treat as undetermined,
# not as safe.
declare -A januscape=( [5.10]=260 [5.15]=211 [6.1]=177 [6.6]=144 [6.12]=95 [6.18]=38 [7.1]=3 )
declare -A zapscape=(  [6.6]=148 [6.12]=101 [6.18]=42 [7.1]=6 )

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
    warnf "$label" "no fix version on record for the $series series (kernel $kernel); verify against kernel.org CVE data or the distro's backport"
    return
  fi
  if [ "$patch" -ge "${table[$series]}" ]; then
    pass "$label" "kernel $kernel >= $series.${table[$series]}"
  else
    failf "$label" "kernel $kernel < $series.${table[$series]} — a nested guest can reach the vulnerable path; do not admit nested sandboxes"
  fi
}
check_cve "CVE-2026-53359 Januscape" januscape
check_cve "CVE-2026-64561 Zapscape" zapscape

# Distro kernels backport without bumping the upstream patch level. Give the
# operator the string they need to check against the vendor advisory.
if [ -r /proc/version ]; then
  echo "       /proc/version: $(cut -c1-120 /proc/version)"
fi

# --- 5. optional: the VMM itself ----------------------------------------------
# CLOUD_HYPERVISOR=/path/to/binary reports its version and whether `nested`
# is a recognised --cpus parameter (it arrived in v50.0).
if [ -n "${CLOUD_HYPERVISOR:-}" ]; then
  if [ -x "$CLOUD_HYPERVISOR" ]; then
    ver=$("$CLOUD_HYPERVISOR" --version 2>/dev/null | head -1)
    if "$CLOUD_HYPERVISOR" --help 2>/dev/null | grep -q 'nested=on|off'; then
      pass "cloud-hypervisor" "${ver:-unknown version}, --cpus nested= supported"
    else
      failf "cloud-hypervisor" "${ver:-unknown version} does not document --cpus nested= (needs >= v50.0)"
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
