#!/usr/bin/env bash
# Bake up-to-date agent CLIs (Claude Code + Codex) and the guest workload-identity
# payload into the sparkbox rootfs templates WITHOUT rebuilding the image. Runs
# on the host as root.
#
# Rebuilding the rootfs is a ~65-minute docker+CI affair; this is seconds. The
# firecracker driver reflinks <image-dir>/<image>.ext4 at every sandbox create,
# so patching the template is picked up by the next `ssh new@...` instantly.
# The patch is atomic (reflink copy -> loop mount -> install -> rename), so a
# concurrent create sees either the old or the new template, never a torn one.
# Running/paused VMs keep their own rootfs copies and are untouched.
#
# Sources (all self-contained single binaries, no guest deps):
#   claude:   downloads.claude.ai native build, sha256-verified via the release
#             manifest (same scheme as the official install.sh)
#   codex:    github.com/openai/codex latest release, static musl build (zst).
#             No plain checksum published (only sigstore) — TLS-only fetch.
#   hivemind: github.com/wandb/hivemind release binary, version + sha256
#             resolved from the repo's hivemind-latest.json manifest (the same
#             manifest the official installer at hivemind.wandb.tools uses).
#
# Usage: refresh-agent-tools.sh [--force]
#   --force  re-patch templates even when the version stamp says current
#            (use after replacing a template, e.g. a fresh provision)
set -euo pipefail

IMAGES_DIR=${IMAGES_DIR:-/srv/sparkbox/data/images}
TOOLS_DIR=${TOOLS_DIR:-/srv/sparkbox/data/tools}
# Installs the guest OIDC token unit + timer into a mounted template. Shared
# verbatim with hack/build-rootfs.sh so the two paths can't drift; cloud-init
# lands it next to this script. Templates published before workload identity
# existed get it here, with no ~65-minute image rebuild.
GUEST_IDENTITY=${GUEST_IDENTITY:-/usr/local/sbin/sparkbox-install-guest-identity.sh}
CLAUDE_BASE=${CLAUDE_BASE:-https://downloads.claude.ai/claude-code-releases}
CODEX_REPO=${CODEX_REPO:-openai/codex}
HIVEMIND_MANIFEST=${HIVEMIND_MANIFEST:-https://raw.githubusercontent.com/wandb/hivemind/main/manifests/hivemind-latest.json}
# Revision of the guest-side agent conditioning below (/etc/environment knobs +
# the ~/.claude.json onboarding seed). Versioned like IDENTITY_REV so bumping it
# re-patches every template on the next run even when no tool version moved.
AGENT_ENV_REV=1
FORCE=0
[ "${1:-}" = --force ] && FORCE=1

case "$(uname -m)" in
  x86_64)  CLAUDE_PLAT=linux-x64;   CODEX_ARCH=x86_64;  HM_PLAT=linux-x86_64 ;;
  aarch64) CLAUDE_PLAT=linux-arm64; CODEX_ARCH=aarch64; HM_PLAT=linux-arm64 ;;
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
# Hivemind's manifest carries version, per-platform URL, and sha256 in one doc.
read -r HM_VER HM_URL HM_SHA <<EOF
$(curl -fsSL "$HIVEMIND_MANIFEST" | python3 -c '
import json, sys
m = json.load(sys.stdin)
p = m["platforms"][sys.argv[1]]
print(m["version"], p["binary_url"], p["binary_sha256"])' "$HM_PLAT")
EOF
case "$HM_VER" in
  [0-9]*.[0-9]*.[0-9]*) ;;
  *) echo "bad hivemind version from manifest: $HM_VER" >&2; exit 1 ;;
esac

# The guest identity payload is versioned by the installer itself, so bumping
# IDENTITY_REV there re-patches every template on the next run.
[ -x "$GUEST_IDENTITY" ] || { echo "missing guest identity installer at $GUEST_IDENTITY" >&2; exit 1; }
IDENTITY_REV=$(sed -n 's/^IDENTITY_REV=//p' "$GUEST_IDENTITY" | head -1)
case "$IDENTITY_REV" in
  [0-9]*) ;;
  *) echo "could not read IDENTITY_REV from $GUEST_IDENTITY" >&2; exit 1 ;;
esac

if [ "$FORCE" = 0 ] && [ -f "$STAMP" ] \
   && grep -qx "CLAUDE_VERSION=$CLAUDE_VER" "$STAMP" \
   && grep -qx "CODEX_TAG=$CODEX_TAG" "$STAMP" \
   && grep -qx "HIVEMIND_VERSION=$HM_VER" "$STAMP" \
   && grep -qx "IDENTITY_REV=$IDENTITY_REV" "$STAMP" \
   && grep -qx "AGENT_ENV_REV=$AGENT_ENV_REV" "$STAMP"; then
  echo "templates already current (claude $CLAUDE_VER, codex $CODEX_TAG, hivemind $HM_VER, identity rev $IDENTITY_REV, agent env rev $AGENT_ENV_REV)"
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

HM_BIN="$TOOLS_DIR/hivemind-$HM_VER-$HM_PLAT"
if [ ! -x "$HM_BIN" ]; then
  echo ">> downloading hivemind $HM_VER ($HM_PLAT)"
  curl -fsSL "$HM_URL" -o "$HM_BIN.tmp"
  echo "$HM_SHA  $HM_BIN.tmp" | sha256sum -c - >/dev/null
  chmod 0755 "$HM_BIN.tmp" && mv "$HM_BIN.tmp" "$HM_BIN"
