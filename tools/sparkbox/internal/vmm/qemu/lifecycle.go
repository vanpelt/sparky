//go:build linux

package qemu

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/guestnet"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmhelper"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
)

// Timings for the VMM handshake. They bound only this driver's own waits; the
// caller's ctx still wins whenever it is shorter (vmmtest gives Create, Pause
// and Resume three minutes each).
const (
	// vmmPollInterval paces every "is it there yet" loop: the monitor socket
	// appearing, the runstate settling, the child being reaped.
	vmmPollInterval = 25 * time.Millisecond
	// vmmDialTimeout bounds exec -> monitor reachable. QEMU binds and listens on
	// the -qmp socket during startup, so this is startup cost only; the spike
	// measured a whole cold boot to SSH in 2.3s.
	vmmDialTimeout = 60 * time.Second
	// vmmRunnableTimeout bounds monitor reachable -> runstate "running". On the
	// restore path that covers loading the whole migration stream, which the
	// spike measured at ~9s to SSH for a 57MB file from a 1 GiB guest; a large
	// guest's stream is proportionally bigger.
	vmmRunnableTimeout = 180 * time.Second
	// vmmQuitTimeout is how long `quit` gets before SIGTERM, and SIGTERM before
	// SIGKILL, and SIGKILL before we give up and call the child unreapable.
	vmmQuitTimeout = 10 * time.Second
	// vmmSnapshotTimeout bounds stop -> migrate -> query-migrate. It is the same
	// order as vmmRunnableTimeout on purpose: a restore reads back exactly the
	// stream a snapshot writes, so the two directions deserve the same ceiling
	// (the spike measured 57MB written and ~9s to SSH reading it back). It
	// exists because Pause holds d.mu for its whole duration and its callers
	// frequently have no deadline at all — RunReaper drives Pause on the
	// process-lifetime context — so an unbounded wait here is an unbounded wait
	// for every Create, Resume, Destroy and vitals sample on the host.
	vmmSnapshotTimeout = 180 * time.Second
	// vmmMigrateCancelTimeout bounds the migrate_cancel + settle that precedes
	// the recovery `cont`. Separate from vmmResumeRecoveryTimeout so a slow
	// cancel cannot eat the budget for the `cont`, which is the step that
	// actually gets the guest running again.
	vmmMigrateCancelTimeout = 15 * time.Second
	// vmmResumeRecoveryTimeout bounds the `cont` that un-does a failed snapshot.
	vmmResumeRecoveryTimeout = 15 * time.Second
)

// --- names and paths -------------------------------------------------------
//
// Every path under a VM's directory is derived here and nowhere else. That
// matters most for the snapshot: Firecracker writes mem.snap + state.snap and
// stats that literal pair in five places, QEMU's migrate writes one file, and a
// predicate lifted across the difference matches nothing and silently stops
// refusing what it exists to refuse (fc.go:1612 is the one that bites).

func (d *Driver) vmDir(name string) string {
	return filepath.Join(d.opts.VMStateDir, vmsSubdir, name)
}

func (d *Driver) rootfsPath(name string) string {
	return filepath.Join(d.vmDir(name), rootfsName)
}

func (d *Driver) qmpSocketPath(name string) string {
	return filepath.Join(d.vmDir(name), qmpSocketName)
}

// jailed reports whether the privileged helper owns VM launch, tap creation and
// jail construction for this driver. It is the one predicate that decides which
// of two quite different worlds a method is in, so it is named rather than
// spelled out at each site — the same shape fc.go:387 uses.
func (d *Driver) jailed() bool { return d.opts.PrivilegedHelperSocket != "" }

// jailRoot is the per-slot chroot the helper builds, and the only thing this
// driver ever reads out of it is the QMP socket. Both halves of the path are
// fixed by agreement with internal/vmhelper: the "qemu" component is a constant
// there for exactly this reason, and sparkbox-<slot> matches jailID.
func (d *Driver) jailRoot(idx int) string {
	return filepath.Join(d.opts.JailerChrootBase, "qemu", fmt.Sprintf("sparkbox-%d", idx), "root")
}

// monitorSocket is where this driver dials QEMU's monitor. Under the helper the
// VMM is confined in a jail this process cannot write to, and the helper chowns
// the socket to the controller group so it can be reached through a base
// directory that is traversable but not listable.
func (d *Driver) monitorSocket(name string, st *vmState) string {
	if d.jailed() {
		return filepath.Join(d.jailRoot(st.idx), qmpSocketName)
	}
	return d.qmpSocketPath(name)
}

// snapshotTarget is the path QEMU is told to migrate TO, which is not the path
// this driver later promotes. Under the helper QEMU is chrooted into its jail,
// so a runtime `migrate uri=file:` — resolved from the monitor long after the
// chroot — must name the file relatively; the helper hardlinked the same inode
// into the VM directory, where snapshotNextPath finds it to rename.
func (d *Driver) snapshotTarget(name string) string {
	if d.jailed() {
		return snapshotName + nextSuffix
	}
	return d.snapshotNextPath(name)
}

// vmmLogPath holds the child's stdout and stderr, truncated per launch. With no
// PL011 driver in our arm64 guest kernel serial.log stays empty, so this is the
// only thing a rejected argv or an unloadable migration stream leaves behind.
func (d *Driver) vmmLogPath(name string) string {
	return filepath.Join(d.vmDir(name), vmmLogName)
}

func (d *Driver) serialLogPath(name string) string {
	return filepath.Join(d.vmDir(name), serialLogName)
}

// snapshotPath is the promoted memory snapshot Resume loads with -incoming.
func (d *Driver) snapshotPath(name string) string {
	return filepath.Join(d.vmDir(name), snapshotName)
}

// snapshotNextPath is where Pause migrates to. It is promoted onto
// snapshotPath by a single rename(2) only after the VMM has been reaped, so a
// torn migration can never leave a truncated snapshot for Resume to load.
func (d *Driver) snapshotNextPath(name string) string {
	return d.snapshotPath(name) + nextSuffix
}

