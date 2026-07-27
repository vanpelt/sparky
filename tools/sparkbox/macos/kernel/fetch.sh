#!/bin/bash
# Fetch the macOS outer kernel from a sparkbox release instead of compiling it.
#
# THIS IS THE DEFAULT PATH. macos/kernel/build.sh — which downloads 149MB of
# Linux source and spends several minutes compiling
# it — is now the escape hatch, taken only when SPARKBOX_KERNEL_SOURCE=build.
# Nobody should compile a kernel as an onboarding step; CI restores the
# content-addressed build when its inputs are unchanged and publishes it as
# `vmlinux-macos-arm64`.
#
# The asset is verified against SHA256_OUTER_KERNEL in the same release's
# manifest-darwin-arm64.env, exactly the way `sparkbox setup` verifies the guest
# kernel, firecracker and the rootfs. That checksum is an INTEGRITY claim — this
# is the file that release published. CI's adjacent kernel manifest records the
# exact builder digest and content key; see the reproducibility note in build.sh.
#
# Output is byte-for-byte what build.sh would have written, at the same paths,
# so poc.sh, `container machine create --kernel`, the evidence bundle and
# `destroy --kernel` cannot tell the two apart except by kernel_source= in the
# manifest:
#   macos/out/vmlinux-kvm          the Image the machine boots
#   macos/out/kernel-manifest.txt  provenance (kernel_source=release)
#
# Usage:
#   SPARKBOX_RELEASE=v0.5.0 ./macos/kernel/fetch.sh
# Env:
#   SPARKBOX_RELEASE        release tag, or "latest" (default: latest)
#   SPARKBOX_ARTIFACT_BASE  release base URL (default: GitHub Releases)
#   SPARKBOX_KERNEL_OUT_DIR where to write (default: macos/out)
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
MACOS_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
OUT_DIR="${SPARKBOX_KERNEL_OUT_DIR:-${MACOS_DIR}/out}"
KERNEL_OUTPUT="${OUT_DIR}/vmlinux-kvm"
MANIFEST_OUTPUT="${OUT_DIR}/kernel-manifest.txt"

RELEASE="${SPARKBOX_RELEASE:-latest}"
ARTIFACT_BASE="${SPARKBOX_ARTIFACT_BASE:-https://github.com/vanpelt/sparky/releases}"

# The Mac's own manifest, not the linux one. manifest-arm64.env parses cleanly
# on a Mac and every checksum in it is correct — for linux binaries — which is
# precisely the confusion the darwin-qualified name exists to prevent.
DARWIN_MANIFEST="manifest-darwin-arm64.env"
DEFAULT_KERNEL_ASSET="vmlinux-macos-arm64"

die() {
  echo "error: $*" >&2
  exit 1
}

sha256_file() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  else
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}

# Mirrors hostsetup's assetURL and sparkbox-bootstrap's asset_url: an empty or
# "latest" tag rides GitHub's /releases/latest/download redirect, which only a
# published non-prerelease release moves.
asset_url() {
  local base="${1%/}" release="$2" name="$3"
  if [[ -z "${release}" || "${release}" == "latest" ]]; then
    printf '%s/latest/download/%s\n' "${base}" "${name}"
  else
    printf '%s/download/%s/%s\n' "${base}" "${release}" "${name}"
  fi
}

# Never source a manifest: GATEWAY_PUBKEY is a quoted value with spaces, and
# sourcing a file we just downloaded would execute it.
manifest_value() {
  sed -n "s/^$1=//p" "$2" | tail -1 | tr -d '"'
}

fetch() {
  curl --fail --silent --show-error --location \
    --proto '=https' --tlsv1.2 --retry 3 --max-time 600 \
    --output "$2" "$1"
}

command -v curl >/dev/null || die "curl is required"
mkdir -p "${OUT_DIR}"

tmp="$(mktemp -d "${TMPDIR:-/tmp}/sparkbox-kernel.XXXXXX")"
# shellcheck disable=SC2064  # expand tmp now: the trap must outlive this scope
trap "rm -rf -- '${tmp}'" EXIT

