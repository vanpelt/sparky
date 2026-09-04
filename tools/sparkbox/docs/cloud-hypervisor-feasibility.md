# Cloud Hypervisor feasibility: nested virtualization on CKS

Status: research spike, no code changed. Written 2026-09-04, revised the same day
after an adversarial audit of every claim below against primary sources (upstream
tags, kernel.org CVE records, and this repo). Where the first draft was wrong,
the correction is stated rather than quietly patched — the errors were
instructive.

Companion to [security-hardening.md](security-hardening.md) (the CKS boundary
this has to preserve), [cks-reflink-persistence-plan.md](cks-reflink-persistence-plan.md)
(the reflink disk model this must not break) and
[resource-model-design.md](resource-model-design.md) (the exe.dev-shaped density
target). Implementation detail lives in two companions written with this one:
[cloud-hypervisor-port-design.md](cloud-hypervisor-port-design.md) (driver
mapping, helper protocol v2, kernel and artifacts) and
[nested-virtualization-design.md](nested-virtualization-design.md) (per-sandbox
plumbing, risk register, kill criteria).

The prompt: exe.dev supports nested virtualization, and Sparkbox cannot, because
Firecracker will not.

## Conclusion

**Switching the VMM to Cloud Hypervisor is feasible. It is not as cheap as it
first looks, and the reason to do it is narrower than "nested virtualization".**

Everything Sparkbox's product is built on — reflink-cloned raw ext4 disks,
templates, checkpoints, per-slot TAPs with the iptables/Sluice perimeter, the
gateway/node split, the capability-free controller behind a narrow privileged
helper, the KVM/TUN device plugin — is either VMM-agnostic or maps onto a Cloud
Hypervisor feature. But the pause path does **not** map one-to-one, and it is the
mechanism the whole node's security model is built around; §3 and the
[port design](cloud-hypervisor-port-design.md) cost it honestly.

**Firecracker's limit is narrower and stranger than the first draft said.**
Firecracker does not *mask* VMX/SVM. Its CPUID normaliser touches only
CPUID.1:ECX bits 15, 24 and 31; everything else is whatever
`KVM_GET_SUPPORTED_CPUID` returns, and KVM sets the VMX/SVM bit there whenever
`kvm_{intel,amd}.nested=1`, which is the upstream default. Sparkbox sets no CPU
template. **So a Sparkbox guest on a stock Node very likely already sees the
`vmx` bit today** — it cannot use it only because our guest kernel is built
without `CONFIG_KVM`. What Firecracker actually lacks is (a) any supported
per-microVM switch, and (b) nested state in its snapshot: it never issues
`KVM_GET_NESTED_STATE` and its serialised MSR list omits the VMX capability MSRs
`0x480`–`0x491`, so pausing a guest with a live L2 loses the inner VM. That
second point is the real, unfixable blocker, and it is why this is a VMM
decision rather than a kernel-config decision.

Two consequences follow immediately, and the second is free:

- The cheapest experiment for the *goal* is not a port. Add `CONFIG_KVM*` to the
  guest kernel and a Firecracker **custom CPU template** setting CPUID.1:ECX[5],
  and an L2 may well boot today. It would be unsupported (upstream states
  in-tree that "Firecracker is not tested with nested virtualization" and ships
  `tests/integration_tests/security/test_nv.py` as a guard), and pause would
  corrupt it — but it answers "does our stack want this" for a day's work. §9 M0
  folds it in.
- The cheapest *hardening* is also not a port. Pinning a masking template
  (T2CL/T2/C3 on Intel, T2A on AMD, all of which clear the bit) would make "our
  guests see no VMX" a property of Sparkbox rather than of CoreWeave's module
  parameters. That is worth doing regardless of what we decide here.

