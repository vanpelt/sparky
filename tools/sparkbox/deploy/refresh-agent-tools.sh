#!/usr/bin/env bash
# Bake up-to-date agent CLIs (Claude Code + Codex) into the sparkbox rootfs
# templates WITHOUT rebuilding the image. Runs on the host as root.
#
# Rebuilding the rootfs is a ~65-minute docker+CI affair; this is seconds. The
# firecracker driver reflinks <image-dir>/<image>.ext4 at every sandbox create,
# so patching the template is picked up by the next `ssh new@...` instantly.
# The patch is atomic (reflink copy -> loop mount -> install -> rename), so a
# concurrent create sees either the old or the new template, never a torn one.
# Running/paused VMs keep their own rootfs copies and are untouched.
#
# Sources (both self-contained single binaries, no guest deps):
#   claude: downloads.claude.ai native build, sha256-verified via the release
#           manifest (same scheme as the official install.sh)
#   codex:  github.com/openai/codex latest release, static musl build (zst).
#           No plain checksum published (only sigstore) — TLS-only fetch.
#
# Usage: refresh-agent-tools.sh [--force]
#   --force  re-patch templates even when the version stamp says current
#            (use after replacing a template, e.g. a fresh provision)
set -euo pipefail

IMAGES_DIR=${IMAGES_DIR:-/srv/sparkbox/data/images}
TOOLS_DIR=${TOOLS_DIR:-/srv/sparkbox/data/tools}
CLAUDE_BASE=${CLAUDE_BASE:-https://downloads.claude.ai/claude-code-releases}
CODEX_REPO=${CODEX_REPO:-openai/codex}
FORCE=0
[ "${1:-}" = --force ] && FORCE=1

case "$(uname -m)" in
  x86_64)  CLAUDE_PLAT=linux-x64;   CODEX_ARCH=x86_64 ;;
  aarch64) CLAUDE_PLAT=linux-arm64; CODEX_ARCH=aarch64 ;;
  *) echo "unsupported arch $(uname -m)" >&2; exit 1 ;;
esac

mkdir -p "$TOOLS_DIR"
STAMP="$TOOLS_DIR/versions.env"

# ---- resolve latest versions ------------------------------------------------
CLAUDE_VER=$(curl -fsSL "$CLAUDE_BASE/stable")
case "$CLAUDE_VER" in
  [0-9]*.[0-9]*.[0-9]*) ;;
  *) echo "bad claude version from $CLAUDE_BASE/stable: $CLAUDE_VER" >&2; exit 1 ;;
esac
# Latest codex tag via the release redirect (no API => no rate limits/auth).
CODEX_TAG=$(curl -fsSLI -o /dev/null -w '%{url_effective}' \
  "https://github.com/$CODEX_REPO/releases/latest")
CODEX_TAG=${CODEX_TAG##*/}
case "$CODEX_TAG" in
  rust-v[0-9]*) ;;
  *) echo "bad codex tag from releases/latest redirect: $CODEX_TAG" >&2; exit 1 ;;
esac

if [ "$FORCE" = 0 ] && [ -f "$STAMP" ] \
   && grep -qx "CLAUDE_VERSION=$CLAUDE_VER" "$STAMP" \
   && grep -qx "CODEX_TAG=$CODEX_TAG" "$STAMP"; then
  echo "agent tools already current (claude $CLAUDE_VER, codex $CODEX_TAG)"
  exit 0
fi

# ---- download (cached by version, so reruns are free) ------------------------
CLAUDE_BIN="$TOOLS_DIR/claude-$CLAUDE_VER-$CLAUDE_PLAT"
if [ ! -x "$CLAUDE_BIN" ]; then
  echo ">> downloading claude $CLAUDE_VER ($CLAUDE_PLAT)"
  want=$(curl -fsSL "$CLAUDE_BASE/$CLAUDE_VER/manifest.json" \
    | python3 -c 'import json,sys; print(json.load(sys.stdin)["platforms"][sys.argv[1]]["checksum"])' "$CLAUDE_PLAT")
  curl -fsSL "$CLAUDE_BASE/$CLAUDE_VER/$CLAUDE_PLAT/claude" -o "$CLAUDE_BIN.tmp"
  echo "$want  $CLAUDE_BIN.tmp" | sha256sum -c - >/dev/null
  chmod 0755 "$CLAUDE_BIN.tmp" && mv "$CLAUDE_BIN.tmp" "$CLAUDE_BIN"
fi

CODEX_BIN="$TOOLS_DIR/codex-$CODEX_TAG-$CODEX_ARCH"
if [ ! -x "$CODEX_BIN" ]; then
  echo ">> downloading codex $CODEX_TAG ($CODEX_ARCH-musl)"
  curl -fsSL "https://github.com/$CODEX_REPO/releases/download/$CODEX_TAG/codex-$CODEX_ARCH-unknown-linux-musl.zst" \
    | zstd -d -o "$CODEX_BIN.tmp" -f
  chmod 0755 "$CODEX_BIN.tmp" && mv "$CODEX_BIN.tmp" "$CODEX_BIN"
fi

# ---- patch every template atomically -----------------------------------------
MNT=""
TMP=""
cleanup() {
  [ -n "$MNT" ] && mountpoint -q "$MNT" && umount "$MNT" || true
  [ -n "$MNT" ] && rmdir "$MNT" 2>/dev/null || true
  [ -n "$TMP" ] && rm -f "$TMP" || true
}
trap cleanup EXIT

shopt -s nullglob
patched=0
for tpl in "$IMAGES_DIR"/*.ext4; do
  echo ">> patching $(basename "$tpl")"
  TMP="$tpl.tools-new"
  # Reflink copy on the XFS data volume: instant, and leaves the live template
  # untouched until the final atomic rename.
  cp --reflink=auto "$tpl" "$TMP"
  MNT=$(mktemp -d)
  mount -o loop "$TMP" "$MNT"
  install -m 0755 "$CLAUDE_BIN" "$MNT/usr/local/bin/claude"
  install -m 0755 "$CODEX_BIN"  "$MNT/usr/local/bin/codex"
  # The template stays the single source of tool versions; don't let each guest
  # race to self-update on top of it (wasted bandwidth, mid-session surprises).
  grep -qs '^DISABLE_AUTOUPDATER=' "$MNT/etc/environment" || \
    echo 'DISABLE_AUTOUPDATER=1' >> "$MNT/etc/environment"
  umount "$MNT" && rmdir "$MNT"; MNT=""
  mv -f "$TMP" "$tpl"; TMP=""
  patched=$((patched + 1))
done

if [ "$patched" = 0 ]; then
  echo "no templates in $IMAGES_DIR — nothing to patch" >&2
  exit 1
fi

printf 'CLAUDE_VERSION=%s\nCODEX_TAG=%s\n' "$CLAUDE_VER" "$CODEX_TAG" > "$STAMP"
# Drop cached binaries from older versions; keep the current pair.
find "$TOOLS_DIR" -maxdepth 1 -type f \( -name 'claude-*' -o -name 'codex-*' \) \
  ! -name "$(basename "$CLAUDE_BIN")" ! -name "$(basename "$CODEX_BIN")" -delete
echo ">> done: $patched template(s) now ship claude $CLAUDE_VER + codex $CODEX_TAG"
