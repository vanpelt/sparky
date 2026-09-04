#!/usr/bin/env bash
# M0b, the part that runs on the KVM host. See run-on-cks.sh for the wrapper
# that puts this in a throwaway Pod on the CKS node, and
# docs/cloud-hypervisor-feasibility.md section 9 (M0b) for why.
#
# The question: with CONFIG_KVM_INTEL in the guest kernel, does a Firecracker
# sandbox run an inner VM? M0 measured every other precondition as already
# satisfied, so this is the step that turns "feasible" into "works" or "does
# not".
#
# Three layers, all built here:
#
#   L0  this container, on the CKS node, with hostPath /dev/kvm
#   L1  a Firecracker microVM on the sparkbox guest kernel + the nested fragment
#   L2  a Firecracker microVM launched from inside L1
#
# L1 and L2 boot the same vmlinux and use initramfs only -- no drives, no TAP,
# no network. That keeps this self-contained and means it touches nothing the
# sparkbox deployment owns. L1's initramfs carries the firecracker binary, a
# second copy of the kernel and L2's initramfs, so the whole nesting is one
# artifact.
#
# Output is a set of M0B_* marker lines, parsed by the wrapper.
set -uo pipefail

WORK=${WORK:-/work}
KVER=${KVER:-6.1.155}
FC_VER=${FC_VER:-v1.16.1}
# NOT $(nproc): a container sees all 64 of the node's CPUs regardless of its cgroup
# limit, so the default would fork 64 gcc processes against a 16-CPU/16Gi cap --
# throttled, memory-hungry, and rude to the sandboxes sharing the node. Derive it
# from the cgroup v2 quota when there is one.
default_jobs() {
  local q p
  if read -r q p 2>/dev/null </sys/fs/cgroup/cpu.max && [ "$q" != max ]; then
    echo $(( q / p ))
  else
    echo 8
  fi
}
JOBS=${JOBS:-$(default_jobs)}
L1_MEM=${L1_MEM:-3072}
L2_MEM=${L2_MEM:-512}
L1_TIMEOUT=${L1_TIMEOUT:-180}

hack="$WORK/hack"
out="$WORK/out"
mkdir -p "$out"

step() { printf '\n\033[1m== %s\033[0m\n' "$1"; }
marker() { printf 'M0B_%s=%s\n' "$1" "$2"; }

# --- 0. host preconditions ----------------------------------------------------
step "0. host"
uname -a
[ -c /dev/kvm ] || { marker HOST_KVM missing; echo "no /dev/kvm in this container" >&2; exit 1; }
marker HOST_KVM ok
marker HOST_NESTED "$(cat /sys/module/kvm_intel/parameters/nested 2>/dev/null ||
                      cat /sys/module/kvm_amd/parameters/nested 2>/dev/null || echo unknown)"

# --- 1. build deps ------------------------------------------------------------
# build-kernel.sh installs its own via `sudo apt-get`, and there is no sudo in a
# stock ubuntu image running as root. Give it a shim rather than patching it.
step "1. deps"
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y -qq busybox-static cpio gzip curl xz-utils file >/dev/null
if [ ! -x /usr/bin/sudo ]; then
  # `exec env "$@"`, not `exec "$@"`: build-kernel.sh calls
  # `sudo DEBIAN_FRONTEND=noninteractive apt-get ...`, and only env accepts a
  # leading VAR=value the way real sudo does.
  #
  # Written unconditionally rather than behind `command -v sudo`, which finds a
  # shim this script wrote on an earlier run into the same Pod and would then
  # skip fixing it.
  printf '#!/bin/sh\nexec env "$@"\n' >/usr/local/bin/sudo
  chmod +x /usr/local/bin/sudo
fi

