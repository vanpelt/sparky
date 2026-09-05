# Cocoon as Sparkbox's VM engine: evaluated, declined, and mined

Status: evaluation, 2026-09-04. No code changed.

[cocoonstack/cocoon](https://github.com/cocoonstack/cocoon) is a Go MicroVM engine
built on Cloud Hypervisor — OCI and cloud images, snapshot and clone via reflink,
CNI networking, a Docker-like CLI, MIT licensed. It overlaps Sparkbox's
`internal/vmm` layer closely enough to be worth a serious look, and it is built on
the VMM [cloud-hypervisor-feasibility.md](cloud-hypervisor-feasibility.md)
recommends moving to.

**Conclusion: keep our own driver. Read cocoon's Cloud Hypervisor backend before
writing ours, and lift the specific problems it has already solved.**

The reason is not quality. Cocoon is well built — the interface is close to ours,
the clone story is ahead of ours, and its bookkeeping is more thoroughly tested
than our VMM layer is. It is the wrong *shape* for this service in three
structural ways, and one of them is a security regression we would not accept
from our own code.

## Repository health, for the record

| | |
|---|---|
| commits / authors | 588, of which **580 from one author** |
| age | first commit 2026-02-22, active daily (last: 2026-09-04) |
| latest tag | **v0.6.2** — pre-1.0, no `/v2` module path |
| license | MIT |
| API stability | none promised; recent commits explicitly cut exported API |
| tests | ~19.5k test LOC against ~28.7k non-test — a high ratio |
| **CI boots a VM** | **never** — `go test -race` on plain `ubuntu-latest`, no `/dev/kvm`, `Makefile` budget 120s |

The test ratio is genuinely good and the coverage is real, but it covers
*decision logic*: state machines, lease protocols, GC, JSON patching, argv
construction. Whether a VM boots, whether TC redirect passes packets, whether the
guest overlay assembles — none of that is validated by any CI. We would need our
own boot-level suite on day one either way (we have the same gap), so this is not
a differentiator; it is a reason not to mistake the green badge for inherited
confidence.

## Why it does not fit

### 1. There is no seam for an unprivileged controller

This is the blocker, and it is architectural rather than a missing feature.

Cocoon is importable Go — `hypervisor.Hypervisor` is an ordinary exported
interface, there are no `internal/` packages, and constructors are
dependency-injected. But the privileged work happens **in the calling process**,
interleaved inside the lifecycle sequences under held locks
(`hypervisor/start.go:115-199`):

- `cgroup.Prepare` — `mkdir` and writes under `/sys/fs/cgroup/cocoon.slice`
- `EnterNetns` → `unix.Setns` (CAP_SYS_ADMIN)
- `unix.Setrlimit(RLIMIT_MEMLOCK, INFINITY)` (CAP_SYS_RESOURCE)
- named netns creation at `/var/run/netns` (CAP_SYS_ADMIN)
- TAP via `netlink.LinkAdd` (CAP_NET_ADMIN)

`docs/install.md:8` states the requirement plainly: "Root access (sudo)".

The daemon cannot broker any of it. Its entire IPC surface is three **read-only**
routes (`daemon/api.go:85`) — `GET /healthz`, `GET /v1/vms`, `GET /v1/events`.
There is no create/start/stop over the socket; every mutation is a direct Go call
or a CLI invocation, and the daemon only *observes* VMMs it never spawned.

Sparkbox's node runs an unprivileged controller (uid 65532) that reaches a tiny
privileged helper over a deliberately path-free four-verb protocol
(`internal/vmhelper/protocol.go`): `ping`, `launch`, `snapshot-outputs`,
`cpu-time`. The controller "cannot supply commands, device numbers, credentials,
or arbitrary filesystem paths" — the helper derives every path from immutable
startup config after validating a name and a slot.

Adopting cocoon as a library means moving root into the controller, or forking
`hypervisor.Backend` to hoist netns/cgroup/TAP behind our helper. The first is a
security regression wearing the costume of a dependency upgrade. The second is
not "building on cocoon" in any meaningful sense.

### 2. It has no pause/resume, which is our entire idle model

`pauseVM` and `resumeVM` exist (`hypervisor/cloudhypervisor/utils.go:132,141`)
but are internal, used only inside snapshot/clone/restore windows. There is no
pause verb in the CLI and no method on the interface.

Cocoon's answer to idle is **hibernate**: `HibernateSequence`
(`hypervisor/snapshot.go:97`) atomically snapshots a running VM and stops it, and
you resume with `vm restore`. That is a coherent design — the VMM process dies
only after the persist succeeds, which is careful work — but it is a different
cost curve from ours. Sparkbox scale-to-zero pauses in place and keeps the VMM,
and the reaper drives a balloon to reclaim host memory from VMs it has not
paused.

Cocoon's balloon is emitted once at launch with `deflate_on_oom=on,
free_page_reporting=on` (`hypervisor/utils.go:135`, `args.go:139`) and then never
touched. There is no `vm.resize` call anywhere in the tree; the documented answer
to memory pressure is to tell the guest operator to drop caches.

Against our ten optional capability interfaces, cocoon is missing:
`Ballooner` (no runtime control), `Renamer`, `Rebooter`, `DiskResizer`, and
pause/resume from the core `Driver` itself. It compensates with real cgroup v2
CPU control per VM, which we do not have.

Related: **CPU/memory/storage are fixed at snapshot time** on both its backends
(`docs/known-issues.md`). Clones cannot be resized. If forks ever need to vary
sizing — and turbo mode says they might — that is a redesign, not a flag.

### 3. The image store is mandatory, and our rootfs model does not go through it

`types.BootConfig` has exactly the fields we would want — `KernelPath`,
`InitrdPath`, `Cmdline`, `FirmwarePath` — but nothing in the CLI populates them
directly. Every path routes through a content-addressed image store: OCI layers
are converted to read-only **EROFS** blobs, attached as separate virtio-blk
devices, and assembled in-guest with overlayfs driven by a custom cmdline
(`hypervisor/utils.go:196`):

```
boot=cocoon-overlay cocoon.layers=<...> cocoon.cow=<...>
```

Sparkbox boots one reflink-cloned raw ext4 rootfs plus a pinned `vmlinux-<arch>`.
Getting there through cocoon means repackaging our template as an OCI-ish layer
set and teaching our guest init the overlay contract — for no benefit we
currently want.

### 4. Two smaller things worth knowing

**The headline clone mode needs a forked hypervisor.** The README's "memory
copy-on-write" is `memory_restore_mode: "CopyOnWrite"`, an mmap of the snapshot's
memory file shared across sibling clones — and `docs/snapshots.md:44` says it
"requires the cocoonstack CH build" (`cocoonstack/cloud-hypervisor` `dev`). Stock
v53.0 gives `copy` and `ondemand` only.

This also **corrects our own note** in
[cloud-hypervisor-feasibility.md](cloud-hypervisor-feasibility.md): we recorded
`memory_restore_mode=copyonwrite` as existing only on upstream `main`. The more
precise picture is that `ondemand` (userfaultfd) *is* in released v53.0, and it is
the mmap/CopyOnWrite mode that lives outside upstream releases.

**A second authoritative record store.** Cocoon owns `<root>/meta/meta.db`
(embedded SQLite, or legacy JSON) plus GC that reaps anything under its roots it
does not recognise. We would run two VM registries whose reconcilers race, with no
equivalent of its `NetScope` for "another manager owns these".

## What to take from it

All MIT, all worth reading before writing our Cloud Hypervisor driver.

| What | Where | Why we want it |
|---|---|---|
| CH argv construction | `hypervisor/cloudhypervisor/args.go:92` | A complete, working `--cpus/--memory/--disk/--net/--rng/--watchdog` mapping from a typed config |
| CH `/api/v1` client | `hypervisor/cloudhypervisor/utils.go` | pause, resume, snapshot, restore, add-disk, add-net — **with no `ch-remote` binary**, which is one fewer asset to pin than our M0c harness used |
| **The parallel-clone TAP race** | `hypervisor/cloudhypervisor/clone.go:91` | `vm.restore` reattaches the *snapshot's* TAP names, so concurrent clones of one template race to EBUSY — and `vm.remove-device` only releases a TAP after the guest ACKs the eject, so the obvious fix deadlocks too. Their answer: a throwaway per-clone TAP, then hot-swap NICs while paused. **We will hit this exactly when forking templates.** |
| Clone fan-out mechanics | `hypervisor/utils.go:364` | Hardlink `memory-range*` (every clone MAP_PRIVATEs the same inode, so page cache is shared), reflink the COW disks, plain-copy the rest |
| Clone-burst safety | `lock/vmlock/shared_lease_unix.go` | A shared flock lease per source VM, so `vm rm` of a template waits out an in-flight clone burst |
| `Quiesce` / `Unquiesce` | `network/cni/cni.go:97` | Bring host-side veths down on stop so a stopped VM's TC redirect stops storming softirqs against a carrier-less TAP — the right shape for scale-to-zero |
| Race-free cgroup placement | `hypervisor/cgroup_linux.go:9` | `CLONE_INTO_CGROUP` via `SysProcAttr.UseCgroupFD` |
| **Post-clone CRNG reseed** | `cmd/vm/reseed.go:51` | Pushes 32 bytes of fresh entropy and regenerates machine-id after every clone/restore. **This is a real gap for us**: without it every fork of a template shares the snapshot's CRNG state. We already ship a guest agent that could carry the verb. |
| Post-clone IP conflict | `docs/known-issues.md` | A documented ARP-flap window between source and clone. Worth checking our fork path against. |
| Restore-mode selection | `hypervisor/cloudhypervisor/restore.go:124` | Silently degrades mmap → eager copy under hugepages/shared memory, and fails loud on a misspelled mode because the silent fallback is a ~6× latency regression |

Adding `--cpus nested=on` to cocoon, if we ever wanted to contribute it, is about
five files along the groove `Windows`/`kvm_hyperv=on` already wore — and it would
survive snapshot→clone for free, because `patchCHConfig`
(`hypervisor/cloudhypervisor/patch.go:24`) preserves unknown fields by design.

## What would change this decision

If Sparkbox were greenfield — no unprivileged-controller constraint, no existing
raw-ext4 template model, no pause-based idle loop — cocoon would be a strong base.
The `Hypervisor` interface is close to what we would design, and its
clone-one-snapshot-into-many path is ahead of ours.

The decision would also change if cocoon grew a privileged-broker mode: a socket
that accepts validated, path-free lifecycle requests the way our helper does. That
is not on its roadmap and would be a large change to a project whose model is
"every caller is root, converge lazily".
