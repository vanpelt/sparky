#!/usr/bin/env bash
# Provision smoke test: run `sparkbox setup` against a LOCAL stub release and
# assert that what it claims to have done actually happened. This is the test
# that would have caught F0 (setup never installs the sparkbox binary) and F7
# (setup prints PASS over a crash-looping service) — see docs/onboarding-notes.md.
#
# Two phases, because they need very different hosts:
#
#   dry    `setup --dry-run`. Touches nothing, needs no root, no systemd, no
#          network, no KVM — it only calls each step's Satisfied+Plan. Safe to
#          run on a CI runner VM, a laptop, or a mac. Asserts the plan names
#          every step and that nothing was created.
#
#   full   a real `setup`. Needs root, a PID-1 systemd, and a disposable
#          filesystem, so CI runs it inside the privileged systemd container
#          built by hack/smoke-container.sh (which is also how you run it by
#          hand). Asserts the artifacts landed, the binary landed, the service
#          is LIVE across a settle window, a re-run is idempotent, and the SSH
#          control door answers.
#
# The stub release is four small files served over HTTP at
# <base>/download/<tag>/<name>, which is exactly the layout assetURL() builds
# (internal/hostsetup/manifest.go). Nothing in setup validates the *content* of
# the kernel or the rootfs — only their sha256 against the manifest — so a
# 64 KiB random blob and a 64 MiB ext4 exercise every code path a 1.5 GB real
# release would, in a couple of seconds.
#
# NO SILENT CAPS. Anything a runner genuinely cannot do is announced with a
# `SKIP:` line and counted in the summary. If you see a green run with skips,
# the skips are the honest limit of the environment, not a passing assertion.
#
# Usage:
#   SPARKBOX_BIN=/path/to/sparkbox ./hack/smoke-setup.sh dry
#   sudo SPARKBOX_BIN=/path/to/sparkbox ./hack/smoke-setup.sh full
#
# Env:
#   SPARKBOX_BIN   the binary under test (required). `full` installs THIS file.
#   RELEASE        stub release tag (default v0-smoke). Build the binary with
#                  -ldflags "-X main.version=$RELEASE" and the F0 assertion
#                  becomes the plan's literal acceptance criterion: "sparkbox
#                  version on the host matches the release".
#   BIN_PATH       where the binary must land (default /usr/local/bin/sparkbox)
#   SETTLE         liveness settle window in seconds (default 12)
#   WORK           scratch dir (default /tmp/sparkbox-smoke)
#   STUB_PORT      stub artifact server port (default 8123)

set -uo pipefail # deliberately NOT -e: we report every failure, not just the first

PHASE=${1:-dry}
SPARKBOX_BIN=${SPARKBOX_BIN:?set SPARKBOX_BIN to the sparkbox binary under test}
RELEASE=${RELEASE:-v0-smoke}
BIN_PATH=${BIN_PATH:-/usr/local/bin/sparkbox}
SETTLE=${SETTLE:-12}
WORK=${WORK:-/tmp/sparkbox-smoke}
STUB_PORT=${STUB_PORT:-8123}
DOMAIN=${DOMAIN:-smoke.invalid}
# The real host root. sparkbox-net.service hardcodes
# EnvironmentFile=/srv/sparkbox/sparkbox.env, so a `full` run that moved --root
# elsewhere would enable a unit that cannot start. The dry phase has no such
# constraint and uses a temp root precisely to prove nothing is created there.
FULL_ROOT=${FULL_ROOT:-/srv/sparkbox}
DRY_ROOT=$WORK/dry-root

ARCH=$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')
FAILURES=0
SKIPS=0

say()  { printf '\n\033[1m== %s ==\033[0m\n' "$*"; }
ok()   { printf '  \033[32mPASS\033[0m  %s\n' "$*"; }
bad()  { printf '  \033[31mFAIL\033[0m  %s\n' "$*"; FAILURES=$((FAILURES + 1)); }
skip() { printf '  \033[33mSKIP\033[0m  %s\n' "$*"; SKIPS=$((SKIPS + 1)); }
info() { printf '  ....  %s\n' "$*"; }

# assert_grep <file> <extended-regex> <description>
assert_grep() {
  if grep -Eq -- "$2" "$1"; then ok "$3"; else
    bad "$3 (no /$2/ in $1)"
  fi
}

