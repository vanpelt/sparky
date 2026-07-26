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

| Step | Today | Wants to be | Status |
| --- | --- | --- | --- |
| Host doctor (macOS ≥15, Apple Container ≥1.1.0, disk, machine-name ownership) | `poc.sh doctor` | `sparkbox doctor` on darwin | **closed — B5** |
| Outer KVM kernel | `kernel/build.sh` — downloads Linux 6.14.9 + Apple `config-arm64` 0.5.0, applies `sparkbox-arm64.fragment`, compiles in an Ubuntu container | a **released artifact** (`vmlinux-macos-arm64`), fetched + checksum-verified like every other asset | **closed — B2** |
| Outer machine image | `container build` of `Containerfile.gateway`, which compiles sparkbox **from source** (needs the git repo + a Go toolchain) | a released/GHCR image, or one assembled from the released `sparkbox-linux-arm64` | **half closed — B3**: the image no longer compiles anything from source and needs neither the repo nor a Go toolchain, but it is still built locally on every Mac. No image is *published*; see the still-open bullet in §1.3 |
| Machine lifecycle (`create/start/stop/destroy`) | `container machine …` shellouts | `sparkbox setup` driving the same CLI (or Virtualization.framework) from Go | **half closed — B4**: `create`/`start` run from Go behind one driver (still `container` shellouts). `stop`/`destroy` have no Go home — there is no `sparkbox machine` subcommand, so every error message punts to `container machine delete` and the cost-tiered teardown lives only in `poc.sh destroy` |
| Exec transport | `container machine run --root --interactive /bin/bash -s` heredocs | a Go exec helper | **closed — B4** (`internal/machine`) |
| Inner provisioning | `sparkbox setup` ✅ | unchanged | unchanged |
| Verification | `smoke.sh` + `gateway-verify.sh` | `sparkbox doctor` levels | **L1 closed, L2 open** — the darwin verify battery covers `gateway-verify.sh`'s assertions (nested virt, machine kernel, `/dev/kvm`, TUN, no `/Users` virtiofs mount) and relays the gateway's own doctor out of the machine. `smoke.sh`'s L2 — SSH into a real sandbox, in-guest DNS/HTTPS, the metadata cross-slot refusal, a published HTTP route — has no Go equivalent |
| Darwin release binary | nothing — `build-artifacts.yml` built only `linux/{amd64,arm64}` | `sparkbox-darwin-arm64` + `manifest-darwin-arm64.env` on every release | **closed — B1** |

~~**Blocker:** the release publishes **no darwin binary at all**.~~ **Closed by
B1.** Every release now publishes `sparkbox-darwin-arm64`,
`manifest-darwin-arm64.env` and `vmlinux-macos-arm64`, and the release is not
published until all of them land, so `latest` can never resolve half-populated.

### What is genuinely still open

1. **Nothing here is proven on real hardware by CI, and cannot be.** GitHub's
   hosted macOS runners have no Apple `container` CLI and cannot nest VMs. The
   `macos` job in `go.yml` builds and runs the full unit suite on real Apple
   Silicon — which is more than a cross-compile can do, since a cross-compiled
   test binary cannot be executed — but every machine-lifecycle test there
   drives an in-memory fake of the `container` CLI. The first real
   `sparkbox setup` on a Mac is an operator running it on their own laptop.
   Until that has happened, treat B4 as written-and-unit-tested, not proven.
2. **`macos/poc.sh` is still the only thing that has ever created a real
   machine.** It stays in the tree as the fallback and as the reference
   behaviour B4 was ported from; see its header for which subcommands are now
   redundant. It is deliberately not deleted in the same pass that replaced it.
3. **L2 verification has no Go equivalent.** `smoke.sh` exercises the things
   only a booted sandbox can show — SSH in, in-guest DNS and HTTPS, the
   metadata endpoint's cross-slot refusal, a published HTTP route answering
   through the edge. `sparkbox doctor` on darwin reports the machine and relays
   the gateway's own doctor; it does not boot a sandbox.
4. **The two provisioners still coexist** (`cloud-init` vs `sparkbox setup` on
   Linux, `poc.sh` vs `sparkbox setup` on darwin), which is the same dual-path
   hazard Part 2 records for the Linux side.

**Second blocker (FIXED — B2):** building a kernel from source on a user's
laptop is not an onboarding step anyone will tolerate. The outer kernel's inputs
are all pinned (Linux 6.14.9 and Apple's `config-arm64` by SHA-256, the builder
image by digest), so it is now built once in CI on a native arm64 runner and
published as `vmlinux-macos-arm64`; `macos/kernel/fetch.sh` downloads it and
verifies it against `SHA256_OUTER_KERNEL` in `manifest-darwin-arm64.env`.
`macos/kernel/build.sh` survives as the escape hatch
(`SPARKBOX_KERNEL_SOURCE=build`).

