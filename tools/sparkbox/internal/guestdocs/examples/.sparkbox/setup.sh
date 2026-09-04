#!/usr/bin/env bash
# .sparkbox/setup.sh — replays this VM's dev-environment setup.
#
# Written by the agent that first configured this project to run inside a
# Sparkbox VM, and kept current by whichever agent changes that setup next.
# Check it into the project's own repo so a fresh checkout, a fresh VM, or a
# teammate's VM can reproduce the same setup with `bash .sparkbox/setup.sh`
# instead of re-deriving it. Nothing runs it on an ordinary VM's boot; the one
# thing that does is `ssh ctl@<gateway> env build <name>`, which runs it in a
# builder VM and captures the result as that environment's disk.
#
# Every step below is idempotent: installs are safe to repeat, the systemd
# unit is rewritten and reloaded rather than appended to, and `sparkbox
# set-port` just re-asserts the route. Re-run freely.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."

# 1. Dependencies — whatever this project actually declares.
pnpm install

# 2. The dev server as a systemd --user unit: it survives an SSH disconnect,
#    restarts itself, and its output is one `journalctl --user -u web -f`
#    away instead of living in a detached terminal nobody can find again.
mkdir -p ~/.config/systemd/user
cat > ~/.config/systemd/user/web.service <<UNIT
[Unit]
Description=web dev server

[Service]
WorkingDirectory=$(pwd)
ExecStart=/usr/bin/env pnpm dev --host 0.0.0.0
Restart=on-failure

[Install]
WantedBy=default.target
UNIT
systemctl --user daemon-reload
systemctl --user enable --now web.service

# 3. Wire the domain through: this VM's default HTTPS endpoint should open
#    the thing a person should see, not whichever service happened to bind
#    first.
sparkbox set-port 5173

# 4. Anything else worth finding this session by later, since only the
#    default port is discoverable from the URL bar. The domain is this
#    deployment's own, not a constant, so it is read rather than assumed.
DOMAIN=$(sparkbox whoami | sed -n 's/^domain: //p')
hivemind tag api_url="https://$(hostname).$DOMAIN:8080"