# ---------------------------------------------------------------------------
# Does the binary under test have A1's --bin-path yet?
#
# The workflow that runs this script has to be mergeable BEFORE A1 lands, so
# every A1-specific assertion is gated on this probe and downgraded to a loud
# SKIP rather than a failure. Once A1 is in, the same script starts enforcing
# them with no edit. --bin-path is the gate because it is the flag half of
# stepInstallBinary; a binary that has one has the other.
# ---------------------------------------------------------------------------
#
# Captured into a variable and matched with `case`, NOT piped into `grep -q`:
# under `pipefail`, grep -q exits the moment it matches, the writer takes
# SIGPIPE, and the pipeline's status becomes 141 — so the probe reports "not
# found" for a flag that is right there, nondeterministically, depending on
# whether the writer finished first. It bit this script on macOS and not in the
# container.
HAVE_A1=0
SETUP_HELP=$("$SPARKBOX_BIN" setup -h 2>&1)
case "$SETUP_HELP" in *-bin-path*) HAVE_A1=1 ;; esac

# The same trick for A2 (addressable binds + the port preflight). --api-addr is
# the gate: it is the flag half of the templated addresses, and the API port is
# what the F7 negative fixture below has to occupy. Before A2 the unit hardcoded
# 127.0.0.1:8080; after it, the default is 127.0.0.1:8079, and squatting the
# wrong one would make that fixture silently prove nothing — the same class of
# mistake as F7 itself.
HAVE_A2=0
case "$SETUP_HELP" in *-api-addr*) HAVE_A2=1 ;; esac

# And the same for the sluice install step. Before it, a release published no
# sluice binary and `setup` could only point --sluice-socket at a daemon
# somebody had installed by hand; the flag's presence in --help is how this
# script tells the two worlds apart.
#
# The glob has to match the WHOLE line, not a substring: `-sluice` is a prefix
# of `-sluice-socket`, which has existed since A5, so a plain *-sluice* would
# report the install step as landed on every ref that only has the socket flag
# — exactly the false positive these guards exist to avoid. Hence the embedded
# newlines, and the leading one so a first-line match is still bounded.
#
# And still `case`, not `printf | grep -qx`: that is the SIGPIPE trap described
# above, and it bit this very line — grep -q exited on the match, printf took
# SIGPIPE, pipefail made the pipeline 141, and the probe reported "not found"
# for a flag that was right there in the help text.
HAVE_SLUICE=0
case $'\n'"$SETUP_HELP" in *$'\n  -sluice\n'*) HAVE_SLUICE=1 ;; esac
if [ "$HAVE_A2" = 1 ]; then
  API_PORT=${API_PORT:-8079}
else
  API_PORT=${API_PORT:-8080}
fi

