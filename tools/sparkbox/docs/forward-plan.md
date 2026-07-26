# Forward plan — trustworthy setup, macOS as a host, and finishing the fleet

Written 2026-07-25, straight off the v0.4.0 fleet rebuild. Companion to
[`onboarding-notes.md`](onboarding-notes.md) (the findings) and
[`multi-node-implementation.md`](multi-node-implementation.md) (the M2/M3
blueprint, whose W-numbers this plan reuses rather than restates).

## Where we actually are

- **v0.4.0 is published and `latest`** — but following the README quick-start on
  a fresh host does **not** produce a working gateway (F0), and `setup` says it
  did (F7). That combination is live right now for anyone who tries it.
- **The fleet link works** end to end: enroll → approve → heartbeat → roster,
  surviving a node restart. **Remote placement does not exist** —
  `Fleet.Create` hardcodes the local node. That is M2/M3.
- **`GOOS=darwin GOARCH=arm64 go build ./cmd/sparkbox` already succeeds.** The
  macOS gap is packaging and host-provisioning logic, not portability.
- **There is no CI running `go test`.** `.github/workflows/` holds only
  `build-artifacts.yml` and `pages.yml`. F0 and F7 would both have been caught
  by a provision-a-host smoke job.

Three workstreams follow. **A is urgent and small. B is mostly packaging. C is
the big engineering push** and is already specced work-item by work-item.

---

# Workstream A — make `setup` trustworthy

Goal: `curl` one binary, run `sparkbox setup`, get a working host — or a loud,
accurate failure. Ship as **v0.4.1** as soon as A1 lands; the rest can follow in
v0.4.2.

### A1 — F0 + F7 · *ship these together, as one PR* — **size S, priority 0**

They compound: F0 breaks the install, F7 hides it. Fixing either alone still
leaves a silent failure mode.

- **F0** — new `stepInstallBinary` in `internal/hostsetup`: `os.Executable()` →
  copy to a temp file in the destination dir → `chmod 0755` → `os.Rename`
  (atomic, survives a running service). Skip when the destination is already
  byte-identical (`sha256`). Add a `--bin-path` flag defaulting to
  `/usr/local/bin/sparkbox` and template it into the unit instead of hardcoding.
  Refuse to overwrite a *newer* version without `--force`.
- **F7** — replace the `is-active` check with a liveness probe: read
  `NRestarts` and `ExecMainStartTimestamp`, wait a settle window (~10s), read
  again; a changed start timestamp or climbing `NRestarts` is a **FAIL** with
  the last 20 journal lines inlined. Same probe backs `doctor`.
- Add to `doctor`: compare the running service's reported version against the
  binary on disk, and both against `--release`. A version skew is a WARN.

**Acceptance:** on a host with `:8080` occupied and no `/usr/local/bin/sparkbox`,
`setup` exits non-zero, names the port conflict, and prints the failing journal
lines. On a clean host it installs its own binary and `sparkbox version` on the
host matches the release.

### A2 — F1: addressable binds + port preflight — **size S, priority 1**

- Real flags: `--ssh-addr`, `--proxy-addr`, `--api-addr`, `--dns-addr`, each
  templated into the unit. Default `--api-addr` to `127.0.0.1:8079` (`:8080` is
  a bad default on any workstation).
- Preflight: attempt to bind every configured address before writing units; a
  conflict fails *there*, with the owning process named, not at first boot.
- Keep the env bundles for genuine extras, but rename to something honest
  (`EXTRA_FLAGS`) and document that a repeated flag wins last — today real
  config is smuggled into `TLS_FLAGS` and it reads like a bug.

### A3 — F4 + F4b: stop silently half-migrating — **size M, priority 2**

- Every `Satisfied` probe must verify the install **matches this config**, not
  merely that a file exists: parse the unit's `--state-dir`/`--image-dir` and
  compare against `cfg`. Mismatch → not satisfied.
- Detect a populated legacy layout (`<root>/state` with a DB but no
  `<root>/data`) and either adopt it behind `--adopt-legacy` or refuse with the
  exact `mv` command to run. Never provision a second data root beside a live
  one.
