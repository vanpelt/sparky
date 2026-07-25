# Onboarding notes — toward `sparkbox setup` as the only entry point

Running log from the v0.4.0 fleet rebuild (2026-07-25): tear down the
hand-provisioned DGX gateway and the standalone macOS PoC, then re-onboard both
from the release — `sparky` as the fleet gateway, the laptop as a node named
`laptop`.

**Goal of the exercise.** A user should provision any supported host by
downloading one released `sparkbox` binary and running `sparkbox setup`. Every
shell script, hand-edited unit and out-of-band CLI we touch during this rollout
is a gap against that goal. Part 1 is the ledger of those gaps; Part 2 is
friction we hit even where the binary *is* the entry point.

---

# Part 1 — what still runs outside the released binary

## 1.1 macOS: the entire outer layer

`macos/poc.sh` (757 lines) + `macos/kernel/build.sh` (172) +
`macos/smoke.sh` (236) + `macos/gateway-verify.sh` are all host-side bash. The
released binary only enters at the very last step, *inside* the guest.

| Step | Today | Wants to be |
| --- | --- | --- |
| Host doctor (macOS ≥15, Apple Container ≥1.1.0, disk, machine-name ownership) | `poc.sh doctor` | `sparkbox doctor` on darwin |
| Outer KVM kernel | `kernel/build.sh` — downloads Linux 6.14.9 + Apple `config-arm64` 0.5.0, applies `sparkbox-arm64.fragment`, compiles in an Ubuntu container | a **released artifact** (`vmlinux-macos-arm64`), fetched + checksum-verified like every other asset |
| Outer machine image | `container build` of `Containerfile.gateway`, which compiles sparkbox **from source** (needs the git repo + a Go toolchain) | a released/GHCR image, or one assembled from the released `sparkbox-linux-arm64` |
| Machine lifecycle (`create/start/stop/destroy`) | `container machine …` shellouts | `sparkbox setup` driving the same CLI (or Virtualization.framework) from Go |
| Exec transport | `container machine run --root --interactive /bin/bash -s` heredocs | a Go exec helper |
| Inner provisioning | `sparkbox setup` ✅ | unchanged |
| Verification | `smoke.sh` + `gateway-verify.sh` | `sparkbox doctor` levels |

**Blocker:** the release publishes **no darwin binary at all** —
`build-artifacts.yml` builds only `linux/{amd64,arm64}`. `sparkbox setup` cannot
be the macOS entry point until `sparkbox-darwin-arm64` ships. That is the first
piece of work.

**Second blocker:** building a kernel from source on a user's laptop is not an
onboarding step anyone will tolerate. The outer kernel is pinned and
reproducible (SHA-256
`7bb865dfc2dfb6578d41a9fb2d044299c626377ff69c540b15108afb75dd080c`) — it belongs
in the release, built once in CI.

## 1.2 Linux: subsystems `setup` doesn't install

`setup` installs `sparkbox.service`, `sparkbox-net.service`, `sparkbox-net.sh`
and the sysctl drop-in. Everything below was installed on the DGX by hand and
has no `setup` story:

| Component | What it is | Where it lives now |
| --- | --- | --- |
| **sluice** | per-VM egress control (`/run/sluice.sock`, `sluice.env`, `allowlist.txt`) — the gateway is started with `--guest-dns 172.30.0.53 --sluice-socket …` and silently loses egress filtering without it | `tools/sluice`, hand-installed |
| **agent tooling** | `sparkbox-refresh-tools.sh` → `/srv/sparkbox/tools/{claude,codex,hivemind}` + `versions.env`; what makes a sandbox useful | `deploy/install-host-tooling.sh` |
| **guest identity** | `sparkbox-install-guest-identity.sh` | `deploy/install-guest-identity.sh` |
| **cloudflared** | public `*.catnip.sh` tunnel | hand-configured |
| **tailscale + the edge /32** | `10.66.0.1` dedicated tailnet IP the edge binds; split-DNS via the Tailscale API | hand-configured, `docs/dedicated-edge-ip-cutover.md` |

`sluice` is the one that feels core: a gateway without it is a gateway with no
egress control, and nothing in `setup` says so.

## 1.3 Release-pipeline gaps implied by the above

- no `sparkbox-darwin-arm64` asset
- no `vmlinux-macos-arm64` (outer kernel) asset
- no macOS outer-machine image
- the `Containerfile.gateway` build path duplicates the release build instead of
  consuming it

---

