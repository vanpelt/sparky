#!/usr/bin/env bash
# Install the sandbox workload-identity payload into a mounted guest rootfs.
#
# Usage: install-guest-identity.sh <rootfs-mountpoint>
#
# This is the single source of truth for the guest side of OIDC federation, and
# it is deliberately callable against any mounted tree so both paths can use it:
#   - hack/build-rootfs.sh calls it when baking a fresh template;
#   - deploy/refresh-agent-tools.sh calls it to patch ALREADY-PUBLISHED
#     templates on a host, with no image rebuild — which is how a newly
#     provisioned box gets this without waiting on a ~65-minute CI run.
#
# Keep IDENTITY_REV in step with any change here: refresh-agent-tools.sh stamps
# it and re-patches templates whose stamp is behind.
set -euo pipefail

MNT=${1:?usage: install-guest-identity.sh <rootfs-mountpoint>}
[ -d "$MNT" ] || { echo "no such mountpoint: $MNT" >&2; exit 1; }

# Bump when the payload below changes so hosts re-patch their templates.
IDENTITY_REV=13

# The metadata port must match internal/metadata.DefaultPort.
META_PORT=8967

# Which account runs the agent in this guest? The token unit runs as root, but
# the daemon reading /var/run/secrets/hivemind/token runs as the login user — so
# the token has to be readable by them. Derive that user from the tree itself:
# build-rootfs.sh baked the gateway key into exactly one home's authorized_keys
# (see its sparkbox.login-user handling), so the non-root account that owns an
# authorized_keys IS the login user. Default root (legacy root-login templates).
SANDBOX_USER=root
while IFS=: read -r u _ _ _ _ home _; do
  [ "$u" != root ] && [ -n "$home" ] && [ -f "$MNT$home/.ssh/authorized_keys" ] \
    && { SANDBOX_USER=$u; break; }
done < "$MNT/etc/passwd"

mkdir -p "$MNT/usr/local/bin" "$MNT/usr/local/sbin"

# Fetch this sandbox's OIDC id token from the host metadata service and park it
# where hivemind's zero-config auth chain already looks. The host authenticates
# us by our network position — our own tap is the only way to reach its
# metadata endpoint as us — so no secret is baked into the image and nothing
# has to be injected per sandbox.
#
# HIVEMIND_OIDC_TOKEN_FILE defaults to /var/run/secrets/hivemind/token, and the
# daemon re-reads that file ~5 minutes before expiry. Keeping the path fresh is
# the whole integration: `hivemind start` federates with no env vars, no login,
# and nothing pasted.
sed -e "s/@@META_PORT@@/$META_PORT/g" -e "s/@@SANDBOX_USER@@/$SANDBOX_USER/g" \
    > "$MNT/usr/local/sbin/sparkbox-token" <<'EOF'
#!/bin/sh
# Refresh this sandbox's OIDC id token and identity snapshot.
set -eu
TOKEN_FILE=${HIVEMIND_OIDC_TOKEN_FILE:-/var/run/secrets/hivemind/token}
IDENTITY_FILE=/run/sparkbox/identity.json
# The account that reads the token (the login user; root on legacy templates).
SANDBOX_USER=@@SANDBOX_USER@@

# The metadata service listens on our default gateway: the host end of our own
# tap. We cannot reach any other sandbox's endpoint, and none can reach ours.
GW=$(ip -4 route show default | awk '{print $3; exit}')
[ -n "$GW" ] || { echo "sparkbox-token: no default gateway" >&2; exit 1; }
META="http://$GW:@@META_PORT@@"

mkdir -p "$(dirname "$TOKEN_FILE")" /run/sparkbox
# 0755 (not 0700) so the non-root SANDBOX_USER can traverse to the token; the
# token file itself stays 0600, owned by that user, so only they (and root) read.
chmod 0755 "$(dirname "$TOKEN_FILE")"

# Write via temp file + rename: the daemon re-reads this path on its own
# schedule, so it must never catch a half-written token.
TMP="$TOKEN_FILE.tmp"
if curl -fsS --max-time 10 "$META/token" -o "$TMP"; then
  chmod 0600 "$TMP"
  [ "$SANDBOX_USER" != root ] && id "$SANDBOX_USER" >/dev/null 2>&1 \
    && chown "$SANDBOX_USER" "$TMP"
  mv -f "$TMP" "$TOKEN_FILE"
else
  rm -f "$TMP"
  echo "sparkbox-token: could not fetch a token from $META" >&2
  exit 1
fi

# The decoded claims, so shells and tools can cheaply answer "who am I"
# without parsing a JWT. Fetched separately because it mints nothing: every
# /token response burns a single-use jti.
TMP="$IDENTITY_FILE.tmp"
if curl -fsS --max-time 10 "$META/identity" -o "$TMP"; then
  chmod 0644 "$TMP"
  mv -f "$TMP" "$IDENTITY_FILE"
  /usr/local/sbin/sparkbox-git-identity "$IDENTITY_FILE" || true
else
  rm -f "$TMP"
fi
EOF
chmod 0755 "$MNT/usr/local/sbin/sparkbox-token"

# Give git an author, so `git commit` in a fresh sandbox does not stop on
# "Please tell me who you are".
#
# System scope, NOT --global: /etc/gitconfig is the lowest precedence git reads,
# so a person who runs `git config --global user.email ...` still wins and we
# never overwrite an answer they gave. Rewritten between markers on every run
# rather than appended under a grep guard like the credential helper above,
# because unlike that helper this content is per-VM and can change under us — a
# handle rename or a GitHub link made after the box booted must be able to
# correct it.
#
# The address is GitHub's `<id>+<login>@users.noreply.github.com`. The account
# NUMBER is what makes it attribute: the legacy `<login>@users.noreply...` form
# is only linked for accounts created before 2017-07-18, so a modern account
# committing under it appears on github.com as nobody. When the host reports no
# number we still write that legacy form, because a commit that names the right
# person unattributed beats a commit that names nobody at all.
#
# When the host reports no GitHub login we write NOTHING but a comment. The
# handle is not usable as a substitute: handles and GitHub logins are separate
# namespaces here, so `<handle>@users.noreply.github.com` would hand a
# stranger's GitHub account the authorship of this person's commits.
cat > "$MNT/usr/local/sbin/sparkbox-git-identity" <<'EOF'
#!/bin/sh
# Write the [user] block in /etc/gitconfig from this sandbox's identity.
set -eu
IDENTITY_FILE=${1:-/run/sparkbox/identity.json}
# Overridable so the deploy tests can run this against a tree instead of the
# machine running them, the same way the token script takes its paths.
GITCONFIG=${SPARKBOX_GITCONFIG:-/etc/gitconfig}
BEGIN='# >>> sparkbox identity (managed) >>>'
END='# <<< sparkbox identity (managed) <<<'

[ -s "$IDENTITY_FILE" ] || exit 0

# Field extraction without a JSON parser: python3 is not a dependency this early
# path may take on, and both values have grammars too narrow to need one — a
# GitHub login is [A-Za-z0-9-] and an account number is digits. "github" cannot
# match inside "github_id" because the pattern demands the colon immediately
# after the name.
#
# [[:space:]]* after every colon is load-bearing, not defensive. The metadata
# service serves /identity through an encoder with SetIndent("", "  "), so the
# file on disk reads `"github": "vanpelt"` WITH a space — and patterns written
# for the compact form matched nothing, silently, taking every linked account
# down the "no GitHub account" path. Whitespace after a colon is JSON's to
# choose, so the reader has to tolerate it either way.
login=$(sed -n 's/.*"github":[[:space:]]*"\([A-Za-z0-9-]*\)".*/\1/p' "$IDENTITY_FILE" | head -1)
ghid=$(sed -n 's/.*"github_id":[[:space:]]*\([0-9][0-9]*\).*/\1/p' "$IDENTITY_FILE" | head -1)
owner=$(sed -n 's/.*"owner":[[:space:]]*"\([^"]*\)".*/\1/p' "$IDENTITY_FILE" | head -1)

if [ -n "$login" ] && [ -n "$ghid" ] && [ "$ghid" != 0 ]; then
  body="[user]
	name = $login
	email = $ghid+$login@users.noreply.github.com"
elif [ -n "$login" ]; then
  body="[user]
	name = $login
	email = $login@users.noreply.github.com"
else
  body="# No GitHub account is linked to ${owner:-this account}, and a Sparkbox
# handle is not a GitHub login, so no address here could be yours. Set one:
#   git config --global user.name  \"Your Name\"
#   git config --global user.email \"you@example.com\""
fi

# Rewrite in place: strip any previous block, append the current one. awk rather
# than sed -i so the whole file is rebuilt in one pass and a partial write can
# never leave a half-deleted marker behind; the temp-file-plus-rename is what
# makes that true even if we are killed mid-boot.
umask 022
tmp=$(mktemp "$GITCONFIG.sparkbox.XXXXXX") || exit 0
if [ -f "$GITCONFIG" ]; then
  awk -v b="$BEGIN" -v e="$END" '
    $0 == b { skip = 1; next }
    $0 == e { skip = 0; next }
    !skip   { print }
  ' "$GITCONFIG" > "$tmp" || { rm -f "$tmp"; exit 0; }
fi
{
  printf '%s\n' "$BEGIN"
  printf '%s\n' "$body"
  printf '%s\n' "$END"
} >> "$tmp"
chmod 0644 "$tmp"
mv -f "$tmp" "$GITCONFIG"
EOF
chmod 0755 "$MNT/usr/local/sbin/sparkbox-git-identity"

# A tiny in-guest control client. The metadata service authenticates the
# caller from its tap source address, so this carries no operator credential
# and can only change the sandbox from which the request originated.
sed -e "s/@@META_PORT@@/$META_PORT/g" > "$MNT/usr/local/bin/sparkbox" <<'EOF'
#!/bin/sh
set -eu
GW=$(ip -4 route show default | awk '{print $3; exit}')
[ -n "$GW" ] || { echo "sparkbox: no default gateway" >&2; exit 1; }
META="http://$GW:@@META_PORT@@"

# The checkout worker, named once so the `repos` verb and the capture survey
# cannot drift onto two different binaries. Overridable so the deploy tests can
# point it at a stub, the same way the other guest scripts take their roots; it
# is never derived from a request.
SPARKBOX_REPOS_BIN=${SPARKBOX_REPOS_BIN:-/usr/local/sbin/sparkbox-repos}

