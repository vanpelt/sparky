#!/bin/bash
#
# macos/poc.sh — the original macOS bring-up script. NO LONGER THE SUPPORTED
# PATH, and kept deliberately.
#
#   ############################################################################
#   #  On macOS the supported way to stand up a sparkbox host is now:          #
#   #                                                                          #
#   #      sparkbox doctor                                                     #
#   #      sparkbox setup --proxy-domain <domain>                              #
#   #                                                                          #
#   #  from the released sparkbox-darwin-arm64 binary — no repo, no Go         #
#   #  toolchain, no shell scripts. See docs/getting-started.md.               #
#   ############################################################################
#
# This script proved the whole design (docs/macos-nested-poc-results.md) and
# `sparkbox setup` on darwin is a port OF IT, step for step. It survives as a
# development and fallback tool for three concrete reasons:
#
#   1. It is the only thing that has ever created a real machine. The Go path is
#      unit-tested against an in-memory fake of the `container` CLI, and no CI
#      runner on earth can nest a VM for us (see the `macos` job in
#      .github/workflows/go.yml, which says so out loud). Until an operator has
#      run `sparkbox setup` on real hardware, deleting the thing that works
#      would be trading a proven path for an untested one.
#   2. `smoke` has no Go equivalent at all. Nothing in the binary boots a
#      sandbox and checks SSH, in-guest DNS/HTTPS, the metadata endpoint's
#      cross-slot refusal and a published HTTP route.
#   3. It is the reference for what the port is supposed to do. When the Go path
#      and this script disagree about a real machine, this script is the older
#      and better-tested claim.
#
# WHICH SUBCOMMANDS ARE NOW REDUNDANT
#
#   doctor     REDUNDANT — `sparkbox doctor` runs the macOS host checks (macOS
#              version, Apple Container version, Apple Silicon generation, disk,
#              machine ownership) and then relays the gateway's own doctor out
#              of the machine.
#   build      REDUNDANT — the `outer-kernel` and `machine-image` steps of
#              `sparkbox setup` fetch the released vmlinux-macos-arm64 and build
#              the gateway image from an embedded build context.
#   create     REDUNDANT — the `machine` step creates/adopts the machine.
#   provision  REDUNDANT — the `machine-sparkbox` and `provision-inner` steps
#              stage the released linux binary and run `sparkbox setup` inside.
#   status     MOSTLY REDUNDANT — `sparkbox doctor` reports machine state and
#              gateway health. This still prints a denser one-screen summary.
#
#   start      NOT redundant. `setup` ensures the machine is running as a side
#   stop       effect of provisioning; there is no `sparkbox machine start/stop`.
#   smoke      NOT redundant. The only L2 test that exists — see (2) above.
#   destroy    NOT redundant. Nothing in the binary deletes a machine, an image
#              or the outer kernel, and the cost-tiered teardown has no
#              equivalent.
#
# ONE THING THAT MATTERS IF YOU RUN BOTH: they use DIFFERENT machines. This
# script owns `sparkbox-poc`; `sparkbox setup` creates `sparkbox`. They coexist
# on one Mac without touching each other's state, which is the point — you can
# try the binary without giving up a working PoC. Both are `--home-mount none`
# and both check ownership before mutating, so neither will adopt the other's.
#
# Retirement plan: once `sparkbox setup` has provisioned a Mac on real hardware
# and `smoke` has an equivalent, this becomes a thin dev script (or disappears).
# Do not extend it in the meantime — new behaviour belongs in the Go path.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SPARKBOX_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
OUT_DIR="${SCRIPT_DIR}/out"

MACHINE_NAME="sparkbox-poc"
GATEWAY_IMAGE="local/sparkbox-gateway:macos-poc"
KERNEL_BUILD_SCRIPT="${SCRIPT_DIR}/kernel/build.sh"
KERNEL_FETCH_SCRIPT="${SCRIPT_DIR}/kernel/fetch.sh"
KERNEL_PATH="${OUT_DIR}/vmlinux-kvm"
SMOKE_SCRIPT="${SCRIPT_DIR}/smoke.sh"
# The gateway image needs only the files beside this script — never the Go module
# — so it is built from a staging directory rather than from tools/sparkbox. That
# keeps the context three small files instead of a source tree that also contains
# macos/out's 149MB kernel tarball, and it makes "this image needs no repo and no
# Go toolchain" structural rather than a claim in a comment.
IMAGE_CONTEXT="${OUT_DIR}/image-context"
IMAGE_CONTEXT_FILES=(Containerfile.gateway gateway-verify.sh sparkbox-bootstrap.sh)

# Where sparkbox-bootstrap stages the binary inside the machine, and where it
# records that binary's provenance. setup is run from BOOTSTRAP_BINARY and
# installs itself to /usr/local/bin/sparkbox (A1's stepInstallBinary).
BOOTSTRAP_BINARY="/var/lib/sparkbox-bootstrap/sparkbox"
BOOTSTRAP_RESULT="/var/lib/sparkbox-bootstrap/release.env"

MIN_CONTAINER_VERSION="1.1.0"
MIN_MACOS_VERSION="15.0"
SPARKBOX_RELEASE="${SPARKBOX_RELEASE:-v0.4.0}"
ARTIFACT_BASE="${SPARKBOX_ARTIFACT_BASE:-https://github.com/vanpelt/sparky/releases}"
# The opt-in escape hatch: develop against a working-tree sparkbox instead of
# the released one. Either build it here (SPARKBOX_SOURCE_BUILD=1) or point at a
# linux/arm64 binary you built yourself (SPARKBOX_LOCAL_BINARY). Both paths shout
# about it — see bootstrap_local_binary.
SOURCE_BUILD="${SPARKBOX_SOURCE_BUILD:-0}"
LOCAL_BINARY="${SPARKBOX_LOCAL_BINARY:-}"
# Where the outer KVM kernel comes from. "release" downloads vmlinux-macos-arm64
# for SPARKBOX_RELEASE and verifies it against SHA256_OUTER_KERNEL in that
# release's manifest-darwin-arm64.env; "build" compiles Linux 6.14.9 locally,
# which is what this script used to do unconditionally and what nobody should
# have to do to onboard. Same opt-in shape as SPARKBOX_SOURCE_BUILD above, for
# the same reason: developing on the kernel config is a real need, doing it by
# accident is not.
KERNEL_SOURCE="${SPARKBOX_KERNEL_SOURCE:-release}"
MACHINE_CPUS="${SPARKBOX_CPUS:-8}"
MACHINE_MEMORY="${SPARKBOX_MEMORY:-24G}"
BUILD_CPUS="${SPARKBOX_BUILD_CPUS:-8}"
BUILD_MEMORY="${SPARKBOX_GATEWAY_BUILD_MEMORY:-8G}"
DATA_VOLUME_GB="${SPARKBOX_DATA_VOLUME_GB:-40}"
PROXY_DOMAIN="${SPARKBOX_PROXY_DOMAIN:-sparkbox.test}"
OPERATOR_HANDLE="${SPARKBOX_OPERATOR_HANDLE:-operator}"
OPERATOR_KEY_FILE="${SPARKBOX_OPERATOR_KEY_FILE:-${HOME}/.ssh/id_ed25519.pub}"
FLEET_GATEWAY="${SPARKBOX_FLEET_GATEWAY:-}"
NODE_NAME="${SPARKBOX_NODE_NAME:-sparkbox-poc}"
SERVICE_WAIT_SECONDS="${SPARKBOX_SERVICE_WAIT_SECONDS:-90}"

UBUNTU_IMAGE="docker.io/library/ubuntu:24.04@sha256:4fbb8e6a8395de5a7550b33509421a2bafbc0aab6c06ba2cef9ebffbc7092d90"

DOCTOR_FAILURES=0

# Provenance of the sparkbox binary this provision run installed, read back out
# of the machine after the bootstrap step. Recorded in the evidence bundle and
# reconciled against the installed binary once setup has run.
BOOTSTRAP_SOURCE=""
BOOTSTRAP_RELEASE=""
BOOTSTRAP_SHA256=""
BOOTSTRAP_VERSION=""

