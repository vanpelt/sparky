# Cloud Hypervisor port design

Status: design, unimplemented. Written 2026-09-04 alongside the
[feasibility spike](cloud-hypervisor-feasibility.md), which is the decision
document — read it first for why this is worth doing and what it costs. This one
is the how: the driver mapping the spike's §2 stands on, the privileged-helper
protocol change the spike's §3 calls "the one mechanism that does not carry
over", and the kernel/artifact/rollout work between "the driver compiles" and "a
CKS Pod is running it".

Every upstream fact here was read at a named tag, not from `main`. That matters:
`main` carries `/vm.balloon-stats` and `memory_restore_mode=copyonwrite`, and
**no release does**. v52.0 is the version floor (before it, `--cpus nested=off`
was a silent no-op on AMD).

Per-sandbox nested plumbing, the risk register and the kill criteria live in
[nested-virtualization-design.md](nested-virtualization-design.md).

## 1. Driver mapping, method by method

§2 says the port is "a hand-rolled client for ten REST routes". This section is
the mapping that claim stands on. Every route, flag and default below was read
out of the **v53.0** tag — `vmm/src/api/openapi/cloud-hypervisor.yaml`,
`vmm/src/api/http/mod.rs`, `vmm/src/config.rs`, `cloud-hypervisor/src/main.rs`
and the `docs/` tree — not recalled. Where a fact is true only of unreleased
`main`, it is labelled as such, because §9 M1 pins a released version and
v52.0 is the floor.

Two of the document's assumptions do not survive that check, and both change M1:

- **`/vm.balloon-stats` does not exist in any released Cloud Hypervisor.** It
  appears in `vmm/src/api/http/mod.rs` only on `main`
  (`endpoint!("/vm.balloon-stats")`, `VmBalloonStats`); the v52.0 and v53.0
  route tables have no such entry and the v53.0 OpenAPI file has no
  `BalloonStatsResponse` schema. `docs/balloon.md` at v53.0 documents
  `BalloonConfig` only. §1's table row and §9 M1 both name it as available.
- **`memory_restore_mode=copyonwrite` does not exist in any released version
  either.** v53.0's `MemoryRestoreMode` enum is `[Copy, OnDemand]`
  (`RestoreConfig::SYNTAX`: `memory_restore_mode=copy|ondemand`);
  `CopyOnWrite` is a third variant on `main` only.

The counts are also off in the cheap direction: the driver needs **seven**
routes, not ten, because cold boot and restore are argv, not API calls
(`cloud-hypervisor/src/main.rs` dispatches `VmCreate`+`VmBoot` itself when
`--kernel` is present, and `VmRestore` when `--restore` is). All paths below
are relative to `http://localhost/api/v1` over the jail's Unix socket.

### The table

| `vmm` method | Firecracker driver today (`internal/vmm/firecracker/fc.go`) | Cloud Hypervisor equivalent |
| --- | --- | --- |
| `Driver.Create` | `reflinkClone` → `installAuthorizedKey` → `freeSlot` → `createTap` → `boot` (l.748). `boot` builds `sdk.Config` and the SDK issues `PUT /boot-source`, `/drives/rootfs`, `/network-interfaces/…`, `/machine-config`, `/balloon`, then `PUT /actions{InstanceStart}`. | Disk and slot work unchanged. `boot` becomes: helper `exec`s the cold-boot argv below — the VMM creates and boots itself, no configure-after-start calls. Readiness = socket appears (the helper already waits, `publishSocket`, `server_linux.go:435`) then `GET /vm.info` until `state == "Running"`. |
| `Driver.Pause` | `PauseVM` → `prepareJailedSnapshotOutputs` → `CreateSnapshot(mem.snap.next, state.snap.next)` → `stopVMM` → two `os.Rename`s (l.997). | `PUT /vm.pause` → `PUT /vm.snapshot {"destination_url":"file:///snap.next"}` → `PUT /vmm.shutdown` → helper links the three files out. **No clean equivalent for the promote.** See "Snapshots" below. On snapshot failure, recover with `PUT /vm.resume` on a fresh context exactly as today (l.986). |
| `Driver.Resume` | `boot(..., snapshot=[mem,state])` with `sdk.WithSnapshot` → `PUT /snapshot/load` then `PUT /vm{Resumed}`. | A **second, disjoint argv** (below). Not a variant of the boot line: every VM-config flag is in clap group `vm-config`, which `.requires("vm-payload")`, and if `--kernel` is passed anyway the `payload_present` branch wins and `--restore` is ignored *with no error* — a paused sandbox silently cold-boots and loses its memory. |
| `Driver.Destroy` | `stopVMM`, `deleteTap`, `os.RemoveAll(vmDir)`, `cleanupJail`. | Same shape; `PUT /vmm.shutdown` replaces `StopVMM()`. File operations unchanged. |
| `Driver.Close` | `stopVMM` per VM + `cleanupJail`. | Unchanged. |
| `Archivable.PackRootfs` | `stoppedRootfs` → `compact` (e2fsck+zerofree) → `os.Remove` of `mem.snap`/`state.snap` (l.1191) → `zstd`. | Unchanged **except** the snapshot drop: `os.RemoveAll(<vmdir>/snap)`. `os.Remove` on a non-empty directory returns `ENOTEMPTY`, so the literal pair lifted as-is silently stops working. |
| `Archivable.UnpackRootfs` | `zstd -d --sparse` → `os.Rename`. | Unchanged, file operation. |
| `Archivable.Snapshot` | `reflinkClone` → `sanitizeTemplate` → `compact` → `os.Rename` → `.login-user` sidecar. | Unchanged, file operation. Add one host-side gate: refuse to promote a rootfs whose first four bytes are the QCOW2 magic. v53.0 `device_manager.rs` compares the declared `image_type` against `detect_image_type`'s reading of those bytes and returns `DiskImageTypeMismatch`, and neither `compact()` nor `sanitizeTemplate` rewrites the ext4 boot block — so a guest can poison its own template and every fork of it. |
| `Archivable.RemoveTemplate` | `os.Remove` of the template + sidecar. | Unchanged, file operation. |
| `DiskReporter.DiskUsageMB` / `.DiskCapacityMB` | `ext4DiskMB` — a passive read of the ext4 superblock. | Unchanged, file operation. Deliberately not `/vm.info`: the superblock read is what makes pooled accounting independent of sparse and reflink representation, and `sparse=on` is now the `--disk` default. |
| `TemplateReporter.TemplateUsageMB` | `ext4DiskMB` on the template. | Unchanged, file operation. |
| `RootfsPresencer.RootfsPresent` | `os.Stat`. | Unchanged, file operation. |
| `Renamer.RenameVM` | Refuses while `mem.snap` or `state.snap` exists (`os.Stat` on the literal pair, l.1612), then `os.Rename` of the VM dir. | Same `os.Rename`; the **refusal predicate** becomes "does `<vmdir>/snap` exist". Lifted unchanged the stat matches nothing, and `RenameVM` stops refusing the renames it exists to refuse. Whether the refusal is still *necessary* is a separate question — `config.json` is documented as editable between snapshot and restore, so a Cloud Hypervisor snapshot may be movable where Firecracker's is not. Do not act on that in M1; `internal/vmm/driver.go:158` states the obligation in Firecracker's terms and `host.Manager` obeys it unconditionally. |
| `Rebooter.DropSnapshots` | `os.Remove` of `mem.snap`/`state.snap`, then `delete(d.vms, name)`. | `os.RemoveAll` of `<vmdir>/snap` and `<vmdir>/snap.next`, same record drop. Same `ENOTEMPTY` trap as `PackRootfs`. |
| `CPUStatser.CPUTimeNanos` | Helper path: `OpCPUTime` → helper reads `/proc/<pid>/stat`. Direct path: `machine.PID()`. | Helper path unchanged — it keys on the pid the helper already holds, not on the VMM's identity. Direct path: `GET /vmm.ping` returns `pid` (`vmm_ping` fills it with `process::id()`), which replaces the SDK's `PID()`; or just `cmd.Process.Pid`, since without the SDK the driver owns the `exec.Cmd`. `procStatCPUTicks` is unchanged. |
| `NetStatser.NetBytes` | `/sys/class/net/sbtapN/statistics/{rx,tx}_bytes`, directions swapped. | Unchanged, file operation — recommended. `GET /vm.counters` is an alternative: it returns `{"<net id>":{"rx_bytes":…,"tx_bytes":…,…}}` from the virtio-net device, already in the guest's orientation, so **no swap** — but it resets on VMM exit rather than on tap teardown, which is a different reset cadence than `NetStatser`'s contract documents. Keep the tap read. |
| `DiskResizer.ResizeDisk` | `stoppedRootfs` → `e2fsck -fy` → `os.Truncate` → `resize2fs`. | Unchanged, file operation. **`PUT /vm.resize-disk` is not this.** It grows the live block device (`Block::resize` → `disk_image.resize` → pause vCPUs → update `config.capacity` → config-change interrupt) and never touches the ext4. It is a future capability (online growth without a stop), not a mapping. |
| `Ballooner.SetBalloonTarget` | `machine.UpdateBalloon(targetMiB)` → `PATCH /balloon`. | `PUT /vm.resize {"desired_balloon": <bytes>}`. Same semantics (balloon size = RAM reclaimed), different unit: `VmResize.desired_balloon` is bytes, `vmm.Ballooner` is MiB, so the driver multiplies by 1 MiB. `desired_vcpus`/`desired_ram` omitted leaves both untouched. |
| `Ballooner.BalloonStats` | `machine.GetBalloonStats` → `GET /balloon/statistics` → `actual_mib`, `free_memory`, `available_memory`. | **NO clean equivalent at any released version.** Partial fallback from `GET /vm.info`: `ActualMiB = (config.memory.size − memory_actual_size) / MiB` — `Vmm::vm_info` computes `memory_actual_size` as `total_size − hotplugged_size − balloon_size(+virtio_mem)`, and `DeviceManager::balloon_size()` returns `Balloon::get_actual()`, the guest-acked `VirtioBalloonConfig.actual`, not the target. `TargetMiB = config.balloon.size / MiB`, because `Vm::resize` writes `balloon_config.size = desired_balloon` back into the stored config. `FreeMiB` and `AvailableMiB` — the guest's `VIRTIO_BALLOON_S_MEMFREE`/`S_AVAIL` — are **not obtainable**. See below. |

### The one method with no answer: `BalloonStats`

This is the only place the port loses a capability rather than renaming one, and
it is load-bearing for density, so it needs a decision in M1 rather than a
discovery in M2.

`Manager.MemStats` (`internal/host/manager.go:2653`) computes
`used = memMB − ActualMiB − unused`, with `unused` falling back from
`AvailableMiB` to `FreeMiB`. A driver that returns a well-formed sample with
both zeroed reports every running sandbox as using its entire ceiling; that
number reaches `m.memUsed`, `observedMemMB`, `reconcileMemoryPressure` and
`reclaimMemory`, which then balloons *other* sandboxes to relieve an overage
that does not exist.

The honest interim is therefore to **return an error** from `BalloonStats`
whenever free/available are unavailable. `MemStats` then answers `ok=false`,
`refreshMemoryUsage` never writes `m.memUsed[name]`, and `observedMemMB` falls
back to `sandboxEffectiveMemMB` — the reserve-based charge. The fleet loses the
live working-set signal and the consoles' memory meter; it does not gain a
fabricated one. `SetBalloonTarget` still works, so the reaper can still inflate;
what it cannot do is verify.

Three ways out, in increasing order of cost:

1. Pin a version that has `/vm.balloon-stats` once one ships. Its response
   (`BalloonStatsResponse` on `main`) carries `balloon_actual` plus the guest's
   `free_memory`/`available_memory`/`total_memory`, all in bytes — every field
   `vmm.BalloonStats` needs. Note two behavioural differences from Firecracker
   even then: there is no `stats_polling_interval_s` equivalent (Cloud
   Hypervisor returns the cached sample and asks the guest to refresh
   asynchronously, so a reading is as fresh as the *previous* request), and
   before the first sample — including after every restore — the endpoint
   answers 200 with an empty `stats` object, which the driver must map to an
   error for exactly the reason above.
2. Split `vmm.Ballooner` into `Ballooner` (`SetBalloonTarget`) and a separate
   `BalloonStatser` (`BalloonStats`), so a driver can implement the lever
   without the gauge and `host.Manager` detects each by type assertion, as it
   already does for the other ten capabilities. This is small and it is the
   shape the rest of `driver.go` already uses — but it contradicts §2's
   "`internal/vmm/driver.go`: Nothing."
3. Read the guest's working set out of band (a guest agent, or `sparkbox-netcfg`'s
   sibling). Out of scope for this port; noted so the option is on the record.

M1 should do (1)-when-available with (2) as the mechanism, and M2 must not
publish a density number measured with `BalloonStats` returning zeros.

### The literal command lines

Sandbox `foo`, slot 7, 2 vCPU, 8 GiB, nested off, IPv6 configured, default
`172.30.0.0/16` guest subnet. The helper `exec`s with
`SysProcAttr{Chroot: <ChrootBase>/cloud-hypervisor/sparkbox-7/root,
Credential: {Uid: 100007, Gid: 100007}}`, `Dir: "/"`, `Env: []string{}`, and
`ExtraFiles[0]` = the TAP fd, which arrives in the child as **fd 3** —
`NetConfig::validate` rejects any fd ≤ 2, and requires `fds.len()*2 ==
num_queues`, so one fd against the default `num_queues=2`.