# ---------------------------------------------------------------------------
# dry phase
# ---------------------------------------------------------------------------
dry_phase() {
  say "dry run — the plan, and that it changes nothing"
  rm -rf "$DRY_ROOT"
  mkdir -p "$WORK"
  local out=$WORK/dry.txt

  "$SPARKBOX_BIN" setup --dry-run \
    --root "$DRY_ROOT" \
    --release "$RELEASE" \
    --artifact-base "http://127.0.0.1:$STUB_PORT" \
    --operator-key "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIH0000000000000000000000000000000000000000 smoke@ci" \
    --swap-gb 0 --data-volume-gb 2 --proxy-domain "$DOMAIN" \
    >"$out" 2>&1
  local rc=$?
  cat "$out"

  # A dry run must succeed even on a host that fails preflight: preflight is
  # advisory in --dry-run (steps.go), which is what lets this phase run on a
  # hosted runner with no /dev/kvm.
  if [ $rc -eq 0 ]; then ok "setup --dry-run exit 0"; else bad "setup --dry-run exit $rc"; fi

  # Every step must be named. This list is allSteps() in dependency order; if
  # somebody adds or renames a step, this fails and they update it here — which
  # is the point, because the plan is the user-facing contract of --dry-run.
  local s
  for s in swapfile resolve-release data-volume fetch-artifacts users.conf \
           host-config net-rules systemd-units admin-ssh enable-services; do
    assert_grep "$out" "^  - $s " "plan names step '$s'"
  done

  # F0's step. Matched loosely (name OR plan text) so a reasonable rename on the
  # A1 branch does not fail the build for a cosmetic reason.
  if [ "$HAVE_A1" = 1 ]; then
    assert_grep "$out" '^  - install-binary |install .*sparkbox binary' \
      "plan names the binary-install step (F0)"
  else
    skip "binary-install step not in the plan — A1 (--bin-path / stepInstallBinary) has not landed on this ref. This is F0: setup will provision a host whose unit ExecStarts a binary nothing ever installed."
  fi

  # A2's preflight is REPORTED in a dry run, never attempted: opening a socket
  # is a mutation like any other, and on a live host probing the gateway's own
  # addresses would report a wall of phantom conflicts.
  if [ "$HAVE_A2" = 1 ]; then
    assert_grep "$out" 'port-preflight .*would probe' "plan reports the port preflight without running it"
    assert_grep "$out" "127\\.0\\.0\\.1:$API_PORT" "the plan names the control-API address it would probe"
  else
    skip "no port preflight in the plan — A2 has not landed on this ref. This is F1: a busy port is only discovered when the service fails to bind at boot."
  fi

  assert_grep "$out" 'dry run — nothing was changed\.' "dry run announces it changed nothing"

  # ...and prove it. --dry-run calls Satisfied and Plan only; if either one ever
  # mutates, this catches it.
  if [ -e "$DRY_ROOT" ]; then
    bad "--dry-run created $DRY_ROOT"
    ls -la "$DRY_ROOT"
  else
    ok "--dry-run created nothing under $DRY_ROOT"
  fi

  # The egress gateway. Two things are asserted and they are opposites, which
  # is the point: WITHOUT --sluice the plan must say out loud that sandboxes
  # are unfiltered (that is the default, and its whole danger is that it is
  # silent), and WITH it the plan must name the install and the resolver
  # address it is about to bind.
  if [ "$HAVE_SLUICE" = 1 ]; then
    assert_grep "$out" '^  - sluice .*skipped' "plan says the default installs no egress control"
    assert_grep "$out" 'whole internet' "plan says unfiltered sandboxes reach everything"

    local sout=$WORK/dry-sluice.txt
    rm -rf "$DRY_ROOT"
    "$SPARKBOX_BIN" setup --dry-run --sluice \
      --root "$DRY_ROOT" \
      --release "$RELEASE" \
      --artifact-base "http://127.0.0.1:$STUB_PORT" \
      --operator-key "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIH0000000000000000000000000000000000000000 smoke@ci" \
      --swap-gb 0 --data-volume-gb 2 --proxy-domain "$DOMAIN" \
      >"$sout" 2>&1
    local src=$?
    cat "$sout"
    if [ $src -eq 0 ]; then ok "setup --dry-run --sluice exit 0"; else bad "setup --dry-run --sluice exit $src"; fi
    assert_grep "$sout" '^  - sluice .*install sluice to' "plan names the sluice install"
    assert_grep "$sout" 'allowlist\.txt' "plan names the allowlist it would seed"
    # :53 is the likeliest busy port on any host (systemd-resolved), and the
    # unit is Restart=always — so losing that bind is a permanent loop that
    # `systemctl is-active` calls "active". The preflight has to cover it.
    assert_grep "$sout" 'sluice-dns-addr' "plan probes the address sluice's resolver would bind"
    if [ -e "$DRY_ROOT" ]; then
      bad "--dry-run --sluice created $DRY_ROOT"
    else
      ok "--dry-run --sluice created nothing"
    fi
  else
    skip "no --sluice flag — the sluice install step has not landed on this ref. This is the F2 gap: a gateway can be told to talk to an egress filter but not to install one, so every sandbox reaches the whole internet."
  fi
}