usage() {
  cat <<'EOF'
usage: ./macos/poc.sh <command>

NOTE: this script is no longer the supported way to stand up a macOS host.
Use the released sparkbox-darwin-arm64 binary instead:
    sparkbox doctor
    sparkbox setup --proxy-domain <domain>
It creates a machine named `sparkbox`, separate from this script's
`sparkbox-poc`, so the two coexist. doctor/build/create/provision here are
redundant with it; start/stop/smoke/destroy have no equivalent yet. See the
header of this file and docs/getting-started.md.

Commands:
  doctor         Check the macOS host without changing it
  build          Fetch the released KVM kernel and build the gateway OCI image
  create         Create/reuse sparkbox-poc and run the full L1 preflight
  provision      Run the pinned Sparkbox setup inside sparkbox-poc
  status         Report outer-machine and Sparkbox gateway state
  stop           Stop sparkbox-poc without deleting its state
  start          Start sparkbox-poc and wait for Sparkbox readiness
  smoke          Run the L2 SSH/network/metadata/proxy smoke test
  destroy        Tear down by cost tier; always needs --yes (see below)

Teardown (./macos/poc.sh destroy [targets] --yes):
  --machine      delete sparkbox-poc and its state       (default; seconds to redo)
  --image        delete the local gateway OCI image      (minutes: a container build)
  --kernel       delete the outer kernel in macos/out    (cheap if it was
                                                          downloaded; a full
                                                          in-container Linux
                                                          build if not)
  --all          all three, plus the rest of macos/out (inputs + results evidence)

  With no target, destroy --yes deletes only the machine. The kernel is now
  fetched from the release and checksum-verified, so re-getting it is a 29MB
  download — but it is still the one artifact deleting buys nothing at all,
  and on SPARKBOX_KERNEL_SOURCE=build it costs a full kernel compile.

Where the outer KVM kernel comes from:
  `build` downloads vmlinux-macos-arm64 for SPARKBOX_RELEASE and verifies it
  against SHA256_OUTER_KERNEL in that release's manifest-darwin-arm64.env. That
  checksum says "this is the file the release published"; it is NOT a claim that
  recompiling reproduces those bytes (the builder's gcc version is baked into
  the kernel banner and the Ubuntu archive is not pinned — see the header of
  macos/kernel/build.sh).

  To compile it yourself instead — the escape hatch, needed when changing
  sparkbox-arm64.fragment or the pinned Linux version:
    SPARKBOX_KERNEL_SOURCE=build ./macos/poc.sh build
  kernel-manifest.txt and the evidence bundle record which path was taken.

Where the sparkbox binary comes from:
  The gateway image bakes no sparkbox. `provision` downloads the released
  sparkbox-linux-arm64 for SPARKBOX_RELEASE inside the machine, verifies it
  against SHA256_SPARKBOX in that release's manifest, and runs `setup` from it;
  setup then installs it at /usr/local/bin/sparkbox. The binary and the
  artifacts it fetches are therefore the same release, and `provision` fails if
  the installed binary ever reports a different version.

  To develop against your working tree instead (opt-in, and loudly announced):
    SPARKBOX_SOURCE_BUILD=1 ./macos/poc.sh provision
    SPARKBOX_LOCAL_BINARY=/path/to/sparkbox-linux-arm64 ./macos/poc.sh provision
  Such a machine is marked with /etc/sparkbox-poc-source-build and reports the
  source build in `status`, so it can never be mistaken for a release again.

Environment overrides:
  SPARKBOX_CPUS                  outer machine CPUs (default 8)
  SPARKBOX_MEMORY                outer machine memory (default 24G)
  SPARKBOX_BUILD_CPUS            image/kernel build CPUs (default 8)
  SPARKBOX_KERNEL_BUILD_MEMORY   kernel build memory (default 16G)
  SPARKBOX_GATEWAY_BUILD_MEMORY  gateway image build memory (default 8G)
  SPARKBOX_DATA_VOLUME_GB        required free space (default 40)
  SPARKBOX_RELEASE               pinned release: artifacts AND the sparkbox
                                 binary (default v0.4.0; "latest" allowed)
  SPARKBOX_ARTIFACT_BASE         release base URL (default GitHub Releases)
  SPARKBOX_KERNEL_SOURCE         release (default) = download + verify the
                                 released outer kernel; build = compile it here
  SPARKBOX_SOURCE_BUILD          1 = build sparkbox from this repo instead of
                                 downloading the release (opt-in, announced)
  SPARKBOX_LOCAL_BINARY          path to a linux/arm64 sparkbox to install
                                 instead of the release (opt-in, announced)
  SPARKBOX_PROXY_DOMAIN          gateway proxy domain (default sparkbox.test)
  SPARKBOX_OPERATOR_HANDLE       operator account handle (default operator)
  SPARKBOX_OPERATOR_KEY_FILE     public key to seed (default ~/.ssh/id_ed25519.pub)
  SPARKBOX_OPERATOR_PRIVATE_KEY_FILE
                                 matching private key for smoke (default public key path without .pub)
  SPARKBOX_FLEET_GATEWAY          gateway host:port; provision as a fleet node when set
  SPARKBOX_NODE_NAME              fleet node name (default sparkbox-poc)
  SPARKBOX_SERVICE_WAIT_SECONDS  start readiness timeout (default 90)
EOF
}

pass() {
  printf '  [PASS] %-22s %s\n' "$1" "$2"
}

warn() {
  printf '  [WARN] %-22s %s\n' "$1" "$2"
}

fail() {
  printf '  [FAIL] %-22s %s\n' "$1" "$2"
  DOCTOR_FAILURES=$((DOCTOR_FAILURES + 1))
}

die() {
  echo "error: $*" >&2
  exit 1
}

version_ge() {
  awk -v have="$1" -v need="$2" 'BEGIN {
    split(have, h, ".");
    split(need, n, ".");
    for (i = 1; i <= 3; i++) {
      hv = h[i] + 0;
      nv = n[i] + 0;
      if (hv > nv) exit 0;
      if (hv < nv) exit 1;
    }
    exit 0;
  }'
}

machine_exists() {
  local machines
  machines="$(container machine list --quiet)" || return 2
  grep -Fqx "${MACHINE_NAME}" <<<"${machines}"
}

machine_inspect() {
  container machine inspect "${MACHINE_NAME}"
}

machine_is_owned() {
  local inspect_json="$1"
  jq -e --arg image "${GATEWAY_IMAGE}" \
    '.[0].id == "sparkbox-poc"
      and .[0].image.reference == $image
      and .[0].homeMount == "none"' \
    >/dev/null <<<"${inspect_json}"
}

require_owned_machine() {
  local inspect_json machine_status

  # The status must be read in an else branch: after `fi`, `$?` is the `if`
  # statement's own status (always 0), which would swallow the "does not
  # exist" case behind the misleading list-failure message.
  if machine_exists; then
    inspect_json="$(machine_inspect)"
    machine_is_owned "${inspect_json}" \
      || die "${MACHINE_NAME} exists but is not owned by this PoC"
    printf '%s\n' "${inspect_json}"
    return
  else
    machine_status="$?"
  fi

  [[ "${machine_status}" -eq 1 ]] || die "could not list container machines"
  die "${MACHINE_NAME} does not exist; run ./macos/poc.sh create"
}

require_container_runtime() {
  command -v container >/dev/null || die "Apple Container CLI is not installed"
  command -v jq >/dev/null || die "jq is required"
  container system status >/dev/null 2>&1 \
    || die "container service is not running; run: container system start"
}