manifest="${tmp}/${DARWIN_MANIFEST}"
echo "fetching ${DARWIN_MANIFEST} for ${RELEASE}"
fetch "$(asset_url "${ARTIFACT_BASE}" "${RELEASE}" "${DARWIN_MANIFEST}")" "${manifest}" \
  || die "could not fetch ${DARWIN_MANIFEST} for release '${RELEASE}' from ${ARTIFACT_BASE}. Check SPARKBOX_RELEASE, or that the release is published (a draft release has no downloadable assets). Releases cut before the macOS kernel shipped have no darwin manifest at all — build it locally instead: SPARKBOX_KERNEL_SOURCE=build ./macos/poc.sh build"

# Resolve "latest" once, here, and address the asset by the concrete tag. Asking
# for latest twice can straddle a release published between the two requests and
# hand back a kernel the first manifest never described.
resolved="$(manifest_value RELEASE "${manifest}")"
[[ -n "${resolved}" ]] || resolved="${RELEASE}"

platform="$(manifest_value PLATFORM "${manifest}")"
[[ "${platform}" == "darwin" ]] \
  || die "${DARWIN_MANIFEST} for ${resolved} declares PLATFORM='${platform}', expected darwin"

expected="$(manifest_value SHA256_OUTER_KERNEL "${manifest}")"
[[ "${expected}" =~ ^[0-9a-f]{64}$ ]] \
  || die "release ${resolved} has no usable SHA256_OUTER_KERNEL in ${DARWIN_MANIFEST} (got '${expected}'). Without it the kernel cannot be verified, and an unverified kernel is the one file on this host that nothing downstream can check"

# Asset names are data when the manifest carries them (same convention as
# ROOTFS_ASSET), so renaming the asset in the release pipeline does not need a
# matching edit here.
asset="$(manifest_value OUTER_KERNEL_ASSET "${manifest}")"
[[ -n "${asset}" ]] || asset="${DEFAULT_KERNEL_ASSET}"

if [[ -f "${KERNEL_OUTPUT}" ]] && [[ "$(sha256_file "${KERNEL_OUTPUT}")" == "${expected}" ]]; then
  echo "reusing verified ${KERNEL_OUTPUT} (${asset} ${resolved})"
else
  echo "downloading ${asset} ${resolved}"
  fetch "$(asset_url "${ARTIFACT_BASE}" "${resolved}" "${asset}")" "${tmp}/vmlinux-kvm" \
    || die "could not download ${asset} for ${resolved} from ${ARTIFACT_BASE}"
  actual="$(sha256_file "${tmp}/vmlinux-kvm")"
  [[ "${actual}" == "${expected}" ]] \
    || die "${asset} sha256 mismatch: expected ${expected}, got ${actual}. Refusing to boot an unverified kernel"
  # mv into place only after verification, so an interrupted or corrupt download
  # never leaves a plausible-looking kernel behind for the next run to adopt.
  mv -f "${tmp}/vmlinux-kvm" "${KERNEL_OUTPUT}"
  chmod 0644 "${KERNEL_OUTPUT}"
  echo "verified sha256 ${expected}"
fi

# Same filename and the same kernel_source/kernel_sha256 keys build.sh writes,
# because poc.sh copies this file into every provision evidence bundle and the
# question it has to answer — which kernel is this machine booting, and where
# did it come from — is the same either way.
{
  printf 'kernel_source=release\n'
  printf 'release=%s\n' "${resolved}"
  printf 'artifact_base=%s\n' "${ARTIFACT_BASE}"
  printf 'kernel_asset=%s\n' "${asset}"
  printf 'kernel_url=%s\n' "$(asset_url "${ARTIFACT_BASE}" "${resolved}" "${asset}")"
  printf 'kernel_sha256=%s\n' "${expected}"
  printf 'fetched_at=%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
} > "${MANIFEST_OUTPUT}"

echo "kernel:   ${KERNEL_OUTPUT}"
echo "manifest: ${MANIFEST_OUTPUT}"
