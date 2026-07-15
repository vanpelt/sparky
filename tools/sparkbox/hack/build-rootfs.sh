#!/usr/bin/env bash
# Build a Firecracker rootfs ext4 template from an OCI/Docker image.
#
# This is the "container image as block device" trick from the design doc:
# flatten an image, bake in the gateway's SSH public key + a tiny init, and
# every sandbox gets a CoW reflink copy of the result.
#
# Usage: build-rootfs.sh <docker-image> <output.ext4> <gateway-pubkey-file> [size-mb]
# Requires: docker (or podman), mkfs.ext4, root (for the loop mount).
#
# Works with any Debian/Ubuntu-based image, including big toolchain images like
# ghcr.io/openai/codex-universal (ubuntu:24.04 + toolchains for ~10 languages,
# ~30GB unpacked — pass a size-mb that fits; the guard below checks). The ext4
# is a thin ceiling: hosts clone it with XFS reflinks, so sandboxes only pay
# for blocks they write.
set -euo pipefail
HERE=$(cd "$(dirname "$0")" && pwd)

IMAGE=${1:?docker image, e.g. ubuntu:24.04}
OUT=${2:?output path, e.g. images/ubuntu.ext4}
PUBKEY=${3:?gateway public key file (state/gateway_upstream_key.pem -> derive with sparkbox, or ssh-keygen -y)}
SIZE_MB=${4:-2048}

DOCKER=$(command -v docker || command -v podman)
MNT=$(mktemp -d)
CID=""
BUILD_IMAGE=""
cleanup() {
  [ -n "$CID" ] && $DOCKER rm -f "$CID" >/dev/null 2>&1 || true
  [ -n "$BUILD_IMAGE" ] && $DOCKER rmi "$BUILD_IMAGE" >/dev/null 2>&1 || true
  mountpoint -q "$MNT" && umount "$MNT" || true
  rmdir "$MNT" 2>/dev/null || true
}
trap cleanup EXIT