# ---------------------------------------------------------------------------
# stub release artifacts
# ---------------------------------------------------------------------------
build_stub_release() {
  say "stub release $RELEASE ($ARCH)"
  local d=$WORK/rel/download/$RELEASE
  rm -rf "$WORK/rel"
  mkdir -p "$d"

  # Nothing verifies the kernel is a kernel — checkKernel stats it and
  # firecracker.New stats it. 64 KiB of noise has the right sha and the right
  # shape.
  head -c 65536 /dev/urandom >"$d/vmlinux-$ARCH"

  # checkFirecracker runs `firecracker --version` and prints the first line.
  cat >"$d/firecracker-$ARCH" <<'EOF'
#!/bin/sh
echo "Firecracker v1.16.1"
EOF
  chmod +x "$d/firecracker-$ARCH"

  # A real (tiny) ext4, so the template is genuinely a filesystem and the
  # .zst → decompress → <name>.ext4 path in downloadVerify is exercised.
  dd if=/dev/zero of="$WORK/universal.ext4" bs=1M count=64 status=none
  mkfs.ext4 -q -F "$WORK/universal.ext4"
  zstd -q -f "$WORK/universal.ext4" -o "$d/universal-$ARCH.ext4.zst"
  rm -f "$WORK/universal.ext4"

  # The egress gateway. A stub, like firecracker above: nothing in setup runs
  # it — the install step fetches, sha-verifies, chmods and writes a unit — and
  # the eBPF data plane is unreachable from a hosted runner anyway. What this
  # DOES keep honest is the manifest: build_stub_release mirrors
  # hack/stage-artifacts.sh key for key, so an asset that ships in a real
  # release and not here would let a broken fetch path pass the smoke.
  cat >"$d/sluice-linux-$ARCH" <<'EOF'
#!/bin/sh
echo "sluice stub"
EOF
  chmod +x "$d/sluice-linux-$ARCH"

  sha() { sha256sum "$1" | cut -d' ' -f1; }
  # Mirrors hack/stage-artifacts.sh key for key. PLATFORM matters: setup
  # refuses a manifest built for another OS, and the unqualified
  # manifest-<arch>.env name means linux.
  cat >"$d/manifest-$ARCH.env" <<EOF
RELEASE=$RELEASE
ARCH=$ARCH
PLATFORM=linux
FIRECRACKER_VERSION=v1.16.1
SHA256_VMLINUX=$(sha "$d/vmlinux-$ARCH")
SHA256_FIRECRACKER=$(sha "$d/firecracker-$ARCH")
SPARKBOX_ASSET=sparkbox-linux-$ARCH
SHA256_SPARKBOX=$(sha "$SPARKBOX_BIN")
SLUICE_ASSET=sluice-linux-$ARCH
SHA256_SLUICE=$(sha "$d/sluice-linux-$ARCH")
ROOTFS_NAME=universal
ROOTFS_ASSET=universal-$ARCH.ext4.zst
SHA256_ROOTFS=$(sha "$d/universal-$ARCH.ext4.zst")
ROOTFS_LOGIN_USER=sparky
EOF
  cat "$d/manifest-$ARCH.env"

  python3 -m http.server "$STUB_PORT" --bind 127.0.0.1 --directory "$WORK/rel" \
    >"$WORK/http.log" 2>&1 &
  STUB_PID=$!
  for _ in $(seq 1 30); do
    curl -fsS "http://127.0.0.1:$STUB_PORT/download/$RELEASE/manifest-$ARCH.env" >/dev/null 2>&1 && break
    sleep 0.2
  done
  if curl -fsS "http://127.0.0.1:$STUB_PORT/download/$RELEASE/manifest-$ARCH.env" >/dev/null 2>&1; then
    ok "stub artifact server up on 127.0.0.1:$STUB_PORT"
  else
    bad "stub artifact server never came up"; cat "$WORK/http.log"
  fi
}

# ---------------------------------------------------------------------------
# environment shims, each announced
# ---------------------------------------------------------------------------
prepare_host_env() {
  say "environment shims (each one is a real limit of this host)"

  # checkVirt hard-FAILs on amd64 when /proc/cpuinfo has no vmx/svm, and
  # Provision aborts on any preflight FAIL outside --dry-run. On arm64 it passes
  # unconditionally (KVM availability is reported by the /dev/kvm check), which
  # is why CI runs this phase on an aarch64 runner.
  if [ "$ARCH" = arm64 ]; then
    info "arch arm64: checkVirt is not applicable, preflight can pass"
  elif grep -Eq 'vmx|svm' /proc/cpuinfo; then
    info "arch amd64 with vmx/svm in /proc/cpuinfo: preflight can pass"
  else
    skip "amd64 host without vmx/svm — checkVirt is a hard FAIL and Provision will abort. Run this phase on an aarch64 runner (or a host with nested virt)."
  fi

  # checkKVM stats /dev/kvm; firecracker.New stats it; the unit gates on
  # ConditionPathExists=/dev/kvm. A plain file satisfies all three. NO GUEST IS
  # EVER BOOTED in this job, so nothing is being papered over that the job could
  # otherwise have tested — but say so out loud.
  if [ -c /dev/kvm ]; then
    info "/dev/kvm is a real KVM device"
  else
    : >/dev/kvm 2>/dev/null
    if [ -e /dev/kvm ]; then
      skip "/dev/kvm is a PLACEHOLDER FILE, not a KVM device. Hosted runners have no nested virt, so this job proves PROVISIONING only — no microVM is booted, and firecracker/vmlinux/rootfs are stubs."
    else
      bad "could not create a /dev/kvm placeholder — preflight will FAIL"
    fi
  fi

  # --swap-gb 0: stepSwap dd's a 16 GiB file and swapon's it by default, which a
  # container cannot do and a runner should not.
  skip "swapfile step disabled (--swap-gb 0): a 16 GiB dd + swapon is out of scope for a runner. The step's 'disabled' branch is still exercised."

  # --move-admin-ssh: masks ssh.socket. Safe here, pointless to assert.
  skip "admin-ssh step not exercised (--move-admin-ssh unset): relocating the host's sshd has nothing to prove in a throwaway container."

  # stepDataVolume mkfs.xfs's a loop image and mounts it. That needs the host
  # kernel to have XFS. If it does not, pre-mount a tmpfs at the data dir so the
  # step reports 'mounted at …' and the REST of the pipeline still runs — but
  # say clearly that the volume itself went untested.
  if grep -qw xfs /proc/filesystems || modprobe xfs 2>/dev/null; then
    info "kernel has XFS: the data-volume step will build a real reflink volume"
  else
    mkdir -p "$FULL_ROOT/data"
    mount -t tmpfs -o size=512m tmpfs "$FULL_ROOT/data" &&
      skip "kernel has no XFS — mounted a tmpfs at $FULL_ROOT/data so the data-volume step reports 'already satisfied'. The XFS reflink volume is NOT tested on this host." ||
      bad "no XFS and could not mount a tmpfs at $FULL_ROOT/data"
  fi
}