# _call METHOD URL — print the host's own sentence, on success AND on refusal.
#
# Deliberately NOT `curl -f`, which every verb below still uses: -f throws the
# response BODY away, and the body is the only place the host's explanation
# exists, so a refused `-f` verb prints curl's generic "The requested URL
# returned error: 409" and none of the reason. The two verbs that can end this
# session must never do that — "it was refused" and "it worked" have to be
# distinguishable from inside a box that is about to stop.
#
# Exit codes: 0 ok · 1 declined · 2 usage/invalid · 3 denied · 4 conflict, busy
# or rate-limited · 5 unsupported or disabled · 75 temporary or ambiguous.
_call() {
  # mktemp, not a predictable /tmp name: these verbs run under sudo on a box
  # whose /tmp is world-writable, and `curl -o` follows a symlink somebody else
  # left there. The other payload scripts take the same precaution.
  _d=$(mktemp -d) || { echo "sparkbox: no writable temporary directory" >&2; return 75; }
  _h=$_d/h; _b=$_d/b
  _code=$(curl -sS --max-time 20 -H 'Accept: text/plain' -D "$_h" -o "$_b" \
            -w '%{http_code}' -X "$1" "$2" 2>/dev/null) || _code=000
  SPARKBOX_TAG=$(sed -n 's/^[Ss]parkbox-[Tt]ag: *//p' "$_h" 2>/dev/null | tr -d '\r')
  SPARKBOX_SNAPSHOT=$(sed -n 's/^[Ss]parkbox-[Ss]napshot: *//p' "$_h" 2>/dev/null | tr -d '\r')
  SPARKBOX_PLAN=$(sed -n 's/^[Ss]parkbox-[Pp]lan: *//p' "$_h" 2>/dev/null | tr -d '\r')
  SPARKBOX_CTL=$(sed -n 's/^[Ss]parkbox-[Cc]tl: *//p' "$_h" 2>/dev/null | tr -d '\r')
  # A guest is never told its own domain, so every hint that names the gateway
  # is host-authored. This placeholder is what is left when no reply arrived.
  [ -n "$SPARKBOX_CTL" ] || SPARKBOX_CTL="ssh ctl@<gateway>"
  case "$_code" in
    2*)  cat "$_b"; rm -rf "$_d"; return 0 ;;
    000) rm -rf "$_d"
         # The one case this side genuinely cannot resolve: the reply was
         # written and lost, or never written at all. It claims nothing, and
         # its exit code is not success.
         echo "sparkbox: the gateway stopped answering before it confirmed. Either" >&2
         echo "nothing happened, or it is starting and this sandbox is about to pause." >&2
         echo "Check from outside the VM:" >&2
         echo "  $SPARKBOX_CTL snapshot ls" >&2
         return 75 ;;
    400) cat "$_b" >&2; rm -rf "$_d"; return 2 ;;
    403|404) cat "$_b" >&2; rm -rf "$_d"; return 3 ;;
    409|429) cat "$_b" >&2; rm -rf "$_d"; return 4 ;;
    501) cat "$_b" >&2; rm -rf "$_d"; return 5 ;;
    502|503) cat "$_b" >&2; rm -rf "$_d"; return 75 ;;
    *)   cat "$_b" >&2; rm -rf "$_d"; return 1 ;;
  esac
}

case "${1:-}" in
  pin)    exec curl -fsS --max-time 10 -X POST "$META/self/pin" ;;
  unpin)  exec curl -fsS --max-time 10 -X POST "$META/self/unpin" ;;
  status) exec curl -fsS --max-time 10 "$META/self" ;;
  pause)
    # The host answers BEFORE it pauses and waits for this process to have read
    # the answer, so the line below really does arrive. Without that the happy
    # path would print a curl transport error: a paused VM's kernel is frozen
    # mid-connection and the reply has nowhere to land.
    _call POST "$META/self/pause" || exit $?
    ;;
  snapshot)
    shift
    _yes=0; _busyok=0; _tag=""; _name=""
    for _a in "$@"; do
      case "$_a" in
        --yes|-y) _yes=1 ;;
        --allow-busy) _busyok=1 ;;
        -*) echo "usage: sparkbox snapshot [--yes] [--allow-busy] [TAG [NAME]]" >&2; exit 2 ;;
        *)  if   [ -z "$_tag"  ]; then _tag=$_a
            elif [ -z "$_name" ]; then _name=$_a
            else echo "usage: sparkbox snapshot [--yes] [--allow-busy] [TAG [NAME]]" >&2; exit 2; fi ;;
      esac
    done
    # The plan mutates nothing. Every refusal a user can act on lands here,
    # while this sandbox is still running and this session is still open.
    _call GET "$META/self/snapshot?tag=$_tag&name=$_name" || exit $?

    # The repository survey, and it belongs HERE rather than in the plan the
    # gateway just answered: the gateway cannot see inside this filesystem and
    # is never going to grow a way to read a guest's working trees. This is the
    # only place that can, and it is the last moment somebody is still sitting
    # in front of the box about to be frozen.
    #
    # Everything it prints is a statement, not an offer. A capture does not OWN
    # this sandbox — it is one somebody is working in, which is precisely why it
    # is worth capturing — so nothing here commits, stashes, resets or checks
    # out on their behalf. What it does is make sure that what is about to be
    # copied into every future fork is what they think it is.
    if [ -x "$SPARKBOX_REPOS_BIN" ]; then
      # `_survey=$(cmd); _srv=$?` would be wrong under `set -e`: the assignment
      # TAKES the command's status, so a survey exiting 3 kills this script
      # before the line that reads $? — silently, with the survey it just
      # captured still in the variable. || is what makes the status readable.
      _srv=0
      _survey=$("$SPARKBOX_REPOS_BIN" survey 2>/dev/null) || _srv=$?
      case "$_survey" in
        ""|"no repos are attached"*) ;;
        *) printf 'repos, as they would be captured:\n%s\n\n' "$_survey" ;;
      esac
      # Exit 3 is a git operation in flight. It is the one state that makes a
      # capture actively BROKEN rather than merely surprising: the index lock
      # and the rebase directory are copied byte-for-byte, so every fork
      # inherits a git that refuses to run, in a box whose owner never saw the
      # rebase. Everything else the survey found is printed and not refused.
      if [ "$_srv" = 3 ] && [ "$_busyok" -ne 1 ]; then
        echo "sparkbox: a git operation is in progress in one of the checkouts above." >&2
        echo "A template freezes that state byte-for-byte, so every fork of this one" >&2
        echo "would inherit a git that refuses to run. Finish or abort it first, or" >&2
        echo "pass --allow-busy if you meant to capture it mid-flight." >&2
        exit 3
      fi
    fi
    if [ "$_yes" -ne 1 ]; then
      if [ -r /dev/tty ] && [ -t 0 ]; then
        printf 'Capture and re-point `%s`? [y/N] ' "$SPARKBOX_TAG" > /dev/tty
        read -r _ans < /dev/tty || _ans=n
        case "$_ans" in
          y|Y|yes|YES) ;;
          *) echo "sparkbox: nothing was captured."; exit 1 ;;
        esac
      else
        # Refusing without a terminal is deliberate. The thing being warned
        # about is the destruction of the terminal displaying the warning, so
        # "there is nobody here to read it" is a reason not to proceed. It
        # prevents accidents, not attacks — an agent that means it passes --yes.
        echo "sparkbox: this ends your session and re-points a tag, so it wants a" >&2
        echo "terminal to confirm at. Re-run with --yes if you meant it:" >&2
        echo "  sparkbox snapshot $SPARKBOX_TAG --yes" >&2
        exit 2
      fi
    fi
    # The pause freezes dirty page cache into the MEMORY snapshot, but the
    # capture reads the BLOCK DEVICE — so an unflushed write is present when you
    # resume and absent from the template. One line, and it is not optional.
    printf 'flushing writes… '; sync; printf 'ok\n'
    # The tag, the name and the token are the PLAN's, not re-derived: a commit
    # that drifted into the next minute would capture under a name nobody was
    # shown, and the token is what refuses a plan the world moved out from under.
    _call POST "$META/self/snapshot?tag=$SPARKBOX_TAG&name=$SPARKBOX_SNAPSHOT&plan=$SPARKBOX_PLAN" \
      || exit $?
    ;;
  make-public)  exec curl -fsS --max-time 10 -X POST "$META/self/visibility/public" ;;
  make-private) exec curl -fsS --max-time 10 -X POST "$META/self/visibility/private" ;;
  set-port)
    case "${2:-}" in ''|*[!0-9]*) echo "sparkbox: port must be from 1 through 65535" >&2; exit 2 ;; esac
    [ "$2" -ge 1 ] && [ "$2" -le 65535 ] \
      || { echo "sparkbox: port must be from 1 through 65535" >&2; exit 2; }
    exec curl -fsS --max-time 10 -X POST "$META/self/port/$2"
    ;;
  repos)
    # Delegated rather than reimplemented: the clone layout rule (~/<name> for a
    # single attachment, ~/src/<owner>/<name> for several) has to be the SAME
    # rule that reports where a repo is as the one that decided where to put it,
    # or `sparkbox repos` will confidently name a directory nothing cloned into.
    #
    # Run it as root when we can, because the boot unit does and the two must
    # not behave differently: sparkbox-repos writes the status file under
    # /run/sparkbox and the login banner at /etc/motd, both root-owned, and it
    # drops back to the login user for the clone itself. Without this, the
    # banner that says "run `sparkbox repos`" is never cleared BY running it —
    # the clone succeeds and the banner keeps reporting the old failure at every
    # login, forever. The image gives the login user passwordless sudo, so this
    # costs nothing and asks for nothing; -n means a template that ever stops
    # doing so degrades to the unprivileged path instead of hanging on a
    # password prompt nobody is there to answer.
    SB=$SPARKBOX_REPOS_BIN
    if [ "$(id -u)" -ne 0 ] && command -v sudo >/dev/null 2>&1 && sudo -n true 2>/dev/null; then
      SB="sudo -n $SPARKBOX_REPOS_BIN"
    fi
    case "${2:-}" in
      ''|status) exec $SB status ;;
      survey)    exec $SB survey ;;
      sync)      exec $SB sync ;;
      *) echo "usage: sparkbox repos [survey|sync]" >&2; exit 2 ;;
    esac
    ;;
  update-tools)
    # Same escalation as `repos`, for the same reason and with the same -n
    # degradation: the installer writes /usr/local/bin, /usr/local/lib and
    # /var/lib/sparkbox, none of which the login user owns. Without root it
    # would download 150MB and then fail on the first install.
    SB=/usr/local/sbin/sparkbox-update-tools
    if [ "$(id -u)" -ne 0 ] && command -v sudo >/dev/null 2>&1 && sudo -n true 2>/dev/null; then
      SB="sudo -n /usr/local/sbin/sparkbox-update-tools"
    fi
    case "${2:-}" in
      '')        exec $SB ;;
      --check)   exec $SB --check ;;
      *) echo "usage: sparkbox update-tools [--check]" >&2; exit 2 ;;
    esac
    ;;
  *)
    echo "usage: sparkbox <pin|unpin|status|pause|snapshot [--yes] [--allow-busy] [TAG [NAME]]|make-public|make-private|set-port PORT|repos [survey|sync]|update-tools [--check]>" >&2
    exit 2
    ;;
