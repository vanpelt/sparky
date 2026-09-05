//go:build linux

// Package qemu implements vmm.Driver on QEMU/KVM microVMs driven over QMP.
//
// This is the SECOND implementation of vmm.Driver, and that is its point.
// internal/vmm/driver.go plus its ten optional capability interfaces were
// written against one engine; until a second one satisfies them unmodified,
// "abstraction" is a claim rather than a fact. QEMU was chosen over Cloud
// Hypervisor precisely because it disagrees with Firecracker on every axis the
// interface touches — QMP instead of REST, one migration file instead of a
// mem/state pair, a balloon whose units run the other way — so it exercises the
// seams the parity harness exists to find. See docs/vmm-choice.md for the
// decision and docs/vmm-parity-harness.md for the harness.
//
// Every QEMU behaviour this package relies on was MEASURED by the hand-driven
// spike in docs/qemu-spike.md on the arm64 dev box against the guest kernel and
// rootfs template we actually ship: the boot argv, the four kernel-command-line
// differences from Firecracker, the stop/migrate/query-migrate/quit snapshot
// sequence, the byte-identical restore argv plus -incoming, and the balloon
// stats that Cloud Hypervisor could not provide from any released version.
// hack/qemu-spike/ holds the scripts. Do not re-derive those facts; where this
// package goes beyond them the comment says so.
//
// Requires QEMU 8.2 or newer: the `file:` migration URI Pause depends on landed
// in 8.2, and Debian 12's 7.2 does not have it.
package qemu

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/guestnet"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
)

