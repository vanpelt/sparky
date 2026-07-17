# sparkbox base image — the rootfs every sandbox microVM boots from.
#
# Adapted (heavily trimmed) from boldsoftware/exeuntu's Dockerfile: we keep the
# "real machine, not a stripped container" feel (man pages, systemd, a login
# user, a polished shell) but drop everything exe.dev-specific (shelley, the
# exeuntu guest CLI, pi, tailscale, exetini) and every heavy toolchain we don't
# need. This replaces ghcr.io/openai/codex-universal (~30GB unpacked) as the base
# hack/build-rootfs.sh flattens into an ext4 template.
#
# hack/build-artifacts.sh builds this, then build-rootfs.sh flattens it, bakes in
# the fleet gateway public key, and installs the guest hooks. Firecracker boots
# the result with init=/sbin/init (systemd) directly — there is no CMD/entrypoint.

# Self-contained headless Chrome for browser automation (screenshots, scraping).
FROM docker.io/chromedp/headless-shell:stable AS chrome

FROM ubuntu:24.04

SHELL ["/bin/bash", "-euxo", "pipefail", "-c"]

# ---- packages -------------------------------------------------------------
# Un-minimize into a usable dev machine, pull current security updates (the base
# layer can be months behind), then install a lean-but-real toolset. Language
# toolchains that agents rarely need are deliberately absent — uv/pipx/go/npm
# make on-demand installs cheap. docker/tailscale/mitmproxy are included (all
# disabled by default; start on demand). Excluded vs exeuntu: imagemagick/ffmpeg
# and the ubuntu-server/-standard/-dev-tools metapackages — the server set drags
# in lvm/mdadm/multipath/iscsi/cloud-init that a Firecracker microVM can't use and
# that our systemd boot-trim below would just have to mask.
RUN rm -f /etc/dpkg/dpkg.cfg.d/excludes /etc/dpkg/dpkg.cfg.d/01_nodoc && \
    apt-get update && \
    DEBIAN_FRONTEND=noninteractive apt-get -y \
      -o Dpkg::Options::=--force-confold -o Dpkg::Options::=--force-confdef \
      dist-upgrade && \
    echo 'debconf debconf/frontend select Noninteractive' | debconf-set-selections && \
    # unminimize restores man pages/docs; on 24.04 it can be a separate package.
    ( command -v unminimize >/dev/null 2>&1 || \
      DEBIAN_FRONTEND=noninteractive apt-get install -y unminimize >/dev/null 2>&1 || true ) && \
    ( yes | DEBIAN_FRONTEND=noninteractive unminimize || true ) && \
    DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      ca-certificates gnupg wget curl git jq ripgrep fd-find tree file less \
      lsof rsync unzip zip xz-utils zstd \
      build-essential make python3-pip python-is-python3 pipx sqlite3 \
      vim neovim nano bash-completion \
      iproute2 net-tools iputils-ping netcat-openbsd socat dnsutils \
      locales tzdata ncurses-term ncurses-bin procps psmisc bsdmainutils util-linux \
      libcap2-bin htop btop \
      man-db manpages manpages-dev \
      nginx \
      openssh-server openssh-client sudo \
      systemd systemd-sysv dbus-user-session \
      docker.io docker-buildx docker-compose-v2 mitmproxy \
      libglib2.0-0 libnss3 libx11-6 libxcomposite1 libxdamage1 libxext6 \
      libxi6 libxrandr2 libgbm1 libgtk-3-0 fontconfig \
      fonts-noto-color-emoji fonts-symbola && \
    # fd ships as `fdfind` on Debian/Ubuntu; give it its upstream name too.
    ln -sf "$(command -v fdfind)" /usr/local/bin/fd && \
    # Locale so forwarded LC_* stop warning "cannot change locale".
    sed -i 's/^# *en_US.UTF-8 UTF-8/en_US.UTF-8 UTF-8/' /etc/locale.gen && \
    locale-gen && update-locale LANG=en_US.UTF-8 && \
    # Let non-root ping without sudo.
    setcap cap_net_raw=+ep /usr/bin/ping && \
    fc-cache -f && \
    # policy-rc.d blocks service starts during build; we run systemd at runtime.
    rm -f /usr/sbin/policy-rc.d && \
    rm -rf /var/lib/apt/lists/* /run/sshd && mkdir -p /run/sshd

# ---- toolchains: Go, Node, uv, starship -----------------------------------
# Installed system-wide under /usr/local so they resolve in NON-interactive ssh
# execs (which read /etc/environment's PATH, not a profile) — this is why we use
# the official tarballs rather than a per-user nvm/pyenv.
RUN ARCH=$(dpkg --print-architecture) && \
    GO_VERSION=$(curl -fsSL 'https://go.dev/dl/?mode=json' | jq -r '.[0].version') && \
    curl -fsSL "https://go.dev/dl/${GO_VERSION}.linux-${ARCH}.tar.gz" | tar -xzC /usr/local && \
    ln -sf /usr/local/go/bin/go /usr/local/bin/go && \
    ln -sf /usr/local/go/bin/gofmt /usr/local/bin/gofmt

RUN ARCH=$(dpkg --print-architecture) && \
    case "$ARCH" in amd64) NARCH=x64 ;; arm64) NARCH=arm64 ;; \
      *) echo "unsupported arch: $ARCH" >&2; exit 1 ;; esac && \
    NODE_VERSION=$(curl -fsSL https://nodejs.org/dist/index.json | jq -r '[.[] | select(.lts != false)][0].version') && \
    curl -fsSL "https://nodejs.org/dist/${NODE_VERSION}/node-${NODE_VERSION}-linux-${NARCH}.tar.xz" \
      | tar -xJC /usr/local --strip-components=1 --no-same-owner && \
    corepack enable

RUN curl -LsSf https://astral.sh/uv/install.sh | env UV_INSTALL_DIR=/usr/local/bin sh
RUN curl -fsSL https://starship.rs/install.sh | sh -s -- -y -b /usr/local/bin

# Tailscale via its apt keyring (the official install.sh method). Installed but
# disabled below — a sandbox runs `sudo tailscale up` when it wants a tailnet.
RUN curl -fsSL https://pkgs.tailscale.com/stable/ubuntu/noble.noarmor.gpg \
      -o /usr/share/keyrings/tailscale-archive-keyring.gpg && \
    curl -fsSL https://pkgs.tailscale.com/stable/ubuntu/noble.tailscale-keyring.list \
      -o /etc/apt/sources.list.d/tailscale.list && \
    apt-get update && \
    DEBIAN_FRONTEND=noninteractive apt-get install -y tailscale && \
    rm -rf /var/lib/apt/lists/*

# ---- systemd: trim for a fast microVM boot --------------------------------
# ubuntu:24.04's image is already fairly minimal (we skip the ubuntu-server
# metapackages), so we only mask the units that would otherwise probe hardware,
# fight our kernel-cmdline networking, or churn in the background. ssh stays
# enabled — the gateway logs in over sshd; nginx ships disabled (start on demand).
RUN systemctl set-default multi-user.target && \
    systemctl mask -- \
      getty.target console-getty.service getty@.service \
      systemd-udevd.service systemd-udevd-control.socket systemd-udevd-kernel.socket \
      systemd-udev-trigger.service systemd-udev-settle.service \
      systemd-resolved.service \
      systemd-networkd.service systemd-networkd.socket networkd-dispatcher.service \
      systemd-random-seed.service \
      e2scrub_all.timer e2scrub_reap.service fstrim.timer \
      man-db.timer motd-news.timer motd-news.service \
      apt-daily.timer apt-daily-upgrade.timer \
      2>/dev/null || true && \
    # sshd as a plain always-on daemon on :22: enable ssh.service and DISABLE the
    # socket-activation unit, or the two conflict on 24.04 and :22 ends up unbound.
    systemctl enable ssh.service 2>/dev/null || true && \
    systemctl disable ssh.socket 2>/dev/null || true && \
    # Heavy optional daemons ship installed but OFF (keep boot fast + RAM free);
    # a sandbox starts them on demand. docker.socket stays enabled, so `docker`
    # activates the engine on first use without it running in every idle box.
    systemctl disable nginx.service docker.service containerd.service tailscaled.service \
      2>/dev/null || true && \
    mkdir -p /etc/systemd/journald.conf.d && \
    printf '[Journal]\nStorage=persistent\n' > /etc/systemd/journald.conf.d/persistent.conf
# Grow the root fs on first boot if the host ever ships a smaller template than
# the block device (build-rootfs currently sizes the fs to the full ceiling, so
# this is a no-op today — kept for forward-compat, matching exeuntu).
RUN echo '/dev/vda / ext4 defaults,x-systemd.growfs 0 1' > /etc/fstab
COPY tmpfiles-tmp.conf /etc/tmpfiles.d/tmp.conf

# ---- login user: sparky ---------------------------------------------------
# Rename ubuntu:24.04's uid-1000 `ubuntu` user to `sparky`: passwordless sudo, a
# real home, linger so a user systemd instance + XDG_RUNTIME_DIR come up. The
# gateway logs in here (see the sparkbox.login-user label); build-rootfs.sh bakes
# the gateway key into /home/sparky/.ssh.
RUN usermod -l sparky ubuntu && groupmod -n sparky ubuntu && \
    usermod -d /home/sparky -s /bin/bash sparky && \
    if [ -d /home/ubuntu ]; then mv /home/ubuntu /home/sparky; \
    else mkdir -p /home/sparky && (cp -aT /etc/skel /home/sparky 2>/dev/null || true); fi && \
    usermod -aG sudo sparky && usermod -aG docker sparky && \
    echo 'sparky ALL=(ALL) NOPASSWD:ALL' > /etc/sudoers.d/sparky && \
    chmod 440 /etc/sudoers.d/sparky && \
    (sed -i 's/^ubuntu:/sparky:/' /etc/subuid /etc/subgid 2>/dev/null || true) && \
    mkdir -p /var/lib/systemd/linger && touch /var/lib/systemd/linger/sparky && \
    mkdir -p /home/sparky/.ssh /home/sparky/go && chmod 700 /home/sparky/.ssh && \
    printf 'export PATH="$HOME/.local/bin:$HOME/go/bin:$PATH"\nexport XDG_RUNTIME_DIR="/run/user/$(id -u)"\n' \
      | tee -a /home/sparky/.bashrc >> /home/sparky/.profile && \
    chown -R sparky:sparky /home/sparky && \
    git config --system init.defaultBranch main

# ---- shell polish: env, MOTD, starship, ghostty ---------------------------
# /etc/environment carries the toolchain PATH into non-interactive ssh (docker
# ENV is lost on `docker export`). Replace Ubuntu's ad-wall MOTD with a static
# banner. Wire starship into the global interactive bashrc.
COPY environment /etc/environment
COPY motd /etc/motd
COPY starship.toml /etc/starship.toml
COPY bashrc-snippet.sh /tmp/bashrc-snippet.sh
RUN rm -f /etc/update-motd.d/* /etc/legal 2>/dev/null || true && \
    cat /tmp/bashrc-snippet.sh >> /etc/bash.bashrc && rm /tmp/bashrc-snippet.sh
# The ghostty terminfo the user loves: make TERM=xterm-ghostty a first-class
# citizen so curses apps init correctly under a forwarded Ghostty.
COPY xterm-ghostty.terminfo /tmp/xterm-ghostty.terminfo
RUN tic -x /tmp/xterm-ghostty.terminfo && rm /tmp/xterm-ghostty.terminfo

# ---- nginx landing page ---------------------------------------------------
COPY nginx.conf /etc/nginx/sites-available/default
COPY index.html /var/www/html/index.html
RUN chmod 644 /var/www/html/index.html

# ---- headless Chrome ------------------------------------------------------
COPY --from=chrome /headless-shell /headless-shell

# ---- host keys ------------------------------------------------------------
# openssh-server generated per-image host keys during install; do not bake those
# into every sandbox. sshd regenerates them on first boot (the gateway pins no
# host key — it owns the only route to the guest).
RUN rm -f /etc/ssh/ssh_host_*_key /etc/ssh/ssh_host_*_key.pub

LABEL sparkbox.login-user="sparky"
LABEL org.opencontainers.image.title="sparkbox base"
LABEL org.opencontainers.image.description="Lean Ubuntu 24.04 dev image for sparkbox agentic microVMs"
