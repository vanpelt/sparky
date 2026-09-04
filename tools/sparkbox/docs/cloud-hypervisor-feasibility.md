# Cloud Hypervisor feasibility: nested virtualization on CKS

Status: research spike, no code changed. Written 2026-09-04.

Companion to [security-hardening.md](security-hardening.md) (the CKS boundary
this has to preserve), [cks-reflink-persistence-plan.md](cks-reflink-persistence-plan.md)
(the reflink disk model this must not break) and
[resource-model-design.md](resource-model-design.md) (the exe.dev-shaped
density target). The prompt: exe.dev now offers nested virtualization, and
Sparkbox cannot, because Firecracker will not.

## Conclusion

**Switching the VMM to Cloud Hypervisor is feasible and contained.** Everything
Sparkbox is actually built on — reflink-cloned raw ext4 disks, the pause/resume
snapshot model, the balloon reaper, per-slot TAPs with the iptables/Sluice
perimeter, the gateway/node split, the capability-free controller with a narrow
privileged helper, the KVM/TUN device plugin, VAST checkpoints — is either
VMM-agnostic or maps one-to-one onto a Cloud Hypervisor feature. The change is
one driver package, the helper's `exec`, the asset pins, and two kernel-config
lines. Nothing above `internal/vmm` needs to know.

**Nested virtualization is the only reason to do it, and it is a real one.**
Firecracker masks VMX/SVM from every guest and has no option to stop; Cloud
Hypervisor has had `--cpus nested=on|off` since v50.0 (December 2025), and its
KVM backend saves and restores nested vCPU state, so a paused sandbox with an
inner VM still running is at least *designed* to survive our snapshot-to-disk
pause.

**The risk is not Cloud Hypervisor. It is KVM.** Exposing nested virtualization
hands guest root the host kernel's nested-VMX/SVM and shadow-MMU code, which
produced two public guest-to-host escapes this summer (Januscape,
CVE-2026-53359, July; Zapscape, CVE-2026-64561, August). On CKS we do not
control the node kernel. So nested must be **off by default and opt-in per
sandbox**, admitted only on a Node whose kernel we have verified carries both
fixes, and preferably on a dedicated node pool.

**No Kata.** Kata answers a different question (make a Pod a VM); Sparkbox's unit
is an owner's sandbox with pause-to-disk, rename, resize, fork and checkpoint,
none of which exist at the CRI. Kata would also replace the raw-file disk we
reflink with a snapshotter-managed rootfs, and it needs node-level containerd
changes a managed CKS cluster may not permit. The "narrow node runtime owns
virtualization" model in `security-hardening.md` already gives us Kata's
security shape with a smaller protocol; keep it and swap the binary under it.

**Recommendation:** build `internal/vmm/cloudhypervisor` beside the Firecracker
driver, prove parity through the existing mock-driver-shaped tests plus one CKS
canary, and make nested a per-sandbox capability gated on a node preflight.
Firecracker stays the default until the gates in §9 pass. The rest of this note
is the evidence and the plan.

A note on words: "linking fnodes" in the ask is the XFS reflink clone
(`cp --reflink=always`, Linux `FICLONE`) that lets many sandboxes share one
base image's extents. That is what §6 means by reflink throughout.

## 1. Why the VMM decides this

Sparkbox on any host is only as capable as the CPU its VMM shows the guest.

