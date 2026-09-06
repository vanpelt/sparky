#!/usr/bin/env bash
# Bake up-to-date agent CLIs (Claude Code, Codex, Pi, Hivemind, agent-browser) and
# the guest workload-identity payload into the sparkbox rootfs templates WITHOUT
# rebuilding the image. Runs on the host as root.
#
# Rebuilding the rootfs is a ~65-minute docker+CI affair; this is seconds. The
# firecracker driver reflinks <image-dir>/<image>.ext4 at every sandbox create,
# so patching the template is picked up by the next `ssh new@...` instantly.
# The patch is atomic (reflink copy -> loop mount -> install -> rename), so a
# concurrent create sees either the old or the new template, never a torn one.
# Only release/operator base templates are mounted here. User-derived
# snap-*.ext4 images are deliberately excluded: mounting an untrusted guest
# filesystem asks the privileged host kernel to parse attacker-controlled ext4
# metadata and turns the management plane into a second sandbox boundary.
# Running/paused VMs and their snapshot templates are untouched.
#
# A template is a snapshot of the tools on the day it was patched, so a VM that
# has been alive for a month is running whatever its template shipped with. That
# VM cannot be fixed from out here — the whole point of the atomic-rename scheme
# above is that a live rootfs is never touched — so the answer is a pull, not a
# push: this run also publishes $TOOLS_DIR/manifest.json describing the artifacts
# it just verified, the host serves that directory to its own guests over the
# metadata tap, and `sparkbox update-tools` inside a VM installs from it. See
# write_tools_manifest below and install-guest-identity.sh's
# sparkbox-update-tools.
#
# Sources (all self-contained single binaries, no guest deps):
#   claude:   downloads.claude.ai native build, sha256-verified via the release
#             manifest (same scheme as the official install.sh)
#   codex:    github.com/openai/codex latest release, static musl build (zst).
#             No plain checksum published (only sigstore) — TLS-only fetch.
#   pi:       github.com/earendil-works/pi latest release, standalone Linux
#             bundle, sha256-verified against the release's SHA256SUMS.
#   hivemind: github.com/wandb/hivemind release binary, version + sha256
#             resolved from the repo's hivemind-latest.json manifest (the same
#             manifest the official installer at hivemind.wandb.tools uses).
#   agent-browser:
#             registry.npmjs.org tarball for the `agent-browser` package.
#             Version, URL and digest all come back in ONE unauthenticated GET
#             of the registry's /latest document — the hivemind-manifest shape,
#             not the tag-then-SHA256SUMS dance the GitHub-hosted tools need.
#             npm publishes dist.integrity as base64 sha512; dist.shasum is sha1
#             and is NOT what we check. The tarball already carries prebuilt
#             per-platform Rust binaries, so nothing here needs npm or node —
#             which matters, because the host container that runs this script
#             (deploy/kubernetes/Containerfile) has curl and python3 and no node
#             at all.
#
# WHICH TEMPLATES ARE CURRENT IS ASKED OF THE TEMPLATES THEMSELVES.
#
# This used to key off one host-side stamp file ($TOOLS_DIR/versions.env): if it
# named today's versions, every template was declared current and the run exited
# without looking at one. That is a claim about files the script never opened,
# and a template can be replaced underneath it — which is exactly what `sparkbox
# setup` does when it fetches a release's rootfs. On the DGX the v0.4.0 upgrade
# dropped a fresh universal.ext4 at 12:43 over a stamp written at 00:38; every
# run afterwards said "templates already current" and every sandbox created from
# it had no claude, no codex, no pi and no hivemind. --force was the documented
# escape hatch, which means the correct behaviour depended on the operator
# remembering an invariant the script was in a position to check.
#
# So the stamp now lives INSIDE each template, at /etc/sparkbox/tools-rev, and is
# read per template with debugfs (no mount, no loop device, read-only). A
# template whose stamp does not match what this run resolved is patched; one that
# matches is skipped. A template that cannot be read is treated as STALE, because
# the failure directions are not symmetric: a needless re-patch costs a minute,
# a wrong "current" costs a sandbox with no agent in it and says nothing.
#
# Usage: refresh-agent-tools.sh [--force]
#   --force  re-patch every template even when its own stamp says current
#            (a repair tool now, not a correctness requirement)
set -euo pipefail

IMAGES_DIR=${IMAGES_DIR:-/srv/sparkbox/data/images}
TOOLS_DIR=${TOOLS_DIR:-/srv/sparkbox/tools}
# Installs the guest OIDC token unit + timer into a mounted template. Shared
# verbatim with hack/build-rootfs.sh so the two paths can't drift; cloud-init
# lands it next to this script. Templates published before workload identity
# existed get it here, with no ~65-minute image rebuild.
GUEST_IDENTITY=${GUEST_IDENTITY:-/usr/local/sbin/sparkbox-install-guest-identity.sh}
# Optional canonical static banner for trusted base templates. CKS supplies the
# repository's images/motd here because its fast image build reuses a released
# rootfs rather than rebuilding images/Dockerfile. Snapshot templates remain
# excluded by the selection loop below.
GUEST_MOTD_FILE=${GUEST_MOTD_FILE:-}
# Optional public-only fleet key to bake into trusted operator templates. CKS
# uses this from a read-only Secret so the long-lived VM controller never has
# to loop-mount a guest disk merely to install authorized_keys.
GATEWAY_PUBLIC_KEY_FILE=${GATEWAY_PUBLIC_KEY_FILE:-}
CLAUDE_BASE=${CLAUDE_BASE:-https://downloads.claude.ai/claude-code-releases}
CODEX_REPO=${CODEX_REPO:-openai/codex}
PI_REPO=${PI_REPO:-earendil-works/pi}
HIVEMIND_MANIFEST=${HIVEMIND_MANIFEST:-https://raw.githubusercontent.com/wandb/hivemind/main/manifests/hivemind-latest.json}
AGENT_BROWSER_LATEST=${AGENT_BROWSER_LATEST:-https://registry.npmjs.org/agent-browser/latest}
# Revision of the guest-side agent conditioning below (/etc/environment knobs +
# the ~/.claude.json onboarding seed + the ~/.claude/settings.json permission
# default + the hivemind daemon unit + the agent-browser env wiring and skill +
# the ~/.agents/AGENTS.md guidance text + the docker-compose shim). Versioned
# like IDENTITY_REV so bumping it re-patches every template on the next run even
# when no tool version moved — editing any of it without bumping this ships the
# change to nobody.
AGENT_ENV_REV=12
FORCE=0
[ "${1:-}" = --force ] && FORCE=1

# AB_ARCH names agent-browser's glibc build, not the musl one beside it in the
# same tarball: the guest is ubuntu:24.04.
case "$(uname -m)" in
  x86_64)  CLAUDE_PLAT=linux-x64;   CODEX_ARCH=x86_64;  PI_ARCH=x64;   HM_PLAT=linux-x86_64; AB_ARCH=x64 ;;
  aarch64) CLAUDE_PLAT=linux-arm64; CODEX_ARCH=aarch64; PI_ARCH=arm64; HM_PLAT=linux-arm64;  AB_ARCH=arm64 ;;
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
# Pi publishes self-contained Linux bundles alongside a SHA256SUMS file.
PI_TAG=$(curl -fsSLI -o /dev/null -w '%{url_effective}' \
  "https://github.com/$PI_REPO/releases/latest")
PI_TAG=${PI_TAG##*/}
case "$PI_TAG" in
  v[0-9]*) ;;
  *) echo "bad pi tag from releases/latest redirect: $PI_TAG" >&2; exit 1 ;;
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
# agent-browser: npm's /latest document is version + tarball URL + digest in one
# GET. Insist on sha512 rather than accepting whatever `integrity` names, so a
# registry that ever downgraded the algorithm fails the run instead of silently
# weakening the only check we have on this tarball.
read -r AB_VER AB_URL AB_SHA512 <<EOF
$(curl -fsSL "$AGENT_BROWSER_LATEST" | python3 -c '
import json, sys
d = json.load(sys.stdin)
dist = d["dist"]
if not dist["integrity"].startswith("sha512-"):
    raise SystemExit("agent-browser dist.integrity is not sha512: " + dist["integrity"])
print(d["version"], dist["tarball"], dist["integrity"])')
EOF
case "$AB_VER" in
  [0-9]*.[0-9]*.[0-9]*) ;;
  *) echo "bad agent-browser version from $AGENT_BROWSER_LATEST: $AB_VER" >&2; exit 1 ;;