fi

# ---- guest agent conditioning -------------------------------------------------
# Claude Code gates its TUI behind two first-run dialogs that have nothing to do
# with auth, so a sandbox carrying a valid CLAUDE_CODE_OAUTH_TOKEN still lands on
# the theme picker and then the login screen instead of a prompt:
#
#   1. the theme picker / welcome flow, gated on ~/.claude.json's
#      `hasCompletedOnboarding`
#   2. the "is this a project you trust?" dialog, gated per-directory on
#      `projects[cwd].hasTrustDialogAccepted`
#
# We satisfy (1) by seeding the config below, and (2) with CLAUDE_CODE_SANDBOXED,
# a first-class escape hatch in the binary (its trust check opens with
# `if (CLAUDE_CODE_SANDBOXED) return true`). That flag ALSO stops project-scoped
# permission rules from being dropped as untrusted, i.e. a cloned repo's
# .claude/settings.json allow-rules apply without a prompt — correct here, since
# the VM is itself the sandbox boundary and holds nothing the owner can't reach.
#
# Note this seeds config, never credentials: auth stays the env token that
# envsync pushes per-tag, and no ~/.claude/.credentials.json is ever written, so
# there is no credential state to sync between host and guest or across boxes.
seed_agent_env() {
  local mnt=$1
  # The template stays the single source of tool versions; don't let each guest
  # race to self-update on top of it (wasted bandwidth, mid-session surprises).
  grep -qs '^DISABLE_AUTOUPDATER=' "$mnt/etc/environment" || \
    echo 'DISABLE_AUTOUPDATER=1' >> "$mnt/etc/environment"
  grep -qs '^CLAUDE_CODE_SANDBOXED=' "$mnt/etc/environment" || \
    echo 'CLAUDE_CODE_SANDBOXED=1' >> "$mnt/etc/environment"

  # Resolve the login user from the template itself rather than hardcoding
  # `sparky`, so this tracks the sparkbox.login-user label if it ever changes.
  local pw home uid gid
  pw=$(awk -F: '$3 == 1000 {print; exit}' "$mnt/etc/passwd") || return 0
  [ -n "$pw" ] || { echo "   !! no uid-1000 user in template; skipping claude seed" >&2; return 0; }
  uid=$(echo "$pw" | cut -d: -f3)
  gid=$(echo "$pw" | cut -d: -f4)
  home=$(echo "$pw" | cut -d: -f6)
  [ -d "$mnt$home" ] || { echo "   !! $home missing in template; skipping claude seed" >&2; return 0; }

  # MERGE, never overwrite: this loop also walks `snap-<owner>-<name>.ext4` fork
  # templates, whose ~/.claude.json is a real user's accumulated state (project
  # trust, history pointers, theme choice). We assert only the onboarding keys,
  # and leave a theme already chosen by the user alone.
  CLAUDE_VER="$CLAUDE_VER" python3 - "$mnt$home/.claude.json" <<'PY'
import json, os, sys

path = sys.argv[1]
try:
    with open(path) as f:
        cfg = json.load(f)
    if not isinstance(cfg, dict):
        raise ValueError("not an object")
except FileNotFoundError:
    cfg = {}
except Exception as e:
    print(f"   !! {path} unreadable ({e}); leaving it alone", file=sys.stderr)
    sys.exit(0)

cfg["hasCompletedOnboarding"] = True
cfg["lastOnboardingVersion"] = os.environ["CLAUDE_VER"]
cfg.setdefault("theme", "dark")

tmp = path + ".seed-new"
with open(tmp, "w") as f:
    json.dump(cfg, f, indent=2)
os.replace(tmp, path)
PY
  chown "$uid:$gid" "$mnt$home/.claude.json"
  chmod 0644 "$mnt$home/.claude.json"
}

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
  install -m 0755 "$HM_BIN"     "$MNT/usr/local/bin/hivemind"
  seed_agent_env "$MNT"
  # Workload identity: the token unit + timer that keep
  # /var/run/secrets/hivemind/token fresh, so `hivemind start` federates with
  # no secret in the guest and nothing to paste.
  "$GUEST_IDENTITY" "$MNT"
  umount "$MNT" && rmdir "$MNT"; MNT=""
  mv -f "$TMP" "$tpl"; TMP=""
  patched=$((patched + 1))
done

if [ "$patched" = 0 ]; then
  echo "no templates in $IMAGES_DIR — nothing to patch" >&2
  exit 1
fi

printf 'CLAUDE_VERSION=%s\nCODEX_TAG=%s\nHIVEMIND_VERSION=%s\nIDENTITY_REV=%s\nAGENT_ENV_REV=%s\n' \
  "$CLAUDE_VER" "$CODEX_TAG" "$HM_VER" "$IDENTITY_REV" "$AGENT_ENV_REV" > "$STAMP"
# Drop cached binaries from older versions; keep the current set.
find "$TOOLS_DIR" -maxdepth 1 -type f \( -name 'claude-*' -o -name 'codex-*' -o -name 'hivemind-*' \) \
  ! -name "$(basename "$CLAUDE_BIN")" ! -name "$(basename "$CODEX_BIN")" ! -name "$(basename "$HM_BIN")" -delete
echo ">> done: $patched template(s) now ship claude $CLAUDE_VER + codex $CODEX_TAG + hivemind $HM_VER + identity rev $IDENTITY_REV"