// ---------------------------------------------------------------------------
// The contract for the rest of this package.
//
// This file owns the shared surface: Options, Driver, vmState, the constants
// below, New and Close. Every other method lives in one of five sibling files,
// and the split is exclusive — if a helper is listed under a file, only that
// file declares it. Two files declaring the same helper is a compile error, and
// two files disagreeing about a path or a unit is a hardware-only bug.
//
// FILE OWNERSHIP
//
//	qmp.go        the QMP client (type qmpConn) and nothing else.
//	args.go       the QEMU argv and the guest kernel command line. Declares:
//	              (d *Driver) qemuArgs(...), (d *Driver) bootCmdline(...)
//	              <- boot calls this to build-or-replay -append,
//	              (d *Driver) kernelArgs(name string,
//	              idx int, fresh bool) (string, error), machineIDFor(name string)
//	              string, macFor(idx int) string, guestDNSArg(guestDNS,
//	              gatewayIP string) (string, error), validateGuestDNS(guestDNS
//	              string) error  <- New calls this one.
//	lifecycle.go  Create, Pause, Resume, Destroy, plus everything they need:
//	              boot, stopVMM, freeSlot, reserveName/releaseName,
//	              createTap/deleteTap/tapName/sweepStaleTaps  <- New calls
//	              sweepStaleTaps, defaultRoute6Dev  <- New calls this too,
//	              hostIP/guestIP/guestSlot/hostIP6/guestIP6/addr6, instance,
//	              and ALL per-VM path helpers: vmDir, rootfsPath, qmpSocketPath,
//	              vmmLogPath, serialLogPath, snapshotPath, snapshotNextPath,
//	              snapshotFiles, hasSnapshot, removeSnapshotFiles.
//	caps_files.go Archivable, DiskReporter, TemplateReporter, RootfsPresencer,
//	              Renamer, Rebooter, DiskResizer — the pure host-side half,
//	              which lifts
//	              almost verbatim from internal/vmm/firecracker/fc.go. Declares
//	              the helpers that half needs: stoppedRootfs, captureDir,
//	              templatePath, reflinkClone, compact, ext4DiskMB, imageNameRe,
//	              installAuthorizedKey and its writeAuthorizedKey /
//	              mergeAuthorizedKeys / sameAuthorizedKey / ensureGuestDir /
//	              rootfsLoginIdentity family, and sanitizeTemplate.
//	caps_vmm.go   CPUStatser, NetStatser, Ballooner. Declares procStatCPUTicks.
//
// THE SIGNATURES MORE THAN ONE FILE DEPENDS ON. These are fixed; a file may add
// unexported internals beside them but may not change these.
//
//	// qmp.go. One goroutine owns the decoder for the connection's whole life
//	// and always drains, because events (BALLOON_CHANGE at the stats polling
//	// rate, STOP/RESUME/MIGRATION/SHUTDOWN) arrive unsolicited and a chardev
//	// whose reader stalls eventually stalls the monitor. The event sink must
//	// never block that goroutine and must never issue a command from inside it.
//	// The reply/event discriminator is the presence of the "event" key, and the
//	// unsolicited greeting must be consumed BEFORE qmp_capabilities or every
//	// reply is off by one forever.
//	type qmpConn struct{ ... }
//	func dialQMP(ctx context.Context, sockPath string) (*qmpConn, error) // connect + greeting + qmp_capabilities
//	func (c *qmpConn) Close() error
//	func (c *qmpConn) Execute(ctx context.Context, cmd string, args, out any) error
//	func (c *qmpConn) Stop(ctx context.Context) error
//	func (c *qmpConn) Cont(ctx context.Context) error
//	func (c *qmpConn) Quit(ctx context.Context) error                  // tolerates EOF instead of a reply
//	func (c *qmpConn) QueryStatus(ctx context.Context) (string, error) // the RunState string
//	func (c *qmpConn) AwaitRunnable(ctx context.Context) error         // polls until the runstate leaves "inmigrate"
//	func (c *qmpConn) MigrateToFile(ctx context.Context, path string) error // returns as soon as QEMU accepts it
//	func (c *qmpConn) AwaitMigration(ctx context.Context) error        // polls query-migrate; absent status means "none"
//	func (c *qmpConn) SetBalloonBytes(ctx context.Context, b uint64) error
//	func (c *qmpConn) BalloonActualBytes(ctx context.Context) (uint64, error)
//	func (c *qmpConn) EnableBalloonStats(ctx context.Context, intervalSecs int) error
//	func (c *qmpConn) GuestStats(ctx context.Context) (free, available uint64, err error) // error when there is no sample
//
//	// lifecycle.go
//	func (d *Driver) boot(ctx context.Context, name string, st *vmState, rootfs string, restore bool, fresh bool) error
//	func (d *Driver) stopVMM(st *vmState) error // nil once the child is reaped, however that happened
//	func (d *Driver) instance(name string, st *vmState) *vmm.Instance
//	func (d *Driver) freeSlot() (int, error)    // caller holds d.mu
//	func tapName(idx int) string                // tapPrefix + strconv.Itoa(idx)
//	func sweepStaleTaps()
//	func defaultRoute6Dev() string
//
// boot takes the machine size from st (st.vcpus, st.memMB) rather than
// arguments, so the restore path cannot accidentally pass the zeros
// fc.go:1073 passes. stopVMM is the only thing that ends a QEMU process: it
// sends quit, waits on st.exited, then escalates SIGTERM and SIGKILL, and
// returns non-nil ONLY if the child could not be reaped — which means the
// rootfs may still be open, so Pause and Destroy must propagate it rather than
// rename or remove anyway.
//
// fc.go's unexported helpers cannot be imported, so they are copied. Copy them
// with their comments: those comments record measurements and fixed bugs (why
// reflinkClone trims cp's trailing newline, why ensureGuestDir re-chmods ~/.ssh
// alone, why templatePath resolves ImageDir first), and a copy stripped of them
// is the version somebody "simplifies" back into the bug.
//
// MUTEX DISCIPLINE — identical to fc.go, deliberately.
//
// d.mu is one driver-wide mutex. Hold it for the whole of Pause, Resume,
// Destroy, Close, SetBalloonTarget, BalloonStats, CPUTimeNanos, NetBytes,
// DropSnapshots and RenameVM. Hold it for the SECOND half of Create only —
// freeSlot, createTap, boot — never across the rootfs clone or the loop mount;
// d.creating (reserveName/releaseName) is what keeps the name refused while
// d.mu is down, and Create must re-check d.vms after reacquiring because
// reserveName only excludes another Create. Take-and-release it for the running
// check in stoppedRootfs and UnpackRootfs. Do not take it at all in
// RootfsPresent, DiskUsageMB, DiskCapacityMB or TemplateUsageMB.
//
// A name is in exactly one of d.creating and d.vms. Every path that drops a
// record (Destroy, DropSnapshots, RenameVM) releases its slot back to freeSlot,
// and must do so only after the tap is gone.
//
// PackRootfs, Snapshot and ResizeDisk run multi-second e2fsck/zerofree/zstd
// work with NO lock held. That TOCTOU is deliberate; holding d.mu through an
// archive serializes the whole fleet.
//
// ON-DISK LAYOUT
//
//	<VMStateDir>/qemu-vms/<name>/rootfs.ext4       the sandbox's disk
//	<VMStateDir>/qemu-vms/<name>/state.migrate     the memory snapshot
//	<VMStateDir>/qemu-vms/<name>/state.migrate.next  transient, during Pause
//	<VMStateDir>/qemu-vms/<name>/qmp.sock          the monitor socket
//	<VMStateDir>/qemu-vms/<name>/serial.log        -serial file: target
//	<VMStateDir>/qemu-vms/<name>/qemu.log          the child's stdout+stderr
//	<VMStateDir>/qemu-vms/<name>.pack.ext4.zst     PackRootfs output, a SIBLING
//
// qemu-vms/, not fc-vms/: the two drivers must be able to share a VMStateDir
// without either one's Destroy reaching the other's disks.
//
// The pack artifact is a sibling of the VM dir because Destroy is
// os.RemoveAll(vmDir) and the manager uploads the artifact after the destroy.
//
// serial.log captured 0 bytes on every spike boot — our arm64 guest kernel has
// no PL011 driver — so qemu.log is the only boot diagnostic there is. A QEMU
// that rejects its own argv (a device the incoming stream does not match, a
// property the packaged build lacks) prints to stderr and exits nonzero, which
// is loud but only after exec: boot MUST capture the child's stdout and stderr
// into qemu.log with O_TRUNC per launch, and MUST include its tail in the error
// it returns when the process exits early. Without that a failed start is a
// bare exit status.
//
// THE SNAPSHOT IS ONE FILE, AND FIVE PLACES IN fc.go ASSUME TWO
//
// Firecracker writes mem.snap + state.snap and stats that literal pair in
// Pause, Resume, PackRootfs, DropSnapshots and RenameVM's refusal predicate.
// QEMU's migrate writes one state.migrate. Lifted unchanged, RenameVM's
// os.Stat matches nothing and it silently stops refusing the renames it exists
// to refuse, while PackRootfs and DropSnapshots leave the snapshot in place.
// So NO file outside lifecycle.go names a snapshot path: lifecycle.go declares
//
//	func (d *Driver) snapshotPath(name string) string      // .../state.migrate
//	func (d *Driver) snapshotNextPath(name string) string  // ....next
//	func (d *Driver) snapshotFiles(name string) []string   // every file that IS the snapshot
//	func (d *Driver) hasSnapshot(name string) (bool, error)
//	func (d *Driver) removeSnapshotFiles(name string) error // those plus the .next
//
// and PackRootfs, DropSnapshots and RenameVM go through them. snapshotFiles
// returns a slice today holding one path; keeping the plural is what makes a
// future second file (a UFFD side-car, a config dump) a one-line change instead
// of a fourth silent regression.
//
// Pause writes to state.migrate.next and promotes it with a single rename(2)
// only after query-migrate reports completed AND the process has been reaped.
// Unlike Firecracker's unjailed path, which snapshots straight onto the live
// file, this means a torn migration can never leave a truncated state.migrate
// that Resume would happily try to load. The rename is within one directory, so
// it is atomic and free.
//
// THE VMM SEQUENCES, from docs/qemu-spike.md
//
// Cold boot: exec the argv, wait for qmp.sock, dial, qmp_capabilities, then
// poll for SSH. Restore: THE SAME ARGV with `-incoming file:<state.migrate>`
// appended, then poll query-status until the runstate leaves "inmigrate", then
// `cont`. args.go therefore builds one argv and appends one flag — the identity
// requirement is structural, not a review convention, because QEMU matches the
// migration stream's device sections against argv by name and PCI address.
//
// "The same argv" includes -append, and only because boot works to keep it
// that way: it records the cold boot's guest command line on vmState and
// bootCmdline replays it on the restore. Rebuilt instead, the line of a
// sandbox that cold-booted fresh would come back one token short
// (sparkbox_fresh=1 is cold-boot-only), which is a difference no run has ever
// taken. It is inert for the guest either way — -kernel/-append only seed guest
// RAM at reset and the incoming stream overwrites that RAM, so a restored guest
// reads its FIRST boot's cmdline out of its own memory — but "inert as far as
// we know" is not the standard the rest of this argv is held to.
//
// Pause: stop -> migrate uri=file:<...next> -> poll query-migrate to completed
// -> quit -> WAIT for the process to exit -> promote the file -> deleteTap.
// migrate returns {} immediately and is asynchronous; treating it as
// synchronous truncates the snapshot. The rootfs stays open until the process
// is reaped, so nothing may rename, pack or resize before the wait returns. On
// migration failure: `migrate_cancel`, wait for query-migrate to settle, remove
// the partial file, THEN `cont` — all on contexts derived with
// context.WithoutCancel, because when the failure IS the caller's ctx expiring,
// recovering on that same dead ctx leaves the vCPUs stopped and the sandbox
// unresumable. The cancel has to come first: a deadline reached while the
// stream is still `active` leaves QEMU writing, so unlinking strands a
// full-size image in a pathless inode and `cont` restarts vCPUs that the
// completing migration then re-stops in `postmigrate`.
//
// If the promotion rename fails after the VMM is reaped, Pause tears the tap
// down, removes the snapshot (a STALE one from an earlier pause is worse than
// none: restoring hour-old memory over a rootfs that has moved on is
// corruption, not staleness) and DROPS THE RECORD, which is the shape
// resumeOrRecreate cold-boots from. fc.go:1000 sets paused=true instead; the
// difference is deliberate and DropSnapshots' comment explains why a paused
// record with no snapshot behind it is a trap.
//
// The tap teardown in Pause is mandatory. Leaving it wedges Resume with
// "tuntap add: Device or resource busy".
//
// THE ARGV, from hack/qemu-spike/probe.sh with per-VM substitutions
//
//	-M <opts.MachineType> -cpu host -enable-kvm -m <st.memMB> -smp <st.vcpus>
//	-kernel <opts.KernelPath> -append "<kernelArgs>"
//	-drive file=<rootfs>,format=raw,if=none,id=rootfs
//	-device virtio-blk-pci,drive=rootfs,romfile=
//	-netdev tap,id=net0,ifname=<tapName(idx)>,script=no,downscript=no
//	-device virtio-net-pci,netdev=net0,mac=<macFor(idx)>,romfile=
//	-device virtio-balloon-pci,id=balloon0,deflate-on-oom=on,romfile=
//	-qmp unix:<qmp.sock>,server=on,wait=off
//	-nographic -serial file:<serial.log> -monitor none
//	[restore only, appended last] -incoming file:<state.migrate>
//
// romfile= is not optional on the devices that carry it: the Ubuntu package
// ships no option ROMs and virtio-net-pci without it fails outright with
// `failed to find romfile "efi-virtio.rom"`. We boot with -kernel, so the PXE
// ROM is dead weight regardless.
//
// It is on ALL THREE PCI devices, which is docs/qemu-spike.md's stated finding
// ("every PCI device needs romfile=") rather than the line that actually booted
// — hack/qemu-spike/probe.sh carries the token on virtio-net-pci and
// virtio-balloon-pci only. The generalisation is followed here because romfile
// is a property of PCIDevice itself: it is accepted on any PCI device and does
// nothing where the build ships no default ROM for it, so matching the doc
// costs one token and covers the packaged QEMU whose virtio-blk-pci does have
// one. That failure would be a hard exec-time abort with only qemu.log behind
// it, on a build nobody has run this on yet.
//
// Two tokens are not in probe.sh. `mac=` pins the NIC's address to the slot the
// way fc.go's macFor does, so a restored guest lands on an identically
// configured interface. `deflate-on-oom=on` matches the `true` Firecracker
// passes to NewCreateBalloonHandler and is what stops an aggressive reaper
// OOM-killing a guest that needs its pages back; it is the one argv token no
// spike run exercised, so if the first hardware run dies with a
// "Property ... not found" message from QEMU, that is the token.
//
// The QMP socket must be unlinked before exec — QEMU does not clear a stale
// socket file and the bind fails.
//
// Guest kernel command line: fc.go's string with the four measured differences
// — drop pci=off (virtio is on PCIe under -M virt, and left in the guest comes
// up with no disk and no NIC), add root=/dev/vda rw (QEMU does not synthesise
// it from a drive flag the way Firecracker does), console=ttyAMA0 rather than
// ttyS0, and everything else unchanged.
//
// THE BALLOON RUNS THE OTHER WAY ROUND
//
// vmm.Ballooner speaks in RAM RECLAIMED FROM the guest, MiB. QEMU speaks in the
// guest's REMAINING VISIBLE RAM, bytes. They are inverses:
//
//	SetBalloonTarget(name, t)  ->  balloon value = (st.memMB - clamp(t,0,st.memMB-1)) << 20
//	                                               (memMB-1, because QEMU rejects
//	                                                a balloon target of zero)
//	BalloonStats.ActualMiB     =   st.memMB - actual>>20   (actual from query-balloon)
//	BalloonStats.TargetMiB     =   ActualMiB               (fc.go:1119 does the same;
//	                                                        QEMU exposes no target either)
//	BalloonStats.FreeMiB       =   stat-free-memory      >> 20
//	BalloonStats.AvailableMiB  =   stat-available-memory >> 20
//
// The baseline is st.memMB, the configured -m value, NEVER the guest's
// stat-total-memory: the spike measured 1034022912 (986 MiB) reported by a
// 1024 MiB guest, because MemTotal is net of kernel reservations. Deriving from
// it skews every reading by ~38 MiB and never reaches the exact 0 the parity
// suite demands after a full deflate. Forwarding targetMiB straight through
// instead would set an 8 GiB guest's RAM to 256 MiB, and nothing on the host
// side errors — the guest just starts OOM-killing.
//
// Guest stats need `qom-set guest-stats-polling-interval` and one interval to
// elapse before qom-get returns a sample. That property is NOT part of
// virtio-balloon's migrated device state, so boot must re-issue it after EVERY
// successful start, restore included, or BalloonStats goes stale on a resumed
// sandbox. (Inferred, not measured — it is the likeliest first parity failure.)
// Which is why the qom-set is fatal on a cold boot and NOT on a restore: a
// restored guest whose memory has already loaded must not lose it to a stats
// property nobody has yet watched behave. boot records the failure on
// st.statsErr and BalloonStats reports "no sample" — the degradation
// host.Manager.MemStats already handles by keeping its last reading.
// Unset stat fields come back as uint64 0xFFFFFFFFFFFFFFFF, so decode them as
// uint64 and return an ERROR for "no sample yet" rather than zeros: zeroed
// stats make host.Manager.MemStats charge every sandbox its full ceiling and
// balloon innocent ones.
//
// Unlike Firecracker, whose balloon is attached only on a cold boot because the
// snapshot restores it, QEMU's balloon is an argv device and must appear —
// identically, same id, same position — on the restore line too.
//
// WHAT MUST NOT BE LIFTED FROM fc.go BY REFLEX
//
//   - Resume must pass st.vcpus and st.memMB, not the literal 0 fc.go:1073
//     passes. Firecracker reads the machine config out of state.snap; QEMU
//     takes -smp and -m from argv on the restore line and the values must match
//     the source exactly. That is why vmState carries them.
//   - exec.CommandContext(callerCtx, ...) is the same bug fc.go:799's vmCtx
//     exists to prevent, wearing different clothes. The caller's ctx is the
//     create-on-connect SSH session; bind the child to it and the microVM dies
//     the instant that first connection closes. QEMU has no ctx-driven kill at
//     all, so the child must be built with exec.Command and killed only by
//     stopVMM.
//   - HostIP in vmm.Instance is the GUEST's address (fc.go:2059 returns
//     d.guestIP(idx)) — "the address reachable from the host for the VM's own
//     services". Returning d.hostIP points the whole HTTP proxy at the wrong
//     end of every /30.
//   - Create's `fresh` is true ONLY when the rootfs did not exist and was just
//     reflink-cloned. It becomes sparkbox_fresh=1, the guest's permission to
//     move somebody's git checkout onto the manifest branch. Create also runs
//     for cold boots of existing sandboxes, so "no driver record" is not fresh.
//   - Destroy's no-record branch returns os.RemoveAll(vmDir), not nil. d.vms is
//     per-process and nothing rehydrates it, so after a restart every sandbox
//     takes that path; the early return leaked the disk and left it for Create
//     to adopt under a reused name. Destroy of a name with no record and no
//     disk must return nil — every parity box registers a teardown Destroy.
//   - DisableHostRootfsMounts gates exactly two call sites, installAuthorizedKey
//     in Create and sanitizeTemplate in Snapshot, and CKS depends on both.
//   - The e2fsck exit-code thresholds differ on purpose: fatal at >2 in
//     ResizeDisk, fatal at >=4 in compact. Copy each where it belongs.
//
// PAUSED-STATE POLARITY, which the parity suite checks in both directions:
// CPUTimeNanos, NetBytes, SetBalloonTarget and BalloonStats must ERROR on a
// paused sandbox; DiskUsageMB and DiskCapacityMB must NOT. Pause on an
// already-paused sandbox must return nil.
//
// st.cmd != nil is the single liveness predicate, replacing fc.go's
// st.machine != nil. stopVMM clears st.cmd and st.qmp once the child is reaped.
// ---------------------------------------------------------------------------