esac
EOF
chmod 0755 "$MNT/usr/local/bin/sparkbox"

# A token that exists before anything wants to read it.
#
# The consumers are user units — hivemind's daemon lingers, so it starts with
# the machine — and a user unit CANNOT be ordered against a system one: they
# are run by different managers, and systemd silently drops an After= that
# crosses that line. So the token has to be there early on its own merits, and
# the readers have to check rather than be sequenced.
#
# Measured on a fresh CKS sandbox before this changed: boot 16:03:22, hivemind
# daemon up at 16:03:23.97, "Authentication required" at 16:03:24.44, token
# finally written at 16:03:33.51. Nine seconds late, and hivemind resolves its
# credential chain once at startup — so the daemon ran for the life of the box
# saying `hivemind login`, on a box whose token was sitting on disk the whole
# time. `hivemind stop && hivemind start` fixed it, which is exactly the manual
# step this integration exists to delete.
#
# await-token is the reader's half. It waits for the file rather than for the
# unit that writes it, which is the only question a user unit can ask, and it
# gives up rather than blocking forever: a daemon running unauthenticated is a
# degraded box, a daemon that never starts is a broken one.
cat > "$MNT/usr/local/bin/sparkbox-await-token" <<'EOF'
#!/bin/sh
# Block until this sandbox's OIDC token exists, up to a bounded wait.
# Exit 0 either way: this gates a daemon's start, it does not decide it.
set -u
TOKEN_FILE=${HIVEMIND_OIDC_TOKEN_FILE:-/var/run/secrets/hivemind/token}
WAIT=${SPARKBOX_TOKEN_WAIT:-60}
i=0
while [ "$i" -lt "$WAIT" ]; do
  [ -s "$TOKEN_FILE" ] && exit 0
  i=$((i + 1))
  sleep 1
done
echo "sparkbox-await-token: no token at $TOKEN_FILE after ${WAIT}s; starting anyway" >&2
exit 0
EOF
chmod 0755 "$MNT/usr/local/bin/sparkbox-await-token"

# ---- agent tool updates -----------------------------------------------------

# Pull this host's verified agent-CLI cache into a sandbox that already exists.
#
# refresh-agent-tools.sh can only patch TEMPLATES — a live rootfs is never
# touched, deliberately — so a VM created a month ago still runs whatever its
# template shipped with, and DISABLE_AUTOUPDATER=1 in /etc/environment stops each
# agent from fixing that for itself (one template, one set of versions, no
# mid-session surprises). This is the sanctioned way to move, and it is a PULL:
# the guest asks its own host for artifacts that host already downloaded and
# checksummed, over the tap it already trusts. Nothing here reaches the open
# internet, so it works unchanged on a tagged VM whose egress is filtered.
#
# Sibling of sparkbox-repos below, and the family resemblance is on purpose: the
# same curl/awk/ip vocabulary, the same non-`-f` fetch with a sentence per status
# code, the same US-separated flattener. Read the two together.
sed -e "s/@@META_PORT@@/$META_PORT/g" -e "s/@@SANDBOX_USER@@/$SANDBOX_USER/g" \
    > "$MNT/usr/local/sbin/sparkbox-update-tools" <<'EOF'
#!/bin/sh
# Install the agent CLIs this sandbox's host has cached.
set -eu

MODE=${1:-install}
case "$MODE" in
  install|--check) ;;
  *) echo "usage: sparkbox-update-tools [--check]" >&2; exit 2 ;;
esac

# The account the harness config belongs to (the login user; root on legacy
# templates).
SANDBOX_USER=@@SANDBOX_USER@@

# Overridable so the deploy tests can install into a tree instead of the machine
# running them, the same way sparkbox-git-identity takes its gitconfig. It moves
# paths and nothing else: this script is already root when it matters, and the
# escalation is the dispatcher's business rather than this file's.
ROOT=${SPARKBOX_TOOLS_ROOT:-}

# This sandbox's own record of what it has installed, and the only stamp this
# script ever writes.
#
# It is deliberately NOT /etc/sparkbox/tools-rev. That file is the HOST's
# decision variable: refresh-agent-tools.sh reads it back out of every template
# with debugfs to decide which ones to patch, and its identity= and agentenv=
# words name systemd units, /etc/environment keys and harness config that only a
# host with the template loop-mounted can install. A guest writing its own
# versions there would make the next refresh believe a template was current that
# it had never patched — the exact "claims tools it never received" failure that
# stamp was introduced to stop. We read it once, to learn what this VM booted
# with, and never write it.
STATE="$ROOT/var/lib/sparkbox/tools-rev"
TEMPLATE_STAMP="$ROOT/etc/sparkbox/tools-rev"

# No unverified installs, ever. This is the only check on bytes that are about
# to become every agent in this VM, so its absence is a refusal and not a
# warning.
command -v sha256sum >/dev/null 2>&1 || {
  echo "sparkbox-update-tools: no sha256sum in this sandbox; refusing to install unverified binaries" >&2
  exit 1
}

GW=$(ip -4 route show default | awk '{print $3; exit}')
[ -n "$GW" ] || { echo "sparkbox-update-tools: no default gateway" >&2; exit 1; }
META="http://$GW:@@META_PORT@@"

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT INT TERM HUP

# Seed our stamp from the template's on the first run, so a VM that has never
# updated reports the versions it was actually built with instead of "unknown"
# for everything. Only the five tool words are copied: identity= and agentenv=
# name payloads this script cannot install, and carrying them would let a later
# reader mistake this file for the host's.
if [ ! -f "$STATE" ] && [ -f "$TEMPLATE_STAMP" ]; then
  mkdir -p "$(dirname "$STATE")"
  if awk '{
        line = ""
        for (i = 1; i <= NF; i++) {
          n = index($i, "=")
          if (n == 0) continue
          k = substr($i, 1, n - 1)
          if (k == "claude" || k == "codex" || k == "pi" || k == "hivemind" || k == "agentbrowser")
            line = line (line == "" ? "" : " ") $i
        }
        print line
        exit
      }' "$TEMPLATE_STAMP" > "$STATE.new"; then
    chmod 0644 "$STATE.new"
    mv -f "$STATE.new" "$STATE"
  else
    rm -f "$STATE.new"
  fi
fi

stamp_get() {
  [ -f "$STATE" ] || return 0
  awk -v k="$1" '{
    for (i = 1; i <= NF; i++) {
      n = index($i, "=")
      if (n > 0 && substr($i, 1, n - 1) == k) { print substr($i, n + 1); exit }
    }
  }' "$STATE"
}

give_up() {
  echo "sparkbox-update-tools: $1" >&2
  exit 1
}

# Not `curl -f`: 501 is a real answer — this host serves no tool cache — and it
# deserves a different sentence from "the metadata service is down".
code=$(curl -sS --max-time 30 -o "$WORK/manifest.json" -w '%{http_code}' \
  "$META/tools/manifest" 2>/dev/null) || code=000
case "$code" in
  200) ;;
  501) give_up "this host does not serve an agent tool cache" ;;
  429) give_up "this sandbox is being rate-limited; try again in a minute" ;;
  000) give_up "could not reach the metadata service at $META" ;;
  *)   give_up "the metadata service answered $code for /tools/manifest" ;;
esac

# Fields are joined with ASCII US for the reason sparkbox-repos spells out: an
# empty field between two whitespace separators would silently shift every later
# field left by one, and here that means installing a tarball as if it were a
# bare binary. US cannot occur in a version, a path or a digest.
SEP=$(printf '\037')

# Flatten the manifest to one row per tool, with the same tr+awk pair-walk the
# repo worker uses and for the same reason (no jq, no python: the slim
# systemd-less fallback template has neither). It only recognises `"key":
# "value"`, which is why refresh-agent-tools.sh quotes every value including
# size, and it cannot survive a value containing a quote or a brace — none of
# these can, being versions, absolute paths and hex digests.
tr -d '\n\r' < "$WORK/manifest.json" | tr '{' '\n' | awk -F'"' '
  /"key"/ {
    name = ""; key = ""; version = ""; file = ""; sha = ""; size = ""
    kind = ""; bin = ""; dir = ""; exe = ""; link = ""; keep = ""; drop = ""
    for (i = 2; i + 2 <= NF; i += 2) {
      if ($(i + 1) !~ /^[ \t]*:[ \t]*$/) continue
      k = $i; v = $(i + 2)
      if (k == "name") name = v
      else if (k == "key") key = v
      else if (k == "version") version = v
      else if (k == "file") file = v
      else if (k == "sha256") sha = v
      else if (k == "size") size = v
      else if (k == "kind") kind = v
      else if (k == "bin") bin = v
      else if (k == "dir") dir = v
      else if (k == "exec") exe = v
      else if (k == "link") link = v
      else if (k == "keep_only") keep = v
      else if (k == "drop") drop = v
    }
    if (name == "" || key == "" || version == "" || sha == "" || bin == "") next
    printf "%s\037%s\037%s\037%s\037%s\037%s\037%s\037%s\037%s\037%s\037%s\037%s\037%s\n", \
      name, key, version, file, sha, size, kind, bin, dir, exe, link, keep, drop
  }
' > "$WORK/rows"
[ -s "$WORK/rows" ] || give_up "the host published no tools"

