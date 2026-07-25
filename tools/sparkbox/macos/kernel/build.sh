#!/bin/bash
set -euo pipefail

LINUX_VERSION="6.14.9"
LINUX_URL="https://cdn.kernel.org/pub/linux/kernel/v6.x/linux-${LINUX_VERSION}.tar.xz"
LINUX_SHA256="390cdde032719925a08427270197ef55db4e90c09d454e9c3554157292c9f361"

APPLE_CONFIG_TAG="0.5.0"
APPLE_CONFIG_URL="https://raw.githubusercontent.com/apple/containerization/${APPLE_CONFIG_TAG}/kernel/config-arm64"
APPLE_CONFIG_SHA256="0b05408d7d5f5d5e941d89767780dc87a2e90e2f8ef20ec6f8cf11a3037f9f36"

KERNEL_BUILD_IMAGE="docker.io/library/ubuntu:24.04@sha256:4fbb8e6a8395de5a7550b33509421a2bafbc0aab6c06ba2cef9ebffbc7092d90"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
MACOS_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
OUT_DIR="${MACOS_DIR}/out"
DOWNLOAD_DIR="${OUT_DIR}/downloads"
SOURCE_ARCHIVE="${DOWNLOAD_DIR}/linux-${LINUX_VERSION}.tar.xz"
APPLE_CONFIG="${DOWNLOAD_DIR}/apple-config-arm64-${APPLE_CONFIG_TAG}"
FRAGMENT="${SCRIPT_DIR}/sparkbox-arm64.fragment"
KERNEL_OUTPUT="${OUT_DIR}/vmlinux-kvm"
CONFIG_OUTPUT="${OUT_DIR}/kernel.config"
MANIFEST_OUTPUT="${OUT_DIR}/kernel-manifest.txt"

BUILD_CPUS="${SPARKBOX_BUILD_CPUS:-8}"
BUILD_MEMORY="${SPARKBOX_KERNEL_BUILD_MEMORY:-16G}"

die() {
  echo "error: $*" >&2
  exit 1
}

sha256_file() {
  shasum -a 256 "$1" | awk '{print $1}'
}

verify_sha256() {
  local path="$1"
  local expected="$2"
  local actual
  actual="$(sha256_file "${path}")"
  [[ "${actual}" == "${expected}" ]] \
    || die "checksum mismatch for ${path}: got ${actual}, want ${expected}"
}

download_verified() {
  local url="$1"
  local path="$2"
  local expected="$3"
  local tmp="${path}.tmp.$$"

  if [[ -f "${path}" ]]; then
    verify_sha256 "${path}" "${expected}"
    echo "using verified $(basename "${path}")"
    return
  fi

  trap 'rm -f -- "${tmp}"' RETURN
  echo "downloading ${url}"
  curl --fail --location --silent --show-error \
    --proto '=https' --tlsv1.2 --retry 3 \
    --output "${tmp}" "${url}"
  verify_sha256 "${tmp}" "${expected}"
  mv "${tmp}" "${path}"
  trap - RETURN
}