// snapshotFiles is every file that together IS name's memory snapshot. It
// returns one path today; the plural is what makes a future second file (a UFFD
// side-car, a config dump) a one-line change here instead of a fourth place
// that silently disagrees about what a snapshot is.
func (d *Driver) snapshotFiles(name string) []string {
	return []string{d.snapshotPath(name)}
}

// removeSnapshotFiles deletes everything that is, or was going to be, name's
// memory snapshot. The half-written .next goes too: it is a full-size image
// with no reader once the promotion it was staged for is not going to happen.
// A file that is already absent is not an error.
func (d *Driver) removeSnapshotFiles(name string) error {
	for _, path := range append(d.snapshotFiles(name), d.snapshotNextPath(name)) {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// hasSnapshot reports whether a complete memory snapshot exists for name. A
// missing file is false with no error; anything else is an error, because
// "cannot tell" must not read as "there is none" to RenameVM's refusal.
func (d *Driver) hasSnapshot(name string) (bool, error) {
	for _, path := range d.snapshotFiles(name) {
		if _, err := os.Stat(path); err != nil {
			if os.IsNotExist(err) {
				return false, nil
			}
			return false, err
		}
	}
	return true, nil
}

// --- addressing ------------------------------------------------------------

func (d *Driver) hostIP(idx int) string  { return d.guestSlot(idx).Host.String() }
func (d *Driver) guestIP(idx int) string { return d.guestSlot(idx).Guest.String() }

// tapName is derived from the slot alone, like the MAC and the addresses, and
// is a METHOD because the answer depends on who creates the device.
//
// On the direct path this driver creates and sweeps its own taps, and the
// prefix must differ from the firecracker driver's or one driver's startup
// sweep would delete the other's live networking (see tapPrefix).
//
// Under the helper it creates none: the helper makes the device, in the Pod
// network namespace shared by both containers, and it calls it sbtap<slot> —
// the name internal/netpush, internal/hostsetup's sluice --tap-prefix and
// deploy/sparkbox-net.sh all already hardcode. So this follows the helper's
// name there, which is what the tapPrefix comment said would happen when this
// path landed. Neither sweep can reach the other: the helper's runs in the
// helper, and this driver's is skipped entirely when jailed.
func (d *Driver) tapName(idx int) string {
	if d.jailed() {
		return helperTapPrefix + strconv.Itoa(idx)
	}
	return tapPrefix + strconv.Itoa(idx)
}

func (d *Driver) guestSlot(idx int) guestnet.Slot {
	slot, err := d.guestNet.Slot(idx)
	if err != nil {
		// Slots are allocated by freeSlot and stored in vmState, so reaching
		// this means an internal invariant was violated rather than bad input.
		panic(err)
	}
	return slot
}

// IPv6 addressing: each slot gets a point-to-point /127 carved from the /64,
// host on the even address and guest on the odd one. IPv4 slot indexes begin
// at zero, so add one before deriving the IPv6 offset: slot idx=0 becomes
// ::2 (host) / ::3 (guest), leaving ::1 free for the host's own edge address
// (the AAAA target). Globally routable, so egress needs no NAT — just host
// forwarding.
func (d *Driver) hostIP6(idx int) string  { return d.addr6((idx + 1) * 2) }
func (d *Driver) guestIP6(idx int) string { return d.addr6((idx+1)*2 + 1) }

func (d *Driver) addr6(off int) string {
	ip := make(net.IP, net.IPv6len)
	copy(ip, d.prefix6)
	// Place the offset in the low 32 bits of the /64's host portion.
	ip[12] = byte(off >> 24)
	ip[13] = byte(off >> 16)
	ip[14] = byte(off >> 8)
	ip[15] = byte(off)
	return ip.String()
}

// --- name reservation and slots --------------------------------------------

// reserveName claims cfg.Name for an in-flight Create so no second Create
// prepares the same rootfs concurrently, and so the name is still refused
// while d.mu is released.
func (d *Driver) reserveName(name string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.creating == nil {
		d.creating = map[string]bool{}
	}
	if _, ok := d.vms[name]; ok {
		return fmt.Errorf("vm %q already exists", name)
	}
	if d.creating[name] {
		return fmt.Errorf("vm %q is already being created", name)
	}
	d.creating[name] = true
	return nil
}

func (d *Driver) releaseName(name string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.creating, name)
}

// freeSlot returns the lowest network slot no vmState holds. Caller must hold
// d.mu. Every path that drops a record (Destroy, DropSnapshots, RenameVM)
// thereby releases its slot for reuse — a reused slot can't collide with a
// live tap because paused VMs keep their record (idx stays reserved) and the
// record only goes away after the tap does. The configured prefix determines
// the bound: every index maps to exactly one /30 inside it.
func (d *Driver) freeSlot() (int, error) {
	used := make(map[int]bool, len(d.vms))
	for _, s := range d.vms {
		used[s.idx] = true
	}
	for idx := 0; idx < d.guestNet.Capacity(); idx++ {
		if !used[idx] && !d.reservedSlots[idx] {
			return idx, nil
		}
	}
	return 0, fmt.Errorf("no free network slots in %s (max %d concurrent VMs)",
		d.guestNet, d.guestNet.Capacity())
}

// --- Create ----------------------------------------------------------------

func (d *Driver) Create(ctx context.Context, cfg vmm.Config) (inst *vmm.Instance, retErr error) {
	// Unlike Firecracker, whose API validates the machine config and reports a
	// useful error, QEMU turns a zero here into either a cryptic startup failure
	// or a guest with no RAM. memMB is also the balloon's baseline (the units are
	// inverted, so a zero baseline reports every guest as fully ballooned), so
	// refuse it at the door.
	if cfg.VCPUs < 1 || cfg.MemMB < 1 {
		return nil, fmt.Errorf("vm %q: vcpus (%d) and memory (%d MiB) must both be positive",
			cfg.Name, cfg.VCPUs, cfg.MemMB)
	}
	// Preparing the rootfs means a template copy and a loop mount that may
	// replay an ext4 journal on a 25 GiB image — far too slow to hold the
	// driver-wide lock through, especially on a restart recreating every
	// sandbox in turn. Reserve the name instead, do the disk work unlocked,
	// then take d.mu for the slot bookkeeping and boot.
	if err := d.reserveName(cfg.Name); err != nil {
		return nil, err
	}
	defer d.releaseName(cfg.Name)

	dir := d.vmDir(cfg.Name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}

	// CoW-copy the rootfs template. Refuse to fall back to a full 25 GiB copy:
	// VMStateDir is the hot tier and an incompatible mount is a startup/config
	// error, not a reason for sandbox creation to become unexpectedly huge.
	// (Measured: on the container's overlayfs --reflink=auto silently copied the
	// image and turned a 2.3s boot into a >45s one.)
	//
	// Whether this branch is taken is the only honest answer to "is this disk
	// new", and the guest needs that answer: sparkbox-repos may switch an
	// inherited checkout onto the branch the manifest asks for on a disk nobody
	// has logged into yet, and may never do it on one somebody is working in.
	// Create runs for a cold boot of an EXISTING sandbox too — the manager
	// re-creates after DropSnapshots and after a restart — so the absence of a
	// record proves nothing and this stat is what decides.
	rootfs := d.rootfsPath(cfg.Name)
	fresh := false
	switch _, err := os.Stat(rootfs); {
	case os.IsNotExist(err):
		template := d.templatePath(cfg.Image)
		if err := reflinkClone(ctx, template, rootfs); err != nil {
			return nil, err
		}
		fresh = true
	case err != nil:
		return nil, err
	case cfg.NewSandbox:
		// A disk under a name the ledger has never issued is residue, not
		// state, and booting it would hand its previous owner's home directory
		// to whoever claimed the name next. Refuse rather than reclaim: the
		// ledger says this name is free, but a ledger can be restored from a
		// backup and 25 GiB of somebody's work is not recoverable from a wrong
		// guess. RenameVM already refuses a destination dir for the same reason.
		return nil, fmt.Errorf("vm dir for %q already holds a rootfs (%s); "+
			"a previous sandbox of this name did not finish being destroyed", cfg.Name, rootfs)
	}
	// A clone this call made must not survive this call failing. Nothing else
	// will ever come back for it: the name is not in d.vms, so Close and Destroy
	// have no record to work from, and host.Manager.CreateSandbox returns the
	// driver's error without calling Destroy. Left behind, the disk turns the
	// NEXT Create of this name into the NewSandbox refusal above — the manager
	// always sets NewSandbox for a name absent from its ledger, so it stats the
	// rootfs, calls it residue, and reports "a previous sandbox of this name did
	// not finish being destroyed" about a sandbox that never existed. The name
	// is then unusable until someone removes the directory on the node by hand.
	//
	// Guarded on `fresh`, which is the whole point: Create also runs for a cold
	// boot of an EXISTING sandbox, which takes every one of these failure paths,
	// and deleting the tenant's 25 GiB rootfs there is the accident this guard
	// exists to not have.
	//
	// The rootfs only, never the directory: qemu.log and serial.log are the sole
	// account of why the boot failed, and a caller that folds a log tail into an
	// error must not then delete the log. Removing the rootfs is enough — it is
	// what the next Create stats.
	if fresh {
		defer func() {
			if retErr != nil {
				os.Remove(rootfs) //nolint:errcheck
			}
		}()
	}

	if !d.opts.DisableHostRootfsMounts {
		if err := installAuthorizedKey(ctx, rootfs, d.opts.LoginUser, cfg.GatewayPublicKey); err != nil {
			return nil, fmt.Errorf("install gateway key: %w", err)
		}
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	// Re-check under the lock: reserveName only excludes a second Create, and
	// Restore could have registered this name while the disk work ran.
	if _, ok := d.vms[cfg.Name]; ok {
		return nil, fmt.Errorf("vm %q already exists", cfg.Name)
	}

	idx, err := d.freeSlot()
	if err != nil {
		return nil, err
	}
	// vcpus and memMB are recorded because QEMU's restore argv has to repeat
	// them: -smp and -m come from the command line on both paths, where
	// Firecracker recovers them from the snapshot.
	st := &vmState{idx: idx, vcpus: cfg.VCPUs, memMB: cfg.MemMB}
	if err := d.createTap(ctx, st.idx); err != nil {
		// Clean up any half-configured (or stale) device so a retry can reuse
		// this slot; only recording it in d.vms reserves it, so a failed create
		// leaks no idx.
		d.deleteTap(st.idx)
		return nil, err
	}
	if err := d.boot(ctx, cfg.Name, st, rootfs, false, fresh); err != nil {
		d.deleteTap(st.idx)
		return nil, err
	}
	d.vms[cfg.Name] = st
	return d.instance(cfg.Name, st), nil
}

// --- boot ------------------------------------------------------------------

// boot launches the QEMU process for name and does not return until the guest
// is actually executing. restore appends -incoming and turns the launch into a
// migration load; fresh says the caller just laid this rootfs down from a
// template and no guest has ever run on it (it becomes sparkbox_fresh=1, which
// the guest reads as permission to move somebody's git checkout, so it must
// stay false on every path that reuses a disk).
//
// The guest command line is BUILT on a cold boot and REPLAYED on a restore, out
// of st.cmdline — see bootCmdline. That is what makes the restore argv the boot
// argv token for token, rather than the boot argv with a differing -append.
//
// The machine size comes from st, never from arguments: the restore argv must
// repeat the source's -smp and -m exactly, and taking them from parameters is
// how fc.go:1073's harmless zeros would become a driver that cold-boots a
// 0 MiB guest.
//
// READINESS. boot reports success when three things hold: the child is still
// alive, its QMP monitor completed capability negotiation, and query-status
// reports the runstate "running". That is trustworthy in a way "the socket
// appeared" is not — QEMU creates the monitor socket during startup and will
// happily accept a connection on it and then die when it reaches an argv it
// cannot satisfy, and on the restore path the runstate stays "inmigrate" for as
// long as the stream takes to load. Reaching "running" means the vCPUs are
// executing this guest's memory, which is the same guarantee Firecracker's
// InstanceStart gives fc.go's boot, plus proof the incoming stream loaded. It
// deliberately stops short of the guest's own readiness: the driver has no SSH
// credentials, and waiting for sshd is the manager's job (and the parity
// fixture's BootTimeout).
func (d *Driver) boot(ctx context.Context, name string, st *vmState, rootfs string, restore bool, fresh bool) error {
	dir := d.vmDir(name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if !d.jailed() {
		// QEMU does not unlink a stale monitor socket, it just fails to bind —
		// so a crashed VMM would otherwise make this name unbootable forever.
		// Under the helper there is nothing to clear and nothing we could:
		// the socket lives in a jail root this process cannot write to, and the
		// helper removes the whole jail at every VMM exit.
		if err := os.Remove(d.qmpSocketPath(name)); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("clear stale qmp socket: %w", err)
		}
	}

	cmdline, err := d.bootCmdline(name, st, restore, fresh)
	if err != nil {
		return err
	}

	cmd, logFile, err := d.vmmCommand(name, st, rootfs, cmdline, restore)
	if err != nil {
		return err
	}
	// The child keeps its own descriptor; ours is only needed to hand over.
	// (nil under the helper, which opens the log on its side of the boundary.)
	if logFile != nil {
		defer logFile.Close() //nolint:errcheck
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start qemu for %q: %w", name, err)
	}
	st.cmd = cmd
	st.qmp = nil
	st.waitErr = nil
	st.exited = make(chan struct{})
	// One supervisor per launch. It exists so every wait in this file can tell
	// "still coming up" from "already dead" instead of timing out, and so
	// stopVMM has a reap signal that is true however the child died. The write
	// to waitErr happens-before the close, and nothing reads it without first
	// receiving from exited.
	go func(st *vmState, cmd *exec.Cmd, exited chan struct{}) {
		st.waitErr = cmd.Wait()
		close(exited)
	}(st, cmd, st.exited)

	conn, err := d.dialVMM(ctx, name, st)
	if err != nil {
		d.stopVMM(st) //nolint:errcheck // the dial error is the one worth reporting
		return err
	}
	st.qmp = conn

	if restore {
		// -incoming loads the stream after startup, and `cont` fails outright
		// while the runstate is still "inmigrate" ("Migration is not
		// finalized"). hack/qemu-spike/probe.sh only worked because it retried
		// cont in a loop; polling the runstate is the same wait, stated.
		rctx, cancel := context.WithTimeout(ctx, vmmRunnableTimeout)
		err := conn.AwaitRunnable(rctx)
		if err == nil {
			err = conn.Cont(rctx)
		}
		cancel()
		if err != nil {
			d.stopVMM(st) //nolint:errcheck
			return d.vmmError(name, st, fmt.Sprintf("restore vm %q from snapshot", name), err)
		}
	}

	if err := d.awaitRunning(ctx, name, st); err != nil {
		d.stopVMM(st) //nolint:errcheck
		return err
	}

	// Guest balloon stats need this QOM property set and one interval to
	// elapse, and the property is NOT part of virtio-balloon's migrated device
	// state — so it must be re-issued after a restore just as after a cold
	// boot, or BalloonStats goes stale on every resumed sandbox.
	//
	// Fatal on a cold boot: silently losing the fleet's working-set signal
	// makes host.Manager.MemStats charge every sandbox its full ceiling.
	//
	// NOT fatal on a restore, and the asymmetry is the point. By this line a
	// restored guest's memory has loaded and its vCPUs are executing; throwing
	// that away over a stats property would turn a stats problem into "every
	// Resume of every sandbox fails", with an error naming the balloon — the
	// reverse of the diagnosis the operator needs. That the qom-set behaves the
	// same against a migrated virtio-balloon is INFERRED, not measured (see
	// qemu.go's balloon section), so it is the wrong inference to bet a
	// sandbox's RAM on. Record it instead: st.statsErr makes BalloonStats say
	// there is no sample, which MemStats already handles by keeping its last
	// reading — where saying nothing would let qom-get hand back the
	// pre-pause sample forever, with a plausible last-update and no error.
	st.statsErr = nil
	if err := st.qmp.EnableBalloonStats(ctx, balloonStatsIntervalSecs); err != nil {
		if !restore {
			d.stopVMM(st) //nolint:errcheck
			return d.vmmError(name, st, fmt.Sprintf("enable balloon stats for %q", name), err)
		}
		st.statsErr = fmt.Errorf("balloon stats were not re-enabled after the restore: %w", err)
	}

	st.paused = false
	// Recorded only now, so a launch that never reached a running guest cannot
	// pin a command line for a later restore to replay.
	st.cmdline = cmdline
	return nil
}

// vmmCommand builds the process boot supervises, which is QEMU itself on a dev
// box and a launch client standing in for it on a hardened node.
//
// The substitution is what keeps the rest of this file honest. Under the helper
// the real QEMU is a child of a root process in another container, so st.cmd
// cannot be it — but the launch client lives exactly as long as that VMM does
// (the helper answers its half-close only once the VMM, the tap and the jail
// are gone), so every wait, signal and reap in stopVMM keeps its meaning.
//
// It is exec.Command, NOT exec.CommandContext(ctx, ...), on both paths. Neither
// child has a ctx-driven kill of its own, and binding one to the caller's ctx
// reintroduces exactly the bug fc.go:799's vmCtx exists to prevent: ctx here is
// request-scoped (the create-on-connect SSH session), so the microVM would die
// the instant that first connection closed. stopVMM is the only thing that ends
// these processes.
func (d *Driver) vmmCommand(name string, st *vmState, rootfs, cmdline string, restore bool) (*exec.Cmd, *os.File, error) {
	if d.jailed() {
		// The argv is built on the far side of the boundary, from this machine
		// and the helper's own configuration — see internal/vmhelper. What
		// crosses is data: a validated name and slot, the size of the machine,
		// and the guest command line. No path, no binary, no command.
		//
		// The command line is passed rather than rebuilt for the same reason
		// bootCmdline replays it: the restore argv must be the boot argv token
		// for token, and only this process knows what the boot argv was.
		cmd := vmhelper.LaunchCommand(d.opts.PrivilegedHelperBin, vmhelper.Launch{
			Socket: d.opts.PrivilegedHelperSocket,
			Name:   name, Slot: st.idx, Resume: restore,
			VCPUs: st.vcpus, MemMB: st.memMB, Cmdline: cmdline,
		})
		cmd.Env = []string{}
		return cmd, nil, nil
	}
	args, err := d.qemuArgs(name, st, rootfs, cmdline, restore)
	if err != nil {
		return nil, nil, err
	}
	logFile, err := os.OpenFile(d.vmmLogPath(name), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, nil, fmt.Errorf("open vmm log: %w", err)
	}
	cmd := exec.Command(d.opts.QemuBin, args...)
	// Everything on the argv is absolute; running in the VM's own directory just
	// means a core dump or a stray temp file lands where Destroy will reclaim it.
	cmd.Dir = d.vmDir(name)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	return cmd, logFile, nil
}

// dialVMM waits for the monitor to become reachable, giving up early if the
// child dies rather than burning the whole timeout on a process that is
// already gone.
func (d *Driver) dialVMM(ctx context.Context, name string, st *vmState) (*qmpConn, error) {
	dctx, cancel := context.WithTimeout(ctx, vmmDialTimeout)
	defer cancel()
	sock := d.monitorSocket(name, st)
	var lastErr error
	for {
		select {
		case <-st.exited:
			return nil, d.vmmError(name, st, fmt.Sprintf("qemu for %q exited before its monitor was reachable", name), nil)
		default:
		}
		conn, err := dialQMP(dctx, sock)
		if err == nil {
			return conn, nil
		}
		lastErr = err
		select {
		case <-st.exited:
			return nil, d.vmmError(name, st, fmt.Sprintf("qemu for %q exited before its monitor was reachable", name), nil)
		case <-dctx.Done():
			return nil, d.vmmError(name, st, fmt.Sprintf("qemu monitor for %q never came up", name), lastErr)
		case <-time.After(vmmPollInterval):
		}
	}
}

// awaitRunning polls the runstate until the vCPUs are executing. See boot's
// comment on why this, and not the socket appearing, is the readiness signal.
func (d *Driver) awaitRunning(ctx context.Context, name string, st *vmState) error {
	rctx, cancel := context.WithTimeout(ctx, vmmRunnableTimeout)
	defer cancel()
	var lastErr error
	for {
		select {
		case <-st.exited:
			return d.vmmError(name, st, fmt.Sprintf("qemu for %q exited before the guest was running", name), nil)
		default:
		}
		state, err := st.qmp.QueryStatus(rctx)
		lastErr = err
		if err == nil {
			switch state {
			case "running":
				return nil
			case "prelaunch", "inmigrate", "finish-migrate", "paused", "postmigrate":
				// Still on its way up. "paused" is reachable here only in the
				// window between a restore's stream landing and our own cont.
				lastErr = fmt.Errorf("runstate %q", state)
			default:
				// shutdown, guest-panicked, internal-error and friends: the
				// guest is not coming up on its own.
				return d.vmmError(name, st,
					fmt.Sprintf("vm %q reached runstate %q instead of running", name, state), nil)
			}
		}
		select {
		case <-st.exited:
			return d.vmmError(name, st, fmt.Sprintf("qemu for %q exited before the guest was running", name), nil)
		case <-rctx.Done():
			return d.vmmError(name, st, fmt.Sprintf("vm %q never reached the running state", name), lastErr)
		case <-time.After(vmmPollInterval):
		}
	}
}

// vmmError builds the operator-facing failure for a VMM that would not start or
// would not stay up. It folds in the child's exit status when it has one and
// the tail of qemu.log always, because with an empty serial.log that log is the
// only diagnostic a rejected argv or an unloadable migration stream produces.
func (d *Driver) vmmError(name string, st *vmState, what string, cause error) error {
	// Only consult the exit status if the child is actually reaped; this must
	// never block a caller holding d.mu.
	var exit error
	if st.exited != nil {
		select {
		case <-st.exited:
			exit = st.waitErr
		default:
		}
	}
	msg := what
	if cause != nil {
		msg = fmt.Sprintf("%s: %v", msg, cause)
	}
	if exit != nil {
		msg = fmt.Sprintf("%s (%v)", msg, exit)
	}
	if tail := qemuLogTail(d.vmmLogPath(name)); tail != "" {
		msg = fmt.Sprintf("%s: %s", msg, tail)
	}
	return fmt.Errorf("%s", msg)
}

// qemuLogTail returns the end of a VM's qemu.log as ONE line.
//
// The flattening is not cosmetic. ctlops.wireSentence blanks any message
// containing a control character and substitutes "the remote host reported a
// failure it could not describe", so a multi-line QEMU diagnostic pasted into
// an error reaches the operator as no error at all — the same trap
// reflinkClone's trailing-newline trim exists for.
func qemuLogTail(path string) string {
	const maxTail = 2 << 10
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close() //nolint:errcheck
	info, err := f.Stat()
	if err != nil {
		return ""
	}
	size, offset := info.Size(), int64(0)
	if size > maxTail {
		offset, size = size-maxTail, maxTail
	}
	buf := make([]byte, size)
	n, _ := f.ReadAt(buf, offset) //nolint:errcheck // a short read is still a useful tail
	return strings.Join(strings.Fields(string(buf[:n])), " ")
}

// --- Pause -----------------------------------------------------------------

// Pause stops the guest's vCPUs, writes its memory to a file and shuts the VMM
// down, leaving a sandbox Resume can bring back with its RAM intact.
//
// The sequence is stop -> migrate -> poll to completion -> quit -> wait for
// exit -> promote, and every step of that order was measured (see
// docs/qemu-spike.md): migrate returns {} immediately and is asynchronous, so
// quitting on its reply truncates the snapshot, and the rootfs stays open until
// the process is reaped, so nothing may rename or pack before the wait returns.
func (d *Driver) Pause(ctx context.Context, name string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	st, ok := d.vms[name]
	if !ok {
		return fmt.Errorf("vm %q not found", name)
	}
	// Pausing a paused sandbox is a no-op, not an error: the manager pauses
	// before archiving, resizing and renaming, and those paths must compose.
	if st.paused {
		return nil
	}
	if st.cmd == nil || st.qmp == nil {
		return fmt.Errorf("vm %q is not running", name)
	}
	select {
	case <-st.exited:
		return d.vmmError(name, st, fmt.Sprintf("cannot pause %q: its qemu process is gone", name), nil)
	default:
	}

	next := d.snapshotNextPath(name)
	// A previous torn attempt would otherwise contribute its tail to this one.
	if err := os.Remove(next); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("clear stale snapshot output: %w", err)
	}
	if d.jailed() {
		// A confined QEMU cannot create a file this process can read, and a
		// file this process creates is not one it can write. The helper does
		// both halves: it creates the output in the VM directory owned by the
		// per-slot uid and hardlinks it into the jail, which is the same
		// create-chown-link pattern fc.go's jailed pause relies on and the one
		// the CKS measurement proved mandatory — the negative control, with the
		// file not pre-created, failed with "Permission denied".
		hctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := vmhelper.PrepareSnapshotOutputs(hctx, d.opts.PrivilegedHelperSocket, name, st.idx)
		cancel()
		if err != nil {
			return fmt.Errorf("privileged helper snapshot outputs for %q: %w", name, err)
		}
	}

	// Every other VMM wait in this file has a driver-side ceiling; this is the
	// longest-running one and it holds d.mu throughout, so it gets one too. See
	// vmmSnapshotTimeout. The caller's ctx still wins when it is shorter.
	sctx, cancel := context.WithTimeout(ctx, vmmSnapshotTimeout)
	defer cancel()
	if err := st.qmp.Stop(sctx); err != nil {
		return fmt.Errorf("pause vcpus of %q: %w", name, err)
	}
	// The path QEMU writes to is not the path this driver promotes: under the
	// helper the two are hardlinks to one inode, named relatively inside the
	// jail and absolutely in the VM directory. See snapshotTarget.
	if err := d.snapshotMemory(sctx, name, st, d.snapshotTarget(name), next); err != nil {
		return err
	}
	// quit and reap. A child that cannot be reaped still holds the rootfs and
	// the migration file open, so promote nothing and tear nothing down: report
	// it and leave the sandbox as it is.
	if err := d.stopVMM(st); err != nil {
		return fmt.Errorf("stop vmm for %q: %w", name, err)
	}
	if err := os.Rename(next, d.snapshotPath(name)); err != nil {
		// The VMM is already reaped, so returning here with st.paused still
		// false would leave a record that is neither running nor paused:
		// instance() would report StateRunning with an SSHAddr for a process
		// that is gone, Resume would hand that bogus instance back, a retried
		// Pause would say "not running" forever, and the tap would stay up
		// until a Destroy that nothing is going to issue.
		//
		// fc.go:1000 resolves the same failure by still tearing the tap down
		// and setting paused=true. That is not enough here, for the reason
		// DropSnapshots already documents: a paused record whose snapshot did
		// not land is a trap. Worse, a second Pause of a sandbox that has been
		// resumed once leaves the PREVIOUS state.migrate in place, and
		// resuming a guest's hour-old memory against a rootfs it has been
		// writing to since is filesystem corruption, not a stale sandbox. So
		// drop the memory outright and forget the name: that is the shape a
		// controller restart leaves behind, and the shape resumeOrRecreate
		// cold-boots from the preserved rootfs. The memory is lost either way;
		// the disk is intact.
		err = fmt.Errorf("install memory snapshot for %q: %w", name, err)
		d.deleteTap(st.idx)
		if rmErr := d.removeSnapshotFiles(name); rmErr != nil {
			err = fmt.Errorf("%w (and its stale snapshot could not be removed: %v)", err, rmErr)
		}
		delete(d.vms, name)
		return err
	}
	// The VM is gone, so its host-side tap is orphaned. Tear it down here;
	// Resume recreates it via createTap. Leaving it wedges Resume with
	// "tuntap add: Device or resource busy" on the still-present device.
	d.deleteTap(st.idx)
	st.paused = true
	return nil
}

// snapshotMemory drives migrate + query-migrate and, on failure, puts the guest
// back the way it found it. Caller holds d.mu and has already stopped the vCPUs.
func (d *Driver) snapshotMemory(ctx context.Context, name string, st *vmState, target, next string) error {
	resumeAndFail := func(err error, what string) error {
		// Recover on a FRESH context: when the failure IS the caller's ctx
		// expiring (the migration outran a deadline), resuming on that same dead
		// ctx fails instantly and leaves the vCPUs stopped — an unreachable,
		// unresumable sandbox that needs a manual monitor poke. A running
		// sandbox that failed to snapshot is strictly better than a half-paused
		// one, so this is best-effort and the snapshot error is what we return.
		//
		// CANCEL BEFORE UNLINKING AND BEFORE `cont`. Only "failed" and
		// "cancelled" reach here having stopped the migration; a deadline
		// reached while the stream was still `active` leaves QEMU writing.
		// Unlinking under it strands a full guest's worth of hot tier in an
		// inode with no path — invisible to du, to PackRootfs and to
		// DropSnapshots — and the `cont` restarts the vCPUs under a live
		// migration that then completes and re-stops them in `postmigrate`,
		// leaving a sandbox the driver reports as Running that nothing can
		// reach. See qmpConn.MigrateCancel.
		cctx, ccancel := context.WithTimeout(context.WithoutCancel(ctx), vmmMigrateCancelTimeout)
		if cerr := st.qmp.MigrateCancel(cctx); cerr == nil {
			st.qmp.AwaitMigrationSettled(cctx) //nolint:errcheck // best effort; the remove below is what it protects
		}
		ccancel()
		os.Remove(next) //nolint:errcheck // a partial stream is worse than none

		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), vmmResumeRecoveryTimeout)
		defer cancel()
		st.qmp.Cont(rctx) //nolint:errcheck
		return fmt.Errorf("%s for %q: %w", what, name, err)
	}
	if err := st.qmp.MigrateToFile(ctx, target); err != nil {
		return resumeAndFail(err, "start memory snapshot")
	}
	if err := st.qmp.AwaitMigration(ctx); err != nil {
		return resumeAndFail(err, "write memory snapshot")
	}
	return nil
}