# Part 2 — friction where the binary *is* the entry point

## F1 — `setup` can't bind the edge to a specific address

`deploy/units/sparkbox-standalone.service.tmpl` +
`internal/hostsetup/assets.go:renderService` hardcode `--ssh-addr :2222` (or
`:22` with `--move-admin-ssh`) and `--proxy-addr :8081`
(`config.go:proxyPort`). The DGX gateway binds **`10.66.0.1`** — a dedicated
tailnet /32 — for both, so any-port DNATs key off the destination IP and can't
collide with host services. There is no flag or env var for it.

It *is* reachable, but only by accident: `$OVERCOMMIT_FLAGS`, `$TLS_FLAGS` and
`$GATEWAY_FLAG` are appended **after** the hardcoded flags, and Go's `flag`
package lets a repeated flag win last. So `--ssh-addr 10.66.0.1:2222` stuffed
into `TLS_FLAGS` works — while reading like a mistake.

Worse, `--api-addr 127.0.0.1:8080` is hardcoded with **no** bundle after it that
could override... except the same trailing bundles, by the same accident. And
`:8080` is a genuinely bad default on a workstation-class host: on the DGX it is
already held by an unrelated python process, so a fresh `setup` there produces a
gateway whose REST API silently loses a port race at boot. The live host uses
`:8079`.

**Fix:** template `SSHAddr`/`ProxyAddr`/`APIAddr` off real `setup` flags
(`--ssh-addr`, `--proxy-addr`, `--api-addr`), or add an honestly-named
`EXTRA_FLAGS` bundle. Also probe the chosen ports during `setup` preflight and
fail loudly rather than at first boot.

## F2 — whole subsystems have no `setup` flags

Flags on the live DGX unit that `setup` never emits and never mentions:
`--dns-addr` / `--dns-answer` (dnsedge), `--archive-remote` / `--archive-bucket`
(R2), `--guest-dns` (sluice resolver), `--sluice-socket`,
`--ssh-advertise-port` (bare `ssh ctl@host`), `--tls-provider cloudflare`
(DNS-01 wildcard). A fresh `setup` yields a gateway that runs but has no egress
control, no archiving, no DNS edge, and advertises the wrong SSH port.

## F3 — `poc.sh` forces a destroy to change a machine's role

`macos/poc.sh:450` refuses to reprovision when `GATEWAY_FLAG` differs:
`existing machine role/config differs …; destroy and recreate before switching
roles`. So turning the standalone macOS gateway into a fleet node destroys the
machine and its sandboxes, even though the underlying `setup` is idempotent and
the only real change is one line in `sparkbox.env`. Defensible as a guard (a
gateway's users DB and secrets are meaningless on a node) but it should say why
and allow the in-place path when the machine holds no state.

## F4 — legacy layout vs `setup` layout diverge silently

The DGX predates `sparkbox setup`: flat
`/srv/sparkbox/{state,images,vmlinux,users.conf}`. `setup` lays down
`<root>/data/state` + `<root>/data/images` on an XFS reflink volume. Nothing
detects or migrates the old shape — re-running `setup` over it provisions a
*second*, empty gateway beside the live one's data. Should adopt or refuse.

## F5 — certmagic is the real teardown hazard

Wiping a gateway drops `state/certmagic/`, and `*.catnip.sh` is DNS-01 issued
under Let's Encrypt's 5-duplicate-certs-per-week cap. Nothing warns you.
Preserving `state/certmagic/` across a rebuild is cheap and should be the
documented default.

## F6 — the README doesn't know about fleets

`README.md` has no mention of nodes, gateways or federation after #15 and #16.
Onboarding a second machine means reading `docs/multi-node-implementation.md`, a
1000-line implementation blueprint, to discover
`sparkbox setup --gateway host:port --node-name x`.

---

# Work ranking

1. **Ship `sparkbox-darwin-arm64`** — nothing else in Part 1 can start without it.
2. **Ship the pinned macOS outer kernel as a release artifact** — kills the
   from-source kernel build on the user's laptop.
3. **Port `poc.sh` into `sparkbox setup` on darwin** — doctor, machine
   create/start/stop/destroy, provisioning, in that order.
4. **F2 / sluice** — core subsystems silently absent after `setup`.
5. **F1** — edge bind address; the `TLS_FLAGS` workaround is a trap.
6. **F4** — legacy-layout detection.
7. **F6** — README fleet quick-start (cheap, high leverage).
8. **F5**, **F3** — teardown safety and the role-switch guard.
