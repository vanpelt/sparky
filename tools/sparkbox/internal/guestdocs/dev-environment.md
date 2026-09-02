# Running a dev server behind the Sparkbox proxy

The edge terminates TLS and forwards plain HTTP — WebSocket upgrades included —
to a port inside the VM; see [the proxy guide](./proxy.md) for how a URL picks
that port. Two things a dev server takes for granted on a laptop are false
here: the browser's Host header is `<name>.<domain>`, not `localhost`, and both
the box's hostname and the domain are different on every deployment — `<name>`
is `$(hostname)`, and `<domain>` below is whatever `sparkbox whoami` reports on
its `domain:` line, never a literal to hardcode.

## Bind to 0.0.0.0

Same rule as the proxy guide: listen on `0.0.0.0` (or a framework's
equivalent, often `--host` or `HOST=0.0.0.0`), not the default `127.0.0.1` —
or nothing outside the VM can reach it.

## Allow the proxy's Host header

Most dev servers reject requests whose Host header they do not recognize —
correctly, since that check is what stops a malicious page from driving your
dev server through DNS rebinding. Sparkbox's Host header is real but is never
`localhost`, so it needs adding explicitly. Match the whole domain, not one
VM's name, since the same fix has to keep working after `sparkbox snapshot` or
a plain `git clone` lands it on a different box:

- **Vite**: `server.allowedHosts: ['.<domain>']` in `vite.config.*` — a
  leading dot matches the domain and every subdomain.
- **Next.js**: `allowedDevOrigins: ['*.<domain>']` in `next.config.*`.
- **Django**: `ALLOWED_HOSTS = [".<domain>"]`, and — separately, since
  Django checks `Origin` against a different list — `CSRF_TRUSTED_ORIGINS =
  ["https://*.<domain>"]` for anything that POSTs, including the admin.
- **Rails**: `config.hosts << ".<domain>"` in
  `config/environments/development.rb`.
- **webpack-dev-server / Create React App**: `allowedHosts: ['.<domain>']`
  in the dev-server config. `DANGEROUSLY_DISABLE_HOST_CHECK=true` is a last
  resort for a config you cannot easily reach — it is exactly as dangerous as
  the name says, so revert it once the real fix is in.

## Hot reload and other websockets

The edge passes WebSocket upgrades straight through, and for one route the
port the browser connects to and the port a dev server's own client script
assumes are already the same number — so a client that derives its host and
protocol from the page it was served on (Vite's and webpack's HMR clients do,
by default) needs no override. If reload silently never fires, check the
browser's Network tab for the WS connection first: a client hardcoded to
`ws://localhost:<port>` instead of the page's own origin is the one case that
still needs one, e.g. Vite's `server.hmr: { protocol: 'wss' }`.

## Point the default route at it

Once it is listening, run `sparkbox set-port PORT` so the endpoint a person
opens is the one they should actually look at — the frontend of a stack, not
whichever service happened to bind first. Record any other port with
`hivemind tag`, for example:

```sh
DOMAIN=$(sparkbox whoami | sed -n 's/^domain: //p')
hivemind tag api_url="https://$(hostname).$DOMAIN:8080"
```

## Turn the start command into a service

A command run in a foreground shell dies with the SSH session. Wrap it in a
`systemd --user` unit instead — the login user carries linger, so `--user`
units keep running unattended, restart themselves, and their output is one
`journalctl --user -u NAME -f` away instead of living in a terminal nobody can
get back to:

```sh
mkdir -p ~/.config/systemd/user
cat > ~/.config/systemd/user/web.service <<'EOF'
[Unit]
Description=web dev server

[Service]
WorkingDirectory=%h/myproject
ExecStart=/usr/bin/env pnpm dev --host 0.0.0.0
Restart=on-failure

[Install]
WantedBy=default.target
EOF
systemctl --user daemon-reload
systemctl --user enable --now web.service
```

## Make it replayable: `.sparkbox/setup.sh`

Everything above — installing dependencies, writing the unit, calling
`sparkbox set-port` — is worth doing once and writing down, not re-deriving
from scratch on the next VM or the next teammate's checkout. Commit it to the
project's own repo as `.sparkbox/setup.sh`: a plain, idempotent shell script
that a fresh checkout can run with `bash .sparkbox/setup.sh` to arrive at the
same running state.

This is deliberately a script, not a Sparkbox feature — `sparkbox snapshot`
already freezes a whole VM disk (see the docs), and that is the right tool
when the setup is genuinely expensive to repeat. A script is the better
default: it is readable in a diff, works on a VM that never forked from a
snapshot, and survives changes to the setup being ordinary commits instead of
a new disk image each time.

```sh
{{SETUP_SH_EXAMPLE}}
```

Read a `.sparkbox/setup.sh` you did not write before running it — like any
script, it runs as you.