| | Firecracker v1.16.1 (what we ship) | Cloud Hypervisor v53.0 (July 2026) |
| --- | --- | --- |
| Nested virtualization in the guest | Unsupported and unconfigurable: no option, no documentation, no CPU-template knob, and maintainers answer nested-virt requests with "we do not support nested virtualization" (#668, #1721). The CPUID normaliser does *not* explicitly clear VMX/SVM, so what a guest sees is whatever `KVM_GET_SUPPORTED_CPUID` passes through — unsupported territory either way. | `--cpus nested=on\|off`, since v50.0 (2025-12-19). **Default `on`.** x86-64 only. `nested=off` clears the VMX/SVM CPUID bits *and* excludes the virtualization-capability MSRs (`arch/src/x86_64/mod.rs`). |
| Nested state in snapshot | None: `vstate/vcpu.rs` has no `KVM_GET_NESTED_STATE` path, so a pause-to-disk of a guest with a live inner VM cannot be correct. | vCPU state includes `KVM_GET_NESTED_STATE` / `KVM_SET_NESTED_STATE` (`hypervisor/src/kvm/mod.rs`). |
| Sandboxing of the VMM | Built-in seccomp; the external `jailer` (needs `CAP_SYS_ADMIN`, which is why CKS uses our chroot launcher instead). | Per-thread seccomp, on by default (`--seccomp true\|false\|log\|errno`); `--landlock` file-access ruleset (v41.0); no jailer, by design (issue #5170 closed without one). Runs unprivileged given `/dev/kvm`. |
| Pause / resume | `mem.snap` + `state.snap`, absolute paths baked in. | `vm.snapshot` → directory of `config.json`, `state.json`, `memory-ranges`; sparse memory file (v52.0); restore modes `copy`, `ondemand` (userfaultfd), `copyonwrite`. `config.json` is editable JSON. |
| Balloon | `deflate_on_oom`, stats polling. | `size`, `deflate_on_oom=on`, **`free_page_reporting=on`**; resize through `/vm.resize desired_balloon`; `/vm.balloon-stats`. |
| Device model | virtio-blk/net/vsock, balloon, serial, i8042, RTC (arm64). ~83 kLoC. | The above plus ACPI, IOAPIC, virtio-console/rng/iommu/pmem/fs, vhost-user, VFIO, TPM, IOMMU. ~106 kLoC. Unused devices are not instantiated, but the code is in the binary. |
| Network device | tap by name. | tap by name, or **`fd=`** for a pre-opened tap (v36.0); `--api-socket fd=` too. |
| SMT side channels | `smt=false` hides siblings from the guest. | Same, plus `core_scheduling=vm\|vcpu\|off` (`PR_SCHED_CORE`) so vCPU threads never share a physical core with another VM's. |
| arm64 | Yes. | Yes, but `nested` "can only be changed on x86-64". |
| Control | REST over Unix socket; we use `firecracker-go-sdk`. | REST over Unix socket, OpenAPI 3.0 spec at `vmm/src/api/openapi/cloud-hypervisor.yaml`; no official Go client (Kata generates one). |

Two consequences fall straight out of that table.

First, this closes the tempting shortcut. Even if a Firecracker guest with a
`CONFIG_KVM` kernel turned out to see `vmx` on a `nested=1` host through CPUID
pass-through (nobody has documented it either way, and M0 costs an hour to
find out), it would be unsupported upstream and, decisively, our pause is a
memory snapshot: Firecracker would silently drop the L2's state and the guest
would resume with a corrupt inner VM. Nested virtualization as a *feature* needs
a VMM that saves nested state, and of the two that is only Cloud Hypervisor.

Second, `nested` defaulting to **on** means a naive Cloud Hypervisor driver would
expose nested virtualization to every sandbox on day one. The driver must pass
`nested=off` unless the sandbox asked, and the helper — which is what actually
builds the command line on CKS — must treat it as a per-launch input.

Third, the two things the CKS hardening doc lists as reasons the chroot launcher
was worth writing (no `CAP_SYS_ADMIN`, a per-VM chroot with only KVM/TUN nodes
and hard links) carry over unchanged, and Cloud Hypervisor lets us go one step
tighter: with `--net fd=` the helper opens the TAP itself and hands over the
descriptor, so the chroot needs **no** `/dev/net/tun` node at all.

## 2. What Sparkbox actually takes from the VMM

An inventory of every Firecracker-shaped thing in the tree, with what happens
to it. The blast radius is small and almost all of it is in one package.

| Where | What is Firecracker-specific | Under Cloud Hypervisor |
| --- | --- | --- |
| `internal/vmm/driver.go` | Nothing. The `Driver` contract and the nine optional capabilities (`Archivable`, `DiskReporter`, `TemplateReporter`, `RootfsPresencer`, `Renamer`, `Rebooter`, `CPUStatser`, `NetStatser`, `DiskResizer`, `Ballooner`) are VMM-neutral. | Unchanged. A second implementation slots in beside `mock` and `firecracker`. |
| `internal/vmm/firecracker/fc.go` (2,153 lines) | 18 call sites into `firecracker-go-sdk`: `sdk.Config` (boot source, one drive, one NIC, `MachineCfg`), `Start`, `PauseVM`, `CreateSnapshot`, `ResumeVM`, `UpdateBalloon`, `GetBalloonStats`, `StopVMM`, the `FcInit` handler chain for jail staging and the balloon. | Those become HTTP calls against `/vm.pause`, `/vm.snapshot`, `/vm.resume`, `/vm.resize`, `/vm.balloon-stats`, `/vm.info`, `/vmm.shutdown`. The VM itself is fully described on the command line at exec (`--kernel --cmdline --disk --net --cpus --memory --balloon --serial --console --api-socket`), which fits the helper's "the controller sends no paths" protocol *better* than Firecracker's configure-after-start API. |
| same file, the other ~1,600 lines | `reflinkClone`, `compact` (e2fsck + zerofree), `ext4DiskMB`, `ResizeDisk`, `RenameVM`, `DropSnapshots`, `Snapshot`/`PackRootfs`/`UnpackRootfs`, `installAuthorizedKey`, `sanitizeTemplate`, tap create/delete, slot and address maths, `kernelArgs`, `machineIDFor`. | **VMM-agnostic.** Lift into a shared package (`internal/vmm/rootfs`, `internal/vmm/slots`) so the two drivers share one implementation and one set of tests. This is most of the port. |
| `internal/vmhelper/server_linux.go` | `exec.Command("/firecracker", "--api-sock", ...)` under chroot + slot uid; links `vmlinux`, `rootfs.ext4`, and on resume `mem.snap`/`state.snap`; `prepareSnapshotOutputs` pre-creates `*.next` files owned by the slot uid. | Exec `/cloud-hypervisor` with the full argument list derived from name, slot and a new `nested` flag; pass the TAP as an inherited fd; link a snapshot *directory* instead of two files (or three files by name). Protocol version bump for the `nested` field. Everything else — `SO_PEERCRED`, `openat2` `RESOLVE_BENEATH`, TAP source pinning, Sluice readiness, stop handshake — is untouched. |
| `cmd/sparkbox/driver_linux.go`, `internal/hostsetup/{manifest,checks,config,steps}.go` | `firecracker-<arch>` / `jailer-<arch>` assets, `SHA256_FIRECRACKER`, `SHA256_JAILER`, `--jailer`, version pairing check. | Add `cloud-hypervisor-<arch>` + `SHA256_CLOUD_HYPERVISOR`; a `--vmm firecracker\|cloud-hypervisor` selector; no jailer asset for the new VMM. |
| `deploy/kubernetes/entrypoint.sh`, `hack/check-cks-pin.sh`, `deploy.sh` | Pinned Firecracker/jailer checksums per arch; downloads at Pod start. | Same shape, one more pinned artifact; `check-cks-pin.sh` gains a row. |
| `.github/workflows/build-artifacts.yml`, `go.yml` | `FIRECRACKER_VERSION: v1.16.1` download and re-publish. | Upstream ships static musl binaries for both arches (`cloud-hypervisor-static`, `cloud-hypervisor-static-aarch64`); download, verify, re-publish under our name exactly as Firecracker is today. |
| Guest kernel (`hack/build-kernel.sh`, `kernel-config.fragment`) | Firecracker CI 6.1 config + our fragment. Booted as ELF `vmlinux` (PVH) on x86, `Image` on arm64. | **The same kernel boots.** The base config already has `CONFIG_PCI`, `CONFIG_PCI_MSI`, `CONFIG_VIRTIO_PCI`, `CONFIG_ACPI`, `CONFIG_PVH`, `CONFIG_VIRTIO_BALLOON`, `CONFIG_VIRTIO_MEM` (and `PCI_HOST_GENERIC` on arm64), verified 2026-09-04 against upstream `microvm-kernel-ci-{x86_64,aarch64}-6.1.config`. To *use* nested virtualization inside the guest it additionally needs `CONFIG_VIRTUALIZATION=y`, `CONFIG_KVM=y`, `CONFIG_KVM_INTEL=y`, `CONFIG_KVM_AMD=y` — the CI config has `# CONFIG_VIRTUALIZATION is not set`. Two fragment lines and a `build-kernel.sh` assertion. |
| Kernel command line (`fc.go kernelArgs`) | `console=ttyS0 reboot=k panic=1 pci=off quiet ip=…:eth0:off sparkbox_host=… systemd.machine_id=…` | Drop `pci=off` (Cloud Hypervisor is virtio-pci). Add `root=/dev/vda rw` explicitly (the SDK's `IsRootDevice` did that for us). Add `net.ifnames=0` unless the image already pins it: on a PCI bus udev would rename `eth0` to `enp0s…` and the `ip=` line, `sparkbox-netcfg` and Sluice's `sbtapN`↔`eth0` assumptions all name `eth0`. Every `sparkbox_*` token is unchanged. |
| Guest rootfs (`images/Dockerfile`, `hack/build-rootfs.sh`) | Nothing VMM-specific; root is `/dev/vda` on both. | Unchanged. |
| Everything above the driver | `host.Manager`, `ctlops`, fleet, checkpoints, archive, Sluice, metadata, edge, consoles. | Unchanged by construction: they only see `vmm.Driver` and files on disk. |

The port is therefore: one new driver of roughly the size of `fc.go`'s API half
(a few hundred lines once the disk/slot code is shared), a generated or
hand-rolled client for ten REST routes, a helper change, and asset plumbing.
It is not a rewrite.

## 3. Mapping onto the CKS deployment

The Pod in `deploy/kubernetes/deployment.yaml` does not change shape.

```text
sparkbox-node Pod (pinned to one CKS bare-metal Node)
  prepare-vm-assets   root, SYS_ADMIN, loop bundle, exits before any guest    unchanged
  vmm-helper          root, ten caps, sparkbox.dev/kvm + tun                   execs cloud-hypervisor instead of firecracker
  sluice              BPF/NET_ADMIN, TCX on sbtapN                             unchanged
  sparkbox-node       uid 65532, no caps, read-only root                       new driver, same protocol
device plugin DaemonSet (kvm / tun / loop)                                     unchanged
```

What each boundary looks like after the swap:

- **VMM process.** Still a child of the helper, still `chroot` + `100000+slot`
  uid/gid + empty environment + no capabilities, still charged to the helper's
  cgroup for memory. Cloud Hypervisor's own seccomp is on by default and is
  per-thread; add `--landlock` with rules for exactly the chroot's handful of
  files. With `--net fd=` and `--api-socket fd=` the jail can shrink to the
  binary, `/dev/kvm`, the kernel and the disk (plus `/dev/urandom` if the
  static binary wants it — verify in M1). The `sparkbox.dev/tun` allocation
  stays with the helper, which is the process that opens it.
- **Controller.** Unchanged privileges. It talks to the API socket the helper
  publishes at mode 0660 in the jail root, as today.
- **Network perimeter.** Unchanged. Every guest packet still exits via
  `sbtapN`, so the per-TAP anti-spoof rule, `SPARKBOX_GUEST_{OUT,IN,HOST}`,
  Sluice's DNS-derived grants, the Cilium egress policy and the metadata
  service's tap check all hold. An L2 guest inside a sandbox reaches the world
  through the L1's `eth0`, which is the same tap; it cannot get a second
  interface because the VMM only has one.
- **Disks.** Raw ext4 file on the Node-local XFS hot tier, reflink-cloned from
  `images/` or `templates/`, checkpointed to VAST as today. See §6.
- **Snapshots.** The helper currently pre-creates `mem.snap.next` and
  `state.snap.next` owned by the slot uid and hard-links them into the jail so
  the VMM never receives a path. Cloud Hypervisor writes three named files into
  a directory; the helper pre-creates a `snap.next/` directory the same way.
  The "never snapshot over the files you were restored from" rule still holds.
- **Templates and checkpoints** are reflinks of the disk plus `e2fsck` and
  `zerofree`; none of that touches the VMM.

Two Node-level preconditions are new, and neither is ours to set:

1. **`kvm_intel.nested` / `kvm_amd.nested` must be `1` on the Node.** It has
   been the upstream default for years, but "set it to 0" is the published
   mitigation for both 2026 escapes, so a hardened fleet may have turned it
   off. It is readable from inside the helper container at
   `/sys/module/kvm_{intel,amd}/parameters/nested`.
2. **The Node kernel must carry both fixes** before any sandbox is admitted
   with nested on: CVE-2026-53359 (fixed in 6.1.177, 6.6.144, 6.12.95,
   6.18.38, 7.1.3) and CVE-2026-64561 (fixed in 6.6.148, 6.12.101, 6.18.42,
   7.1.6). We can read `uname -r`; we cannot patch it. That is a question for
   CoreWeave, and the answer may differ per node pool.

`hack/probe-nested-virt.sh` checks both, plus the CPU flags, from any Linux
shell including `kubectl exec … -c vmm-helper`. Run it on `g084f44` before
anything else in §9.

## 4. Kata Containers: not for this

Kata would put a Cloud Hypervisor under every Pod that selects its
`RuntimeClass`. It is the wrong tool here for five independent reasons.

1. **The unit of management is wrong.** Kata makes *a Pod* the VM and gives you
   the CRI verbs: create, start, stop, delete. Sparkbox's unit is an owner's
   sandbox that is paused to disk and resumed on connect, renamed, resized,
   forked into a template, checkpointed to object storage and archived. None of
   those are Pod operations; we would be reaching around the shim for every one.
2. **The disk is wrong.** Kata's guest rootfs comes from the containerd
   snapshotter — overlayfs exported over virtio-fs, or a devmapper/erofs block
   snapshot. The efficiency the ask names, one user's many sandboxes sharing a
   base image's extents, is delivered today by XFS reflinks of a raw ext4 file
   that we also `e2fsck`, `zerofree`, grow, reflink for checkpoints, and ship
   to VAST. Kata has no notion of that file. We would lose `DiskReporter`,
   `TemplateReporter`, `DiskResizer`, checkpoints and templates in one move.
3. **CKS is managed.** `kata-deploy` is a privileged DaemonSet that writes the
   node's containerd configuration and restarts it. Whether a CKS tenant may do
   that on a bare-metal CPU pool is unverified, and the answer being "no" is
   plausible. The current design needs only a device plugin and a Pod.
4. **The security shape is already ours.** Kata's real merit is an
   out-of-process runtime owning the hypervisor in its own cgroup. That is
   precisely the `vmm-helper` model in `security-hardening.md`, with a path-free
   four-verb protocol instead of the OCI/agent/virtiofsd surface Kata adds to
   the trusted base.
5. **Nested would become cluster-wide.** Cloud Hypervisor's `nested` defaults
   to on and we found no per-Pod toggle for it in Kata's hypervisor
   configuration; the choice would be made once for the RuntimeClass, not once
   per sandbox, which contradicts §7.

Keep the architecture; change the binary.

## 5. exe.dev, for calibration

exe.dev's public pages do not name their hypervisor, and their docs and blog say
nothing we could quote about how nested virtualization is provisioned. Treat
"exe.dev has it" as the product bar, not as an implementation reference. What
is documented in the ecosystem is consistent with §1: every Firecracker-based
agent sandbox (E2B, Vercel, Fly Sprites, Unikraft) lacks it, and the published
2026 write-ups that recommend a VMM for Docker-in-Docker, Android emulators or
`/dev/kvm` inside the VM all land on Cloud Hypervisor.

## 6. Reflink ("linking fnodes") under Cloud Hypervisor

Unchanged, and slightly better.

- `--disk path=<jail>/rootfs.ext4` is a raw file; `cp --reflink=always` from
  `images/universal.ext4` or `templates/snap-<owner>-<name>.ext4` produces it
  exactly as now, on the same XFS hot tier, under the same
  `--reflink=always`-or-fail policy. Forks, `TemplateUsageMB` baselines,
  pooled accounting, checkpoint staging clones: all identical.
- `--disk … sparse=on` (v51.0) turns guest `DISCARD`/`WRITE_ZEROES` into
  host hole-punching. A guest `fstrim` then returns blocks to XFS while the
  sandbox runs, which shrinks checkpoints and the used-block baseline without
  waiting for the pause-time `zerofree`. On a reflinked file a punch simply
  unshares that extent, so it is safe against the template.
- Two Cloud Hypervisor features that *look* like sharing are rejected on the
  spot. **qcow2 backing files** (v51.0) put a guest-writable image format in
  front of host userspace parsing; CVE-2026-27211 was exactly a backing-file
  path abuse, and `security-hardening.md` already treats host parsing of
  guest-authored bytes as debt to remove, not add. **virtio-fs / vhost-user**
  adds a `virtiofsd` to the trusted base and breaks the one-file block model the
  checkpoint and template machinery is built on.

So: the reflink story is a reason this port is cheap, not something it puts at
risk.

## 7. Security and perimeter

| Boundary | Today (Firecracker) | With Cloud Hypervisor | Net |
| --- | --- | --- | --- |
| Guest ↔ VMM | Small device model, built-in seccomp. | Larger device model (ACPI, IOAPIC, i8042, serial, virtio-console/rng/iommu present by default; vhost-user, VFIO, TPM, pmem compiled in). Per-thread seccomp on by default; Landlock. Public CVEs: API fd-close (2023-30612), qcow backing exfiltration (2026-27211), virtio-blk async UAF (2026-45782). | **Wider.** Mitigated by not configuring optional devices, by the existing chroot/uid, and by Landlock. Advisories become a release gate, which the hardening doc already asks for. |
| VMM ↔ host | chroot, slot uid, no caps, KVM+TUN nodes in jail. | Same, minus the TUN node (`--net fd=`), plus Landlock. | **Tighter.** |
| Guest ↔ network | `sbtapN`, per-TAP pinning, named chains, Sluice, Cilium. | Identical; keyed on tap names, not the VMM. | **Unchanged.** |
| Guest ↔ KVM | Guest sees no VMX/SVM; L0 nested code is unreachable. | With `nested=on`: guest root can drive L0's nested VMX/SVM and shadow MMU. Januscape (CVE-2026-53359, 16 years latent, both vendors, public PoC) and Zapscape (CVE-2026-64561, public PoC) were both reachable only this way. | **The material change**, and it is a property of enabling nested, not of the VMM. |
| Cross-VM CPU side channels | `smt=false` in the guest. | `core_scheduling=vm` keeps other VMs' vCPUs off the same core. | **Tighter**, opt-in. |

Policy that follows:

- **`nested=off` is the driver default.** Firecracker parity is the baseline;
  nothing gets more than it has today without asking.
- **Opt-in per sandbox**, carried like any other resource: a `vmm.Config`
  field, a helper protocol field (version 2), an admission check in
  `host.Manager`, and a user-facing knob on the `new@` door / environment
  (`--nested`, or a `nested` tag rule) so it shows in `ctl info`.
- **Admission is gated on the Node**, not the request: nested is admitted only
  when the helper's startup preflight (the same checks as
  `hack/probe-nested-virt.sh`) has found `nested=1` and a kernel at or past
  both fix versions; otherwise the request fails with a typed control error.
  This is the one part of the design that cannot be "just enabled" on CKS:
  it depends on CoreWeave's kernel cadence.
- **Prefer a dedicated node pool** for nested-enabled sandboxes. A
  guest-to-host escape from a nested-enabled sandbox lands on a shared bare
  metal Node; keeping such sandboxes off the pool that holds everybody else's
  is the cheapest containment available and it is one `nodeSelector`.
- **Remove the loop bundle** (already item 1 of the hardening doc's remaining
  work) before widening anything else; the init container is the only place a
  guest-shaped filesystem is still parsed by the host kernel.

What nested does *not* change: memory accounting (L2 RAM is inside the L1's
allocation; the balloon still reclaims free pages, and `deflate_on_oom` still
protects the L1), the metadata identity check, egress metering, or the owner
pool maths.

## 8. Open questions only hardware can answer

None of these is a reason not to start; all of them are reasons not to cut
over before M2.

1. **Nested state through our pause.** Cloud Hypervisor snapshots carry
   `KVM_GET_NESTED_STATE`, but nobody has shown *our* sequence (pause → snapshot
   to `.next` → kill VMM → resume from files later, on a possibly different
   Cloud Hypervisor build) with an L2 mid-flight. Test: `kind` or a Firecracker
   VM inside the sandbox, pause the sandbox, resume, confirm the inner VM is
   alive. If it is not, the fallback is to refuse pause-with-snapshot for
   nested sandboxes and cold-boot them instead — the same thing `Reboot`,
   `Resize` and `Rename` already do via `DropSnapshots`.
2. **Balloon versus an L1 running KVM.** KVM pins nothing by default, so the
   balloon should still take free pages, but `free_page_reporting` and an L1
   that has just given 4 GiB to an L2 need measuring, not assuming.
3. **Latencies and overhead.** Boot, resume (`copy` vs `ondemand`), per-VM
   RSS, and L2 CPU performance (nested EPT/NPT is the difference between
   "usable" and "demo"). Extend `hack/measure-density.py`.
4. **Node kernel and `nested` on `g084f44`,** per §3. This one is a five-minute
   probe and it decides whether the CKS half of this is a 2026 or 2027 story.
5. **arm64.** The DGX Spark and the macOS nested machine are arm64.
   Cloud Hypervisor runs there and the driver must be exercised there for
   parity, but nested is x86-only in Cloud Hypervisor, arm64 KVM nested
   support in mainline is recent and FEAT_NV2-only, and whether Grace exposes
   it under CoreWeave's or Apple's outer layers is unverified. Plan arm64 as
   "same driver, nested unavailable", and say so in `doctor`.
6. **Cloud Hypervisor advisory cadence** as a release-gate input, alongside
   Firecracker's and KVM's. Someone has to own watching it.

## 9. Spike plan

Milestones follow the repo's convention: each is independently mergeable and
each ends in a measured result, not a belief.

### M0 — Node preflight (hours)

Run `hack/probe-nested-virt.sh` in the live `vmm-helper` container. While
there, boot one throwaway Firecracker guest with a `CONFIG_KVM` kernel and
read its `/proc/cpuinfo`: whether `vmx` shows up settles the pass-through
question in §1 as a recorded fact rather than an inference. Record
CPU vendor and flags (`vmx`/`svm`, `ept`/`npt`), `nested` module parameter,
kernel version and its status against both CVEs, and `/dev/kvm`. Ask
CoreWeave whether nested is intentionally disabled anywhere and what their
kernel patch cadence is for the CPU pools. Outcome: a go/no-go for M2 on the
*current* Node, and a list of pools where it would be a go.

### M1 — Driver beside driver (days)

- Lift the VMM-neutral half of `fc.go` into shared packages with the existing
  tests moved, not duplicated.
- `internal/vmm/cloudhypervisor`: full `Driver` plus every optional capability
  the Firecracker driver implements, so `host.Manager` sees no difference. A
  thin hand-written client for the ten routes we use, or `oapi-codegen` from
  the pinned OpenAPI file; either way pin the Cloud Hypervisor version the
  same way the Firecracker version is pinned.
- `nested` in `vmm.Config`, default false; `--cpus nested=off` unless set;
  `--balloon size=0,deflate_on_oom=on,free_page_reporting=on`;
  `--memory size=…,thp=on`; `--serial file=<jail>/console.log`; `--console off`;
  `--seccomp true`; `--landlock` with rules for the jail files.
- Kernel: drop `pci=off`, add `root=/dev/vda rw net.ifnames=0`; fragment gains
  `CONFIG_VIRTUALIZATION=y CONFIG_KVM=y CONFIG_KVM_INTEL=y CONFIG_KVM_AMD=y`
  with `build-kernel.sh` asserting them.
- Helper: protocol v2 with `nested`; exec Cloud Hypervisor with the TAP as an
  inherited fd; snapshot directory instead of two files; jail without a TUN
  node.
- `setup`/`doctor`/manifest: `cloud-hypervisor-<arch>` asset, `--vmm` flag,
  `doctor` reports nested availability per §3.
- Acceptance: `go test ./...` green; on a KVM host, the existing Firecracker
  Linux tests pass against the new driver (parametrise by driver as the fleet
  e2e tests are parametrised by placement); a sandbox boots, SSHes, pauses,
  resumes, forks, checkpoints, restores, renames, resizes and reboots with the
  same observable behaviour.

### M2 — CKS canary (days)

- Ship the driver in the CKS image behind `SPARKBOX_VMM=cloud-hypervisor`,
  nested off. Roll one Pod. Run the §8 measurements. Roll back is the env var.
- Enable nested for one owner-scoped sandbox on a Node that passed M0. Inside
  it: `kind create cluster` (containers) and one real KVM guest (Firecracker
  or Cloud Hypervisor with a stock kernel). Pause/resume with the inner VM
  running (§8.1). Record whether Sluice, the metadata service, egress metering
  and the balloon behave.
- Acceptance: numbers for boot, resume, RSS, L2 throughput; a written answer to
  §8.1; no regression in the `security-hardening.md` acceptance gates.

### M3 — Decide the default (a meeting)

Inputs: M2 numbers, the advisory cadence of both VMMs, and CoreWeave's answer
from M0. Outputs: either Cloud Hypervisor becomes the CKS default with nested
opt-in, or it stays the opt-in VMM for nested-requesting sandboxes only (both
drivers can coexist on one node, keyed per sandbox, because the helper builds
the command line per launch). Either is a legitimate end state; the second is
where I would expect this to land first.

### Not in the spike

Live migration, `virtio-mem` hot-add, VFIO/GPU passthrough, the offloaded
snapshot daemon, TDX/SEV. Each is a reason Cloud Hypervisor might be worth
having later; none is a reason to switch now, and listing them keeps the
decision honest about what it is actually buying: nested virtualization.

## Sources

- Cloud Hypervisor v50.0 release (2025-12-19), `nested=on|off`: <https://www.cloudhypervisor.org/blog/cloud-hypervisor-v50.0-released/>
- Cloud Hypervisor release notes (v41 Landlock, v51 sparse/discard and qcow2 backing files, v52 sparse snapshots and userfaultfd restore, v53 offloaded snapshot daemon): <https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/release-notes.md>
- `docs/cpu.md` (`nested` default on, x86-64 only; `core_scheduling`): <https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/docs/cpu.md>
- `docs/snapshot_restore.md`, `docs/balloon.md`, `docs/landlock.md`, `docs/seccomp.md`, `docs/api.md`, `docs/device_model.md`: <https://github.com/cloud-hypervisor/cloud-hypervisor/tree/main/docs>
- OpenAPI spec (`CpusConfig.nested` default true, `VmResize.desired_balloon`, `/vm.balloon-stats`): <https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/vmm/src/api/openapi/cloud-hypervisor.yaml>
- Nested state in vCPU snapshots: <https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/hypervisor/src/kvm/mod.rs>; VMX/SVM masking when `nested=false`: <https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/arch/src/x86_64/mod.rs>
- "Sandboxing Cloud Hypervisor", issue #5170: <https://github.com/cloud-hypervisor/cloud-hypervisor/issues/5170>
- Cloud Hypervisor advisories: <https://github.com/cloud-hypervisor/cloud-hypervisor/security/advisories/GHSA-g6mw-f26h-4jgp> (CVE-2023-30612), <https://github.com/cloud-hypervisor/cloud-hypervisor/security/advisories/GHSA-jmr4-g2hv-mjj6> (CVE-2026-27211), CVE-2026-45782 via the v52.x notes
- Firecracker guest kernel CI configs (PCI/virtio-pci/ACPI present, `CONFIG_VIRTUALIZATION` not set): <https://github.com/firecracker-microvm/firecracker/tree/main/resources/guest_configs>
- Firecracker maintainers on nested virtualization: <https://github.com/firecracker-microvm/firecracker/issues/668>, <https://github.com/firecracker-microvm/firecracker/issues/1721>; CPUID normaliser with no VMX/SVM mask: <https://github.com/firecracker-microvm/firecracker/blob/main/src/vmm/src/cpu_config/x86_64/cpuid/normalize.rs>; vCPU snapshot state without nested state: <https://github.com/firecracker-microvm/firecracker/blob/main/src/vmm/src/vstate/vcpu.rs>
- 2026 microVM survey (Firecracker vs Cloud Hypervisor for agent sandboxes): <https://emirb.github.io/blog/microvm-2026/> (2026-03-27)
- Januscape, CVE-2026-53359: <https://thehackernews.com/2026/07/16-year-old-linux-kvm-flaw-lets-guest.html>, <https://tuxcare.com/blog/januscape-exposes-the-kvm-shadow-paging-bug-that-kept-coming-back/>
- Zapscape, CVE-2026-64561: <https://thehackernews.com/2026/08/new-zapscape-kvm-flaw-could-let.html>, <https://tuxcare.com/blog/zapscape-cve/>
- Kata Containers with Cloud Hypervisor and block-snapshotter requirements: <https://katacontainers.io/blog/kata-containers-with-cloud-hypervisor/>, <https://github.com/kata-containers/kata-containers/tree/main/docs/how-to>
- CKS runs Kubernetes on bare metal without a hypervisor: <https://docs.coreweave.com/docs/products/cks>
- arm64 KVM nested virtualization (FEAT_NV2-only series): <https://lists.infradead.org/pipermail/linux-arm-kernel/2023-November/883328.html>