- **F4b**: probe for *any* active swap of sufficient size before creating
  `/swapfile`; Ubuntu's `/swap.img` already counted.

### A4 — F8 + F2: doctor honesty and the missing subsystems — **size M, priority 3**

- **F8** — branch the NAT check on `SPARKBOX_EDGE_IP`/`SPARKBOX_EDGE_REDIRECT`
  and assert whichever chain that mode builds (`SPARKBOX_EDGE` vs
  `SPARKBOX_TNET`). Today a correct tunnel-mode host warns forever.
- **F2** — decide core vs optional. Recommendation: **sluice is core** — give
  `setup` a `--sluice`/`--guest-dns` path that installs and enables it, because
  a gateway without it has no egress control and nothing says so. `--archive-*`,
  `--dns-*`, `--tls-provider` and `--ssh-advertise-port` become real flags that
  write into the unit; the DGX's config should be reproducible from flags alone.

  **Done.** The blocker A4 recorded (`tools/sluice` is a second Go module that
  no CI built and no release published, so there was nothing to fetch) is
  cleared: `hack/stage-artifacts.sh` builds and stages `sluice-linux-<arch>`
  with a `SHA256_SLUICE` manifest key, `go.yml` gained a `sluice` job, and
  `setup --sluice` fetches, sha-verifies, seeds `allowlist.txt` + `sluice.env`,
  renders `sluice.service` and enables it — implying `--sluice-socket` and
  `--guest-dns` so the pair cannot be installed-but-not-talking. Shipping a
  prebuilt binary is sound because the embedded eBPF object needs no clang and
  is neither arch- nor kernel-version-specific (`EM_BPF` bytecode, and
  `core_relo_len = 0` — zero CO-RE relocations). The one real floor is a **host
  kernel >= 6.6** for the TCX attach, which `setup` refuses below and `doctor`
  reports, because a `ConditionKernelVersion` alone makes systemd *skip* the
  unit while `systemctl start` still exits 0. Egress filtering stays opt-in:
  the unfiltered default is unchanged and now stated in three places.

### A5 — docs and teardown ergonomics — **size S, priority 4**

- **F5** — document preserving `state/certmagic/` across a rebuild, and have
  `setup` refuse to clobber an existing cert cache without `--force`.
- **F6** — README fleet quick-start: gateway, then
  `sparkbox setup --gateway host:port --node-name x`, then `ctl node approve`.
  Cheap, high leverage, currently requires reading a 1000-line blueprint.
- **F3 / F3b** — `poc.sh` (and its successor) should separate teardown by cost:
  machine / image / kernel are three decisions, not one `--yes`. Allow an
  in-place role switch when the machine holds no sandboxes.

### A6 — CI: actually run the tests — **size S, priority 1 (cross-cutting)**

New workflow: `go build ./... && go vet ./... && go test ./... -race` on
push/PR. Then a **provision smoke job**: in a container or VM, run
`setup --dry-run` and a real `setup` against a local artifact stub, and assert
the service is live by the A1 liveness probe. This is the job that would have
caught F0 and F7. Prerequisite for Workstream C, where the acceptance criteria
are all local-only today.

---

# Workstream B — macOS as a first-class host

Goal: on Apple Silicon, `sparkbox setup` provisions the nested machine itself.
`macos/poc.sh` becomes a thin dev script or disappears.

**Ordering matters:** B1 and B2 are packaging and unblock everything; B4 is the
real work and should not start until A1 lands, because the darwin binary
installing the linux binary into the machine *is* F0's mechanism.

### B1 — ship `sparkbox-darwin-arm64` — **size S, priority 1**

Verified: it already cross-compiles clean (30MB, `darwin/arm64`). Add a
darwin leg to `build-artifacts.yml` — it needs no kernel, no rootfs, no
firecracker, so it is a `go build` + upload, and can run on the existing
`ubuntu-24.04` runner via `GOOS=darwin`. Add `sparkbox-darwin-arm64` (and
`-amd64` if Intel Macs matter) to the manifest.