run_doctor() {
  DOCTOR_FAILURES=0
  echo "sparkbox macOS nested gateway doctor"
  echo

  local os_name
  os_name="$(uname -s 2>/dev/null || true)"
  if [[ "${os_name}" == "Darwin" ]]; then
    pass "operating system" "macOS"
  else
    fail "operating system" "${os_name:-unknown}; macOS is required"
  fi

  local arch
  arch="$(uname -m 2>/dev/null || true)"
  if [[ "${arch}" == "arm64" ]]; then
    pass "architecture" "Apple Silicon (arm64)"
  else
    fail "architecture" "${arch:-unknown}; Apple Silicon is required"
  fi

  if command -v sw_vers >/dev/null; then
    local macos_version
    macos_version="$(sw_vers -productVersion)"
    if version_ge "${macos_version}" "${MIN_MACOS_VERSION}"; then
      pass "macOS version" "${macos_version} (>= ${MIN_MACOS_VERSION})"
    else
      fail "macOS version" "${macos_version}; nested virtualization requires >= ${MIN_MACOS_VERSION}"
    fi
  else
    fail "macOS version" "sw_vers not found"
  fi

  if command -v sysctl >/dev/null; then
    local chip generation
    chip="$(sysctl -n machdep.cpu.brand_string 2>/dev/null || true)"
    generation="$(sed -E 's/^Apple M([0-9]+).*/\1/' <<<"${chip}")"
    if [[ "${generation}" =~ ^[0-9]+$ ]] && [[ "${generation}" -ge 3 ]]; then
      pass "nested virtualization" "${chip}"
    else
      fail "nested virtualization" "${chip:-unknown chip}; Apple M3 or newer is required"
    fi
  else
    fail "nested virtualization" "sysctl not found"
  fi

  local tool
  for tool in container curl jq shasum; do
    if command -v "${tool}" >/dev/null; then
      pass "host tool: ${tool}" "$(command -v "${tool}")"
    else
      fail "host tool: ${tool}" "not found"
    fi
  done

  if command -v container >/dev/null; then
    local container_output container_version
    container_output="$(container --version 2>/dev/null || true)"
    container_version="$(sed -E 's/.*version ([0-9]+\.[0-9]+\.[0-9]+).*/\1/' <<<"${container_output}")"
    if [[ "${container_version}" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] \
      && version_ge "${container_version}" "${MIN_CONTAINER_VERSION}"; then
      pass "Apple Container CLI" "${container_version} (>= ${MIN_CONTAINER_VERSION})"
    else
      fail "Apple Container CLI" "${container_output:-unavailable}; need >= ${MIN_CONTAINER_VERSION}"
    fi
  fi

  local service_running=0
  if command -v container >/dev/null; then
    if container system status >/dev/null 2>&1; then
      service_running=1
      pass "container service" "running"
    else
      fail "container service" "not running; run: container system start"
    fi
  fi

  local available_kb required_kb
  if [[ ! "${DATA_VOLUME_GB}" =~ ^[0-9]+$ ]] || [[ "${DATA_VOLUME_GB}" -lt 1 ]]; then
    fail "data volume size" "${DATA_VOLUME_GB}; SPARKBOX_DATA_VOLUME_GB must be a positive integer"
  else
    available_kb="$(df -Pk "${SCRIPT_DIR}" | awk 'NR == 2 {print $4}')"
    required_kb=$((DATA_VOLUME_GB * 1024 * 1024))
    if [[ "${available_kb:-0}" -ge "${required_kb}" ]]; then
      pass "available disk" "$((available_kb / 1024 / 1024)) GiB (need ${DATA_VOLUME_GB} GiB)"
    else
      fail "available disk" "$((available_kb / 1024 / 1024)) GiB; need ${DATA_VOLUME_GB} GiB"
    fi
  fi

  if [[ "${service_running}" -eq 1 ]]; then
    if machine_exists; then
      local inspect_json
      inspect_json="$(machine_inspect)"
      if machine_is_owned "${inspect_json}"; then
        pass "machine name" "${MACHINE_NAME} is an existing Sparkbox PoC with home-mount=none"
      else
        fail "machine name" "${MACHINE_NAME} exists but is not the expected home-mount=none Sparkbox image"
      fi
    else
      local machine_status="$?"
      if [[ "${machine_status}" -eq 1 ]]; then
        pass "machine name" "${MACHINE_NAME} is available"
      else
        fail "machine name" "could not list container machines"
      fi
    fi
  else
    warn "machine name" "ownership check skipped until the container service is running"
  fi

  # Say at every entry point which sparkbox a provision would install. The PoC
  # once ran a source build while reporting a release; the cheapest guard against
  # a repeat is for the escape hatch to be impossible to miss, including from the
  # command whose whole job is to tell you what you are about to get.
  if using_source_build; then
    warn "sparkbox binary" "SOURCE BUILD requested; provision will NOT install a release"
    if [[ -n "${LOCAL_BINARY}" ]]; then
      if [[ -f "${LOCAL_BINARY}" ]]; then
        pass "  local binary" "${LOCAL_BINARY}"
      else
        fail "  local binary" "${LOCAL_BINARY} does not exist"
      fi
    elif command -v go >/dev/null; then
      pass "  Go toolchain" "$(command -v go)"
    else
      fail "  Go toolchain" "not found; SPARKBOX_SOURCE_BUILD=1 needs Go, or set SPARKBOX_LOCAL_BINARY"
    fi
  else
    pass "sparkbox binary" "released sparkbox-linux-arm64 for ${SPARKBOX_RELEASE}"
  fi

  echo
  if [[ "${DOCTOR_FAILURES}" -gt 0 ]]; then
    echo "${DOCTOR_FAILURES} check(s) failed"
    return 1
  fi
  echo "all host checks passed"
}

# write_build_manifest records what went INTO the image.
#
# It deliberately no longer records a sparkbox version or a release. It used to
# record both, and that is how one evidence bundle came to hold two contradictory
# answers: build-inputs.txt said sparkbox_release=v0.3.0 (captured at build time,
# from an env var only provision reads) while provision-manifest.txt beside it
# said v0.4.0. Neither was wrong about what it measured; the file was simply
# claiming a build-time fact about a provision-time choice. The release now
# belongs solely to the provision manifest, and the binary's real provenance is
# read back out of the machine rather than assumed.
write_build_manifest() {
  local commit
  # git is a convenience here, not a requirement: the image no longer needs the
  # repo, so a checkout without git history still builds.
  commit="$(git -C "${SPARKBOX_DIR}" rev-parse HEAD 2>/dev/null || echo unknown)"

  {
    printf 'apple_container_min_version=%s\n' "${MIN_CONTAINER_VERSION}"
    printf 'gateway_image=%s\n' "${GATEWAY_IMAGE}"
    printf 'ubuntu_image=%s\n' "${UBUNTU_IMAGE}"
    printf 'image_sources_commit=%s\n' "${commit}"
    printf 'sparkbox_binary=none (fetched per release at provision time)\n'
    # Which kernel this machine will boot and where it came from. The B3 skew
    # (a source-built binary provisioning a release's artifacts) had the same
    # shape as a locally-compiled kernel under a released everything-else, so
    # record it here rather than leaving it to kernel-manifest.txt alone.
    printf 'kernel_source=%s\n' "$(sed -n 's/^kernel_source=//p' "${OUT_DIR}/kernel-manifest.txt" 2>/dev/null || true)"
    printf 'kernel_sha256=%s\n' "$(sed -n 's/^kernel_sha256=//p' "${OUT_DIR}/kernel-manifest.txt" 2>/dev/null || true)"
    printf 'machine_cpus=%s\n' "${MACHINE_CPUS}"
    printf 'machine_memory=%s\n' "${MACHINE_MEMORY}"
    printf 'data_volume_gb=%s\n' "${DATA_VOLUME_GB}"
  } > "${OUT_DIR}/inputs.txt"
}

# stage_image_context copies the handful of files the Containerfile references
# into a dedicated directory, and builds from there. The build context used to be
# the whole Go module because a build stage compiled sparkbox out of it; nothing
# in the image comes from the module any more.
stage_image_context() {
  local file
  rm -rf -- "${IMAGE_CONTEXT}"
  mkdir -p "${IMAGE_CONTEXT}"
  for file in "${IMAGE_CONTEXT_FILES[@]}"; do
    [[ -f "${SCRIPT_DIR}/${file}" ]] || die "missing ${SCRIPT_DIR}/${file}"
    cp "${SCRIPT_DIR}/${file}" "${IMAGE_CONTEXT}/${file}"
  done
}

# ensure_kernel puts macos/out/vmlinux-kvm in place. The default is a download:
# CI compiles this kernel once per release on a native arm64 runner and publishes
# it as vmlinux-macos-arm64, so a Mac verifies a checksum instead of spending
# five minutes and 149MB of Linux source on an onboarding step.
#
# Both paths write the same two files (the Image and kernel-manifest.txt), and
# the manifest's kernel_source= line is the only way to tell them apart — which
# is deliberate: it lands in the provision evidence bundle, so a machine built
# on the escape hatch can never be mistaken later for one built on the release.
ensure_kernel() {
  case "${KERNEL_SOURCE}" in
    release)
      SPARKBOX_RELEASE="${SPARKBOX_RELEASE}" \
      SPARKBOX_ARTIFACT_BASE="${ARTIFACT_BASE}" \
        "${KERNEL_FETCH_SCRIPT}"
      ;;
    build)
      echo "*** compiling the outer kernel locally (SPARKBOX_KERNEL_SOURCE=build) ***"
      echo "    the released kernel would be downloaded and checksum-verified instead"
      "${KERNEL_BUILD_SCRIPT}"
      ;;
    *)
      die "SPARKBOX_KERNEL_SOURCE must be 'release' or 'build', not '${KERNEL_SOURCE}'"
      ;;
  esac
}