# --- 2. the guest kernel ------------------------------------------------------
# The tracked fragment plus the nested experiment fragment, concatenated. The
# experiment file is kept separate on purpose: the default build must not pick
# it up.
step "2. guest kernel $KVER (+ nested fragment)"
cat "$hack/kernel-config.fragment" "$hack/m0b/kernel-config.nested.fragment" >"$out/merged.fragment"
if [ ! -s "$out/vmlinux" ]; then
  FRAGMENT="$out/merged.fragment" OUT="$out/vmlinux" WORK="$WORK/kbuild" \
    KVER="$KVER" JOBS="$JOBS" bash "$hack/build-kernel.sh" || {
      marker KERNEL_BUILD failed; exit 1; }
fi
marker KERNEL_BUILD ok
file "$out/vmlinux" | cut -c1-120

# Prove the option actually survived olddefconfig rather than trusting the
# fragment. kconfig silently drops a symbol whose dependencies are unmet.
kconfig="$WORK/kbuild/linux-$KVER/.config"
for sym in CONFIG_KVM CONFIG_KVM_INTEL; do
  v=$(grep -E "^$sym=" "$kconfig" 2>/dev/null | cut -d= -f2)
  marker "CONFIG_$sym" "${v:-unset}"
done

# --- 3. firecracker -----------------------------------------------------------
# Downloaded rather than copied off the node: this container must not read from
# /var/lib/sparkbox, and pinning the version keeps the result reproducible.
step "3. firecracker $FC_VER"
if [ ! -x "$out/firecracker" ]; then
  curl -fsSL "https://github.com/firecracker-microvm/firecracker/releases/download/${FC_VER}/firecracker-${FC_VER}-x86_64.tgz" \
    -o "$out/fc.tgz" || { marker FIRECRACKER fetch_failed; exit 1; }
  tar -xzf "$out/fc.tgz" -C "$out"
  cp "$out/release-${FC_VER}-x86_64/firecracker-${FC_VER}-x86_64" "$out/firecracker"
  chmod +x "$out/firecracker"
fi
"$out/firecracker" --version | head -1
marker FIRECRACKER ok

# --- 4. initramfs, innermost first --------------------------------------------
# busybox-static so there is no libc to carry. L2 only has to prove it reached
# userspace; it prints a marker and powers off, which makes firecracker exit.
step "4. initramfs"
build_initramfs() { # <dir> <output.gz>
  ( cd "$1" && find . -print0 | cpio --null -o -H newc --quiet ) | gzip -9 >"$2"
}
busybox_bin=$(command -v busybox) || { marker BUSYBOX missing; exit 1; }

rm -rf "$out/l2root" && mkdir -p "$out/l2root"/{bin,proc,sys,dev}
cp "$busybox_bin" "$out/l2root/bin/busybox"
ln -sf busybox "$out/l2root/bin/sh"
cat >"$out/l2root/init" <<'L2INIT'
#!/bin/sh
/bin/busybox --install -s /bin
mount -t proc proc /proc 2>/dev/null
echo "M0B_L2_ALIVE=yes"
echo "M0B_L2_UNAME=$(uname -r)"
echo "M0B_L2_CPU=$(grep -c ^processor /proc/cpuinfo)"
# `reboot=k` in boot_args makes this an i8042 reset, which Firecracker traps and
# exits on. poweroff would need ACPI and can simply hang instead.
reboot -f
L2INIT
chmod +x "$out/l2root/init"
build_initramfs "$out/l2root" "$out/l2-initrd.gz"

# L1 carries everything needed to be a hypervisor itself.
rm -rf "$out/l1root" && mkdir -p "$out/l1root"/{bin,proc,sys,dev,vm}
cp "$busybox_bin" "$out/l1root/bin/busybox"
ln -sf busybox "$out/l1root/bin/sh"
cp "$out/firecracker" "$out/l1root/vm/firecracker"
cp "$out/vmlinux"     "$out/l1root/vm/vmlinux"
cp "$out/l2-initrd.gz" "$out/l1root/vm/l2-initrd.gz"
cat >"$out/l1root/vm/l2.json" <<L2CFG
{
  "boot-source": {
    "kernel_image_path": "/vm/vmlinux",
    "initrd_path": "/vm/l2-initrd.gz",
    "boot_args": "console=ttyS0 reboot=k panic=1 pci=off"
  },
  "drives": [],
  "machine-config": { "vcpu_count": 1, "mem_size_mib": $L2_MEM, "smt": false }
}
L2CFG

