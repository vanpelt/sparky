#!/usr/bin/env bash
# Bring an EXISTING sparkbox host up to cloud-init parity for the "keep the
# rootfs templates fresh" tooling — the agent-CLI refresher + guest workload
# identity installer, their systemd service + daily timer, and one immediate
# --force patch. Use this on a box provisioned by hand (the DGX / manual
# docs/deploy-dgx.md path, hack/setup-host.sh) so it doesn't silently drift from
# what deploy/cloud-init.yaml sets up automatically on a cloud host.
#
# Why this exists: cloud-init has no repo checkout at boot, so it b64-embeds the
# deploy/*.sh scripts and inlines the unit definitions. A hand-built box, by
# contrast, HAS the repo — so it can just run this. The scripts themselves
# (refresh-agent-tools.sh, install-guest-identity.sh) are the single source of
# truth shared by both paths; this script owns the unit + first-run
# orchestration that a manual setup otherwise has to copy by hand (and forget).
#
# Deliberately OUT of scope: the packet-filter unit (deploy/sparkbox-net.sh) and
# the serve unit. Both are genuinely box-specific — net ordering differs on a
# docker host (After=docker.service) vs a cloud host, and serve carries per-box
# ports/TLS/edge flags. hack/setup-host.sh and docs/deploy-dgx.md own those.
#
# Idempotent: safe to re-run. Run as root.
#
# Config (all optional, via env):
#   SPARKBOX_DIR   base dir                     (default /srv/sparkbox)
#   IMAGES_DIR     rootfs templates live here   (default $SPARKBOX_DIR/images)
#                  NB the cloud path uses $SPARKBOX_DIR/data/images (XFS reflink
#                  volume); a manual box often uses $SPARKBOX_DIR/images — pass
#                  whatever matches `sparkbox serve --image-dir`.
#   TOOLS_DIR      tool cache + version stamp   (default $SPARKBOX_DIR/tools)
#   SKIP_REFRESH   =1 installs the units but skips the immediate --force patch
#                  (the daily timer will pick it up)
set -euo pipefail

[ "$(id -u)" -eq 0 ] || { echo "run as root" >&2; exit 1; }
HERE=$(cd "$(dirname "$0")" && pwd)   # tools/sparkbox/deploy

SPARKBOX_DIR=${SPARKBOX_DIR:-/srv/sparkbox}
IMAGES_DIR=${IMAGES_DIR:-$SPARKBOX_DIR/images}
TOOLS_DIR=${TOOLS_DIR:-$SPARKBOX_DIR/tools}

echo "== install helper scripts to /usr/local/sbin =="
# Same canonical names cloud-init writes, so the units below are interchangeable
# with the cloud path's. refresh-tools calls install-guest-identity by that path.
install -m0755 "$HERE/refresh-agent-tools.sh"    /usr/local/sbin/sparkbox-refresh-tools.sh
install -m0755 "$HERE/install-guest-identity.sh" /usr/local/sbin/sparkbox-install-guest-identity.sh

echo "== install sparkbox-refresh-tools.service + .timer =="
# IMAGES_DIR/TOOLS_DIR are baked into the service env so the box's actual layout
# wins over the script's cloud-default (/srv/sparkbox/data/images).
cat > /etc/systemd/system/sparkbox-refresh-tools.service <<UNIT
[Unit]
Description=refresh agent CLIs (claude, codex, hivemind) in sparkbox rootfs templates
Wants=network-online.target
After=network-online.target

[Service]
Type=oneshot
Environment=IMAGES_DIR=$IMAGES_DIR
Environment=TOOLS_DIR=$TOOLS_DIR
ExecStart=/usr/local/sbin/sparkbox-refresh-tools.sh
UNIT

cat > /etc/systemd/system/sparkbox-refresh-tools.timer <<'UNIT'
[Unit]
Description=daily agent-CLI refresh for sparkbox rootfs templates

[Timer]
OnCalendar=daily
RandomizedDelaySec=1h
Persistent=true

[Install]
WantedBy=timers.target
UNIT

systemctl daemon-reload
systemctl enable --now sparkbox-refresh-tools.timer

if [ "${SKIP_REFRESH:-0}" = 1 ]; then
  echo "== SKIP_REFRESH=1: units installed; daily timer will run the first patch =="
else
  echo "== first agent-CLI refresh (--force: patch the current template now) =="
  # Best-effort: a transient download hiccup shouldn't fail the whole install —
  # the daily timer retries. Mirrors cloud-init's provision-time invocation.
  IMAGES_DIR="$IMAGES_DIR" TOOLS_DIR="$TOOLS_DIR" \
    /usr/local/sbin/sparkbox-refresh-tools.sh --force \
    || echo "WARN: agent tools refresh failed; the daily timer will retry"
fi

echo "== done: templates in $IMAGES_DIR will carry claude/codex/hivemind + guest identity, refreshed daily =="