build_all() {
  run_doctor || die "host doctor failed"
  mkdir -p "${OUT_DIR}"

  ensure_kernel

  stage_image_context
  echo "building ${GATEWAY_IMAGE} for linux/arm64 (no Go toolchain, no module source)"
  container build \
    --arch arm64 \
    --cpus "${BUILD_CPUS}" \
    --memory "${BUILD_MEMORY}" \
    --file "${IMAGE_CONTEXT}/Containerfile.gateway" \
    --tag "${GATEWAY_IMAGE}" \
    --build-arg "UBUNTU_IMAGE=${UBUNTU_IMAGE}" \
    "${IMAGE_CONTEXT}"

  container image inspect "${GATEWAY_IMAGE}" >/dev/null
  write_build_manifest
  echo
  echo "build complete"
  echo "  kernel: ${KERNEL_PATH}"
  echo "  image:  ${GATEWAY_IMAGE} (contains no sparkbox binary by design)"
  echo "  inputs: ${OUT_DIR}/inputs.txt"
  echo
  echo "provision downloads sparkbox-linux-arm64 for \$SPARKBOX_RELEASE into the"
  echo "machine and verifies it against that release's manifest."
}

write_host_evidence() {
  local destination="$1"
  {
    sw_vers
    uname -a
    sysctl -n machdep.cpu.brand_string
    container --version
    df -h "${SCRIPT_DIR}"
  } > "${destination}"
}

run_gateway_preflight() {
  local destination="$1"
  local attempt

  for attempt in 1 2 3; do
    if container machine run --name "${MACHINE_NAME}" --root \
      /usr/local/sbin/sparkbox-poc-verify \
      2>&1 | tee "${destination}"; then
      return 0
    fi

    if [[ "${attempt}" -lt 3 ]]; then
      echo "gateway preflight attempt ${attempt} failed; retrying machine connection" >&2
      sleep 2
    fi
  done

  return 1
}

write_provision_manifest() {
  local destination="$1"
  local operator_key="$2"

  {
    printf 'machine_name=%s\n' "${MACHINE_NAME}"
    printf 'gateway_image=%s\n' "${GATEWAY_IMAGE}"
    printf 'sparkbox_release_requested=%s\n' "${SPARKBOX_RELEASE}"
    printf 'artifact_base=%s\n' "${ARTIFACT_BASE}"
    # The binary's real provenance, read back out of the machine after the
    # bootstrap step rather than inferred from what we asked for. When
    # binary_source=release these lines and sparkbox_release_requested all
    # describe one artifact; when it is "local" they deliberately do not, and
    # the file says so rather than leaving a reader to notice.
    printf 'binary_source=%s\n' "${BOOTSTRAP_SOURCE}"
    printf 'sparkbox_release_resolved=%s\n' "${BOOTSTRAP_RELEASE}"
    printf 'sparkbox_binary_sha256=%s\n' "${BOOTSTRAP_SHA256}"
    printf 'sparkbox_binary_version=%s\n' "${BOOTSTRAP_VERSION}"
    if [[ "${BOOTSTRAP_SOURCE}" != "release" ]]; then
      printf 'WARNING=this machine runs a local source build, not a release\n'
    fi
    printf 'proxy_domain=%s\n' "${PROXY_DOMAIN}"
    printf 'data_volume_gb=%s\n' "${DATA_VOLUME_GB}"
    printf 'swap_gb=0\n'
    if [[ -n "${FLEET_GATEWAY}" ]]; then
      printf 'role=fleet-node\n'
      printf 'fleet_gateway=%s\n' "${FLEET_GATEWAY}"
      printf 'node_name=%s\n' "${NODE_NAME}"
    else
      printf 'role=standalone-gateway\n'
      printf 'operator_handle=%s\n' "${OPERATOR_HANDLE}"
      printf 'operator_key_file=%s\n' "${OPERATOR_KEY_FILE}"
      printf 'operator_key_sha256=%s\n' \
        "$(printf '%s' "${operator_key}" | shasum -a 256 | awk '{print $1}')"
    fi
  } > "${destination}"
}

run_machine_script() {
  container machine run --name "${MACHINE_NAME}" --root --interactive \
    /bin/bash -s
}

capture_gateway_storage() {
  run_machine_script <<'EOF'
set -euo pipefail
echo "== df -h =="
df -h
echo
echo "== df -T =="
df -T
echo
echo "== lsblk =="
lsblk
EOF
}

capture_gateway_health() {
  if [[ -n "${FLEET_GATEWAY}" ]]; then
    container machine run --name "${MACHINE_NAME}" --root \
      /usr/local/bin/sparkbox doctor --gateway "${FLEET_GATEWAY}"
  else
    # Bash 3.2 (the macOS system shell) treats "${empty_array[@]}" as an
    # unbound variable under set -u. Keep the no-argument path explicit.
    container machine run --name "${MACHINE_NAME}" --root \
      /usr/local/bin/sparkbox doctor
  fi
  echo
  run_machine_script <<'EOF'
set -euo pipefail
echo "== services =="
systemctl is-active sparkbox-net.service sparkbox.service
systemctl is-enabled sparkbox-net.service sparkbox.service
echo
echo "== installed artifacts =="
/usr/local/bin/sparkbox version
/usr/local/bin/firecracker --version
uname -r
findmnt -T /srv/sparkbox/data
xfs_info /srv/sparkbox/data
stat -c "%n %s bytes" /srv/sparkbox/vmlinux /srv/sparkbox/data/images/*.ext4
EOF
}

# describe_role renders a GATEWAY_FLAG value as the role an operator thinks in.
# An empty flag is not "no role"; it is the standalone gateway, which is the
# thing the message has to say out loud or the refusal reads as a parse error.
describe_role() {
  if [[ -z "$1" ]]; then
    printf 'a standalone gateway'
  else
    printf 'a fleet node (%s)' "$1"
  fi
}

# machine_sandbox_count prints how many sandboxes this machine still carries.
#
# Conservative by construction, because the only safe error here is
# over-counting: it sums persisted VM directories, persisted manager records
# and live firecracker processes, and it looks for the first two under EVERY
# state directory below /srv/sparkbox rather than the one this script expects.
# A layout it does not recognise therefore reads as occupied, not as empty, and
# a machine it cannot inspect at all makes the caller refuse (a non-numeric or
# empty answer is not zero).
machine_sandbox_count() {
  run_machine_script <<'EOF'
set -uo pipefail
count=0
# One directory per sandbox the firecracker driver has ever materialised.
while IFS= read -r dir; do
  n="$(find "${dir}" -mindepth 1 -maxdepth 1 -type d 2>/dev/null | wc -l)"
  count=$((count + n))
done < <(find /srv/sparkbox -maxdepth 4 -type d -name fc-vms 2>/dev/null)
# Manager records outlive a stopped VM's directory, and an archived sandbox has
# a record and no directory at all. grep -c counts matching LINES, which for a
# single-line JSON document is 0 or 1 — enough, since the caller only asks
# whether the total is zero.
while IFS= read -r file; do
  n="$(grep -c '"name"' "${file}" 2>/dev/null || true)"
  count=$((count + ${n:-0}))
done < <(find /srv/sparkbox -maxdepth 4 -type f -name sandboxes.json 2>/dev/null)
# Anything live right now, in case its state lives somewhere unexpected.
for comm in /proc/[0-9]*/comm; do
  [[ -r "${comm}" ]] || continue
  read -r name < "${comm}" || true
  [[ "${name:-}" == firecracker ]] && count=$((count + 1))
done
printf '%s\n' "${count}"
EOF
}

