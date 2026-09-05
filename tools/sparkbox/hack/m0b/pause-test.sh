#!/usr/bin/env bash
# M0b step 4: does a Firecracker snapshot survive a running inner VM?
#
# inside-pod.sh answered the capability question -- an L2 boots. This answers the
# one that decides whether the Cloud Hypervisor port is worth doing, because it
# is the only remaining thing Firecracker cannot do.
#
# The claim, from source: Firecracker never issues KVM_GET_NESTED_STATE and its
# serialised MSR list omits the VMX capability MSRs 0x480-0x491, so a snapshot
# taken while an L2 is live cannot contain the L2. Sparkbox pauses sandboxes
# constantly -- scale-to-zero is the whole idle model -- so if that is true,
# "nested works on Firecracker" comes with a caveat that eats most of its value.
#
# Until now that has been an argument from reading upstream. This observes it.
#
# Run inside-pod.sh first: this reuses /work/out/{vmlinux,firecracker} and does
# not rebuild anything. Unlike inside-pod.sh both guests are long-lived and emit
# a per-second tick, so "did the inner VM survive" is answerable by reading the
# console rather than inferred.
set -uo pipefail

WORK=${WORK:-/work}
out="$WORK/out"
L1_MEM=${L1_MEM:-3072}
L2_MEM=${L2_MEM:-512}
WAIT=${WAIT:-45}

here=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
# shellcheck source=lib-guests.sh
. "$here/lib-guests.sh"

step()   { printf '\n\033[1m== %s\033[0m\n' "$1"; }
marker() { printf 'M0B_%s=%s\n' "$1" "$2"; }
api()    { # <method> <path> [body]
  curl -sS --unix-socket "$out/fc.sock" -X "$1" "http://localhost$2" \
    -H 'Accept: application/json' -H 'Content-Type: application/json' \
    ${3:+-d "$3"} -w '\nHTTP:%{http_code}\n' 2>&1
}

# --- 1. long-lived guests -----------------------------------------------------
# From lib-guests.sh, so ch-snapshot-test.sh boots byte-identical images and the
# outer VMM is the only thing that differs between the two experiments.
step "1. initramfs (both guests tick once a second)"
build_tick_guests || exit 1

# --- 2. boot L1 under the API so it can be paused ------------------------------
step "2. boot L1 with an API socket"
rm -f "$out/fc.sock"; rm -rf "$out/snap"; mkdir -p "$out/snap"
cat >"$out/p1.json" <<P1CFG
{
  "boot-source": {
    "kernel_image_path": "$out/vmlinux",
    "initrd_path": "$out/p1-initrd.gz",
    "boot_args": "console=ttyS0 reboot=k panic=1 pci=off"
  },
  "drives": [],
  "machine-config": { "vcpu_count": 2, "mem_size_mib": $L1_MEM, "smt": false }
}
P1CFG
"$out/firecracker" --api-sock "$out/fc.sock" --config-file "$out/p1.json" \
  >"$out/pause-l1.log" 2>&1 &
fc_pid=$!

if await "$out/pause-l1.log" 'M0B_P_L2_ALIVE=yes' "$WAIT"; then
  marker P_L2_BOOTED yes
else
  marker P_L2_BOOTED no
  echo "the inner VM never came up; nothing to test. Console:" >&2
  tail -30 "$out/pause-l1.log" >&2
  kill "$fc_pid" 2>/dev/null; exit 1
fi
marker P_L2_TICKS_BEFORE "$(count_matches 'M0B_P_L2_TICK=' "$out/pause-l1.log")"

# --- 3. pause + snapshot -------------------------------------------------------
step "3. pause, then snapshot with the L2 live"
pause_out=$(api PATCH /vm '{"state":"Paused"}')
printf '%s\n' "$pause_out" | sed 's/^/    /'
marker P_PAUSE_HTTP "$(printf '%s' "$pause_out" | sed -n 's/^HTTP://p')"

