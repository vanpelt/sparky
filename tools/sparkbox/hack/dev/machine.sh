#!/usr/bin/env bash
# Get the Apple container machine into the shape `sparkbox devpod` needs, and
# say precisely what is wrong when it is not.
#
# WHY THIS EXISTS. On a Mac the node Pod cannot run on the Mac: it needs
# /dev/kvm, /dev/net/tun, cgroup v2 and a reflink-capable filesystem, none of
# which macOS has. All of that lives one level down, inside the Linux VM Apple's
# `container machine` boots. Everything about that VM that the dev loop depends
# on — the Docker data-root on the XFS volume, the insecure-registries entry
# that lets it pull from the Mac — was configured by hand this session and is
# invisible from the outside. A hand-configured prerequisite is a prerequisite
# that silently drifts, so it is checked and re-applied here.
#
# WHAT THIS DELIBERATELY DOES NOT DO: create, start, stop or delete a machine.
# The machine is a ~27GB one-way ratchet (Apple's disk image never shrinks) that
# also carries the outer KVM kernel and a provisioned sparkbox install. Losing
# it costs a full `sparkbox setup` and a kernel download. This script only
# ADOPTS one that already exists; if it is missing it prints the command that
# makes one and exits non-zero.
#
# HOW COMMANDS GET IN. There is no shared filesystem (`--home-mount none`) and
# no `container machine cp`. The only transport is a script on stdin:
#
#   container machine run -i --root --name <machine> -- bash -s < script.sh
#
# `-i` is MANDATORY and is the single most expensive thing to get wrong here:
# without it stdin is discarded silently, bash reads EOF, and the command exits
# 0 having run NOTHING. A green run that did nothing is worse than a red one.
# Payloads at or above ~192 KiB deadlock, so the guest scripts below stay small.
#
# Usage:
#   hack/dev/machine.sh ensure    check + repair (idempotent); exits non-zero on
#                                 anything it cannot fix by itself
#   hack/dev/machine.sh status    the same checks, read-only, plus disk
#   hack/dev/machine.sh shell     interactive root shell in the machine
#
# Env:
#   SPARKBOX_DEV_MACHINE        machine name           (default sparkbox)
#   SPARKBOX_DEV_HOST_IP        the Mac, from inside   (default 192.168.64.1)
#   SPARKBOX_DEV_REGISTRY_PORT  registry port          (default 5001)
#   SPARKBOX_DEV_DATA_DIR       reflink data volume    (default /srv/sparkbox/data)
set -euo pipefail

readonly script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
readonly module_dir="$(cd "$script_dir/../.." && pwd)"
readonly machine="${SPARKBOX_DEV_MACHINE:-sparkbox}"
readonly host_ip="${SPARKBOX_DEV_HOST_IP:-192.168.64.1}"
readonly reg_port="${SPARKBOX_DEV_REGISTRY_PORT:-5001}"
readonly data_dir="${SPARKBOX_DEV_DATA_DIR:-/srv/sparkbox/data}"

die() {
  echo "machine.sh: $*" >&2
  exit 1
}

need_container_cli() {
  command -v container > /dev/null 2>&1 ||
    die "no \`container\` on PATH — Apple's container CLI is not installed
     fix: https://github.com/apple/container/releases, or \`brew install --cask container\`"
}

# Existence and state, read-only. `container machine ls --format json` emits one
# object per machine with "id" and "status"; python3 reads it rather than a grep
# that would match a machine whose name is a prefix of another's.
machine_status() {
  local json
  json=$(container machine ls --format json 2>/dev/null) || {
    echo unknown
    return
  }
  if command -v python3 > /dev/null 2>&1; then
    printf '%s' "$json" | python3 -c '
import json, sys
want = sys.argv[1]
try:
    rows = json.load(sys.stdin)
except Exception:
    print("unknown"); raise SystemExit
for row in rows:
    if row.get("id") == want:
        print(row.get("status") or "unknown"); break
else:
    print("missing")
' "$machine"
    return
  fi
  # No python3: fall back to the table. The column count varies (the default
  # machine carries a trailing "*"), so the state is found by matching the known
  # state words rather than by counting fields.
  container machine ls 2>/dev/null | awk -v want="$machine" '
    $1 == want { for (i = 2; i <= NF; i++) if ($i ~ /^(running|stopped|stopping|starting)$/) { print $i; found = 1 } }
    END { if (!found) print "missing" }'
}

