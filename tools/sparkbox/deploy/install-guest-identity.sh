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
IDENTITY_REV=1

# The metadata port must match internal/metadata.DefaultPort.
META_PORT=8967

mkdir -p "$MNT/usr/local/sbin"

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
sed "s/@@META_PORT@@/$META_PORT/g" > "$MNT/usr/local/sbin/sparkbox-token" <<'EOF'
#!/bin/sh
# Refresh this sandbox's OIDC id token and identity snapshot.
set -eu
TOKEN_FILE=${HIVEMIND_OIDC_TOKEN_FILE:-/var/run/secrets/hivemind/token}
IDENTITY_FILE=/run/sparkbox/identity.json

# The metadata service listens on our default gateway: the host end of our own
# tap. We cannot reach any other sandbox's endpoint, and none can reach ours.
GW=$(ip -4 route show default | awk '{print $3; exit}')
[ -n "$GW" ] || { echo "sparkbox-token: no default gateway" >&2; exit 1; }
META="http://$GW:@@META_PORT@@"

mkdir -p "$(dirname "$TOKEN_FILE")" /run/sparkbox
chmod 0700 "$(dirname "$TOKEN_FILE")"

# Write via temp file + rename: the daemon re-reads this path on its own
# schedule, so it must never catch a half-written token.
TMP="$TOKEN_FILE.tmp"
if curl -fsS --max-time 10 "$META/token" -o "$TMP"; then
  chmod 0600 "$TMP"
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

# systemd path (ubuntu:24.04 and friends). Slim images without systemd fall
# back to the tiny rc build-rootfs.sh writes, which calls sparkbox-token once.
if [ -e "$MNT/lib/systemd/systemd" ]; then
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
EOF

  cat > "$MNT/etc/systemd/system/sparkbox-token.timer" <<'EOF'
[Unit]
Description=refresh the sparkbox workload identity token

[Timer]
OnBootSec=10s
OnUnitActiveSec=45min
AccuracySec=1min

[Install]
WantedBy=timers.target
EOF

  # Enable without a chroot: symlink into the target's .wants directory. Only
  # the timer is enabled — it owns the boot fetch (OnBootSec) as well, so the
  # service must not also be wanted by multi-user.target.
  mkdir -p "$MNT/etc/systemd/system/timers.target.wants"
  ln -sf ../sparkbox-token.timer \
    "$MNT/etc/systemd/system/timers.target.wants/sparkbox-token.timer"
fi

# Stamp the tree so refresh-agent-tools.sh can tell a patched template from a
# stale one.
mkdir -p "$MNT/etc/sparkbox"
echo "IDENTITY_REV=$IDENTITY_REV" > "$MNT/etc/sparkbox/identity-rev"

echo "   guest identity payload installed (rev $IDENTITY_REV)"
