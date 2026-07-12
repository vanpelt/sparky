# Building an exe.dev-Style Agentic Sandbox Service

*A research-backed design exercise: on-demand microVM sandboxes behind a smart SSH
proxy, built with Go server components, hosted as cheaply as possible.*

This document synthesizes a deep-research pass (23 sources, 115 extracted claims,
top 25 adversarially verified 3-0) covering isolation runtimes, orchestration,
SSH proxying, snapshot/fast-resume techniques, and cost. Vendor-self-reported
numbers are flagged where they appear.

---

## 1. The reference design: how exe.dev actually works

From exe.dev's own docs and engineering blog (verified against
[how-exedev-works](https://exe.dev/docs/faq/how-exedev-works),
[ssh-host-header](https://blog.exe.dev/ssh-host-header),
[proxy docs](https://exe.dev/docs/proxy)):

- **Bare metal + Cloud Hypervisor.** VMs run on rented bare-metal machines using
  Cloud Hypervisor as the VMM — which they explicitly call "a bit of an
  implementation detail (and may change!)".
- **Container image as root block device.** Instead of a traditional VM base
  image, a VM boots from a container image (default "exeuntu") hooked up as a
  block device. This makes new-VM creation take **~2 seconds** (self-reported).
  Trade-off: users can't pick their own kernel — the platform supplies it.
- **No public IP per VM.** HTTPS/TLS terminates at a shared edge and is proxied
  to web servers inside the VM. This is a real cost lever given IPv4 pricing.
- **The SSH trick.** SSH has no `Host:` header, so `ssh vmname.exe.xyz` can't be
  routed by name the way HTTP can. Their smart proxy identifies the *user* by
  the SSH public key presented, and the *target VM* by the destination IP the
  client connected to, drawn from a **shared pool of proxy-owned IPv4
  addresses**. The `{user public key, destination IP}` tuple uniquely resolves
  the VM. DNS for each `vmname.exe.xyz` just points at one of the pool IPs.

Everything below is "how to build that ourselves, in Go, cheaper."

---

## 2. Isolation runtime

| Runtime | Boot | Overhead | Notes |
|---|---|---|---|
| **Firecracker** | ~125ms to userspace | <5 MiB/VM | Minimal device model (virtio-net, virtio-block, vsock, serial). Jailer + per-thread seccomp before guest code runs. Snapshots, UFFD, balloon. Caps: 32 vCPU / 128 GB. Runs AWS Lambda, E2B, Vercel Sandbox. |
| **Cloud Hypervisor** | ~200ms (sub-100ms advertised with direct kernel boot) | ~10 MiB/VM | PCI virtio (vs Firecracker's mmio-only), CPU/memory hotplug, VFIO GPU passthrough, live migration, TDX/SEV. Scales to 512 vCPU / 1 TB. What exe.dev and Northflank (via Kata) use. Rust project — no first-class Go SDK. |
| **Kata Containers** | ~150–300ms (VMM-dependent) | higher | Hardware isolation with native K8s/CRI integration. Right answer if you're already on Kubernetes; otherwise it drags in that whole world. |
| **gVisor** | fast (no VM boot) | low | Userspace kernel intercepting syscalls; host syscall surface as small as 53–68 allowed syscalls. But 10–30% I/O penalty and syscall-compat gaps. Modal builds on it (runsc) — viable, not the default choice here. |
| **Plain containers + userns** | instant | none | Not a sufficient boundary for hostile, agent-generated code with full shell access. Fine only as an inner layer. |

**Recommendation: Firecracker.** For multi-tenant execution of hostile
agent-generated code needing full Linux, the verified consensus is a microVM —
Firecracker for density and minimal attack surface, Cloud Hypervisor when you
need GPU passthrough or hotplug. Firecracker wins for us specifically because
Go is a first-class control-plane language:
[`firecracker-go-sdk`](https://github.com/firecracker-microvm/firecracker-go-sdk)
(official, firecracker-microvm org) wraps the API with source-level support for
snapshots (`snapshot.go`), balloon devices (`balloon.go`), the jailer
(`jailer.go`), and I/O rate limiting — exactly the primitives fast-resume and
oversubscription need. Caveat: last tagged release is v1.0.0 (Sept 2022); the
repo is active but users pin commits.

Keep the VMM swappable (exe.dev does). If GPU sandboxes ever matter, that's the
moment to add Cloud Hypervisor — or start on
[flintlock](https://github.com/liquidmetal-dev/flintlock), which abstracts both.

## 3. Host lifecycle layer

Three ready-made Go options, or roll our own:

1. **`firecracker-go-sdk` directly (recommended).** Maximum control, minimum
   moving parts. E2B's production orchestrator does exactly this — Go code in
   `packages/orchestrator/pkg/sandbox/fc` managing one Firecracker process per
   sandbox over a Unix-socket HTTP client. Their repo
   ([e2b-dev/infra](https://github.com/e2b-dev/infra)) is the best open
   reference implementation: `unshare -m` mount isolation, cgroup placement via
   fd during clone, orphan reclaim by scanning `/proc`, token-bucket rate limits
   on net and block devices via Firecracker API.
2. **flintlock** (liquidmetal-dev): Go, gRPC/HTTP microVM lifecycle service,
   supports Firecracker *and* Cloud Hypervisor, OCI-image-backed volumes via
   containerd, cloud-init/ignition metadata. v0.9.0 (Nov 2025). Caveat:
   community-owned since Weaveworks shut down in 2024.
3. **firecracker-containerd** (AWS): containerd control plugin + ttrpc shim +
   in-VM agent running runC. Explicitly targets untrusted-workload sandboxing at
   near-container density. Take this path only if containerd/CRI compatibility
   is a requirement — it has no tagged releases and the README disclaims
   multi-tenant use "at the user's own risk."

## 4. Scheduling: don't use Nomad or Kubernetes

The strongest evidence here is Fly.io's production account
([Carving the Scheduler Out of Our Orchestrator](https://fly.io/blog/carving-the-scheduler-out-of-our-orchestrator/)).
They launched on Nomad and left because:

- Bin-packing concentrated workloads onto a few overloaded servers ("Katamari
  Damacy scheduling").
- Async, consensus-based scheduling couldn't do synchronous scale-from-zero —
  "create a machine on this host *now* and tell me it worked" — which is exactly
  the sandbox-on-connect operation we need.

Their replacement, **flyd**, is the pattern to copy: a per-host Go daemon with a
local BoltDB append-only log, *no shared state and no consensus protocol*
between hosts. Placement is market-style: the API layer discovers workers and
capacity (via Corrosion, a gossip-replicated SQLite) and runs a quick best-fit
ranking. No pending-job queue.

Also relevant: self-hosting E2B's stack requires operating Nomad + Consul —
operationally heavy, and a point *against* adopting their infra wholesale even
while cribbing from their Firecracker code.

**Recommendation:** at our likely scale (1–10 hosts) this is small:
a control-plane service holding a host inventory in SQLite/Postgres, per-host
agents reporting capacity, best-fit ranking at placement time, host agent is
the source of truth for what's actually running. That's a week of Go, not a
distributed-systems project — and it's the same shape Fly runs at planet scale.

## 5. The smart SSH proxy

Go is unusually well-supplied here. Three verified options, in increasing
weight:

1. **Build on [gliderlabs/ssh](https://pkg.go.dev/github.com/gliderlabs/ssh)
   (recommended).** A net/http-style server API over `golang.org/x/crypto/ssh`
   that automatically handles env vars, PTY requests, and window resizing, with
   port forwarding in both directions (`DirectTCPIPHandler` +
   `LocalPortForwardingCallback`; `ReversePortForwardingCallback` +
   `ForwardedTCPHandler`). Production-proven adjacent: Tailscale vendors it for
   its SSH server; Charm's Wish builds on it; 1,000+ importers. Caveats: pre-1.0
   (v0.3.8), slow release cadence — pin it, or vendor like Tailscale does.
2. **[sshpiper](https://github.com/tg123/sshpiper)** — the closest off-the-shelf
   drop-in: MIT, ~91% Go, actively maintained, an SSH *reverse proxy* supporting
   ssh/scp/port-forwarding with plugin-based routing (yaml, username-router,
   docker, kubernetes, lua — or a custom Go plugin routing on public key).
   Caveats: it's a standalone daemon + plugin framework, not a library; and as
   an SSH MITM it can't forward the client's publickey auth upstream — the
   routing plugin must supply upstream credentials (fine: our proxy owns a key
   trusted by every sandbox).
3. **[Teleport](https://github.com/gravitational/teleport)** — full
   identity-aware proxy: Auth Service + Proxy + agents, CA issuing short-lived
   SSH/mTLS certs bound to identity, SSO via Okta/Google/GitHub. Mature (~74%
   Go, single binary), but AGPL core and a much larger system than we need.
   Adopt only if enterprise SSO/audit becomes a requirement.

### Routing without a Host header

Two viable schemes:

- **exe.dev's scheme:** `{client public key → user, destination IP → VM}` over a
  small pool of proxy IPs. Elegant, preserves plain `ssh vmname.our.domain`,
  costs a pool of IPv4s and wildcard-ish DNS management.
- **Simpler v1:** encode the target in the SSH username —
  `ssh vmname@ssh.our.domain` — and identify the user by public key alone. One
  IP, zero DNS tricks, works with ProxyJump and scp/sftp/port-forwarding
  unchanged. sshpiper's username-router plugin does exactly this. Upgrade to the
  destination-IP scheme later purely for UX polish.

**The key move: resume-on-connect.** The gateway authenticates the pubkey, asks
the control plane "where is VM X; wake it if suspended," the host agent restores
the snapshot (hundreds of ms, see below), and the gateway then dials the VM
(vsock or its tap IP) and pipes SSH channels through. The user perceives a
slightly slow first connection to an always-on machine — while suspended
sandboxes cost only disk.

## 6. Snapshot / fast-resume

This is where the cheap-and-efficient economics actually come from. Verified
mechanics from Firecracker's docs and provider engineering blogs:

- **Firecracker snapshot/restore** captures full memory + CPU + device state
  (full and diff snapshot flavors). Restore mmaps the memory file
  (`MAP_PRIVATE`, copy-on-write) so pages load **lazily on fault** — resume is
  ~200–300ms even for multi-GB guests because typically only 300–400 MB of
  pages are actually touched. One practitioner writeup measured **~28ms**
  restores for small VMs (5ms process start, 8ms mmap, 10ms device/CPU state,
  5ms vsock reconnect).
- **UFFD (userfaultfd)** lets an external userspace process serve guest page
  faults — the primitive under E2B's lazy resume. E2B backs guest memory with a
  memfd passed over the UFFD socket, dedups snapshot memory at 4 KiB page
  granularity against the base template, and uses virtio-balloon stats plus a
  pre-pause hugepage collapse (via their in-guest agent) to shrink footprints.
- **CodeSandbox's fork pipeline** (their engineering blog): `MAP_SHARED` mmap of
  the memory file so the kernel lazily flushes dirty pages → snapshot *save*
  drops from ~1s/GB to 30–100ms; copy-on-write for both disk and memory files →
  clone copy ~50ms; full running-VM fork < 2s. Their time-to-running-preview on
  a real Next.js repo: 132s fresh → **0.6s from memory snapshot**. Caveat they
  hit: xfs fragments under random writes.
- **Fly.io suspend/resume**: Firecracker-snapshot-based, resume in a few hundred
  ms vs 2+s cold start; they discourage it above 2 GB RAM and treat snapshots as
  best-effort, not durable — both good calibration points.
- **Modal** (contrast): gVisor userspace checkpoint/restore rather than
  microVM snapshots; ~2.5× faster than cold starts with background lazy page
  loading. Confirms the technique matters more than the runtime.

Operational caveats to design for: guest wall-clock drift after resume (guest
continues from snapshot time — re-sync clock via agent), network connections not
guaranteed across resume, snapshots not portable across CPU models (keep the
fleet homogeneous), and restoring one snapshot multiple times risks reused
randomness/tokens in-guest (reseed entropy; CodeSandbox and E2B both handle
this).

**Disk strategy:** OCI image → flattened ext4/erofs base image, one per
template; per-VM writable layer via CoW reflink clones (`cp --reflink` on
XFS/btrfs, or ZFS clones). Base layers are shared page-cache across all VMs on
the host, which compounds the memory savings.

## 7. Cost analysis

What the managed vendors charge (verified from Northflank's pricing comparison,
so treat as one vendor's framing, but the E2B/Daytona list prices check out):

- E2B and Daytona: **$0.0504/vCPU-hr + $0.0162/GiB-hr**.
- 200 concurrent 2vCPU/4GB-class sandboxes: ~$7.2k/mo (Northflank) to $16.8k
  (E2B/Daytona), $24.5k (Modal), $35k+ (Fly Sprites).
- Idle economics is the differentiator: E2B pauses indefinitely for storage-only
  cost; Fly Sprites charges nothing idle. Any self-hosted design must match this
  via suspend-to-snapshot.

Self-hosted bare metal (my figures, not from verified sources — spot-check
current price lists): Hetzner dedicated boxes run roughly **€40–110/mo** for
14–32 threads and 64–128 GB (e.g. EX-class i5/64GB near the bottom, AX102
Ryzen 16c/32t/128GB near the top; server auction is cheaper). Taking ~€100/mo
for 32 threads / 128 GB:

- Raw cost ≈ **$0.005/vCPU-hr** — ~10× under E2B's list price before any
  oversubscription.
- Firecracker explicitly supports oversubscribing CPU *and* memory, ratio chosen
  by workload correlation. Agent workloads are bursty and mostly idle-waiting on
  LLM calls — 3–5× CPU oversubscription is plausible; memory reclaims via
  balloon + suspend-idle-VMs + page dedup against shared base snapshots.
- Memory is the real capacity bound: 128 GB ≈ ~100 concurrent *active* 1 GB
  sandboxes plus effectively unlimited suspended ones (disk-only). That's
  ≈ **€1/mo per always-available sandbox** on one machine.
- No per-VM public IPv4 (the proxy pool + HTTPS edge handles ingress) — at
  ~$0.5–4/mo per IPv4 these days, per-VM IPs would rival the compute cost.
  IPv6 flips this: a single routed `/64` (free with most bare-metal hosts) has
  18 quintillion addresses, so every sandbox can hold its *own* globally-routable
  `/128` for no-NAT egress. sparkbox does exactly this (`--subnet6`) while still
  fronting all *ingress* through the dual-stack edge — public URLs stay reachable
  from IPv4-only clients, which ~half of them still are.

The requirement bare metal imposes: **KVM access**, i.e. real hardware or the
few VPS providers with nested virt. Hetzner/OVH dedicated is the sweet spot;
avoid hyperscaler VMs (need expensive .metal instances).

## 8. Recommended architecture

Four small Go services plus an image pipeline:

```
                    ┌──────────────────────────────────────────────┐
   ssh vmname@gw ──▶│ sshgw (gliderlabs/ssh)                       │
                    │  pubkey → user; username → VM                │
                    │  resume-on-connect; channel pipe over vsock  │
                    └──────────────┬───────────────────────────────┘
                                   │
   https://vm.dom ─▶┌──────────────▼───────────────┐   ┌───────────────────┐
                    │ edge (Go reverse proxy/Caddy)│──▶│ control plane      │
                    │  TLS termination, *.wildcard │   │  API + placement   │
                    └──────────────────────────────┘   │  SQLite/Postgres   │
                                                       │  best-fit ranking  │
                                                       └─────────┬─────────┘
                                                                 │ gRPC
                          ┌──────────────────────────────────────▼─────┐
                          │ hostd (per bare-metal host)                │
                          │  firecracker-go-sdk · BoltDB local log     │
                          │  snapshots/UFFD · balloon · tap/vsock      │
                          │  CoW reflink disks on XFS/btrfs            │
                          │  [ fc-vm ] [ fc-vm ] [ fc-vm ] … ~100/host │
                          └────────────────────────────────────────────┘

   image pipeline: OCI image ──(containerd/skopeo + mkfs)──▶ ext4/erofs base
                   + warm "booted + sshd ready" memory snapshot per template
```

1. **`hostd`** — per-host agent on the flyd pattern: owns Firecracker processes
   via `firecracker-go-sdk`, local BoltDB append-only log as source of truth,
   manages snapshots (warm template snapshot → CoW clone per sandbox), balloon
   deflation/inflation, tap devices, and disk clones. Reports capacity.
2. **`control`** — API + placement: sandbox CRUD, template registry, best-fit
   placement over live host capacity, no job queue (synchronous create like
   flyd/flaps).
3. **`sshgw`** — gliderlabs/ssh gateway: pubkey auth, `vmname@` routing (v1),
   resume-on-connect, then bidirectional channel piping into the VM over
   vsock/tap. Add the exe.dev destination-IP trick later.
4. **`edge`** — wildcard-TLS HTTP reverse proxy into VMs (a thin Go proxy or
   just Caddy). *Implemented in the MVP (`internal/proxy` + `internal/routes`):*
   `<subdomain>.hivemind.tools` → guest `IP:port`, subdomain defaults to the
   sandbox name and port to `:8000`, routes stored in sqlite, resume-on-connect
   on HTTP hits. TLS via `--proxy-tls`: a single `*.hivemind.tools` wildcard
   cert obtained by ACME DNS-01 through Cloudflare (CertMagic) — one cert covers
   every ephemeral sandbox subdomain, sidestepping Let's Encrypt per-name rate
   limits — or per-host autocert as a no-DNS-API fallback.
5. **Image pipeline** — flatten OCI images to block devices; for each template,
   pre-boot once to sshd-ready and keep the memory snapshot: every sandbox
   thereafter *resumes* in ~hundreds of ms instead of cold-booting.

### Build order

1. `hostd` + firecracker-go-sdk on one Hetzner box: boot a VM from a flattened
   Ubuntu OCI image, ssh into it directly. (The whole thing is demoable here.)
2. Warm-snapshot pipeline: template → booted snapshot → CoW clone + restore.
   Target < 1s sandbox acquisition.
3. `sshgw` with username routing + resume-on-connect.
4. `control` + second host + placement.
5. `edge` HTTPS proxy; suspend-idle reaper (balloon → pause → snapshot → kill).

## 9. Open questions the research didn't settle

- Verified pricing for Hetzner/OVH configs and real-world safe oversubscription
  ratios (the cost leg above uses list-price memory, flagged).
- Whether replicating E2B's UFFD + page-dedup layer is worth it over plain
  mmap lazy restore at our scale (plain mmap is probably fine to start).
- gVisor/Kata/QEMU-microVM never survived claim verification in depth — the
  comparison above leans Firecracker/Cloud-Hypervisor-centric, matching both
  exe.dev's and E2B's production choices.
- At what fleet size the custom scheduler stops being "a week of Go" — for
  single-digit hosts it's trivially simple; the flyd pattern says it stays
  simple longer than intuition suggests.

## Sources (primary)

- exe.dev: [how it works](https://exe.dev/docs/faq/how-exedev-works) · [SSH host-header problem](https://blog.exe.dev/ssh-host-header) · [proxy](https://exe.dev/docs/proxy)
- [firecracker-go-sdk](https://github.com/firecracker-microvm/firecracker-go-sdk) · [Firecracker snapshot docs](https://github.com/firecracker-microvm/firecracker/blob/main/docs/snapshotting/snapshot-support.md) · [Firecracker design](https://github.com/firecracker-microvm/firecracker/blob/main/docs/design.md)
- [flintlock](https://github.com/liquidmetal-dev/flintlock) · [firecracker-containerd](https://github.com/firecracker-microvm/firecracker-containerd)
- Fly.io: [Carving the scheduler out of our orchestrator](https://fly.io/blog/carving-the-scheduler-out-of-our-orchestrator/) · [suspend/resume](https://fly.io/docs/reference/suspend-resume/)
- [sshpiper](https://github.com/tg123/sshpiper) · [gliderlabs/ssh](https://pkg.go.dev/github.com/gliderlabs/ssh) · [Teleport](https://github.com/gravitational/teleport)
- [CodeSandbox: cloning a running VM in 2 seconds](https://codesandbox.io/blog/how-we-clone-a-running-vm-in-2-seconds) · [Modal memory snapshots](https://modal.com/blog/mem-snapshots) · [E2B infra Firecracker integration](https://deepwiki.com/e2b-dev/infra/3.2-firecracker-integration)