Jail contents, in the shape `prepareJail` (`server_linux.go:466`) already
builds:

```text
/                    0710 root:<ControllerGID>   chroot root
  cloud-hypervisor   0555 root                   copyExecutable
  dev/kvm            c 10:232, 0600 uid:uid      cloneDevice
  dev/urandom        c 1:9,    0600 uid:uid      NEW — virtio-rng is always
                                                 instantiated and Rng::new
                                                 File::opens its source; there
                                                 is no flag that removes it
  vmlinux            0444 root                   linkTrustedResource
  rootfs.ext4        0660 uid:<ControllerGID>    linkStateResource
  ch.sock                                        created by the VMM, then
                                                 chowned uid:<ControllerGID>
                                                 0660 by publishSocket
  console.log                                    created by the VMM
  snap.next/         0700 uid:uid, EMPTY         NEW — must exist before exec
  snap/              0700 uid:uid                resume only: three hard links
  dev/net/tun                                    GONE (--net fd=)
```

**Cold boot:**

```sh
/cloud-hypervisor \
  --api-socket path=/ch.sock \
  --seccomp true \
  --landlock \
  --landlock-rules path=/snap.next,access=rw \
  --cpus boot=2,nested=off \
  --memory size=8192M \
  --kernel /vmlinux \
  --cmdline "console=ttyS0 reboot=k panic=1 quiet root=/dev/vda rw net.ifnames=0 ip=172.30.0.30::172.30.0.29:255.255.255.252::eth0:off sparkbox_host=foo systemd.machine_id=<32 hex> sparkbox_ip6=<guest v6>/127 sparkbox_gw6=<host v6> sparkbox_dns=172.30.0.29 sparkbox_fresh=1" \
  --disk path=/rootfs.ext4,id=disk0,image_type=raw,sparse=on,lock_granularity=byte-range \
  --net id=net0,fd=3,num_queues=2,mac=02:5b:00:00:00:07 \
  --balloon size=0,deflate_on_oom=on,free_page_reporting=on,id=balloon0 \
  --rng src=/dev/urandom \
  --serial file=/console.log \
  --console off
```

Why each non-obvious token is there:

- `--api-socket path=/ch.sock` — a bare path also parses (`parse_api_socket`
  falls through to the raw string), so the helper's existing fixed name works
  unchanged. `fd=` is the alternative; it removes nothing from the jail, since
  the VMM creates the socket in the jail either way. The name is declared
  independently in `fc.go:147` and `server_linux.go:31` and the two must agree
  — as must the two copies of the chroot path, which both derive from
  `filepath.Base(<VMM binary>)` (`fc.go:397`, `server_linux.go:807`).
- `--cpus boot=2` — `max` defaults to `boot`. There is **no `smt=` option**;
  Firecracker's `Smt: false` parity comes from omitting `topology=`, which
  leaves the guest with no SMT siblings. `nested=off` must be explicit:
  `CpusConfig::parse` reads it with `.is_none_or(|t| t.0)`, i.e. absent means
  **on**. `core_scheduling` is omitted because its default is already `Vm`
  (`unwrap_or(CoreScheduling::Vm)`) — write it explicitly if you want an
  upstream default change to show up in a diff.
- `--disk … image_type=raw` — not decoration. Omitted on v53.0 the format is
  auto-detected and, having detected raw, `disable_sector0_writes` is set and
  every guest write to sector 0 of `/dev/vda` returns an I/O error; omitted on
  current `main` the VM does not start (`ValidationError::ImageTypeRequired`).
  `sparse=on` and `lock_granularity=byte-range` are both already the defaults
  (`default_diskconfig_sparse() -> true`; `LockGranularity` default
  `ByteRange`) and are written out so a default change is visible in review.
  `id=disk0` is needed only if `/vm.resize-disk` is ever used.
- `--net id=net0,fd=3` — the `id` is **not optional**: `RestoreConfig::validate`
  matches `net_fds` entries to devices by the id recorded in `config.json`.
  Use a slot-independent literal (`net0`), not a slot-derived one, so a resume
  cannot be broken by a slot change. `mac=` matches `macFor(idx)` and is
  recorded in the snapshot, so the restored NIC comes back identical.
- `--rng src=/dev/urandom` — writing the default explicitly, because the device
  cannot be removed and the source is what dictates the new jail node.
- `--serial file=/console.log --console off` — `--console` defaults to `tty`
  and `--serial` to `null`; only `off` suppresses a device
  (`add_virtio_console_device`: `ConsoleTransport::Off => return Ok(None)`).
  Omitting either flag *adds* a device silently rather than failing, which
  argues for asserting the whole argv in a driver unit test.
- `--landlock-rules path=/snap.next,access=rw` — `VmConfig::apply_landlock`
  derives its ruleset from the VM config (disk, rng src, serial file, payload,
  plus an unconditional `/dev/net/tun` rule whenever `--net` is present, even
  with `fd=`) and it runs at `vm.create`, before any snapshot destination is
  known. Without this rule the later `/vm.snapshot` cannot create files in
  `/snap.next`. `access=rw` maps to `AccessFs::from_write(ABI::V3)`, which
  includes `MakeReg`. rust-landlock's `path_beneath_rules` silently drops a
  rule whose path cannot be opened, so **`/snap.next` must exist before exec** —
  which is why it is created in `prepareJail`, not at pause time.
- `--landlock` itself needs a node preflight: `vmm/src/landlock.rs` pins
  `ABI::V3` with `CompatLevel::HardRequirement`, so on a host kernel below 6.2,
  or one without landlock in `CONFIG_LSM`, `Landlock::new()` fails and
  `vm.create` returns `VmError::ApplyLandlock` — it does not degrade.

**Restore.** A different argument list, not a modified one:

```sh
/cloud-hypervisor \
  --api-socket path=/ch.sock \
  --seccomp true \
  --restore source_url=file:///snap,net_fds=[net0@[3]],resume=on
```

and nothing else. `--kernel`, `--disk`, `--net`, `--cpus`, `--memory`,
`--balloon`, `--serial`, `--console`, `--rng`, `--landlock` and
`--landlock-rules` are all in the `vm-config` group and cannot appear here. The
whole `VmConfig` — including `nested`, the balloon size `Vm::resize` wrote back,
and `landlock_enable`/`landlock_rules`, all of which are serde-serialized —
comes from the snapshot's `config.json`. Two consequences the driver has to
carry:

- **Nested is baked into the snapshot.** A sandbox created with `nested=on`
  resumes with nested on regardless of what the helper passes at restore, so
  §7's node gate has to be re-evaluated *on resume*, with a nested sandbox that
  fails it cold-booted via `DropSnapshots` (the §8.1 fallback) or its
  `config.json` rewritten first.
- **Landlock on the restore path is whatever the snapshot recorded**, which is
  a reason to get the cold-boot ruleset right the first time.

`resume=on` parses (`Toggle` accepts `on`/`off`/`true`/`false`).
`memory_restore_mode` is omitted, which selects `copy` — the eager read.
Firecracker's restore `mmap`s `mem.snap` `MAP_PRIVATE` and pays no eager read,
so this is a resume-latency regression that M2 must measure. `ondemand` is not a
reachable alternative from this jail: it needs `/dev/userfaultfd` (which
`prepareJail` does not mknod) or the `userfaultfd(2)` syscall (which the
capability-free slot uid cannot call, and which containerd's `RuntimeDefault`
profile does not list), and the mode is strict — it fails the restore rather
than falling back. `copyonwrite`, the mode whose semantics actually match what
we have today, is unreleased.

### Snapshots: the one mechanism that does not carry over

The helper's current trick — pre-create `mem.snap.next`/`state.snap.next` in the
VM state dir with `openat` on a `RESOLVE_BENEATH` fd, `Fchown` to
`uid:ControllerGID`, then publish into the jail with
`linkat(fd, "", AT_EMPTY_PATH)` (`server_linux.go:605`) — cannot be reused, for
three independent reasons:

1. `url_to_path` (`vmm/src/migration/mod.rs`) requires the destination to
   already be a **directory**, and `link(2)` returns `EPERM` for directories.
   The helper holds `CHOWN, DAC_OVERRIDE, DAC_READ_SEARCH, FOWNER, KILL, MKNOD,
   NET_ADMIN, SETGID, SETUID, SYS_CHROOT` and no `SYS_ADMIN`, so a bind mount
   is not available either.
2. Pre-creating the three files by name fails too: `Vm::send` opens
   `config.json` and `state.json` and `MemoryManager::send` opens
   `memory-ranges` with `create_new(true)` (`O_CREAT|O_EXCL`), so an existing
   inode is `EEXIST`.
3. Anything that exists only inside the jail is destroyed by
   `cleanupSlot`'s `os.RemoveAll(jailWorkspace)` (`server_linux.go:703`), which
   the launch handler calls the moment the VMM is reaped.

So the snapshot lifecycle becomes three helper responsibilities:

- `prepareJail` creates `<jail>/snap.next/` empty and uid-owned, **before
  exec**, because Landlock's ruleset is fixed at `vm.create`.
- `OpSnapshot` keeps its name and changes its meaning: instead of pre-creating
  two files it *empties* `<jail>/snap.next/`. That preserves today's
  self-healing property — a pause whose `/vm.snapshot` failed leaves a partial
  directory, and because `Vm::send` writes `config.json` first, a partial
  directory is indistinguishable from a complete one by existence alone.
- The launch handler drains the directory on teardown: after `waitCh` and
  **before** `cleanupSlot`, hard-link `config.json`, `state.json` and
  `memory-ranges` out into `fc-vms/<name>/snap.next/`. This cannot be a
  controller-driven op racing `cleanupSlot`; today the `.next` inodes survive
  only because they already have a second link in the VM state dir.

Promotion is then three links or a remove-then-rename rather than one atomic
`rename` — `rename(2)` will not replace a non-empty directory (`ENOTEMPTY`) —
so the controller needs a defined recovery for a crash mid-promotion, and
something must verify all three files are present before promoting. Ownership
also changes hands: today the helper creates the `.next` files and
`Fchown(uid, ControllerGID)`s them so the controller group can read and
`os.Remove` them; under Cloud Hypervisor the VMM creates them as the slot uid
under its own umask, and the controller (uid 65532, jail root 0710) has no path
to them at all until the link-out. `DropSnapshots`, `PackRootfs` and the
checkpoint flow all depend on the controller being able to remove them, so the
link-out has to reproduce the chown, not just the link.

The "never snapshot over the files you were restored from" rule still holds and
is now free: a resume execs a fresh VMM with a fresh jail, so `/snap.next` is
empty at every launch and `/snap` (the restore source) is a different directory.

One further behavioural difference in the same area, for the record: `reboot=k`
still reaches the i8042, but Cloud Hypervisor's reset handler calls
`vm_reboot()` **in process** (`vmm/src/lib.rs`, `EpollDispatch::Reset`) where
Firecracker exits. A guest typing `reboot` therefore no longer surfaces to the
controller as a VMM exit; it re-scans the disk and re-takes the disk locks
inside the running VMM. `Manager.Reboot` (pause → `DropSnapshots` →
`EnsureRunning`) is unaffected. Guest *shutdown* still exits the VMM
(`EpollDispatch::GuestExit` → `vmm_shutdown` → `break`), so that path is
unchanged.

### Guest kernel command line

Diff against `kernelArgs` (`fc.go:711`). Tokens in order as emitted today:

```diff
  console=ttyS0            # x86 only; on arm64 this must become console=ttyAMA0
  reboot=k
  panic=1
- pci=off
  quiet
+ root=/dev/vda rw
+ net.ifnames=0
  ip=<guest>::<host>:255.255.255.252::eth0:off
  sparkbox_host=<name>
  systemd.machine_id=<32 hex>
  [sparkbox_ip6=<guest v6>/127 sparkbox_gw6=<host v6>]
  [sparkbox_dns=<gateway ip | literal>]
  [sparkbox_fresh=1]
```

- **`pci=off` goes.** Cloud Hypervisor is virtio-pci and the guest needs the
  bus. The token is redundant on the Firecracker side too — v1.16.1's
  `src/vmm/src/builder.rs` inserts `pci=off` itself when PCI is disabled — so
  removing it from the shared string costs nothing there.
- **`root=/dev/vda rw` arrives.** Firecracker derives it from the drive we flag
  `IsRootDevice` (`fc.go:781` → `append_root_device_cmdline` in
  `src/vmm/src/vmm_config/boot_source.rs`). Cloud Hypervisor derives nothing:
  the cmdline it gets is the cmdline we pass.
- **`net.ifnames=0` arrives**, as insurance rather than for the obvious reason.
  IPv4 survives a udev rename regardless — `ip=` is consumed by the kernel's own
  autoconfiguration (`CONFIG_IP_PNP=y` in both CI configs) before userspace runs
  — and `images/Dockerfile` already masks `systemd-udevd` and its sockets, so
  nothing in the shipped image applies predictable names today. The real
  exposure is `hack/build-rootfs.sh`'s `sparkbox-netcfg`, which hard-codes
  `IFACE=eth0` for the IPv6 leg. Sluice keys on the host-side `sbtapN` and does
  not name `eth0` at all.