# ---- installing one row -----------------------------------------------------
# These read the row variables the loop below sets, the way the repo worker's
# helpers read its own. Each returns non-zero to mean "this tool is unchanged",
# so one bad artifact never takes the other four down with it.

fetch_artifact() {
  # The manifest comes from our own host, but it still names a path we are about
  # to write, so refuse a separator rather than trust the far side to have
  # checked. Same instinct as the credential helper's slug guard.
  case "$name" in ''|*/*) echo "sparkbox-update-tools: refusing tool name '$name'" >&2; return 1 ;; esac
  case "$file" in ''|*/*) echo "sparkbox-update-tools: refusing artifact name '$file'" >&2; return 1 ;; esac
  case "$bin"  in /*) ;; *) echo "sparkbox-update-tools: refusing relative install path '$bin'" >&2; return 1 ;; esac

  # 15 minutes: agent-browser's tarball is ~92MB and a tagged VM's tap may be
  # bandwidth-metered.
  if ! curl -fsS --max-time 900 "$META/tools/$name" -o "$WORK/$file.part"; then
    echo "sparkbox-update-tools: could not download $name $version from $META" >&2
    rm -f "$WORK/$file.part"
    return 1
  fi
  mv -f "$WORK/$file.part" "$WORK/$file"
  # Size first, and it earns its place by naming the failure instead of
  # disguising it. The host serves these bodies under a write deadline, over a
  # tap its own egress rules may be metering, so a body that simply stops
  # arriving is the expected failure here — and reported as a checksum mismatch
  # it reads like a corrupted or tampered artifact, which sends whoever is
  # holding it looking in entirely the wrong place.
  got=$(wc -c < "$WORK/$file" | tr -d ' ')
  if [ -n "$size" ] && [ "$got" != "$size" ]; then
    echo "sparkbox-update-tools: $name $version arrived as $got bytes, not the $size the host published; not installing" >&2
    rm -f "$WORK/$file"
    return 1
  fi
  # Verified BEFORE anything is installed, never after: a truncated or wrong
  # `claude` that has already replaced the working one is not recoverable from
  # inside the VM.
  if ! (cd "$WORK" && printf '%s  %s\n' "$sha" "$file" | sha256sum -c - >/dev/null 2>&1); then
    echo "sparkbox-update-tools: $name $version failed its checksum; not installing" >&2
    rm -f "$WORK/$file"
    return 1
  fi
}

install_binary() {
  dest="$ROOT$bin"
  mkdir -p "$(dirname "$dest")"
  # Rename into place. Writing over the file directly fails with ETXTBSY the
  # moment anything in this VM is running claude — which, in a sandbox that
  # exists to run agents, is the ordinary case and not the unlucky one.
  install -m 0755 "$WORK/$file" "$dest.sparkbox-new"
  mv -f "$dest.sparkbox-new" "$dest"
}

install_bundle() {
  case "$dir" in /*) ;; *) echo "sparkbox-update-tools: $name has no bundle directory" >&2; return 1 ;; esac
  [ -n "$exe" ] && [ -n "$link" ] || { echo "sparkbox-update-tools: $name names no executable" >&2; return 1; }
  d="$ROOT$dir"
  dest="$ROOT$bin"

  rm -rf "$d.sparkbox-new"
  mkdir -p "$d.sparkbox-new"
  # --no-same-owner because the tarball's uids are the publisher's and we are
  # root; and NO -P, so tar keeps stripping a leading / and refusing ../ members
  # and an archive that ever carried one lands inside this directory instead of
  # over the guest's filesystem.
  tar -xzf "$WORK/$file" -C "$d.sparkbox-new" --strip-components=1 --no-same-owner

  # The ~92MB -> ~13MB prune, and in here it is not housekeeping: these bytes
  # land in this VM's own 25 GiB ceiling and against its owner's disk pool, once
  # per sandbox, unlike the template blocks every fork shares.
  if [ -n "$keep" ]; then
    case "$keep" in
      */*) keepdir=${keep%/*}; keepname=${keep##*/} ;;
      *)   keepdir=.;          keepname=$keep ;;
    esac
    find "$d.sparkbox-new/$keepdir" -type f ! -name "$keepname" -delete
  fi
  # Unquoted on purpose: drop is a space-separated list from our own host.
  for unwanted in $drop; do
    rm -rf "$d.sparkbox-new/$unwanted"
  done

  if [ ! -f "$d.sparkbox-new/$exe" ]; then
    echo "sparkbox-update-tools: $name $version ships no $exe" >&2
    rm -rf "$d.sparkbox-new"
    return 1
  fi
  # npm ships bin/* mode 0644 and chmods them from a postinstall we never run,
  # so the exec bit is ours to set or the symlink below points at something that
  # cannot be executed.
  chmod 0755 "$d.sparkbox-new/$exe"

  # Two renames rather than a delete-then-extract: the window in which $dir does
  # not exist is one rename wide, and anything still running out of the old tree
  # holds its inodes until it exits.
  rm -rf "$d.sparkbox-old"
  if [ -e "$d" ]; then mv -f "$d" "$d.sparkbox-old"; fi
  mv -f "$d.sparkbox-new" "$d"
  rm -rf "$d.sparkbox-old"

  mkdir -p "$(dirname "$dest")"
  # The manifest's own RELATIVE target, verbatim. Deriving it here is what breaks
  # these tools: pi and agent-browser resolve their runtime assets against the
  # real binary's directory (agent-browser looks for ../skill-data), so PATH has
  # to hold a link INTO the bundle rather than a copy of the executable out of
  # it — a copy yields a CLI whose every `skills` subcommand fails.
  ln -sfn "$link" "$dest"
  refresh_agent_skill "$d"
}

# Re-point the harnesses at a bundle's own skill file when the template already
# wired one up. agent-browser's SKILL.md is a deliberate discovery stub whose
# body is little more than "run `agent-browser skills get core`", so it has to
# match the CLI now installed. Only ever a REFRESH: creating that wiring from in
# here would be this script inventing a layout the template owns
# (refresh-agent-tools.sh's install_agent_browser_skill decides it).
refresh_agent_skill() {
  skill="$1/skills/agent-browser/SKILL.md"
  [ -f "$skill" ] || return 0
  home=$(awk -F: -v u="$SANDBOX_USER" '$1 == u {print $6; exit}' "$ROOT/etc/passwd")
  [ -n "$home" ] || return 0
  dst="$ROOT$home/.agents/skills/agent-browser"
  [ -d "$dst" ] || return 0
  if install -m 0644 "$skill" "$dst/SKILL.md" 2>/dev/null; then
    chown "$SANDBOX_USER" "$dst/SKILL.md" 2>/dev/null || true
  else
    echo "sparkbox-update-tools: could not refresh $dst/SKILL.md" >&2
  fi
}

install_tool() {
  fetch_artifact || return 1
  case "$kind" in
    binary) install_binary ;;
    bundle) install_bundle ;;
    *) echo "sparkbox-update-tools: $name has unknown kind '$kind'" >&2; return 1 ;;
  esac
}

# ---- walk the manifest ------------------------------------------------------
behind=0
failed=0
: > "$WORK/stamp"
printf '%-16s %-18s %-18s %s\n' TOOL INSTALLED AVAILABLE STATUS
while IFS="$SEP" read -r name key version file sha size kind bin dir exe link keep drop; do
  have=$(stamp_get "$key")
  record=$have
  if [ "$have" = "$version" ]; then
    status=current
  elif [ "$MODE" = --check ]; then
    status=behind
    behind=$((behind + 1))
  # stdin is the row list this loop is reading; hand every child /dev/null so
  # nothing downstream can eat a tool we have not installed yet.
  elif install_tool </dev/null; then
    status=updated
    record=$version
  else
    status=failed
    failed=$((failed + 1))
  fi
  printf '%-16s %-18s %-18s %s\n' "$name" "${have:-unknown}" "$version" "$status"
  if [ -n "$record" ]; then
    printf '%s=%s\n' "$key" "$record" >> "$WORK/stamp"
  fi
done < "$WORK/rows"

if [ "$MODE" = --check ]; then
  [ "$behind" -eq 0 ] || exit 1
  exit 0
fi

# Record only what is actually on this disk: a tool that failed keeps whatever
# word it had, and one we have never seen installed gets no word at all. Temp
# file plus rename, because the next run reads this to decide what to download.
mkdir -p "$(dirname "$STATE")"
awk '{ printf "%s%s", (NR > 1 ? " " : ""), $0 } END { printf "\n" }' "$WORK/stamp" > "$STATE.new"
chmod 0644 "$STATE.new"
mv -f "$STATE.new" "$STATE"

[ "$failed" -eq 0 ] || exit 1
EOF
chmod 0755 "$MNT/usr/local/sbin/sparkbox-update-tools"

# ---- GitHub repositories ----------------------------------------------------

# git's credential helper, riding the same tap the OIDC token arrives on.
#
# The gateway mints a GitHub App installation token scoped to the ONE repository
# git is talking to, good for an hour, from its own ledger of who this sandbox
# belongs to and what that owner attached. Nothing is stored guest-side: no
# token in ~/.git-credentials, none in a remote URL, none in /etc/environment —
# so none of it rides into a snapshot, a fork or an archived rootfs. The warm
# checkout does, which is the half worth keeping.
#
# Silence is the contract on every failure path, and it is the reason this file
# has no diagnostics in it. A credential helper that exits nonzero, or writes to
# stderr, makes git fall through to prompting on the terminal — and the boot-time
# clone has no terminal, so it blocks until something kills it. Printing nothing
# lets git carry on to its own error, which is the one a human can act on.
sed -e "s/@@META_PORT@@/$META_PORT/g" \
    > "$MNT/usr/local/bin/sparkbox-git-credential" <<'EOF'
#!/bin/sh
set -eu

# `store` and `erase` are the other verbs git may send. There is nothing to
# store and nothing to forget — the token is minted per request and never
# written down — so accept them and say nothing.
[ "${1:-}" = get ] || exit 0