# switch_machine_role changes a provisioned machine between standalone gateway
# and fleet node in place, or refuses and says why.
#
# The change itself is one line in sparkbox.env, so destroying the machine to
# make it is pure waste — and under the old teardown it also took the compiled
# outer kernel with it. What makes the switch unsafe is not the flag but the
# state underneath: a gateway's users DB, routes, invite codes and fleet
# secrets are meaningless on a node, and the sandboxes on it belong to accounts
# that would stop existing. With no sandboxes there is nothing to strand, so the
# switch proceeds; with any, or with no way to prove there are none, it refuses.
switch_machine_role() {
  local from="$1" to="$2" count=""

  count="$(machine_sandbox_count 2>/dev/null | tr -d '[:space:]' || true)"
  [[ "${count}" =~ ^[0-9]+$ ]] \
    || die "cannot switch ${MACHINE_NAME} from $(describe_role "${from}") to $(describe_role "${to}"): could not count the sandboxes on it, so it cannot be shown to be empty. Start the machine and retry, or destroy it (./macos/poc.sh destroy --machine --yes) and provision again"
  if [[ "${count}" -ne 0 ]]; then
    die "refusing to switch ${MACHINE_NAME} from $(describe_role "${from}") to $(describe_role "${to}") while it carries ${count} sandbox(es): a gateway's users DB, routes and fleet secrets are meaningless on a node, and those sandboxes belong to accounts that would stop existing. Destroy the sandboxes first, or the machine (./macos/poc.sh destroy --machine --yes), which keeps the image and the kernel"
  fi

  echo "switching ${MACHINE_NAME} from $(describe_role "${from}") to $(describe_role "${to}") (no sandboxes on it)"
  # setup writes sparkbox.env only when there is none, so the file is moved
  # aside rather than edited: setup then renders the whole thing from the new
  # flags. The operator's own lines (TLS_FLAGS, the console password) go with
  # it, which is why it is a backup and not a delete.
  run_machine_script <<'EOF'
set -euo pipefail
env_file=/srv/sparkbox/sparkbox.env
if [[ -f "${env_file}" ]]; then
  backup="${env_file}.bak-$(date -u +%Y%m%dT%H%M%SZ)"
  mv "${env_file}" "${backup}"
  echo "  moved ${env_file} to ${backup}"
fi
EOF
  echo "  gateway state (fleet keys, certificates, sqlite) is left untouched"
}

# using_source_build answers whether this run has opted out of the release
# binary. Both switches mean the same thing to everything downstream, so ask
# once, here.
using_source_build() {
  [[ "${SOURCE_BUILD}" == "1" ]] || [[ -n "${LOCAL_BINARY}" ]]
}

# bootstrap_release_binary downloads the released sparkbox into the machine.
#
# The download and its verification both happen INSIDE the machine, for the same
# reason `sparkbox setup` fetches its own kernel and rootfs there: the machine
# has no host mount (--home-mount none, asserted by gateway-verify.sh) and has
# already proven outbound HTTPS to the releases endpoint, so a host-side download
# would only add a transport that could corrupt what it carries. The checksum
# comes from the same release's manifest-<arch>.env, which is exactly how the
# host side verifies vmlinux, firecracker and the rootfs.
bootstrap_release_binary() {
  local log="$1" status=0

  echo "installing sparkbox ${SPARKBOX_RELEASE} into ${MACHINE_NAME} from ${ARTIFACT_BASE}"
  container machine run --name "${MACHINE_NAME}" --root \
    /usr/local/sbin/sparkbox-bootstrap release "${SPARKBOX_RELEASE}" "${ARTIFACT_BASE}" \
    2>&1 | tee "${log}" \
    || status="$?"
  [[ "${status}" -eq 0 ]] \
    || die "could not install the released sparkbox binary in ${MACHINE_NAME} (see ${log})"
}

# push_binary_to_machine copies a file from this Mac into the machine, in pieces.
#
# There is no shared filesystem (--home-mount none), no `container machine cp`,
# and `container machine run -c 'a | b'` is not available either — argv does not
# survive spaces inside one element. So stdin is the only channel, and it has a
# hard ceiling. Measured against container CLI 1.1.0 on this machine:
#
#   64 KiB  stdin -> sha256sum in the machine: byte-exact
#   128 KiB stdin -> sha256sum in the machine: byte-exact
#   192 KiB and 1 MiB: DEADLOCK. The CLI stops draining stdin, the guest process
#   never sees EOF, and the hang does not even respond to a SIGTERM aimed at the
#   pipeline — it has to be killed by hand.
#
# A 24MB sparkbox therefore cannot be handed over in one call. It is appended in
# chunks with dd instead, which was measured end to end on this hardware: a real
# 24,379,576-byte linux/arm64 build went over in 373 chunks in 37 seconds and
# arrived byte-exact (sha256 matched on both sides), after which the real
# bootstrap script installed it and it ran in the machine. Raw bytes, not base64:
# random blocks round-trip byte-exact, and base64 would add a third more calls
# for nothing. The receiving side re-hashes the assembled file, which is what
# makes a dropped or duplicated chunk a refusal instead of silent corruption.
#
# 64 KiB is the default chunk because it is the smaller of the two sizes proven
# good, i.e. a 3x margin under the smallest size known to hang.
BOOTSTRAP_INCOMING="/var/lib/sparkbox-bootstrap/incoming"
PUSH_CHUNK_BYTES="${SPARKBOX_PUSH_CHUNK_BYTES:-65536}"

push_binary_to_machine() {
  local binary="$1" sha="$2"
  local staging chunk total=0 sent=0 first=1

  staging="${OUT_DIR}/push-tmp"
  rm -rf -- "${staging}"
  mkdir -p "${staging}"
  # -a 4 because a two-character suffix runs out at 676 pieces, which a 45MB
  # binary would reach — and `split` fails at that point rather than wrapping.
  split -b "${PUSH_CHUNK_BYTES}" -a 4 "${binary}" "${staging}/chunk."
  total="$(find "${staging}" -name 'chunk.*' | wc -l | tr -d ' ')"
  [[ "${total}" -gt 0 ]] || die "could not split ${binary} for transfer"

  echo "pushing $(basename "${binary}") (sha256 ${sha}) into ${MACHINE_NAME}: ${total} chunks"
  container machine run --name "${MACHINE_NAME}" --root \
    /bin/mkdir -p "$(dirname "${BOOTSTRAP_INCOMING}")" \
    || die "${MACHINE_NAME} has no $(dirname "${BOOTSTRAP_INCOMING}"); rebuild the gateway image (./macos/poc.sh build)"

  for chunk in "${staging}"/chunk.*; do
    if [[ "${first}" -eq 1 ]]; then
      # No oflag=append on the first chunk: dd truncates, which is what clears
      # any half-finished push left by an earlier run.
      container machine run --name "${MACHINE_NAME}" --root --interactive \
        /usr/bin/dd of="${BOOTSTRAP_INCOMING}" status=none < "${chunk}" \
        || die "chunk 1/${total} did not reach ${MACHINE_NAME}"
      first=0
    else
      container machine run --name "${MACHINE_NAME}" --root --interactive \
        /usr/bin/dd of="${BOOTSTRAP_INCOMING}" oflag=append conv=notrunc status=none < "${chunk}" \
        || die "chunk $((sent + 1))/${total} did not reach ${MACHINE_NAME}"
    fi
    sent=$((sent + 1))
    # A minute of silence reads as a hang, and a hang is exactly what this
    # transport does when it goes wrong, so say something on the way.
    if [[ $((sent % 25)) -eq 0 ]] || [[ "${sent}" -eq "${total}" ]]; then
      printf '  %s/%s chunks\n' "${sent}" "${total}"
    fi
  done
  rm -rf -- "${staging}"
}