# Ensure the image ships sshd + a real init AND a polished interactive
# environment before we flatten it. We do this with `docker build` (a real
# builder with working apt + network) rather than chrooting into the exported
# tree — the chroot has no /proc,/sys,/dev bind mounts, so apt/gpg fail with
# "apt-key error code 29". Debian/Ubuntu only; bring your own sshd for other
# bases.
#
# The package set makes the sandbox feel like a real machine rather than a
# stripped container: procps (`top`/`ps`/`free`), ncurses-term (the terminfo DB
# — without it curses apps like `top` init-fail and print nothing under a
# forwarded xterm-256color TERM), locales (kills the "cannot change locale"
# warning when the client forwards LC_*), plus everyday CLI tooling and
# starship for a nice prompt. `unminimize` restores man pages/docs the base
# image strips (best-effort — it's slow and occasionally flaky in a builder).
echo ">> ensuring sshd + init + polished env in $IMAGE"
BUILD_IMAGE="sparkbox-rootfs-build:$$"
$DOCKER build -t "$BUILD_IMAGE" - >/dev/null <<EOF
FROM $IMAGE
ENV DEBIAN_FRONTEND=noninteractive
RUN set -eu; \
    apt-get update -qq; \
    apt-get install -y -qq --no-install-recommends \
      openssh-server systemd-sysv iproute2 iputils-ping ca-certificates \
      procps ncurses-term ncurses-base locales tzdata \
      curl git less nano vim-tiny sudo bash-completion; \
    sed -i 's/^# *en_US.UTF-8 UTF-8/en_US.UTF-8 UTF-8/' /etc/locale.gen; \
    locale-gen; \
    update-locale LANG=en_US.UTF-8; \
    curl -fsSL https://starship.rs/install.sh -o /tmp/starship-install.sh; \
    sh /tmp/starship-install.sh -y -b /usr/local/bin; \
    rm -f /tmp/starship-install.sh; \
    if command -v unminimize >/dev/null 2>&1; then yes | unminimize || true; fi; \
    rm -rf /var/lib/apt/lists/*; \
    mkdir -p /run/sshd
# Persist the image's environment for VM logins. Docker ENV vars (PATH with
# tool shims, NVM_DIR, PYENV_ROOT, COREPACK_*, ...) live in image *metadata*
# and vanish when \`docker export\` flattens the filesystem. Resolve a login
# shell's final environment (ENV + /etc/profile hooks: mise/pyenv/nvm/cargo/
# phpenv init) and bake it into /etc/environment, which pam_env applies to
# EVERY ssh session — including the non-interactive \`ssh box '<cmd>'\` execs
# coding agents use, which read no profile at all. LANG/LC_* are excluded
# (/etc/default/locale owns locale, see update-locale above).
# Also: images like codex-universal ship pyenv with no global version set
# (their container entrypoint picks one; entrypoints never run in a VM), which
# leaves \`python\` shims erroring — default to the newest installed version.
RUN set -eu; \
    if command -v pyenv >/dev/null 2>&1; then \
      cur=\$(pyenv global 2>/dev/null || true); \
      if [ -z "\$cur" ] || [ "\$cur" = system ]; then \
        pyenv global "\$(pyenv versions --bare | sort -V | tail -1)"; \
      fi; \
    fi; \
    bash -lc env 2>/dev/null \
      | grep -E '^[A-Za-z_][A-Za-z0-9_]*=' \
      | grep -vE '^(HOME|HOSTNAME|PWD|OLDPWD|SHLVL|SHELL|LOGNAME|USER|MAIL|TERM|PS1|LS_COLORS|LESS[A-Z]*|LANG|LC_[A-Z]+|DEBIAN_FRONTEND|_)=' \
      > /etc/environment
EOF

# Fail fast if the flattened image won't fit the requested ext4 — better now
# than 20 minutes in with a half-extracted tar and ENOSPC. 1.2x + 512MB covers
# ext4 metadata + a little breathing room (the real workspace headroom should
# come from passing a generous size-mb; the copy is thin either way).
IMG_BYTES=$($DOCKER image inspect -f '{{.Size}}' "$BUILD_IMAGE")
MIN_MB=$(( IMG_BYTES / 1048576 * 12 / 10 + 512 ))
if [ "$SIZE_MB" -lt "$MIN_MB" ]; then
  echo "size-mb $SIZE_MB is too small: $IMAGE unpacks to ~$((IMG_BYTES/1048576))MB (need >= ${MIN_MB}MB)" >&2
  exit 1
fi

echo ">> creating ${SIZE_MB}MB ext4 at $OUT"
truncate -s "${SIZE_MB}M" "$OUT"
mkfs.ext4 -q -F "$OUT"
mount -o loop "$OUT" "$MNT"

echo ">> exporting $IMAGE"
CID=$($DOCKER create "$BUILD_IMAGE" /bin/true)
$DOCKER export "$CID" | tar -x -C "$MNT"

echo ">> baking in gateway key + sshd config"
mkdir -p "$MNT/root/.ssh"
cp "$PUBKEY" "$MNT/root/.ssh/authorized_keys"
chmod 700 "$MNT/root/.ssh" && chmod 600 "$MNT/root/.ssh/authorized_keys"

if [ ! -x "$MNT/usr/sbin/sshd" ]; then
  echo "sshd still missing after build — use a Debian/Ubuntu base or an image that ships sshd" >&2
  exit 1
fi
mkdir -p "$MNT/etc/ssh/sshd_config.d"
cat > "$MNT/etc/ssh/sshd_config.d/sparkbox.conf" <<'EOF'
PermitRootLogin prohibit-password
PasswordAuthentication no
AcceptEnv LANG LC_* GIT_* TERM
EOF

echo ">> baking in the polished shell environment (motd, starship, prompt)"
# Custom login splash. Ubuntu's default MOTD is a wall of dynamic ads
# ("system minimized", ESM upsells, last-login noise) generated by the
# /etc/update-motd.d scripts and printed by pam_motd — clear them and drop a
# clean static banner instead.
rm -f "$MNT"/etc/update-motd.d/* 2>/dev/null || true
rm -f "$MNT/etc/legal" 2>/dev/null || true
cat > "$MNT/etc/motd" <<'MOTD'

   ____                  _    _
  / ___| _ __   __ _ _ __| | _| |__   _____  __
  \___ \| '_ \ / _` | '__| |/ / '_ \ / _ \ \/ /
   ___) | |_) | (_| | |  |   <| |_) | (_) >  <
  |____/| .__/ \__,_|_|  |_|\_\_.__/ \___/_/\_\
        |_|          agentic sandbox

  An ephemeral microVM. Work persists while the box is warm; it
  suspends when idle and resumes the instant you reconnect.

MOTD

# Starship prompt with sensible, nerd-font-free defaults. The binary is
# installed in the docker build above; here we configure it and wire it into
# the global bashrc (sourced by both login and interactive shells on Ubuntu).
cat > "$MNT/etc/starship.toml" <<'TOML'
add_newline = false
format = '$username$hostname$directory$git_branch$git_status$cmd_duration$character'

[username]
show_always = true
style_user = 'bold cyan'
style_root = 'bold red'
format = '[$user]($style)'

[hostname]
ssh_only = false
style = 'bold green'
format = '[@$hostname]($style) '

[directory]
style = 'bold blue'
truncation_length = 4
truncate_to_repo = false

[git_branch]
symbol = 'git '
style = 'bold purple'

[git_status]
style = 'bold yellow'

[cmd_duration]
min_time = 2000
style = 'yellow'

[character]
success_symbol = '[#](bold green)'
error_symbol = '[#](bold red)'
TOML

# Append the starship hook once; guard so non-interactive shells (e.g. the
# gateway's `ssh <vm> true` health probe) skip it.
touch "$MNT/etc/bash.bashrc"
cat >> "$MNT/etc/bash.bashrc" <<'BRC'

# sparkbox: interactive shell polish
case $- in *i*)
  # Fall back to a known-good TERM when the client's terminal has no terminfo
  # entry in this guest (ghostty/kitty ship their own, absent from ncurses-term)
  # — otherwise curses apps like top/htop silently fail to initialize.
  if ! infocmp "$TERM" >/dev/null 2>&1; then export TERM=xterm-256color; fi
  export STARSHIP_CONFIG=/etc/starship.toml
  if command -v starship >/dev/null 2>&1; then eval "$(starship init bash)"; fi
  ;;
esac
BRC

# Don't let a stale /etc/hostname from the build container leak into the guest;
# the sparkbox-netcfg hook sets the hostname to the sandbox name at boot.
: > "$MNT/etc/hostname"

echo ">> installing IPv6 guest-network hook"
# The kernel ip= arg only configures IPv4. The firecracker driver passes the
# guest's routable IPv6 on the cmdline (sparkbox_ip6=<addr>/127 sparkbox_gw6=..);
# this oneshot applies it to eth0 at boot. No-op when those args are absent.
mkdir -p "$MNT/usr/local/sbin"
cat > "$MNT/usr/local/sbin/sparkbox-netcfg" <<'EOF'
#!/bin/sh
# Apply sparkbox guest config (hostname + IPv6) from the kernel cmdline.
IFACE=eth0
IP6=""; GW6=""; HOST=""
for tok in $(cat /proc/cmdline); do
  case "$tok" in
    sparkbox_ip6=*)  IP6="${tok#sparkbox_ip6=}" ;;
    sparkbox_gw6=*)  GW6="${tok#sparkbox_gw6=}" ;;
    sparkbox_host=*) HOST="${tok#sparkbox_host=}" ;;
  esac
done

# Name the box after the sandbox so the prompt reads root@<name> (the kernel's
# ip= arg otherwise leaves the hostname as the guest IP -> "root@172"). This
# runs before sshd, so the login shell picks up the new hostname. Keep
# /etc/hosts resolvable too, or sudo/tools warn "unable to resolve host".
if [ -n "$HOST" ]; then
  hostname "$HOST" 2>/dev/null || true
  echo "$HOST" > /etc/hostname 2>/dev/null || true
  grep -q "[[:space:]]$HOST\$" /etc/hosts 2>/dev/null || \
    printf '127.0.1.1\t%s\n' "$HOST" >> /etc/hosts
fi

# The kernel ip= arg configures the interface but writes no resolver (Firecracker
# boots vmlinux with no initrd, so nothing populates /etc/resolv.conf). Egress is
# NAT'd, so point at public resolvers; skip if something already set one.
if [ ! -s /etc/resolv.conf ]; then
  printf 'nameserver 1.1.1.1\nnameserver 8.8.8.8\n' > /etc/resolv.conf
fi

[ -n "$IP6" ] || exit 0
ip link set "$IFACE" up
ip -6 addr replace "$IP6" dev "$IFACE"
[ -n "$GW6" ] && ip -6 route replace default via "$GW6" dev "$IFACE"
exit 0
EOF
chmod +x "$MNT/usr/local/sbin/sparkbox-netcfg"

echo ">> installing workload identity (OIDC token) hook"
# Shared with deploy/refresh-agent-tools.sh, which patches the same payload
# into already-published templates on a host — keep it in one place.
"$HERE/../deploy/install-guest-identity.sh" "$MNT"

# systemd path (ubuntu:24.04 and friends): run early, before sshd.
if [ -e "$MNT/lib/systemd/systemd" ]; then
  cat > "$MNT/etc/systemd/system/sparkbox-net.service" <<'EOF'
[Unit]
Description=sparkbox guest IPv6 configuration
DefaultDependencies=no
After=network-pre.target local-fs.target
Before=ssh.service sshd.service network-online.target

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/local/sbin/sparkbox-netcfg

[Install]
WantedBy=multi-user.target
EOF
  # Enable without a chroot: symlink into multi-user.target.wants.
  mkdir -p "$MNT/etc/systemd/system/multi-user.target.wants"
  ln -sf ../sparkbox-net.service \
    "$MNT/etc/systemd/system/multi-user.target.wants/sparkbox-net.service"
fi

# Minimal boot: systemd images work as-is with init=/sbin/init; for slim
# images, drop in a tiny rc that brings up sshd on the kernel-arg-configured
# eth0 (ip=... is handled by the kernel itself, no DHCP client needed).
if [ ! -e "$MNT/sbin/init" ] && [ ! -e "$MNT/lib/systemd/systemd" ]; then
  cat > "$MNT/sbin/init" <<'EOF'
#!/bin/sh
mount -t proc proc /proc
mount -t sysfs sys /sys
mount -t devtmpfs dev /dev 2>/dev/null
/usr/local/sbin/sparkbox-netcfg 2>/dev/null || true
# No systemd here, so no timer: fetch an identity token once at boot. It
# expires in an hour and won't renew — slim images are a fallback for bring-up,
# not the shipped path (which is the systemd timer).
/usr/local/sbin/sparkbox-token 2>/dev/null || true
mkdir -p /run/sshd
/usr/sbin/sshd
exec /bin/sh -c 'while true; do sleep 3600; done'
EOF
  chmod +x "$MNT/sbin/init"
fi

echo ">> done: $OUT"