**Nested virtualization is a real capability gap, but the premise needs
restating.** exe.dev does publicly name its VMM — "We happen to use Cloud
Hypervisor, but that's a bit of an implementation detail (and may change!)"
([FAQ](https://exe.dev/docs/faq/how-exedev-works)) — and this repo already
recorded that in [`docs/agentic-sandbox-design.md`](../../../docs/agentic-sandbox-design.md).
The first draft claimed the opposite, having never grepped its own tree. What
exe.dev does **not** document anywhere is nested virtualization: absent from all
117 pages of their `llms.txt`, the blog, the release notes and pricing. So the
right framing is: the closest public analogue to Sparkbox runs in production on
the VMM proposed here, and *if* they offer nested it is undocumented. Freestyle
does advertise it explicitly ("nested virtualization, FUSE, eBPF"). Treat this as
a capability to argue for on its own merits, not parity we can point at.

**The risk is not Cloud Hypervisor. It is KVM's shadow MMU.** Nested puts guest
root on `arch/x86/kvm/mmu/mmu.c` — the legacy path L0 must use to shadow the
nested EPT/NPT an L1 builds — which produced **three** 2026 guest-to-host
escapes, not two: Januscape (CVE-2026-53359), Zapscape (CVE-2026-64561) and its
deferred follow-on CVE-2026-80726 (CVSS 9.3, PR:N). On CKS we do not control the
node kernel. Nested must be off by default, opt-in per sandbox, admitted only on
a Node whose kernel carries **all three** fixes, and preferably on its own pool.

**No Kata.** Kata's unit is a Pod; ours is an owner's sandbox with pause-to-disk,
rename, resize, fork and checkpoint. Its rootfs comes from the containerd
snapshotter, which would discard the reflinked raw ext4 that makes overlapping
base filesystems cheap. `kata-deploy` mutates node containerd, which a managed
CKS tenant may not be permitted to do. And the "narrow node runtime owns
virtualization" model in `security-hardening.md` already gives us Kata's security
shape with a four-verb protocol instead of an OCI/agent/virtiofsd surface. §4
argues the other side before concluding.

**Recommendation unchanged in direction, sharpened in cost:** build
`internal/vmm/cloudhypervisor` beside the Firecracker driver, pin **v52.0 or
later**, and gate nested per sandbox on a node preflight. Budget it as a driver
rewrite plus a new integration-test harness, not as a few hundred lines. §9 has
the plan; the [risk register](nested-virtualization-design.md) has the kill
criteria.

A note on words: "linking fnodes" in the ask is the XFS reflink clone
(`cp --reflink=always`, Linux `FICLONE`) that lets many sandboxes share one base
image's extents. §6 is about that.

## 1. Why the VMM decides this

| | Firecracker v1.16.1 (what we ship) | Cloud Hypervisor v53.0 (July 2026) |
| --- | --- | --- |
| Nested in the guest | Unsupported, and **not masked by default**. The normaliser (`cpuid/normalize.rs`) touches only CPUID.1:ECX 15/24/31; the bit is whatever `KVM_GET_SUPPORTED_CPUID` gives. The only VMX/SVM knob points the other way: static templates T2/T2S/T2CL/C3 *clear* CPUID.1:ECX[5] and T2A clears CPUID.80000001:ECX[2]. We set no template. | `--cpus nested=on\|off`, since v50.0 (2025-12-19). **Default `on`.** `nested=off` clears the same two CPUID bits (`arch/src/x86_64/mod.rs`), which suffices: KVM gates the emulated `IA32_VMX_*` MSRs and CR4.VMXE on that bit. **On AMD it was a silent no-op until v52.0** — the CPUID loop `break`ed on leaf 1 before reaching leaf `0x8000_0001` (fixed by `f57b7c5b`, never backported). **v52.0 is the version floor.** |
| Nested state in snapshot | None. No `KVM_GET_NESTED_STATE`; the serialised MSR list omits `0x480`–`0x491`. A pause with a live L2 loses it. | Saved and restored (`hypervisor/src/kvm/mod.rs`), on every x86-64 vCPU snapshot regardless of the `nested` setting. |
| Sandboxing | Built-in seccomp; external `jailer` needs `CAP_SYS_ADMIN`, which is why CKS uses our chroot launcher. | Per-thread seccomp, on by default (`--seccomp true\|false\|log\|errno`). `--landlock` (v41.0) — but it pins Landlock **ABI v3**, so a node kernel < 6.2 or without landlock in `CONFIG_LSM` fails `vm.create` rather than degrading. No jailer, by design (#5170 closed without one). |
| Pause / resume | `mem.snap` + `state.snap`, two files the helper pre-creates and hard-links. Resume mmaps `mem.snap` `MAP_PRIVATE` — no eager read. | `config.json` + `state.json` + `memory-ranges` into a **pre-existing directory**, all three opened `O_CREAT\|O_EXCL`. Restore modes in **released** versions: `copy` (eager, the default) and `ondemand` (userfaultfd). `copyonwrite` is `main`-only. |
| Balloon | `deflate_on_oom`, `stats_polling_interval_s`, `GET /balloon/statistics` → actual/free/available. | `size` (**bytes**), `deflate_on_oom`, `free_page_reporting`; resize via `PUT /vm.resize {desired_balloon}`. **`/vm.balloon-stats` does not exist in any released version** — it is `main`-only. See §8.2: this is the one capability the port loses. |
| Device model | virtio-blk/net/vsock, balloon, serial, i8042, RTC (arm64). ~83 kLoC. | The above plus ACPI, IOAPIC, virtio-console/rng/iommu/pmem/fs, vhost-user, VFIO, TPM. ~106 kLoC. Optional devices are instantiated only when configured — except **virtio-rng, which is always on** and opens `/dev/urandom` at boot with no flag to remove it. |
| Network device | tap by name. | tap by name, or **`fd=`** for a pre-opened tap (v36.0); `--api-socket fd=` too. |
| Disk | raw file, one flag. | raw file, but `image_type=raw` is **not optional** — omit it on v53.0 and format autodetection additionally refuses all guest writes to sector 0; omit it on `main` and the VM refuses to start. |
| SMT side channels | `smt=false`. | Same, plus `core_scheduling=vm` — **on by default**, silently inert on kernels < 5.14. |
| arm64 | Yes. | Yes. `nested` is x86-only, but since v52.0 the parser *accepts and ignores* `nested=off` on arm64; **before v52.0 it was a hard parse error**, so a v50/v51 pin would refuse to boot any arm64 VM the driver launches. |
| Control | REST over Unix socket via `firecracker-go-sdk`. | REST over Unix socket, OpenAPI 3.0 in-tree. **No Go SDK** — and the SDK does more for us than the routes suggest (machine lifecycle, the `WithProcessRunner` seam the entire vmhelper protocol is built on, API readiness, ordered configure-then-boot, snapshot load). |

Three consequences shape everything downstream.

First, `nested` and `sparse` and `core_scheduling` all **default on**. A naive
driver exposes nested to every sandbox on day one. Pass every security-relevant
flag explicitly, so an upstream default change shows up in a diff.

Second, the helper — not the controller — builds the command line, which suits
us: on a cold boot the VM is fully described in argv. But **restore is a
disjoint command line**, not a variant. Every VM-config flag is in clap group
`vm-config`, which `.requires("vm-payload")`; and if `--kernel` is passed
alongside `--restore`, the cold-boot branch wins and the snapshot is **ignored
with no error**. A paused sandbox would silently cold-boot and lose its memory.
That is the single most dangerous detail in the port.

Third, because the restored config comes from `config.json`, **nested is baked
into the snapshot**. A sandbox created with nested on resumes with nested on
whatever the helper passes. The node gate in §7 must therefore be re-evaluated
on *resume*, not only on create.

## 2. What Sparkbox actually takes from the VMM

The first draft said "the blast radius is small and almost all of it is in one
package". That is true of the *logic* and false of the *plumbing*: 31 Go files
carry a non-comment Firecracker reference, plus 23 shell/YAML/Python files.

| Where | What is Firecracker-specific | Under Cloud Hypervisor |
| --- | --- | --- |
| `internal/vmm/driver.go` | Nothing — except that `Ballooner` bundles the lever and the gauge. | One change needed: split `BalloonStats` out, or accept an error-returning stub. See §8.2. |
| `fc.go`, ~950 lines | `reflinkClone`, `compact`, `ext4DiskMB`, `ResizeDisk`, `Snapshot`/`PackRootfs`/`UnpackRootfs`/`RemoveTemplate`, `installAuthorizedKey`, `sanitizeTemplate`, tap create/delete, slot and address maths, `machineIDFor`. | **VMM-agnostic.** Lift into `internal/vmm/rootfs` + `internal/vmm/slots`, shared by both drivers with one set of tests. |
| `fc.go`, ~100 lines that *look* neutral | `DropSnapshots` (l.1588) and `RenameVM` (l.1612) iterate the literal `[]string{"mem.snap","state.snap"}`; `kernelArgs` bakes in `pci=off` and leans on the SDK's `IsRootDevice`. | **Not agnostic.** A CH snapshot is a directory: `os.Remove` returns `ENOTEMPTY`, and `RenameVM`'s stat-based refusal silently stops matching, leaving an orphan snapshot `Resume` still finds. These need a driver-supplied snapshot-artifact seam. |
| `fc.go`, the remaining ~1,100 lines | `Options`/`vmState`/`Driver`, `New`, jail construction, `Create`, `boot`, `jailedResourcesHandler`, `Pause`, `prepareJailedSnapshotOutputs`, `Resume`, the balloon pair, `Destroy`, `Close`/`stopVMM`. | **Rewritten.** Plus the work `firecracker-go-sdk` does that has no CH equivalent. Cost the port as a driver rewrite. |
| `internal/vmhelper` | `exec.Command("/firecracker", ...)`; the fixed `fc.sock` name; a chroot path embedding the VMM binary's basename that `fc.go:397` recomputes and must match; the `fc-vms/<name>` state dir (mirrored in five places); `prepareSnapshotOutputs`. | New argv builders (cold boot and restore, disjoint), TAP fd passing, **a new snapshot-promotion verb** (§3), protocol v2. Every security invariant — `SO_PEERCRED`, `openat2 RESOLVE_BENEATH`, path-free protocol, stop handshake — is preserved. |
| `cmd/sparkbox/{main,node,devpod,driver_*}.go` | The driver name is an enum in **three** places; the VMM binary is a named flag in two; eleven `firecracker:`-prefixed flags. | Add a case to each; one new binary flag. |
| `internal/hostsetup` | `firecracker-<arch>` asset, `SHA256_FIRECRACKER`, an unconditional firecracker download, and `{"firecracker binary", checkFirecracker}` in `DefaultChecks()` which **hard-fails**. | A healthy Cloud Hypervisor node fails `doctor` today on a binary it never execs. Make the check VMM-aware; parameterise `steps.go`'s freshness triple. |
| `deploy/`, `hack/`, CI | Per-arch pins, the upstream-jailer fallback, the FC/jailer version-pairing assertion, `check-cks-pin.sh`, `stage-artifacts.sh` (the manifest *producer*). | One more pinned artifact, producer first then consumers. |
| `macos/` | `poc.sh` counts running VMs by matching `/proc/<pid>/comm` against the literal `firecracker`, in two places. | Nested is x86-only so macOS gets "same driver, no nested" — but the counters and the `--version` probe still break. |
| Guest kernel | Firecracker CI 6.1 config + our fragment. | Boots under CH on x86_64 unchanged (PCI, PCI_MSI, VIRTIO_PCI, ACPI, PVH all present; CH's loader wants exactly the PVH note). On **arm64 the console breaks silently**: CH's UART is a PL011 and the CI config has `# CONFIG_SERIAL_AMBA_PL011 is not set`. Nested inside the guest additionally needs `CONFIG_VIRTUALIZATION`/`CONFIG_KVM`/`CONFIG_KVM_INTEL`/`CONFIG_KVM_AMD`, which the CI config lacks. |
| Kernel command line | `console=ttyS0 … pci=off …` | Drop `pci=off` (Firecracker inserts it itself). Add `root=/dev/vda rw` (Firecracker derives it from `IsRootDevice`; CH derives nothing). Add `net.ifnames=0` or udev renames `eth0` on a PCI bus and breaks `ip=`, `sparkbox-netcfg` and Sluice. Use `console=ttyAMA0` on arm64. So `kernelArgs` is a function of (VMM, arch), not a lifted constant. |
| Tests | 1,361 lines of Firecracker package tests that **never boot a guest**; every lifecycle e2e runs on the mock driver. | There is no real-VMM parity harness to inherit. **M1 must build one.** Also extend the compile-time capability assertions from four to all ten, or a missing capability silently degrades the fleet. |
| Everything above the driver | `host.Manager`, `ctlops`, fleet, checkpoints, archive, Sluice, metadata, edge, consoles. | Unchanged — **except** the per-sandbox nested flag, which is the first attribute that has to travel from the `new@` door to the VMM's argv and touches eight packages. See [nested-virtualization-design.md](nested-virtualization-design.md). |

## 3. Mapping onto the CKS deployment

The Pod shape does not change:

```text
sparkbox-node Pod (pinned to one CKS bare-metal Node)
  prepare-vm-assets   root, SYS_ADMIN, loop bundle, exits before any guest    unchanged
  vmm-helper          root, ten caps, sparkbox.dev/kvm + tun                   execs cloud-hypervisor
  sluice              BPF/NET_ADMIN, TCX on sbtapN                             unchanged
  sparkbox-node       uid 65532, no caps, read-only root                       new driver
device plugin DaemonSet (kvm / tun / loop)                                     unchanged
```

- **VMM process.** Still a chroot + `100000+slot` uid + empty env + no
  capabilities, charged to the helper's cgroup. With `--net fd=` the jail loses
  its `/dev/net/tun` node — but gains `/dev/urandom`, because virtio-rng is
  always instantiated and opens its source inside the chroot. The node count
  does not fall; one node is swapped for a weaker one. No Pod or device-plugin
  change is needed: the helper holds `CAP_MKNOD` and runc's default device
  cgroup allows `c 1:9 rwm`.
- **Network perimeter.** Unchanged, and keyed on tap names rather than the VMM.
  An L2 inside a sandbox reaches the world through the L1's `eth0` — the same
  tap — so the anti-spoof rule, the named chains, Sluice's DNS grants and the
  Cilium policy all still apply.
- **Snapshots — the one mechanism that does not carry over.** The helper's trick
  is to pre-create `mem.snap.next`/`state.snap.next` with `O_EXCL` in the state
  dir and hard-link them into the jail, so the VMM writes through an inode the
  controller never named. **Neither half survives.** Cloud Hypervisor requires
  the destination directory to already exist and creates all three files itself
  with `O_CREAT|O_EXCL`, so pre-created files are rejected `EEXIST`; and Linux
  forbids hard links to directories, with no `CAP_SYS_ADMIN` for a bind mount
  instead. Files created only inside the jail are then destroyed by
  `cleanupSlot`'s `os.RemoveAll` as soon as the VMM is reaped. Pause therefore
  needs a **new helper verb**: create an empty `snap.next/` before the snapshot,
  then hard-link the three files out after the VMM exits and before the jail is
  torn down. Promotion becomes three links rather than one atomic `rename`
  (`rename(2)` will not replace a non-empty directory), so the controller needs
  a defined recovery for a crash mid-promotion. Cost this as helper work in M1,
  not as a rename.
- **Disks.** Unchanged, with `image_type=raw` mandatory (§6).
- **Resume latency is not free.** Firecracker mmaps `mem.snap` `MAP_PRIVATE`;
  Cloud Hypervisor's default `copy` mode reads the saved ranges into guest RAM
  before the VM runs. `copyonwrite` — the mode that matches what we have — is
  `main`-only. `ondemand` needs userfaultfd, which our jail and the Pod's
  `RuntimeDefault` seccomp profile do not permit and which **fails rather than
  falling back**. So on a released pin, resume gets slower unless we measure
  otherwise.

Three Node-level preconditions, none of them ours to set:

1. `kvm_intel.nested` / `kvm_amd.nested` = 1. (Also: `ept`/`npt` on. With
   second-level paging off, *every* guest runs on the shadow MMU — the code all
   three 2026 escapes live in — nested or not.)
2. The node kernel carries all three shadow-MMU fixes. Floors: **6.1.183,
   6.6.152, 6.12.104, 6.18.45 or 7.1.9** — the binding one is CVE-2026-80726,
   which lands 3–4 stable releases after Zapscape in every series but 6.1.
3. Landlock ABI v3 (kernel ≥ 6.2, landlock in `CONFIG_LSM`), or `--landlock`
   fails VM creation outright.

`hack/probe-nested-virt.sh` checks all of these. Run it on `g084f44` first.

## 4. Kata Containers: not for this

Arguing the other side first, because the first draft did not: Kata *can* use a
block-device rootfs (devmapper/erofs snapshotters), it *does* expose per-pod
hypervisor annotations, and it would hand us an out-of-process runtime owning
the hypervisor in its own cgroup — which is genuinely the right shape.

It still does not fit:

1. **The unit is wrong.** Kata's verbs are the CRI's: create, start, stop,
   delete. Ours are pause-to-disk with resume-on-connect, rename, resize, fork
   to a template, checkpoint to object storage, archive. We would reach around
   the shim for every one.
2. **The disk is wrong.** A Kata block snapshotter manages the rootfs; ours is a
   raw ext4 file we reflink, `e2fsck`, `zerofree`, grow, clone for checkpoints
   and ship to VAST. `DiskReporter`, `TemplateReporter`, `DiskResizer`,
   templates and checkpoints all read or write that file directly.
3. **CKS is managed.** `kata-deploy` is a privileged DaemonSet that rewrites node
   containerd config and restarts it. Whether a tenant may do that on a CKS CPU
   pool is unverified, and "no" is plausible. Our current design needs a device
   plugin and a Pod.
4. **The security shape is already ours**, with a four-verb path-free protocol
   instead of Kata's OCI/agent/virtiofsd surface.

Keep the architecture; change the binary.

## 5. exe.dev, corrected

The first draft asserted exe.dev does not name its hypervisor. It does, in its
FAQ, and this repo had already recorded it in `docs/agentic-sandbox-design.md`.
That is the more useful fact anyway: **the closest public analogue to Sparkbox —
a multi-tenant fleet of persistent per-user VMs on rented bare metal, sold as a
pooled plan — runs in production on Cloud Hypervisor.** They also reserve the
right to change it.

What is *not* public is nested virtualization: nothing in their 117-page docs
corpus, blog, release notes or pricing mentions it, `new`/`resize` expose no
flag, and their default image ships Docker but no qemu/libvirt/kvm. Where they
market "KVM virtual machines, not shared kernels" they mean the outer boundary.
So if the ask came from a support answer or a hands-on test, that provenance
should be recorded here, because no public source establishes it.

Two calibration notes. Containers are not the case for nested — E2B ships a
Docker template on Firecracker, and exe.dev's own docs say their VMs run Docker
normally. The nested case rests entirely on inner *VMs*: an Android emulator, a
guest Firecracker or Cloud Hypervisor, a VM-driver Kubernetes. And Freestyle
advertises "nested virtualization, FUSE, eBPF" on agent VMs with forking and
pause/resume — our exact product shape — so this is a differentiator that at
least one competitor already claims.

## 6. Reflink ("linking fnodes") under Cloud Hypervisor

Unchanged in substance, with three corrections to the first draft.

- The disk line is `--disk path=<jail>/rootfs.ext4,image_type=raw`, and the
  format argument is not decoration. Omit it on **v53.0** and autodetection also
  refuses every guest write to sector 0; omit it on **`main`** and the VM does
  not start (`ImageTypeRequired`). Supply it on v53.0 and the declared type must
  match the detected one — so a guest that writes the QCOW2 magic into its ext4
  boot block (bytes 0–1023, which ext4 does not use and which neither `e2fsck`
  nor `zerofree` rewrites) makes its own sandbox unbootable, and a capture from
  that disk poisons the template and every fork. **`Snapshot` should check those
  four bytes before promoting a capture.**
- **CVE-2026-27211 was our shape, not qcow2's.** The first draft cited it as a
  backing-file abuse. It is host-file exfiltration through *raw* images: a guest
  overwrites its own disk header with a crafted QCOW2 structure naming a host
  path, and autodetection parses it on the next boot — and we expose `Rebooter`.
  Affected 34.0–50.0, fixed in 50.1. Declaring `image_type=raw` is what avoids
  the class, not avoiding qcow2.
- `sparse=on` is the **default**, and it buys less than claimed: our accounting
  reads the ext4 superblock precisely so it is independent of sparse and reflink
  representation, and `PackRootfs` still runs `zerofree` unconditionally. Treat
  it as host-capacity headroom, not as a billing or checkpoint change.

Two further facts the first draft missed, both about who holds the file:

- Cloud Hypervisor takes **byte-range advisory locks** on disk images (v50.0+),
  so the VMM must be provably gone before `PackRootfs` runs `e2fsck` in place.
  The pause path already stops the VMM; keep it that way.
- **Checkpoints do not reflink today.** `internal/host/checkpoint.go` pauses,
  calls `PackRootfs` (in-place `e2fsck` + `zerofree` + `zstd` of the whole
  image), uploads, and cold-boots — roughly 45 s of observed downtime. The
  reflink-staging design is M3 of the persistence plan and is unbuilt. So the
  checkpoint path touches the VMM twice: through `DropSnapshots` (which must
  delete a *directory*) and through that quiescence assumption.

Still rejected: qcow2 backing files (`backing_files=off`, its default) and
virtio-fs/vhost-user, for the reasons the first draft gave.

## 7. Security and perimeter

| Boundary | Today (Firecracker) | With Cloud Hypervisor | Net |
| --- | --- | --- | --- |
| Guest ↔ VMM | Small device model, seccomp. | Larger model, but optional devices are instantiated only when configured — except virtio-rng, the x86 legacy set (i8042, CMOS, debug ports) and ACPI. Per-thread seccomp on by default; Landlock where the node kernel allows. CVEs: CVE-2023-30612 (API fd-close, fixed 30.1/31.1), CVE-2026-27211 (raw-image autodetect, fixed 50.1), CVE-2026-45782 (virtio-blk async-I/O UAF, fixed 51.2/52.0 — **on our default path**, since `RuntimeDefault` seccomp blocks io_uring but allows aio). | **Wider.** Pin past all three; make fixed versions, not just the advisory feed, a release gate. |
| VMM ↔ host | chroot, slot uid, no caps, KVM+TUN nodes. | Same, `/dev/net/tun` swapped for `/dev/urandom`, plus Landlock **if** the node kernel supplies ABI v3. | **Tighter, conditionally.** |
| Guest ↔ network | `sbtapN`, per-TAP pinning, named chains, Sluice, Cilium. | Identical. | **Unchanged.** |
| Guest ↔ KVM | We ask for no VMX/SVM and Firecracker has no nested support, so L0 should never shadow a nested EPT/NPT — but per §1 the bit is not masked, so this is an inference until M0 measures it in a guest. | With `nested=on`: guest root drives L0's shadow MMU. Januscape (16 years latent, both vendors, public host-panic PoC), Zapscape (full public escape chain, demoed on AMD) and CVE-2026-80726 (CVSS 9.3, **PR:N**, scored against a plain guest) all live in that file. | **The material change** — and a property of enabling nested, not of the VMM. |
| Cross-VM CPU side channels | `smt=false`. | `core_scheduling=vm`, on by default, inert below kernel 5.14. | **Tighter.** |

Policy, unchanged in direction:

- **`nested=off` is the driver default**, passed explicitly, on a v52.0+ pin (or
  it is a no-op on AMD).
- **Opt-in per sandbox**, and re-checked on **resume**, because nested is baked
  into `config.json`.
- **Admission gated on the node preflight** in §3 — all three checks.
- **A dedicated node pool is not one `nodeSelector`.** The Deployment is
  `replicas: 1`, `strategy: Recreate`, pinned to one hostname with a Node-local
  `hostPath`, and the device plugin advertises one `sparkbox.dev/kvm` per Node
  precisely so there is one VM-node Pod per machine. A nested pool is a second
  full deployment.
- **Remove the loop bundle** first (already item 1 of the hardening doc's
  remaining work).

What nested changes that the first draft said it did not: **the balloon cannot
reclaim an L2's RAM.** virtio-balloon inflates by allocating pages *in the L1*,
so it takes only what the L1 kernel considers free; an L2's resident memory is
the inner VMM's anonymous memory — neither MemFree nor MemAvailable.
`free_page_reporting` is the same story. Sparkbox's density comes from charging
`MemReserveMB` rather than the ceiling and ballooning idle guests down to that
floor, so a nested sandbox's real floor is its L1 working set *plus* the touched
part of its L2. Admission has to charge nested sandboxes differently, or the
pool oversells.

## 8. Open questions only hardware can answer

1. **Nested state through our pause.** Cloud Hypervisor snapshots carry nested
   state, but upstream has essentially no validation of nested + snapshot/restore
   together. Test with a **real inner KVM guest** mid-flight — not `kind`, whose
   "nodes" are containers and which would pass under Firecracker today. Fallback
   if it fails: refuse pause-with-snapshot for nested sandboxes and cold-boot
   them, exactly as `Reboot`/`Resize`/`Rename` already do.
2. **`BalloonStats` has no released equivalent.** This is the one capability the
   port loses. A driver returning zeros would report every sandbox as using its
   whole ceiling and make the reaper balloon *other* sandboxes to relieve a
   phantom overage. The honest interim is to return an error, which makes
   `MemStats` answer `ok=false` and falls accounting back to the reserve-based
   charge — losing the live working-set signal and the consoles' memory meter,
   but not fabricating one. Decide in M1: error-stub, split the interface, or
   wait for a release carrying `/vm.balloon-stats`.
3. **Balloon against an L1 running KVM.** The direction is settled (above); the
   size of the failure is not. Measure how far the balloon gets before
   `balloon_page_alloc()` fails and whether `deflate_on_oom` returns pages
   before the L1 OOM-kills the inner VMM.
4. **Latencies.** Boot; resume `copy` vs `copyonwrite` (not `ondemand`, which is
   unreachable from our jail); per-VM RSS; L2 CPU performance. There is no
   published Firecracker baseline in this repo, so M1's first deliverable is
   recording one on the same Node in the same run.
5. **Node kernel, `nested`, `ept`/`npt`, and Landlock on `g084f44`** (§3), plus
   CoreWeave's patch cadence per pool.
6. **arm64.** Same driver, no nested. But "nested unavailable" is not "no escape
   exposure": CVE-2026-46316 (ITScape, arm64 vGIC-ITS, CVSS 9.3) is a
   guest-to-host escape whose write-up states plainly "the PoC is not nested".
   The arm64 kernel gate is a different CVE list, not an empty one.
7. **Snapshot compatibility across pinned versions.** We resume sandboxes across
   deploys; upstream does not guarantee snapshot compatibility between releases.
   A VMM upgrade may need the same "cold-boot everything" treatment a kernel
   change gets.

## 9. Spike plan

### M0 — Node preflight (one command, minutes)

`hack/probe-cks-nested.sh` is M0. It is read-only — `kubectl get`/`exec` plus one
optional `ssh` — and it answers all three questions in one pass:

```sh
# from tools/sparkbox, with the CKS kubeconfig live
hack/probe-cks-nested.sh --sandbox <a-live-sandbox>
```

**A. Can the Node host nested guests?** It pipes `hack/probe-nested-virt.sh`
into the `vmm-helper` container over stdin — nothing is written to the node — so
the answer describes the very process that would exec the VMM: CPU flags,
`/dev/kvm`, `nested`, `ept`/`npt`, Landlock, and all three CVE gates.

**B. Does a Firecracker guest already see VMX/SVM?** §1 argues from source that
it does; this settles it by reading `/proc/cpuinfo` in a live sandbox. The first
draft of this plan proposed building a `CONFIG_KVM` guest kernel for this. That
is unnecessary: `X86_FEATURE_VMX` is CPUID.1:ECX[5], printed by the kernel's
generic cpuinfo flag loop through `cpu_has()`, entirely independent of
`CONFIG_KVM` — and the Firecracker CI config sets `CONFIG_X86_FEATURE_NAMES=y`,
so the flags line is populated. One `grep`, no build, no template, no risk.

Seeing the flag is not a vulnerability: without `CONFIG_KVM` the guest cannot
use it. What it would mean is that "our guests see no VMX" is currently a
property of **CoreWeave's module parameters, not of Sparkbox** — and that
pinning a masking CPU template (T2CL/T2/C3 on Intel, T2A on AMD) is worth doing
whatever we decide about this port.

**C. What shape is the fleet?** Node kernel, OS, arch, CPU vendor, device-plugin
allocatables, and whether a second pool exists to isolate nested sandboxes onto.

The script ends by printing the four questions only CoreWeave can answer:
whether `nested` is deliberately set anywhere, the kernel patch cadence per pool
(the binding gate is CVE-2026-80726), whether landlock is in the node kernel's
`CONFIG_LSM`, and whether a second CPU pool is available.

## 10. M0 results

Not yet run — this session has no CKS kubeconfig. Fill in from
`hack/probe-cks-nested.sh` and date it; the answers gate M1's pin and M2
entirely.

| | |
|---|---|
| date / operator | |
| node | |
| kernel | |
| os / runtime / arch | |
| CPU vendor and flags (`vmx`/`svm`, `ept`/`npt`) | |
| `kvm_*.nested` | |
| `kvm_*.ept` / `.npt` | |
| CVE-2026-53359 / -64561 / -80726 | |
| Landlock ABI | |
| node preflight verdict | |
| **guest sees vmx/svm** | |
| node pools | |

CoreWeave answers:

| question | answer |
|---|---|
| Is `kvm_{intel,amd}.nested` deliberately set, and stable? | |
| Kernel patch cadence per CPU pool? | |
| Is landlock in the node kernel's `CONFIG_LSM`? | |
| Can we have a second CPU pool for nested sandboxes? | |

### M1 — Driver beside driver (weeks, not days)

Lift the VMM-neutral half of `fc.go` into shared packages; write
`internal/vmm/cloudhypervisor` with every capability; pin **v52.0+**; build the
**parity harness that does not exist today** (a KVM-host integration test
parametrised by driver, exercising boot, SSH, pause, resume, fork, checkpoint,
restore, rename, resize, reboot); extend the compile-time capability assertions
to all ten. Helper protocol v2 with `nested` and the snapshot-promotion verb.
Kernel fragment and per-arch build assertions. Asset plumbing, producer first.
Decide the `BalloonStats` question. Detail in
[cloud-hypervisor-port-design.md](cloud-hypervisor-port-design.md).

### M2 — CKS canary (days)

Not a canary beside the incumbent: `replicas: 1` + `Recreate` + one
`sparkbox.dev/kvm` means the node Pod is terminated before the new one starts.
Every sandbox on the pinned Node goes down and comes back on the other VMM.
Budget a maintenance window; rollback is a second full Recreate. Then enable
nested for one sandbox on a Node that passed M0 and run the §8 measurements.

### M3 — Decide the default (a meeting)

Either Cloud Hypervisor becomes the CKS default with nested opt-in, or it stays
the opt-in VMM for nested-requesting sandboxes only. Both drivers can coexist,
keyed per sandbox, because the helper builds the argv per launch. Kill criteria
live in [nested-virtualization-design.md](nested-virtualization-design.md).

### Not in the spike

Live migration, `virtio-mem`, VFIO/GPU passthrough, the offloaded snapshot
daemon, TDX/SEV. Each is a reason Cloud Hypervisor might be worth having later;
none is a reason to switch now.

## Sources

Upstream facts below were read at a named tag rather than from `main`, because
`main` carries at least two features (`/vm.balloon-stats`,
`memory_restore_mode=copyonwrite`) that no release has — a trap the first draft
fell into.

- Cloud Hypervisor v50.0 release, `nested=on|off`: <https://www.cloudhypervisor.org/blog/cloud-hypervisor-v50.0-released/>; release notes: <https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/release-notes.md>
- AMD `nested=off` no-op before v52.0: upstream commit `f57b7c5b86fa`, "arch: x86_64: Correctly disable nested virtualization on AMD" (tags v52.0, v53.0 only)
- arm64 `nested=off` parse error removed in v52.0: commit `4e7f9595c8e5`
- `--cpus` (`nested`, `core_scheduling`), snapshot/restore, balloon, landlock, seccomp, device model: `docs/` at tag v53.0
- CPUID clearing when `nested=off`: `arch/src/x86_64/mod.rs`; nested vCPU state: `hypervisor/src/kvm/mod.rs`; clap groups and the `--restore` dispatch: `cloud-hypervisor/src/main.rs`; snapshot file creation: `vmm/src/vm.rs`, `vmm/src/memory_manager.rs`; image type: `vmm/src/device_manager.rs`, `block/src/lib.rs`
- Cloud Hypervisor advisories: GHSA-g6mw-f26h-4jgp (CVE-2023-30612), GHSA-jmr4-g2hv-mjj6 (CVE-2026-27211), GHSA-f47p-p25q-83rh (CVE-2026-45782)
- Firecracker CPUID normaliser and templates: `src/vmm/src/cpu_config/x86_64/cpuid/`; no nested state: `src/vmm/src/vstate/vcpu.rs`; in-tree statement and guard: `src/vmm/src/arch/x86_64/msr.rs`, `tests/integration_tests/security/test_nv.py`; maintainer position: issues #668, #1721
- Guest kernel configs: `resources/guest_configs/microvm-kernel-ci-{x86_64,aarch64}-6.1.config`
- KVM shadow-MMU escapes, from the kernel.org CVE records (`git.kernel.org/pub/scm/linux/security/vulns.git`): CVE-2026-53359 (fixed 6.1.177, 6.6.144, 6.12.95, 6.18.38, 7.1.3, 7.2), CVE-2026-64561 (5.15.218, 6.1.183, 6.6.148, 6.12.101, 6.18.42, 7.1.6, 7.2), CVE-2026-80726 (6.1.183, 6.6.152, 6.12.104, 6.18.45, 7.1.9, 7.2). arm64: CVE-2026-46316
- exe.dev on its VMM: <https://exe.dev/docs/faq/how-exedev-works>, and this repo's own `docs/agentic-sandbox-design.md`
- Freestyle's nested-virtualization claim: <https://www.freestyle.sh/docs>
- Kata with Cloud Hypervisor and its snapshotter requirements: <https://katacontainers.io/blog/kata-containers-with-cloud-hypervisor/>
- CKS runs Kubernetes on bare metal: <https://docs.coreweave.com/docs/products/cks>