proto=; host=; path=
# git speaks key=value lines, terminated by a blank line or EOF.
while IFS= read -r line; do
  [ -n "$line" ] || break
  case "$line" in
    protocol=*) proto=${line#protocol=} ;;
    host=*)     host=${line#host=} ;;
    path=*)     path=${line#path=} ;;
  esac
done

[ "$proto" = https ] || exit 0
[ "$host" = github.com ] || exit 0

# `path` is what credential.useHttpPath buys: without it git sends only the
# host, and the best this helper could then ask for is a token covering every
# repository the sandbox is attached to. With it we ask for exactly the one
# repository this fetch touches. A clone URL usually carries the .git suffix;
# the gateway's ledger stores the slug without it.
path=${path%.git}
case "$path" in
  */*/*|/*|*/) exit 0 ;;
  */*) ;;
  *) exit 0 ;;
esac
# This is the one place guest-controlled text becomes a query string. Refusing
# everything outside this set is what makes it safe there without escaping.
case "$path" in
  *[!A-Za-z0-9._/-]*) exit 0 ;;
esac

GW=$(ip -4 route show default | awk '{print $3; exit}') || exit 0
[ -n "$GW" ] || exit 0

body=$(curl -fsS --max-time 10 \
  "http://$GW:@@META_PORT@@/github/credential?slug=$path" 2>/dev/null) || exit 0
user=$(printf '%s' "$body" | sed -n 's/.*"username"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p')
pass=$(printf '%s' "$body" | sed -n 's/.*"password"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p')
[ -n "$pass" ] || exit 0

printf 'username=%s\npassword=%s\n' "${user:-x-access-token}" "$pass"
EOF
chmod 0755 "$MNT/usr/local/bin/sparkbox-git-credential"

# `gh`, on the same credential the clone rides.
#
# The GitHub CLI does not speak git's credential-helper protocol — it reads a
# token out of the environment — so the helper above does nothing for it, and a
# sandbox with a warm checkout of a private repository had a `gh` that answered
# "You are not logged into any GitHub hosts". Wrapping it is the whole fix: mint
# the same per-repository, one-hour token the clone uses, hand it to `gh` for the
# length of one command, and let it die with the process. Nothing is written to
# ~/.config/gh, so nothing rides into a snapshot, a fork or an archived rootfs —
# the same property the credential helper has, for the same reason.
#
# /usr/local/bin precedes /usr/bin in every PATH the image sets, so this shadows
# the real binary without moving it. It execs the absolute path, never `gh`, or
# it would find itself.
#
# WHICH repository: the one the user is standing in. `gh` is repository-scoped
# in the same way git is — `gh pr create` means "here" — so the remote of the
# current checkout is both the right answer and the narrowest one. Outside a
# checkout there is no repository to scope to and no token is set; `gh auth
# status` then says what it always said, which is the honest answer rather than
# a token for whatever happened to be attached first.
sed -e "s/@@META_PORT@@/$META_PORT/g" \
    > "$MNT/usr/local/bin/gh" <<'EOF'
#!/bin/sh
# Wrapper: see install-guest-identity.sh. Falls through to the real gh always.
GH_REAL=/usr/bin/gh
[ -x "$GH_REAL" ] || GH_REAL=/usr/local/share/sparkbox/gh
[ -x "$GH_REAL" ] || { echo "gh is not installed in this sandbox" >&2; exit 127; }

# An explicit token from the environment always wins: somebody who exported
# GH_TOKEN meant it, and silently overriding it with ours would be the same
# class of surprise this wrapper exists to remove.
if [ -n "${GH_TOKEN:-}" ] || [ -n "${GITHUB_TOKEN:-}" ]; then
  exec "$GH_REAL" "$@"
fi

# Every failure below falls through to an unauthenticated gh. This wrapper must
# never be the reason a command does not run: `gh --version`, `gh --help` and
# `gh auth login` all have to work in a directory with no git in it at all.
slug=$(git config --get remote.origin.url 2>/dev/null) || slug=
case "$slug" in
  https://github.com/*) slug=${slug#https://github.com/} ;;
  git@github.com:*)     slug=${slug#git@github.com:} ;;
  ssh://git@github.com/*) slug=${slug#ssh://git@github.com/} ;;
  *) slug= ;;
esac
slug=${slug%.git}
slug=${slug%/}
# Exactly owner/name, and only the characters github.com issues. Same guard as
# the credential helper: this is the one place guest-controlled text becomes a
# query string, and refusing everything outside the set is what makes it safe
# there without escaping.
case "$slug" in
  */*/*|/*) slug= ;;
  */*) ;;
  *) slug= ;;
esac
case "$slug" in
  *[!A-Za-z0-9._/-]*) slug= ;;
esac
[ -n "$slug" ] || exec "$GH_REAL" "$@"

GW=$(ip -4 route show default | awk '{print $3; exit}' 2>/dev/null) || GW=
[ -n "$GW" ] || exec "$GH_REAL" "$@"

body=$(curl -fsS --max-time 10 \
  "http://$GW:@@META_PORT@@/github/credential?slug=$slug" 2>/dev/null) || exec "$GH_REAL" "$@"
tok=$(printf '%s' "$body" | sed -n 's/.*"password"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p')
[ -n "$tok" ] || exec "$GH_REAL" "$@"

# GH_TOKEN and not `gh auth login --with-token`: the env var is scoped to this
# one process and leaves no file behind, which is the entire point.
GH_TOKEN=$tok exec "$GH_REAL" "$@"
EOF
chmod 0755 "$MNT/usr/local/bin/gh"

# Point git at the helper, system scope, as literal ini text.
#
# Not `git config --system`: this runs on the HOST against a mounted tree, so it
# would edit the host's own /etc/gitconfig — and the CKS controller image, which
# calls this installer, has no git binary at all. Appended under a grep guard
# and never rewritten: the guest image already put init.defaultBranch in this
# file, and truncating it would be a silent regression.
#
# The helper value must stay an ABSOLUTE path; a bare word makes git exec
# `git credential-<word>` instead.
mkdir -p "$MNT/etc"
grep -qs 'sparkbox-git-credential' "$MNT/etc/gitconfig" \
  || cat >> "$MNT/etc/gitconfig" <<'EOF'
[credential "https://github.com"]
	helper = /usr/local/bin/sparkbox-git-credential
	useHttpPath = true
EOF

# Preserve the baked login banner so the clone worker can rewrite /etc/motd as
# (banner + status) without the two ever accumulating. Captured once and only
# once: re-patching an already-patched template must not snapshot a banner that
# already carries a status line.
mkdir -p "$MNT/etc/sparkbox"
if [ ! -f "$MNT/etc/sparkbox/motd.base" ]; then
  if [ -f "$MNT/etc/motd" ]; then
    cp "$MNT/etc/motd" "$MNT/etc/sparkbox/motd.base"
  else
    : > "$MNT/etc/sparkbox/motd.base"
  fi
fi

# The clone worker. Three callers, all of them running the same reconciliation:
# sparkbox-repos.service at boot, `sparkbox repos sync` by hand, and the gateway
# when an owner retags a live box or attaches a repository to a tag it already
# carries (see internal/envsync/repos.go, which restarts the unit).
#
# That last caller is why this must stay idempotent and must never be destructive.
# It runs against a filesystem somebody is working in, so a repository already
# checked out is reported present and left exactly alone — the reconciliation
# only ever ADDS what is missing. Detaching removes nothing from the disk either.
# A clone that takes minutes costs the person who triggered it nothing, because
# the gateway returns as soon as the unit accepts the job and the work happens
# out here on the guest's own clock.
sed -e "s/@@META_PORT@@/$META_PORT/g" -e "s/@@SANDBOX_USER@@/$SANDBOX_USER/g" \
    > "$MNT/usr/local/sbin/sparkbox-repos" <<'EOF'
#!/bin/sh
# Reconcile this sandbox's checkouts with the repo manifest its gateway holds.
set -eu

MODE=${1:-sync}
case "$MODE" in
  sync|status|survey) ;;
  *) echo "usage: sparkbox-repos [status|survey|sync]" >&2; exit 2 ;;
esac

# The account the checkouts belong to (the login user; root on legacy templates).
SANDBOX_USER=@@SANDBOX_USER@@

# R and CMDLINE are overridable so the deploy tests can drive this against a
# tree instead of the machine running them, the same way sparkbox-identity-reset
# and sparkbox-update-tools take theirs. Nothing else reads them.
R=${SPARKBOX_REPOS_ROOT:-}
CMDLINE=${SPARKBOX_CMDLINE:-/proc/cmdline}
STATUS_FILE="$R/run/sparkbox/repos.status"
LOG_FILE="$R/run/sparkbox/repos.log"
MOTD_BASE="$R/etc/sparkbox/motd.base"

# A sync that cannot run is a warning, never a failed boot. This unit is ordered
# into boot beside sshd, and exiting nonzero would mark the whole machine
# degraded over a checkout that will still be missing — and still fixable — the
# next time anybody looks. `status` is a question a person asked out loud, so it
# is allowed to answer with a failure.
give_up() {
  echo "sparkbox-repos: $1" >&2
  if [ "$MODE" = sync ]; then exit 0; fi
  exit 1
}

GW=$(ip -4 route show default | awk '{print $3; exit}')
[ -n "$GW" ] || give_up "no default gateway"
META="http://$GW:@@META_PORT@@"

HOME_DIR=$(awk -F: -v u="$SANDBOX_USER" '$1 == u {print $6; exit}' "$R/etc/passwd")
[ -n "$HOME_DIR" ] || give_up "no home directory for $SANDBOX_USER in /etc/passwd"
# Rooted for the same reason everything else here is: /etc/passwd records the
# path a guest sees, and a test driving this against a tree needs the checkouts
# inside that tree. Empty R leaves it exactly as passwd spelled it.
HOME_DIR="$R$HOME_DIR"

# Checkouts belong to whoever will edit them, and this unit runs as root.
# Dropping privilege before `git clone` is not tidiness: git refuses to operate
# in a tree owned by somebody else, so a root-owned checkout sitting in a user's
# home is worse than no checkout at all — it is one that every later git command
# in that directory rejects for dubious ownership. If privilege cannot be
# dropped we clone nothing rather than clone it wrong.
#
# Skipped entirely when R is set, and that is not a loosening: with a root
# override this is not the machine /etc/passwd describes, the accounts in that
# tree are not this kernel's accounts, and there is nobody to drop TO — `id
# sparky` fails and the whole run gives up. The checkouts under a tree belong to
# whoever is driving it.
RUNAS=
if [ -z "$R" ] && [ "$(id -u)" = 0 ] && [ "$SANDBOX_USER" != root ]; then
  if ! id "$SANDBOX_USER" >/dev/null 2>&1; then
    give_up "no such user: $SANDBOX_USER"
  elif command -v runuser >/dev/null 2>&1; then
    RUNAS="runuser -u $SANDBOX_USER --"
  elif command -v sudo >/dev/null 2>&1; then
    RUNAS="sudo -n -u $SANDBOX_USER --"
  else
    give_up "cannot drop privilege to $SANDBOX_USER (no runuser, no sudo)"
  fi