// Options configures the driver. It mirrors firecracker.Options wherever the
// meaning is the same, so cmd/sparkbox can pass one flat argument list to
// either constructor.
//
// The jailer, chroot-jailer, privileged-helper and jailer-uid fields have no
// counterpart here YET. That is a scope decision, not a verdict on whether it
// can be done.
//
// An earlier version of this comment said it could not: that Firecracker is a
// static binary whose chroot holds four files, while the packaged QEMU is
// dynamically linked against a library closure that would all have to be staged
// per slot. THAT WAS WRONG, and it was measured wrong on the production x86_64
// CKS node, inside a byte-for-byte copy of the vmm-helper container's
// securityContext. QEMU jails ITSELF, after machine init, once every library
// and firmware blob is already open — nothing needs staging:
//
//	-runas <uid>:<uid> -run-with chroot=<jailRoot> -sandbox on
//
// gave a process with Uid and Gid 100000 in all four slots, CapEff and CapPrm
// both 0000000000000000, NoNewPrivs 1 and Seccomp 2. That is strictly MORE
// dropped than the firecracker chroot launcher achieves, which keeps the slot
// uid's default capability set. Containment was proven rather than assumed: a
// migrate to an absolute path outside the jail failed with ENOENT.
//
// Two things that design must respect, both measured:
//   - The runtime `migrate uri=file:` resolves POST-chroot. The target must be
//     pre-created and pre-chowned to the slot uid and hardlinked into the jail,
//     which is what the helper's prepareSnapshotOutputs already does for
//     Firecracker. Without it the migration fails "Permission denied" and Pause
//     silently has no snapshot.
//   - /dev/kvm cannot be passed by a hostPath bind mount under that
//     securityContext; the device cgroup denies it even to uid 0 holding
//     DAC_OVERRIDE. Only the device plugin's sparkbox.dev/kvm allocation opens
//     it, and vmm-helper holds it. So New's /dev/kvm stat below must gain the
//     `if PrivilegedHelperSocket == ""` guard fc.go:303 has, or this driver
//     cannot start in the container that would run it.
//
// Until that lands this driver is the direct launcher only, which is what
// hack/parity/run-on-mac.sh and hack/parity/run-on-cks.sh exercise.
type Options struct {
	// KernelPath is an uncompressed vmlinux. Note that the arm64 guest kernel
	// we ship has no PL011 driver, so -serial captures nothing on -M virt until
	// CONFIG_SERIAL_AMBA_PL011 lands; see docs/qemu-spike.md.
	KernelPath string
	// ImageDir holds <image>.ext4 rootfs templates.
	ImageDir string
	// TemplateDir is where CAPTURED templates are written, and it exists
	// because on a hardened node ImageDir is mounted read-only to this process:
	// base images are laid down by a privileged one-shot that exits before any
	// guest runs, so a compromised controller cannot rewrite the rootfs every
	// future sandbox boots from. Captures go here instead. Empty means "the
	// same place as ImageDir", which is every single-machine host.
	TemplateDir string
	// VMStateDir holds per-VM dirs. It must be on a filesystem supporting
	// reflink copies (XFS, btrfs): Create refuses to fall back to a full 25 GiB
	// copy, and the measured cost of the fallback was a 2.3s boot becoming >45s.
	VMStateDir string
	// QemuBin is the system emulator binary (default: qemu-system-<arch> on
	// PATH, resolved to an absolute path once here so a later PATH change
	// cannot swap the binary under a running fleet).
	QemuBin string
	// MachineType is the -M argument, and it MUST be a versioned name.
	//
	// The migration stream is keyed on the machine model, and bare "virt" is an
	// alias for the newest versioned machine the binary knows. Leave it
	// unpinned and a QEMU package upgrade silently changes the model, at which
	// point every paused sandbox's state.migrate becomes unloadable. Versioned
	// types are retained across releases, so a pinned snapshot survives.
	//
	// Empty defaults to virt-8.2 on arm64, the pairing docs/qemu-spike.md
	// measured. There is no default on other architectures: nothing here has
	// ever been run on x86_64 and guessing a machine model that determines
	// snapshot compatibility is not the place to be optimistic.
	MachineType string
	// PrivilegedHelperSocket moves VM launch, tap creation and jail construction
	// into the root-owned helper, exactly as it does for the firecracker driver.
	// Empty is the DIRECT LAUNCHER: this process execs QEMU itself, which is
	// what a dev box and the parity suite use.
	//
	// It changes what this driver may assume about its own container. On CKS the
	// controller runs with every capability dropped and NO device node at all —
	// the device plugin's sparkbox.dev/kvm allocation belongs to the helper — so
	// the /dev/kvm probe in New and the stale-tap sweep are BOTH conditional on
	// this being empty, mirroring fc.go:303 and fc.go:314. Measured on the CKS
	// node: a hostPath bind mount of /dev/kvm is not enough, the device cgroup
	// refuses it even to uid 0 holding DAC_OVERRIDE, so a driver that probes
	// unconditionally cannot start in the container that would run it.
	PrivilegedHelperSocket string
	// PrivilegedHelperBin is the launch client the driver execs; it holds no
	// privilege of its own and only keeps an authenticated connection open for
	// the VMM's lifetime.
	PrivilegedHelperBin string
	// HelperControllerGID is the group the helper shares VM files and the QMP
	// socket with, so this unprivileged process can reach them.
	HelperControllerGID int
	// DisableHostRootfsMounts skips per-create key injection and the template
	// sanitize pass — the two runtime paths that otherwise loop-mount a
	// guest-authored ext4 in the management process. Templates must then
	// already carry the current gateway key. This is how CKS captures
	// snapshots at all.
	DisableHostRootfsMounts bool
	// Subnet is an IPv4 CIDR carved into per-VM /30 slots. Empty uses
	// guestnet.DefaultPrefix.
	Subnet string
	// Subnet6 is a routable IPv6 /64 delegated to the host. When set, each VM
	// gets a globally-routable /127 from it (dual-stack, no NAT).
	Subnet6 string
	// LoginUser is the guest account the gateway SSHes in as — set to match the
	// template's baked authorized_keys. Empty defaults to root.
	LoginUser string
	// GuestDNS points guests at a specific resolver via the sparkbox_dns kernel
	// arg. The literal "gateway" expands per-VM to the guest's own host-side
	// address, where the sluice allowlist resolver listens; any other value is
	// used verbatim. Empty leaves guests on public resolvers.
	GuestDNS string
}