- **`console=ttyS0` is x86-only.** Cloud Hypervisor's arm64 UART is a PL011
  (`arch/src/aarch64/fdt.rs` emits `compatible = "arm,pl011\0arm,primecell\0"`)
  where Firecracker's is an `ns16550a`; `fc.go:716` hard-codes `ttyS0` for both
  arches and `hack/stage-artifacts.sh` publishes `vmlinux-arm64`. Either use
  `console=ttyAMA0` on arm64 and add `CONFIG_SERIAL_AMBA_PL011{,_CONSOLE}=y`
  (the CI config has `# CONFIG_SERIAL_AMBA_PL011 is not set`), or switch arm64
  to `--console` with `console=hvc0`, which needs no config change —
  `CONFIG_VIRTIO_CONSOLE=y` and `CONFIG_HVC_DRIVER=y` are already set on both
  arches. Untreated, an arm64 guest boots and produces no console output at
  all, which is a silent failure: SSH still works, so nothing notices until the
  first boot that fails.
- Every `sparkbox_*` token, the `ip=` line and `systemd.machine_id=` are
  unchanged, and so is the property that matters about them: the host asserts
  them and the guest cannot forge them.

Because the deltas are both VMM- **and** arch-specific, `kernelArgs` cannot be
lifted verbatim into a shared package. It becomes a function of
(VMM, target arch); the tokens are shared, the frame is not. Both drivers ship
at once until the M3 gate, so both cmdlines have to stay correct.

### `fc.go`: what moves, what stays

Measured by summing per-function line spans over all 2,153 lines: roughly **930
lines are VMM-neutral**, **~120 look neutral and are not**, and **~1,100 are
VMM-coupled and get rewritten**. §2's "the other ~1,600 lines" / "a few hundred
lines" split is optimistic by about a factor of two on the driver half.

**Move to `internal/vmm/rootfs`** — disk, template, key and compaction work,
none of which mentions a VMM:

`reflinkClone`, `compact`, `ext4DiskMB`, `exitCode`, `rootfsPath`,
`captureDir`, `templatePath`, `imageNameRe`, `machineIDFor`,
`rootfsLoginIdentity`, `installAuthorizedKey`, `writeAuthorizedKey`,
`ensureGuestDir`, `mergeAuthorizedKeys`, `sameAuthorizedKey`,
`sanitizeTemplate` — plus the method bodies of `DiskUsageMB`,
`DiskCapacityMB`, `TemplateUsageMB`, `RootfsPresent`, `ResizeDisk`,
`UnpackRootfs`, `RemoveTemplate` and `Snapshot`.

`stoppedRootfs` moves with them but needs a seam: it consults `d.vms` for
liveness, so the shared package takes a `running func(name string) bool` (or a
`Stopped(name) error`) supplied by the driver.

**Move to `internal/vmm/slots`** — addressing, tap and naming arithmetic:

`guestSlot`, `hostIP`, `guestIP`, `hostIP6`, `guestIP6`, `addr6`, `tapName`,
`macFor`, `jailID`, `freeSlot`, `reserveName`, `releaseName`, `createTap`,
`deleteTap`, `defaultRoute6Dev`, `sweepStaleTaps`, `readTapCounter`,
`procStatCPUTicks`, `validateGuestDNS`, `guestDNSArg` — plus `NetBytes`.

**Shared in shape, not in content** — parameterise, do not lift:

- `kernelArgs` — see §3 above; a function of (VMM, arch).
- `DropSnapshots` (`fc.go:1588`), `RenameVM` (`fc.go:1612`) and `PackRootfs`
  (`fc.go:1191`) each iterate the literal `[]string{"mem.snap", "state.snap"}`.
  The shared code should call a driver-supplied
  `SnapshotArtifacts(name) []string` / `DropSnapshot(name) error` instead. All
  three are load-bearing: `internal/host/checkpoint.go:164` calls
  `DropSnapshots` immediately after `PackRootfs`, and `manager.go` calls it on
  the turbo, resize and rename paths.

**Stay driver-specific and get rewritten:**

`New`, `Options`, `vmState`, `Driver`, the jailed-name constants, `vmDir`,
`jailed`, `jailUID`, `jailWorkspace`, `jailRoot`, `cleanupJail`,
`prepareChrootJail`, `copyJailExecutable`, `cloneJailDevice`,
`chrootJailerCommand`, `chrootProcess`, `boot`, `jailedResourcesHandler`,
`linkJailedResource`, `prepareJailedSnapshotOutputs`, `Create`, `Pause`,
`Resume`, `Destroy`, `Close`, `stopVMM`, `stopMachine`, `instance`,
`SetBalloonTarget`, `BalloonStats`, `CPUTimeNanos` (the direct branch only).

**Deleted outright:**

`validateJailerPair` (Firecracker's jailer/VMM version pairing; there is no
jailer for Cloud Hypervisor, by design), and `strPtr`/`boolPtr`/`int64Ptr`,
which exist only to satisfy `firecracker-go-sdk`'s pointer-typed `models`.

**What the SDK was doing for us, which now has to be written.** `fc.go` touches
`firecracker-go-sdk` on ~31 lines, but the work behind them is larger than that
count: `sdk.NewMachine` (l.836), `sdk.WithProcessRunner` (l.832 — the injection
point the entire vmhelper protocol is built around, see
`internal/vmhelper/protocol.go:113`), `sdk.WithSnapshot` (l.834), the `FcInit`
handler chain for jail staging and the balloon (l.844–861), API-socket
readiness, the ordered configure-then-boot sequence inside `m.Start`, and
`PID()`/`Wait()` (l.1648, 2036, 2041). Cloud Hypervisor has no official Go
client. The configure-then-boot half disappears (argv does it) and readiness
becomes a `GET /vm.info` poll, but the process-supervision contract — spawn the
launch client, drive the half-close stop handshake, wait for helper cleanup —
has to be reimplemented, and `*sdk.Machine` is currently the driver's
is-running predicate in ten places and the liveness sentinel in two tests
(`fc_linux_test.go:745,791`), so a process-handle type has to exist before any
of the lift can start.

**Testing.** There is no real-VMM parity harness to inherit. Every lifecycle
e2e test runs on the mock driver (`e2e_test.go:81`,
`fleet_e2e_test.go:244,470`), and the Firecracker package's ~1,360 lines of
tests are host-side unit tests that never boot a guest (`fc_linux_test.go:32`
constructs the `Driver` directly "to avoid `New()`'s /dev/kvm requirement").
M1 must *build* the parametrised KVM-host harness, not run it. While there,
extend the compile-time capability assertions from the four currently checked
(`Renamer`, `Rebooter`, `CPUStatser`, `TemplateReporter`) to all ten, or a
Cloud Hypervisor driver that quietly omits `Archivable`, `DiskReporter`,
`DiskResizer`, `NetStatser`, `RootfsPresencer` or `Ballooner` will degrade the
fleet rather than fail the build.

### Open questions from this section

- Is there a released Cloud Hypervisor version with /vm.balloon-stats, or a target date for one? At v52.0 and v53.0 the route does not exist, and without the guest's free/available figures Sparkbox's memory-pressure controller loses its input entirely. If the answer is 'not soon', M1 has to choose between splitting vmm.Ballooner and accepting reserve-based charging for every Cloud Hypervisor sandbox.
- Same question for memory_restore_mode=copyonwrite, which is main-only at v53.0. copy is a full eager read of guest RAM where Firecracker mmaps the file MAP_PRIVATE; without copyonwrite, what resume-latency regression is acceptable on the CKS hot tier, and does that change the version floor from v52.0 to 'whatever ships copyonwrite'?
- Does CoreWeave's CKS node kernel provide Landlock ABI v3 (>= 6.2, CONFIG_SECURITY_LANDLOCK=y, landlock in CONFIG_LSM or the lsm= boot parameter)? Cloud Hypervisor pins that ABI as a HardRequirement, so --landlock fails vm.create rather than degrading. hack/probe-nested-virt.sh does not check it, and §7 books Landlock as a mitigation.
- Can the vmm-helper container mknod /dev/urandom into each jail? runc's default device cgroup lists c 1:9 rwm and the helper holds CAP_MKNOD, so this should need no device-plugin or Pod change — but it has not been tried on a CKS node, and virtio-rng cannot be removed from the VMM's device model.
- What should the driver do about a guest-initiated reboot now that Cloud Hypervisor reboots in process rather than exiting? Today a `reboot` inside a sandbox terminates the VMM and the manager cold-boots it; under Cloud Hypervisor the sandbox keeps running and re-scans its own disk. Is the new behaviour desirable (fewer cold boots) or does the driver need to force parity?
- Is snapshot/restore compatibility guaranteed across the pinned Cloud Hypervisor versions we would deploy? release-notes.md's stability list says 'Snapshot/restore is not supported across different versions', and docs/live_migration.md's compatibility promise is scoped to migration and starts at v54. Every sandbox paused before a VMM bump may be unresumable, and the fallback (DropSnapshots, cold boot) is a fleet-wide loss of running state at each upgrade.
- Should the promoted snapshot directory be `fc-vms/<name>/snap/` or should the VM state directory be renamed at the same time? `fc-vms` is hard-coded in five independent places (fc.go:384, server_linux.go:803, hostsetup/layout.go:36, entrypoint.sh:312, macos/poc.sh:689) and a second driver either shares a directory whose name lies about its contents or needs a migration.
- On the restore command line, does clap reject an explicitly passed --landlock (group vm-config requires vm-payload)? Upstream's own restore examples pass no vm-config flags and the defaulted ones evidently do not trip the group, but I did not run cloud-hypervisor to confirm the explicit case. The design does not depend on the answer — Landlock on restore comes from the snapshot's config.json — but M1 should confirm it rather than discover it.

## 2. Privileged helper protocol v2

The helper is the only place on CKS where the VMM command line is actually
constructed, so every Cloud Hypervisor decision in §1, §6 and §7 lands here.
This section is the design, with the invariants from `security-hardening.md`
carried through unchanged: `SO_PEERCRED` on the controller UID, exact protocol
version, `vmNameRE`-validated name, subnet-bounded slot, every path/UID/device
number/executable derived from immutable startup configuration, and a stop
handshake that acknowledges only after the VMM is reaped and the TAP and jail
are gone.

One upstream fact drives most of the shape. `cloud-hypervisor/src/main.rs` (v53.0,
lines 757–790) dispatches on `payload_present = contains_id("kernel") ||
contains_id("firmware")`: if a payload is present it sends `VmCreate` + `VmBoot`
built **entirely from the command line** and never looks at `--restore`;
otherwise it sends `VmRestore`. There is no configure-after-start window. Where
Firecracker lets the helper exec `/firecracker --api-sock fc.sock` and knows
nothing about vCPUs, memory or the guest cmdline (`server_linux.go:339`), Cloud
Hypervisor forces the helper to own the whole VM configuration or none of it.
Owning none of it means the controller POSTs a `VmConfig` and chooses its own
`nested`, `landlock_enable` and `landlock_rules` — which destroys §7's gate. So
the helper owns it, and the protocol has to carry every per-VM number.

### 2.1 The protocol delta

```diff
 const (
-	ProtocolVersion = 1
+	ProtocolVersion = 2
 	OpPing          = "ping"
 	OpLaunch        = "launch"
 	OpSnapshot      = "snapshot-outputs"
 	OpCPUTime       = "cpu-time"
 	maxMessageBytes = 4096
 )

 // Request is deliberately path-free. The helper derives every path from its
 // immutable startup configuration after validating Name and Slot.
 type Request struct {
 	Version int    `json:"version"`
 	Op      string `json:"op"`
 	Name    string `json:"name,omitempty"`
 	Slot    int    `json:"slot,omitempty"`
 	Resume  bool   `json:"resume,omitempty"`
+	// Cloud Hypervisor is configured once, at exec: `cloud-hypervisor/src/main.rs`
+	// boots straight from the command line when --kernel is present. These are
+	// therefore inputs to the helper, not later API calls. They are bounded
+	// scalars, never paths, device numbers or credentials.
+	VCPUs  int  `json:"vcpus,omitempty"`
+	MemMB  int  `json:"mem_mb,omitempty"`
+	Fresh  bool `json:"fresh,omitempty"` // adds sparkbox_fresh=1 to the cmdline
+	Nested bool `json:"nested,omitempty"`
 }

 type Response struct {
 	OK           bool   `json:"ok"`
 	Error        string `json:"error,omitempty"`
 	CPUTimeNanos uint64 `json:"cpu_time_nanos,omitempty"`
+	// Ping: whether this Node passed the helper's startup nested preflight, so
+	// `doctor` and host.Manager admission can read it instead of guessing.
+	NestedAvailable bool `json:"nested_available,omitempty"`
+	// Launch (final response only): the helper promoted a complete snapshot
+	// directory out of the jail before removing it. Pause treats false as
+	// "the VMM exited without producing a snapshot".
+	SnapshotPromoted bool `json:"snapshot_promoted,omitempty"`
 }
```

`ServerOptions` gains `VMMKind` (`firecracker|cloud-hypervisor`), `VMMBin`
(replacing `FirecrackerBin`), `GuestDNS`, and `MaxVCPUs`/`MaxMemMB` bounds;
`cmd/sparkbox-vmm-helper/main.go` gains the matching flags and
`vmm-helper-entrypoint.sh:52` passes `--vmm cloud-hypervisor --vmm-bin
"$asset_dir/cloud-hypervisor"`.

**Why the version must bump.** JSON with `omitempty` is wire-compatible in both
directions, and that is exactly the problem. A v1 controller talking to a v2
helper sends no `nested`, `vcpus` or `mem_mb`; the helper would boot a
1-vCPU/512 MiB VM — Cloud Hypervisor's `--cpus`/`--memory` defaults — and call it
success. A v2 controller talking to a v1 helper sends `nested: true` and the
helper silently drops it, producing a sandbox that the control plane believes is
nested-capable and that is not. `validateRequest` compares the version for exact
equality (`server_linux.go:257-259`), so the bump converts both silent
downgrades into a refusal. The semantics of `OpSnapshot` also change (§10.4),
which is a second, independent reason.