esac

# The guest identity payload is versioned by the installer itself, so bumping
# IDENTITY_REV there re-patches every template on the next run.
[ -x "$GUEST_IDENTITY" ] || { echo "missing guest identity installer at $GUEST_IDENTITY" >&2; exit 1; }
IDENTITY_REV=$(sed -n 's/^IDENTITY_REV=//p' "$GUEST_IDENTITY" | head -1)
case "$IDENTITY_REV" in
  [0-9]*) ;;
  *) echo "could not read IDENTITY_REV from $GUEST_IDENTITY" >&2; exit 1 ;;
esac

# ---- decide which templates are stale, by asking each one ---------------------
# The single line every template must carry to count as current. One line so
# reading it back is a string compare and not a parse.
#
# TOOLS_REV is the leading half of it: the five words that name a payload a
# GUEST could install for itself. It goes into the manifest below and is what
# `sparkbox update-tools` compares against, which is exactly why identity= and
# agentenv= are not in it — those name systemd units, /etc/environment keys and
# harness config that only a host with the template loop-mounted can write, and
# a guest that thought it could satisfy them would report itself current after
# installing nothing of the sort.
TOOLS_REV="claude=$CLAUDE_VER codex=$CODEX_TAG pi=$PI_TAG hivemind=$HM_VER agentbrowser=$AB_VER"
WANT="$TOOLS_REV identity=$IDENTITY_REV agentenv=$AGENT_ENV_REV"
if [ -n "$GUEST_MOTD_FILE" ]; then
  [ -f "$GUEST_MOTD_FILE" ] \
    || { echo "guest motd file does not exist: $GUEST_MOTD_FILE" >&2; exit 1; }
  MOTD_SHA=$(sha256sum "$GUEST_MOTD_FILE" | awk '{print $1}')
  WANT="$WANT motd=$MOTD_SHA"
fi
if [ -n "$GATEWAY_PUBLIC_KEY_FILE" ]; then
  [ -f "$GATEWAY_PUBLIC_KEY_FILE" ] \
    || { echo "gateway public key file does not exist: $GATEWAY_PUBLIC_KEY_FILE" >&2; exit 1; }
  ssh-keygen -lf "$GATEWAY_PUBLIC_KEY_FILE" >/dev/null \
    || { echo "gateway public key file is not a valid SSH public key: $GATEWAY_PUBLIC_KEY_FILE" >&2; exit 1; }
  GATEWAY_KEY_SHA=$(sha256sum "$GATEWAY_PUBLIC_KEY_FILE" | awk '{print $1}')
  WANT="$WANT gateway_key=$GATEWAY_KEY_SHA"
fi
TEMPLATE_STAMP=/etc/sparkbox/tools-rev

# Read one template's stamp WITHOUT mounting it. debugfs (e2fsprogs) opens the
# image read-only, needs no loop device — so this scales to a box holding dozens
# of snap-<owner>-<name>.ext4 forks — and exits 0 even when the file is absent,
# which is why the OUTPUT is the answer and the exit status is discarded.
template_stamp() {
  debugfs -R "cat $TEMPLATE_STAMP" "$1" 2>/dev/null | tr -d '\0' | head -1 || true
}
command -v debugfs >/dev/null \
  || echo "WARN: debugfs not found (install e2fsprogs) — no template can be read, so every one will be re-patched on every run" >&2