That checksum is an **integrity** claim, not an identity one, and the earlier
wording here — "pinned and reproducible", with
`7bb865dfc2dfb6578d41a9fb2d044299c626377ff69c540b15108afb75dd080c` quoted as
though anyone could re-derive it — was wrong. `build.sh` installs its toolchain
with an unpinned `apt-get install build-essential`, and gcc's and binutils'
version strings are compiled into the kernel banner
(`CONFIG_CC_VERSION_TEXT="gcc (Ubuntu 13.3.0-6ubuntu2~24.04.1) 13.3.0"`), so a
packaging-only Ubuntu revision changes the bytes with no change in behaviour.
What CI gates on instead is determinism *within* a toolchain: on a `v*` tag it
builds twice at different `-j` and fails if the hashes differ. The known-good
hash is kept as a witness that reports drift, not as a gate.

## 1.2 Linux: subsystems `setup` doesn't install

`setup` installs `sparkbox.service`, `sparkbox-net.service`, `sparkbox-net.sh`
and the sysctl drop-in. Everything below was installed on the DGX by hand and
has no `setup` story:

| Component | What it is | Where it lives now |
| --- | --- | --- |
| **sluice** | per-VM egress control (`/run/sluice.sock`, `sluice.env`, `allowlist.txt`) — the gateway is started with `--guest-dns 172.30.0.53 --sluice-socket …` and silently loses egress filtering without it | **fixed**: `setup --sluice` installs it from the release |
| **agent tooling** | `sparkbox-refresh-tools.sh` → `/srv/sparkbox/tools/{claude,codex,hivemind}` + `versions.env`; what makes a sandbox useful | **fixed**: `setup` installs the script, its daily timer, and bakes the tools (`--agent-tools`, on by default) |
| **guest identity** | `sparkbox-install-guest-identity.sh` | **fixed**: installed by the same step; the refresher calls it by its installed path |
| **cloudflared** | public `*.catnip.sh` tunnel | hand-configured |
| **tailscale + the edge /32** | `10.66.0.1` dedicated tailnet IP the edge binds; split-DNS via the Tailscale API | hand-configured, `docs/dedicated-edge-ip-cutover.md` |

**Agent tooling closed next, and it was worse than a gap — it was a wrong
answer.** The two rows above are now one `setup` step (`stepAgentTools`,
`--agent-tools`, on by default), and closing them turned up the bug that
motivated it. The refresher decided whether templates were current from a
host-side stamp file, `$TOOLS_DIR/versions.env`, and exited without opening a
single template. On the DGX the v0.4.0 upgrade fetched a fresh `universal.ext4`
at 12:43 over a stamp written at 00:38; every run after that printed "templates
already current" and every sandbox created from that template had no `claude`,
no `codex` and no `hivemind`. `--force` existed and was documented — which means
correctness depended on an operator remembering an invariant the script was in a
position to check.

The stamp now lives **inside** each template at `/etc/sparkbox/tools-rev`, read
per template with `debugfs` (no mount, no loop device, read-only, so it is safe
against a template being reflinked from right now). Both layers ask the same
file: the script patches what does not match, `setup`'s `Satisfied` refuses to
call the step done over a bare template, and `doctor` gained an `agent tooling`
line that WARNs rather than staying green. An unreadable template counts as
stale on purpose — a needless re-patch costs a minute, a wrong "current" costs
every sandbox made that day and says nothing.

The bake is also the one step in the pipeline whose failure is **not** fatal: it
pulls several hundred MB from three third-party release channels, and a hiccup
there must not undo a provisioning run whose gateway, network and units are all
correct. It warns, names the retry, and the daily timer picks it up.

`sluice` was the one that felt core, and it is now installable: a release
publishes `sluice-linux-<arch>` with `SHA256_SLUICE` in the manifest, and
`sparkbox setup --sluice` fetches it, seeds `allowlist.txt` and `sluice.env`,
renders `sluice.service` and enables it — implying `--sluice-socket` and
`--guest-dns` so the daemon and the gateway cannot end up installed-but-not-
talking. It stays **opt-in**: turning egress filtering on changes what running
sandboxes can reach, so the unfiltered default remains, and `doctor` plus the
`setup` banner both say so in words.

