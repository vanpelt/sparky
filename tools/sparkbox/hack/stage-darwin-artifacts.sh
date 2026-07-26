#!/usr/bin/env bash
# Stage the macOS half of a sparkbox release: the darwin binary plus the
# manifest a Mac reads. Companion to stage-artifacts.sh (the linux half); the
# caller uploads whatever lands in $OUT_DIR (see
# .github/workflows/build-artifacts.yml).
#
#   sparkbox-darwin-arm64          the binary a Mac runs `sparkbox setup` from
#   manifest-darwin-arm64.env      sha256s + metadata that setup reads there
#
# The third darwin-only asset, `vmlinux-macos-arm64`, is NOT staged here: it is
# a 25-minute native arm64 kernel compile with its own job and its own cache
# (see the macos-kernel job). This script is handed the file that job already
# uploaded, via OUTER_KERNEL, purely to record its checksum in the manifest —
# the same "one file, one producer, everyone else reads the sha" rule that makes
# the guest keys below copies rather than recomputations.
#
# Why this is a separate script, and a separate CI job, from stage-artifacts.sh:
# that script's runner spends ~15 minutes compiling a guest kernel, pulls a
# multi-GB base image, loop-mounts an ext4 and needs zstd, firecracker and the
# fleet gateway pubkey. The darwin side needs exactly none of it — `GOOS=darwin
# go build` and a checksum. Folding it into that matrix would have meant a third
# leg carrying eight `if:` guards, or paying for all of it to produce a 30MB
# static binary.
#
# NOT built on a macOS runner: GOOS=darwin cross-compiles from any host, the
# repo's go.yml already gates darwin/arm64 on every push, and a macos runner
# bills at a 10x multiplier for a build that needs nothing from macOS. The one
# thing this forfeits is CGO_ENABLED=1 (which would need the macOS SDK) — see
# the note on the build below.
#
# Usage: OUT_DIR=/tmp/rel RELEASE=v0.5.0 GUEST_MANIFEST=/tmp/manifest-arm64.env \
#          ./stage-darwin-artifacts.sh
set -euo pipefail

REPO_DIR=${REPO_DIR:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)}
RELEASE=${RELEASE:-$(date -u +%Y-%m-%d-%H%M)}
OUT_DIR=${OUT_DIR:?set OUT_DIR: where to stage the release assets}

# The linux arm64 manifest this release already published. Every checksum the
# two files share is COPIED from it rather than recomputed, so the Mac and the
# machine it provisions can never end up describing different artifacts for the
# same tag. It is also the reason this runs after the arm64 artifacts leg.
#
# No apostrophe in that message: the word in ${var:?word} is quote-processed,
# so a lone ' opens a string that runs to the next ' anywhere in the file —
# here it swallowed thirty lines including two function definitions, and
# `bash -n` still passed because the result is valid syntax.
GUEST_MANIFEST=${GUEST_MANIFEST:?set GUEST_MANIFEST: path to the manifest-arm64.env of this release}

# Apple Silicon only, deliberately. A darwin/amd64 build is nearly free — one
# more `go build` — but it would be a binary that cannot do the only two things
# this binary does on a Mac. `sparkbox setup` on darwin provisions a nested
# linux machine, which needs Virtualization.framework nested virt: macos/poc.sh
# hard-fails on anything that is not Apple Silicon (arch check) and again on
# anything older than an M3 (nested-virt check), and the outer kernel B2 ships
# is arch/arm64/boot/Image. There is no `sparkbox` client subcommand — the CLI
# is serve|setup|doctor|fetch-secrets|version — so an Intel Mac would download
# a binary whose every useful path exits non-zero, and we would owe it a
# manifest-darwin-amd64.env pointing at a macOS kernel that will never exist.
# Cost is not the reason to skip it; there being nothing for it to do is.
DARWIN_ARCH=${DARWIN_ARCH:-arm64}
GOARCH=arm64                     # the arch of the LINUX assets a Mac provisions
SPARKBOX_ASSET="sparkbox-darwin-$DARWIN_ARCH"
MACHINE_SPARKBOX_ASSET="sparkbox-linux-$GOARCH"