const (
	// vmsSubdir keeps this driver's per-VM directories out of the firecracker
	// driver's fc-vms/, so the two can share a VMStateDir.
	vmsSubdir  = "qemu-vms"
	rootfsName = "rootfs.ext4"
	// snapshotName is the whole memory snapshot: `migrate uri=file:` produces
	// exactly one file, where Firecracker produces mem.snap + state.snap.
	snapshotName = "state.migrate"
	// nextSuffix marks the snapshot Pause is still writing. It is promoted onto
	// snapshotName by a single rename(2) after the VMM has exited, so a torn
	// migration cannot leave a truncated file that Resume would try to load.
	nextSuffix    = ".next"
	qmpSocketName = "qmp.sock"
	serialLogName = "serial.log"
	// vmmLogName holds the child's stdout and stderr. With no PL011 driver in
	// the guest kernel, this is the only diagnostic a failed boot produces.
	vmmLogName = "qemu.log"
	// packSuffix is appended to the VM DIRECTORY path, making the artifact a
	// sibling of the directory Destroy removes.
	packSuffix = ".pack.ext4.zst"
	// loginUserSuffix names the sidecar hack/build-rootfs.sh writes beside a
	// template, telling the gateway which account to log a fork in as.
	loginUserSuffix = ".login-user"

	// tapPrefix deliberately differs from the firecracker driver's "sbtap".
	// Each driver's New() sweeps stale devices carrying its own prefix, so a
	// shared prefix would mean constructing one driver silently deletes the
	// other's live networking. Neither prefix is a prefix of the other, so the
	// two sweeps cannot overlap. (The spike's throwaway probe used "sbtapq0",
	// which fc's sweep would have eaten — that is how the hazard was noticed.)
	//
	// UNIFYING THIS IS PLANNED, AND IT BELONGS WITH THE PRIVILEGED HELPER, NOT
	// BEFORE IT. The payoff is entirely on that path: internal/netpush,
	// internal/hostsetup's sluice --tap-prefix, internal/vmhelper's own tapName
	// and deploy/sparkbox-net.sh all hardcode "sbtap", and under the helper the
	// tap is created by the helper anyway — so a QEMU node running there would
	// have the driver's name for the device disagree with the device that
	// exists. That is the change to make, once the driver has a helper path.
	//
	// It cannot be made now, and vmm.ClaimStateDir is not enough to make it
	// safe. The claim stops a second driver from being CONSTRUCTED against a
	// state directory the first one owns, which covers every deployment. It does
	// not cover the parity suite: each fixture builds its own MkdirTemp state
	// dir, so both drivers claim cleanly, while sweepStaleTaps is host-global.
	// `go test ./...` on a gated Linux host runs the two packages concurrently,
	// and a shared prefix would have each driver's New() delete the other's live
	// taps mid-run. When the helper path lands, this constant follows the
	// helper's name and the sweep gets the same `PrivilegedHelperSocket == ""`
	// guard fc.go:314 already has — which is what makes it a non-issue there,
	// because the helper path does not sweep at all.
	tapPrefix = "sbqtap"

	// balloonDeviceID must appear as `id=` on the -device line: the QOM path
	// guest stats are read from is derived from it, and an unnamed device lands
	// under /machine/peripheral-anon/device[N] with an index that shifts
	// whenever another device is added.
	balloonDeviceID = "balloon0"
	balloonQOMPath  = "/machine/peripheral/" + balloonDeviceID
	// balloonStatsIntervalSecs is how often the guest refreshes balloon stats.
	// It is a QOM property set at runtime, not migrated device state, so boot
	// re-issues it after a restore as well as a cold boot.
	balloonStatsIntervalSecs = 1
)

