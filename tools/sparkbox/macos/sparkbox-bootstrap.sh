#!/bin/bash
# sparkbox-bootstrap — put a sparkbox binary inside the nested gateway machine
# so that `sparkbox setup` can install itself.
#
# WHY THIS EXISTS
#
# The gateway image used to compile sparkbox from source in a Go build stage and
# bake the result at /usr/local/bin/sparkbox. That made the machine's binary and
# the release it provisions two independent things, and they drifted: a machine
# built from commit v0.3.0-5-g18bfe3b provisioned `--release v0.4.0`, so it
# fetched v0.4.0 kernels, firecracker and rootfs and then ran them under a
# v0.3.0-era control plane. Both facts were recorded in the same evidence bundle
# (build-inputs.txt said v0.3.0, provision-manifest.txt said v0.4.0) and nothing
# compared them, so the PoC reported PASS for a combination no release ever
# produced.
#
# The image now bakes NO sparkbox at all. This script fetches the released
# sparkbox-linux-<arch> for the tag being provisioned, verifies it against
# SHA256_SPARKBOX in that same release's manifest-<arch>.env, and drops it in a
# staging directory. poc.sh then runs `setup` from there, and setup's own
# install-binary step (A1) copies it to /usr/local/bin/sparkbox. The binary that
# runs the box and the release whose artifacts it fetched are therefore the same
# build, by construction — there is no second copy that could differ.
#
# An unverified download into a privileged image would be worse than the source
# build it replaces, so the checksum is not optional: a missing or malformed
# SHA256_SPARKBOX is a hard failure, not a warning.
#
# MODES
#   sparkbox-bootstrap release [<tag>] [<artifact-base>]
#       Fetch + verify the released binary. This is the only mode poc.sh uses
#       unless a developer explicitly opts out.
#
#       This reads the LINUX manifest (manifest-<arch>.env) from inside the
#       machine, because that is where the fetch happens. A darwin host's
#       manifest-darwin-arm64.env names the same file from the outside, as
#       MACHINE_SPARKBOX_ASSET / SHA256_MACHINE_SPARKBOX; when `sparkbox setup`
#       learns to provision a machine on darwin (B4) it can push that binary in
#       instead of running this mode, and the two must agree — they will, since
#       SHA256_MACHINE_SPARKBOX is derived from this manifest's SHA256_SPARKBOX.
#   sparkbox-bootstrap local <sha256> [<label>] [<path>]
#       Install a binary the host pushed in (default path "-" reads stdin),
#       verify it against the sha256 the host computed, and mark it as a *source
#       build*. This is the documented escape hatch for developing against
#       uncommitted changes. It announces itself loudly and leaves
#       /etc/sparkbox-poc-source-build behind so `poc.sh status`, the evidence
#       bundle and anyone reading the machine can see that this box is NOT
#       running a release.
set -euo pipefail

# The two path overrides exist so this script can be exercised outside a machine
# (see macos/test-bootstrap.sh); poc.sh never sets them, and the defaults are the
# only paths the machine ever uses.
BOOTSTRAP_DIR="${SPARKBOX_BOOTSTRAP_DIR:-/var/lib/sparkbox-bootstrap}"
BOOTSTRAP_BINARY="${BOOTSTRAP_DIR}/sparkbox"
BOOTSTRAP_RESULT="${BOOTSTRAP_DIR}/release.env"
SOURCE_BUILD_MARKER="${SPARKBOX_BOOTSTRAP_MARKER:-/etc/sparkbox-poc-source-build}"
DEFAULT_ARTIFACT_BASE="https://github.com/vanpelt/sparky/releases"

die() {
  echo "sparkbox-bootstrap: $*" >&2
  exit 1
}

# guest_arch maps uname -m onto the GOARCH spelling release assets carry. The
# release namespace is flat and every asset is arch-suffixed Go-style
# (sparkbox-linux-arm64, manifest-arm64.env), so this is the only place the two
# vocabularies meet.
guest_arch() {
  case "$(uname -m)" in
    aarch64 | arm64) printf 'arm64\n' ;;
    x86_64 | amd64) printf 'amd64\n' ;;
    *) die "unsupported machine architecture $(uname -m); sparkbox ships linux/amd64 and linux/arm64" ;;
  esac
}

# asset_url mirrors hostsetup's assetURL exactly, including the "latest" case:
# an empty or "latest" tag rides GitHub's /releases/latest/download redirect,
# which only a published, non-prerelease release moves. Any other tag is a
# direct /releases/download/<tag>/ URL.
asset_url() {
  local base="${1%/}" release="$2" name="$3"
  if [[ -z "${release}" || "${release}" == "latest" ]]; then
    printf '%s/latest/download/%s\n' "${base}" "${name}"
  else
    printf '%s/download/%s/%s\n' "${base}" "${release}" "${name}"
  fi
}

sha_of() {
  sha256sum "$1" | awk '{print $1}'
}

