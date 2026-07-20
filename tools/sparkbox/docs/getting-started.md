# Getting started: stand up a sparkbox host

This is the operator quickstart for running your own sparkbox on **any Linux
host with KVM** — a bare-metal box, a nested-virt cloud instance, a DGX, a
homelab server. The `sparkbox` binary is its own installer: point it at a host
and it fetches a prebuilt release, lays down the storage and networking, wires
systemd, and starts the gateway.

```
sparkbox doctor        # is this host ready?
sparkbox setup         # provision it into a running gateway
```

For a provider-managed, zero-touch fleet on Scaleway Elastic Metal, see
[deploy-scaleway.md](deploy-scaleway.md) instead. To just kick the tires with no
VM at all, jump to [Local dev](#local-dev-no-kvm) below.

## 0. What you need

- A **Linux host with hardware virtualization** — `/dev/kvm` must exist and the
  CPU must expose `vmx` (Intel) or `svm` (AMD). Bare metal, or a cloud instance
  with nested virtualization enabled. `sparkbox doctor` checks this for you.
- **Root** on that host (firecracker needs `/dev/kvm` and tap devices).
- Ideally a **wildcard DNS record** (`*.yourdomain`) pointed at the host, if you
  want web routes and HTTPS. You can add this later.
- An **SSH public key** — the operator identity. `setup` auto-detects
  `~/.ssh/id_ed25519.pub`, or pass one explicitly.

Outbound HTTPS to the artifact bucket is required during `setup` (it downloads
the guest kernel, the firecracker binary, and the rootfs template).

## 1. Get the binary onto the host

Build it from this repo:

```sh
cd tools/sparkbox
go build -o sparkbox ./cmd/sparkbox
scp sparkbox root@<host>:/usr/local/bin/sparkbox
```

(Or download the `sparkbox` binary from your artifact release. `setup` itself
fetches the *kernel*, *firecracker*, and *rootfs* — it does not need to build
anything on the host.)

## 2. Preflight

```sh
ssh root@<host>
sparkbox doctor
```

`doctor` reports **PASS / WARN / FAIL** for every prerequisite — KVM, CPU
virtualization, IP forwarding, the guest kernel, the rootfs template, the fleet
keys, `users.conf`, disk space, the NAT rules, and whether the service is
running — each with a one-line fix. Before provisioning, the two that must pass
are **kvm device** and **hardware virtualization**; everything else `setup`
puts in place. It exits non-zero if anything FAILs, so it drops into CI too.

## 3. Provision

```sh
sudo sparkbox setup --proxy-domain yourdomain.tools
```

That runs the full pipeline (preview any time with `--dry-run`):

1. **swapfile** — a 16 GiB overcommit safety valve.
2. **resolve-release** + **fetch-artifacts** — download and **sha256-verify**
   the guest kernel, firecracker, and the rootfs template from the release
   bucket, decompressing the rootfs in place.
3. **data-volume** — a 300 GiB **XFS reflink** volume at `/srv/sparkbox/data`,
   so each sandbox's rootfs is an instant copy-on-write clone.
4. **users.conf** — seed your operator SSH key.
5. **host-config** — write `/srv/sparkbox/sparkbox.env`.
6. **net-rules** — install the packet-filter script + sysctls (IP forwarding,
   strict rp_filter, sandbox NAT, any-port edge REDIRECT).
7. **systemd-units** — install `sparkbox.service` + `sparkbox-net.service`.
8. **enable-services** — start everything, then re-run the health checks.

Every step is idempotent: re-running `setup` fixes only what drifted, and an
already-verified artifact is not re-downloaded.

Useful flags:

| Flag | Default | Purpose |
|---|---|---|
| `--proxy-domain` | `hivemind.tools` | base domain for `<name>.<domain>` web routes |
| `--operator-key` | auto-detect `~/.ssh/*.pub` | path to, or literal text of, your SSH public key |
| `--move-admin-ssh` | off | relocate the host's own sshd to **:2222** so the gateway owns **:22** (see below) |
| `--release` | `latest` | pin a specific artifact release tag |
| `--artifact-base` | the public bucket | point at your own release mirror |
| `--data-volume-gb` / `--swap-gb` | 300 / 16 | size the data volume / swapfile |
| `--dry-run` | off | print the plan and change nothing |

### Port 22 and the SSH gateway

By default the gateway binds **:2222** and leaves the host's own sshd on :22, so
`setup` can never lock you out. You connect with `ssh -p 2222 new@<host>`.

For the clean `ssh new@<host>` experience, re-run with **`--move-admin-ssh`**:
the host's admin sshd moves to :2222 and the gateway takes :22. This evicts the
listener on :22, so **keep a second shell open** the first time in case you need
to reconnect on :2222.

## 4. Connect

```sh
ssh -p 2222 new@<host>          # create a sandbox; it prints the name
ssh -p 2222 <name>@<host>       # resume + shell in (auto-resumes if paused)
ssh -p 2222 ctl@<host> help     # account/keys/routes/schedule controls
```

Words after `new@` (or `new+<name>@`) are **tags**, not a command — a new
sandbox always gets a shell. Passing any word also stops ssh from allocating a
terminal, which leaves you in a shell with no prompt, so add `-t`:

```sh
ssh -t -p 2222 new+foo@<host> claude   # sandbox `foo` tagged `claude`
```

Web routes are live at `http://<name>.<proxy-domain>:8081` immediately. For a
public **HTTPS** edge, add a wildcard DNS record and turn on TLS (next section).

## 5. Day-2

- **HTTPS edge.** Point `*.yourdomain` at the host, then set `TLS_FLAGS` in
  `/srv/sparkbox/sparkbox.env` and restart:
  ```sh
  # on-demand Let's Encrypt (needs :443 + :80 reachable):
  TLS_FLAGS=--proxy-addr :443 --proxy-tls --tls-provider autocert --tls-email you@example.com
  ```
  For a wildcard cert via Cloudflare DNS-01 (no inbound :80/:443 needed), see
  [deploy-dns.md](deploy-dns.md). Restart with `systemctl restart sparkbox`.
- **Operator console.** Uncomment `SPARKBOX_CONSOLE_PASSWORD` in `sparkbox.env`
  (run it under TLS) to get `console.<domain>`.
- **IPv6 per sandbox.** With a routed `/64`, set `SUBNET6` + `SUBNET6_FLAG` in
  `sparkbox.env`; see the README's IPv6 section.
- **Add users.** Operators mint invite codes (`ssh -p 2222 ctl@<host> invite`);
  newcomers register with `ssh signup@<host>`.
- **Health + logs.** `sparkbox doctor` any time; `journalctl -u sparkbox -f`.
- **Re-provision / upgrade.** Drop in a newer `sparkbox` binary and re-run
  `sparkbox setup` — idempotent, and `--release <tag>` pins the artifacts.

## Local dev (no KVM)

To develop against the full control plane with no VM isolation, use the **mock
driver** — it emulates each sandbox as an in-process SSH server, so it runs on a
laptop:

```sh
go build ./cmd/sparkbox
ssh-keygen -t ed25519 -f mykey -N ''
echo "me $(cat mykey.pub)" > users.conf
./sparkbox serve --driver mock --state-dir ./state --users users.conf
# elsewhere:
ssh -i mykey -p 2222 new@localhost
```

`go test ./...` runs the end-to-end suite over exactly this stack.