// vmState is the driver's record of one sandbox. It survives a pause: the slot
// (and therefore the tap name, the addresses, the MAC) stays reserved, which is
// what makes freeSlot's reuse of a released index safe.
type vmState struct {
	// idx is the /30 network slot; host is +1 and guest is +2.
	idx int
	// cmd is the QEMU child, and cmd != nil is this package's single liveness
	// predicate. stopVMM nils it once the process has been reaped.
	cmd *exec.Cmd
	// qmp is the monitor connection, nil whenever cmd is.
	qmp *qmpConn
	// exited is closed by the supervisor goroutine boot starts, after cmd.Wait
	// returns. Until then the child still holds the rootfs and the migration
	// file open, so nothing may rename, pack or resize them.
	exited chan struct{}
	// waitErr is the child's exit status. Valid only once exited is closed.
	waitErr error
	// vcpus and memMB are the configured machine size. They are kept because
	// QEMU's restore argv must match the boot argv exactly — unlike
	// Firecracker, which recovers both from the snapshot — and because memMB is
	// the baseline the balloon's inverted units are computed against.
	vcpus int64
	memMB int64
	// cmdline is the -append this VM last booted with, recorded by boot once the
	// guest was actually running. A restore replays it verbatim, for the same
	// reason vcpus and memMB are kept: the restore argv must repeat the boot
	// argv, and -append is part of the argv. See bootCmdline.
	cmdline string
	// statsErr records a balloon that came back from a restore without its
	// guest-stats polling property. It is not fatal there — the guest's memory
	// has already loaded — but BalloonStats must report it rather than serve the
	// pre-pause sample qom-get would keep returning. Cleared by every boot.
	statsErr error
	// paused means a memory snapshot exists and Resume can bring it back.
	paused bool
}