**Validation, added to `validateRequest` after the existing name/slot checks.**

| Field | Rule | Failure |
| --- | --- | --- |
| `VCPUs` | `1 <= VCPUs <= opts.MaxVCPUs` | refuse |
| `MemMB` | `opts.MinMemMB <= MemMB <= opts.MaxMemMB` | refuse |
| `Nested` | requires `s.nestedOK` **and** `runtime.GOARCH == "amd64"` | refuse, typed |
| `Fresh`, `Resume` | no constraint; both only select cmdline tokens / link sets | — |

`s.nestedOK` is computed once in `newServer` from the same checks
`hack/probe-nested-virt.sh` performs: CPU vendor and `vmx`/`svm`,
`/sys/module/kvm_{intel,amd}/parameters/nested` accepting `1|Y|y` (Intel's
parameter is a `bool` and renders `Y`, AMD's is an `int` and renders `1`), and
`uname -r` against the three shadow-MMU CVE floors. It is read at startup, not
per request, because both module parameters are mode 0444 and AMD's is
`__ro_after_init`: the answer cannot change without a reboot. Refusing here
rather than in the controller is what makes §7's "admission is gated on the
Node" true even for a compromised controller.

**The honest limit of that gate.** It only binds a *cold boot*. On a restore the
whole `VmConfig` — `cpus.nested` included — comes from the snapshot's
`config.json` (`vmm/src/migration/mod.rs::recv_vm_config`), and `RestoreConfig`
accepts only `source_url`, `prefault`, `memory_restore_mode`, `net_fds` and
`resume`. So a controller that cold-boots a sandbox with nested on, pauses it,
and then resumes it while claiming `nested: false` gets a nested VM. The helper
could only prevent that by parsing and rewriting `config.json`, which is exactly
the host-userspace parsing of guest-adjacent bytes that `security-hardening.md`
lists as debt to remove. The design therefore does not parse it: the enforcement
that actually holds is the Node's `kvm_{intel,amd}.nested` parameter, and the
controller is expected to cold-boot via `DropSnapshots` when a paused sandbox's
recorded nested value and the requested one disagree. Say so in the code
comment; do not let the field read as a security boundary it is not.

### 2.2 Building the argv from (name, slot, nested, vcpus, mem, fresh)

Every token is either a constant, a value derived from the validated request, or
a value from immutable startup configuration. Nothing in the request is a path,
a device number, a UID, or a filename.

```go
uid := uint32(s.jailUID(req.Slot))                 // JailerUIDBase + slot
args := []string{
    "--api-socket", "fd=4",                        // helper-created listener, §10.3
    "--seccomp", "true",                           // default, stated so a change shows in a diff
    "--landlock",                                  // §10.5
    "--landlock-rules", "path=/snap.next,access=rw",
    "--kernel", "/" + jailedKernelName,            // "vmlinux", hard-linked by prepareJail
    "--cmdline", s.kernelArgs(req),                // derived; see below
    "--disk", "path=/" + jailedRootfsName + ",image_type=raw,sparse=on," +
        "lock_granularity=byte-range",
    "--net", fmt.Sprintf("id=%s,fd=[3],mac=%s,num_queues=2", netID, macFor(req.Slot)),
    "--cpus", fmt.Sprintf("boot=%d,nested=%s", req.VCPUs, onOff(req.Nested)),
    "--memory", fmt.Sprintf("size=%dM", req.MemMB),
    "--balloon", "size=0,deflate_on_oom=on,free_page_reporting=on",
    "--rng", "src=/dev/urandom",
    "--serial", "tty",                             // guest console to the Pod log, as today
    "--console", "off",
}
cmd := exec.Command("/cloud-hypervisor", args...)
cmd.Dir = "/"
cmd.Env = []string{}
cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
cmd.ExtraFiles = []*os.File{tapFile, apiListenerFile} // → child fds 3 and 4
cmd.SysProcAttr = &syscall.SysProcAttr{
    Chroot:     s.jailRoot(req.Slot),
    Credential: &syscall.Credential{Uid: uid, Gid: uid, Groups: []uint32{uid}},
}
```

On a resume the argument list is **disjoint**, not a superset — `--kernel`
alongside `--restore` takes the cold-boot branch and silently discards the
snapshot:

```go
args := []string{
    "--api-socket", "fd=4",
    "--seccomp", "true",
    "--restore", fmt.Sprintf("source_url=file:///snap,net_fds=[%s@[3]],resume=on", netID),
}
```

Two consequences of clap's group table
(`ArgGroup::new("vm-config").multiple(true).requires("vm-payload")`, main.rs:511):
`--landlock`, `--landlock-rules`, `--console`, `--serial`, `--rng`, `--cpus`,
`--memory`, `--disk` and `--net` are all in `vm-config` and cannot appear on the
restore line at all, while `--api-socket`, `--restore` and `--seccomp` can.
Everything the restored VM runs under therefore comes from `config.json` — which
is why the Landlock rules in §10.5 are written as chroot-relative paths that do
not change between boots or between slots.

Where each argument's value comes from:

| Argument | Source | Controller-supplied? |
| --- | --- | --- |
| `--api-socket fd=4`, `--seccomp`, `--landlock*`, `--console off`, `--rng`, `--serial` | constants in the helper binary | no |
| `--kernel`, `--disk path=` | fixed jailed names; the inodes come from `linkTrustedResource`/`linkStateResource` under `openat2` `RESOLVE_BENEATH` | no |
| `--net fd=[3]`, `mac=` | `macFor(slot)`, slot validated against `s.network.Capacity()` | slot only |
| `--cmdline` | `guestnet.Slot(slot)` addresses, `Subnet6`/`GuestDNS` from `ServerOptions`, `sparkbox_host=<name>`, `machineIDFor(name)`, `sparkbox_fresh` | name, slot, fresh |
| `--cpus`, `--memory` | request scalars, range-checked | yes, bounded |
| `--balloon` | constants; later resizes go over the API, not the helper | no |
| `--restore source_url=file:///snap` | fixed jailed name | no |

Note the cmdline move. Today `kernelArgs` lives in the driver
(`internal/vmm/firecracker/fc.go:711`) and its own comment says "every value on
it is something the host asserts and the guest cannot forge". Under Cloud
Hypervisor it has to be built by the helper, which makes those values
unforgeable by a compromised *controller* as well. That is a small tightening,
and it is not optional: there is nowhere else to put it. The x86 arguments drop
`pci=off` and gain `root=/dev/vda rw net.ifnames=0`; on arm64 the console token
becomes `console=ttyAMA0` (Cloud Hypervisor's arm64 UART is a PL011).

### 2.3 The TAP as an inherited descriptor

`createTap` (`server_linux.go:673`) keeps its `ip tuntap add` / `ip addr add` /
`ip link set up` sequence, the per-TAP anti-spoof rules and the Sluice readiness
wait unchanged — Sluice attaches TCX by interface name and never sees a
descriptor. What is new is that after the TAP is up and Sluice is ready, and
before `exec`, the helper opens `/dev/net/tun` itself and attaches a queue:

```go
fd, err := unix.Open("/dev/net/tun", unix.O_RDWR|unix.O_CLOEXEC, 0)
ifr := ...                                   // ifr_name = tapName(slot)
ifr.Flags = unix.IFF_TAP | unix.IFF_NO_PI | unix.IFF_VNET_HDR
unix.IoctlSetInt(fd, unix.TUNSETIFF, ...)    // attach; CAP_NET_ADMIN, held
tapFile := os.NewFile(uintptr(fd), "sbtap")  // → cmd.ExtraFiles[0] → child fd 3
```

The three flags are load-bearing, and I verified the mechanism empirically on
6.18.44 rather than inferring it. Cloud Hypervisor's `Tap::from_tap_fd`
(`net_util/src/tap.rs:235`) issues `TUNGETIFF`, then re-issues `TUNSETIFF` with
`IFF_TAP|IFF_NO_PI|IFF_VNET_HDR` and **ignores `EEXIST`**, then calls
`TUNSETVNETHDRSZ(12)`. On an already-attached descriptor the kernel returns
`EEXIST` from that second `TUNSETIFF`, so the device keeps whatever `TUN_FEATURES`
the helper set. If the helper attaches without `IFF_VNET_HDR`, the re-set is
swallowed, `TUNSETVNETHDRSZ` still **succeeds**, and the guest gets a virtio-net
device whose 12-byte header the tap does not prepend — silent breakage, not an
error. Measured:

```
helper attaches IFF_TAP|IFF_NO_PI            flags 0x1802
  CH TUNSETIFF  -> EEXIST (ignored)          flags 0x1802   (no IFF_VNET_HDR)
  CH TUNSETVNETHDRSZ(12) -> ok
helper attaches IFF_TAP|IFF_NO_PI|IFF_VNET_HDR  flags 0x5802
  CH TUNSETIFF  -> EEXIST (ignored)          flags 0x5802
  CH TUNSETVNETHDRSZ(12) -> ok
```

`NetConfig::validate` (`vmm/src/config.rs:1842`) requires `num_queues ==
fds.len() * 2` and rejects any fd `<= 2`, so one queue pair, one descriptor, and
Go's `ExtraFiles[0] → 3` all line up. The helper closes its own copy after
`cmd.Start()`; the child holds the open file description, so no permission is
re-checked when it drops to the slot UID.

`--api-socket fd=4` uses the same mechanism for the control socket, and it
removes a race as well as a node. Today `publishSocket` (`server_linux.go:435`)
polls for up to five seconds waiting for Firecracker to create `fc.sock` inside
the jail, then chowns and chmods it. Instead the helper does
`net.ListenUnix` at `<jailWorkspace>/api.sock` — *outside* the chroot — chowns it
`root:ControllerGID` mode 0660 before exec, and passes the listening descriptor.
The controller reaches it by traversing `ChrootBase` (0711) and `jailWorkspace`
(0710, controller group), and the VMM never has a path to it. `publishSocket`
disappears; so does the `.lock` file Cloud Hypervisor creates on the path-based
API socket (`vmm/src/api/http/mod.rs:430`, only in `start_http_path_thread`).

**What the jail then contains.** Because the VMM now creates nothing — no API
socket, no serial file (`--serial tty` writes to the inherited stdout, exactly
as Firecracker does today via `cmd.Stdout = os.Stdout`) — the jail root can stop
being writable by the slot UID at all:

| Path | Owner / mode | Why |
| --- | --- | --- |
| `/` (jail root) | `root:root` 0711 | traverse only; the VMM can no longer create, rename or unlink anything at top level (today it is slot-UID-owned 0710) |
| `/cloud-hypervisor` | `root:root` 0555 | `copyExecutable` of the pinned binary |
| `/vmlinux` | `root` 0444 | `linkTrustedResource` |
| `/rootfs.ext4` | slot UID `:ControllerGID` 0660 | `linkStateResource`, `openat2` `RESOLVE_BENEATH` |
| `/snap.next/` | slot UID 0700, empty | the one writable directory; §10.4 |
| `/snap/` | `root:root` 0500 + three hard links | resume only, read-only to the VMM |
| `/dev/kvm` | slot UID 0600, `mknod` 10:232 | `cloneDevice`, unchanged |
| `/dev/urandom` | slot UID 0400, `mknod` 1:9 | new; see below |

Gone: `/dev/net/tun` and `/dev/net`, and the API socket. Added: `/dev/urandom`,
which is not optional — `make_virtio_rng_devices` runs unconditionally, `--rng`
defaults to `src=/dev/urandom`, `Rng::new` does a plain `File::open` at boot, and
there is no flag that removes the device. So the node count does not fall; one
node is swapped for a weaker one. runc's default device cgroup already permits
`c 1:9 rwm` and the helper already holds `MKNOD`, so no device-plugin or Pod
change is needed. `sparkbox.dev/tun` stays allocated to `vmm-helper` — it is now
the only process that opens the TUN device — and
`vmm-helper-entrypoint.sh`'s assertion that `/dev/kvm` and `/dev/net/tun` are
character devices stays, as does the identical check in `newServer`.

### 2.4 Snapshot outputs: the part that does not port

This is the one place the current mechanism cannot be reused, and it is worth
being explicit about why, because three independent facts each rule out an
obvious workaround.

`prepareSnapshotOutputs` (`server_linux.go:605`) creates `mem.snap.next` and
`state.snap.next` with `openat` `O_CREAT|O_EXCL` on an `openat2`
`RESOLVE_BENEATH` directory descriptor in the VM state directory, `Fchown`s them
to the slot UID and the controller group, `Fchmod`s 0660, and publishes them into
the jail with `linkat(fd, "", AT_EMPTY_PATH)`. The VMM then receives the relative
names `mem.snap.next` / `state.snap.next` and never learns a path; the controller
promotes with two `os.Rename`s after the VMM exits (`fc.go:995-1001`), and the
inodes survive `cleanupSlot`'s `os.RemoveAll(jailWorkspace)` because they have a
second link in the VM state directory.

Against Cloud Hypervisor:

- **The destination must be an existing directory.** `url_to_path`
  (`vmm/src/migration/mod.rs:24`) strips `file://` and returns
  `Destination is not a directory` unless the path already resolves to one. It
  does not create it.
- **The three files are created `O_CREAT|O_EXCL` by the VMM.**
  `Vm::send` (`vmm/src/vm.rs:3395`) opens `config.json` and `state.json` with
  `create_new(true)`, and `MemoryManager::send` (`vmm/src/memory_manager.rs:3232`)
  does the same for `memory-ranges`. Pre-creating them and hard-linking them in
  fails with `EEXIST`.
- **A directory cannot be hard-linked.** `link(2)` returns `EPERM` for
  directories, and the helper has no `CAP_SYS_ADMIN` for a bind mount instead.
  A symlink is no use either: the VMM is chrooted, so a symlink out of the jail
  is unresolvable by construction.

So the snapshot inodes can only be created inside the jail, and the jail is
`RemoveAll`ed as soon as the VMM is reaped. The mechanism has to invert: link
**out**, after the VMM exits and before cleanup.

**Where the destination URL comes from.** `url_to_path` takes whatever follows
`file://` verbatim, so `file:///snap.next` is a chroot-absolute path that names
`<jailRoot>/snap.next` on the host. That satisfies the same invariant the
relative filename satisfies today — the string is a constant in the helper
binary, the controller never sends it, and inside the chroot it cannot denote
anything the helper did not place there. (`file://snap.next`, relative to
`cmd.Dir = "/"`, also works, but the absolute form is what upstream documents and
it is the one the Landlock rule has to match.)

**Lifecycle.** The pre-creation moves from pause time to launch time, because
Landlock forces it to (§10.5): a rule can only be added for a path that is
openable when the ruleset is built, and after `restrict_self` the VMM cannot
create the directory itself.

1. `prepareJail` creates `<jailRoot>/snap.next/` (slot UID, 0700, empty) on every
   launch, cold or resume. On resume it also creates `<jailRoot>/snap/`
   (`root:root` 0500) and `linkStateResource`s `config.json`, `state.json` and
   `memory-ranges` from `<VMStateDir>/fc-vms/<name>/snap/` into it. Two distinct
   directories, so "never snapshot over the files you were restored from" is
   structural rather than a naming convention.
2. `OpSnapshot` keeps its name, its `active[slot].name == req.Name && pid != 0`
   check and its position in the Pause sequence, but its body changes: it
   **empties** `snap.next/` — `unlinkat` each entry through a `RESOLVE_BENEATH`
   descriptor — without removing the directory. It must not remove and recreate
   it: Landlock's rule is attached to that directory's inode, and a fresh inode
   would not be covered. Emptying is also what makes a crashed previous pause
   self-healing, which is the property the current `Unlinkat`-then-`Openat`
   sequence provides today and which `create_new(true)` would otherwise take
   away.
3. The controller `POST`s `/api/v1/vm.snapshot` with
   `{"destination_url":"file:///snap.next"}`, then shuts the VMM down.
4. In the launch handler, between `waitCh` returning and `cleanupSlot`, the
   helper promotes. This needs no new verb and no new round trip, and it is the
   only ordering that fits: the stop handshake already guarantees that cleanup
   completes before the acknowledgement, so anything that must outlive the jail
   has to happen inside that window.

```go
// after the VMM is reaped, before os.RemoveAll(jailWorkspace)
func (s *server) promoteSnapshot(name string, slot int) (bool, error) {
    src := filepath.Join(s.jailRoot(slot), "snap.next")   // three files or nothing
    dirFD, err := s.openState(s.vmRel(name), unix.O_PATH|unix.O_DIRECTORY)
    ...
    // A partial directory is not a snapshot: Vm::send writes config.json, then
    // state.json, then memory-ranges, so presence alone does not mean complete.
    for _, n := range []string{"config.json", "state.json", "memory-ranges"} {
        if _, err := os.Stat(filepath.Join(src, n)); err != nil {
            return false, nil                              // leave the old snapshot alone
        }
    }
    unix.Mkdirat(dirFD, "snap", 0o770)                     // idempotent
    unix.Mkdirat(dirFD, "snap.next", 0o770)
    for _, n := range ... {                                // link out, chown, chmod 0660
        fd, _ := unix.Openat(...)                          // in the jail, O_RDONLY|O_NOFOLLOW
        unix.Fchown(fd, s.jailUID(slot), s.opts.ControllerGID)
        unix.Fchmod(fd, 0o660)
        unix.Linkat(fd, "", dirFD, "snap.next/"+n, unix.AT_EMPTY_PATH)
    }
    // Atomic promotion: swap the two directories, then discard the old one.
    if err := unix.Renameat2(dirFD, "snap", dirFD, "snap.next",
        unix.RENAME_EXCHANGE); err != nil { return false, err }
    os.RemoveAll(filepath.Join(s.opts.VMStateDir, s.vmRel(name), "snap.next"))
    return true, nil
}
```

`RENAME_EXCHANGE` is what replaces the two `os.Rename`s. A plain rename cannot be
used — `rename(2)` over a non-empty directory returns `ENOTEMPTY` — and
remove-then-rename has a window in which the sandbox has no snapshot at all. The
exchange is atomic and leaves the previous snapshot intact under `snap.next/`
until it is explicitly discarded, so a crash between the exchange and the
`RemoveAll` costs disk, not correctness. I confirmed the semantics on ext4 here;
XFS supports `RENAME_EXCHANGE` and the hot tier is XFS, but that is worth an M1
assertion rather than an assumption. `Renameat2` is addressed through the same
`RESOLVE_BENEATH` descriptor with single-component names, so it inherits the
escape resistance of `linkStateResource`.

The link-out requires the jail and the VM state directory to share a filesystem.
They already must — `linkJailedResource` says so in as many words
(`fc.go:947-949`) — and on CKS both live under `$hot_dir`.

Failure surfacing changes with it. Today `Pause` performs the promotion and
returns the error directly; now the promotion happens on the helper side, so its
error rides the launch connection's final `Response` and reaches the driver as
the launch client's exit. That argues for the Cloud Hypervisor driver calling
`vmhelper.RunLaunchClient` in-process rather than through
`vmhelper.LaunchCommand`: the separate process exists only because
`firecracker-go-sdk` wants an `*exec.Cmd` for `WithProcessRunner`, and without
the SDK the driver can hold the connection itself, cancel a context instead of
sending SIGTERM, and read the typed error out of the final response. The protocol
is unchanged either way; `SO_PEERCRED` still sees the controller UID.

### 2.5 Landlock rules

`--landlock` builds one ruleset from the `VmConfig` and applies it with
`restrict_self` on the VMM thread inside `vm_create`
(`vmm/src/lib.rs:2211-2220`), after `pre_create_console_devices` and before the
VM exists. `vmm/src/landlock.rs` pins `ABI::V3` as a `HardRequirement`, so on a
Node whose kernel lacks Landlock ABI v3 (`CONFIG_SECURITY_LANDLOCK=y`, landlock
in `CONFIG_LSM`/`lsm=`, kernel ≥ 6.2) `vm.create` **fails** rather than degrading.
That is a fourth Node precondition alongside the two in §3, it is CoreWeave's to
set, and `hack/probe-nested-virt.sh` does not check it — so the helper computes
it at startup next to `nestedOK` and omits `--landlock` when it is absent, rather
than failing every launch.

Given the §10.3 layout, the effective ruleset is:

| Path (inside the chroot) | Access | Where the rule comes from |
| --- | --- | --- |
| `/rootfs.ext4` | `rw` | `DiskConfig::apply_landlock` |
| `/vmlinux` | `r` | `PayloadConfig::apply_landlock` |
| `/dev/urandom` | `r` | `RngConfig::apply_landlock` |
| `/snap.next` | `rw` | `--landlock-rules path=/snap.next,access=rw` |
| `/dev/net/tun` | `rw` | added unconditionally when `net.is_some()`; **silently dropped**, because the node no longer exists in the jail |

Three details make this work, and each would break it if it were otherwise.
`"w"` in Cloud Hypervisor's mapping includes `MakeReg`, `MakeDir`, `Refer` and
`Truncate` (`vmm/src/landlock.rs:133-146`), so an `rw` rule on `/snap.next` is
what lets the VMM create the three files inside it. The rust-landlock helper
`path_beneath_rules` is a `filter_map` that drops any path it cannot open, so the
now-absent `/dev/net/tun` rule is discarded without error — `--net fd=` and
`--landlock` are compatible only because of that. And the rule paths are
chroot-relative constants, which matters because on a restore the ruleset comes
from the snapshot's `config.json` rather than the command line: a slot change
between pause and resume changes the host path of the jail but not a single
string in the ruleset.

`/dev/kvm` needs no rule — it is opened by `hypervisor::new()` before `vm_create`
— and the API socket needs none because the helper passes the descriptor. Nothing
grants write access to the jail root, so with `--landlock` the VMM's writable set
is exactly `rootfs.ext4` and `snap.next/`.

Note what Landlock is and is not doing here. It is defence in depth *inside* an
already four-inode chroot, and its practical value is that it survives a chroot
escape and blocks the `openat` half of a directory-traversal bug. It is also the
mitigation upstream itself names in the CVE-2026-27211 advisory, which is a
reason to treat the preflight as a gate rather than a nicety.

### 2.6 Launch and stop handshake

| Behaviour | Firecracker today | Cloud Hypervisor | Consequence for the helper |
| --- | --- | --- | --- |
| SIGTERM | terminates the VMM | `Vmm::HANDLED_SIGNALS = [SIGTERM, SIGINT]`; the handler thread writes `exit_evt` and the VMM shuts the VM down and exits | `stopProcess` (SIGTERM, 10 s, SIGKILL) is unchanged |
| Guest-initiated reboot | VMM exits (`reboot=k`), the launch connection ends, the slot is torn down and rebuilt | the VM is destroyed and re-created **inside the same process**; the VMM does not exit | one launch connection can now span many guest boots. `active[slot].pid` stays valid, so `OpCPUTime` keeps working across a reboot instead of losing its subject. Nothing in the helper needs to change, but "VMM exit ⇒ guest is gone" stops being true and the driver's `Rebooter` (host-driven pause → `DropSnapshots` → cold boot) remains the only reboot Sparkbox observes |
| Disk locks | none | byte-range advisory locks taken in `try_lock_disks` at boot **and at restore**, released only at VMM shutdown (`vmm/src/device_manager.rs`) | the existing guarantee that cleanup completes before the stop acknowledgement is now load-bearing for correctness, not just tidiness: a leaked VMM still holding the lock makes the next `Resume` on that sandbox fail where Firecracker would simply have started. `cleanupSlot` must reap the process, which it already does |
| Cleanup ordering | `waitCh` → `cleanupSlot` → respond | `waitCh` → **promote snapshot** → `cleanupSlot` → respond | the new step is three `linkat`s and one `renameat2`, all metadata-only, well inside `RunLaunchClient`'s 15 s cleanup budget. The slot-reuse guarantee is unchanged because `delete(s.active, req.Slot)` still runs in the deferred close after cleanup |
| API socket readiness | `publishSocket` polls for the socket, then chowns it | the listener exists before `exec` | delete `publishSocket`; the "VMM exited before creating its socket" error class disappears, and with it the five-second window in which a slot is half-live |
| VM configuration | sent over the API after start | fixed at `exec`; boot and restore are disjoint command lines | the helper builds two argument lists, and `Resume` selects between them. Mixing them is the dangerous failure: `--kernel` alongside `--restore` cold-boots and discards the snapshot with no error at all, so the two builders should not share a slice |

Unchanged: `SO_PEERCRED` peer authentication, the `vmNameRE` name check, the
subnet-bounded slot check, `openat2` `RESOLVE_BENEATH|NO_SYMLINKS|NO_MAGICLINKS`
for every state-directory resolution, TAP source pinning and the named
`SPARKBOX_GUEST_*` chains, the Sluice readiness wait, `sweepStaleTaps`, the
half-close stop request, the slot-exclusive `active` map, and `OpCPUTime`'s
`/proc/<pid>/stat` read. The helper still has no network listener, no
service-account token, no shell RPC and no caller-controlled path.

### 2.7 Acceptance for this piece, in M1

- A driver-level unit test asserts the **complete** argv for a cold boot and for
  a resume, byte for byte, the way `hack/check-cks-pin.sh` asserts checksums.
  `--console` defaults to `tty` and `--serial` to `null`, so an omitted flag adds
  or loses a device silently rather than failing.
- A test asserts that no `vm-config` flag appears on the resume line and that
  `--kernel` never co-occurs with `--restore`.
- A helper test asserts `TUNGETIFF` reports `IFF_VNET_HDR` on the descriptor
  handed to the child.
- A helper test kills the VMM mid-snapshot and asserts the previous snapshot
  directory is intact and `SnapshotPromoted` is false.
- A `v1` request against a `v2` helper, and the reverse, are both refused.

### Open questions from this section

- Does micro_http's `HttpServer::new_from_fd` accept a listening Unix socket descriptor created by Go's `net.ListenUnix(...).File()`? Go returns a blocking dup; Cloud Hypervisor's own path-based route hands it a descriptor from Rust's `UnixListener::bind`, which is also blocking, so the shapes match — but I did not read micro_http (it is an out-of-tree crate) and could not confirm whether it sets `O_NONBLOCK` itself or assumes the caller did. If it assumes non-blocking, the helper must clear/set the flag before `exec`. This is the only unverified link in the `--api-socket fd=` design.
- Does clap treat `--landlock`'s `.default_value("false")` as absent for group-requirement purposes, so that a restore command line carrying no `vm-config` flag parses? Upstream's own `docs/snapshot_restore.md` shows restore invocations with no VM-config arguments and every one of `--cpus`, `--memory`, `--rng`, `--serial`, `--console` and `--landlock` carries a default value, so it must — but this is inferred from the group table (`cloud-hypervisor/src/main.rs:511-517`) plus upstream docs rather than executed. It should be an executed assertion in M1, because the failure mode is that resume never works at all.
- Does XFS support `renameat2(RENAME_EXCHANGE)` on directories on the CKS hot tier's kernel and mkfs options? I verified the semantics on ext4 in this environment; XFS implements the flag, but the atomic-promotion design depends on it and the fallback (remove-then-rename) has a window with no snapshot.
- Is `memory_restore_mode` worth passing at all on the pinned version? At v53.0 the enum is only `Copy` and `OnDemand` (`vmm/src/config.rs:2779-2803`); `CopyOnWrite` exists only on `main`. `ondemand` is unreachable from the jail as designed (no `/dev/userfaultfd` node, capability-free slot UID, and `userfaultfd` is absent from containerd's `RuntimeDefault` allowlist), and it fails the restore rather than falling back. So on v53.0 the helper has exactly one usable mode and should omit the parameter — but if the pin moves forward to pick up `copyonwrite`, the flag becomes a helper-side decision and the section above needs a fourth restore argument.
- Should the helper ever rewrite `cpus.nested` in a snapshot's `config.json` to enforce `nested=off` on a resume? The design above says no, on the grounds that parsing guest-adjacent JSON in the privileged helper is exactly the debt `security-hardening.md` wants removed — but that leaves the helper unable to enforce the §7 gate on the restore path, and the only real enforcement is the Node's `kvm_{intel,amd}.nested` parameter. Someone has to decide whether that is acceptable or whether the controller's cold-boot-via-`DropSnapshots` fallback is made mandatory (and testable) instead.
- Does `--serial tty` remain the right parity choice once the guest console is expected to survive a Cloud Hypervisor in-process reboot? `pre_create_console_devices` dups stdout specifically so the descriptor survives `vm_shutdown` (`vmm/src/console_devices.rs:242-257`), which suggests yes, but Sparkbox has never run a VMM that reboots in place and nobody has watched what the Pod log does across one.
- Does Cloud Hypervisor's `try_lock_disks` take its byte-range advisory lock on the *jailed* hard link, and does the lock therefore conflict with the controller's own access to the same inode in the VM state directory (`ext4DiskMB`'s superblock read, `stoppedRootfs`)? Advisory locks bind to the inode, not the path, so a conflict would be with any other lock-taker rather than with plain reads — but the interaction between an in-jail lock and the host-side `e2fsck`/`zerofree` path in `PackRootfs` has not been checked.
- Whether the vCPU/memory scalars now crossing the helper boundary should instead be bounded by a per-owner policy the helper can see, rather than the flat `MaxVCPUs`/`MaxMemMB` proposed here. A compromised controller can still launch every slot at the maximum; today it can do the same over the Firecracker API, so this is parity rather than a regression, but the helper is now the natural place to cap it and someone should decide whether it should.

## 3. Kernel, artifacts and rollout

Everything between "the driver compiles" and "a CKS Pod is running it". §2 gives
this three table rows and §9 gives it four bullets; the work is bigger than that
in two places (the guest kernel is one artifact shared by every sandbox, and the
CKS deployment cannot run two node Pods) and smaller than feared in one (a
paused sandbox's memory snapshot is already discarded on every roll, so the VMM
switch costs it nothing extra). All config-symbol claims below were checked on
2026-09-04 against the two upstream files `hack/build-kernel.sh` actually
fetches: `microvm-kernel-ci-{x86_64,aarch64}-6.1.config` on Firecracker's
`main`.

### 3.1 The guest kernel: one artifact, four symbols, one arch-specific gap

**One kernel per arch serves both VMMs, and both nested and non-nested guests.**
The decisive fact is one line in each CI config:

| Symbol | x86_64 CI 6.1 | aarch64 CI 6.1 | Why it decides this |
| --- | --- | --- | --- |
| `CONFIG_VIRTIO_MMIO` | `=y` (l.2401) | `=y` (l.2456) | Firecracker's default transport. |
| `CONFIG_VIRTIO_PCI` | `=y` (l.2395) | `=y` (l.2450) | Cloud Hypervisor's transport. |
| `CONFIG_PVH` | `=y` (l.335) | absent (x86 symbol) | CH's x86 loader returns `KernelMissingPvhHeader` for an ELF without the note (`vmm/src/vm.rs::load_kernel`). Convenience under Firecracker, a hard boot dependency under CH. |
| `CONFIG_PCI_HOST_GENERIC` / `CONFIG_PCI_ECAM` | — | `=y` (l.1523, l.1505) | Matches the `pci-host-ecam-generic` node CH emits in the arm64 FDT. |
| `CONFIG_MODULES` | `# not set` (l.761) | `# not set` (l.729) | Everything is built in. "Ship KVM as a module and load it only for nested sandboxes" is not available without turning modules on for every guest. |
| `CONFIG_VIRTUALIZATION` | `# not set` (l.614) | `# not set` (l.610) | The only thing standing between us and a nested-capable guest on x86. |
| `CONFIG_HAVE_KVM`, `CONFIG_HIGH_RES_TIMERS`, `CONFIG_X86_LOCAL_APIC`, `CONFIG_IA32_FEAT_CTL` | `=y` (l.613, 105, 372, 353) | (n/a) | Every x86 dependency of `CONFIG_KVM_{INTEL,AMD}` is already satisfied. |
| `CONFIG_SERIAL_AMBA_PL011` | (n/a) | **`# not set` (l.1992)** | CH's arm64 UART is a PL011; Firecracker's is an `ns16550a`. This is the one real gap. |
| `CONFIG_VIRTIO_CONSOLE` / `CONFIG_HVC_DRIVER` | `=y` (l.1961, 1958) | `=y` (l.2018, 2015) | The zero-config alternative to fixing PL011. |

Both transports are compiled in, so nothing has to be *removed* for either VMM.
That is what makes one artifact possible; the rest is additive.

**Fragment additions, x86_64 only — four lines, not two.** §2 and the Conclusion
both undercount this (§2 says "Two fragment lines", the Conclusion "two
kernel-config lines"); a kconfig fragment carries one symbol per line and the
sentence itself names four. Correct both.

```
# hack/kernel-config-x86_64.fragment
# Nested virtualization inside the guest (Cloud Hypervisor --cpus nested=on).
# Inert on a nested=off guest: kvm_intel/kvm_amd are built-in initcalls that
# probe CPUID for VMX/SVM and bail without it, so no /dev/kvm appears.
CONFIG_VIRTUALIZATION=y
CONFIG_KVM=y
CONFIG_KVM_INTEL=y
CONFIG_KVM_AMD=y
```

```
# hack/kernel-config-arm64.fragment
# Cloud Hypervisor's arm64 UART is a PL011 (arch/src/aarch64/fdt.rs emits
# compatible = "arm,pl011\0arm,primecell\0"); Firecracker's is an ns16550a and
# the CI config carries only 8250. Without this an arm64 guest under CH boots
# and SSHes fine and prints nothing at all to --serial.
CONFIG_SERIAL_AMBA_PL011=y
CONFIG_SERIAL_AMBA_PL011_CONSOLE=y
```

The arm64 alternative is `--console` with `console=hvc0`, which costs no config
change at all (`CONFIG_VIRTIO_CONSOLE=y` and `CONFIG_HVC_DRIVER=y` are already
set on both arches). Prefer the PL011 lines anyway: §9 M1 sets `--console off`
and `--serial file=<jail>/console.log` on x86 for attack-surface reasons, and
taking the virtio-console path on one arch only would make the guest's console
*device class* differ per arch as well as per VMM. Two config symbols is the
cheaper divergence than a second console mechanism.

**Why the fragment has to split per arch.** `build-kernel.sh` has one
`$FRAGMENT` (l.24) and one arch-blind verification loop (l.77-83). `merge_config.sh`
does not validate symbols and `make ARCH=arm64 olddefconfig` silently drops
`CONFIG_KVM_INTEL`/`CONFIG_KVM_AMD`, which do not exist for arm64 — so a shared
fragment would still produce a *correct* arm64 `.config` for those two. But
`CONFIG_VIRTUALIZATION` and `CONFIG_KVM` do exist on arm64 and would be enabled
there, where KVM/arm64 initialises only if the kernel booted at EL2 — which a
Cloud Hypervisor arm64 guest does not — so it is dead weight in the one image
every sandbox on the DGX and the macOS machine boots. Since PL011 is arm64-only
anyway, split:

```sh
# after the ARCH case block that sets KARCH (l.33-37)
ARCH_FRAGMENT=${ARCH_FRAGMENT:-$HERE/kernel-config-$KARCH.fragment}
...
ARCH=$KARCH ./scripts/kconfig/merge_config.sh -m .config "$FRAGMENT" "$ARCH_FRAGMENT"
```

**The verification loop becomes per-arch, and gains rows it does not have
today.** Asserting `CONFIG_KVM_INTEL` on an arm64 build would `exit 1` on every
arm64 kernel, so the loop cannot stay one flat list:

```sh
verify=(CONFIG_TUN CONFIG_IP_NF_RAW CONFIG_NF_TABLES CONFIG_VXLAN CONFIG_WIREGUARD
        CONFIG_VIRTIO_PCI CONFIG_VIRTIO_MMIO)
case "$KARCH" in
  x86_64) verify+=(CONFIG_VIRTUALIZATION CONFIG_KVM CONFIG_KVM_INTEL CONFIG_KVM_AMD
                   CONFIG_PVH) ;;
  arm64)  verify+=(CONFIG_SERIAL_AMBA_PL011 CONFIG_SERIAL_AMBA_PL011_CONSOLE
                   CONFIG_PCI_HOST_GENERIC) ;;
esac
for sym in "${verify[@]}"; do
  grep -q "^${sym}=[ym]" .config || { echo "ERROR: ${sym} did not end up enabled" >&2; exit 1; }
  echo "  ok: $(grep "^${sym}=" .config)"
done
```

`CONFIG_PVH`, `CONFIG_VIRTIO_PCI`, `CONFIG_VIRTIO_MMIO` and
`CONFIG_PCI_HOST_GENERIC` come from the *base* config, not from our fragment,
and they are the rows that earn their place: `BASE_CONFIG_URL` (l.39) points at
`.../firecracker/main/resources/guest_configs/...` — a moving branch, not a tag.
Upstream is free to drop `CONFIG_PVH` from its CI config tomorrow, and the only
thing between that and a release whose kernel silently will not boot under Cloud
Hypervisor is this loop. `# CONFIG_MODULES is not set` means every one of these
lands as `=y`, so the existing `=[ym]` pattern matches unchanged.

**One kernel or two.** One, and the argument is entirely from the config above:
the transports coexist, so nothing is subtracted; `CONFIG_KVM=y` is inert on a
guest that never sees VMX/SVM, so nothing is exposed; and modules are off, so
the load-it-on-demand middle option does not exist.

State the counter-argument honestly, because it is the only real one: a shared
kernel means the guest kernel is *not* a second gate. Today "no nested" is
enforced by Cloud Hypervisor's CPUID masking plus the Node's `kvm_*.nested`; with
`CONFIG_KVM=y` in the one shared image, a regression in that masking gives every
sandbox a working `/dev/kvm`, not just the opted-in ones. That is a real
widening of the blast radius of a single upstream bug, and it is what a second
kernel would buy.

What a second kernel would cost, which is more than "another asset, another
checksum, another pin":

- `stage-artifacts.sh`: a second `KERNEL_NESTED` input and a second
  `SHA256_VMLINUX_NESTED` key. `build-artifacts.yml`: a second ~15-minute
  kernel build per arch (the kernel step is already the slowest in the matrix).
- `entrypoint.sh`: two more pinned constants, one more `fetch_checked`, one more
  `check-cks-pin.sh` row.
- **The blocker.** The helper takes one `--kernel` path at startup
  (`cmd/sparkbox-vmm-helper/main.go`) and hard-links exactly that file into every
  jail under the fixed name `vmlinux`
  (`internal/vmhelper/server_linux.go:491`, `linkTrustedResource(root, s.opts.KernelPath, jailedKernelName)`).
  The controller sends no paths, by design. A per-sandbox kernel is therefore a
  *protocol* change — a second startup flag plus a kernel selector in the launch
  Request — on top of the `nested` field §2 already proposes. Same shape of
  change, so not enormous, but it is helper work rather than asset plumbing, and
  §9 M1 does not budget it.

Recommendation: one kernel per arch. Revisit only if M2 measures a material size
or boot-time delta, or if M3 decides a shared KVM-capable guest kernel is itself
unacceptable. Measure rather than assume: the workflow already prints
`sha256sum` after the kernel build and a `du -h` of every staged asset into the
release step summary, so the size delta is visible for free on the first release
that carries it.

**One CI trap to avoid.** The guest-kernel cache key is

```
guest-kernel-${{ matrix.arch }}-${{ env.KERNEL_VERSION }}-${{ hashFiles('tools/sparkbox/hack/build-kernel.sh', 'tools/sparkbox/hack/kernel-config.fragment') }}
```

Adding per-arch fragments without adding them to `hashFiles` means CI restores a
cached kernel built before the change and publishes a release whose `vmlinux`
has no `CONFIG_KVM` — with a green build and a correct-looking checksum. That is
the same silent-staleness failure `check-cks-pin.sh` exists to prevent, one
layer up. Change it to `hashFiles('tools/sparkbox/hack/build-kernel.sh', 'tools/sparkbox/hack/kernel-config*.fragment')`
in the same commit that adds the files.

### 3.2 Release artifacts and pins

**What upstream publishes.** From
`cloud-hypervisor/.github/workflows/release.yaml` on `main` (fetched
2026-09-04), the release job uploads per target triple:

| Target triple | VMM asset | Companion | Linkage |
| --- | --- | --- | --- |
| `x86_64-unknown-linux-gnu` | `cloud-hypervisor` | `ch-remote` | glibc, dynamic |
| `x86_64-unknown-linux-musl` | `cloud-hypervisor-static` | `ch-remote-static` | musl, static |
| `aarch64-unknown-linux-musl` | `cloud-hypervisor-static-aarch64` | `ch-remote-static-aarch64` | musl, static |

plus a vendored source tarball on the gnu leg. Take the two musl binaries.
Static is not a preference here, it is a requirement: the VMM is exec'd already
chrooted into a jail containing the binary, `/dev/kvm`, `/dev/urandom`, the
kernel and the disk and no libc, so a dynamically linked `cloud-hypervisor`
could not start. Do not ship `ch-remote` — the driver speaks the REST API
directly, and a second executable in `$asset_dir` buys nothing.

Two things to get right: the x86_64 asset is `cloud-hypervisor-static`, with no
arch in the name, while the aarch64 one carries `-aarch64`. They cannot be
templated off the `$upstream_arch` variable `entrypoint.sh` uses for
Firecracker's `.tgz` URL. That variable is only used for the upstream-jailer
fallback, so the asymmetry stays confined to `build-artifacts.yml` if it is
written there as an explicit `case`.

**No checksums, no signatures.** The workflow's `softprops/action-gh-release`
step uploads the binaries and the tarball and nothing else. That is the same
position we are in with Firecracker, and the trust model is unchanged: CI
downloads over TLS, `stage-artifacts.sh` hashes what arrived, and that hash
becomes the pin every Node enforces. Say what it buys plainly — it pins *what CI
downloaded*, not *what upstream intended*. Nothing in this chain would detect a
compromised upstream release between publication and our build. (I did not
confirm whether the release pages carry a GitHub provenance attestation; the
asset listing would not render for me. Worth one look, and worth `gh attestation
verify` in the download step if one exists.)

**`build-artifacts.yml`.** One env line beside `FIRECRACKER_VERSION: v1.16.1`,
and one step in the `artifacts` matrix job mirroring the Firecracker download:

```yaml
env:
  CLOUD_HYPERVISOR_VERSION: v53.0   # >= v52.0; see the floor assertion below
```

```yaml
      - name: Download cloud-hypervisor ${{ env.CLOUD_HYPERVISOR_VERSION }}
        run: |
          case "$(uname -m)" in
            x86_64)  asset=cloud-hypervisor-static ;;
            aarch64) asset=cloud-hypervisor-static-aarch64 ;;
            *) echo "unsupported arch $(uname -m)" >&2; exit 1 ;;
          esac
          ver=${CLOUD_HYPERVISOR_VERSION}
          curl -fsSLo cloud-hypervisor \
            "https://github.com/cloud-hypervisor/cloud-hypervisor/releases/download/${ver}/${asset}"
          sudo install cloud-hypervisor /usr/local/bin/cloud-hypervisor
          cloud-hypervisor --version
```

The `prepare` job's release-notes heredoc should gain a paragraph for
`cloud-hypervisor-$arch` next to the one it already writes for `jailer-$arch`,
saying that no jailer accompanies it (the chroot launcher is the jail) and
naming the v52.0 floor.

**`stage-artifacts.sh`.** Copy the binary alongside firecracker/jailer, and add
two manifest keys. The Firecracker leg asserts a *pairing* (FC and jailer from
one release, l.194-197); Cloud Hypervisor has no jailer to pair with, but it has
a floor, and §1's AMD/arm64 findings make that floor a correctness property
rather than a preference — so the producer should assert it rather than leaving
it in prose:

```sh
CH_VER=$("$CLOUD_HYPERVISOR_BIN" --version | grep -oE 'v[0-9]+\.[0-9]+' | head -1)
# v52.0 floor: before it, `nested=off` was a silent no-op on AMD hosts (the
# CPUID loop broke out on leaf 1 and never reached the SVM leaf) and a hard
# parse error on arm64. Either one breaks §7's "nested=off is the driver
# default" guarantee on a Node we ship to.
[ -n "$CH_VER" ] && [ "$(printf '%s\nv52.0\n' "$CH_VER" | sort -V | head -1)" = v52.0 ] || {
  echo "cloud-hypervisor $CH_VER is below the v52.0 floor" >&2; exit 1; }
```

```
CLOUD_HYPERVISOR_VERSION=$CH_VER
SHA256_CLOUD_HYPERVISOR=$(sha "$OUT_DIR/cloud-hypervisor-$GOARCH")
```

**Consumers of the manifest, in the order they must change.**

| File | Change | Note |
| --- | --- | --- |
| `hack/stage-artifacts.sh` | emit `SHA256_CLOUD_HYPERVISOR`, `CLOUD_HYPERVISOR_VERSION` | **First.** Everything downstream degrades silently until this ships. |
| `internal/hostsetup/manifest.go` | `SHA256CloudHV` field; `SHA256_CLOUD_HYPERVISOR` in `ParseManifest`; `cloudHypervisorAsset(arch)`; make `Artifacts()` VMM-aware | `Artifacts()` (l.262) returns an unconditional `{vmlinux, firecracker, rootfs}`. Left alone, every `sparkbox setup` host downloads a VMM it will not run. |
| `internal/hostsetup/config.go` | `Config.VMM`, `Config.CloudHypervisorBin` (default `/usr/local/bin/cloud-hypervisor`, beside `FirecrackerBin` at l.423) | |
| `internal/hostsetup/steps.go` | the `{kernel, firecracker, rootfs}` freshness triple (l.731) becomes VMM-aware | Otherwise `setup --dry-run` on a Cloud Hypervisor host plans a Firecracker download forever. |
| `internal/devpod/source.go` | `ReleaseManifest.SHA256CloudHypervisor` (field list l.64-78, parse l.313-330) | **Do not add it to the required-keys map** (l.325-333, which errors on a missing `SHA256_FIRECRACKER`/`SHA256_JAILER`): every already-published manifest must keep parsing, and a future CH-only release must be able to drop the Firecracker keys without breaking the round-trip. Regenerate `internal/devpod/release/manifest-{amd64,arm64}.env`. |
| `hack/stage-darwin-artifacts.sh` | emit the key; do **not** add it to the hard-fail list at l.102-105 | Same reason. |
| `deploy/kubernetes/entrypoint.sh` | two constants, one fetch | §10.4. |
| `deploy/kubernetes/vmm-helper-entrypoint.sh` | `--cloud-hypervisor "$asset_dir/cloud-hypervisor"` | §10.3. |

**The exact rows `check-cks-pin.sh` gains.** One `check` line per arch loop, and
two `readonly` constants it reads with `sed`:

```sh
# hack/check-cks-pin.sh, in the per-arch loop
  check cloud-hypervisor "cloud_hypervisor_sha256_$arch" SHA256_CLOUD_HYPERVISOR
```

```sh
# deploy/kubernetes/entrypoint.sh, beside the firecracker/jailer pins
readonly cloud_hypervisor_sha256_amd64="${SPARKBOX_CLOUD_HYPERVISOR_SHA256_AMD64:-<from manifest-amd64.env>}"
readonly cloud_hypervisor_sha256_arm64="${SPARKBOX_CLOUD_HYPERVISOR_SHA256_ARM64:-<from manifest-arm64.env>}"
readonly cloud_hypervisor_version="${SPARKBOX_CLOUD_HYPERVISOR_VERSION:-v53.0}"
```

The constant name must be underscored (`cloud_hypervisor_sha256_$arch`, not
`cloud-hypervisor-…`), because `value_of()` matches
`^readonly $1="\${[A-Z_0-9]*:-\(.*\)}"$` and a hyphen is not a valid shell
identifier anyway; the override env name must match `[A-Z_0-9]*`, which
`SPARKBOX_CLOUD_HYPERVISOR_SHA256_AMD64` does. The `check` *label* may contain a
hyphen — it is only printed.

**The ordering trap, and it is the one that bites.** `check()` prints
`skip <label> (not in manifest-<arch>.env)` and sets no failure status when the
manifest lacks the key. Committing the row before `stage-artifacts.sh` emits it
therefore produces a green CI over an unverified pin — precisely the silent-pass
shape this script was written to close, one key over. The order is fixed:

1. `stage-artifacts.sh` + `build-artifacts.yml` publish `SHA256_CLOUD_HYPERVISOR`
   for both arches.
2. Cut a release.
3. Read the values out of the published `manifest-<arch>.env` and commit them
   into `entrypoint.sh` — the script's own trailer already says the constants
   "can only be filled in AFTER that release is published".
4. Add the `check` row.

Worth one line of hardening while there: promote `check`'s skip to a failure for
a declared-mandatory key set, so the next artifact added cannot repeat this.

### 3.3 Choosing the VMM

**Pick one selector name.** §2 proposes `--vmm firecracker|cloud-hypervisor` and
§9 M2 proposes `SPARKBOX_VMM=cloud-hypervisor`; they are the same knob and the
doc should say so once. Neither exists today: `grep -rn SPARKBOX_VMM` hits only
this document, and `deploy/kubernetes/entrypoint.sh` execs
`sparkbox serve --driver firecracker` as a literal.

Proposal: **one value, three surfaces, no new controller flag.**

| Surface | Shape | Default |
| --- | --- | --- |
| Pod spec | `SPARKBOX_VMM` env on both `sparkbox-node` and `vmm-helper` | `firecracker` |
| Controller | the existing `--driver` enum gains a third value (`main.go:113`, switch at `:513`; a second switch at `node.go:208`; a third default at `devpod.go:33`) | `mock` for `serve`, `firecracker` on CKS |
| Helper | `--cloud-hypervisor <path>` beside `--firecracker <path>` (both fixed, both startup-only) | — |
| `setup`/`doctor` | `hostsetup.Config.VMM` | `firecracker` |

Do **not** add a `--vmm` flag beside `--driver`. `--driver` already is the enum,
`mock` is already a VMM choice inside it, and a second flag creates
`--driver firecracker --vmm cloud-hypervisor`, which has no meaning. The one
place a new field is genuinely needed is `hostsetup.Config`, because `setup` and
`doctor` must know which binary to fetch and check without a driver ever being
constructed.

`entrypoint.sh` should validate rather than pass through, because an unvalidated
typo reaches `unknown driver %q` *after* the Pod has downloaded its assets and
`Recreate` has already torn down every VM on the Node:

```sh
readonly vmm="${SPARKBOX_VMM:-firecracker}"
case "$vmm" in
  firecracker|cloud-hypervisor) ;;
  *) echo "SPARKBOX_VMM must be firecracker | cloud-hypervisor" >&2; exit 2 ;;
esac
```

The two entrypoints are separate containers reading the same env, so a
disagreement between them is possible and its symptom is illegible: the
controller dials `fc.sock` in a jail whose VMM never created it, and sees a boot
timeout rather than a configuration error. Set `SPARKBOX_VMM` once on the Pod
spec, read it in both, and have the helper reject a launch whose requested VMM is
not the one it was configured with — one more reason the protocol bump to v2
should carry a VMM field alongside `nested`.

**How `doctor` answers "which VMM is this host running, and is nested
available".** Today `DefaultChecks()` (`checks.go:67`) registers
`{"firecracker binary", checkFirecracker}` at l.76, and that check *fails*
(l.322-331) when neither `$PATH` nor `cfg.FirecrackerBin` has the binary. A
healthy Cloud Hypervisor node therefore fails `doctor` on a binary it never
execs. Two rows:

```go
{"vmm binary", checkVMM},                    // replaces "firecracker binary"
{"nested virtualization", checkNested},      // new, after "hardware virtualization"
```

`checkVMM` keeps `checkFirecracker`'s exact shape — `LookPath`, fall back to the
configured path, run `--version`, `pass(firstLine(out))` — dispatching on
`cfg.VMM`, so `doctor` prints `cloud-hypervisor v53.0` where it printed
`Firecracker v1.16.1`. That line is the operator's answer to "which VMM", and it
has to come from executing the binary that would actually be launched, not from
echoing a flag, or it reports intent rather than fact. On CKS, note that `doctor`
runs in the `sparkbox-node` container, which holds no `/dev/kvm` and never execs
a VMM — so `cfg.CloudHypervisorBin` must point into `$asset_dir`, which both
containers mount.

`checkNested` reports and never fails, and mirrors `hack/probe-nested-virt.sh`
so there is one set of rules and not two:

| Condition | Result |
| --- | --- |
| arch is arm64 | `pass` — "not applicable (nested is x86-64-only in Cloud Hypervisor)" |
| VMM is firecracker | `warn` — "unavailable (Firecracker has no nested support)". *Unavailable*, not *disabled*: per §1 the guest may still see the CPUID bit. |
| `/sys/module/kvm_{intel,amd}/parameters/nested` reads off | `warn` — naming the parameter and its value |
| parameter on, kernel below the §3.2 CVE floor | `fail` — naming the CVE and the required stable release |
| all clear | `pass` — "available (kvm_amd.nested=1, kernel 6.12.104)" |

Read the parameter; do not compare it to `1`. Intel's is
`module_param(nested, bool, 0444)` and renders as `Y`/`N`; AMD's is declared
`int` and renders `1`/`0`. `probe-nested-virt.sh` already accepts `1|Y|y` and the
Go check must match it. Note also that two existing remediation strings conflate
the two senses of the word — `checkVirt` tells the operator to "enable nested
virtualization / VT-x in BIOS" when it means *host* virtualization
(`checks.go:270`). Reword them in the same commit, or `doctor` will have two
lines about "nested" that mean different things.

**Per-sandbox VMM on one host: not with the current shape, and the cost is not
where §9 M3 puts it.** M3 says both drivers "can coexist on one node, keyed per
sandbox, because the helper builds the command line per launch". The helper half
is true. The controller half is not:

- `serve` builds exactly one `vmm.Driver` from a switch and hands it to
  `host.Manager`. There is no multiplexing driver.
- Two driver instances would each run their own slot allocator. `freeSlot()`
  (`fc.go:690`) scans only that instance's `d.vms`, and the slot *is* the tap
  name `sbtap<slot>`, the jail uid `100000+slot` and the guest `/30`. Both
  instances would hand out slot 0 and the second `createTap` would fail with
  "Device or resource busy". The jail path would not collide — it is keyed on
  the VMM binary's basename in both `server_linux.go:808` and `fc.go:397` —
  which is exactly why the collision surfaces as a tap error rather than as
  anything legible.

Making it real needs a multiplexing `vmm.Driver` owning one shared slot
allocator, a `VMM` field on `host.Sandbox` persisted in the ledger (because
§10.4's snapshot rule is per-sandbox), and a per-launch VMM field in the helper
protocol. That is a feature, not a configuration.

Recommend instead, and say so in M3: **one VMM per node, nested per sandbox.**
That is what §7 actually requires, and it is satisfied entirely inside the Cloud
Hypervisor driver by `--cpus nested=on|off`. Per-sandbox *VMM* choice only
becomes necessary if we want Firecracker's smaller device model for non-nested
sandboxes on the same Node — an argument nobody has made yet, and one that §7's
dedicated-nested-node-pool recommendation already answers differently: two node
Pods, two VMMs, placement routes.

### 3.4 Rollout and rollback on the live CKS deployment

The target is the deployment in `docs/deploy-cks.md`: namespace `sparkbox-poc`,
NodePool `default-node-pool`, Node `g084f44`, `catnip.sh`.

**There is no canary in the blue/green sense, and §9 M2 should stop implying
one.** "Roll one Pod. Roll back is the env var" reads as a side-by-side.
`deployment.yaml` is `replicas: 1` with `strategy: Recreate`, pinned by
`kubernetes.io/hostname` to one Node, backed by a Node-local `hostPath`, and the
device plugin advertises a single `sparkbox.dev/kvm` per Node. The old node Pod
is terminated before the new one is scheduled, and a second one could not be
admitted even if it were requested. So M2 is a **maintenance window on one
Node**. What it honestly can be is a canary *in time and in fleet position* —
this Node runs Cloud Hypervisor while the DGX, the EM box and any future node
Pod stay on Firecracker — which is real, because §10.3's selector is per node
Pod. It cannot be a canary in *population*, because this cluster has one VM node.

Procedure, in the idiom of `deploy-cks.md`'s existing "Update an existing
deployment":

```sh
# 1. Quiesce. The node restart stops every running VM either way; pausing first
#    is how a user learns why, and it is the state this switch is about.
ssh -p 22 ctl@ssh.catnip.sh list
ssh -p 22 ctl@ssh.catnip.sh pause <name>

# 2. Roll. Same script, same digest resolution, one flag added.
deploy/kubernetes/deploy.sh \
  --image "$IMAGE" \
  --node-pool default-node-pool --node g084f44 \
  --public-key ~/.ssh/id_ed25519.pub --user vanpelt \
  --vmm cloud-hypervisor          # sets SPARKBOX_VMM on sparkbox-node and vmm-helper

# 3. Verify before letting anyone in.
kubectl -n sparkbox-poc exec deployment/sparkbox-node -c sparkbox-node -- \
  sparkbox doctor | grep -E 'vmm binary|nested virtualization'
kubectl -n sparkbox-poc logs deployment/sparkbox-node -c vmm-helper | head
```

`deploy.sh` rebuilds Pod templates from the manifests rather than from live
objects, so `--vmm` must be passed on every subsequent run — exactly like
`--proxy-domain`, and with a worse failure mode. Give it the same carry-forward
treatment the script already implements for `--proxy-domain` (reading the live
Deployment and printing `Keeping the deployed public domain`, l.300-313) and for
`--hivemind-api`. Without it, the next unrelated deploy silently reverts the
Node to Firecracker, and per the table below that is a fleet-wide loss of paused
memory state that nobody asked for.

**Rollback is one line and is not free.** Re-run the same command with
`--vmm firecracker` (or, once carry-forward exists, `--vmm firecracker`
explicitly rather than by omission). It is a second full `Recreate`: every
running VM stops again. One line, one maintenance window — do not describe it as
a hot switch.

Keep both binaries in `$asset_dir` on this Node: download
`cloud-hypervisor-$artifact_arch` unconditionally in `entrypoint.sh`, exactly as
`firecracker-$artifact_arch` is downloaded today, so a rollback is a Pod restart
rather than a Pod restart plus a download over a link that may be the thing that
is broken. Two VMM binaries against a hot tier sized for 25 GiB guest disks is
not a real cost. Make the *jailer* fetch conditional instead — it already is, on
`chroot_jailer`.

**What survives a VMM switch.**

| State | Written by | Survives FC ⇄ CH | Why |
| --- | --- | --- | --- |
| `fc-vms/<name>/rootfs.ext4` | either | **Yes, byte for byte** | Raw ext4 file on both sides. `image_type=raw` is a declaration, not a conversion (§6). |
| `templates/*.ext4` | `Snapshot` | **Yes** | `reflinkClone` + `e2fsck` + `zerofree`; none of it VMM-aware. |
| Checkpoints on VAST | `PackRootfs` | **Yes** | A zstd of the same ext4; `RestoreCheckpoint` writes a full fresh image. |
| `images/universal.ext4` | `prepare-vm-assets` | **Yes** | Guest-side; `CONFIG_KVM` in the kernel changes nothing in it. |
| `vmlinux-<arch>` | our release | **Yes** | §10.1 — the same image boots both. |
| `sandboxes.json` | controller | **Yes** | `State: paused` is preserved and is exactly what triggers the resume attempt. |
| `fc-vms/<name>/{mem,state}.snap` | Firecracker | **No — and it is already dead** | See below. |

**The paused sandbox, answered precisely — this is the part people get wrong.**

The instinct is that a sandbox paused under Firecracker will fail to resume, or
worse resume into a corrupt guest, when the Node comes back on Cloud Hypervisor.
Neither happens, and the reason is that **the memory snapshot does not survive a
node Pod restart at all, under any VMM.**

`Driver.Resume` (`fc.go:1051`) looks the VM up in `d.vms`, an in-memory map that
`New()` builds empty; nothing scans `fc-vms/` at startup. So after any restart
the first thing that happens is `Resume` returning `vm %q not found`.
`host.Manager.resumeOrRecreate` (`manager.go:1951`) then asks `RootfsPresent`,
finds the disk, logs `resume failed, recreating`, and calls `Create`, which sees
an existing `rootfs.ext4`, takes neither the reflink branch nor the `NewSandbox`
refusal, and **cold-boots the preserved disk**. That is today's behaviour on
every roll — it is what `deploy-cks.md`'s "Paused, checkpointed, and archived
sandboxes keep their disks on the Node-local hot tier and come back" is
describing. They come back cold, from disk, with their RAM gone.

The VMM switch therefore changes nothing about the outcome for a paused
sandbox: same cold boot, same intact disk, same lost RAM. It changes two smaller
things, and both want handling in M2:

1. **The stale files are never reclaimed.** Under Firecracker the orphaned
   `mem.snap`/`state.snap` are overwritten by the next `Pause` (`fc.go:997`
   renames the `.next` pair over them). Under Cloud Hypervisor `Pause` writes a
   snapshot *directory* and never touches them, so a full guest's RAM — 8 GiB
   for a default sandbox — sits on the node-local hot tier per
   previously-paused sandbox, forever, uncounted: `DiskUsageMB` and
   `pooledDiskMB` read the ext4 superblock and know nothing about these files.
   Fix: have the Cloud Hypervisor driver's `DropSnapshots` remove the
   Firecracker pair as well as its own artifacts — three extra
   `os.Remove` / `os.IsNotExist` calls — and sweep once per sandbox on the first
   roll. It is the cheapest possible migration and it makes the reverse roll
   clean too.
2. **Nothing on the resume path says which VMM wrote what.**
   `resumeOrRecreate` logs `resume failed, recreating` at Warn with the
   underlying error either way, so a cross-VMM snapshot is indistinguishable in
   the logs from genuine snapshot corruption. Since §10.3 needs a `VMM` field on
   `host.Sandbox` for the multiplexing case anyway, record it on every start
   even in the single-VMM design, and have the driver return a typed reason when
   the snapshot it finds belongs to the other VMM. One field, and it turns a
   confusing Warn into a fact.

The dangerous failure — one VMM interpreting the other's memory image — cannot
occur, and it is worth stating because it is the thing to be afraid of.
Firecracker's `Resume` stats `state.snap` by name; Cloud Hypervisor's restore
takes `source_url=file://…/snap` and reads `config.json` out of that directory.
The filenames do not overlap, so no code path exists on which either VMM parses
the other's bytes. The design rules it out by naming, not by checking.

**Two compatibility rules that are not about the switch but arrive with it.**

- **A Cloud Hypervisor version bump is a snapshot-format migration.** Upstream's
  stability guarantees say snapshot/restore is not supported across versions, and
  the only newer guarantee — two-version *migration* compatibility — starts at
  v54 and does not cover restore at the v53 this design proposes to pin. The
  serialized vCPU state also embeds an 8320-byte `KvmNestedStateBuffer` whose
  `#[repr(C)]` layout tracks the kvm-bindings crate version, for every x86_64
  sandbox and not only nested ones. Today this is masked by the empty-map
  behaviour above; if a future change adds snapshot adoption at startup, it
  becomes a real gate, and `cloud_hypervisor_sha256_*` changing in a roll must
  then force a cold boot rather than a restore.
- **`nested` is baked into the snapshot.** A restore takes `source_url`,
  `prefault`, `memory_restore_mode`, `resume`, `zone_updates` and `net_fds`, and
  nothing else; the whole `CpusConfig`, `nested` included, comes from
  `config.json`. So §7's node gate has to be re-evaluated on *resume*, and a
  nested sandbox that fails it must be cold-booted via `DropSnapshots` rather
  than restored. Again, today's restart behaviour does that by accident; a
  deliberate adoption path must do it on purpose.

**Gates before M3 flips the default.**

- `hack/check-cks-pin.sh` green with a real `SHA256_CLOUD_HYPERVISOR` row — the
  word `ok`, not `skip`.
- `sparkbox doctor` on `g084f44` printing `vmm binary  cloud-hypervisor vNN.N`
  and a `nested virtualization` line that agrees with
  `hack/probe-nested-virt.sh` run in the same container.
- **Non-empty `console.log` from a cold boot on both arches.** On arm64 this is
  an acceptance criterion, not a nicety: a guest with no PL011 driver boots and
  SSHes normally and produces zero console output, so an SSH-based acceptance
  test would hide the defect until the first boot that fails, which is the boot
  you most need the log for.
- One roll forward and one roll back on the live Node with at least one paused
  sandbox present, confirming: disk intact, cold boot, no orphaned `mem.snap`
  left behind, and `--vmm` carried forward by a `deploy.sh` run that omits it.

### Open questions from this section

- Does the Cloud Hypervisor release carry a GitHub provenance attestation or any signature we can verify in CI? `release.yaml` uploads no checksum or signature file, and the release asset listing would not render for me. If an attestation exists, `gh attestation verify` belongs in the download step; if not, the pin only attests to what our CI downloaded.
- What is the actual size and boot-time delta of `CONFIG_VIRTUALIZATION=y CONFIG_KVM=y CONFIG_KVM_INTEL=y CONFIG_KVM_AMD=y` on the x86_64 guest kernel? Unmeasured. It is the only quantitative input to the one-kernel-versus-two decision, and the release step summary already prints the number for free.
- On arm64, does `CONFIG_KVM=y` in a Cloud Hypervisor guest ever initialise? I claim it does not (KVM/arm64 needs the kernel to have booted at EL2, which CH's arm64 guest does not), so it would be dead weight and the fragment should stay per-arch — but this is reasoned, not tested. One arm64 boot with the shared fragment settles it.
- Which stable kernel series is `g084f44` on, and does CoreWeave's patch cadence reach the §3.2 CVE floor? `checkNested`'s fail branch is written against that floor, and on most cloud kernels the probe's honest answer is "cannot map this version to an upstream series" rather than pass or fail. The design has no rule for that outcome beyond asking CoreWeave.
- Will `deploy.sh` carry `--vmm` forward from the live Deployment the way it already carries `--proxy-domain` and `--hivemind-api`? If not, the first unrelated re-run silently reverts the Node to Firecracker and every paused sandbox loses its memory state. This is a decision about `deploy.sh`, not about the VMM.
- Is Firecracker's guest-kernel CI config (`BASE_CONFIG_URL` points at `main`, not a tag) an acceptable moving input now that `CONFIG_PVH` is a hard boot dependency under Cloud Hypervisor rather than a convenience? The proposed verify-loop rows turn an upstream change into a build failure, but pinning the base config to a Firecracker tag is the stronger answer and is a separate decision.