// --- Resume ----------------------------------------------------------------

func (d *Driver) Resume(ctx context.Context, name string) (*vmm.Instance, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	st, ok := d.vms[name]
	if !ok {
		return nil, fmt.Errorf("vm %q not found", name)
	}
	if !st.paused {
		return d.instance(name, st), nil
	}
	snapshot := d.snapshotPath(name)
	if _, err := os.Stat(snapshot); err != nil {
		return nil, fmt.Errorf("no snapshot for %q: %w", name, err)
	}
	if err := d.createTap(ctx, st.idx); err != nil {
		return nil, err
	}
	// The restore argv is the cold-boot argv plus -incoming, built by the same
	// function — and, since boot replays st.cmdline on this path, that includes
	// -append. The identity QEMU's migration stream demands is structural.
	// fresh=false is what the REBUILD would use if this record somehow carried
	// no command line; a resume restores the memory of a guest that already ran
	// on this disk, so nothing about it is a first boot.
	if err := d.boot(ctx, name, st, d.rootfsPath(name), true, false); err != nil {
		// Unlike a failed cold boot the tap here was created by us moments ago,
		// and leaving it behind makes the next Resume fail with "Device or
		// resource busy" — a retry would then look like a snapshot problem.
		d.deleteTap(st.idx)
		return nil, err
	}
	return d.instance(name, st), nil
}