# ---------------------------------------------------------------------------
# liveness — the F7 probe, in shell
#
# `systemctl is-active` is not a liveness signal: the unit sets Restart=always,
# RestartSec=2 and StartLimitIntervalSec=0, so a service that dies on startup
# and is restarted forever reads 'active' (or 'activating') at essentially any
# sampled instant. That is exactly how F7 happened. Liveness is a *comparison*:
# sample NRestarts and the main PID's start timestamp, wait, sample again.
# Unchanged across the window = actually running. This mirrors the probe A1 puts
# in internal/hostsetup so the smoke job does not depend on it to detect a crash
# loop it is supposed to be testing for.
# ---------------------------------------------------------------------------
svc_props() {
  systemctl show sparkbox.service --no-pager \
    -p LoadState -p ActiveState -p SubState -p NRestarts \
    -p ExecMainPID -p ExecMainStartTimestampMonotonic 2>/dev/null
}

# liveness_verdict → echoes: live | crashloop | dead | notloaded
liveness_verdict() {
  local before after
  before=$(svc_props)
  case "$(echo "$before" | grep '^LoadState=' | cut -d= -f2)" in
    loaded) ;;
    *) echo notloaded; return ;;
  esac
  sleep "$SETTLE"
  after=$(svc_props)

  local a_state b_n a_n b_t a_t
  a_state=$(echo "$after" | grep '^ActiveState=' | cut -d= -f2)
  b_n=$(echo "$before" | grep '^NRestarts=' | cut -d= -f2)
  a_n=$(echo "$after" | grep '^NRestarts=' | cut -d= -f2)
  b_t=$(echo "$before" | grep '^ExecMainStartTimestampMonotonic=' | cut -d= -f2)
  a_t=$(echo "$after" | grep '^ExecMainStartTimestampMonotonic=' | cut -d= -f2)

  # A changed start timestamp or a climbing restart counter means the process we
  # sampled first is not the process running now — it died and came back.
  if [ "${b_n:-x}" != "${a_n:-y}" ] || [ "${b_t:-x}" != "${a_t:-y}" ]; then
    echo crashloop; return
  fi
  case "$a_state" in
    active) echo live ;;
    *) echo dead ;;
  esac
}

dump_journal() {
  echo "---- journalctl -u sparkbox -n 40 ----"
  journalctl -u sparkbox.service -n 40 --no-pager 2>&1 || echo "(no journal access)"
  echo "---- systemctl show ----"
  svc_props
  echo "--------------------------------------"
}

