# Terminal over HTTPS (exe-ssh-compatible) — design

## Problem

Getting an interactive shell into a sandbox today requires the **sparkbox SSH
gateway on `:2222`**, reachable only over the tailnet. That is fine for the
operator, but the goal is a **default that works for anyone, with no tailscale,
no VPN, no bastion, and no open inbound port 22** — the box lives behind home NAT
and the only public ingress is the Cloudflare Tunnel, which is HTTP(S) only.

The options that expose raw TCP/22 (a public bastion, a router port-forward,
Tailscale Funnel, Cloudflare Spectrum) each fail one of those constraints:
Funnel and the HTTP tunnel are L7-only, a bastion/port-forward reintroduces
public inbound infra, Spectrum is Enterprise-priced.

## Approach: carry the shell over the HTTPS edge we already have

Boldsoftware's **[exe-ssh](https://github.com/boldsoftware/exe.dev/tree/main/exe-ssh)**
(same lineage as the exeuntu image our guest is derived from) solves exactly this:
an *"SSH-like terminal that connects over HTTPS rather than port 22"*. A shell is
just a PTY; a PTY multiplexes cleanly over a **WebSocket**, and WebSockets ride
straight through cloudflared. So the "SSH front door" becomes a reserved path on
the web edge we already run — `https://<name>.<domain>/…` — reusing the tunnel,
the wildcard cert, and the sparkbox user/key model. No new ingress, no new port.

Crucially, **the exe-ssh client is reusable as-is** (its base domain is
configurable), so if sparkbox implements the *server* side to exe-ssh's wire
protocol, users get a maintained, `uvx`-installable client for free:

```
uvx --from 'exe-ssh @ git+https://github.com/boldsoftware/exe.dev#subdirectory=exe-ssh' \
  exe-ssh dazzling-canyon.catnip.sh
```

The only client dependency is `uv` — which our guest image already standardizes
on. Auth is the user's **existing SSH key**, so it stays "SSH-like": same keys as
`users.conf`, just carried over TLS instead of TCP/22.

## Wire protocol (as implemented by the exe-ssh client)

Reverse-engineered from `exe-ssh/src/exe_ssh/cli.py`; sparkbox must serve this
exactly to stay client-compatible.

**Endpoints** (on the box host, `<name>.<base_domain>`, port 443):
- `GET /terminal/ws/experimental-exe-ssh?name=<session>` — WebSocket upgrade; the PTY stream.
- `GET /terminal/sessions` — JSON `{"sessions":[{"name":"…"}]}` for listing/reconnect.

**Auth** — a short-lived, SSH-signed bearer token (no public-key upload, no password):
1. Client builds payload `{"exp": <now+300>}`.
2. Signs it: `ssh-keygen -Y sign -f <key> -n <namespace>`, where
   `namespace = v0@<host>` (e.g. `v0@dazzling-canyon.catnip.sh`).
3. Sends `Authorization: Bearer exe0.<b64url(payload)>.<b64url(signature)>`.

Server verifies: parse the `exe0.` token, check `exp`, then verify the SSH
signature (`ssh-keygen -Y verify` / an allowed-signers check) against the
**account keys in `users.conf`**, binding the namespace to this host so a token
minted for one box can't be replayed against another.

**Framing** (JSON over WS):
- client→server: `{"type":"input","data":"<utf8>"}`, `{"type":"resize","rows":N,"cols":N,"term":"<TERM>"}`
- server→client: `{"type":"output","data":"<base64>"}`
- close code **4001** = shell exited normally.

## Server architecture

Put the handler in the **sparkbox proxy edge** (host-side), not the guest —
that's where auth (`users.conf`), sandbox ownership, and the gateway's
SSH-into-guest path already live. Flow for a connection to
`<name>.<domain>/terminal/ws/…`:

1. **Reserve the `/terminal/` path** on sandbox web routes, intercepted *before*
   the normal proxy-to-guest step (the same way `console`/`oidc`/`login` are
   reserved today). This path uses its own token auth and **bypasses** the
   private-by-default web-route gate (`authorize()`), because the SSH-signed
   bearer is the authenticator here.