fi

# May this run move a checkout it did not make?
#
# sparkbox_fresh=1 is written by the host on the first boot of a rootfs it has
# just copied from a template (internal/vmm/firecracker, Driver.Create) and on
# no other boot. That is the single moment a branch switch is safe: the disk
# came out of somebody's snapshot, nobody has logged in, and there is no work in
# flight to lose. Every other run — a boot after a resume, `sparkbox repos sync`
# by hand, the gateway nudging a box after a retag — happens against a
# filesystem somebody is working in, where the manifest naming a branch is not a
# reason to take them off theirs.
#
# A host that predates the marker emits nothing and every guest stays in refresh
# mode, which is the correct degradation: a fork keeps the branch it inherited
# and says so.
ADOPT=0
if [ "$MODE" = sync ]; then
  for tok in $(cat "$CMDLINE" 2>/dev/null); do
    case "$tok" in sparkbox_fresh=1) ADOPT=1 ;; esac
  done
fi

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT INT TERM HUP
: > "$WORK/report"
: > "$WORK/log"

# Not `curl -f`: 501 is a real answer — this fleet has no GitHub App configured
# — and it deserves a different sentence from "the metadata service is down".
code=$(curl -sS --max-time 15 -o "$WORK/manifest.json" -w '%{http_code}' \
  "$META/repos" 2>>"$WORK/log") || code=000
case "$code" in
  200) ;;
  501) give_up "repo attachment is not enabled on this host" ;;
  000) give_up "could not reach the metadata service at $META" ;;
  *)   give_up "the metadata service answered $code for /repos" ;;
esac

# Fields are joined with ASCII US, not a tab, and the reason is a trap rather
# than a preference: `read` collapses RUNS of IFS *whitespace*, so an
# attachment with neither a ref nor a path would hand two adjacent tabs to
# `read` and every field after them would silently shift left by one — a repo
# quietly cloning to a directory named after its access mode. A non-whitespace
# separator delimits every field, empty ones included, and US cannot occur in a
# slug, a ref or a path.
SEP=$(printf '\037')

# Flatten the manifest to one row per repo.
#
# Not jq and not python. build-rootfs.sh keeps a slim, systemd-less fallback
# template alive and the whole guest payload holds to curl/awk/ip for that
# reason. This is safe without a real JSON parser only because every value in
# the manifest is a constrained string — the gateway refuses a slug, ref or path
# containing a quote, a backslash or a brace — so splitting on '"' can never
# land inside a value. If that grammar ever loosens, this has to become a parser
# rather than a slightly cleverer awk.
tr -d '\n\r' < "$WORK/manifest.json" | tr '{' '\n' | awk -F'"' '
  /"slug"/ {
    host = ""; slug = ""; ref = ""; path = ""; access = ""
    for (i = 2; i + 2 <= NF; i += 2) {
      if ($(i + 1) !~ /^[ \t]*:[ \t]*$/) continue
      key = $i; val = $(i + 2)
      if (key == "host") host = val
      else if (key == "slug") slug = val
      else if (key == "ref") ref = val
      else if (key == "path") path = val
      else if (key == "access") access = val
    }
    if (slug == "") next
    if (host == "") host = "github.com"
    if (access == "") access = "read"
    printf "%s\037%s\037%s\037%s\037%s\n", host, slug, ref, path, access
  }
' > "$WORK/rows"
count=$(awk 'END {print NR}' "$WORK/rows")

report_add() {
  printf "%s${SEP}%s${SEP}%s${SEP}%s${SEP}%s\n" "$1" "$2" "$3" "$4" "$5" >> "$WORK/report"
}

# The one line the login banner gets: counts, and a pointer. A banner is read in
# the second before somebody starts typing, and the list is one command away.
status_line() {
  inflight=${1:-}
  if [ -n "$inflight" ]; then
    printf 'repos: %s %s…' "${2:-cloning}" "$inflight"
    return 0
  fi
  # The per-repository words collapse to four counts. A banner is read in the
  # second before somebody starts typing and cannot carry the whole vocabulary;
  # the word that says what is actually wrong lives one command away, in the
  # table below.
  ready=0; stale=0; pending=0; failed=0
  while IFS="$SEP" read -r state rest; do
    case "$state" in
      ready)  ready=$((ready + 1)) ;;
      stale)  stale=$((stale + 1)) ;;
      failed) failed=$((failed + 1)) ;;
      *)      pending=$((pending + 1)) ;;
    esac
  done < "$WORK/report"
  out=""
  if [ "$ready" -gt 0 ];   then out="$ready ready"; fi
  if [ "$stale" -gt 0 ];   then out="${out:+$out, }$stale need a look"; fi
  if [ "$pending" -gt 0 ]; then out="${out:+$out, }$pending not cloned"; fi
  if [ "$failed" -gt 0 ];  then out="${out:+$out, }$failed failed"; fi
  if [ -z "$out" ]; then out="none attached"; fi
  if [ "$failed" -gt 0 ] || [ "$pending" -gt 0 ] || [ "$stale" -gt 0 ]; then
    out="$out — run \`sparkbox repos\`"
  fi
  printf 'repos: %s' "$out"
}

# /etc/motd and not /etc/update-motd.d: the guest image empties that directory,
# and whether pam_motd's dynamic half is wired up on a given template is not
# something this tree tests or controls — whereas the static banner demonstrably
# prints today. The banner the image baked is kept verbatim in $MOTD_BASE by
# install-guest-identity.sh, so this rewrite is a pure function of (baked banner,
# current status) and re-running it can never accumulate lines.
publish_motd() {
  [ -f "$MOTD_BASE" ] || return 0
  # 2>/dev/null FIRST, then the redirection that can fail: redirections are
  # applied left to right, so `> file 2>/dev/null` still reports "Permission
  # denied" on the stderr it has not replaced yet. Verified, and the reason an
  # unprivileged run used to spray raw shell errors over its own output.
  if {
        cat "$MOTD_BASE"
        if [ -f "$STATUS_FILE" ]; then printf '\n'; cat "$STATUS_FILE"; fi
      } 2>/dev/null > "$R/etc/.motd.sparkbox"; then
    mv -f "$R/etc/.motd.sparkbox" "$R/etc/motd" 2>/dev/null || rm -f "$R/etc/.motd.sparkbox"
  fi
}

publish() {
  mkdir -p "$R/run/sparkbox" 2>/dev/null || true
  # Same redirection-order rule as publish_motd above.
  if printf '  %s\n' "$1" 2>/dev/null > "$STATUS_FILE.new"; then
    chmod 0644 "$STATUS_FILE.new" 2>/dev/null || true
    mv -f "$STATUS_FILE.new" "$STATUS_FILE" 2>/dev/null || rm -f "$STATUS_FILE.new"
  fi
  publish_motd
}

# Put the banner back exactly as the image baked it. Also the repair path for
# the boot after the last attachment was detached.
clear_status() {
  rm -f "$STATUS_FILE" 2>/dev/null || true
  publish_motd
}

# git inside a checkout, as the person who owns it: no terminal to block on, no
# stdin to eat a manifest row, and stderr in the log the report points at.
gitq() {
  _gd=$1; shift
  $RUNAS env GIT_TERMINAL_PROMPT=0 git -C "$_gd" "$@" </dev/null 2>>"$WORK/log"
}