# ---------------------------------------------------------------------------
# full phase
# ---------------------------------------------------------------------------
full_phase() {
  if [ "$(id -u)" != 0 ]; then
    bad "the full phase needs root (it provisions a host); got uid $(id -u)"
    return
  fi
  if ! command -v systemctl >/dev/null || [ ! -d /run/systemd/system ]; then
    bad "the full phase needs a PID-1 systemd; run it via hack/smoke-container.sh"
    return
  fi

  mkdir -p "$WORK"
  build_stub_release
  prepare_host_env

  say "operator key"
  rm -f "$WORK/op" "$WORK/op.pub"
  ssh-keygen -q -t ed25519 -N '' -f "$WORK/op" -C smoke@ci
  local opkey; opkey=$(cat "$WORK/op.pub")
  # Passing the key as a literal on purpose: under sudo/root the auto-detect
  # looks in /root/.ssh and finds nothing, and the failure is confusing.
  info "seeding users.conf with a literal --operator-key"

  # --------------------------------------------------------------- real setup
  say "real setup"
  local args=(setup
    --root "$FULL_ROOT"
    --release "$RELEASE"
    --artifact-base "http://127.0.0.1:$STUB_PORT"
    --operator-key "$opkey"
    --swap-gb 0 --data-volume-gb 2 --proxy-domain "$DOMAIN")
  [ "$HAVE_A1" = 1 ] && args+=(--bin-path "$BIN_PATH")

  "$SPARKBOX_BIN" "${args[@]}" >"$WORK/setup.txt" 2>&1
  local rc=$?
  cat "$WORK/setup.txt"
  if [ $rc -eq 0 ]; then ok "setup exit 0"; else bad "setup exit $rc"; fi

  # ------------------------------------------------------ what actually landed
  say "artifacts on disk"
  [ -s "$FULL_ROOT/vmlinux" ]                  && ok "kernel at $FULL_ROOT/vmlinux"            || bad "no kernel at $FULL_ROOT/vmlinux"
  [ -s "$FULL_ROOT/data/images/universal.ext4" ] && ok "rootfs template decompressed"          || bad "no $FULL_ROOT/data/images/universal.ext4"
  [ -x /usr/local/bin/firecracker ]            && ok "firecracker installed + executable"      || bad "no executable /usr/local/bin/firecracker"
  [ -s "$FULL_ROOT/users.conf" ]               && ok "users.conf seeded"                       || bad "no users.conf"
  [ -s "$FULL_ROOT/sparkbox.env" ]             && ok "sparkbox.env written"                    || bad "no sparkbox.env"
  [ -s /etc/systemd/system/sparkbox.service ]  && ok "sparkbox.service installed"              || bad "no sparkbox.service"
  [ -s /etc/systemd/system/sparkbox-net.service ] && ok "sparkbox-net.service installed"       || bad "no sparkbox-net.service"
  [ -x /usr/local/sbin/sparkbox-net.sh ]       && ok "sparkbox-net.sh installed"               || bad "no sparkbox-net.sh"

  # ------------------------------------------------------------------ F0 -----
  say "F0 — did setup install the binary it is running?"
  if [ "$HAVE_A1" = 1 ]; then
    if [ -x "$BIN_PATH" ]; then
      ok "binary landed at $BIN_PATH and is executable"
      local want have
      want=$("$SPARKBOX_BIN" version 2>&1 | awk '{print $2}')
      have=$("$BIN_PATH" version 2>&1 | awk '{print $2}')
      if [ -n "$have" ] && [ "$have" = "$want" ]; then
        ok "\`$BIN_PATH version\` = $have (same build that ran setup)"
      else
        bad "version skew: setup ran $want, $BIN_PATH reports ${have:-<nothing>}"
      fi
      # The plan's acceptance criterion is stated against the release tag, which
      # only holds when the binary was stamped with -X main.version=$RELEASE.
      if [ "$want" = "$RELEASE" ]; then
        ok "installed \`sparkbox version\` matches --release $RELEASE"
      else
        skip "binary reports version '$want', not the stub release tag '$RELEASE' — build it with -ldflags \"-X main.version=\$RELEASE\" to assert the plan's exact wording."
      fi
      if cmp -s "$SPARKBOX_BIN" "$BIN_PATH"; then
        ok "$BIN_PATH is byte-identical to the binary under test"
      else
        bad "$BIN_PATH differs from the binary that ran setup"
      fi
    else
      bad "F0 REGRESSION: --bin-path exists but nothing landed at $BIN_PATH"
    fi
  else
    if [ -x "$BIN_PATH" ]; then
      info "$BIN_PATH exists (pre-installed by this host, not by setup)"
    else
      skip "F0 IS LIVE ON THIS REF: setup finished without installing a binary, so the unit's ExecStart=$BIN_PATH does not exist and the service would die at 203/EXEC. Installing it by hand now so the rest of the smoke can run — that hand-install is precisely the step A1 adds."
      install -m 0755 "$SPARKBOX_BIN" "$BIN_PATH" ||
        bad "could not hand-install $BIN_PATH"
      systemctl restart sparkbox.service 2>/dev/null
    fi
  fi

  # ------------------------------------------------------------- F7 liveness -
  say "F7 — is the service actually LIVE (settle window ${SETTLE}s)?"
  local verdict; verdict=$(liveness_verdict)
  case "$verdict" in
    live) ok "sparkbox.service is live: restart counter and main-PID start time unchanged across ${SETTLE}s" ;;
    crashloop) bad "sparkbox.service is CRASH-LOOPING (restarts/start-time moved across the window)"; dump_journal ;;
    dead) bad "sparkbox.service is not active"; dump_journal ;;
    notloaded) bad "sparkbox.service is not loaded"; dump_journal ;;
  esac

  # A live gateway must actually answer. This traverses users.conf seeding, the
  # sqlite store, host-key minting and the SSH gateway — none of which a file
  # existence check covers.
  if [ "$verdict" = live ]; then
    if ssh -i "$WORK/op" -p 2222 \
         -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
         -o LogLevel=ERROR -o ConnectTimeout=10 \
         ctl@127.0.0.1 node ls >"$WORK/ctl.txt" 2>&1; then
      ok "ssh ctl@:2222 node ls answered: $(head -1 "$WORK/ctl.txt")"
    else
      bad "ssh ctl@:2222 node ls failed"; cat "$WORK/ctl.txt"; dump_journal
    fi
    if curl -fsS -o /dev/null "http://127.0.0.1:8081/" -H "Host: my.$DOMAIN"; then
      ok "HTTP edge answered on :8081"
    else
      bad "HTTP edge did not answer on :8081"
    fi
  else
    skip "control-door and edge probes not run — the service is not live"
  fi
  skip "no sandbox is created: that needs a real firecracker, a real kernel, a real rootfs and real KVM. Sandbox lifecycle stays covered by the in-process mock-driver e2e tests."

  # ------------------------------------------------------- F7 negative fixture
  # Break it on purpose and confirm the probe SAYS SO. This is the assertion
  # that would have failed on 2026-07-25: with the API port occupied the gateway
  # crash-loops, and both `setup` and `doctor` reported PASS and exited 0.
  say "F7 negative fixture — occupy the API port (:$API_PORT) and expect a crash loop"
  # Order matters: the gateway currently HOLDS the API port, so the squatter has
  # to take it while the service is stopped. (A restart-then-squat sequence just
  # makes python fail to bind, and the fixture silently proves nothing — which
  # is the same class of mistake as F7 itself.)
  systemctl stop sparkbox.service 2>/dev/null
  python3 -m http.server "$API_PORT" --bind 127.0.0.1 >"$WORK/squat.log" 2>&1 &
  local squat=$!
  for _ in $(seq 1 30); do
    curl -fsS -o /dev/null "http://127.0.0.1:$API_PORT/" 2>/dev/null && break
    sleep 0.2
  done
  if ! curl -fsS -o /dev/null "http://127.0.0.1:$API_PORT/" 2>/dev/null; then
    bad "could not occupy :$API_PORT — the F7 fixture proves nothing"
    cat "$WORK/squat.log"
    systemctl start sparkbox.service 2>/dev/null
    return
  fi

  # ------------------------------------------------------- A2 port preflight --
  # With a stranger on the API port and our service stopped, `setup` must refuse
  # BEFORE it writes anything, naming the address and the process — rather than
  # completing and leaving the gateway to discover the conflict at boot.
  if [ "$HAVE_A2" = 1 ]; then
    say "A2 — setup refuses to provision into a port conflict"
    "$SPARKBOX_BIN" "${args[@]}" >"$WORK/setup-busy.txt" 2>&1
    rc=$?
    if [ $rc -ne 0 ]; then
      ok "setup exits non-zero when :$API_PORT is taken (exit $rc)"
    else
      bad "A2 REGRESSION: setup exited 0 with :$API_PORT held by another process"
    fi
    assert_grep "$WORK/setup-busy.txt" "127\.0\.0\.1:$API_PORT" "the failure names the busy address"
    if grep -Eq 'python|pid' "$WORK/setup-busy.txt"; then
      ok "the failure names the owning process"
    else
      skip "setup did not name the owning process — \`ss\`/\`lsof\` may not be installed in this container"
    fi
  else
    skip "F1 IS LIVE ON THIS REF: setup has no --api-addr and no port preflight, so a busy port is only discovered at first boot. A2 is what changes this."
  fi

  info ":$API_PORT is occupied by a squatter; starting the gateway into the conflict"
  systemctl start sparkbox.service 2>/dev/null
  verdict=$(liveness_verdict)
  if [ "$verdict" = crashloop ]; then
    ok "probe correctly reports a crash loop when :$API_PORT is occupied"
    # Captured, not piped into grep -q — see the SETUP_HELP note above.
    local jrnl; jrnl=$(journalctl -u sparkbox.service -n 20 --no-pager 2>/dev/null)
    case "$jrnl" in
      *"address already in use"*) ok "journal names the port conflict" ;;
      *) info "journal did not name the port conflict (root may be needed to read it)" ;;
    esac
  else
    bad "probe reported '$verdict' over an occupied :$API_PORT — it cannot detect a crash loop"
    dump_journal
  fi

  # And the product's own verdict, which is the actual F7 fix.
  "$SPARKBOX_BIN" doctor --root "$FULL_ROOT" >"$WORK/doctor-broken.txt" 2>&1
  rc=$?
  if [ "$HAVE_A1" = 1 ]; then
    if [ $rc -ne 0 ]; then
      ok "\`sparkbox doctor\` exits non-zero over a crash loop (exit $rc)"
    else
      bad "F7 REGRESSION: \`sparkbox doctor\` exited 0 over a crash-looping service"
      cat "$WORK/doctor-broken.txt"
    fi
  else
    skip "F7 IS LIVE ON THIS REF: \`sparkbox doctor\` exited $rc over a crash-looping service (it should be non-zero). A1's liveness probe is what changes this."
    grep -E '\[(PASS|WARN|FAIL)\] sparkbox service' "$WORK/doctor-broken.txt" || true
  fi

  kill "$squat" 2>/dev/null
  wait "$squat" 2>/dev/null
  systemctl restart sparkbox.service 2>/dev/null
  sleep 3

  # ------------------------------------------------------------ idempotency --
  say "re-run — setup must be idempotent"
  "$SPARKBOX_BIN" "${args[@]}" >"$WORK/setup2.txt" 2>&1
  rc=$?
  cat "$WORK/setup2.txt"
  if [ $rc -eq 0 ]; then ok "second setup exit 0"; else bad "second setup exit $rc"; fi
  # resolve-release is deliberately NOT in this list: its Satisfied always
  # returns false so downstream steps see a manifest, which is a design choice,
  # not a leak. Every other step must recognise its own work.
  local s
  for s in data-volume fetch-artifacts users.conf host-config \
           net-rules systemd-units enable-services; do
    assert_grep "$WORK/setup2.txt" "^== $s: already satisfied" "re-run skips '$s'"
  done
  if [ "$HAVE_A1" = 1 ]; then
    assert_grep "$WORK/setup2.txt" '^== install-binary: already satisfied' \
      "re-run does not rewrite the installed binary"
  fi

  # ---------------------------------------------------------------- doctor ---
  say "doctor on the healthy host"
  "$SPARKBOX_BIN" doctor --root "$FULL_ROOT" >"$WORK/doctor.txt" 2>&1
  rc=$?
  cat "$WORK/doctor.txt"
  if [ $rc -eq 0 ]; then ok "doctor exit 0"; else bad "doctor exit $rc on a healthy host"; fi
  assert_grep "$WORK/doctor.txt" '[0-9]+ passed' "doctor printed a summary"
}

# ---------------------------------------------------------------------------
cleanup() {
  [ -n "${STUB_PID:-}" ] && kill "$STUB_PID" 2>/dev/null
  return 0
}
trap cleanup EXIT

printf 'sparkbox provision smoke — phase=%s arch=%s bin=%s a1=%s\n' \
  "$PHASE" "$ARCH" "$SPARKBOX_BIN" "$HAVE_A1"

case "$PHASE" in
  dry) dry_phase ;;
  full) full_phase ;;
  *) echo "usage: $0 [dry|full]" >&2; exit 2 ;;
esac

say "summary"
printf '  %d failed, %d skipped (skips are real limits of this host, listed above)\n' \
  "$FAILURES" "$SKIPS"
[ "$FAILURES" -eq 0 ] || exit 1