# bootstrap_local_binary installs a working-tree build instead — the documented
# escape hatch, and the only path that can put a non-release binary on a machine.
#
# It exists because developing a control-plane change otherwise means cutting a
# release for every iteration. It is opt-in, it prints a banner the reader cannot
# miss, it stamps the version string with a "source-" prefix so `sparkbox
# version` can never be read as a release tag, and it leaves
# /etc/sparkbox-poc-source-build in the machine so the fact outlives this
# terminal.
#
# Transport: see push_binary_to_machine. Whatever arrives is verified inside the
# machine against a sha256 computed here, so a damaged push is a refusal rather
# than an unrunnable — or subtly wrong — /usr/local/bin/sparkbox.
bootstrap_local_binary() {
  local log="$1" binary="${LOCAL_BINARY}" label sha status=0

  label="source-$(git -C "${SPARKBOX_DIR}" describe --tags --always --dirty 2>/dev/null || echo unknown)"

  if [[ -z "${binary}" ]]; then
    command -v go >/dev/null \
      || die "SPARKBOX_SOURCE_BUILD=1 needs a Go toolchain on this Mac; install Go or set SPARKBOX_LOCAL_BINARY to a prebuilt linux/arm64 sparkbox"
    binary="${OUT_DIR}/sparkbox-linux-arm64-source"
    echo "cross-compiling ${label} from ${SPARKBOX_DIR}"
    ( cd "${SPARKBOX_DIR}" \
      && CGO_ENABLED=0 GOOS=linux GOARCH=arm64 \
        go build -trimpath -ldflags "-s -w -X main.version=${label}" \
          -o "${binary}" ./cmd/sparkbox )
  else
    [[ -f "${binary}" ]] || die "SPARKBOX_LOCAL_BINARY does not exist: ${binary}"
    label="source-$(basename "${binary}")"
  fi
  [[ -s "${binary}" ]] || die "empty sparkbox binary: ${binary}"

  sha="$(shasum -a 256 "${binary}" | awk '{print $1}')"
  push_binary_to_machine "${binary}" "${sha}"
  container machine run --name "${MACHINE_NAME}" --root \
    /usr/local/sbin/sparkbox-bootstrap local "${sha}" "${label}" "${BOOTSTRAP_INCOMING}" \
    2>&1 | tee "${log}" \
    || status="$?"
  [[ "${status}" -eq 0 ]] \
    || die "could not install the local sparkbox build in ${MACHINE_NAME} (see ${log})"
}

# read_bootstrap_provenance loads what the machine says about the binary it just
# staged. Asking the machine, rather than trusting what we asked for, is the
# whole point: the version that ends up running is a property of the machine.
read_bootstrap_provenance() {
  local env_text
  env_text="$(
    container machine run --name "${MACHINE_NAME}" --root \
      /bin/cat "${BOOTSTRAP_RESULT}" 2>/dev/null || true
  )"
  [[ -n "${env_text}" ]] \
    || die "${MACHINE_NAME} has no ${BOOTSTRAP_RESULT}; the bootstrap step did not complete"

  BOOTSTRAP_SOURCE="$(sed -n 's/^SOURCE=//p' <<<"${env_text}" | tail -1)"
  BOOTSTRAP_RELEASE="$(sed -n 's/^RELEASE=//p' <<<"${env_text}" | tail -1)"
  BOOTSTRAP_SHA256="$(sed -n 's/^SHA256_SPARKBOX=//p' <<<"${env_text}" | tail -1)"
  BOOTSTRAP_VERSION="$(sed -n 's/^VERSION=//p' <<<"${env_text}" | tail -1)"
  [[ -n "${BOOTSTRAP_VERSION}" ]] \
    || die "${BOOTSTRAP_RESULT} in ${MACHINE_NAME} does not name a sparkbox version"
}

# reconcile_installed_binary compares what is now at /usr/local/bin/sparkbox with
# what the bootstrap staged.
#
# This is the check whose absence was the bug. setup installs the binary it was
# run from, so the two should be identical — but "should" was exactly the
# assumption that let a v0.3.0 source build provision v0.4.0 artifacts and report
# success. A mismatch here means something other than this run's bootstrap put a
# sparkbox on the machine (a leftover from an older image, a hand-copied build),
# and every result gathered afterwards would be about that binary instead.
reconcile_installed_binary() {
  local installed version
  installed="$(
    container machine run --name "${MACHINE_NAME}" --root \
      /usr/local/bin/sparkbox version 2>/dev/null || true
  )"
  version="$(awk 'NR == 1 {print $2}' <<<"${installed}")"
  [[ -n "${version}" ]] \
    || die "no runnable /usr/local/bin/sparkbox in ${MACHINE_NAME} after setup; setup's install-binary step did not run (was --bin-path emptied?)"

  if [[ "${version}" != "${BOOTSTRAP_VERSION}" ]]; then
    die "version skew in ${MACHINE_NAME}: /usr/local/bin/sparkbox reports '${version}' but this run staged '${BOOTSTRAP_VERSION}'. The box is not running the binary that fetched its artifacts — destroy the machine (./macos/poc.sh destroy --machine --yes) and provision again"
  fi

  if [[ "${BOOTSTRAP_SOURCE}" == "release" ]]; then
    echo "binary check: /usr/local/bin/sparkbox is ${version}, the release that provisioned it"
  else
    printf '%s\n' >&2 \
      '' \
      "  *** ${MACHINE_NAME} is running the SOURCE BUILD ${version}, not a release. ***" \
      '  *** Results from it are evidence about your working tree only.            ***' \
      ''
  fi
}

provision_machine() {
  run_doctor || die "host doctor failed"
  require_owned_machine >/dev/null
  local operator_key=""
  if [[ -n "${FLEET_GATEWAY}" ]]; then
    [[ "${FLEET_GATEWAY}" =~ ^[^[:space:]]+:[0-9]+$ ]] \
      || die "SPARKBOX_FLEET_GATEWAY must be host:port"
    [[ "${NODE_NAME}" =~ ^[a-z0-9][a-z0-9-]{0,62}$ ]] \
      || die "invalid SPARKBOX_NODE_NAME: ${NODE_NAME}"
  else
    [[ -f "${OPERATOR_KEY_FILE}" ]] \
      || die "operator public key not found: ${OPERATOR_KEY_FILE}"
    [[ "${OPERATOR_HANDLE}" =~ ^[a-zA-Z0-9][a-zA-Z0-9._-]*$ ]] \
      || die "invalid SPARKBOX_OPERATOR_HANDLE: ${OPERATOR_HANDLE}"
    operator_key="$(<"${OPERATOR_KEY_FILE}")"
    [[ "${operator_key}" == ssh-* ]] \
      || die "${OPERATOR_KEY_FILE} does not contain a supported SSH public key"
  fi

  # setup deliberately preserves an existing sparkbox.env so operator TLS and
  # console edits survive idempotent reruns. That also means setup alone cannot
  # change GATEWAY_FLAG: it would report success while leaving the old role
  # live. So the role change is handled here, before setup runs.
  local existing_env existing_gateway_flag requested_gateway_flag=""
  existing_env="$(
    container machine run --name "${MACHINE_NAME}" --root \
      /bin/cat /srv/sparkbox/sparkbox.env 2>/dev/null || true
  )"
  if [[ -n "${FLEET_GATEWAY}" ]]; then
    requested_gateway_flag="--gateway ${FLEET_GATEWAY} --node-name ${NODE_NAME}"
  fi
  if [[ -n "${existing_env}" ]]; then
    existing_gateway_flag="$(
      sed -n 's/^GATEWAY_FLAG=//p' <<<"${existing_env}" | tail -1
    )"
    if [[ "${existing_gateway_flag}" != "${requested_gateway_flag}" ]]; then
      switch_machine_role "${existing_gateway_flag}" "${requested_gateway_flag}"
    fi
  fi

  local timestamp result_dir
  timestamp="$(date -u +%Y%m%dT%H%M%SZ)"
  result_dir="${OUT_DIR}/results/${timestamp}"
  mkdir -p "${result_dir}"
  write_host_evidence "${result_dir}/host.txt"
  machine_inspect > "${result_dir}/machine-inspect-before.json"
  capture_gateway_storage > "${result_dir}/gateway-storage-before.txt"

  # Get a sparkbox into the machine BEFORE anything is written about this run:
  # the provision manifest reports the binary's real provenance, which is only
  # knowable once the machine has it.
  if using_source_build; then
    bootstrap_local_binary "${result_dir}/sparkbox-bootstrap.txt"
  else
    bootstrap_release_binary "${result_dir}/sparkbox-bootstrap.txt"
  fi
  read_bootstrap_provenance

  write_provision_manifest "${result_dir}/provision-manifest.txt" "${operator_key}"
  cp "${OUT_DIR}/inputs.txt" "${result_dir}/build-inputs.txt"
  cp "${OUT_DIR}/kernel-manifest.txt" "${result_dir}/kernel-manifest.txt"

  # Which tag setup pins its artifacts to. On the release path this is the
  # CONCRETE tag the manifest named, not what was asked for: `latest` resolved
  # once, so a release published between the bootstrap and setup cannot hand the
  # machine a kernel from a different build than the binary driving it. On the
  # source-build path the label is not a tag at all, so the requested release
  # stands — that machine's binary and artifacts are decoupled on purpose, which
  # is the whole reason that path shouts.
  local setup_release="${SPARKBOX_RELEASE}"
  if [[ "${BOOTSTRAP_SOURCE}" == "release" ]]; then
    setup_release="${BOOTSTRAP_RELEASE}"
  fi

  local remote_key_path=""
  # setup runs from the staged binary, not from /usr/local/bin/sparkbox: its
  # install-binary step (A1) copies the running executable to --bin-path, so
  # running it from the staging directory is what makes the installed binary and
  # the fetched artifacts the same release by construction.
  local setup_args=(
    "${BOOTSTRAP_BINARY}" setup
    --release "${setup_release}"
    --artifact-base "${ARTIFACT_BASE}"
    --proxy-domain "${PROXY_DOMAIN}"
    --data-volume-gb "${DATA_VOLUME_GB}"
    --swap-gb 0
  )
  if [[ -n "${FLEET_GATEWAY}" ]]; then
    setup_args+=(--gateway "${FLEET_GATEWAY}" --node-name "${NODE_NAME}")
  else
    # `container machine run` does not preserve spaces inside a single
    # argument, so transport the public key over stdin and give setup an
    # in-VM path.
    remote_key_path="/run/sparkbox-poc-operator.pub"
    printf '%s\n' "${operator_key}" \
      | container machine run --name "${MACHINE_NAME}" --root --interactive \
        /usr/bin/tee "${remote_key_path}" \
      >/dev/null
    container machine run --name "${MACHINE_NAME}" --root \
      /bin/chmod 0600 "${remote_key_path}"
    setup_args+=(
      --operator-handle "${OPERATOR_HANDLE}"
      --operator-key "${remote_key_path}"
    )
  fi

  echo "provisioning ${MACHINE_NAME} with sparkbox ${BOOTSTRAP_VERSION} (artifacts: ${setup_release})"
  local setup_status=0
  container machine run --name "${MACHINE_NAME}" --root \
    "${setup_args[@]}" \
    2>&1 | tee "${result_dir}/sparkbox-setup.txt" \
    || setup_status="$?"
  if [[ -n "${remote_key_path}" ]]; then
    container machine run --name "${MACHINE_NAME}" --root \
      /bin/rm -f "${remote_key_path}"
  fi
  [[ "${setup_status}" -eq 0 ]] || return "${setup_status}"

  echo
  echo "verifying provisioned gateway"
  reconcile_installed_binary 2>&1 | tee "${result_dir}/sparkbox-binary.txt"
  capture_gateway_health 2>&1 | tee "${result_dir}/sparkbox-doctor.txt"
  capture_gateway_storage > "${result_dir}/gateway-storage-after.txt"
  machine_inspect > "${result_dir}/machine-inspect-after.json"
  container machine run --name "${MACHINE_NAME}" --root \
    journalctl --no-pager -u sparkbox-net.service -u sparkbox.service \
    > "${result_dir}/sparkbox-journal.txt"

  echo
  if [[ -n "${FLEET_GATEWAY}" ]]; then
    echo "Sparkbox fleet node is provisioned"
    echo "  node:     ${NODE_NAME}"
    echo "  gateway:  ${FLEET_GATEWAY}"
  else
    echo "Sparkbox gateway is provisioned"
  fi
  echo "  machine:  ${MACHINE_NAME}"
  if [[ "${BOOTSTRAP_SOURCE}" == "release" ]]; then
    echo "  binary:   ${BOOTSTRAP_VERSION} (released sparkbox-linux-arm64, sha256 ${BOOTSTRAP_SHA256})"
  else
    echo "  binary:   ${BOOTSTRAP_VERSION}  *** LOCAL SOURCE BUILD, NOT A RELEASE ***"
  fi
  echo "  evidence: ${result_dir}"
}

