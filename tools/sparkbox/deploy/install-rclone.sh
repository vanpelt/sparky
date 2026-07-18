#!/usr/bin/env bash
# Install a current rclone into /usr/local/bin.
#
# Why not the distro package: Ubuntu ships rclone ~1.60 (2022), which is too old
# for Cloudflare R2 — uploads fail the first attempt with "501 NotImplemented"
# and only land on rclone's retry, so every multi-GB sandbox-archive upload runs
# twice. The current static binary talks to R2 cleanly in one shot. Installing to
# /usr/local/bin means it wins over any apt-installed rclone on PATH.
#
# Idempotent: skips the download when a new-enough rclone is already present.
# Needs root (writes /usr/local/bin) plus curl + unzip. Run: sudo install-rclone.sh
set -euo pipefail
MIN_MINOR=${MIN_MINOR:-65}   # require >= 1.<MIN_MINOR> (R2 upload fixes land by 1.65)

cur=$(rclone version 2>/dev/null | sed -n '1s/^rclone v//p' || true)
if [ -n "$cur" ]; then
  maj=${cur%%.*}; rest=${cur#*.}; min=${rest%%.*}
  if [ "${maj:-0}" -gt 1 ] || { [ "${maj:-0}" -eq 1 ] && [ "${min:-0}" -ge "$MIN_MINOR" ]; }; then
    echo "rclone $cur already >= 1.$MIN_MINOR — nothing to do"; exit 0
  fi
  echo "rclone $cur is older than 1.$MIN_MINOR — upgrading"
fi

case "$(dpkg --print-architecture 2>/dev/null || uname -m)" in
  amd64|x86_64)  RARCH=amd64 ;;
  arm64|aarch64) RARCH=arm64 ;;
  *) echo "install-rclone: unsupported arch" >&2; exit 1 ;;
esac

TMP=$(mktemp -d); trap 'rm -rf "$TMP"' EXIT
curl -fsSL "https://downloads.rclone.org/rclone-current-linux-${RARCH}.zip" -o "$TMP/rclone.zip"
( cd "$TMP" && unzip -oq rclone.zip )
install -m0755 "$TMP"/rclone-*-linux-"$RARCH"/rclone /usr/local/bin/rclone
hash -r 2>/dev/null || true
echo "installed $(/usr/local/bin/rclone version | head -1)"