2. **Verify the token** → resolves to an account (a `users.conf` handle).
3. **Authorize** → that account must own `<name>` (or the sandbox is shared to
   them). Reuse the existing owner/`share` model.
4. **Ensure running** → `mgr.EnsureRunning(name)` (cold-resume if archived/paused,
   exactly like a web hit does today).
5. **Open a PTY in the guest.** sparkbox already dials the guest as the login
   user over the gateway upstream key (`internal/sshgw`). Reuse that: request a
   PTY (`RequestPty` + `Shell`) on an `x/crypto/ssh` session into the sandbox.
6. **Bridge** WS ↔ PTY: decode `input` to the PTY stdin, `resize` →
   `session.WindowChange`, PTY stdout → base64 `output` frames; on shell exit
   send close **4001**.

Nothing new is exposed publicly — it's one more handler on the port cloudflared
already reaches (`127.0.0.1:8091`).

## Persistent named sessions

exe-ssh advertises named, reconnectable sessions. Cheapest correct
implementation: run the guest PTY inside **`tmux new -A -s <session>`** (attach-or-create).
Reconnect with the same `name` reattaches the live session; the process tree
survives a dropped WebSocket and even a client restart (though not a guest
cold-boot — same caveat as any in-guest process). `/terminal/sessions` maps to
`tmux list-sessions`. Default session name when the client omits one: a stable
value like `main`.

## Routing & naming caveat

exe-ssh turns a **bare** name into `<name>.xterm.<base_domain>`, but our wildcard
`*.catnip.sh` is **single-label** — it does *not* cover `<name>.xterm.catnip.sh`.
Two ways out:
- **(preferred)** Have users pass the **fully-qualified** host (`dazzling-canyon.catnip.sh`);
  the client hits `/terminal/ws/…` on it directly, and we serve the path there.
  Namespace becomes `v0@dazzling-canyon.catnip.sh` — verify against that.
- Or add a second wildcard `*.xterm.catnip.sh` (CNAME → tunnel) + ingress, to
  support the bare-name ergonomics. More DNS, buys only a shorter command.

Start with the FQDN form; it needs zero new DNS.

## Security

- **Short TTL** (300s) + **host-bound namespace** blunt replay; consider a
  server-side nonce/jti cache if we want single-use.
- **Authorization is ownership**, not mere key validity — a valid signature from
  a `users.conf` key still must map to a handle that owns/was-shared the sandbox.
- TLS terminates at Cloudflare; the tunnel hop is cloudflared→localhost. Bind the
  terminal listener to localhost (already true for `:8091`).
- Audit-log every session open (account, sandbox, src) like the SSH gateway does.

## Client story

- **MVP:** document `uvx exe-ssh <name>.catnip.sh`. Nothing to build client-side.
- **Later (optional):** a thin `sparkbox ssh <name>` wrapper (or our own `uvx`
  package) that fills in the base domain and picks the key via `ssh -G`, so users
  type a sandbox name, not a URL.

## Phasing

1. **M1 — one shot:** token verify + WS-PTY bridge into the guest, single
   ephemeral session, FQDN routing. Proves `uvx exe-ssh dazzling-canyon.catnip.sh`
   lands a real shell over the tunnel.
2. **M2 — sessions:** tmux-backed named sessions + `/terminal/sessions` + reconnect.
3. **M3 — ergonomics:** `xterm.` wildcard for bare names and/or a sparkbox client
   wrapper; a `ctl@ expose <name> <sub> <port>` command (also closes the
   unrelated tunnel-mode gap where extra ports need an operator API call).

## Open questions

- Reuse exe-ssh's `exe0.`/`v0@` token scheme verbatim (client-compat) vs. fold
  into sparkbox's existing SSH-key session-token — leaning **verbatim** so the
  upstream client Just Works.
- Where PTY resize/`TERM` and scrollback should live if we tmux-wrap (client TERM
  vs tmux TERM).
- Idle/lifetime policy for tmux sessions vs sparkbox's idle-pause (a detached but
  live tmux shouldn't by itself pin a sandbox warm).