// --- Destroy ---------------------------------------------------------------

func (d *Driver) Destroy(_ context.Context, name string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	st, ok := d.vms[name]
	if !ok {
		// No record does not mean no disk. d.vms is per-process and nothing
		// rehydrates it: after the controller restarts, every sandbox it has
		// not booted since is absent from the map while its rootfs sits in
		// VMStateDir exactly where the previous process left it. Returning
		// early here reported a successful destroy and leaked the disk — and
		// worse, left it for Create to adopt under a re-used name.
		//
		// RemoveAll of a path that was never there is nil, which is also what
		// makes destroying an unknown name idempotent — the parity suite
		// destroys every sandbox again in teardown, including ones its body
		// already destroyed or renamed away.
		return os.RemoveAll(d.vmDir(name))
	}
	if st.cmd != nil {
		// A child we could not reap still has the rootfs open. Removing the
		// directory under it would unlink a disk a live VMM keeps writing to,
		// so keep the record and report instead.
		if err := d.stopVMM(st); err != nil {
			return fmt.Errorf("stop vmm for %q: %w", name, err)
		}
	}
	d.deleteTap(st.idx)
	// Drop the record last: it releases the slot (and with it the tap name, the
	// addresses and the MAC) back to freeSlot, which is only safe once the tap
	// is actually gone.
	delete(d.vms, name)
	// The pack artifact is deliberately a SIBLING of this directory, so an
	// archive in flight survives the destroy that reclaims the hot tier.
	return os.RemoveAll(d.vmDir(name))
}