# The macOS OUTER kernel: the KVM-capable arm64 Image that Apple's `container
# machine` boots, inside which the linux gateway then runs firecracker. Two
# kernels ship in a release and they are not interchangeable — vmlinux-arm64 is
# the guest kernel a microVM boots, this one is the machine kernel — so the name
# carries "macos" rather than an arch alone, and it lives only in the darwin
# manifest because no linux host would ever have a use for it.
#
# Path in, not a build: macos/kernel/build.sh needs a native aarch64 runner and
# ~25 minutes, so it has its own CI job. See the note at the top of the file.
OUTER_KERNEL_ASSET=${OUTER_KERNEL_ASSET:-vmlinux-macos-$DARWIN_ARCH}
OUTER_KERNEL=${OUTER_KERNEL:?set OUTER_KERNEL: path to the built $OUTER_KERNEL_ASSET}

[ -f "$GUEST_MANIFEST" ] || { echo "missing guest manifest: $GUEST_MANIFEST" >&2; exit 1; }
[ -s "$OUTER_KERNEL" ] || { echo "missing or empty outer kernel: $OUTER_KERNEL" >&2; exit 1; }

mkdir -p "$OUT_DIR"
OUT_DIR=$(cd "$OUT_DIR" && pwd)
sha() { sha256sum "$1" | cut -d' ' -f1; }

# Read one key out of the linux manifest, with the same semantics as the Go
# side's parseEnv (last-wins is irrelevant here; surrounding quotes are
# stripped). Sourcing the file would be shorter and would execute an artifact
# we just downloaded — not a trade worth making for four lines of sed.
key() { sed -n "s/^$1=//p" "$GUEST_MANIFEST" | head -1 | sed 's/^"//; s/"$//'; }

# Guard rails on the inherited half. A silently-empty checksum here would sail
# through: downloadVerify treats SHA256="" as "do not verify".
guest_release=$(key RELEASE)
guest_arch=$(key ARCH)
guest_platform=$(key PLATFORM)
[ "$guest_arch" = "$GOARCH" ] || {
  echo "$GUEST_MANIFEST is ARCH=$guest_arch, want $GOARCH" >&2; exit 1; }
[ "${guest_platform:-linux}" = linux ] || {
  echo "$GUEST_MANIFEST is PLATFORM=$guest_platform, want linux" >&2; exit 1; }
[ "$guest_release" = "$RELEASE" ] || {
  echo "$GUEST_MANIFEST is RELEASE=$guest_release, want $RELEASE" >&2; exit 1; }
for k in SHA256_VMLINUX SHA256_FIRECRACKER SHA256_ROOTFS SHA256_SPARKBOX ROOTFS_ASSET \
         SLUICE_ASSET SHA256_SLUICE; do
  [ -n "$(key "$k")" ] || { echo "$GUEST_MANIFEST has no $k" >&2; exit 1; }
done

echo "== build sparkbox binary (darwin/$DARWIN_ARCH) =="
# Same flags as the linux assets: -trimpath and -s -w for size and
# reproducibility, and the release tag stamped into main.version so
# `sparkbox version` on the Mac answers which release it is.
#
# CGO_ENABLED=0 matches the linux assets, and on darwin it is also what makes
# cross-compiling possible at all (cgo would want the macOS SDK). Nothing in
# sparkbox needs cgo — the darwin/arm64 leg of go.yml's cross-compile matrix
# proves that on every push. The one behavioural consequence is that Go's pure
# resolver replaces libSystem's, so this binary does not honour macOS's
# scoped/VPN resolvers (a Tailscale split-DNS entry, say). It resolves
# github.com to fetch a release and shells out to `container` for everything
# else, so that does not bite here — but a future darwin feature that must
# resolve a MagicDNS name is the thing that would force a macOS runner.
#
# Go's linker ad-hoc-signs darwin/arm64 output itself, including when
# cross-linking, which is what lets an arm64 Mac execute a binary built on
# Linux at all. Only a real Mac running a CI-built artifact proves it.
( cd "$REPO_DIR/tools/sparkbox" && \
  CGO_ENABLED=0 GOOS=darwin GOARCH="$DARWIN_ARCH" \
  go build -trimpath -ldflags "-s -w -X main.version=$RELEASE" \
    -o "$OUT_DIR/$SPARKBOX_ASSET" ./cmd/sparkbox )

