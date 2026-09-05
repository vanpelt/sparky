#!/usr/bin/env bash
# The question the whole Cloud Hypervisor port now rests on:
#
#   Does a Cloud Hypervisor snapshot bring back a WORKING inner VM?
#
# Section 11 established that nested virtualization already works on the
# Firecracker we ship -- one guest-kernel option, no VMM change -- and that the
# only thing Firecracker cannot do is carry it through a snapshot. It returns
# HTTP 204, writes a normal-looking snapshot, and the restored guest's kernel
# BUG()s on the resume path.
#
# Cloud Hypervisor issues KVM_GET_NESTED_STATE, which Firecracker never does.
# But issuing the ioctl is not the same as restoring a guest that runs, and
# nobody had tested it. If the answer here is no, the port buys nothing that
# matters and the recommendation collapses to "do not do it".
#
# Same L1 and same L2 as pause-test.sh, from lib-guests.sh, so the outer VMM is
# the only variable. The inner VMM stays Firecracker in both.
#
# Run inside-pod.sh first: this reuses /work/out/{vmlinux,firecracker}.
set -uo pipefail

WORK=${WORK:-/work}
out="$WORK/out"
CH_VER=${CH_VER:-v53.0}
L1_MEM=${L1_MEM:-3072}
L2_MEM=${L2_MEM:-512}
WAIT=${WAIT:-60}

here=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
# shellcheck source=lib-guests.sh
. "$here/lib-guests.sh"

step()   { printf '\n\033[1m== %s\033[0m\n' "$1"; }
marker() { printf 'M0B_%s=%s\n' "$1" "$2"; }

# --- 1. cloud-hypervisor ------------------------------------------------------
step "1. cloud-hypervisor $CH_VER"
base="https://github.com/cloud-hypervisor/cloud-hypervisor/releases/download/$CH_VER"
for bin in cloud-hypervisor ch-remote; do
  if [ ! -x "$out/$bin" ]; then
    curl -fsSL "$base/${bin}-static" -o "$out/$bin" || { marker CH_FETCH failed; exit 1; }
    chmod +x "$out/$bin"
  fi
done
"$out/cloud-hypervisor" --version | head -1
marker CH_VERSION "$("$out/cloud-hypervisor" --version | head -1 | tr ' ' '_')"

# v52.0 is the floor: below it `nested=off` is a silent no-op on AMD and a parse
# error on arm64. We are on Intel with nested=on, but check the option exists at
# all rather than discovering it was ignored.
if "$out/cloud-hypervisor" --help 2>&1 | grep -q 'nested'; then
  marker CH_NESTED_OPT present
else
  marker CH_NESTED_OPT absent
  echo "this build does not document --cpus nested=; results below are meaningless" >&2
fi

# Cloud Hypervisor boots x86_64 via PVH, which needs the ELF note CONFIG_PVH=y
# emits. The firecracker CI config sets it, but assert rather than assume --
# without it the failure looks like a mysterious hang.
kconfig="$WORK/kbuild/linux-${KVER:-6.1.155}/.config"
[ -r "$kconfig" ] && marker CONFIG_PVH "$(grep -E '^CONFIG_PVH=' "$kconfig" | cut -d= -f2 || echo unset)"

# --- 2. the guests ------------------------------------------------------------
step "2. guests (same images pause-test.sh used)"
build_tick_guests || exit 1
ls -lh "$out"/p1-initrd.gz "$out"/p2-initrd.gz | awk '{print "    " $5, $9}'

# --- 3. boot L1 under Cloud Hypervisor with nested=on -------------------------
step "3. boot L1 under cloud-hypervisor --cpus nested=on"
rm -f "$out/ch.sock" "$out/ch2.sock"
rm -rf "$out/chsnap" && mkdir -p "$out/chsnap"   # snapshot dir must pre-exist
"$out/cloud-hypervisor" \
  --api-socket "$out/ch.sock" \
  --cpus boot=2,nested=on \
  --memory size=${L1_MEM}M \
  --kernel "$out/vmlinux" \
  --initramfs "$out/p1-initrd.gz" \
  --cmdline "console=ttyS0 reboot=k panic=1 pci=off" \
  --serial tty --console off \
  >"$out/ch-l1.log" 2>&1 &