// --- stopping the VMM ------------------------------------------------------

// stopVMM ends the QEMU process for st and does not return until it has been
// reaped. It is the only thing in this package that stops a VMM.
//
// It returns nil whenever the child is gone, however it went — a SIGKILLed
// process exits with a signal status and that is still a successful stop. The
// only non-nil return is "still running after SIGKILL", which callers must
// treat as "the rootfs and the snapshot may still be open": Pause must not
// promote and Destroy must not remove.
func (d *Driver) stopVMM(st *vmState) error {
	if st.cmd == nil {
		return nil
	}
	cmd, exited, conn := st.cmd, st.exited, st.qmp
	// The clean path: QEMU acknowledges `quit` and then exits. A monitor that
	// has already gone away (EOF) is not a problem, it just means we escalate.
	if conn != nil {
		qctx, cancel := context.WithTimeout(context.Background(), vmmQuitTimeout)
		conn.Quit(qctx) //nolint:errcheck // best effort; the signals below are the guarantee
		cancel()
	}
	err := waitOrSignal(cmd, exited)
	if conn != nil {
		conn.Close() //nolint:errcheck
	}
	if err != nil {
		// Leave st.cmd set: the process is still out there holding this VM's
		// files, and clearing the liveness predicate would tell every other
		// method the disk is safe to touch.
		return err
	}
	st.cmd = nil
	st.qmp = nil
	return nil
}