status_machine() {
  require_container_runtime

  local inspect_json machine_state
  inspect_json="$(require_owned_machine)"
  machine_state="$(jq -r '.[0].status' <<<"${inspect_json}")"

  jq -r '.[0] |
    "machine:  \(.id)\n" +
    "state:    \(.status)\n" +
    "address:  \(.ipAddress // "-")\n" +
    "cpus:     \(.cpus)\n" +
    "memory:   \(.memory / 1073741824 | floor) GiB\n" +
    "disk:     \(.diskSize) bytes\n" +
    "image:    \(.image.reference)\n" +
    "home:     \(.homeMount)"' <<<"${inspect_json}"

  # Exit non-zero when the machine is not up. There is nothing further to
  # report, and callers (smoke.sh's readiness guard) rely on the status to tell
  # "ready" from "exists but stopped" — returning 0 here made that guard dead.
  if [[ "${machine_state}" != "running" ]]; then
    echo "${MACHINE_NAME} is ${machine_state}; run ./macos/poc.sh start" >&2
    return 1
  fi

  echo
  run_machine_script <<'EOF'
set -euo pipefail
printf "kernel:   %s\n" "$(uname -r)"
if [[ -c /dev/kvm && -w /dev/kvm ]]; then
  echo "kvm:      ready"
else
  echo "kvm:      unavailable"
fi
printf "data:     "
if mountpoint -q /srv/sparkbox/data; then
  findmnt -n -o FSTYPE,SOURCE,TARGET -T /srv/sparkbox/data
else
  echo "not mounted"
fi
role="standalone-gateway"
if grep -q '^GATEWAY_FLAG=--gateway' /srv/sparkbox/sparkbox.env 2>/dev/null; then
  role="fleet-node"
fi
printf "role:     %s\n" "${role}"
# The binary line exists because a machine that silently ran a source build
# while claiming a release is the bug this PoC shipped once already. It reports
# what is actually installed, and says so in capitals when it is not a release.
printf "binary:   "
if [[ -x /usr/local/bin/sparkbox ]]; then
  printf '%s' "$(/usr/local/bin/sparkbox version)"
else
  printf 'not installed (run ./macos/poc.sh provision)'
fi
if [[ -f /etc/sparkbox-poc-source-build ]]; then
  printf '  *** LOCAL SOURCE BUILD, NOT A RELEASE ***'
fi
printf '\n'
printf "services: "
service_states="$(systemctl is-active sparkbox-net.service sparkbox.service 2>/dev/null || true)"
printf '%s\n' "${service_states}" | paste -sd " " -
persisted=0
if [[ -d /srv/sparkbox/data/state/fc-vms ]]; then
  persisted="$(find /srv/sparkbox/data/state/fc-vms -mindepth 1 -maxdepth 1 -type d | wc -l)"
fi
running=0
for comm in /proc/[0-9]*/comm; do
  [[ -r "${comm}" ]] || continue
  read -r name < "${comm}" || true
  [[ "${name:-}" == firecracker ]] && running=$((running + 1))
done
printf "sandboxes: %s persisted, %s running\n" "${persisted}" "${running}"
EOF
}

stop_machine() {
  require_container_runtime
  local inspect_json machine_state
  inspect_json="$(require_owned_machine)"
  machine_state="$(jq -r '.[0].status' <<<"${inspect_json}")"
  if [[ "${machine_state}" == "stopped" ]]; then
    echo "${MACHINE_NAME} is already stopped"
    return
  fi

  echo "stopping ${MACHINE_NAME}"
  container machine stop "${MACHINE_NAME}"
  echo "${MACHINE_NAME} stopped"
}

start_machine() {
  require_container_runtime
  [[ "${SERVICE_WAIT_SECONDS}" =~ ^[0-9]+$ ]] \
    || die "SPARKBOX_SERVICE_WAIT_SECONDS must be an integer"

  local inspect_json machine_state
  inspect_json="$(require_owned_machine)"
  machine_state="$(jq -r '.[0].status' <<<"${inspect_json}")"
  if [[ "${machine_state}" == "running" ]]; then
    echo "${MACHINE_NAME} is already running"
  else
    echo "starting ${MACHINE_NAME}"
    container machine run --name "${MACHINE_NAME}" --root /bin/true
  fi

  local waited=0
  while [[ "${waited}" -lt "${SERVICE_WAIT_SECONDS}" ]]; do
    if container machine run --name "${MACHINE_NAME}" --root \
      systemctl is-active --quiet sparkbox.service; then
      echo "sparkbox.service is ready"
      status_machine
      return
    fi
    sleep 2
    waited=$((waited + 2))
  done

  container machine run --name "${MACHINE_NAME}" --root \
    journalctl --no-pager -n 100 -u sparkbox.service || true
  die "sparkbox.service was not ready after ${SERVICE_WAIT_SECONDS}s"
}