echo "== manifest =="
# PLATFORM is what stops a Mac from provisioning off the linux manifest and a
# linux host from provisioning off this one (Manifest.CheckPlatform).
#
# The guest keys are repeated verbatim: a Mac provisions exactly these
# artifacts into the machine it creates, so pinning them here is what makes
# `--release <tag>` on the Mac mean the same thing it means on the DGX.
# SHA256_SPARKBOX is the ONE key whose meaning has to shift — in every manifest
# it is "the sparkbox binary this host runs", which on darwin is the darwin
# build — so the linux binary it installs into the machine moves to
# MACHINE_SPARKBOX_ASSET / SHA256_MACHINE_SPARKBOX.
#
# SLUICE_ASSET needs no such shift and so crosses over verbatim. sluice is a
# linux daemon on BOTH platforms: it attaches eBPF to the host side of guest
# taps, and on darwin those taps live inside the nested machine, so a Mac
# installs sluice-linux-arm64 there exactly as it installs the rootfs. There is
# no darwin sluice to disambiguate it from, which is why it is not MACHINE_*.
#
# SHA256_OUTER_KERNEL is an INTEGRITY claim and not an identity one: it says
# "this is the kernel image this release published", which is what a Mac needs
# to verify a download. It does NOT say a rebuild of macos/kernel/build.sh
# yields the same bytes — the builder's gcc and binutils versions are compiled
# into the kernel banner and the Ubuntu archive is not pinned, so a
# packaging-only compiler revision moves the hash with no behavioural change.
# The long version is at the top of macos/kernel/build.sh.
cat > "$OUT_DIR/manifest-darwin-$DARWIN_ARCH.env" <<EOF
RELEASE=$RELEASE
ARCH=$GOARCH
PLATFORM=darwin
FIRECRACKER_VERSION=$(key FIRECRACKER_VERSION)
SHA256_VMLINUX=$(key SHA256_VMLINUX)
SHA256_FIRECRACKER=$(key SHA256_FIRECRACKER)
SPARKBOX_ASSET=$SPARKBOX_ASSET
SHA256_SPARKBOX=$(sha "$OUT_DIR/$SPARKBOX_ASSET")
MACHINE_SPARKBOX_ASSET=$MACHINE_SPARKBOX_ASSET
SHA256_MACHINE_SPARKBOX=$(key SHA256_SPARKBOX)
SLUICE_ASSET=$(key SLUICE_ASSET)
SHA256_SLUICE=$(key SHA256_SLUICE)
OUTER_KERNEL_ASSET=$OUTER_KERNEL_ASSET
SHA256_OUTER_KERNEL=$(sha "$OUTER_KERNEL")
ROOTFS_NAME=$(key ROOTFS_NAME)
ROOTFS_ASSET=$(key ROOTFS_ASSET)
SHA256_ROOTFS=$(key SHA256_ROOTFS)
ROOTFS_LOGIN_USER=$(key ROOTFS_LOGIN_USER)
GATEWAY_PUBKEY="$(key GATEWAY_PUBKEY)"
EOF
echo "---"; cat "$OUT_DIR/manifest-darwin-$DARWIN_ARCH.env"; echo "---"

echo
echo "== staged $RELEASE (darwin/$DARWIN_ARCH) in $OUT_DIR =="
( cd "$OUT_DIR" && du -h -- * | sed 's/^/  /' )