type Driver struct {
	mu   sync.Mutex
	opts Options
	vms  map[string]*vmState
	// creating holds the names of Creates that have released d.mu to do their
	// rootfs disk work. A name is in exactly one of creating and vms.
	creating map[string]bool
	guestNet guestnet.Network
	// reservedSlots holds /30s occupied by a host service address, such as a
	// dedicated in-prefix sluice DNS listener. They count toward prefix
	// capacity but are never handed to a VM.
	reservedSlots map[int]bool
	prefix6       net.IP // parsed /64 network address; nil disables IPv6
	uplink6       string // iface backing the v6 default route, for per-guest proxy NDP
}

// Compile-time capability checks: every optional interface in vmm, not the four
// somebody happened to write down. host.Manager reaches each of these by type
// assertion and falls back silently when the assertion fails, so a capability
// lost to a refactor — a receiver changed from pointer to value, a method
// renamed, a signature drifting — degrades the fleet with no error anywhere.
// This block is the only thing that turns that into a build failure, and for a
// port it is also the checklist: eleven assertions, twenty methods, and the
// parity suite skips silently past whichever ones are missing.
var (
	_ vmm.Driver           = (*Driver)(nil)
	_ vmm.Archivable       = (*Driver)(nil)
	_ vmm.DiskReporter     = (*Driver)(nil)
	_ vmm.TemplateReporter = (*Driver)(nil)
	_ vmm.RootfsPresencer  = (*Driver)(nil)
	_ vmm.Renamer          = (*Driver)(nil)
	_ vmm.Rebooter         = (*Driver)(nil)
	_ vmm.CPUStatser       = (*Driver)(nil)
	_ vmm.NetStatser       = (*Driver)(nil)
	_ vmm.DiskResizer      = (*Driver)(nil)
	_ vmm.Ballooner        = (*Driver)(nil)
)