# What happens to a checkout that is already there.
#
# THE RULE, and it is not a preference to be weighed against convenience: this
# function may FETCH, it may FAST-FORWARD a clean tree, and it may SAY
# something. It may do nothing else. It runs as root, unattended, on a
# filesystem somebody is working in, fired by events they did not cause — a
# teammate's retag, a boot after the reaper paused them — with no terminal to
# ask in and no way to be undone. `git reset --hard`, `git clean`, `git checkout
# -f`, `git rebase` and any merge that is not a fast-forward are all wrong here
# whatever the manifest says.
#
# `git pull` is on that list too, less obviously: it merges or rebases depending
# on config the user set, so it is a fast-forward right up until somebody sets
# pull.rebase and it silently is not.
#
# The third act is the feature, not the fallback. A fork that comes up with an
# inherited dirty tree and a line saying so is a good outcome; one that comes up
# clean because something threw the dirt away is an incident.
refresh_checkout() {
  _dest=$1; _slug=$2; _access=$3; _want=$4
  _dirty=; _branch=; _switched=; _gitdir=

  # Not a git worktree at all — an empty directory somebody made, a tarball they
  # unpacked, a checkout of something else. Left exactly as it is, which is what
  # this worker has always promised about an occupied path.
  if ! gitq "$_dest" rev-parse --git-dir >/dev/null; then
    report_add ready "$_slug" "$_access" "$_dest" present
    return 0
  fi
  _gitdir=$(gitq "$_dest" rev-parse --absolute-git-dir) || _gitdir="$_dest/.git"

  # An operation left in flight. Reported and never touched: aborting one is a
  # decision with a wrong answer, and a fetch during it is noise. This is the
  # state a capture freezes into an image when it pauses a box mid-rebase — the
  # lock and the rebase directory are copied byte-for-byte — so every fork of
  # that template inherits a git that refuses to run, and this is the line that
  # says why.
  for _m in rebase-merge rebase-apply MERGE_HEAD CHERRY_PICK_HEAD REVERT_HEAD BISECT_LOG; do
    [ -e "$_gitdir/$_m" ] || continue
    case "$_m" in
      rebase-merge|rebase-apply) _op="a rebase";      _fix="git rebase --abort" ;;
      MERGE_HEAD)                _op="a merge";       _fix="git merge --abort" ;;
      CHERRY_PICK_HEAD)          _op="a cherry-pick"; _fix="git cherry-pick --abort" ;;
      REVERT_HEAD)               _op="a revert";      _fix="git revert --abort" ;;
      *)                         _op="a bisect";      _fix="git bisect reset" ;;
    esac
    report_add failed "$_slug" "$_access" "$_dest" "$_op is in progress — $_fix"
    return 0
  done

  # Untracked files count as dirt on purpose. Telling "an untracked file the
  # merge would clobber" from "one it would not" is git's job and git only does
  # it inside the operation; one rule instead of two, erring towards not
  # touching anything.
  #
  # Dirt is a MODIFIER here rather than a state of its own, and it is only worth
  # reporting when it stopped something. Nagging about a scratch file in a
  # checkout that was already current, on every boot, is how a status line stops
  # being read.
  if [ -n "$(gitq "$_dest" status --porcelain || true)" ]; then _dirty=1; fi
  _branch=$(gitq "$_dest" symbolic-ref --short -q HEAD) || _branch=

  # `status` and `survey` ask what is HERE and expect an answer at once, so they
  # stay off the network entirely. The counts they give are therefore against
  # the remote-tracking refs this checkout already has — what the last fetch
  # knew — which is exactly the right basis for the question `survey` is asked:
  # what would a capture of this disk freeze into a template.
  if [ "$MODE" != sync ]; then
    if [ -z "$_branch" ]; then
      _note="detached HEAD"
    else
      _note="on $_branch"
      if gitq "$_dest" rev-parse --verify --quiet "refs/remotes/origin/$_branch" >/dev/null; then
        _n=$(gitq "$_dest" rev-list --count "origin/$_branch..HEAD" || echo 0)
        if [ "$_n" != 0 ]; then _note="$_note, $_n not pushed"; fi
      else
        _note="$_note, no remote branch"
      fi
    fi
    if [ -n "$_dirty" ]; then _note="$_note, uncommitted changes"; fi
    report_add ready "$_slug" "$_access" "$_dest" "$_note"
    return 0
  fi

  publish "$(status_line "$_slug" updating)"
  if ! gitq "$_dest" fetch --quiet origin >>"$WORK/log"; then
    report_add stale "$_slug" "$_access" "$_dest" "could not reach the remote, see $LOG_FILE"
    return 0
  fi

  if [ -z "$_branch" ]; then
    report_add stale "$_slug" "$_access" "$_dest" "detached HEAD, nothing to fast-forward"
    return 0
  fi

  # An attachment with no ref pins nothing: it decides where a CLONE starts and
  # says nothing about where a checkout that already exists should be. So an
  # empty want means "keep whatever branch this is on current", and only a named
  # one can ever move HEAD.
  if [ -n "$_want" ] && [ "$_want" != "$_branch" ]; then
    if [ "$ADOPT" != 1 ]; then
      report_add stale "$_slug" "$_access" "$_dest" "on $_branch, not $_want — \`git switch $_want\` if you meant to"
      return 0
    fi
    if [ -n "$_dirty" ]; then
      report_add stale "$_slug" "$_access" "$_dest" "inherited on $_branch with uncommitted changes; left as captured"
      return 0
    fi
    # The first form is what git does by itself when the branch exists only on
    # the remote; the second is for a git too old to guess, and for a remote
    # whose HEAD this clone never learned.
    if ! gitq "$_dest" switch --quiet "$_want" >>"$WORK/log" &&
       ! gitq "$_dest" switch --quiet --track -c "$_want" "origin/$_want" >>"$WORK/log"; then
      report_add failed "$_slug" "$_access" "$_dest" "could not switch to $_want, see $LOG_FILE"
      return 0
    fi
    _branch=$_want
    _switched=1
  fi

  # What this branch actually tracks, falling back to the remote branch of the
  # same name: a checkout that arrived inside a template may carry no tracking
  # config at all, and a branch with no upstream anywhere is a local branch
  # somebody made, which is theirs.
  _up=$(gitq "$_dest" rev-parse --abbrev-ref --symbolic-full-name '@{upstream}') || _up=
  if [ -z "$_up" ] && gitq "$_dest" rev-parse --verify --quiet "refs/remotes/origin/$_branch" >/dev/null; then
    _up="origin/$_branch"
  fi
  if [ -z "$_up" ]; then
    report_add ready "$_slug" "$_access" "$_dest" "${_switched:+switched to }$_branch, no upstream"
    return 0
  fi

  # left...right counts commits the upstream has that we do not, then ours it
  # does not. `set --` is safe in here: the four arguments were saved above.
  set -- $(gitq "$_dest" rev-list --left-right --count "$_up...HEAD" || echo "0 0")
  _behind=${1:-0}; _ahead=${2:-0}

  if [ "$_ahead" != 0 ] && [ "$_behind" != 0 ]; then
    report_add stale "$_slug" "$_access" "$_dest" "diverged from $_up ($_ahead ahead, $_behind behind)"
  elif [ "$_ahead" != 0 ]; then
    report_add stale "$_slug" "$_access" "$_dest" "$_ahead not pushed to $_up"
  elif [ "$_behind" = 0 ]; then
    report_add ready "$_slug" "$_access" "$_dest" "${_switched:+switched to }$_branch, up to date"
  elif [ -n "$_dirty" ]; then
    report_add stale "$_slug" "$_access" "$_dest" "$_behind behind $_up, uncommitted changes"
  elif gitq "$_dest" merge --ff-only --quiet "$_up" >>"$WORK/log"; then
    report_add ready "$_slug" "$_access" "$_dest" "${_switched:+switched to }$_branch, $_behind fast-forwarded"
  else
    report_add failed "$_slug" "$_access" "$_dest" "could not fast-forward $_behind, see $LOG_FILE"
  fi
}

while IFS="$SEP" read -r host slug ref path access; do
  owner=${slug%%/*}
  name=${slug##*/}
  # Default layout: ~/<name> for a single attachment, ~/src/<owner>/<name> once
  # there is more than one. `cd hivemind` beats `cd src/wandb/hivemind` and the
  # single-repo case is the common one; several repos need the disambiguation.
  # An explicit path from the attachment overrides both, and is relative to the
  # home directory because the gateway refuses an absolute one.
  if [ -n "$path" ]; then
    dest="$HOME_DIR/$path"
  elif [ "$count" -gt 1 ]; then
    dest="$HOME_DIR/src/$owner/$name"
  else
    dest="$HOME_DIR/$name"
  fi

  # Anything already at EITHER default location is left exactly as it is, git
  # repo or not, and is reported where it actually sits.
  #
  # Checking both matters because the default above is a function of how many
  # attachments the tag carries *right now*, and that number changes under a
  # checkout that has already been made. Attaching a second repository moves the
  # first one's default from ~/<name> to ~/src/<owner>/<name>; detaching it
  # moves the survivor back. Testing only the freshly computed path would find
  # nothing there, clone a second full copy, and report the empty one as the
  # location — while a week of uncommitted work sat in the other directory,
  # unmentioned. Detaching makes that worse than untidy: `repo rm` promises the
  # existing clone "is left alone", and re-cloning the survivor elsewhere is
  # precisely not leaving it alone.
  #
  # An explicit --path is exempt: it is stable by construction, and someone who
  # named a directory does not want us guessing at two others.
  #
  # What happens to the one it finds is refresh_checkout's, and the promise the
  # paragraph above makes is unchanged by it: the checkout stays where it is,
  # and nothing in this worker can lose work that is in it.
  found=
  for candidate in "$dest" "$HOME_DIR/$name" "$HOME_DIR/src/$owner/$name"; do
    [ -n "$path" ] && candidate=$dest
    if [ -e "$candidate" ]; then found=$candidate; break; fi
  done
  if [ -n "$found" ]; then
    refresh_checkout "$found" "$slug" "$access" "$ref"
    continue
  fi
  if [ "$MODE" != sync ]; then
    report_add pending "$slug" "$access" "$dest" 'run `sparkbox repos sync`'
    continue
  fi

  publish "$(status_line "$slug")"
  # stdin comes from the manifest rows this loop is reading; hand every child
  # /dev/null instead so nothing downstream can eat a row it did not ask for.
  if ! $RUNAS mkdir -p "$(dirname "$dest")" </dev/null >>"$WORK/log" 2>&1; then
    report_add failed "$slug" "$access" "$dest" "could not create $(dirname "$dest")"
    continue
  fi
  # A blobless clone, not a shallow one: agents run `git log`, `git blame` and
  # `git bisect`, all of which a truncated history breaks. Full history, blobs
  # fetched on demand.
  set -- clone --filter=blob:none
  if [ -n "$ref" ]; then set -- "$@" --branch "$ref"; fi
  set -- "$@" "https://$host/$slug.git" "$dest"
  # GIT_TERMINAL_PROMPT=0 so a repository the credential helper cannot get a
  # token for fails in seconds instead of blocking on a username prompt nobody
  # is there to answer.
  if $RUNAS env GIT_TERMINAL_PROMPT=0 git "$@" </dev/null >>"$WORK/log" 2>&1; then
    report_add ready "$slug" "$access" "$dest" cloned
  else
    report_add failed "$slug" "$access" "$dest" "clone failed, see $LOG_FILE"
  fi
done < "$WORK/rows"

if [ "$MODE" = sync ]; then
  cp -f "$WORK/log" "$LOG_FILE" 2>/dev/null || true
  chmod 0644 "$LOG_FILE" 2>/dev/null || true
fi

if [ "$count" -eq 0 ]; then
  # A banner line reading "none attached" on every login of every sandbox that
  # never had a repo is noise, so the no-attachment case gets no line at all.
  clear_status
  echo "no repos are attached to this sandbox"
  exit 0