require_running() {
  local state
  state=$(machine_status)
  case "$state" in
    running) return 0 ;;
    missing)
      die "no container machine named '$machine'
     This script never creates one — the machine carries the outer KVM kernel and
     a provisioned sparkbox, and making it is \`sparkbox setup\`'s job:
       sparkbox setup --machine-name $machine
     (see docs/getting-started.md; \`sparkbox setup --dry-run\` says whether this
     Mac can host one before it commits to anything)"
      ;;
    unknown)
      die "could not read \`container machine ls\` — is the container CLI healthy?"
      ;;
    *)
      die "container machine '$machine' is $state, not running
     fix: container machine start $machine"
      ;;
  esac
}

# The one transport. See the header on why -i is not optional.
mrun() {
  container machine run -i --root --name "$machine" -- bash -s
}

# The guest-side check/repair pass, generated with the host's values baked into
# a short preamble so the body can be a quoted heredoc (no escaping games).
# mode=ensure may write; mode=status never does.
guest_script() {
  cat << EOF
mode='$1'
host_ip='$host_ip'
reg_port='$reg_port'
data_dir='$data_dir'
EOF
  cat << 'GUEST'
set -uo pipefail
fail=0
changed=0
ok()  { printf 'PASS  %-20s %s\n' "$1" "$2"; }
bad() { printf 'FAIL  %-20s %s\n' "$1" "$2"; fail=1; }

# --- devices ----------------------------------------------------------------
# Firecracker needs both. They exist only because the machine boots the custom
# KVM-capable outer kernel; a machine created without --virtualization has
# neither, and nothing downstream can work around that.
for dev in /dev/kvm /dev/net/tun; do
  if [ -c "$dev" ]; then
    ok "$dev" "$(stat -c '%A %U:%G' "$dev" 2>/dev/null || echo present)"
  else
    bad "$dev" "missing — this machine did not boot the KVM outer kernel"
  fi
done

# --- network device types ---------------------------------------------------
# The outer kernel is built with NO loadable modules (/lib/modules is empty,
# there is no modprobe), so a device type either compiled in or does not exist.
# deploy/sparkbox-net.sh creates `sparkdns` and the edge IP as `type dummy`, and
# Apple's base config leaves CONFIG_DUMMY unset — so on a kernel built before
# macos/kernel/sparkbox-arm64.fragment added it, vmm-helper dies at startup with
# iproute2's bare `Error: Unknown device type.` before it opens its socket.
# sluice then cannot bind its resolver, the node cannot reach the helper socket,
# and three containers exit within seconds of each other. Nothing in that
# cascade names the kernel.
#
# Probing is better than reading a config the running kernel may not even
# expose: create one, delete it, report what happened.
for type in dummy bridge veth; do
  probe="sbprobe$$"
  case "$type" in
    veth) add_args="peer name ${probe}b" ;;
    *)    add_args="" ;;
  esac
  if ip link add "$probe" type "$type" $add_args > /dev/null 2>&1; then
    ip link del "$probe" > /dev/null 2>&1
    ok "netdev $type" "supported"
  else
    bad "netdev $type" "this kernel cannot create '$type' links — rebuild it: macos/kernel/build.sh"
  fi
done

if [ "$(stat -f -c %T /sys/fs/cgroup 2>/dev/null)" = cgroup2fs ]; then
  ok cgroup "v2 unified"
else
  bad cgroup "/sys/fs/cgroup is not cgroup2fs; the Pod's resource limits will not apply"
fi

# --- transparent huge pages -------------------------------------------------
# The single largest performance factor in this environment, by two orders of
# magnitude. Guest RAM is anonymous memory Firecracker mmaps, and every page the
# guest touches for the first time faults through TWO stage-2 layers: ours, and
# Apple's underneath it. Measured here, that costs ~300us per fault — about
# 3,300 faults/sec, or ~13MB/s of first-touch memory, against 4,600MB/s for the
# very same allocation made directly in this machine. A 350x penalty.
#
# Nothing about it looks like a memory problem from the outside. Boot is what
# touches most of guest RAM (the kernel initialises a struct page for all of
# it), so the symptom is that BOOT scales with mem_size_mib and everything else
# looks normal: userspace compute in the booted guest runs at full speed. The
# measured cliff, same kernel and rootfs, time to /init:
#
#     512MB 1.43s | 1024MB 1.42s | 2048MB 25.74s | 4026MB 34.08s
#
# THP collapses 512 of those faults into one. With enabled=always the same
# 4026MB guest reaches init in 0.23s instead of 29.82s -- and that is the whole
# difference between a sandbox that boots and one that trips its 90s systemd
# timeouts and looks, from the host, like a VM pegging four cores forever.
#
# Apple's machine image ships `madvise`, which is the trap: Firecracker does not
# madvise its guest memory, so `madvise` means no THP at all here. We do NOT use
# Firecracker's own huge_pages="2M" instead -- that is MAP_HUGETLB, needs pages
# reserved up front, and is incompatible with the balloon device.
thp=/sys/kernel/mm/transparent_hugepage/enabled
thp_was=$(sed -n 's/.*\[\([a-z]*\)\].*/\1/p' "$thp" 2>/dev/null)
if [ -z "$thp_was" ]; then
  bad thp "no THP support in this kernel — guest boots will be ~100x slower"
elif [ "$thp_was" = always ]; then
  ok thp "always"
elif [ "$mode" != ensure ]; then
  # status reports, never repairs.
  bad thp "$thp_was, not always — guest boots will be ~100x slower; fix: machine.sh ensure"
# deliberately NOT `changed=1`: that flag means "docker's config moved, restart
# the daemon", and a restart kills the node Pod and every sandbox on it. THP is
# a live sysctl -- it takes effect on the next guest boot with nothing
# restarted.
elif echo always > "$thp" 2>/dev/null; then
  ok thp "was $thp_was, set to always"
else
  bad thp "could not set $thp to 'always' — guest boots will take ~30s instead of ~0.2s"
fi

# --- the data volume --------------------------------------------------------
fstype=$(findmnt -no FSTYPE "$data_dir" 2>/dev/null || true)
if [ -n "$fstype" ]; then
  ok "$data_dir" "mounted, $fstype, $(df -h --output=avail "$data_dir" | tail -n 1 | tr -d ' ') free"
else
  bad "$data_dir" "not a mount point — the XFS loop volume is not attached"
fi

# Reflink is TESTED, never assumed: it is a property of the filesystem under the
# path, and the two filesystems in this machine disagree. / is ext4 and
# cp --reflink=always FAILS there; $data_dir is XFS and it works (measured, both
# from a container and from inside a pod). Rootfs clones silently degrade to
# full copies when this is wrong, which shows up as a slow boot, not an error.
if [ -d "$data_dir" ]; then
  probe=$(mktemp -d "$data_dir/.reflink-probe.XXXXXX" 2>/dev/null || true)
  if [ -n "$probe" ]; then
    head -c 65536 /dev/urandom > "$probe/src" 2>/dev/null
    if cp --reflink=always "$probe/src" "$probe/dst" 2>/dev/null; then
      ok reflink "cp --reflink=always works on ${fstype:-?}"
    else
      bad reflink "cp --reflink=always FAILED on $data_dir (${fstype:-?}) — rootfs clones
                       would fall back to full copies"
    fi
    rm -rf "$probe"
  else
    bad reflink "could not create a probe directory under $data_dir"
  fi
fi

# --- docker -----------------------------------------------------------------
if ! command -v docker > /dev/null 2>&1; then
  bad docker "not installed in the machine — the dev pod has no runtime to run in"
else
  if [ "$mode" = ensure ]; then
    # MERGE, never clobber: this file is also where an operator would put
    # anything else the daemon needs, and a rewrite that dropped it would be
    # invisible until the next restart. A non-JSON file is refused outright
    # rather than replaced.
    merge=$(python3 - "$data_dir/docker" "$host_ip:$reg_port" << 'PY'
import json, os, sys, tempfile

path = "/etc/docker/daemon.json"
root, registry = sys.argv[1], sys.argv[2]
try:
    with open(path) as fh:
        cfg = json.load(fh)
except FileNotFoundError:
    cfg = {}
except ValueError as exc:
    print("INVALID %s" % exc)
    raise SystemExit(0)
if not isinstance(cfg, dict):
    print("INVALID top-level value is not an object")
    raise SystemExit(0)

before = json.dumps(cfg, sort_keys=True)
cfg["data-root"] = root
registries = list(cfg.get("insecure-registries") or [])
if registry not in registries:
    registries.append(registry)
cfg["insecure-registries"] = registries
after = json.dumps(cfg, sort_keys=True)
if before == after:
    print("UNCHANGED")
    raise SystemExit(0)

fd, tmp = tempfile.mkstemp(dir=os.path.dirname(path))
with os.fdopen(fd, "w") as fh:
    json.dump(cfg, fh, indent=2)
    fh.write("\n")
os.chmod(tmp, 0o644)
os.replace(tmp, path)
print("CHANGED")
PY
    ) || merge="INVALID python3 failed"
    case "$merge" in
      UNCHANGED) ok daemon.json "already has data-root and $host_ip:$reg_port" ;;
      CHANGED)
        changed=1
        ok daemon.json "merged data-root=$data_dir/docker and insecure-registries += $host_ip:$reg_port"
        ;;
      INVALID*)
        bad daemon.json "refusing to touch /etc/docker/daemon.json: ${merge#INVALID }
                       fix it by hand; nothing was written"
        ;;
      *) bad daemon.json "unexpected merge result: $merge" ;;
    esac
    # Restart ONLY when the file actually changed. A docker restart kills every
    # running container, the dev pod included, so doing it unconditionally would
    # make `ensure` unsafe to run mid-session — which is the one thing an
    # idempotent command must not be.
    if [ "$changed" = 1 ]; then
      systemctl restart docker || bad docker "systemctl restart docker failed"
      for _ in $(seq 1 30); do
        docker info > /dev/null 2>&1 && break
        sleep 1
      done
    fi
  else
    if grep -q '"data-root"' /etc/docker/daemon.json 2>/dev/null &&
      grep -q "$host_ip:$reg_port" /etc/docker/daemon.json 2>/dev/null; then
      ok daemon.json "has data-root and $host_ip:$reg_port"
    else
      bad daemon.json "missing data-root or insecure-registries $host_ip:$reg_port
                       fix: hack/dev/machine.sh ensure"
    fi
  fi

  if [ "$(systemctl is-active docker 2>/dev/null)" = active ] && docker info > /dev/null 2>&1; then
    ok dockerd "$(docker version --format '{{.Server.Version}}' 2>/dev/null)"
  else
    bad dockerd "not running
                       fix: container machine run -i --root --name <m> -- systemctl start docker"
  fi

  root=$(docker info -f '{{.DockerRootDir}}' 2>/dev/null || true)
  case "$root" in
    "$data_dir"/*) ok data-root "$root" ;;
    "") bad data-root "docker info did not answer" ;;
    *) bad data-root "$root is not under $data_dir — images land on the non-reflink rootfs
                       fix: hack/dev/machine.sh ensure (then re-pull images)" ;;
  esac
fi

# --- the Mac's registry, from in here ---------------------------------------
# This is the leg that actually carries images. The Mac reaching 127.0.0.1:5001
# says nothing about whether the machine can reach 192.168.64.1:5001.
if curl -fsS -m 5 -o /dev/null "http://$host_ip:$reg_port/v2/" 2>/dev/null; then
  ok registry "http://$host_ip:$reg_port/v2/ answers"
else
  bad registry "cannot reach http://$host_ip:$reg_port/v2/ from inside the machine
                       fix: hack/dev/registry.sh up   (on the Mac)"
fi

if [ "$mode" != ensure ]; then
  echo
  echo "disk (Apple's machine image never shrinks — this only ratchets up)"
  df -h / "$data_dir" 2>/dev/null | sed 's/^/  /'
fi

echo "EXIT $fail"
exit "$fail"
GUEST
}

# `container machine run`'s own exit status has not been relied on here: the
# guest prints a trailing "EXIT n" line and that is what decides, so a transport
# that swallows the status cannot turn a red run green.
run_guest() {
  local mode="$1" tmp rc
  tmp=$(mktemp -t sparkbox-machine)
  # Streamed rather than captured: `ensure` can spend 30s waiting for a
  # restarted dockerd, and a silent script is indistinguishable from a hung one.
  guest_script "$mode" | mrun 2>&1 | tee "$tmp" | grep -v '^EXIT ' || true
  rc=$(awk '/^EXIT /{print $2}' "$tmp" | tail -n 1)
  rm -f "$tmp"
  [ -n "$rc" ] ||
    die "the guest script produced no result line — did \`container machine run\` get -i?
     (without -i stdin is discarded and bash exits 0 having run nothing)"
  return "$rc"
}

# The machine stores an ABSOLUTE path to the outer kernel, chosen when it was
# created (macos/poc.sh passes --kernel "$OUT_DIR/vmlinux-kvm"). Create the
# machine from a git worktree and it is pinned to that worktree forever — and
# macos/out/ is gitignored, so the kernel does not travel with the branch.
#
# This is not hypothetical. A machine here was pinned to a worktree that had
# since been DELETED. It ran for thirteen hours because the image was already
# loaded, and died the first time it was stopped:
#
#   kernel binary not found at '.../.claude/worktrees/<gone>/macos/out/vmlinux-kvm'
#
# Worse than the outage: re-pointing it at this checkout silently downgraded the
# kernel by six weeks, because the built artifact here predated the fragment
# that adds CONFIG_DUMMY — see the netdev probe in the guest script.
check_kernel_path() {
  local configured local_kernel
  configured=$(container machine inspect "$machine" 2>/dev/null |
    sed -n 's/.*"kernel"[^"]*"\([^"]*\)".*/\1/p' | head -1)
  local_kernel="$module_dir/macos/out/vmlinux-kvm"

  if [ -z "$configured" ]; then
    return 0 # this container CLI does not report it; nothing to check against
  fi
  if [ ! -f "$configured" ]; then
    die "$machine boots a kernel that no longer exists:
       $configured
     It was created from a directory that has since been removed (a git worktree,
     most likely). The machine keeps running until it is stopped, and then cannot
     start again.
     fix: build this checkout's kernel and re-point the machine at it —
       macos/kernel/build.sh
       container machine set -n $machine kernel=$local_kernel"
  fi
  case "$configured" in
    "$local_kernel") ;;
    *"/.claude/worktrees/"*)
      note_stale "$machine boots $configured
     That is inside a git worktree. macos/out/ is gitignored, so deleting the
     worktree strands this machine. Re-point it at $local_kernel once built."
      ;;
    *)
      note_stale "$machine boots $configured, not this checkout's
       $local_kernel
     That is legal — but the two can drift, and a kernel older than
     macos/kernel/sparkbox-arm64.fragment fails in ways that look like sparkbox bugs."
      ;;
  esac
}

note_stale() { printf 'WARN  %s\n' "$*" >&2; }

# Is the built kernel older than the config it claims to be built from?
#
# macos/out/ is gitignored and macos/kernel/sparkbox-arm64.fragment is not, so
# the two drift silently: pull a branch that changes the fragment and your
# kernel is simply wrong, with no signal anywhere. The build already records
# fragment_sha256 in its manifest, so the comparison costs one shasum.
#
# A WARN rather than a failure: a fragment change need not break anything, and
# the things that DO break have functional probes in the guest script that name
# themselves. This is the one that explains them.
check_kernel_freshness() {
  local manifest=$module_dir/macos/out/kernel-manifest.txt
  local fragment=$module_dir/macos/kernel/sparkbox-arm64.fragment
  [ -f "$manifest" ] && [ -f "$fragment" ] || return 0
  command -v shasum > /dev/null 2>&1 || return 0

  local built want
  built=$(sed -n 's/^fragment_sha256=//p' "$manifest" | head -1)
  want=$(shasum -a 256 "$fragment" | awk '{print $1}')
  [ -n "$built" ] || return 0
  [ "$built" = "$want" ] && return 0

  note_stale "the built kernel does not match macos/kernel/sparkbox-arm64.fragment
     built from: $built
     fragment is: $want
     The kernel in macos/out/ predates the config checked into this branch, and
     macos/out/ is gitignored so it did not travel with it.
     fix: macos/kernel/build.sh"
}

cmd_ensure() {
  need_container_cli
  require_running
  check_kernel_path
  check_kernel_freshness
  echo "== ensure $machine =="
  if run_guest ensure; then
    echo "machine ready"
  else
    die "$machine is not ready for the dev pod; see the FAIL lines above"
  fi
}

cmd_status() {
  need_container_cli
  local state
  state=$(machine_status)
  echo "machine   $machine  $state"
  if [ "$state" != running ]; then
    if [ "$state" = missing ]; then
      echo "  fix:    sparkbox setup --machine-name $machine"
    else
      echo "  fix:    container machine start $machine"
    fi
    return 1
  fi
  container machine ls 2>/dev/null | sed -n '1p;/^'"$machine"' /p' | sed 's/^/  /'
  echo
  check_kernel_freshness
  run_guest status
}

cmd_shell() {
  need_container_cli
  require_running
  exec container machine run -it --root --name "$machine" -- bash -l
}

usage() {
  echo "usage: hack/dev/machine.sh ensure|status|shell" >&2
}

case "${1:-}" in
  ensure) cmd_ensure ;;
  status) cmd_status ;;
  shell) cmd_shell ;;
  *)
    usage
    exit 2
    ;;
esac