build_inside_container() {
  export DEBIAN_FRONTEND=noninteractive
  apt-get update
  apt-get install -y --no-install-recommends \
    bc \
    bison \
    build-essential \
    ca-certificates \
    flex \
    libelf-dev \
    libssl-dev \
    python3 \
    xz-utils
  rm -rf /var/lib/apt/lists/*

  rm -rf /build
  mkdir -p /build
  tar -xf "/out/downloads/linux-${LINUX_VERSION}.tar.xz" \
    -C /build --strip-components=1
  cp "/out/downloads/apple-config-arm64-${APPLE_CONFIG_TAG}" /build/.config

  cd /build
  scripts/kconfig/merge_config.sh -m .config /src/sparkbox-arm64.fragment
  make olddefconfig

  local jobs
  jobs="$(nproc)"
  if [[ "${jobs}" -gt 1 ]]; then
    jobs=$((jobs - 1))
  fi

  export KBUILD_BUILD_USER=sparkbox
  export KBUILD_BUILD_HOST=apple-container
  export KBUILD_BUILD_TIMESTAMP="2025-05-29 09:26:00 UTC"
  make -j"${jobs}" Image

  cp arch/arm64/boot/Image /out/vmlinux-kvm
  cp .config /out/kernel.config
}

if [[ "${SPARKBOX_KERNEL_BUILD_CONTAINER:-0}" == "1" ]]; then
  build_inside_container
  exit 0
fi

[[ "$(uname -s)" == "Darwin" ]] || die "the kernel builder must run on macOS"
[[ "$(uname -m)" == "arm64" ]] || die "the kernel builder requires Apple Silicon"
command -v container >/dev/null || die "Apple Container CLI is not installed"
command -v curl >/dev/null || die "curl is required"
command -v shasum >/dev/null || die "shasum is required"
[[ -f "${FRAGMENT}" ]] || die "missing ${FRAGMENT}"

mkdir -p "${DOWNLOAD_DIR}"
download_verified "${LINUX_URL}" "${SOURCE_ARCHIVE}" "${LINUX_SHA256}"
download_verified "${APPLE_CONFIG_URL}" "${APPLE_CONFIG}" "${APPLE_CONFIG_SHA256}"

echo "building Linux ${LINUX_VERSION} in a disposable ARM64 container"
container run --rm --arch arm64 \
  --cpus "${BUILD_CPUS}" \
  --memory "${BUILD_MEMORY}" \
  --volume "${SCRIPT_DIR}:/src:ro" \
  --volume "${OUT_DIR}:/out" \
  --env SPARKBOX_KERNEL_BUILD_CONTAINER=1 \
  "${KERNEL_BUILD_IMAGE}" \
  /bin/bash /src/build.sh

required_symbols=(
  CONFIG_KVM
  CONFIG_VIRTUALIZATION
  CONFIG_TUN
  CONFIG_BLK_DEV_LOOP
  CONFIG_XFS_FS
  CONFIG_EXT4_FS
  CONFIG_NETFILTER
  CONFIG_IP_NF_IPTABLES
  CONFIG_IP_NF_NAT
  CONFIG_NETFILTER_XT_TARGET_REDIRECT
)
for symbol in "${required_symbols[@]}"; do
  grep -qx "${symbol}=y" "${CONFIG_OUTPUT}" \
    || die "${CONFIG_OUTPUT} does not contain ${symbol}=y"
done
grep -qx 'CONFIG_LOCALVERSION="-sparkbox-poc"' "${CONFIG_OUTPUT}" \
  || die "${CONFIG_OUTPUT} has the wrong local version"

fragment_sha256="$(sha256_file "${FRAGMENT}")"
config_sha256="$(sha256_file "${CONFIG_OUTPUT}")"
kernel_sha256="$(sha256_file "${KERNEL_OUTPUT}")"

{
  printf 'linux_version=%s\n' "${LINUX_VERSION}"
  printf 'linux_url=%s\n' "${LINUX_URL}"
  printf 'linux_sha256=%s\n' "${LINUX_SHA256}"
  printf 'apple_config_tag=%s\n' "${APPLE_CONFIG_TAG}"
  printf 'apple_config_url=%s\n' "${APPLE_CONFIG_URL}"
  printf 'apple_config_sha256=%s\n' "${APPLE_CONFIG_SHA256}"
  printf 'kernel_build_image=%s\n' "${KERNEL_BUILD_IMAGE}"
  printf 'fragment_sha256=%s\n' "${fragment_sha256}"
  printf 'config_sha256=%s\n' "${config_sha256}"
  printf 'kernel_sha256=%s\n' "${kernel_sha256}"
} > "${MANIFEST_OUTPUT}"

echo "kernel:   ${KERNEL_OUTPUT}"
echo "config:   ${CONFIG_OUTPUT}"
echo "manifest: ${MANIFEST_OUTPUT}"
