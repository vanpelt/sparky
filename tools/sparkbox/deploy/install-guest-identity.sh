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
IDENTITY_REV=7

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
else
  rm -f "$TMP"
fi
EOF
chmod 0755 "$MNT/usr/local/sbin/sparkbox-token"

# A tiny in-guest control client. The metadata service authenticates the
# caller from its tap source address, so this carries no operator credential
# and can only change the sandbox from which the request originated.
sed -e "s/@@META_PORT@@/$META_PORT/g" > "$MNT/usr/local/bin/sparkbox" <<'EOF'
#!/bin/sh
set -eu
GW=$(ip -4 route show default | awk '{print $3; exit}')
[ -n "$GW" ] || { echo "sparkbox: no default gateway" >&2; exit 1; }
META="http://$GW:@@META_PORT@@"

case "${1:-}" in
  pin)    exec curl -fsS --max-time 10 -X POST "$META/self/pin" ;;
  unpin)  exec curl -fsS --max-time 10 -X POST "$META/self/unpin" ;;
  status) exec curl -fsS --max-time 10 "$META/self" ;;
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
    SB=/usr/local/sbin/sparkbox-repos
    if [ "$(id -u)" -ne 0 ] && command -v sudo >/dev/null 2>&1 && sudo -n true 2>/dev/null; then
      SB="sudo -n /usr/local/sbin/sparkbox-repos"
    fi
    case "${2:-}" in
      ''|status) exec $SB status ;;
      sync)      exec $SB sync ;;
      *) echo "usage: sparkbox repos [sync]" >&2; exit 2 ;;
    esac
    ;;
  *)
    echo "usage: sparkbox <pin|unpin|status|make-public|make-private|set-port PORT|repos [sync]>" >&2
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
  sync|status) ;;
  *) echo "usage: sparkbox-repos [status|sync]" >&2; exit 2 ;;
esac

# The account the checkouts belong to (the login user; root on legacy templates).
SANDBOX_USER=@@SANDBOX_USER@@
STATUS_FILE=/run/sparkbox/repos.status
LOG_FILE=/run/sparkbox/repos.log
MOTD_BASE=/etc/sparkbox/motd.base

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

HOME_DIR=$(awk -F: -v u="$SANDBOX_USER" '$1 == u {print $6; exit}' /etc/passwd)
[ -n "$HOME_DIR" ] || give_up "no home directory for $SANDBOX_USER in /etc/passwd"

# Checkouts belong to whoever will edit them, and this unit runs as root.
# Dropping privilege before `git clone` is not tidiness: git refuses to operate
# in a tree owned by somebody else, so a root-owned checkout sitting in a user's
# home is worse than no checkout at all — it is one that every later git command
# in that directory rejects for dubious ownership. If privilege cannot be
# dropped we clone nothing rather than clone it wrong.
RUNAS=
if [ "$(id -u)" = 0 ] && [ "$SANDBOX_USER" != root ]; then
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
    printf 'repos: cloning %s…' "$inflight"
    return 0
  fi
  ready=0; pending=0; failed=0
  while IFS="$SEP" read -r state rest; do
    case "$state" in
      ready)  ready=$((ready + 1)) ;;
      failed) failed=$((failed + 1)) ;;
      *)      pending=$((pending + 1)) ;;
    esac
  done < "$WORK/report"
  out=""
  if [ "$ready" -gt 0 ];   then out="$ready ready"; fi
  if [ "$pending" -gt 0 ]; then out="${out:+$out, }$pending not cloned"; fi
  if [ "$failed" -gt 0 ];  then out="${out:+$out, }$failed failed"; fi
  if [ -z "$out" ]; then out="none attached"; fi
  if [ "$failed" -gt 0 ] || [ "$pending" -gt 0 ]; then
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
      } 2>/dev/null > /etc/.motd.sparkbox; then
    mv -f /etc/.motd.sparkbox /etc/motd 2>/dev/null || rm -f /etc/.motd.sparkbox
  fi
}

publish() {
  mkdir -p /run/sparkbox 2>/dev/null || true
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
  for candidate in "$dest" "$HOME_DIR/$name" "$HOME_DIR/src/$owner/$name"; do
    [ -n "$path" ] && candidate=$dest
    if [ -e "$candidate" ]; then
      report_add ready "$slug" "$access" "$candidate" present
      continue 2
    fi
  done
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
EOF
chmod 0755 "$MNT/usr/local/sbin/sparkbox-repos"

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
  mkdir -p "$MNT/etc/systemd/system/timers.target.wants" \
           "$MNT/etc/systemd/system/multi-user.target.wants"
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