Two things settled before shipping a prebuilt binary, because a wrong answer
here is an asset that installs cleanly and fails to load on somebody's kernel:

- **No eBPF toolchain is needed.** `internal/meter/sluice_bpfel.o` is committed
  and `//go:embed`-ed, so the whole tool is one `go build` — no clang, no
  bpf2go, no bpftool, no kernel headers, on the builder or the host.
- **The object is neither arch- nor kernel-version-specific.** Its ELF
  `e_machine` is `EM_BPF` (BPF is its own ISA; the only target property is
  endianness, and both release arches are little-endian), and `bpf/sluice.c` is
  CO-RE-free — the compiled object's `.BTF.ext` carries `core_relo_len = 0`,
  i.e. zero CO-RE relocations, so there is no kernel struct layout for a
  different kernel to invalidate. Guest kernels are irrelevant: sluice runs on
  the host, attached to the host side of each guest's tap.

What *is* version-dependent is the runtime attach path, and it belongs to the
box rather than to the artifact: the meter uses a TCX link, which needs a **host
kernel >= 6.6**. `setup --sluice` refuses below it. The unit's
`ConditionKernelVersion=>=6.6` is not enough on its own — systemd *skips* a unit
whose condition fails and `systemctl start` on a skipped unit exits 0, which
would be F7's silent-success shape all over again — so `doctor` reports a
condition-failed unit explicitly rather than telling the operator to start it.

## 1.3 Release-pipeline gaps implied by the above

- no `sparkbox-darwin-arm64` asset — **fixed (B1)**
- no `vmlinux-macos-arm64` (outer kernel) asset — **fixed (B2)**
- no `sluice-linux-<arch>` asset, and `tools/sluice` built by no CI at all —
  **fixed**: `hack/stage-artifacts.sh` stages it with a `SHA256_SLUICE` manifest
  entry, `stage-darwin-artifacts.sh` carries both keys across, and `go.yml` has
  a `sluice` job (gofmt/build/vet/test -race + both release cross-compiles).
  That job goes green on compilation and the pure-Go halves only; the eBPF
  load/attach path needs root, `CAP_BPF` and real taps, and it says so.
- the `Containerfile.gateway` build path duplicates the release build instead of
  consuming it — **fixed (B3)**: the image now bakes *no* sparkbox at all. It
  needs neither the git repo nor a Go toolchain (`poc.sh` builds it from a
  three-file staging dir), and `macos/sparkbox-bootstrap.sh` fetches the
  released `sparkbox-linux-<arch>` for the tag being provisioned, verifies it
  against that release's `SHA256_SPARKBOX`, and lets A1's `stepInstallBinary`
  install it. The skew that motivated this — a `v0.3.0-5-g18bfe3b` source build
  driving `v0.4.0` artifacts — is now impossible rather than merely unobserved,
  and `provision` fails outright if the installed binary reports another tag.
- no macOS outer-machine image — **still open**. The gateway image is still
  built locally by `poc.sh`; only the kernel and the binaries inside it come
  from the release. Publishing the image is B4's business, not B1–B3's.

---

# Part 2 — friction where the binary *is* the entry point

## F0 — `setup` never installs the sparkbox binary

The headline finding. `internal/hostsetup` has no step, flag or constant for
installing `sparkbox` itself — it downloads `vmlinux`, `firecracker` and the
rootfs, then writes a unit whose `ExecStart` is a hardcoded
`/usr/local/bin/sparkbox` that nothing ever put there.

Follow the README quick-start literally:

```sh
curl -fsSLo sparkbox …/sparkbox-linux-$arch
chmod +x sparkbox && sudo ./sparkbox setup --release v0.4.0
```

…and the binary lives in `$PWD`, while the unit points at
`/usr/local/bin/sparkbox`. On a genuinely fresh host that file does not exist,
the service cannot start — and because of **F7** `setup` still prints
`[PASS] sparkbox service active` and `== sparkbox is provisioned ==`.

On the DGX it failed the other way, which is worse than a crash: a *stale*
`/usr/local/bin/sparkbox` from a previous release was already there, so the
service came up happily running **v0.3.0** after a successful "v0.4.0" setup.
Everything looked healthy. The tell was `ctl node ls` → `unknown command
"node"`, because v0.3.0 predates the fleet work — i.e. the freshly "installed"
v0.4.0 gateway silently had none of the features we cut the release for.

**Fix:** `setup` should `os.Executable()` itself into `/usr/local/bin/sparkbox`
(atomically, via a temp file + rename) unless the destination is already the
identical build, and `doctor` should compare the running service's version
against the binary's. This single change is also what makes the macOS story
possible — the darwin binary would install the linux binary into the machine it
provisions.



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