# manifest_value reads one KEY=VALUE line out of a manifest-<arch>.env. The
# manifest also carries GATEWAY_PUBKEY="ssh-ed25519 AAAA... comment", i.e. a
# quoted value with spaces, so the file is never sourced — a parse is enough for
# the two keys this script needs and cannot execute anything.
manifest_value() {
  sed -n "s/^$1=//p" "$2" | tail -1 | tr -d '"'
}

fetch() {
  # --fail turns an HTML 404 page into a non-zero exit instead of a file that
  # hashes to nonsense; --proto '=https' refuses a plain-HTTP redirect.
  curl --fail --silent --show-error --location \
    --proto '=https' --tlsv1.2 --retry 3 --max-time 300 \
    --output "$2" "$1"
}

install_verified() {
  local staged="$1" expected="$2" what="$3" actual
  actual="$(sha_of "${staged}")"
  if [[ "${actual}" != "${expected}" ]]; then
    die "${what} sha256 mismatch: expected ${expected}, got ${actual}. Refusing to install an unverified binary into a privileged machine"
  fi
  mkdir -p "${BOOTSTRAP_DIR}"
  chmod 0755 "${staged}"
  mv -f "${staged}" "${BOOTSTRAP_BINARY}"
}

# reported_version prints the version field of `sparkbox version`, whose output
# is "sparkbox <version> (<goos>/<goarch>)". An empty answer means the file is
# not a runnable sparkbox for this machine, which is itself worth reporting.
reported_version() {
  "${BOOTSTRAP_BINARY}" version 2>/dev/null | awk 'NR == 1 {print $2}'
}