func waitOrSignal(cmd *exec.Cmd, exited <-chan struct{}) error {
	if exited == nil {
		// A cmd with no supervisor cannot happen (boot starts them together),
		// but reaping it here rather than leaking a zombie is free.
		return cmd.Wait()
	}
	// nil is the `quit` already sent; QEMU catches SIGTERM in its main loop and
	// exits the same way quit does, and SIGKILL is for a main loop that is
	// wedged.
	for _, signal := range []os.Signal{nil, syscall.SIGTERM, syscall.SIGKILL} {
		if signal != nil && cmd.Process != nil {
			cmd.Process.Signal(signal) //nolint:errcheck
		}
		select {
		case <-exited:
			return nil
		case <-time.After(vmmQuitTimeout):
		}
	}
	return fmt.Errorf("qemu process %d did not exit after SIGKILL", cmd.Process.Pid)
}

// --- instance ---------------------------------------------------------------

func (d *Driver) instance(name string, st *vmState) *vmm.Instance {
	user := d.opts.LoginUser
	if user == "" {
		user = "root"
	}
	inst := &vmm.Instance{Name: name, SSHUser: user}
	if st.paused {
		inst.State = vmm.StatePaused
	} else {
		inst.State = vmm.StateRunning
		inst.SSHAddr = net.JoinHostPort(d.guestIP(st.idx), "22")
		// HostIP is "the address reachable from the host for the VM's own
		// services", so it is the GUEST's address, not the host end of the /30.
		// The proxy reaches in-VM services over the internal v4 hop (works
		// regardless of whether the guest app binds v4 or ::); the routable v6
		// is the sandbox's public identity + no-NAT egress.
		inst.HostIP = d.guestIP(st.idx)
		if d.prefix6 != nil {
			inst.GuestV6 = d.guestIP6(st.idx)
		}
	}
	return inst
}