ch_pid=$!

if await "$out/ch-l1.log" 'M0B_P_L1_ALIVE=yes' "$WAIT"; then
  marker CH_L1_BOOTED yes
else
  marker CH_L1_BOOTED no
  echo "L1 never reached userspace under cloud-hypervisor. Console:" >&2
  tail -40 "$out/ch-l1.log" >&2
  kill "$ch_pid" 2>/dev/null; exit 1
fi
grep -m1 'M0B_P_L1_VMX=' "$out/ch-l1.log" | sed 's/^/    /'
grep -m1 'M0B_P_L1_DEVKVM=' "$out/ch-l1.log" | sed 's/^/    /'

if await "$out/ch-l1.log" 'M0B_P_L2_ALIVE=yes' "$WAIT"; then
  marker CH_L2_BOOTED yes
else
  marker CH_L2_BOOTED no
  echo "the inner VM never came up under cloud-hypervisor -- nothing to snapshot." >&2
  echo "That is itself a result: nested=on did not deliver a usable /dev/kvm." >&2
  tail -40 "$out/ch-l1.log" >&2
  kill "$ch_pid" 2>/dev/null; exit 1
fi
marker CH_L2_TICKS_BEFORE "$(count_matches 'M0B_P_L2_TICK=' "$out/ch-l1.log")"

# --- 4. pause + snapshot ------------------------------------------------------
step "4. pause and snapshot with the L2 live"
pause_out=$("$out/ch-remote" --api-socket "$out/ch.sock" pause 2>&1); pause_rc=$?
printf '%s\n' "$pause_out" | sed 's/^/    /'
marker CH_PAUSE_RC "$pause_rc"

snap_out=$("$out/ch-remote" --api-socket "$out/ch.sock" snapshot "file://$out/chsnap" 2>&1); snap_rc=$?
printf '%s\n' "$snap_out" | sed 's/^/    /'
marker CH_SNAPSHOT_RC "$snap_rc"
if [ "$snap_rc" -ne 0 ]; then
  marker CH_RESULT snapshot_refused
  echo "Cloud Hypervisor refused to snapshot a VM with a live inner guest."
  echo "Loud beats silent -- but it means the port does not buy pausable"
  echo "nested guests either, which was its entire remaining justification."
  kill "$ch_pid" 2>/dev/null
  exit 0
fi
ls -l "$out/chsnap" | awk 'NR>1 {print "    " $5, $9}'
kill "$ch_pid" 2>/dev/null; wait "$ch_pid" 2>/dev/null

# --- 5. restore ---------------------------------------------------------------
# --restore is a disjoint command line: passing --kernel alongside it silently
# takes the cold-boot branch and loses guest memory, which would look like a
# successful restore of a VM that lost its L2. Nothing but --api-socket and
# --restore here, on purpose.
step "5. restore and see whether the L2 is still there"
"$out/cloud-hypervisor" \
  --api-socket "$out/ch2.sock" \
  --restore "source_url=file://$out/chsnap,resume=true" \
  >"$out/ch-l1-restored.log" 2>&1 &
ch2_pid=$!

# Both guests tick once a second. 20s is unambiguous either way.
sleep 20
kill "$ch2_pid" 2>/dev/null; wait "$ch2_pid" 2>/dev/null
l1_after=$(count_matches 'M0B_P_L1_TICK=' "$out/ch-l1-restored.log")
l2_after=$(count_matches 'M0B_P_L2_TICK=' "$out/ch-l1-restored.log")
marker CH_L1_TICKS_AFTER "$l1_after"
marker CH_L2_TICKS_AFTER "$l2_after"
echo "--- restored console (tail) ---"
tail -25 "$out/ch-l1-restored.log" | sed 's/^/    /'