# The L1 init is the actual experiment. Report the three things that were
# ambiguous before this run, in order, then try to be a hypervisor.
cat >"$out/l1root/init" <<'L1INIT'
#!/bin/sh
/bin/busybox --install -s /bin
mount -t proc proc /proc 2>/dev/null
mount -t sysfs sys /sys 2>/dev/null
mount -t devtmpfs dev /dev 2>/dev/null

echo "M0B_L1_ALIVE=yes"
echo "M0B_L1_UNAME=$(uname -r)"

# The false negative from M0: with CONFIG_KVM_INTEL the kernel takes the other
# branch in init_ia32_feat_ctl(), so this should now say yes where every
# production sandbox says no.
grep -qw vmx /proc/cpuinfo && echo "M0B_L1_PROC_VMX=yes" || echo "M0B_L1_PROC_VMX=no"
[ -c /dev/kvm ] && echo "M0B_L1_DEVKVM=yes" || echo "M0B_L1_DEVKVM=no"
echo "M0B_L1_NESTED=$(cat /sys/module/kvm_intel/parameters/nested 2>/dev/null || echo unknown)"

if [ -c /dev/kvm ]; then
  echo "--- L2 boot begins ---"
  /vm/firecracker --no-api --config-file /vm/l2.json
  echo "M0B_L2_FC_RC=$?"
else
  echo "M0B_L2_FC_RC=skipped_no_kvm"
fi
echo "M0B_L1_DONE=yes"
reboot -f
L1INIT
chmod +x "$out/l1root/init"
build_initramfs "$out/l1root" "$out/l1-initrd.gz"
ls -lh "$out"/*.gz "$out/vmlinux" | awk '{print $5, $9}'

# --- 5. boot L1 ---------------------------------------------------------------
step "5. boot L1 (and, from inside it, L2)"
cat >"$out/l1.json" <<L1CFG
{
  "boot-source": {
    "kernel_image_path": "$out/vmlinux",
    "initrd_path": "$out/l1-initrd.gz",
    "boot_args": "console=ttyS0 reboot=k panic=1 pci=off"
  },
  "drives": [],
  "machine-config": { "vcpu_count": 2, "mem_size_mib": $L1_MEM, "smt": false }
}
L1CFG

timeout "$L1_TIMEOUT" "$out/firecracker" --no-api --config-file "$out/l1.json" 2>&1 |
  tee "$out/l1-console.log"
marker L1_FC_RC "${PIPESTATUS[0]}"

# --- 6. verdict ---------------------------------------------------------------
step "6. verdict"
log="$out/l1-console.log"
has() { grep -q "$1" "$log"; }
if has 'M0B_L2_ALIVE=yes'; then
  echo "VERDICT: an inner VM booted inside a Firecracker sandbox. Nested"
  echo "         virtualization works today on the VMM we already ship."
  marker VERDICT l2_booted
elif has 'M0B_L1_DEVKVM=yes'; then
  echo "VERDICT: L1 got /dev/kvm but L2 did not boot. Read the console log --"
  echo "         this is the interesting failure, not the boring one."
  marker VERDICT l1_kvm_no_l2
elif has 'M0B_L1_ALIVE=yes'; then
  echo "VERDICT: L1 booted without /dev/kvm. Check CONFIG_KVM_INTEL above and"
  echo "         M0B_L1_PROC_VMX -- if PROC_VMX=no the FEAT_CTL lock is still"
  echo "         being taken, which would mean the kernel option did not apply."
  marker VERDICT l1_no_kvm
else
  echo "VERDICT: L1 did not reach userspace. This is a harness problem, not a"
  echo "         result. Console log follows above."
  marker VERDICT l1_dead
fi