// --- host networking --------------------------------------------------------

// createTap sets up the host side of the VM's network: a tap device owned by
// this process's user with the host-side /30 address. QEMU opens it by name
// (-netdev tap,ifname=...,script=no), so it must exist before the VMM starts.
func (d *Driver) createTap(ctx context.Context, idx int) error {
	if d.jailed() {
		// The helper creates the TAP immediately before launching QEMU, in the
		// Pod network namespace shared by both containers, and tears it down
		// when the VMM exits. This process has no NET_ADMIN and nothing to do.
		return nil
	}
	tap := d.tapName(idx)
	cmds := [][]string{
		{"ip", "tuntap", "add", "dev", tap, "mode", "tap"},
		{"ip", "addr", "add", d.hostIP(idx) + "/30", "dev", tap},
	}
	if d.prefix6 != nil {
		// Host side of the point-to-point /127; the connected route this creates
		// is how inbound traffic to the guest's /128 reaches the tap.
		cmds = append(cmds, []string{"ip", "-6", "addr", "add", d.hostIP6(idx) + "/127", "dev", tap})
	}
	cmds = append(cmds, []string{"ip", "link", "set", "dev", tap, "up"})
	for _, c := range cmds {
		if out, err := exec.CommandContext(ctx, c[0], c[1:]...).CombinedOutput(); err != nil {
			return fmt.Errorf("%v: %v: %s", c, err, strings.TrimSpace(string(out)))
		}
	}
	// Strict reverse-path filtering: drop any packet from this tap whose source
	// address doesn't route back to it, so a guest can't source-spoof a
	// neighbour's address across the host's inter-tap forwarding. Set per-tap
	// because the kernel takes the max of the "all" and per-device values, so a
	// permissive host default can't undo it.
	//
	// Best-effort, like the proxy-NDP setup below: this is defence in depth, not
	// the guarantee. The metadata service identifies callers by source address
	// (see internal/metadata), and TCP already makes that unspoofable — a forged
	// SYN is answered towards the real owner of the address, so the spoofer
	// never completes the handshake. Failing sandbox creation over this would
	// trade a whole-host outage for no real security.
	exec.CommandContext(ctx, "sysctl", "-qw", "net.ipv4.conf."+tap+".rp_filter=1").Run() //nolint:errcheck

	// Answer NDP for this guest's /128 on the uplink so the provider's on-link
	// delivery of the routed /64 reaches the VM (its address lives on the tap,
	// not the uplink). Best-effort: the VM still boots if this fails, it just
	// won't have v6 return traffic. del-then-add keeps it idempotent.
	if d.prefix6 != nil && d.uplink6 != "" {
		exec.CommandContext(ctx, "sysctl", "-qw", "net.ipv6.conf."+d.uplink6+".proxy_ndp=1").Run()             //nolint:errcheck
		exec.CommandContext(ctx, "ip", "-6", "neigh", "del", "proxy", d.guestIP6(idx), "dev", d.uplink6).Run() //nolint:errcheck
		exec.CommandContext(ctx, "ip", "-6", "neigh", "add", "proxy", d.guestIP6(idx), "dev", d.uplink6).Run() //nolint:errcheck
	}
	return nil
}