write_result() {
  mkdir -p "${BOOTSTRAP_DIR}"
  {
    printf 'SOURCE=%s\n' "$1"
    printf 'RELEASE=%s\n' "$2"
    printf 'SHA256_SPARKBOX=%s\n' "$3"
    printf 'VERSION=%s\n' "$4"
    printf 'BINARY=%s\n' "${BOOTSTRAP_BINARY}"
    printf 'FETCHED_AT=%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  } > "${BOOTSTRAP_RESULT}"
}

cmd_release() {
  local release="${1:-latest}"
  local base="${2:-${DEFAULT_ARTIFACT_BASE}}"
  local arch tmp manifest resolved expected asset platform reported

  arch="$(guest_arch)"
  tmp="$(mktemp -d /var/tmp/sparkbox-bootstrap.XXXXXX)"
  # shellcheck disable=SC2064  # expand tmp now: the trap must survive its scope
  trap "rm -rf -- '${tmp}'" EXIT

  manifest="${tmp}/manifest-${arch}.env"
  echo "fetching manifest-${arch}.env for ${release}"
  fetch "$(asset_url "${base}" "${release}" "manifest-${arch}.env")" "${manifest}" \
    || die "could not fetch the manifest for release '${release}' from ${base}. Check SPARKBOX_RELEASE, or that the release is published (a draft release has no downloadable assets)"

  # Resolve "latest" to the concrete tag the manifest names, and use THAT for
  # the binary URL. Asking for latest twice could otherwise straddle a release
  # published between the two requests and hand us a binary whose checksum the
  # first manifest never described.
  resolved="$(manifest_value RELEASE "${manifest}")"
  [[ -n "${resolved}" ]] || resolved="${release}"
  expected="$(manifest_value SHA256_SPARKBOX "${manifest}")"
  [[ "${expected}" =~ ^[0-9a-f]{64}$ ]] \
    || die "release ${resolved} has no usable SHA256_SPARKBOX in manifest-${arch}.env (got '${expected}'). Without it the binary cannot be verified, and an unverified download into this machine is worse than no bootstrap at all"

  # PLATFORM appeared when darwin manifests did; the unqualified
  # manifest-<arch>.env name still means linux, which is what this machine is.
  # Older manifests omit the key, so only a contradicting value is an error.
  platform="$(manifest_value PLATFORM "${manifest}")"
  [[ -z "${platform}" || "${platform}" == "linux" ]] \
    || die "manifest-${arch}.env for ${resolved} declares PLATFORM=${platform}; this machine runs linux"

  # The asset name is data when the manifest carries it (the same convention as
  # ROOTFS_ASSET), so a rename in the release pipeline does not need a matching
  # edit here. Releases cut before that key existed get the constructed name.
  asset="$(manifest_value SPARKBOX_ASSET "${manifest}")"
  [[ -n "${asset}" ]] || asset="sparkbox-linux-${arch}"
  # A cached copy is re-verified rather than trusted for existing, the same way
  # kernel/build.sh re-checksums a cached tarball: a truncated download must
  # never be silently adopted on the next run.
  if [[ -f "${BOOTSTRAP_BINARY}" ]] && [[ "$(sha_of "${BOOTSTRAP_BINARY}")" == "${expected}" ]]; then
    echo "reusing verified ${BOOTSTRAP_BINARY} (${asset} ${resolved})"
    chmod 0755 "${BOOTSTRAP_BINARY}"
  else
    echo "downloading ${asset} ${resolved}"
    fetch "$(asset_url "${base}" "${resolved}" "${asset}")" "${tmp}/sparkbox" \
      || die "could not download ${asset} for ${resolved} from ${base}"
    install_verified "${tmp}/sparkbox" "${expected}" "${asset}"
    echo "verified sha256 ${expected}"
  fi

  reported="$(reported_version)"
  [[ -n "${reported}" ]] \
    || die "${BOOTSTRAP_BINARY} did not run on this machine; the ${arch} asset may be corrupt or built for another platform"
  # The whole point of B3: the binary must be the one this release stamped. A
  # mismatch means the release's own -X main.version and its tag disagree, which
  # is exactly the silent skew this script exists to make impossible.
  [[ "${reported}" == "${resolved}" ]] \
    || die "${asset} from release ${resolved} reports version '${reported}'. That is the release/binary skew this bootstrap exists to prevent; do not provision with it"

  rm -f "${SOURCE_BUILD_MARKER}"
  write_result release "${resolved}" "${expected}" "${reported}"
  echo "sparkbox ${reported} staged at ${BOOTSTRAP_BINARY} (setup will install it)"
}

cmd_local() {
  local expected="${1:-}" label="${2:-local-source-build}" staged="${3:--}"
  local tmp reported

  # printf, not a heredoc: this runs as the first thing a source-build install
  # does, and it must not depend on /bin/cat or on bash spilling the body to a
  # temp file. (It also keeps macos/test-bootstrap.sh runnable under a sandboxed
  # shell, where a heredoc large enough to be staged in a temp file can block.)
  printf '%s\n' >&2 \
    '###########################################################################' \
    '##                                                                       ##' \
    '##  SPARKBOX SOURCE BUILD - THIS MACHINE WILL NOT RUN A RELEASE BINARY   ##' \
    '##                                                                       ##' \
    '##  A locally built sparkbox is being installed. Its behaviour is NOT    ##' \
    '##  the behaviour of any published release, and any result measured on   ##' \
    '##  this machine is evidence about your working tree, not about the      ##' \
    '##  release whose artifacts it provisions.                               ##' \
    '##                                                                       ##' \
    '##  Unset SPARKBOX_SOURCE_BUILD / SPARKBOX_LOCAL_BINARY to go back to    ##' \
    '##  the released binary.                                                 ##' \
    '##                                                                       ##' \
    '###########################################################################'

  [[ "${expected}" =~ ^[0-9a-f]{64}$ ]] \
    || die "local mode needs the host-computed sha256 of the binary as its first argument (got '${expected}')"

  tmp="$(mktemp -d /var/tmp/sparkbox-bootstrap.XXXXXX)"
  # shellcheck disable=SC2064
  trap "rm -rf -- '${tmp}'" EXIT

  # How the binary got here: `container machine run` gives exactly one byte
  # stream per call and there is no shared filesystem (the machine is created
  # with --home-mount none, and gateway-verify.sh asserts no /Users virtiofs
  # mount). Measured on container CLI 1.1.0: a single stdin stream is byte-exact
  # up to 128 KiB and DEADLOCKS at 192 KiB — the CLI stops draining and neither
  # side can make progress, and the hang survives SIGTERM to the pipeline. A
  # 24MB binary therefore cannot arrive in one call, so poc.sh appends it in
  # 64 KiB chunks with dd (373 chunks, 37s, byte-exact when measured) and passes
  # the assembled path here. The stdin form ("-") is kept for small payloads and
  # for the tests.
  #
  # Either way the checksum below is what makes the transport trustworthy: a
  # dropped or duplicated chunk is a refusal, not a subtly wrong binary running
  # as root.
  if [[ "${staged}" == "-" ]]; then
    cat > "${tmp}/sparkbox" || die "could not read the binary from stdin"
  else
    [[ -f "${staged}" ]] || die "no staged binary at ${staged}"
    mv "${staged}" "${tmp}/sparkbox" || die "could not read the staged binary at ${staged}"
  fi
  install_verified "${tmp}/sparkbox" "${expected}" "local sparkbox build"

  reported="$(reported_version)"
  [[ -n "${reported}" ]] \
    || die "${BOOTSTRAP_BINARY} did not run on this machine; cross-compile it with GOOS=linux GOARCH=$(guest_arch)"

  printf 'sparkbox-poc-source-build=1\nlabel=%s\nsha256=%s\nversion=%s\ninstalled_at=%s\n' \
    "${label}" "${expected}" "${reported}" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    > "${SOURCE_BUILD_MARKER}"
  write_result local "${label}" "${expected}" "${reported}"
  echo "SOURCE BUILD sparkbox ${reported} staged at ${BOOTSTRAP_BINARY}" >&2
}

case "${1:-}" in
  release)
    shift
    cmd_release "$@"
    ;;
  local)
    shift
    cmd_local "$@"
    ;;
  *)
    die "usage: sparkbox-bootstrap release [<tag>] [<artifact-base>] | sparkbox-bootstrap local <sha256> [<label>] [<path>|-]"
    ;;
esac