## F3b — `destroy` also deletes the expensive kernel

`poc.sh destroy --yes` removes the machine, the local image **and all of
`macos/out`** — which is where the outer kernel lives. The kernel was the single
most expensive artifact in the whole flow, so throwing it away to change a
machine's role is pure waste. (B2 has since made it a 29MB verified download
rather than a compile, which lowers the cost but not the argument — and on the
`SPARKBOX_KERNEL_SOURCE=build` path the original cost is back in full.) We sidestepped it by calling `container machine delete` directly
and keeping `out/`.

Teardown granularity should match cost: machine (cheap, seconds) / image
(minutes) / kernel (very expensive) are three different decisions, not one
`--yes`.

## F4 — legacy layout vs `setup` layout diverge silently

The DGX predates `sparkbox setup`: flat
`/srv/sparkbox/{state,images,vmlinux,users.conf}`. `setup` lays down
`<root>/data/state` + `<root>/data/images` on an XFS reflink volume. Nothing
detects or migrates the old shape.

Confirmed live. `setup --dry-run` against the running v0.3.0 DGX gateway:

```
  - swapfile         → create 16G /swapfile (overcommit safety valve) + swapon + fstab
  - resolve-release  → resolve "v0.4.0" …
  - data-volume      → 300G XFS reflink volume at /srv/sparkbox/data (+ state/ images/)
  - fetch-artifacts  → download + sha256-verify vmlinux, firecracker, rootfs (decompress)
  - users.conf       ✓ already satisfied (2 user(s))
  - host-config      → write /srv/sparkbox/sparkbox.env
  - net-rules        ✓ already satisfied (scripts + sysctl installed)
  - systemd-units    ✓ already satisfied (units installed)
  - admin-ssh        ✓ already satisfied (skipped …)
  - enable-services  ✓ already satisfied (sparkbox.service active)
```

Every `Satisfied` probe is a bare existence check (`stat` the unit file, `is-active`
the service), so the presence of a *differently shaped* install reads as "done".
Applying this plan would have built a new 300G volume and downloaded fresh
artifacts into it while leaving the old units pointed at `/srv/sparkbox/state` —
a half-migrated host with two data roots and no error anywhere.