func New(opts Options) (*Driver, error) {
	switch runtime.GOARCH {
	case "arm64":
		if opts.QemuBin == "" {
			opts.QemuBin = "qemu-system-aarch64"
		}
		if opts.MachineType == "" {
			opts.MachineType = "virt-8.2"
		}
	case "amd64":
		if opts.QemuBin == "" {
			opts.QemuBin = "qemu-system-x86_64"
		}
		if opts.MachineType == "" {
			// This used to refuse to guess, because no x86_64 run had happened.
			// One has: the parity suite is 19/19 on the production CKS node
			// against pc-q35-8.2, which is what hack/parity/run-on-cks.sh
			// resolves as the newest versioned q35 the packaged QEMU 8.2 knows.
			//
			// sata=off and vmport=off are measured hardening, not taste — see
			// the -nodefaults comment in args.go for the `info qtree` device
			// lists. They belong on the machine type because they are machine
			// properties; an operator who overrides MachineType gets exactly
			// what they asked for and loses them, which is why the override is
			// documented as the expert path it is.
			opts.MachineType = "pc-q35-8.2,sata=off,vmport=off"
		}
	default:
		return nil, fmt.Errorf("qemu driver has no known machine type for GOARCH %q", runtime.GOARCH)
	}
	resolved, err := exec.LookPath(opts.QemuBin)
	if err != nil {
		return nil, fmt.Errorf("qemu binary: %w", err)
	}
	if opts.QemuBin, err = filepath.Abs(resolved); err != nil {
		return nil, fmt.Errorf("qemu binary: %w", err)
	}

	guestNetwork, err := guestnet.Parse(opts.Subnet)
	if err != nil {
		return nil, err
	}
	// macFor encodes the slot in two bytes. The supported defaults are much
	// smaller (/16 and /20), but fail explicitly rather than silently reusing
	// MAC addresses if an unusually broad prefix is configured.
	if guestNetwork.Capacity() > 1<<16 {
		return nil, fmt.Errorf("guest subnet %s has %d slots; the qemu driver supports at most 65536",
			guestNetwork, guestNetwork.Capacity())
	}
	opts.Subnet = guestNetwork.String()

	if err := validateGuestDNS(opts.GuestDNS); err != nil {
		return nil, err
	}

	d := &Driver{
		opts: opts, vms: map[string]*vmState{}, creating: map[string]bool{},
		guestNet: guestNetwork, reservedSlots: map[int]bool{},
	}
	if dnsAddr, err := netip.ParseAddr(opts.GuestDNS); err == nil {
		if index, ok := guestNetwork.SlotContaining(dnsAddr.Unmap()); ok {
			d.reservedSlots[index] = true
		}
	}

	if opts.Subnet6 != "" {
		_, ipNet, err := net.ParseCIDR(opts.Subnet6)
		if err != nil {
			return nil, fmt.Errorf("subnet6 %q: %w", opts.Subnet6, err)
		}
		ones, _ := ipNet.Mask.Size()
		if ones > 112 {
			return nil, fmt.Errorf("subnet6 %q: need /112 or larger for per-VM addressing", opts.Subnet6)
		}
		// IPv6 reserves ::1 and assigns one /127 per IPv4 guest slot. A broad
		// IPv4 prefix can therefore outgrow an otherwise valid /112; reject the
		// pair now instead of wrapping addresses into a different IPv6 prefix
		// after enough VMs have been created.
		if hostBits := 128 - ones; hostBits < 32 {
			available := uint64(1) << hostBits
			required := uint64(guestNetwork.Capacity())*2 + 2
			if required > available {
				return nil, fmt.Errorf(
					"subnet6 %q has %d addresses; guest subnet %s needs at least %d",
					opts.Subnet6, available, guestNetwork, required,
				)
			}
		}
		d.prefix6 = ipNet.IP.To16()
		// Providers deliver the routed /64 on-link: the upstream router
		// NDP-resolves each guest's /128 on the segment, and the host only
		// auto-answers for its own addresses. Per-VM addresses live on the
		// taps, so without proxy NDP on the uplink their return traffic is
		// dropped. Record the uplink now; createTap adds a proxy entry per guest.
		d.uplink6 = defaultRoute6Dev()
	}

	// Only the process that will actually open /dev/kvm may insist on it.
	// Under the privileged helper this one never does — the helper holds the
	// device-plugin allocation and execs QEMU — and on CKS this container has no
	// device node at all, so an unconditional probe here is not a safety check,
	// it is a driver that cannot start. fc.go:303 has had this guard from the
	// beginning; the qemu driver was written against the direct launcher and
	// inherited the check without it.
	if opts.PrivilegedHelperSocket == "" {
		if _, err := os.Stat("/dev/kvm"); err != nil {
			// -cpu host, which the spike measured and which every timing in it
			// assumes, is a KVM-only model. TCG would boot and would not be a
			// sandbox host.
			return nil, fmt.Errorf("qemu driver requires /dev/kvm: %w", err)
		}
	}
	if _, err := os.Stat(opts.KernelPath); err != nil {
		return nil, fmt.Errorf("kernel image: %w", err)
	}
	// A previous process leaves its tap devices behind and the first Create
	// would then fail with "Device or resource busy". Nothing of ours is
	// running in a fresh process, so sweep them now — and only ours: see
	// tapPrefix.
	//
	// Gated the way fc.go:314 gates it. Under the helper the taps are not ours
	// to sweep: the helper creates and destroys them, it can still own running
	// guests when this container restarts alone (separate containers of one Pod
	// with independent restarts), and `ip link del` on a live guest's tap would
	// take its network away mid-session. That it currently fails for lack of
	// NET_ADMIN is an accident of the deployment, not a design.
	if opts.PrivilegedHelperSocket == "" {
		sweepStaleTaps()
	}
	return d, nil
}

func (d *Driver) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	var closeErr error
	for _, st := range d.vms {
		if st.cmd != nil {
			closeErr = errors.Join(closeErr, d.stopVMM(st))
		}
	}
	// Taps are deliberately left alone, matching the firecracker driver: their
	// cleanup belongs to the next process's sweepStaleTaps, which runs when
	// nothing can be using them. Close races a shutdown; the sweep does not.
	return closeErr
}
