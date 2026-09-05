#!/usr/bin/env bash
# .sparkbox/setup.sh — replays this VM's dev-environment setup.
#
# Runs tools/sparkbox/hack/preview-console.py: a dummy frontend that serves
# the real sparkbox user-console HTML with its fetch() calls stubbed out to a
# mock backend baked into the page, so the whole console UI renders with no
# real gateway behind it. Stdlib-only, no install step needed.
#
# Idempotent: safe to re-run on an already-set-up VM.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."

# preview-console.py binds $HOST (default 127.0.0.1); the systemd unit below
# sets it to 0.0.0.0 so the Sparkbox proxy can reach it.

mkdir -p ~/.config/systemd/user
cat > ~/.config/systemd/user/sparky-web.service <<UNIT
[Unit]
Description=sparkbox dummy console (mock backend)

[Service]
WorkingDirectory=$(pwd)/tools/sparkbox/hack
Environment=HOST=0.0.0.0
ExecStart=/usr/bin/env python3 preview-console.py console 8081
Restart=on-failure

[Install]
WantedBy=default.target
UNIT

export XDG_RUNTIME_DIR="${XDG_RUNTIME_DIR:-/run/user/$(id -u)}"
systemctl --user daemon-reload
systemctl --user enable --now sparky-web.service

sparkbox set-port 8081