**Fix:** the `Satisfied` probes should verify the install *matches this config*
(unit's `--state-dir` equals `cfg.StateDir`, etc.), not merely that something
exists. Then either adopt a legacy layout or refuse with instructions.

## F4b — `setup` adds a second swapfile beside the distro's

Ubuntu already had a 16G `/swap.img`; `stepSwap` created its own 16G
`/swapfile` next to it and added a second fstab line:

```
NAME      TYPE SIZE USED PRIO
/swap.img file  16G   1M   -2
/swapfile file  16G   0B   -3
```

32G of swap on a host that asked for 16. The probe should look for *any* active
swap of sufficient size, not for its own path.

## F5 — certmagic is the real teardown hazard

Wiping a gateway drops `state/certmagic/`, and `*.catnip.sh` is DNS-01 issued
under Let's Encrypt's 5-duplicate-certs-per-week cap. Nothing warns you.
Preserving `state/certmagic/` across a rebuild is cheap and should be the
documented default.

## F7 — `setup` reported PASS on a gateway in a permanent crash loop

The most serious finding of the rebuild. `setup` finished with

```
  [PASS] sparkbox service         active
  13 passed, 2 warnings, 0 failed
== sparkbox is provisioned ==
```

while the gateway was in fact restarting every ~2 seconds:

```
level=INFO msg="sparkbox up" … api=127.0.0.1:8080 …
sparkbox: listen tcp 127.0.0.1:8080: bind: address already in use
```

The unit sets `Restart=always` + `StartLimitIntervalSec=0` (deliberately — see
F1: the gateway crash-loops until `:22`/`:2222` is free), so
`systemctl is-active` returns `active` at essentially any sampled instant of a
crash loop. The check `checks.go` performs is exactly that `is-active`.

A provisioning tool that prints "provisioned" over a dead service is worse than
one that fails, because the operator walks away.

**Fix:** the service check must prove *liveness*, not state — poll the API
listener, or compare `NRestarts` before/after a short settle window, or read the
unit's `ExecMainStartTimestamp` twice. Cheap and decisive.

## F8 — `doctor` warns forever on a correctly-configured tunnel-mode host

`checks.go:333` looks for the `SPARKBOX_EDGE` nat chain. But
`deploy/sparkbox-net.sh` only builds that chain in uplink-REDIRECT mode
(`SPARKBOX_EDGE_REDIRECT=1`); in dedicated-edge-IP / tunnel mode — the mode the
DGX runs, and the one `docs/dedicated-edge-ip-cutover.md` recommends — it builds
**`SPARKBOX_TNET`** instead, with DNAT rules rather than REDIRECT:

```
Chain SPARKBOX_TNET (1 references)
RETURN  tcp dpt:2222
DNAT    tcp dpts:1024:65535 to:10.66.0.1:443
DNAT    tcp dpt:22 to:10.66.0.1:2222
```

So a perfectly healthy host reports `[WARN] sandbox NAT rules  SPARKBOX_EDGE
chain not found` on every run, and the suggested remedy ("rules install with
`sparkbox setup`") is a no-op. The check should branch on `SPARKBOX_EDGE_IP` /
`SPARKBOX_EDGE_REDIRECT` and look for whichever chain that mode builds.

## F6 — the README doesn't know about fleets

`README.md` has no mention of nodes, gateways or federation after #15 and #16.
Onboarding a second machine means reading `docs/multi-node-implementation.md`, a
1000-line implementation blueprint, to discover
`sparkbox setup --gateway host:port --node-name x`.

---

---

# Part 3 — what the fleet actually does at v0.4.0

Verified live: DGX `sparky` as gateway, the laptop's nested machine as node
`laptop`, linked over the tailnet at `10.66.0.1:2222`.

**Works end to end.** The node dials out, enrolls, and parks as `pending`. Its
own console prints the fingerprint *and the exact approval command*:

```
this node is enrolled and waiting for approval  node=laptop gateway=10.66.0.1:2222
  ssh ctl@catnip.sh node approve SHA256:IZWmZrHR+PrPOFr5DI5b93scC2XC+0uEZ2pD76MJpnM
  — after checking that fingerprint against the one this machine printed at startup.
```

That fingerprint matched `node ls` exactly, `node approve` took it, and the node
reconnected on its own backoff (~30s) to `linked to the gateway … heartbeat_s=15`.
The roster then shows both machines with arch and sandbox counts. The whole
ceremony is genuinely well-built — clear, self-documenting, no guesswork. It is
the nicest part of the onboarding story and should be the model for the rest.

**Does not work yet, by design.** No sandbox can land on a remote node.
`Fleet.Create` (fleet.go:503) hardcodes `f.local`:

```go
return f.placed(f.local, name, owner, image, func() (*host.Sandbox, error) {
    return f.local.Create(ctx, name, owner, image, vcpus, memMB)
})
```

The only call passing a chosen node is Fork (fleet.go:803). There is no
scheduler and no `--node` override — `docs/multi-node-implementation.md:1051`
marks explicit placement as **M2**, and line 2039 states plainly that "at M1 the
Fleet still serves only local rows". A new sandbox therefore always lands on the
gateway; confirmed — `fedtest` went to `sparky` while `laptop` sat at 0.

So what shipped is the **link layer** (M0+M1): enrollment, trust, roster,
heartbeat, capacity and arch reporting. Remote lifecycle is M2 and the data
plane that makes a remote sandbox indistinguishable is M3. Worth stating clearly
in the release notes, because "federated nodes" reads as "sandboxes run on my
laptop now", and that is the next two milestones, not this one.

---

# Work ranking

0. **F0** — `setup` must install the binary it is running. Without this the
   "download one binary and run setup" story is not true on any host.
0. **F7** — `setup` must not print "provisioned" over a crash-looping service.
   F0 and F7 compound: the first breaks the install, the second hides it.
1. **Ship `sparkbox-darwin-arm64`** — nothing else in Part 1 can start without it.
2. **Ship the pinned macOS outer kernel as a release artifact** — kills the
   from-source kernel build on the user's laptop. *(Done: B2. Note the
   checksum ships as an integrity check, not a reproducibility claim.)*
3. **Port `poc.sh` into `sparkbox setup` on darwin** — doctor, machine
   create/start/stop/destroy, provisioning, in that order.
4. **F2 / sluice** — core subsystems silently absent after `setup`. *(Done: the
   release ships `sluice-linux-<arch>`, CI builds the module, and
   `setup --sluice` installs and enables it. Still opt-in by design.)*
5. **F1** — edge bind address; the `TLS_FLAGS` workaround is a trap.
6. **F4** — legacy-layout detection.
7. **F6** — README fleet quick-start (cheap, high leverage).
8. **F5**, **F3** — teardown safety and the role-switch guard.
