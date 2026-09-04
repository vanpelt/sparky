#!/usr/bin/env bash
# Assert that the CKS entrypoint's pinned checksums describe the release it
# claims to pin.
#
# deploy/kubernetes/entrypoint.sh hardcodes SHA-256 constants for the kernel,
# rootfs and firecracker it downloads, and separately names a release. Nothing
# tied the two together, so bumping SPARKBOX_RELEASE without moving the
# constants produced a pair of failures that are individually silent:
#
#   a node with a warm cache  reuses the PREVIOUS release's kernel and rootfs,
#                             because a cache hit is decided by matching the
#                             stale constant. It runs old bytes under a new
#                             version label and says "using cached".
#   a node with a cold cache  downloads the new release's real artifact, fails
#                             the stale checksum, and CrashLoops.
#
# v0.6.0 shipped exactly that. This turns both into a build-time failure.
#
# The image is multi-arch, and each architecture's artifacts are built
# separately, so each has its own set of constants and its own way to be wrong.
# With no arguments this checks every architecture the image publishes; naming
# architectures checks only those (CI called it `check-cks-pin.sh amd64` before
# arm64 existed, and that still means "amd64 only").
set -euo pipefail

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
entrypoint="$script_dir/../deploy/kubernetes/entrypoint.sh"
if [ "$#" -gt 0 ]; then
  arches=("$@")
else
  arches=(amd64 arm64)
fi

value_of() {
  # Pull the default out of `readonly name="${OVERRIDE:-value}"`.
  sed -n "s/^readonly $1=\"\\\${[A-Z_0-9]*:-\\(.*\\)}\"$/\\1/p" "$entrypoint"
}

release=$(value_of release)
[ -n "$release" ] || { echo "could not read release from $entrypoint" >&2; exit 1; }

# entrypoint.sh only supplies the DEFAULT. deploy/kubernetes/deployment.yaml
# sets SPARKBOX_RELEASE explicitly on both the prepare init container and the
# node container, and an explicit value wins — so the manifest, not the
# default, decides what a CKS Pod actually downloads. A manifest naming a
# release the checksums do not describe reaches the very same pair of silent
# failures by a different route, and the checks below would never see it.
deployment="$script_dir/../deploy/kubernetes/deployment.yaml"
if [ -r "$deployment" ]; then
  mismatched=$(awk -v want="$release" '
    /^ *- name: SPARKBOX_RELEASE$/ { expect = 1; next }
    expect {
      expect = 0
      value = $0
      sub(/^ *value: */, "", value)
      gsub(/"/, "", value)
      if (value != want) printf "  FAIL deployment.yaml:%d sets %s\n", NR, value
    }
  ' "$deployment")
  if [ -n "$mismatched" ]; then
    echo "deployment.yaml disagrees with the release $entrypoint pins ($release):" >&2
    printf '%s\n' "$mismatched" >&2
    echo "  An explicit env value overrides the entrypoint default, so the" >&2
    echo "  manifest is what a Pod obeys. Move all of them together." >&2
    exit 1
  fi
  echo "deployment.yaml agrees: $release"
fi

manifest_value() { printf '%s\n' "$manifest" | sed -n "s/^$1=//p"; }

status=0
check() { # check <label> <entrypoint-var> <manifest-key>
  local label=$1 got want
  got=$(value_of "$2")
  want=$(manifest_value "$3")
  if [ -z "$want" ]; then
    echo "  skip $label (not in manifest-$arch.env)"
  elif [ -z "$got" ]; then
    # A renamed or dropped constant reads as empty, which would otherwise look
    # like an ordinary mismatch and send the reader hunting for a checksum typo.
    echo "  FAIL $label"
    echo "         entrypoint.sh: no 'readonly $2=\"\${...:-...}\"' line"
    echo "       $release manifest: $want"
    status=1
  elif [ "$got" = "$want" ]; then
    echo "  ok   $label $got"
  else
    echo "  FAIL $label"
    echo "         entrypoint.sh: $got"
    echo "       $release manifest: $want"
    status=1
  fi
}

for arch in "${arches[@]}"; do
  manifest_url="https://github.com/vanpelt/sparky/releases/download/$release/manifest-$arch.env"
  manifest=$(curl -fsSL "$manifest_url") || {
    echo "FAIL: cannot fetch $manifest_url" >&2
    echo "  entrypoint.sh pins $release; does that release exist and have $arch assets?" >&2
    exit 1
  }

  echo "entrypoint.sh pins $release (arch $arch)"
  # The constants are suffixed by arch because one entrypoint serves both:
  # the image is multi-arch and picks its set from `uname -m` at Pod start.
  check kernel      "kernel_sha256_$arch"      SHA256_VMLINUX
  check rootfs      "rootfs_sha256_$arch"      SHA256_ROOTFS
  check firecracker "firecracker_sha256_$arch" SHA256_FIRECRACKER
  check jailer      "jailer_sha256_$arch"      SHA256_JAILER
done

if [ "$status" -ne 0 ]; then
  cat >&2 <<MSG

The pinned checksums do not describe $release.

Because the artifacts are rebuilt per release and are not bit-reproducible,
the constants can only be filled in AFTER that release is published. Read the
values out of the release's own manifest and commit them alongside the pin:

  curl -fsSL https://github.com/vanpelt/sparky/releases/download/$release/manifest-<arch>.env
MSG
  exit 1
fi
echo "pinned checksums match $release for ${arches[*]}"