Decide: does the darwin binary get its own manifest, or a shared one? Recommend
a `manifest-darwin-arm64.env` listing the *linux* assets it will install into
the guest, so the darwin side pins exactly what it provisions.

### B2 — ship the macOS outer kernel as a release asset — **size M, priority 2** — **DONE**

`macos/kernel/build.sh` compiled Linux 6.14.9 + Apple's `config-arm64` 0.5.0 +
our fragment **on the user's laptop**. It is fully pinned, so it now belongs in
CI, built once, as `vmlinux-macos-arm64`. Nobody should compile a kernel as an
onboarding step.

Shipped: a `macos-kernel` job on a native `ubuntu-24.04-arm` runner publishes
`vmlinux-macos-arm64` (+ its resolved `.config`); `manifest-darwin-arm64.env`
carries `OUTER_KERNEL_ASSET` / `SHA256_OUTER_KERNEL`; `macos/kernel/fetch.sh`
downloads and verifies it and is now the default path, with
`SPARKBOX_KERNEL_SOURCE=build` as the escape hatch.

**The reproducibility gate resolved the other way, deliberately.** The
checksum is published as an **integrity** claim — *this is the file the release
shipped, verify your download against it* — and **not** as an identity claim.
Byte-reproducibility across time is not achievable here at reasonable cost:
`build.sh` installs its toolchain with a bare `apt-get install build-essential`,
and the compiler is not merely used but *embedded* — the resolved config carries
`CONFIG_CC_VERSION_TEXT="gcc (Ubuntu 13.3.0-6ubuntu2~24.04.1) 13.3.0"` and the
same string, plus the binutils version, is compiled into the kernel banner. A
packaging-only Ubuntu revision moves the SHA-256 with zero codegen change.
Pinning the archive as well (snapshot.ubuntu.com) would buy an identity claim at
the price of a builder that can never take a compiler security fix, and would
still not be a proof.

What CI *does* prove, and gates on, is the narrower true claim: the build is a
pure function of its inputs. On every `v*` tag the kernel is built **twice at
different `-j`** and the job fails if the two hashes differ — which also settles
the one input `build.sh` does not pin. The recorded known-good hash survives as
a *witness*: a mismatch prints the observed compiler alongside both hashes and
raises a CI warning, but does not fail the release, because failing a release
because Ubuntu revved gcc is treating a snapshot as a law.

### B3 — build the machine image from the released binary — **size S, priority 3**

`macos/Containerfile.gateway` currently compiles sparkbox **from source**,
requiring the git repo and a Go toolchain, and decoupling the node's binary
version from the release it provisions (we hit exactly this: the node ran a
source build while `--release v0.4.0` pinned the artifacts). Change it to
`COPY` the released `sparkbox-linux-arm64`, or drop the baked binary entirely
once A1 makes `setup` install it.

### B4 — `sparkbox setup` on darwin — **size L, priority 4**

Port `poc.sh` into Go, behind `runtime.GOOS == "darwin"`, reusing the existing
`Step`/`Satisfied`/`Plan`/`Apply` framework so `--dry-run` works identically:

| Step | Replaces |
| --- | --- |
| `macos-preflight` (macOS ≥15, Apple Container ≥1.1.0, disk, machine ownership) | `poc.sh doctor` |
| `fetch-outer-kernel` (download + verify B2's asset) | `kernel/build.sh` |
| `machine-image` (pull or build) | `container build` |
| `machine-create` / `start` / `stop` | `container machine …` |
| `provision-inner` (exec `sparkbox setup` inside, with `--gateway` passthrough) | `poc.sh provision` |
| `verify` | `smoke.sh`, `gateway-verify.sh` |

Shell out to the `container` CLI initially — it is a supported interface and
Virtualization.framework bindings are a much larger commitment. Keep the exec
transport (`container machine run --root -i /bin/bash -s`) behind one Go helper
so it can be swapped later.

### B5 — `sparkbox doctor` on darwin — **size S, priority 5** — **DONE**

`checkOS` hard-failed any non-Linux host, which made "run `sparkbox doctor`" an
untruthful instruction on a Mac: the command could only tell its owner their OS
was wrong. Branched: on darwin `DoctorChecksFor` returns the macOS host battery
plus the machine battery, and the last machine check relays the gateway's own
`doctor` out of the VM.

Shipped:

- **Two labelled layers in one report.** Every darwin check name is prefixed
  `mac:` or `machine:`, by the layer the result *describes* rather than where it
  was measured — so `machine: nested virtualization` (read from `container
  inspect`, on the Mac) sits with the machine, and nobody frees disk on the
  wrong host. `mac: outer kernel` is doctor-only and *verifies*: it hashes the
  file and compares against the release manifest, fetching one (15s cap,
  best-effort) since a standalone doctor has resolved none. That manifest is
  deliberately **not** published onto the Env — doing so would turn "no
  `--release` given, so assert nothing" into an assertion against today's
  `latest` and fail every host deliberately a release behind.
- **The relay can never be silently empty.** Four distinct FAILs replace what
  would otherwise be a blank section reading as health (F7's shape): the inner
  binary missing (exit 127, *not* "127 failing checks"), a transport fault that
  never delivered the script, a `container` CLI too old to run it, and a doctor
  that exited 0 while printing nothing. The relayed text is headed
  `── sparkbox doctor, inside machine <name> ──`, because the inner report's own
  `[PASS]` lines are formatted exactly like the outer one's.
- **Machine down is one honest line**, naming which of "does not exist" /
  "is stopped" / "could not be inspected" it is, with the matching next command
  — and doctor never `Exec`s against a stopped machine, because `container
  machine run` boots what it touches.
- **`doctor` grew the macOS-only flags** (`--machine-name`, `--machine-image`,
  `--outer-kernel`, `--container-bin`), sharing setup's `FlagFate` registry and
  its linux-side refusal. Without them a gateway in a non-default machine was
  confidently reported as absent.
- **Linux is untouched**, and pinned so: `TestLinuxDoctorBatteryUnchanged`
  compares `DoctorChecksFor` against `DefaultChecks()` name-for-name and asserts
  the header line is byte-identical.

Proven on real hardware (read-only) as well as against the fake: on an M4 Max
running macOS 26.5.2 with `container` 1.1.0, `sparkbox doctor --machine-name
sparkbox-poc` reported both layers and relayed the PoC gateway's own doctor,
whose `users.conf` FAIL propagated to exit 1.

---

# Workstream C — finish the fleet (M2 → M3)

Already specced in `multi-node-implementation.md` as **W17–W24**, with files,
dependencies and acceptance criteria per item. Do not re-plan it; execute it.
The order below is the doc's own dependency graph.

### M2 — remote lifecycle (operator-visible, sandbox still unreachable)

| Item | What | Size |
| --- | --- | --- |
| **W17** | Node-side lifecycle handlers + `fleet/remote.go` (~15 `Client.Do` methods). Two invariants live here: every record's `Node` is overwritten with the *authenticated* link name, and a dead link returns `Unreachable`, never `io.EOF`. `DialGuest` stays a stub → independently mergeable. | M |
| **W18** | Fleet remote dispatch + the `--node` override. `place.go` declares `Placer`/`Request`/`Candidate` so M5 swaps an implementation rather than editing `Create`. **Watch `parseTags`** — the `new@` door treats every bare word as a tag, so `--node dgx` would be silently swallowed as two tags without the fourth return value. | L |
| **W19** | Events + session hang-up relay, so a pause on the node closes the user's session with the node's own wording. | S |
| **W20** | Reconcile: orphan / adopt / quarantine / offline grace. The rule that matters: **never auto-delete** a ledger row a node stops reporting, and never run the running→paused downgrade against another machine's VMs. | M |

**M2 exit:** `ssh -t new@gw -- --node laptop ml` creates on the laptop, the
ledger says `laptop`, the laptop's own `sandboxes.json` holds it, and
pause/restore/resize/rename/rm/pin all round-trip. With the link down, every
answer is `node "laptop" is offline` at exit 1 / HTTP 503 — while a *different*
user still gets the byte-identical masked `no sandbox named "x"`.

### M3 — the data plane (this is the milestone users feel)

| Item | What | Size |
| --- | --- | --- |
| **W21** | Reverse streams end to end. Turns on the half of W2 that shipped dead. Includes the connection-pooling regression test — two keep-alive requests must produce **one** dial. | L |
| **W22** | Flip every data path onto the fleet dialer: `sshgw`, `proxy`, `xterm`, `envsync`, `runner`, both consoles. Note `envsync` is what makes secrets reach a remote guest at all — the node has no secrets store. | M |
| **W23** | Indistinguishability. Parameterise the existing e2e/proxy/xterm assertions by placement and run each **twice**, local and remote. This is the definition of done and the only thing that catches address-leak bugs. | M |
| **W24** | Gateway restart must not touch a node's VMs. | S |

**M3 exit:** `ssh <name>@gateway`, `https://<name>.<domain>`,
`https://<name>-xterm.<domain>`, tag-selected secrets and scheduled jobs all
work against a sandbox on the laptop, and **no test body differs** between the
local and remote passes except the placement argument.

### Deferred (M4–M6), explicitly

M4 node-local services (metadata mint relay, per-node sluice push), M5 scheduler
policy + fleet-wide caps, M6 migration/drain. `place.go` from W18 is the seam
M5 plugs into. Not needed for the laptop-as-node story.

---

# Suggested sequencing

```
now ──► A1 (F0+F7) ──► v0.4.1 ────────────────────────────► people can install
        A6 (CI)                                              regressions caught
          │
          ├─► A2, A3, A4, A5 ──────────► v0.4.2              setup is honest
          │
          ├─► B1, B2, B3 ──► B4 ──► B5 ─► v0.5.0             mac = one binary
          │
          └─► W17 ─► W18 ─► W19 ─► W20 ─► v0.6.0 (M2)        remote lifecycle
                       └──► W21 ─► W22 ─► W23 ─► W24 ─► v0.7.0 (M3)
```

**Parallelism.** A and C touch almost disjoint code (`internal/hostsetup` +
`deploy/` vs `internal/fleet` + `internal/nodelink`), so they can run
concurrently. B4 depends on A1. W21 depends on W18 **and** W20.

**Do first, regardless:** A1 and A6. Everything else is improvement; those two
are the difference between a tool that installs and one that appears to.

# Risks worth naming

1. **W18's `parseTags` change** touches the `new@` door's argument parsing,
   which every sandbox creation goes through. The blueprint flags it; treat the
   tags tests as a tripwire.
2. **W23 is where latent address leaks surface**, all at once, late. Expect the
   remote pass to fail in places nobody suspected — that is the test doing its
   job, and it is why W23 must not be deferred out of M3.
3. ~~**B2's kernel reproducibility** is assumed, not proven. Verify before
   treating the checksum as an identity.~~ **Settled: it is not reproducible
   across time, and we no longer claim it is.** The builder's gcc and binutils
   versions are compiled into the kernel banner while the Ubuntu archive that
   supplies them is unpinned, so a packaging-only compiler revision changes the
   bytes. `SHA256_OUTER_KERNEL` is therefore an integrity check on the published
   file, not an identity anyone can re-derive; CI instead gates on determinism
   *within* a toolchain (two builds at different `-j` must match) and reports
   drift from the known-good witness without failing on it. Details in B2 above
   and at the top of `macos/kernel/build.sh`. The residual risk is now the
   honest one: **if the release pipeline ever loses that asset, a Mac has no
   fallback but to compile**, so `macos/kernel/build.sh` must keep working.
4. **No `go test` in CI today** means every acceptance criterion in the
   blueprint is honour-system. A6 should land before C starts in earnest.
5. **Mixed-version fleets** are unhandled: nodes report a release tag but
   nothing warns. Worth a WARN in `node ls` once M2 makes version skew matter.