snap_out=$(api PUT /snapshot/create \
  "{\"snapshot_type\":\"Full\",\"snapshot_path\":\"$out/snap/state\",\"mem_file_path\":\"$out/snap/mem\"}")
printf '%s\n' "$snap_out" | sed 's/^/    /'
snap_code=$(printf '%s' "$snap_out" | sed -n 's/^HTTP://p')
marker P_SNAPSHOT_HTTP "$snap_code"

if [ "$snap_code" != 204 ]; then
  marker P_RESULT snapshot_refused
  echo "Firecracker refused to snapshot a VM with a live inner guest. That is the"
  echo "cleanest possible form of the limitation: it fails loudly instead of"
  echo "producing a snapshot that silently loses the L2."
  kill "$fc_pid" 2>/dev/null
  exit 0
fi
ls -lh "$out/snap" | awk 'NR>1 {print "    " $5, $9}'
kill "$fc_pid" 2>/dev/null; wait "$fc_pid" 2>/dev/null

# --- 4. restore ----------------------------------------------------------------
# The restore command line must NOT carry boot-source: a snapshot load rebuilds
# the VM from the state and memory files alone.
step "4. restore from the snapshot and see whether the L2 is still there"
rm -f "$out/fc2.sock"
"$out/firecracker" --api-sock "$out/fc2.sock" >"$out/pause-l1-restored.log" 2>&1 &
fc2_pid=$!
sleep 2
load_out=$(curl -sS --unix-socket "$out/fc2.sock" -X PUT http://localhost/snapshot/load \
  -H 'Content-Type: application/json' \
  -d "{\"snapshot_path\":\"$out/snap/state\",\"mem_backend\":{\"backend_type\":\"File\",\"backend_path\":\"$out/snap/mem\"},\"resume_vm\":true}" \
  -w '\nHTTP:%{http_code}\n' 2>&1)
printf '%s\n' "$load_out" | sed 's/^/    /'
load_code=$(printf '%s' "$load_out" | sed -n 's/^HTTP://p')
marker P_RESTORE_HTTP "$load_code"

if [ "$load_code" != 204 ]; then
  marker P_RESULT restore_refused
  kill "$fc2_pid" 2>/dev/null
  exit 0
fi

# Both guests tick once a second. After 15s a survivor has produced ticks; a
# guest that died produces none, and the difference is unambiguous.
sleep 15
kill "$fc2_pid" 2>/dev/null; wait "$fc2_pid" 2>/dev/null
l1_after=$(count_matches 'M0B_P_L1_TICK=' "$out/pause-l1-restored.log")
l2_after=$(count_matches 'M0B_P_L2_TICK=' "$out/pause-l1-restored.log")
marker P_L1_TICKS_AFTER "$l1_after"
marker P_L2_TICKS_AFTER "$l2_after"
echo "--- restored console (tail) ---"
tail -20 "$out/pause-l1-restored.log" | sed 's/^/    /'

step "verdict"
if [ "$l1_after" -gt 0 ] && [ "$l2_after" -gt 0 ]; then
  marker P_RESULT both_survived
  echo "The sandbox AND its inner VM both survived snapshot/restore. That would"
  echo "contradict the reading of upstream in the feasibility doc -- check the"
  echo "console above before believing it."
elif [ "$l1_after" -gt 0 ]; then
  marker P_RESULT l1_survived_l2_lost
  echo "The sandbox came back and the inner VM did not. This is the predicted"
  echo "result and the entire remaining case for Cloud Hypervisor: Sparkbox"
  echo "pauses sandboxes as its idle model, so nested-on-Firecracker means"
  echo "'nested until something pauses you'."
else
  marker P_RESULT restore_dead
  echo "Nothing came back. The restore itself failed rather than just losing the"
  echo "inner VM -- read the console above; this is a stronger failure than"
  echo "predicted and worth understanding before quoting it."
fi