# The Firecracker failure signature, for direct comparison.
if grep -q 'kvm_spurious_fault\|kernel BUG at arch/x86/kvm' "$out/ch-l1-restored.log"; then
  marker CH_GUEST_KVM_BUG yes
else
  marker CH_GUEST_KVM_BUG no
fi

# --- 6. control: is nested=on actually load-bearing? --------------------------
# Without this the experiment proves less than it looks like it proves. On
# Firecracker -- which has no nested switch at all -- the VMX bit reached the
# guest anyway, so "we passed nested=on and it worked" does not by itself show
# the flag did anything. It also tests the property section 7's security
# argument depends on: that nested=off is a real per-sandbox gate rather than a
# request the VMM may ignore.
step "6. control: the same guest with --cpus nested=off"
rm -f "$out/ch3.sock"
timeout 60 "$out/cloud-hypervisor" \
  --api-socket "$out/ch3.sock" \
  --cpus boot=2,nested=off \
  --memory size=1024M \
  --kernel "$out/vmlinux" \
  --initramfs "$out/p1-initrd.gz" \
  --cmdline "console=ttyS0 reboot=k panic=1 pci=off" \
  --serial tty --console off \
  >"$out/ch-nestedoff.log" 2>&1 &
ctl_pid=$!
await "$out/ch-nestedoff.log" 'M0B_P_L1_ALIVE=yes' 40
sleep 10
kill "$ctl_pid" 2>/dev/null; wait "$ctl_pid" 2>/dev/null
off_vmx=$(sed -n 's/.*M0B_P_L1_VMX=\([a-z]*\).*/\1/p' "$out/ch-nestedoff.log" | head -1)
off_kvm=$(sed -n 's/.*M0B_P_L1_DEVKVM=\([a-z]*\).*/\1/p' "$out/ch-nestedoff.log" | head -1)
marker CH_OFF_L1_VMX    "${off_vmx:-unknown}"
marker CH_OFF_L1_DEVKVM "${off_kvm:-unknown}"
marker CH_OFF_L2_TICKS  "$(count_matches 'M0B_P_L2_TICK=' "$out/ch-nestedoff.log")"
marker CH_OFF_L1_TICKS  "$(count_matches 'M0B_P_L1_TICK=' "$out/ch-nestedoff.log")"
if [ "$off_vmx" = no ] && [ "$off_kvm" = no ]; then
  marker CH_CONTROL ok
  echo "    nested=off masked VMX and the inner VM could not start -- the switch"
  echo "    is a real per-sandbox gate, which is what section 7 needs."
else
  marker CH_CONTROL suspect
  echo "    nested=off did NOT mask VMX. Treat the nested=on result with care and"
  echo "    do not rely on nested=off as a security boundary until this is understood."
fi

step "verdict"
if [ "$l1_after" -gt 0 ] && [ "$l2_after" -gt 0 ]; then
  marker CH_RESULT both_survived
  echo "Cloud Hypervisor carried the inner VM through snapshot/restore. This is"
  echo "the property the port exists to buy, and it is now measured rather than"
  echo "assumed from the fact that it calls KVM_GET_NESTED_STATE."
elif [ "$l1_after" -gt 0 ]; then
  marker CH_RESULT l1_survived_l2_lost
  echo "Same outcome as Firecracker: the sandbox came back, the inner VM did not."
  echo "If this is the result, the port buys nothing that matters -- calling"
  echo "KVM_GET_NESTED_STATE is evidently not sufficient -- and the honest"
  echo "recommendation becomes 'do not port; make nested sandboxes non-pausable'."
else
  marker CH_RESULT restore_dead
  echo "Nothing came back at all. Read the console above before quoting this:"
  echo "a restore that fails outright is a harness or version problem at least"
  echo "as often as it is a real limitation."
fi