create_machine() {
  run_doctor || die "host doctor failed"
  [[ -s "${KERNEL_PATH}" ]] || die "missing ${KERNEL_PATH}; run ./macos/poc.sh build"
  [[ -s "${OUT_DIR}/inputs.txt" ]] || die "missing build inputs; run ./macos/poc.sh build"
  container image inspect "${GATEWAY_IMAGE}" >/dev/null \
    || die "missing ${GATEWAY_IMAGE}; run ./macos/poc.sh build"

  if machine_exists; then
    local existing_inspect
    existing_inspect="$(machine_inspect)"
    machine_is_owned "${existing_inspect}" \
      || die "${MACHINE_NAME} exists but is not owned by this PoC"
    echo "reusing existing ${MACHINE_NAME}"
  else
    local machine_status="$?"
    [[ "${machine_status}" -eq 1 ]] || die "could not list container machines"
    echo "creating ${MACHINE_NAME}"
    container machine create \
      --virtualization \
      --kernel "${KERNEL_PATH}" \
      --cpus "${MACHINE_CPUS}" \
      --memory "${MACHINE_MEMORY}" \
      --home-mount none \
      --name "${MACHINE_NAME}" \
      "${GATEWAY_IMAGE}"
  fi

  local timestamp result_dir
  timestamp="$(date -u +%Y%m%dT%H%M%SZ)"
  result_dir="${OUT_DIR}/results/${timestamp}"
  mkdir -p "${result_dir}"
  write_host_evidence "${result_dir}/host.txt"
  machine_inspect | tee "${result_dir}/machine-inspect.json"

  echo
  echo "verifying nested gateway"
  run_gateway_preflight "${result_dir}/gateway-preflight.txt"

  echo
  echo "persistent nested gateway is ready"
  echo "  machine:  ${MACHINE_NAME}"
  echo "  evidence: ${result_dir}"
}

# The outer-kernel products under macos/out, listed apart from the rest of the
# directory (inputs.txt, results/) because they are the expensive tier. The list
# covers both provenances: fetch.sh writes vmlinux-kvm + kernel-manifest.txt,
# build.sh writes those plus kernel.config and the 149MB downloads/ cache.
KERNEL_ARTIFACTS=(vmlinux-kvm kernel.config kernel-manifest.txt downloads)

# destroy_all tears down by cost tier. The tiers are separate because their
# costs differ by orders of magnitude: the machine is seconds to recreate, the
# gateway image is a multi-minute container build, and the outer kernel is an
# 8-CPU in-container Linux compile.
#
# The kernel is also the one artifact where deleting it buys nothing at all.
# It is now fetched from the release and verified against SHA256_OUTER_KERNEL,
# and fetch.sh reuses a copy whose checksum still matches, so re-getting it is a
# 29MB download at best and a full compile on SPARKBOX_KERNEL_SOURCE=build.
# (Deleting it is not a way to "get a clean rebuild" either: a rebuild is only
# guaranteed to be *a* valid kernel, not the same bytes — the builder's compiler
# version is embedded in it and the Ubuntu archive that supplies it is not
# pinned.) `--yes` used to mean all three tiers, which is how a role
# change — one line in sparkbox.env — ended up costing a kernel build. It now
# means the cheap tier unless a target says otherwise.
destroy_all() {
  local confirmed=0 targeted=0
  local want_machine=0 want_image=0 want_kernel=0 want_rest=0
  local arg
  for arg in "$@"; do
    case "${arg}" in
      --yes) confirmed=1 ;;
      --machine) want_machine=1; targeted=1 ;;
      --image) want_image=1; targeted=1 ;;
      --kernel) want_kernel=1; targeted=1 ;;
      --all)
        want_machine=1
        want_image=1
        want_kernel=1
        want_rest=1
        targeted=1
        ;;
      *) die "unknown destroy option ${arg}; usage: ./macos/poc.sh destroy [--machine] [--image] [--kernel] [--all] --yes" ;;
    esac
  done
  [[ "${targeted}" -eq 1 ]] || want_machine=1
  [[ "${confirmed}" -eq 1 ]] \
    || die "destroy is destructive; rerun with --yes. Targets: --machine (the default) --image --kernel --all"

  if [[ "${want_machine}" -eq 1 ]] || [[ "${want_image}" -eq 1 ]]; then
    require_container_runtime
  fi

  if [[ "${want_machine}" -eq 1 ]]; then
    if machine_exists; then
      local inspect_json
      inspect_json="$(machine_inspect)"
      machine_is_owned "${inspect_json}" \
        || die "refusing to delete unexpected machine ${MACHINE_NAME}"
      container machine delete "${MACHINE_NAME}"
      echo "deleted machine ${MACHINE_NAME}"
    else
      local machine_status="$?"
      [[ "${machine_status}" -eq 1 ]] || die "could not list container machines"
      echo "machine ${MACHINE_NAME} does not exist"
    fi
  fi

  if [[ "${want_image}" -eq 1 ]]; then
    if container image inspect "${GATEWAY_IMAGE}" >/dev/null 2>&1; then
      container image delete --force "${GATEWAY_IMAGE}"
      echo "deleted local image ${GATEWAY_IMAGE}"
    else
      echo "local image ${GATEWAY_IMAGE} does not exist"
    fi
    # The staged build context belongs to the image tier: it is regenerated by
    # `build` and is meaningless without the image it produced.
    if [[ -d "${IMAGE_CONTEXT}" ]] && [[ "${IMAGE_CONTEXT}" == "${OUT_DIR}/image-context" ]]; then
      rm -rf -- "${IMAGE_CONTEXT}"
      echo "deleted the staged image build context"
    fi
  fi

  if [[ "${want_kernel}" -eq 1 ]] && [[ "${OUT_DIR}" == "${SCRIPT_DIR}/out" ]]; then
    local artifact removed=0
    for artifact in "${KERNEL_ARTIFACTS[@]}"; do
      if [[ -e "${OUT_DIR}/${artifact}" ]]; then
        rm -rf -- "${OUT_DIR:?}/${artifact}"
        removed=1
      fi
    done
    if [[ "${removed}" -eq 1 ]]; then
      echo "deleted the outer kernel in ${OUT_DIR} (re-fetch: ./macos/poc.sh build)"
    else
      echo "no outer kernel in ${OUT_DIR}"
    fi
  fi

  # Everything else under macos/out: inputs.txt and the results/ evidence
  # bundles that create/provision wrote. Only --all reaches these, because they
  # are the record of what was run and nothing regenerates them.
  if [[ "${want_rest}" -eq 1 ]] && [[ -d "${OUT_DIR}" ]] && [[ "${OUT_DIR}" == "${SCRIPT_DIR}/out" ]]; then
    rm -rf -- "${OUT_DIR}"
    echo "deleted generated output ${OUT_DIR} (including the results/ evidence bundles)"
  fi

  if [[ "${want_kernel}" -eq 0 ]] && [[ -s "${KERNEL_PATH}" ]]; then
    echo "kept the outer kernel ${KERNEL_PATH} (delete it with --kernel)"
  fi
}

command_name="${1:-}"
case "${command_name}" in
  doctor)
    [[ "$#" -eq 1 ]] || die "doctor takes no arguments"
    run_doctor
    ;;
  build)
    [[ "$#" -eq 1 ]] || die "build takes no arguments"
    build_all
    ;;
  create)
    [[ "$#" -eq 1 ]] || die "create takes no arguments"
    create_machine
    ;;
  provision)
    [[ "$#" -eq 1 ]] || die "provision takes no arguments"
    provision_machine
    ;;
  status)
    [[ "$#" -eq 1 ]] || die "status takes no arguments"
    status_machine
    ;;
  stop)
    [[ "$#" -eq 1 ]] || die "stop takes no arguments"
    stop_machine
    ;;
  start)
    [[ "$#" -eq 1 ]] || die "start takes no arguments"
    start_machine
    ;;
  smoke)
    [[ "$#" -eq 1 ]] || die "smoke takes no arguments"
    if container machine run --name "${MACHINE_NAME}" --root \
      /usr/bin/grep -q '^GATEWAY_FLAG=--gateway' /srv/sparkbox/sparkbox.env; then
      die "smoke exercises a standalone gateway; this machine is configured as a fleet node"
    fi
    "${SMOKE_SCRIPT}"
    ;;
  destroy)
    shift
    destroy_all "$@"
    ;;
  -h|--help|help)
    usage
    ;;
  *)
    usage >&2
    exit 2
    ;;
esac