// deleteTap is called by Pause, Destroy and both of Create's failure paths, so
// it has to be idempotent — it is, because every step ignores its error.
func (d *Driver) deleteTap(idx int) {
	if d.jailed() {
		// Helper cleanup is synchronized with the launch-client process exit.
		return
	}
	if d.prefix6 != nil && d.uplink6 != "" {
		exec.Command("ip", "-6", "neigh", "del", "proxy", d.guestIP6(idx), "dev", d.uplink6).Run() //nolint:errcheck
	}
	exec.Command("ip", "link", "del", d.tapName(idx)).Run() //nolint:errcheck
}

// sweepStaleTaps deletes leftover devices from a prior process. Safe at startup
// only — call before any VM exists. It matches tapPrefix and nothing else: the
// firecracker driver runs the same sweep over "sbtap", and neither prefix is a
// prefix of the other, so one driver's construction cannot delete the other's
// live networking.
func sweepStaleTaps() {
	out, err := exec.Command("ip", "-o", "link", "show").Output()
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(out), "\n") {
		// "3: sbqtap1@if4: <BROADCAST,...>" -> field 1 is the name.
		parts := strings.SplitN(line, ": ", 3)
		if len(parts) < 2 {
			continue
		}
		name := parts[1]
		if i := strings.IndexByte(name, '@'); i >= 0 {
			name = name[:i]
		}
		if strings.HasPrefix(name, tapPrefix) {
			exec.Command("ip", "link", "del", name).Run() //nolint:errcheck
		}
	}
}

// defaultRoute6Dev returns the interface backing the IPv6 default route (e.g.
// "enp65s0f0"), or "" if there is none. Used to place proxy-NDP entries for
// guest addresses on the correct uplink.
func defaultRoute6Dev() string {
	out, err := exec.Command("ip", "-6", "route", "show", "default").Output()
	if err != nil {
		return ""
	}
	fields := strings.Fields(string(out))
	for i, f := range fields {
		if f == "dev" && i+1 < len(fields) {
			return fields[i+1]
		}
	}
	return ""
}