fi
publish "$(status_line)"
while IFS="$SEP" read -r state slug access dest detail; do
  case "$dest" in
    "$HOME_DIR"/*) dest="~/${dest#"$HOME_DIR"/}" ;;
  esac
  printf '%-40s %-8s %-6s %-26s %s\n' "$slug" "$state" "$access" "$dest" "$detail"
done < "$WORK/report"

# `survey` is `status` with an opinion, and it exists for one caller: the
# in-guest `sparkbox snapshot`, which has to decide whether this disk is fit to
# be frozen into a template. Exit 3 says at least one checkout has a git
# operation in flight — a rebase, a merge, a bisect. Those are the one state
# that makes a capture actively BROKEN rather than merely stale: the lock file
# and the rebase directory are copied byte-for-byte, so every fork of that
# template inherits a git that refuses to run, in a box whose owner never saw
# the rebase. Everything else a survey reports is a surprise, not a defect, and
# is printed rather than refused.
if [ "$MODE" = survey ]; then
  while IFS="$SEP" read -r state rest; do
    if [ "$state" = failed ]; then exit 3; fi
  done < "$WORK/report"
fi
EOF
chmod 0755 "$MNT/usr/local/sbin/sparkbox-repos"

# Fork identity reset: give a sandbox booted from somebody's template its own
# machine identity, before anything can publish the one it inherited.
#
# WHY THIS IS IN THE GUEST AT ALL. Capturing a template used to end with the
# host loop-mounting the captured ext4 and deleting these same paths
# (`sanitizeTemplate`). That mount is exactly what CKS refuses — the VM
# controller drops every capability and cannot call mount(2), because putting
# the host kernel's ext4 driver in front of a guest-authored filesystem would
# enlarge the trusted computing base past Firecracker and KVM — and so the whole
# snapshot feature was refused with it. Nothing being deleted here needs host
# enforcement: a template is bound `(owner, tag)` and masked from every other
# owner, so the only person who boots it is the person who made it. Moving the
# work to the fork's own first boot costs nobody anything and lets the capture
# be a copy of opaque bytes. See docs/cks-snapshot-design.md.
#
# WHY THE CMDLINE IS THE MARKER. sparkbox_host= is written by the host on every
# boot and no guest can forge it; the stamp is what this rootfs was last
# configured as. They agree on a resume and on an ordinary reboot, and disagree
# in exactly two cases — this disk is a fork of a template, or the sandbox was
# renamed — both of which want a new identity.
#
# Ordered Before=sparkbox-net.service, whose `ssh-keygen -A` is what regenerates
# the host keys removed here, and before sshd either way.
cat > "$MNT/usr/local/sbin/sparkbox-identity-reset" <<'EOF'
#!/bin/sh
# Reset per-guest identity when this rootfs is not the sandbox it was built as.
#
# R and CMDLINE are overridable so the deploy tests can run this against a tree
# instead of the machine running them, the same way the token and git-identity
# scripts take their paths. Nothing else reads them.
R=${SPARKBOX_ROOT:-}
CMDLINE=${SPARKBOX_CMDLINE:-/proc/cmdline}
STAMP="$R/var/lib/sparkbox/sandbox"
HOST=""; MID=""
for tok in $(cat "$CMDLINE" 2>/dev/null); do
  case "$tok" in
    sparkbox_host=*)      HOST="${tok#sparkbox_host=}" ;;
    systemd.machine_id=*) MID="${tok#systemd.machine_id=}" ;;
  esac
done

# No marker means an older host that predates this, or a boot we cannot reason
# about. Leave the guest exactly as it is: a wrong reset costs somebody their
# known_hosts entry, and doing nothing costs a fork a shared host key it would
# have had anyway before this existed.
[ -n "$HOST" ] || exit 0
[ "$(cat "$STAMP" 2>/dev/null)" = "$HOST" ] && exit 0

# Remove the parent's host keys and make this guest its own, HERE, rather than
# leaving it to the `ssh-keygen -A` in sparkbox-net.service that is ordered
# after us.
#
# Depending on that ordering would make the worst failure in this file a quiet
# one: -A only creates keys that are MISSING, so a reset landing after it
# deletes the fresh pair and leaves the guest with none at all — sshd exits on
# "no hostkeys available" and the box is simply unreachable, with nothing in it
# to say why. Generating them here costs a second on a fork's first boot, makes
# the second -A a no-op, and means this script is correct even in a rootfs whose
# unit ordering we did not write.
rm -f "$R"/etc/ssh/ssh_host_* 2>/dev/null
if [ -n "$R" ]; then
  ssh-keygen -A -f "$R" 2>/dev/null
else
  ssh-keygen -A 2>/dev/null
fi

# PID 1 has already read /etc/machine-id by now, so the cmdline is what actually
# gives this boot its own id; this keeps the file honest for every boot after,
# and for images whose systemd is too old to read the argument.
if [ -n "$MID" ]; then
  printf '%s\n' "$MID" > "$R/etc/machine-id" 2>/dev/null
else
  : > "$R/etc/machine-id" 2>/dev/null
fi
# Regenerated by dbus at start; it has not started yet.
rm -f "$R/var/lib/dbus/machine-id" 2>/dev/null

mkdir -p "$(dirname "$STAMP")" 2>/dev/null
printf '%s\n' "$HOST" > "$STAMP" 2>/dev/null
exit 0
EOF
chmod 0755 "$MNT/usr/local/sbin/sparkbox-identity-reset"

# systemd path (ubuntu:24.04 and friends). Slim images without systemd fall
# back to the tiny rc build-rootfs.sh writes, which calls sparkbox-token once.
if [ -e "$MNT/lib/systemd/systemd" ]; then
  # Always present in a real systemd tree; created so this branch depends on
  # nothing but the systemd binary it just tested for.
  mkdir -p "$MNT/etc/systemd/system"
  # Refresh on boot, then every 45 minutes. The token lives 1h, so refreshing
  # at ~75% of its life leaves room for a couple of failures before anything
  # expires — the same shape the kubelet uses for projected tokens.
  cat > "$MNT/etc/systemd/system/sparkbox-token.service" <<'EOF'
[Unit]
Description=sparkbox workload identity token refresh
Wants=network-online.target
After=network-online.target sparkbox-net.service
# Give up after 10 tries rather than hammering a dead metadata endpoint
# forever; the timer comes back around regardless.
StartLimitIntervalSec=300
StartLimitBurst=10

[Service]
Type=oneshot
ExecStart=/usr/local/sbin/sparkbox-token
# The host may still be wiring up our tap on a cold boot, so retry instead of
# leaving the guest tokenless until the next timer tick.
Restart=on-failure
RestartSec=5s
# Deliberately NOT RemainAfterExit=yes: the timer below re-triggers this unit,
# and a start job against an already-active oneshot does nothing at all — the
# 45-minute refresh would silently never run again after the boot fetch.

[Install]
WantedBy=multi-user.target
EOF

  cat > "$MNT/etc/systemd/system/sparkbox-token.timer" <<'EOF'
[Unit]
Description=refresh the sparkbox workload identity token

[Timer]
# The BOOT fetch is not this timer's job any more — the service is wanted by
# multi-user.target, so it is ordered into boot behind sparkbox-net and runs as
# soon as the tap can carry it, instead of at a fixed OnBootSec that had to be
# guessed and was always going to be either too early to work or too late to be
# useful. It was 10s, and the daemon that needs the token was up at 1.7s.
#
# Both settings are the refresh, from either anchor: OnUnitActiveSec covers the
# normal case (45 minutes after the last successful fetch) and OnBootSec is the
# backstop for a boot where the service never became active at all, since an
# OnUnitActiveSec timer for a unit that has never run has nothing to count from.
OnBootSec=45min
OnUnitActiveSec=45min

[Install]
WantedBy=timers.target
EOF

  # Clone this sandbox's attached repos once, at boot, after the token unit —
  # the credential helper the clone calls needs the same metadata service, and
  # ordering behind sparkbox-token means the tap is already carrying traffic.
  #
  # There is deliberately NO Before= here, and in particular no
  # `Before=ssh.service sshd.service`. sparkbox-net.service carries that because
  # it generates the host keys sshd needs; copying it into a unit that clones a
  # repository would put a multi-minute network operation in front of the first
  # attach, which is exactly the class of bug main@e196d5f already cost this
  # platform once. A slow clone must make somebody wait for a directory, never
  # for their shell.
  cat > "$MNT/etc/systemd/system/sparkbox-repos.service" <<'EOF'
[Unit]
Description=sparkbox repo checkout
Wants=network-online.target
After=network-online.target sparkbox-net.service sparkbox-token.service

[Service]
Type=oneshot
# Correct HERE, unlike sparkbox-token.service: nothing re-triggers this unit, so
# staying active is what lets `systemctl status` show that the boot pass ran and
# stops a stray `systemctl start` from cloning a second time. If a repos TIMER
# is ever added, this line has to go with it.
RemainAfterExit=yes
# Generous but bounded. A large monorepo can outlast systemd's 90s default and
# be killed mid-clone; a hung one must still let go. The unit blocks nothing, so
# a long wait here costs no one their session.
TimeoutStartSec=1800
ExecStart=/usr/local/sbin/sparkbox-repos sync

[Install]
WantedBy=multi-user.target
EOF

  # Enable without a chroot: symlink into the target's .wants directory. That
  # symlink IS what `systemctl enable` writes. Three units are enabled now: the
  # token service owns the boot fetch, the timer owns the refresh, and the repos
  # service owns the boot clone.
  # DefaultDependencies=no and Before=sparkbox-net.service: the reset removes the
  # host keys that unit's `ssh-keygen -A` then regenerates, so it has to be
  # ahead of it, and both are ahead of sshd. sysinit.target rather than
  # multi-user.target for the same reason sparkbox-net uses the early ordering —
  # a fork must not be reachable under the identity it inherited.
  cat > "$MNT/etc/systemd/system/sparkbox-identity-reset.service" <<'EOF'
[Unit]
Description=sparkbox fork identity reset
DefaultDependencies=no
After=local-fs.target
Before=sparkbox-net.service ssh.service sshd.service

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/local/sbin/sparkbox-identity-reset

[Install]
WantedBy=multi-user.target
EOF

  mkdir -p "$MNT/etc/systemd/system/timers.target.wants" \
           "$MNT/etc/systemd/system/multi-user.target.wants"
  ln -sf ../sparkbox-identity-reset.service \
    "$MNT/etc/systemd/system/multi-user.target.wants/sparkbox-identity-reset.service"
  ln -sf ../sparkbox-token.timer \
    "$MNT/etc/systemd/system/timers.target.wants/sparkbox-token.timer"
  ln -sf ../sparkbox-token.service \
    "$MNT/etc/systemd/system/multi-user.target.wants/sparkbox-token.service"
  ln -sf ../sparkbox-repos.service \
    "$MNT/etc/systemd/system/multi-user.target.wants/sparkbox-repos.service"
fi

# Stamp the tree so refresh-agent-tools.sh can tell a patched template from a
# stale one.
mkdir -p "$MNT/etc/sparkbox"
echo "IDENTITY_REV=$IDENTITY_REV" > "$MNT/etc/sparkbox/identity-rev"

echo "   guest identity payload installed (rev $IDENTITY_REV)"
