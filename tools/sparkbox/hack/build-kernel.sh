#!/usr/bin/env bash
# Build the sparkbox guest kernel: the Firecracker CI microvm 6.1 config plus
# our deltas (kernel-config.fragment — docker container networking + TUN for
# tailscale). The stock firecracker-ci vmlinux omits these, so a sandbox can't
# run docker or tailscale. Produces the uncompressed vmlinux ELF the firecracker
# driver boots (with the PVH note the base config's CONFIG_PVH=y emits).
#
# Runs on any x86_64 Linux build host (a CI ubuntu runner or the sparkbox EM
# box). Installs build deps via apt when missing. Reproducible: pin KVER +
# BASE_CONFIG_URL and the same inputs yield the same config.
#
#   KVER=6.1.155 ./build-kernel.sh              # -> ../vmlinux
#   OUT=/tmp/vmlinux KVER=6.1.155 ./build-kernel.sh
set -euo pipefail

KVER=${KVER:-6.1.155}
HERE=$(cd "$(dirname "$0")" && pwd)
OUT=${OUT:-$HERE/../vmlinux}
WORK=${WORK:-/tmp/sparkbox-kbuild}
JOBS=${JOBS:-$(nproc)}
FRAGMENT=${FRAGMENT:-$HERE/kernel-config.fragment}
BASE_CONFIG_URL=${BASE_CONFIG_URL:-https://raw.githubusercontent.com/firecracker-microvm/firecracker/main/resources/guest_configs/microvm-kernel-ci-x86_64-6.1.config}
SRC_URL=${SRC_URL:-https://cdn.kernel.org/pub/linux/kernel/v6.x/linux-${KVER}.tar.xz}

echo "== sparkbox guest kernel $KVER (jobs=$JOBS) =="

# Build deps. `dwarves` (pahole) is required when the base config has
# CONFIG_DEBUG_INFO_BTF=y, which the firecracker CI config does.
need=(build-essential flex bison bc libssl-dev libelf-dev dwarves cpio xz-utils curl)
missing=()
for p in "${need[@]}"; do dpkg -s "$p" >/dev/null 2>&1 || missing+=("$p"); done
if [ ${#missing[@]} -gt 0 ]; then
  echo "== apt install: ${missing[*]} =="
  sudo apt-get update -qq
  sudo DEBIAN_FRONTEND=noninteractive apt-get install -y -qq "${missing[@]}"
fi

mkdir -p "$WORK"
cd "$WORK"

if [ ! -d "linux-${KVER}" ]; then
  echo "== fetch $SRC_URL =="
  curl -fsSL "$SRC_URL" -o "linux-${KVER}.tar.xz"
  tar xf "linux-${KVER}.tar.xz"
fi
cd "linux-${KVER}"

echo "== base config <- $BASE_CONFIG_URL =="
curl -fsSL "$BASE_CONFIG_URL" -o .config

echo "== merge sparkbox fragment =="
# merge_config.sh applies the fragment on top of .config, then we let
# olddefconfig resolve dependencies (KVER's kconfig, not the base's).
ARCH=x86_64 ./scripts/kconfig/merge_config.sh -m .config "$FRAGMENT"
make ARCH=x86_64 olddefconfig

# Fail loudly if a delta silently didn't take (a typo'd or dependency-gated
# symbol would otherwise ship a kernel that still can't run docker/tailscale).
echo "== verify deltas landed =="
for sym in CONFIG_TUN CONFIG_IP_NF_RAW CONFIG_NF_TABLES CONFIG_VXLAN CONFIG_WIREGUARD; do
  if ! grep -q "^${sym}=[ym]" .config; then
    echo "ERROR: ${sym} did not end up enabled in .config" >&2
    exit 1
  fi
  echo "  ok: $(grep "^${sym}=" .config)"
done

echo "== build vmlinux =="
make ARCH=x86_64 -j"$JOBS" vmlinux

cp -f vmlinux "$OUT"
echo "== done: $OUT =="
sha256sum "$OUT"
file "$OUT"
