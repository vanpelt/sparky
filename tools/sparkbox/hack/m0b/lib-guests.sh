#!/usr/bin/env bash
# Shared guest images for the M0b snapshot experiments.
#
# pause-test.sh (Firecracker as L1) and ch-snapshot-test.sh (Cloud Hypervisor as
# L1) must boot *the same* L1 and L2, or the comparison between them means
# nothing. That is the entire reason this file exists rather than each script
# carrying its own copy.
#
# The inner VMM is Firecracker in both cases, deliberately: it is the one we have
# already watched boot an L2 (section 11), so when an experiment fails the failure
# is attributable to the outer VMM under test rather than to the inner one.
#
# Both guests tick once a second to stdout. "Did it survive the snapshot" is then
# a question about counting lines in a console log, not an inference.
#
# Sourced, not executed. Expects $out set and /work/out/{vmlinux,firecracker}.

build_initramfs() { # <dir> <output.gz>
  ( cd "$1" && find . -print0 | cpio --null -o -H newc --quiet ) | gzip -9 >"$2"
}

build_tick_guests() { # writes $out/p1-initrd.gz (L1) and $out/p2-initrd.gz (L2)
  local busybox_bin
  busybox_bin=$(command -v busybox) || { echo "no busybox" >&2; return 1; }
  for f in "$out/vmlinux" "$out/firecracker"; do
    [ -s "$f" ] || { echo "missing $f -- run inside-pod.sh first" >&2; return 1; }
  done

  # --- L2: the innermost guest. Proves it is alive, then keeps proving it. ---
  rm -rf "$out/p2root" && mkdir -p "$out/p2root"/{bin,proc,sys,dev}
  cp "$busybox_bin" "$out/p2root/bin/busybox"
  ln -sf busybox "$out/p2root/bin/sh"
  cat >"$out/p2root/init" <<'P2'
#!/bin/sh
/bin/busybox --install -s /bin
mount -t proc proc /proc 2>/dev/null
echo "M0B_P_L2_ALIVE=yes"
i=0
while : ; do echo "M0B_P_L2_TICK=$i"; i=$((i+1)); sleep 1; done
P2
  chmod +x "$out/p2root/init"
  build_initramfs "$out/p2root" "$out/p2-initrd.gz"

  # --- L1: a sandbox that is also a hypervisor. ---
  rm -rf "$out/p1root" && mkdir -p "$out/p1root"/{bin,proc,sys,dev,vm}
  cp "$busybox_bin" "$out/p1root/bin/busybox"
  ln -sf busybox "$out/p1root/bin/sh"
  cp "$out/firecracker"  "$out/p1root/vm/firecracker"
  cp "$out/vmlinux"      "$out/p1root/vm/vmlinux"
  cp "$out/p2-initrd.gz" "$out/p1root/vm/l2-initrd.gz"
  cat >"$out/p1root/vm/l2.json" <<P2CFG
{
  "boot-source": {
    "kernel_image_path": "/vm/vmlinux",
    "initrd_path": "/vm/l2-initrd.gz",
    "boot_args": "console=ttyS0 reboot=k panic=1 pci=off"
  },
  "drives": [],
  "machine-config": { "vcpu_count": 1, "mem_size_mib": ${L2_MEM:-512}, "smt": false }
}
P2CFG
  cat >"$out/p1root/init" <<'P1'
#!/bin/sh
/bin/busybox --install -s /bin
mount -t proc proc /proc 2>/dev/null
mount -t sysfs sys /sys 2>/dev/null
mount -t devtmpfs dev /dev 2>/dev/null
echo "M0B_P_L1_ALIVE=yes"
grep -qw vmx /proc/cpuinfo && echo "M0B_P_L1_VMX=yes" || echo "M0B_P_L1_VMX=no"
[ -c /dev/kvm ] && echo "M0B_P_L1_DEVKVM=yes" || echo "M0B_P_L1_DEVKVM=no"
/vm/firecracker --no-api --config-file /vm/l2.json &
i=0
while : ; do echo "M0B_P_L1_TICK=$i"; i=$((i+1)); sleep 1; done
P1
  chmod +x "$out/p1root/init"
  build_initramfs "$out/p1root" "$out/p1-initrd.gz"
}

# Wait for a marker to appear in a log rather than sleeping a guessed interval.
await() { # <file> <pattern> <seconds>
  local i=0
  while [ "$i" -lt "$3" ]; do
    grep -q "$2" "$1" 2>/dev/null && return 0
    i=$((i + 1)); sleep 1
  done
  return 1
}

# grep -c prints 0 and exits 1 when nothing matches, so `|| echo 0` would append
# a second line and every later [ -gt ] would die on "integer expression expected".
count_matches() { # <pattern> <file>
  local n
  n=$(grep -c "$1" "$2" 2>/dev/null)
  printf '%s' "${n:-0}"
}