shopt -s nullglob
ALL=()
for tpl in "$IMAGES_DIR"/*.ext4; do
  case "$(basename "$tpl")" in
    snap-*.ext4) continue ;;
  esac
  ALL+=("$tpl")
done
if [ ${#ALL[@]} = 0 ]; then
  echo "no templates in $IMAGES_DIR — nothing to patch" >&2
  exit 1
fi
STALE=()
for tpl in "${ALL[@]}"; do
  if [ "$FORCE" = 0 ] && [ "$(template_stamp "$tpl")" = "$WANT" ]; then
    continue
  fi
  STALE+=("$tpl")
done

# ---- the cache is this run's product, not a side effect of patching ----------
#
# Everything from here to the "already current" exit below used to sit AFTER it.
# That was wrong the moment the host started serving $TOOLS_DIR to its guests:
# the early exit is an `exit 0` and it precedes every download, so a box whose
# templates all carry a current stamp — the freshest boxes, the ones that have
# been refreshing on their timer all along — reached the end of the run with an
# EMPTY $TOOLS_DIR and nothing for `sparkbox update-tools` to pull.
#
# So the run now always resolves, downloads and verifies the artifacts, and then
# asks the separate question of whether any template needs patching. On a warm
# cache this costs nothing: every fetch below is `[ ! -x ]`/`[ ! -f ]`-guarded
# and the version resolution already ran above. What it does change is that a
# broken upstream now fails a run that used to exit 0 without looking, which is
# the honest outcome — a host that cannot verify an artifact has no business
# publishing a manifest that says it did.

# write_tools_manifest publishes what a guest needs to install these same
# artifacts itself. internal/metadata serves this file and the files it names.
#
# Two rules it exists to enforce, both learned the hard way elsewhere:
#
#   The digests are recomputed from the files ON DISK on every run, never
#   carried over from the download block above. A warm cache skips that block
#   entirely, so a digest remembered from the day the file arrived would be a
#   claim about bytes nobody looked at this run — the same shape of lie the
#   header describes the old host-side stamp telling.
#
#   The LAYOUT is data, not something the guest derives. pi and agent-browser
#   are bundles, not binaries: each resolves its own runtime assets relative to
#   the real executable (agent-browser looks for ../skill-data), so a guest that
#   copied the executable into /usr/local/bin would get a CLI whose every
#   `skills` subcommand fails. The install shape — kind, bin, dir, exec, the
#   RELATIVE symlink, the bin/ prune and the directories to drop — is written
#   here beside the patch loop that performs it, so the two cannot drift.
#
# Every value is a QUOTED JSON string, `size` included. The guest parses this
# with tr+awk and no JSON library (install-guest-identity.sh explains why it
# holds to curl/awk/ip), and that pair-walk only recognises `"key": "value"` —
# an unquoted number reads back as empty.
write_tools_manifest() {
  CLAUDE_BIN="$CLAUDE_BIN" CODEX_BIN="$CODEX_BIN" PI_BUNDLE="$PI_BUNDLE" \
  HM_BIN="$HM_BIN" AB_TGZ="$AB_TGZ" \
  CLAUDE_VER="$CLAUDE_VER" CODEX_TAG="$CODEX_TAG" PI_TAG="$PI_TAG" \
  HM_VER="$HM_VER" AB_VER="$AB_VER" AB_ARCH="$AB_ARCH" \
  TOOLS_ARCH="$(uname -m)" TOOLS_REV="$TOOLS_REV" \
  python3 - "$TOOLS_DIR/manifest.json" <<'PY'
import datetime, hashlib, json, os, sys

out = sys.argv[1]
env = os.environ


def digest(path):
    h = hashlib.sha256()
    with open(path, "rb") as f:
        # Chunked: the agent-browser tarball is ~92MB and this also runs in the
        # CKS prepare-vm-assets init container, which is memory-capped.
        for block in iter(lambda: f.read(1 << 20), b""):
            h.update(block)
    return h.hexdigest()


def tool(name, key, version, src, kind, **layout):
    e = {
        "name": name,
        "key": key,
        "version": version,
        "file": os.path.basename(src),
        "sha256": digest(src),
        "size": str(os.path.getsize(src)),
        "kind": kind,
        "bin": "",
        "dir": "",
        "exec": "",
        "link": "",
        "keep_only": "",
        "drop": "",
    }
    e.update(layout)
    return e


ab_exec = "bin/agent-browser-linux-" + env["AB_ARCH"]
tools = [
    tool("claude", "claude", env["CLAUDE_VER"], env["CLAUDE_BIN"], "binary",
         bin="/usr/local/bin/claude"),
    tool("codex", "codex", env["CODEX_TAG"], env["CODEX_BIN"], "binary",
         bin="/usr/local/bin/codex"),
    tool("pi", "pi", env["PI_TAG"], env["PI_BUNDLE"], "bundle",
         bin="/usr/local/bin/pi", dir="/usr/local/lib/pi", exec="pi",
         link="../lib/pi/pi"),
    tool("hivemind", "hivemind", env["HM_VER"], env["HM_BIN"], "binary",
         bin="/usr/local/bin/hivemind"),
    # keep_only and drop are the ~92MB -> ~13MB prune the patch loop performs:
    # the tarball carries seven platform binaries and a postinstall we never
    # run. In a guest it is not merely tidy — those bytes land in that VM's own
    # 25 GiB ceiling and against its owner's pool, once per sandbox.
    tool("agent-browser", "agentbrowser", env["AB_VER"], env["AB_TGZ"], "bundle",
         bin="/usr/local/bin/agent-browser", dir="/usr/local/lib/agent-browser",
         exec=ab_exec, link="../lib/agent-browser/" + ab_exec,
         keep_only=ab_exec, drop="scripts"),
]

doc = {
    "arch": env["TOOLS_ARCH"],
    "rev": env["TOOLS_REV"],
    "generated_at": datetime.datetime.now(datetime.timezone.utc)
        .replace(microsecond=0).isoformat().replace("+00:00", "Z"),
    "tools": tools,
}

tmp = out + ".new"
with open(tmp, "w") as f:
    json.dump(doc, f, indent=2)
    f.write("\n")
os.chmod(tmp, 0o644)
# Rename into place: the metadata server may be reading this while we write it,
# and it memoises on (size, mtime) — a half-written file it happened to parse
# would be cached until the next run moved those numbers again.
os.replace(tmp, out)
PY
}

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

PI_ASSET="pi-linux-$PI_ARCH.tar.gz"
PI_BUNDLE="$TOOLS_DIR/pi-$PI_TAG-linux-$PI_ARCH.tar.gz"
if [ ! -f "$PI_BUNDLE" ]; then
  echo ">> downloading pi $PI_TAG (linux-$PI_ARCH)"
  PI_RELEASE="https://github.com/$PI_REPO/releases/download/$PI_TAG"
  PI_SHA=$(curl -fsSL "$PI_RELEASE/SHA256SUMS" \
    | awk -v asset="$PI_ASSET" '$2 == asset { print $1; exit }')
  if [ ${#PI_SHA} -ne 64 ] || [[ "$PI_SHA" = *[!0-9a-fA-F]* ]]; then
    echo "no valid checksum for $PI_ASSET in $PI_TAG SHA256SUMS" >&2
    exit 1
  fi
  curl -fsSL "$PI_RELEASE/$PI_ASSET" -o "$PI_BUNDLE.tmp"
  echo "$PI_SHA  $PI_BUNDLE.tmp" | sha256sum -c - >/dev/null
  mv "$PI_BUNDLE.tmp" "$PI_BUNDLE"
fi

HM_BIN="$TOOLS_DIR/hivemind-$HM_VER-$HM_PLAT"
if [ ! -x "$HM_BIN" ]; then
  echo ">> downloading hivemind $HM_VER ($HM_PLAT)"
  curl -fsSL "$HM_URL" -o "$HM_BIN.tmp"
  echo "$HM_SHA  $HM_BIN.tmp" | sha256sum -c - >/dev/null
  chmod 0755 "$HM_BIN.tmp" && mv "$HM_BIN.tmp" "$HM_BIN"
fi

# One cached tarball serves every template on this box; it is pruned down to a
# single platform at INSTALL time, not here, so the cache stays a byte-for-byte
# copy of what the registry served and its digest keeps meaning something.
AB_TGZ="$TOOLS_DIR/agent-browser-$AB_VER.tgz"
if [ ! -f "$AB_TGZ" ]; then
  echo ">> downloading agent-browser $AB_VER (linux-$AB_ARCH)"
  curl -fsSL "$AB_URL" -o "$AB_TGZ.tmp"
  # npm's integrity is base64 sha512, so `sha256sum -c` has nothing to compare
  # against and the hex habit from the other four tools does not transfer.
  # python3 is already a hard dependency of this script (see the hivemind
  # manifest parse above), so this needs nothing new on the host.
  AB_GOT=$(python3 -c 'import base64, hashlib, sys
with open(sys.argv[1], "rb") as f:
    print("sha512-" + base64.b64encode(hashlib.sha512(f.read()).digest()).decode())' \
    "$AB_TGZ.tmp")
  if [ "$AB_GOT" != "$AB_SHA512" ]; then
    echo "agent-browser $AB_VER integrity mismatch: got $AB_GOT want $AB_SHA512" >&2
    rm -f "$AB_TGZ.tmp"; exit 1
  fi
  mv "$AB_TGZ.tmp" "$AB_TGZ"
fi

# The host-side stamp is now a RECORD, not a decision: what counts as current is
# read out of each template above. Keep writing it because it is what an operator
# (and `sparkbox doctor`) reads to see which versions this box last resolved.
printf 'CLAUDE_VERSION=%s\nCODEX_TAG=%s\nPI_TAG=%s\nHIVEMIND_VERSION=%s\nAGENT_BROWSER_VERSION=%s\nIDENTITY_REV=%s\nAGENT_ENV_REV=%s\n' \
  "$CLAUDE_VER" "$CODEX_TAG" "$PI_TAG" "$HM_VER" "$AB_VER" "$IDENTITY_REV" "$AGENT_ENV_REV" > "$STAMP"
# Drop cached binaries from older versions; keep the current set. Before the
# manifest is written, never after: a manifest naming a file this prune has
# already deleted would hand every guest a 404 for the length of a refresh
# interval.
find "$TOOLS_DIR" -maxdepth 1 -type f \( -name 'claude-*' -o -name 'codex-*' -o -name 'pi-*' \
    -o -name 'hivemind-*' -o -name 'agent-browser-*' \) \
  ! -name "$(basename "$CLAUDE_BIN")" ! -name "$(basename "$CODEX_BIN")" \
  ! -name "$(basename "$PI_BUNDLE")" ! -name "$(basename "$HM_BIN")" \
  ! -name "$(basename "$AB_TGZ")" -delete
write_tools_manifest

if [ ${#STALE[@]} = 0 ]; then
  echo "templates already current (claude $CLAUDE_VER, codex $CODEX_TAG, pi $PI_TAG, hivemind $HM_VER, agent-browser $AB_VER, identity rev $IDENTITY_REV, agent env rev $AGENT_ENV_REV): ${#ALL[@]} checked, 0 stale"
  exit 0
fi
echo ">> ${#STALE[@]} of ${#ALL[@]} template(s) need patching"

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
# A third gate is not first-run but every-turn, and it is the one that actually
# costs the owner something: each tool call waits for an approval only a human
# at a terminal can give. A sandbox exists so that work can proceed while nobody
# is watching, and the VM boundary — not a prompt inside it — is what contains
# that work, so the guest is seeded to start in `auto` (see below).
#
# We satisfy (1) by seeding the config below, and (2) with CLAUDE_CODE_SANDBOXED,
# a first-class escape hatch in the binary (its trust check opens with
# `if (CLAUDE_CODE_SANDBOXED) return true`) — that part is confirmed: no TUI
# dialog appears and no session hangs waiting for one.
#
# CONFIRMED WRONG, as of claude-code 2.1.236: the second half of that claim —
# that the same flag also stops a cloned repo's .claude/settings.json
# `permissions.allow` entries from being dropped as untrusted — does not hold.
# Live in a provisioned box, with CLAUDE_CODE_SANDBOXED=1 verified set (checked
# /etc/environment, `systemctl --user show-environment`, and the shell), both
# `claude -p` and `claude config list` still print "Ignoring N
# permissions.allow entries from .claude/settings.json: this workspace has not
# been trusted", because `hasTrustDialogAccepted` for the project's cwd is
# never set to true anywhere — CLAUDE_CODE_SANDBOXED silences the interactive
# prompt without also flipping that flag. The allow-rules a checked-in
# .claude/settings.json ships are therefore silently inert in every sandbox:
# not a hang, not a blocked command (defaultMode `auto` below still governs
# what runs), just an allowlist nobody gets the benefit of.
#
# Fixing it needs `projects["<cwd>"].hasTrustDialogAccepted: true` stamped into
# ~/.claude.json for the path a repo actually lands on, which has to happen
# wherever a repo is attached and cloned into the guest at boot — this script
# only patches the rootfs TEMPLATE, before any specific repo or path exists, so
# it cannot be the one to stamp it. That boot-time step is not in this tree;
# whoever owns it is the one who can add the stamp.
#
# Note this seeds config, never credentials: auth stays the env token that
# envsync pushes per-tag, and no ~/.claude/.credentials.json is ever written, so
# there is no credential state to sync between host and guest or across boxes.
# set_guest_env ASSERTS a value this script owns, instead of defaulting it.
#
# The append-if-absent idiom below it (`grep -qs '^KEY=' || echo >>`) is right
# for a flag that is either set or not and whose value never changes. It is
# WRONG for a tunable, and templates are the reason: the patch loop reflinks the
# template it is replacing, so /etc/environment already carries whatever an
# earlier run appended. Under the skip-if-present form, editing a value in this
# file and bumping AGENT_ENV_REV re-patches and re-stamps every template — and
# changes nothing, because the key is already there. The run then reports
# success for a value that never landed, which is the same shape of lie the
# header rewrote this script to stop telling.
set_guest_env() {
  local mnt=$1 key=$2 val=$3
  sed -i "/^$key=/d" "$mnt/etc/environment"
  printf '%s=%s\n' "$key" "$val" >> "$mnt/etc/environment"
}

seed_agent_env() {
  local mnt=$1
  # The template stays the single source of tool versions; don't let each guest
  # race to self-update on top of it (wasted bandwidth, mid-session surprises).
  grep -qs '^DISABLE_AUTOUPDATER=' "$mnt/etc/environment" || \
    echo 'DISABLE_AUTOUPDATER=1' >> "$mnt/etc/environment"
  grep -qs '^CLAUDE_CODE_SANDBOXED=' "$mnt/etc/environment" || \
    echo 'CLAUDE_CODE_SANDBOXED=1' >> "$mnt/etc/environment"

  # agent-browser will not otherwise find the Chrome this image already ships.
  # Its PATH probe looks for google-chrome / google-chrome-stable / chromium /
  # chromium-browser / brave-browser, and /headless-shell/headless-shell is named
  # none of those — so having it on PATH (images/environment) buys nothing on its
  # own. Unset, the CLI reports "Chrome not found" and tells the agent to run
  # `agent-browser install`, which downloads a SECOND ~170MB Chrome-for-Testing
  # per sandbox from storage.googleapis.com: a host that is not in the sluice
  # allowlist, so on a tagged box it does not even fail fast, it NXDOMAINs. On
  # arm64 it cannot succeed at all — Chrome for Testing publishes no Linux ARM64
  # build, and arm64 is the DGX.
  #
  # This is an env var and not a seeded ~/.agent-browser/config.json even though
  # config wins over env in agent-browser's precedence, for three reasons: only
  # /etc/environment reaches the non-interactive `ssh box '<cmd>'` execs agents
  # actually run (they read no profile); a file in $HOME would become user state
  # that fork templates carry and that we would then rewrite on every refresh;
  # and two sources that can disagree is exactly how you get a working `open`
  # followed by a failing `snapshot`, because the daemon compares the launch
  # config it cached against what the next command hands it.
  # Guarded on the binary actually being in THIS template. The script patches
  # templates it did not build (that is the whole point of install_gateway_key's
  # "images we did not make" case), and a bad executable path is not a soft
  # failure: agent-browser does not fall back to its PATH probe, it dies with
  # `Failed to launch Chrome at "..."`. Pointing at a file that is not there
  # would take a template whose only problem was an unusual Chrome location and
  # give it no browser at all.
  if [ -x "$mnt/headless-shell/headless-shell" ]; then
    set_guest_env "$mnt" AGENT_BROWSER_EXECUTABLE_PATH /headless-shell/headless-shell
  else
    echo "   !! no /headless-shell/headless-shell in template; leaving agent-browser to its PATH probe" >&2
  fi

  # Chrome is the one tool here that stays resident after the command that
  # started it returns: the daemon's default idle timeout is a full hour, so one
  # forgotten session holds a browser's RSS long after the agent moved on, and
  # the reaper's balloon/CPU feedback loop is what pays for it. Ten minutes is
  # still long enough that a multi-step browsing task never eats a relaunch.
  # Unlike the hivemind daemon below, this bound is reasoned rather than
  # measured — if a sandbox ever looks memory-starved with an idle browser in it,
  # measure before raising it.
  set_guest_env "$mnt" AGENT_BROWSER_IDLE_TIMEOUT_MS 600000

  # Deliberately NOT seeded: AGENT_BROWSER_ARGS=--no-sandbox. The guest kernel
  # can run Chrome's own sandbox — the Firecracker CI 6.1 config both arches
  # build from has CONFIG_USER_NS=y, and it does not build AppArmor at all, so
  # Ubuntu 24.04's kernel.apparmor_restrict_unprivileged_userns clamp does not
  # exist in here to get in the way. Turning the renderer sandbox off for every
  # sandbox on the fleet would be a call made once, here, on behalf of every user
  # of a VM that holds their repos and their ~/.ssh — the same call the `auto`
  # vs `bypassPermissions` comment below refuses to make for them. If a live box
  # ever disagrees, `unshare -U true` says so in one command, and the fix is one
  # line here plus an AGENT_ENV_REV bump.

  # Resolve the login user from the template itself rather than hardcoding
  # `sparky`, so this tracks the sparkbox.login-user label if it ever changes.
  local pw home uid gid
  pw=$(awk -F: '$3 == 1000 {print; exit}' "$mnt/etc/passwd") || return 0
  [ -n "$pw" ] || { echo "   !! no uid-1000 user in template; skipping claude seed" >&2; return 0; }
  uid=$(echo "$pw" | cut -d: -f3)
  gid=$(echo "$pw" | cut -d: -f4)
  home=$(echo "$pw" | cut -d: -f6)
  [ -d "$mnt$home" ] || { echo "   !! $home missing in template; skipping claude seed" >&2; return 0; }

  # One daemon per guest, not two. agent-browser puts its socket in
  # $AGENT_BROWSER_SOCKET_DIR, else $XDG_RUNTIME_DIR/agent-browser, else
  # $HOME/.agent-browser — and this image exports XDG_RUNTIME_DIR from ~/.bashrc
  # and ~/.profile but NOT from /etc/environment. Left alone, an interactive
  # shell and a non-interactive ssh exec would resolve different directories,
  # drive different daemons, and open different browsers, so a page opened in
  # one is simply not there to snapshot from the other. Pinning it to $HOME is
  # what makes the two agree; it also means the socket outlives a cold boot, so
  # the CLI's stale-socket handling is what has to be right rather than tmpfs.
  set_guest_env "$mnt" AGENT_BROWSER_SOCKET_DIR "$home/.agent-browser"

  # MERGE, never overwrite. This loop no longer walks `snap-<owner>-<name>.ext4`
  # fork templates — the candidate list skips them (see the `continue` above), for
  # the security reason in the header — so the merge discipline is belt-and-braces
  # rather than the requirement it was written as. Keep it: a base template can
  # still carry operator state, ~/.claude.json is a real user's accumulated state
  # wherever it does exist (project trust, history pointers, theme choice), and
  # the exclusion is a policy that has changed once already. We assert only the
  # onboarding keys, and leave a theme already chosen by the user alone.
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
cfg.setdefault("theme", "auto")

tmp = path + ".seed-new"
with open(tmp, "w") as f:
    json.dump(cfg, f, indent=2)
os.replace(tmp, path)
PY
  chown "$uid:$gid" "$mnt$home/.claude.json"
  chmod 0644 "$mnt$home/.claude.json"

  # The starting permission mode goes in ~/.claude/settings.json — the USER
  # scope — because it has to hold for every directory the owner later clones or
  # creates. A project-scoped .claude/settings.json would only cover the one
  # directory that happened to ship it.
  #
  # `auto` and not `bypassPermissions`: auto still stops for the things whose
  # blast radius leaves the box, which is exactly the line the VM boundary does
  # not draw for us. Seeding a full bypass would make that call once, here, on
  # behalf of every user of every sandbox; `--dangerously-skip-permissions` stays
  # theirs to type. skipAutoPermissionPrompt is the one-time "you are entering
  # auto mode" acknowledgement, which nobody can answer on a machine they have
  # not connected to yet.
  #
  # MERGED, for the reason .claude.json above is: this loop walks fork templates
  # carrying a real user's settings, and someone who deliberately moved off auto
  # must not have it put back under them by the next refresh. setdefault only.
  mkdir -p "$mnt$home/.claude"
  python3 - "$mnt$home/.claude/settings.json" <<'PY'
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

perms = cfg.setdefault("permissions", {})
if not isinstance(perms, dict):
    print(f"   !! {path} has a non-object permissions block; leaving it alone", file=sys.stderr)
    sys.exit(0)
perms.setdefault("defaultMode", "auto")
cfg.setdefault("skipAutoPermissionPrompt", True)

# Deliberately NOT seeded here: an allow rule for Bash(agent-browser:*). The
# browser can read any local file through file://, run arbitrary JavaScript via
# `eval`, and reach the network — so a standing fleet-wide grant would hand an
# agent that ingested a prompt-injected README exactly the capability `auto`
# exists to keep asking about, and would undo the line the comment above draws.
# It is not needed for the browser to be usable, either: upstream's SKILL.md
# carries `allowed-tools: Bash(agent-browser:*)` in its own frontmatter, so the
# grant applies while the skill that teaches the tool is loaded, and nowhere else.

tmp = path + ".seed-new"
with open(tmp, "w") as f:
    json.dump(cfg, f, indent=2)
os.replace(tmp, path)
PY
  chown -R "$uid:$gid" "$mnt$home/.claude"
  chmod 0644 "$mnt$home/.claude/settings.json"

  seed_hivemind_unit "$mnt" "$home" "$uid" "$gid" "$(echo "$pw" | cut -d: -f1)"
  install_docker_compose_shim "$mnt"
}

# `docker-compose` (hyphenated), forwarding to the v2 plugin the image ships.
#
# Ubuntu's docker-compose-v2 package installs ONLY
# /usr/libexec/docker/cli-plugins/docker-compose, so `docker compose` works and
# `docker-compose` is not a command at all. Every Makefile, README and CI script
# written before 2023 calls the hyphenated form, so the first thing an agent
# meets in a real repository is a missing binary — and its own repair is to
# write this file itself, somewhere only that VM has it.
#
# NOT the `docker-compose` package in noble: that is Compose v1 (1.29.2), a
# Python rewrite that upstream retired in 2023, and it would shadow v2 with a
# tool that names containers and networks differently and cannot read the
# current Compose spec. Installing it would make the command exist and the
# builds wrong, which is worse than absent.
#
# NOT a symlink to the plugin either, though that does run: invoked directly the
# binary takes Compose's standalone path, which resolves the daemon from the
# environment rather than through the docker CLI's contexts and config. `exec
# docker compose` is identical to what a person typing `docker compose` gets,
# by construction rather than by matching behaviour.
install_docker_compose_shim() {
  local mnt=$1
  [ -x "$mnt/usr/libexec/docker/cli-plugins/docker-compose" ] || {
    echo "   .. no compose plugin in template; skipping docker-compose shim" >&2
    return 0
  }
  cat > "$mnt/usr/local/bin/docker-compose" <<'EOF'
#!/bin/sh
# Sparkbox: `docker-compose` is not a command on Ubuntu 24.04, which ships only
# the v2 plugin. Forward to it so tooling written for the hyphenated name works.
exec docker compose "$@"
EOF
  chmod 0755 "$mnt/usr/local/bin/docker-compose"
}

# Install one short, platform-owned guide at the harness-specific global paths.
# This runs only against trusted base templates (snap-* and live VM rootfs files
# never enter STALE), so the next sandbox clone inherits it while existing
# sandboxes remain byte-for-byte untouched.
install_agent_guidance() {
  local mnt=$1
  local pw home uid gid canonical
  pw=$(awk -F: '$3 == 1000 {print; exit}' "$mnt/etc/passwd") || return 0
  [ -n "$pw" ] || { echo "   !! no uid-1000 user in template; skipping agent guidance" >&2; return 0; }
  uid=$(echo "$pw" | cut -d: -f3)
  gid=$(echo "$pw" | cut -d: -f4)
  home=$(echo "$pw" | cut -d: -f6)
  [ -d "$mnt$home" ] || { echo "   !! $home missing in template; skipping agent guidance" >&2; return 0; }

  mkdir -p "$mnt$home/.agents" "$mnt$home/.codex" "$mnt$home/.claude"
  canonical="$mnt$home/.agents/AGENTS.md"
  cat > "$canonical" <<'EOF'
You are running in a Sparkbox microVM.

Sparkbox documentation: run `sparkbox docs`. The HTTPS proxy specifically is
documented at `sparkbox docs proxy`.

Your disk is persistent. CPU and memory are shared across your Sparkbox owner
pool; idle guest memory may be reclaimed and returned when it becomes active.

`sparkbox pin` protects the VM from idle pause and memory-pressure
reclamation, and `sparkbox unpin` returns it to the shared pool; `sparkbox
status` shows the current state. Pin only when the owner asks you to. A request
to a paused VM wakes it, so a service stays reachable without being pinned, and
pinning consumes shared capacity for as long as it lasts.

The VM can also manage its default HTTPS endpoint: `sparkbox set-port PORT`
changes the forwarded port, `sparkbox make-public` allows unauthenticated
access to all of this VM's routes, and `sparkbox make-private` restores the
authenticated default.

This VM's name is its hostname, so `$(hostname)` is the name. The domain is
this deployment's own, not a constant across every Sparkbox install — read it
rather than assume one, from the `domain:` line `sparkbox whoami` reports:

    DOMAIN=$(sparkbox whoami | sed -n 's/^domain: //p')
    echo "https://$(hostname).$DOMAIN"      # the default endpoint above
    echo "https://$(hostname).$DOMAIN:5173" # any other port, named in the URL

The edge exposes the common development ports — 3000, 3001, 4000, 4200, 5000,
5173, 6006, 7860, 8000, 8080, 8081, 8082, 8083, 8123, 8443, 8501, 8888, 9000
and 16686 — so listen on one of those, and on 0.0.0.0 rather than 127.0.0.1,
or nothing outside the VM can reach it.

When you start a dev service, point the default endpoint at the port a person
should open. A stack serving an API on 8080 and a Vite frontend on 5173 gets
`sparkbox set-port 5173`, because the frontend is the human entrypoint. Record
the other ports as session labels rather than leaving them undiscoverable:

    hivemind tag api_url="https://$(hostname).$DOMAIN:8080"

`hivemind tag` labels the session you are working in, so it can be found later
by what it was about. A bare word is a tag (`hivemind tag nightly`) and
`KEY=VALUE` records a value (`hivemind tag pr=1234`). Labels expire on their
own: `--pin` keeps one indefinitely, `--ttl 2h` sets your own window.
`hivemind tag --list` shows what this session carries and
`hivemind tag --remove KEY` clears one. Label whatever makes a session worth
finding again — the issue or PR being worked, an experiment name, and the URL of
any service you started.

A dev server's own Host-header check and hot-reload client normally assume
`localhost`, which this domain never is, and most frameworks need one config
line to accept it instead — Vite's `server.allowedHosts`, Next's
`allowedDevOrigins`, Django's `ALLOWED_HOSTS`/`CSRF_TRUSTED_ORIGINS`, and so on.
Full per-framework fixes: `sparkbox docs dev-environment`. Run the service as
a `systemd --user` unit rather than a foreground shell, so it survives your
SSH session ending.

Write down what you did as `.sparkbox/setup.sh` in the project's own repo —
dependency install, the unit file, the `sparkbox set-port` call — so a fresh
checkout or a fresh VM can run `bash .sparkbox/setup.sh` and reach the same
running state instead of re-deriving it. Check for one before redoing this
work by hand, read it before running it, and keep it current as the setup
changes.

GitHub repositories attached to this VM are cloned into your home directory at
boot: `~/<repo>` when one is attached, `~/src/<owner>/<repo>` when several are.
`sparkbox repos` lists every attachment, where it lives, and its git state.
Attaching a repository to a VM that is already running deliberately does not
clone it, because that writes into a filesystem you are working in; run
`sparkbox repos sync` when you want the new one fetched.

`sparkbox repos sync` also brings existing checkouts forward, and it can only do
three things to one: fetch it, fast-forward it when the tree is clean, or tell
you why it did neither. It never resets, rebases, stashes or discards anything —
uncommitted work, untracked files and unpushed commits are all reported and left
exactly where they are. If a VM was forked from a snapshot that had a checkout
in it, the first boot is also allowed to switch that checkout to the branch the
attachment names, and only then, and only if the tree is clean.

Do not create, paste, or store a GitHub token. git already authenticates to
github.com through a system credential helper that asks this host for a
short-lived token scoped to the one repository being fetched, so `git clone`,
`git fetch` and `git push` on an attached repository just work, and nothing
durable holds a credential.

Before opening a pull request, run `sparkbox repo authorize owner/name` for that
checkout if the PR should appear as this VM's owner. Authorization is per repo;
without it, GitHub access still works but PRs and other API actions are
attributed to the Sparkbox bot.

git's author is usually already set, to the GitHub account that owns this VM, so
commits you make are attributed to that person. Leave it alone: an author you
invent cannot be corrected on a branch that has been pushed. If git does ask who
you are, this VM's owner has no GitHub account linked and only they can answer —
ask them to run `git config --global user.name` and `user.email`, and do not
choose a value yourself.

A headless Chrome and the `agent-browser` CLI are already installed and already
pointed at each other. Use `agent-browser` for anything that needs a real
browser, and run `agent-browser skills get core` for the command reference. Do
not run `agent-browser install` and do not download another browser: that host
is not on this VM's egress allowlist, and on arm64 it publishes no build at all.
If this VM carries a tag, its egress is filtered, so pages on the open web may
not resolve even though a service you are running on this VM will.

The agent CLIs here — claude, codex, pi, hivemind and agent-browser — came from
the template this VM was created from, and their own auto-updaters are turned
off, so a VM that has been alive a while keeps the versions that template
shipped with. `sparkbox update-tools --check` compares them against what this
host has cached, and `sparkbox update-tools` installs the difference. It pulls
from the host over the same channel as everything else here, not from the open
internet, so it works even on a VM whose egress is filtered by its tag. A newly
created VM normally reports everything current.

Only use documented Sparkbox features. Undocumented local endpoints, metadata
services, gateway ports, and node services are internal infrastructure and may
change without notice.
EOF

  # Respect a future base image that intentionally supplies a regular harness
  # file. Our own symlinks are safe to update idempotently on every template
  # refresh, but replacing a real file would silently discard its instructions.
  for spec in ".codex/AGENTS.md" ".claude/CLAUDE.md"; do
    if [ -e "$mnt$home/$spec" ] && [ ! -L "$mnt$home/$spec" ]; then
      echo "   !! $home/$spec is a regular file; leaving it unchanged" >&2
      continue
    fi
    ln -sfn ../.agents/AGENTS.md "$mnt$home/$spec"
  done
  chown -R "$uid:$gid" "$mnt$home/.agents" "$mnt$home/.codex" "$mnt$home/.claude"
  chmod 0755 "$mnt$home/.agents" "$mnt$home/.codex" "$mnt$home/.claude"
  chmod 0644 "$canonical"
}

# Put agent-browser's own skill where each harness looks for it.
#
# Sourced from the copy installed under /usr/local/lib above, NOT from a file
# vendored into this repo: upstream's SKILL.md is a deliberate ~3.4KB discovery
# stub whose body says little more than "run `agent-browser skills get core`",
# precisely so the guidance always matches the installed CLI. A vendored copy
# would be a second thing to remember to bump every time the tool moves, and a
# `npx skills add` at guest boot would fetch repo HEAD over a network that a
# fresh sandbox has only just acquired — the class of bug already recorded in
# the first-attach race.
#
# Same canonical-file-plus-symlinks shape install_agent_guidance uses for
# AGENTS.md, one directory deeper. Codex was observed reading ~/.agents/skills in
# one probe and $CODEX_HOME/skills in another; it gets a link either way, because
# a symlink costs nothing and this does not need us to be right about which.
install_agent_browser_skill() {
  local mnt=$1
  local pw home uid gid src spec target
  pw=$(awk -F: '$3 == 1000 {print; exit}' "$mnt/etc/passwd") || return 0
  [ -n "$pw" ] || { echo "   !! no uid-1000 user in template; skipping agent-browser skill" >&2; return 0; }
  uid=$(echo "$pw" | cut -d: -f3)
  gid=$(echo "$pw" | cut -d: -f4)
  home=$(echo "$pw" | cut -d: -f6)
  [ -d "$mnt$home" ] || { echo "   !! $home missing in template; skipping agent-browser skill" >&2; return 0; }

  # FATAL, unlike the two guards above. Those fire on a malformed template — an
  # image with no uid-1000 user or no home — where skipping one guest's skill is
  # the proportionate response. This one fires when the npm tarball's layout
  # moved under us, which is a live possibility (0.35.0 ships TWO skill trees,
  # skills/ and skill-data/) and needs a human. Returning 0 here would let the
  # loop reach the stamp and write agentenv=7 onto a template with no skill in
  # it, and no later run would ever retry — the exact "claims tools it never
  # received" failure the header describes.
  src="$mnt/usr/local/lib/agent-browser/skills/agent-browser/SKILL.md"
  [ -f "$src" ] || { echo "agent-browser $AB_VER ships no SKILL.md at ${src#"$mnt"}" >&2; exit 1; }

  mkdir -p "$mnt$home/.agents/skills/agent-browser" "$mnt$home/.claude/skills" \
           "$mnt$home/.codex/skills" "$mnt$home/.pi/agent/skills"
  install -m 0644 "$src" "$mnt$home/.agents/skills/agent-browser/SKILL.md"

  # The same guard install_agent_guidance carries: a REAL directory at one of
  # these paths is somebody's own skill of that name, and replacing it would
  # silently discard it. Links are relative so they survive the home moving or
  # the login user being renamed, for the reason install_agent_guidance writes
  # ../.agents/.
  for spec in ".claude/skills/agent-browser:../../.agents/skills/agent-browser" \
              ".codex/skills/agent-browser:../../.agents/skills/agent-browser" \
              ".pi/agent/skills/agent-browser:../../../.agents/skills/agent-browser"; do
    target=${spec#*:}; spec=${spec%%:*}
    if [ -e "$mnt$home/$spec" ] && [ ! -L "$mnt$home/$spec" ]; then
      echo "   !! $home/$spec is not a symlink; leaving it unchanged" >&2
      continue
    fi
    ln -sfn "$target" "$mnt$home/$spec"
  done

  chown -R "$uid:$gid" "$mnt$home/.agents" "$mnt$home/.claude" \
    "$mnt$home/.codex" "$mnt$home/.pi"
}

# install_gateway_key replaces the release template's build-time fleet key
# with the key mounted into this one-shot preparation container. These are
# trusted base templates only (snap-* images never enter STALE), so replacing
# rather than merging also removes an obsolete release/operator key.
install_gateway_key() {
  local mnt=$1
  [ -n "$GATEWAY_PUBLIC_KEY_FILE" ] || return 0

  local pw home uid gid ssh_dir key
  pw=$(awk -F: '$3 == 1000 {print; exit}' "$mnt/etc/passwd")
  [ -n "$pw" ] || pw=$(awk -F: '$1 == "root" {print; exit}' "$mnt/etc/passwd")
  [ -n "$pw" ] || { echo "   !! template has no uid-1000 or root login" >&2; return 1; }
  uid=$(echo "$pw" | cut -d: -f3)
  gid=$(echo "$pw" | cut -d: -f4)
  home=$(echo "$pw" | cut -d: -f6)
  case "$home" in
    /*) ;;
    *) echo "   !! template login home is not absolute: $home" >&2; return 1 ;;
  esac
  key=$(awk 'NF && $1 !~ /^#/ {print; exit}' "$GATEWAY_PUBLIC_KEY_FILE")
  [ -n "$key" ] || { echo "   !! gateway public key file is empty" >&2; return 1; }

  ssh_dir="$mnt$home/.ssh"
  mkdir -p "$ssh_dir"
  printf '%s\n' "$key" > "$ssh_dir/authorized_keys"
  chown "$uid:$gid" "$ssh_dir" "$ssh_dir/authorized_keys"
  chmod 0700 "$ssh_dir"
  chmod 0600 "$ssh_dir/authorized_keys"
}

# seed_hivemind_unit pre-arms the session-sync daemon so a fresh sandbox is
# already recording before anyone types anything. Until now `hivemind start` had
# to be run by hand in every new box, and a session that was never synced is not
# recoverable after the fact — the whole point of the tool is the history.
#
# This writes the SAME user unit `hivemind start` writes, rather than a system
# unit of our own, for two reasons: the daemon must run as the login user (its
# state, its credentials chain, and the session files it watches all live in
# that home), and anyone who later runs `hivemind stop/restart/start` then finds
# exactly the unit their CLI expects to manage instead of a competing copy.
#
# Cost was measured before enabling this by fleet default: a daemon in a real
# sandbox burned 0 CPU ticks over 60s while actively syncing, against the idle
# reaper's 2%-of-a-core activity threshold. It syncs in proportion to work the
# sandbox is already doing, so it cannot by itself hold a box awake. Do not
# assume that stays true if the daemon ever grows a periodic poll.
seed_hivemind_unit() {
  local mnt=$1 home=$2 uid=$3 gid=$4 user=$5
  local unitdir="$mnt$home/.config/systemd/user"

  mkdir -p "$unitdir/default.target.wants"
  cat > "$unitdir/hivemind.service" <<'EOF'
[Unit]
Description=HiveMind - Sync agentic coding sessions to W&B
Documentation=https://github.com/wandb/agentstream-py
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=/usr/local/bin/hivemind run
ExecReload=/bin/kill -HUP $MAINPID
Restart=always
RestartSec=10

Environment="LC_ALL=C.UTF-8"

StandardOutput=journal
StandardError=journal
SyslogIdentifier=hivemind

NoNewPrivileges=true

[Install]
WantedBy=default.target
EOF
  # Don't start before the workload-identity token exists.
  #
  # hivemind resolves its credential chain ONCE, at startup. Start it a second
  # too early and it runs for the life of the box telling the user to run
  # `hivemind login` — on a box that has a perfectly good OIDC token on disk,
  # written moments after the daemon gave up looking for it. Measured on a fresh
  # CKS sandbox: daemon up 1.7s after boot, token written at 11.5s.
  #
  # This has to be a WAIT and cannot be an After=: this is a user unit and the
  # token is written by a system one, and systemd will not order across the two
  # managers. See install-guest-identity.sh, which also moved the fetch itself
  # into boot ordering so the wait is normally over before it starts.
  #
  # And it has to be a DROP-IN rather than a line in the unit above, because
  # `hivemind start` rewrites hivemind.service from its own template every time
  # it runs — it says so: "Updated systemd unit at ...". A drop-in is a separate
  # file it does not know about, so the guarantee survives the user restarting
  # their own daemon, which is precisely when they are already troubleshooting.
  mkdir -p "$unitdir/hivemind.service.d"
  cat > "$unitdir/hivemind.service.d/10-sparkbox-token.conf" <<'EOF'
[Service]
# The leading - keeps a missing or broken waiter from being able to stop
# the daemon: this drop-in makes the token MORE likely to be there, and must
# never be the reason nothing starts at all.
ExecStartPre=-/usr/local/bin/sparkbox-await-token
EOF
  chmod 0644 "$unitdir/hivemind.service.d/10-sparkbox-token.conf"

  # Enable without a chroot, the same way install-guest-identity.sh does: the
  # .wants symlink IS what `systemctl --user enable` writes.
  ln -sfn ../hivemind.service "$unitdir/default.target.wants/hivemind.service"
  chown -h "$uid:$gid" "$unitdir/default.target.wants/hivemind.service"
  chown "$uid:$gid" "$unitdir/hivemind.service" "$unitdir/default.target.wants" \
    "$unitdir/hivemind.service.d/10-sparkbox-token.conf" "$unitdir/hivemind.service.d" \
    "$unitdir" "$mnt$home/.config/systemd" "$mnt$home/.config"
  chmod 0644 "$unitdir/hivemind.service"

  # A user unit only runs while that user has a session unless the user lingers.
  # The base image already sets this for uid 1000, but a fork template built
  # from an older image (or a future login-user change) would not, and a missing
  # marker means the daemon silently never starts on a cold boot.
  mkdir -p "$mnt/var/lib/systemd/linger"
  : > "$mnt/var/lib/systemd/linger/$user"
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

patched=0
for tpl in "${STALE[@]}"; do
  echo ">> patching $(basename "$tpl")"
  TMP="$tpl.tools-new"
  # Reflink copy on the XFS data volume: instant, and leaves the live template
  # untouched until the final atomic rename.
  cp --reflink=auto "$tpl" "$TMP"
  MNT=$(mktemp -d)
  mount -o loop "$TMP" "$MNT"
  install -m 0755 "$CLAUDE_BIN" "$MNT/usr/local/bin/claude"
  install -m 0755 "$CODEX_BIN"  "$MNT/usr/local/bin/codex"
  # Pi's standalone bundle includes runtime assets beside the executable. Keep
  # the bundle together under /usr/local/lib and expose its CLI on the normal
  # PATH with a relative symlink.
  rm -rf "$MNT/usr/local/lib/pi"
  mkdir -p "$MNT/usr/local/lib/pi"
  tar -xzf "$PI_BUNDLE" -C "$MNT/usr/local/lib/pi" --strip-components=1 --no-same-owner
  [ -x "$MNT/usr/local/lib/pi/pi" ] \
    || { echo "pi bundle $PI_BUNDLE has no executable pi" >&2; exit 1; }
  ln -sfn ../lib/pi/pi "$MNT/usr/local/bin/pi"
  install -m 0755 "$HM_BIN"     "$MNT/usr/local/bin/hivemind"
  # agent-browser is a self-contained Rust CDP client, but like pi it is not just
  # a binary: it resolves its own skill content at <real-binary-dir>/../skill-data,
  # so the bundle has to stay together under /usr/local/lib with a relative
  # symlink on PATH. Copying only the executable into /usr/local/bin yields a CLI
  # whose every `skills` subcommand fails with "Skills directory not found" —
  # the kind of half-working you would only discover from inside a sandbox.
  #
  # The tarball carries SEVEN platform binaries (~87MB of its ~92MB). Keep the
  # one this box's guests actually run and drop the rest, along with the npm
  # shim bin/agent-browser.js (it wants node>=24 and buys nothing, because
  # /usr/local/bin/agent-browser points straight at the ELF) and scripts/, whose
  # postinstall we never run. ~92MB -> ~13MB.
  rm -rf "$MNT/usr/local/lib/agent-browser"
  mkdir -p "$MNT/usr/local/lib/agent-browser"
  tar -xzf "$AB_TGZ" -C "$MNT/usr/local/lib/agent-browser" --strip-components=1 --no-same-owner
  find "$MNT/usr/local/lib/agent-browser/bin" -type f ! -name "agent-browser-linux-$AB_ARCH" -delete
  rm -rf "$MNT/usr/local/lib/agent-browser/scripts"
  [ -f "$MNT/usr/local/lib/agent-browser/bin/agent-browser-linux-$AB_ARCH" ] \
    || { echo "agent-browser tarball $AB_TGZ has no linux-$AB_ARCH binary" >&2; exit 1; }
  # npm ships bin/* mode 0644 and relies on its postinstall to chmod them. We
  # never run that postinstall, so the exec bit is ours to set or the symlink
  # below points at something that cannot be executed.
  chmod 0755 "$MNT/usr/local/lib/agent-browser/bin/agent-browser-linux-$AB_ARCH"
  ln -sfn "../lib/agent-browser/bin/agent-browser-linux-$AB_ARCH" "$MNT/usr/local/bin/agent-browser"
  seed_agent_env "$MNT"
  install_agent_guidance "$MNT"
  install_agent_browser_skill "$MNT"
  install_gateway_key "$MNT"
  # Workload identity: the token unit + timer that keep
  # /var/run/secrets/hivemind/token fresh, so `hivemind start` federates with
  # no secret in the guest and nothing to paste.
  GUEST_MOTD_FILE="$GUEST_MOTD_FILE" "$GUEST_IDENTITY" "$MNT"
  # Stamp LAST, inside the copy, and only once everything above succeeded — so a
  # run that dies mid-patch leaves a template that still reads as stale and gets
  # redone, rather than one that claims tools it never received. `set -e` makes
  # that automatic: any failure above exits before this line.
  mkdir -p "$MNT/etc/sparkbox"
  printf '%s\n' "$WANT" > "$MNT/etc/sparkbox/tools-rev"
  umount "$MNT" && rmdir "$MNT"; MNT=""
  mv -f "$TMP" "$tpl"; TMP=""
  patched=$((patched + 1))
done
echo ">> done: $patched template(s) now ship claude $CLAUDE_VER + codex $CODEX_TAG + pi $PI_TAG + hivemind $HM_VER + agent-browser $AB_VER + identity rev $IDENTITY_REV"
