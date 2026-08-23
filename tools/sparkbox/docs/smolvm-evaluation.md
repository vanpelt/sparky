# smol machine (smolvm) — evaluation against Sparkbox

Status: research spike
Date: 2026-08-23
Subject: [`smol-machines/smolvm`](https://github.com/smol-machines/smolvm) @ `v1.10.1` / `159fe30`
Lens: what it means for Sparkbox, especially the CKS/Kubernetes work in
[`deploy/kubernetes/`](../deploy/kubernetes/) and [`deploy-cks.md`](deploy-cks.md).

*Updated 2026-08-23 after the CKS work merged to main. The conclusions did not
move; two of them got independent corroboration from that merge, noted inline.*

## Name disambiguation, first

Two unrelated projects answer to "smolVM". This document is about
**`smol-machines/smolvm`** (smolmachines.com, `smolvm` CLI, Rust, libkrun) —
the one that got the Hacker News run. The other is
[`CelestoAI/SmolVM`](https://github.com/CelestoAI/SmolVM), a Python
Firecracker/QEMU/libkrun wrapper aimed at "sandbox for AI-generated code" with
a much smaller surface. Everything below refers to the former; the latter is
not competitive with Sparkbox and is not evaluated further.

## Summary

smolvm is a genuinely impressive piece of work and it overlaps Sparkbox in
exactly one layer — *the VM runtime on a single host* — while diverging from it
almost everywhere else. It is not a service; it has no users, no accounts, no
public edge, no name-addressed routing, no browser terminal. Sparkbox is
mostly those things.

**Recommendation: do not adopt smolvm as Sparkbox's VMM, and do borrow four
specific things from it.** The one part worth a real spike is its containerd
shim, because it reframes the question the CKS POC is currently losing: *should
a sandbox be a Pod?*

The four borrows, in value order:

1. **OCI images as the rootfs format** — retires the 750 MB release payload and
   the runtime template refresher, which are the ugliest parts of the CKS POC.
2. **S3-native volume mounts performed by the guest agent** — turns the CKS
   durable-storage gap from "manual pause-compress-upload checkpoints" into
   "the work isn't on the node in the first place".
3. **VM ownership in sibling cgroups, not the control plane's** — control-plane
   restarts stop killing guests. Cheap, and directly applicable to a Pod whose
   controller container restarts.
4. **Landlock + seccomp confinement on the VMM process** — additive hardening
   over the existing chroot jailer and per-VM UID.

## What smolvm actually is

Verified by reading the source, not the README.

| | smolvm | Sparkbox |
| --- | --- | --- |
| Language / size | Rust, ~175k LOC (excl. vendored VMM) | Go |
| VMM | **libkrun**, linked as a library, in-process | **Firecracker**, separate process, `firecracker-go-sdk` |
| Guest kernel | **libkrunfw** (kernel baked into the library) | own pinned kernel artifact, SHA-256 verified |
| Rootfs | **OCI images**, pulled straight from any registry, no Docker daemon | ext4 template built from a Dockerfile, downloaded as a release payload |
| Host platforms | Linux (KVM), macOS (Hypervisor.framework), Windows (WHP); x86_64 + aarch64 | Linux (KVM); macOS only via a nested Linux gateway VM |
| Networking | TSI socket hijack **or** host-side userspace stack (smoltcp); netns-tap for CNI | TAP + iptables + NAT, per-slot guest addressing |
| Egress policy | `allowed_cidrs` + DNS-learned IPs from `allow_hosts`, TTL clamped 60–3600 s, enforced where the host socket is opened | **Sluice**: eBPF/TCX allow-set on the TAP, DNS-learned, `--enforce`, plus per-domain byte accounting |
| Suspend | `stop` is disk-persistent only; checkpoint/restore exists on their libkrun control socket but is opt-in, best-effort, and used for forking | Firecracker memory snapshot; **pause/resume is the product** (idle reaper + resume-on-connect) |
| Fork | **CoW guest-RAM fork from a frozen golden**, with lease-managed warm pools | `Snapshot` → sanitized ext4 template → cold boot |
| Daemon | `smolvm serve`: axum REST + OpenAPI, optional fail-closed mTLS, systemd scopes, `/drain`, `/capacity`, p2p blob fetch, prewarm | the Sparkbox binary itself: SSH gateway + HTTPS edge + REST + fleet link |
| Kubernetes | **containerd shim v2** `io.containerd.smolvm.v2`, RuntimeClass, k3s installer, `critest` 82/7/24 | manifests that run Sparkbox *on* Kubernetes; VMs are internal to a pinned Pod |
| Identity / multi-tenancy | none — mTLS between control plane and node; the loopback door is "unauthenticated by construction" | SSH-key accounts, OIDC issuer, session tokens, GitHub linking, per-owner pools |
| License | Apache-2.0; **forked** libkrun (Apache-2.0) and libkrunfw (LGPL-2.1 lib / GPL-2.0 kernel) | — |
| Activity | 5.7k stars, ~1,344 commits, 50 commits in the last 7 days, 37 open issues / 38 open PRs, one primary author | — |

Two structural facts matter more than the table suggests.

**smolvm's product direction is not ours.** Its `serve` API has
`/v1/load_lora_adapter`, `/v1/completions`, `{name}/generate`,
`{name}/batches`, CUDA-aware pool admission, and shared immutable CUDA weights
across forked workers. It is becoming an **RL-rollout / GPU-agent fleet node**.
The sandbox story is real but it is not where the roadmap is pointing. Sparkbox
is a multi-tenant developer sandbox service. Betting our VMM layer on a project
optimizing for a different workload is a slow-acting risk.

**smolvm ships forks of both libkrun and libkrunfw.** `.gitmodules` points at
`smol-machines/libkrun` and `smol-machines/libkrunfw`. Adopting smolvm means
adopting their VMM *and* their guest-kernel build. Sparkbox currently pins and
verifies its own kernel — and the macOS nested PoC needed a KVM-enabled custom
kernel. Handing that over is a bigger concession than it looks.

## The Kubernetes lens

### Where the CKS POC actually hurts

Reading [`deploy-cks.md`](deploy-cks.md) against smolvm makes the POC's costs
legible. Today, on CKS, Sparkbox pays for being a VM manager that Kubernetes
does not know is a VM manager:

- **One pinned Node.** `sparkbox-node` is a Deployment with
  `kubernetes.io/hostname: <exact node>`. There is no scheduling, no
  rescheduling, no bin-packing. Placement is Sparkbox's own ledger.
- **A bespoke device plugin.** `sparkbox.dev/kvm`, `sparkbox.dev/tun`,
  `sparkbox.dev/loop` exist because Sparkbox needs devices Kubernetes has no
  opinion about.
- **A root `vmm-helper` sidecar** holding TAP/network capabilities, plus a
  one-shot init container holding `CAP_SYS_ADMIN` and the loop bundle, plus a
  root Sluice sidecar with `CAP_BPF`/`CAP_NET_ADMIN`/`CAP_NET_BIND_SERVICE` —
  all so a TAP device can exist inside the Pod netns.
- **State that dies with the Node.** Guest disks and `sandboxes.json` live on a
  hostPath; CoreWeave local storage is lost when the Node is replaced. Durable
  checkpoints are manual, synchronous, and currently *unavailable* in the split
  deployment because the gateway no longer has the disk.
- **A 750 MB cold start.** First Pod start downloads the release payload, then
  the agent CLI bundles, then decompresses a sparse ext4 image, then patches the
  template — before the gateway opens.
- **Template snapshots refused outright**, because CKS runs
  `--disable-host-rootfs-mounts`.

Every one of those is a symptom of the same thing: Kubernetes is hosting a
process that secretly manages VMs, instead of managing the VMs.

### Option A — sandboxes become Pods, smolvm's shim becomes the runtime

This is the interesting one, and Sparkbox's own security review arrived at the
same doorway from the other side. [`security-hardening.md`](security-hardening.md)
closes by naming the architectural alternative to a device-owning application
Pod: "a node-level runtime shim or daemon", citing Kata Containers and
firecracker-containerd, and concluding "a narrow node runtime owns
virtualization; the public service calls it over a constrained protocol."

That is precisely what a containerd shim v2 is. The disagreement between that
paragraph and this section is not about the destination — it is about whether
the shim should be smolvm's or ours. `runtimeClassName: smolvm` on a Pod and every Pod
sandbox boots as its own microVM. If a Sparkbox sandbox were a Pod:

- the kubelet allocates `/dev/kvm`; **our device plugin disappears**;
- CNI gives the guest a real pod IP at L2 via smolvm's `netns_tap` bridge;
  **our TAP/iptables/NAT layer disappears**, and with it the root vmm-helper;
- the scheduler places sandboxes and reschedules them on node loss; **our
  placement ledger becomes advisory**;
- a PVC carries the rootfs; **hostPath and the node-pinning disappear**;
- the OCI image *is* the rootfs; **the release payload and template refresher
  disappear**;
- `RuntimeClass.overhead.podFixed` makes the per-VM cost visible to the
  scheduler, which our current model cannot express at all.

The seven `critest` failures are, for us, mostly irrelevant. `HostNetwork` and
`HostIpc` are things a sandbox must *not* have. Mount propagation across the VM
boundary is not something we use. `portforward` fails because containerd dials
`127.0.0.1` while the workload listens at the pod IP — but Sparkbox's edge dials
the guest address directly and never uses `kubectl port-forward`, so this
misses us too. That is an unusually clean fit.

What it costs:

- **Pause/resume is the product, and it does not survive the move.** Sparkbox's
  reaper parks idle sandboxes and resumes them with RAM intact on the next SSH
  or HTTP connection. A Pod has no such state: scaling to zero is a cold boot.
  smolvm's checkpoint/restore is an opt-in libkrun control-socket call used to
  freeze fork goldens, not a supported suspend-to-disk. Until that is a real
  feature, "sandbox = Pod" means "resume = cold boot", and resume-on-connect
  loses its meaning.
- **`host.Manager` is the rewrite.** Create/Pause/Resume/Destroy/Archive would
  become Kubernetes object reconciliation. That is a controller, not a patch.
- **Routing gets harder before it gets easier.** Today the gateway resolves a
  sandbox name from its local inventory and dials a TAP address. With Pods it
  resolves through the API server, and every sandbox needs a stable address as
  it is rescheduled.
- **Sluice has nowhere to attach.** Its eBPF programs attach to the TAP in the
  Pod netns. In the shim model that netns belongs to CNI and the tap is created
  by the shim. Enforcement would have to move to a `CiliumNetworkPolicy` per
  sandbox (which cannot express DNS-learned per-tag allow-lists as cheaply, and
  gives us no per-domain byte accounting) or Sluice would have to run as a CNI
  chained plugin. **This is the load-bearing unknown**, and it should be the
  first thing any spike answers.

### Option B — smolvm as a `vmm.Driver` behind the current control plane

Sparkbox's [`internal/vmm/driver.go`](../internal/vmm/driver.go) is a clean
seam: `Create/Pause/Resume/Destroy` plus optional `Archivable`, `Ballooner`,
`DiskResizer`, `Renamer`, `Rebooter`, `CPUStatser`, `NetStatser`,
`DiskReporter`. A `smolvm` driver would talk to `smolvm serve` over its REST
API — the cross-language boundary is a non-issue.

The fit is partial and the gaps are the expensive ones:

| driver capability | smolvm |
| --- | --- |
| `Create` / `Destroy` | ✅ `/machines`, `DELETE /machines/{id}` |
| `Pause` / `Resume` with RAM | ❌ `stop` is disk-only |
| `Ballooner` | ✅ elastic memory via virtio-balloon is a headline feature |
| `DiskResizer` | ✅ `/machines/{id}/resize` |
| `Archivable` (pack/unpack/snapshot) | ✅ `/machines/{id}/export`, `pack` artifacts, `fork` |
| `NetStatser` per-domain | ❌ no accounting; that is Sluice's job |

We would be running a second control plane (`serve` has its own SQLite records,
its own pool controller, its own supervisor) underneath ours, to gain OCI images
and lose pause/resume. **Not worth it.**

### Option C — borrow, keep Firecracker

Lowest risk, highest near-term value, and it does not foreclose Option A.

## What to borrow, concretely

### 1. OCI images as the rootfs format — *do this regardless*

Sparkbox's rootfs pipeline is a monolithic ext4 template plus
`deploy/refresh-agent-tools.sh` running on every Pod start to patch current
`claude`/`codex`/`pi`/`hivemind` binaries into it, with the documented wart that
"existing sandbox disks are not rewritten by a later refresh". smolvm pulls OCI
images from any registry with no daemon (`crates/smolvm-oci-layer`,
`crates/smolvm-registry`) and treats the image as the machine's root.

Adopting OCI-as-rootfs — build the ext4 from an OCI image at create time, cached
by digest — retires the release payload, the refresher, the drift between old
and new sandbox disks, and the "template snapshots refused on CKS" problem in
one move. It also unlocks bring-your-own-image, which is a product feature we
do not currently have. This is the single highest-value borrow and it is
independent of every Kubernetes question.

### 2. S3-native volume mounts, performed by the guest agent

`crates/smolvm-s3fs` is a self-contained FUSE + SigV4 S3 filesystem — no
libfuse, no `fusermount3`, no external binary — that the agent mounts by
entering the workload's mount namespace between container create and start. It
works in a distroless image because nothing is required of the image.

The CKS durability story right now is: guest disks are on ephemeral node-local
XFS, and durable checkpoints are a manual `ssh ctl@ checkpoint` that pauses the
guest, scrubs secrets, compacts, compresses, and uploads to VAST — currently
*disabled* in the split deployment. A guest-mounted S3/VAST volume changes the
question from "how do we get the disk off the node before we lose it" to "the
work was never only on the node". That is a better answer than the reflink plan
in [`cks-reflink-persistence-plan.md`](cks-reflink-persistence-plan.md) is
reaching for, and it composes with it rather than replacing it.

### 3. VM ownership in sibling cgroups

[`docs/lossless-serve-restart.md`](https://github.com/smol-machines/smolvm/blob/main/docs/lossless-serve-restart.md)
documents a bug we will hit if we have not already: VMs forked as children of
the control plane's delegated cgroup get SIGKILLed by `KillMode=control-group`
on restart, and the service cgroup then fails to recreate. Their fix is to make
each VM its own `smolvm-vm-<id>.scope` owned by PID 1, so a restarted control
plane reattaches over the agent socket instead of finding a graveyard.

On CKS the analogue is sharper: the guests are children of the `vmm-helper`
container. A helper restart — an OOM, a rollout, a liveness failure — should not
take every sandbox on the node with it. Whatever the systemd-free equivalent is
inside a Pod, the reattach-over-socket design is the right shape and it is cheap
to adopt.

### 4. Landlock + seccomp on the VMM process

smolvm installs a seccomp allowlist (`seccompiler`) and Landlock filesystem
restrictions on the boot subprocess after its privileged window and before the
guest runs — explicitly because libkrun, unlike Firecracker, ships no seccomp
filter of its own. We get Firecracker's filter for free, but we do not have
Landlock. Our per-VM chroot + UID `100000 + slot` is good; a Landlock ruleset
pinning the VMM to exactly its own VM directory is a strictly additive second
lock on the same door, and `security-hardening.md` is the right place for it.

Borrows 3 and 4, and the lower-priority items below, are worked out in
[`hardening-and-guest-filesystems.md`](hardening-and-guest-filesystems.md).
Borrow 3 turned out to describe a live bug rather than an improvement: a
control-plane restart currently hard-stops every running VMM, losing guest RAM.

### Worth noting, lower priority

- **Fork pools with CoW guest RAM and leases** (`src/pool.rs`,
  `src/api/pool_controller.rs`). Sparkbox's `Snapshot` produces a template that
  cold-boots; a frozen golden that CoW-forks gives a warm sandbox in
  milliseconds. This is what "turbo" wants to grow into. Firecracker can do
  snapshot-restore fan-out, so the idea ports even though the code does not.
- **p2p blob serving + `/artifacts/warm`** for image fanout across fleet nodes —
  relevant once more than one node pulls the same rootfs.
- **`RuntimeClass.overhead.podFixed`** as a modelling idea: even in the current
  architecture we should be able to state the per-sandbox host overhead, and we
  currently cannot.
- **`critest` as a yardstick.** If we ever do Option A, "82/7/24, and here are
  the 7 and why they are structural" is exactly the right way to report it.

## What not to borrow

- **libkrun in place of Firecracker, in the current product.** No supported
  memory-snapshot suspend means no reaper and no resume-on-connect, which is
  most of what makes Sparkbox pleasant. Revisit only if libkrun checkpoint/
  restore becomes a first-class, documented feature rather than an env-var-gated
  best-effort control socket.
- **Their DNS filter in place of Sluice.** `src/dns_filter.rs` is a 372-line
  suffix-match allowlist that returns NXDOMAIN, with IP enforcement bolted on at
  host-socket creation. Sluice is strictly stronger: kernel-level TCX
  enforcement that survives a guest ignoring the resolver, plus per-domain byte
  accounting we bill and meter against. Their model would be a downgrade.
- **Their guest kernel.** libkrunfw bakes the kernel into the library. We pin,
  verify, and (for the macOS nested path) customize ours.

## The one spike worth running

Two weeks, on a scratch k3s node, answering one question: **can a Sparkbox
sandbox be a Pod without losing egress control?**

1. Install the smolvm runtime on a k3s node (`deploy/k3s/install-smolvm-k3s.sh`)
   and run a Sparkbox-shaped guest — our rootfs contents, our agent CLIs — as a
   Pod with `runtimeClassName: smolvm`.
2. Measure cold boot, fork-from-golden, and stop/start against our Firecracker
   numbers on the same hardware.
3. **Determine where Sluice attaches.** The `netns_tap` bridge puts the tap in
   the CNI netns; can Sluice's TCX programs attach there, or does enforcement
   have to become a per-sandbox `CiliumNetworkPolicy` (losing DNS-learned
   per-tag rules and byte accounting)? A clean answer here decides Option A
   outright.
4. Confirm what a Pod restart does to guest state, and whether a PVC-backed
   rootfs survives rescheduling.

Everything else in this document can proceed in parallel and does not depend on
the outcome. Borrow (1) and (2) now.

## Sources

- [smol-machines/smolvm](https://github.com/smol-machines/smolvm) — read at `v1.10.1` / `159fe30`, 2026-08-23
- [smolvm Kubernetes runtime README](https://github.com/smol-machines/smolvm/blob/main/deploy/README.md)
- [smolvm lossless serve restart](https://github.com/smol-machines/smolvm/blob/main/docs/lossless-serve-restart.md)
- [libkrun](https://github.com/containers/libkrun) / [libkrunfw](https://github.com/containers/libkrunfw) (upstream of their forks)
- [CelestoAI/SmolVM](https://github.com/CelestoAI/SmolVM) — the name collision, not evaluated
- Sparkbox: [`deploy-cks.md`](deploy-cks.md), [`cks-reflink-persistence-plan.md`](cks-reflink-persistence-plan.md), [`security-hardening.md`](security-hardening.md), and [`internal/vmm/driver.go`](../internal/vmm/driver.go)
