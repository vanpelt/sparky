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
IDENTITY_REV=5

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
  *)
    echo "usage: sparkbox <pin|unpin|status|make-public|make-private|set-port PORT>" >&2
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

  # Enable without a chroot: symlink into the target's .wants directory. That
  # symlink IS what `systemctl enable` writes. Both units are enabled now: the
  # service owns the boot fetch, the timer owns the refresh.
  mkdir -p "$MNT/etc/systemd/system/timers.target.wants" \
           "$MNT/etc/systemd/system/multi-user.target.wants"
  ln -sf ../sparkbox-token.timer \
    "$MNT/etc/systemd/system/timers.target.wants/sparkbox-token.timer"
  ln -sf ../sparkbox-token.service \
    "$MNT/etc/systemd/system/multi-user.target.wants/sparkbox-token.service"
fi

# Stamp the tree so refresh-agent-tools.sh can tell a patched template from a
# stale one.
mkdir -p "$MNT/etc/sparkbox"
echo "IDENTITY_REV=$IDENTITY_REV" > "$MNT/etc/sparkbox/identity-rev"

echo "   guest identity payload installed (rev $IDENTITY_REV)"
