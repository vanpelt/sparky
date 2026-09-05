//go:build linux

package qemu

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
)

// This file is the half of the capability surface that needs a live VM:
// CPUStatser and NetStatser, which ask the host kernel about the VMM process
// and its tap, and Ballooner, which is the only capability here that talks to
// QEMU at all.
//
// All four methods hold d.mu for their whole duration and all four must ERROR
// on a paused sandbox — the parity suite checks that polarity in both
// directions, and DiskUsageMB/DiskCapacityMB in caps_files.go are the ones that
// must NOT. st.cmd != nil is the liveness predicate; stopVMM clears st.cmd and
// st.qmp together once the child is reaped, so a paused record has neither.

const userHZ = 100

// CPUTimeNanos implements vmm.CPUStatser: cumulative utime+stime of the QEMU
// process from /proc/<pid>/stat. This measures the whole VMM process (vCPU
// threads + emulation and I/O overhead), so surface it to users as "host CPU",
// not guest CPU.
//
// This lifts from the firecracker driver unchanged apart from where the pid
// comes from. Firecracker's SDK owns the child and hands it over via
// machine.PID(); here the driver owns the exec.Cmd directly, so the pid is
// st.cmd.Process.Pid. There is deliberately no privileged-helper branch: this
// driver is the direct launcher only (see Options).
//
// QMP has no equivalent query — QEMU will not tell you its own pid — so /proc
// is not a shortcut, it is the only source.
func (d *Driver) CPUTimeNanos(_ context.Context, name string) (uint64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	st, ok := d.vms[name]
	if !ok || st.cmd == nil {
		return 0, fmt.Errorf("vm %q not running", name)
	}
	if st.cmd.Process == nil {
		return 0, fmt.Errorf("vm %q has no VMM process", name)
	}
	pid := st.cmd.Process.Pid
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		// A QEMU that died on its own leaves the record in place until
		// something calls Pause or Destroy, so /proc is gone while st.cmd is
		// not. Say which pid vanished rather than surfacing a bare ENOENT.
		return 0, fmt.Errorf("vm %q cpu time (pid %d): %w", name, pid, err)
	}
	ticks, err := procStatCPUTicks(string(data))
	if err != nil {
		return 0, err
	}
	return ticks * (1_000_000_000 / userHZ), nil
}

// procStatCPUTicks sums the utime and stime fields (14 and 15) of a
// /proc/<pid>/stat line. The comm field may itself contain spaces and ')',
// so fields are counted from the last ')' rather than split naively.
func procStatCPUTicks(stat string) (uint64, error) {
	i := strings.LastIndexByte(stat, ')')
	if i < 0 {
		return 0, fmt.Errorf("malformed stat line %q", stat)
	}
	fields := strings.Fields(stat[i+1:])
	// fields[0] is field 3 (state), so utime/stime land at indices 11/12.
	if len(fields) < 13 {
		return 0, fmt.Errorf("stat line has %d fields after comm, want >= 13", len(fields))
	}
	utime, err := strconv.ParseUint(fields[11], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("utime: %w", err)
	}
	stime, err := strconv.ParseUint(fields[12], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("stime: %w", err)
	}
	return utime + stime, nil
}

// NetBytes implements vmm.NetStatser from the host tap's byte counters, which
// is a pure host-kernel read: the VMM is not consulted, and QEMU's own
// query-netdev has nothing comparable to offer.
//
// Directions are swapped on the way out: the tap's rx is traffic the *guest*
// transmitted, its tx traffic the guest received. That swap is the whole
// content of the vmm.NetStatser contract, so do not add a second one — an
// implementation that reads a guest-oriented counter somewhere else must NOT
// swap, and one that reads the tap must.
//
// The counters are owned by the tap device, which createTap/deleteTap cycle on
// every pause/resume, so they restart at zero far more often than the CPU
// counter — callers must treat a decrease as a reset.
func (d *Driver) NetBytes(_ context.Context, name string) (rx, tx uint64, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	st, ok := d.vms[name]
	if !ok || st.cmd == nil {
		return 0, 0, fmt.Errorf("vm %q not running", name)
	}
	tap := tapName(st.idx)
	// Guest rx is the tap's tx and vice versa.
	if rx, err = readTapCounter(tap, "tx_bytes"); err != nil {
		return 0, 0, err
	}
	if tx, err = readTapCounter(tap, "rx_bytes"); err != nil {
		return 0, 0, err
	}
	return rx, tx, nil
}

// readTapCounter reads one of a tap device's sysfs byte counters.
func readTapCounter(tap, stat string) (uint64, error) {
	data, err := os.ReadFile(fmt.Sprintf("/sys/class/net/%s/statistics/%s", tap, stat))
	if err != nil {
		return 0, err
	}
	n, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s/%s: %w", tap, stat, err)
	}
	return n, nil
}

// SetBalloonTarget inflates (or deflates, at 0) the named VM's balloon to
// reclaim targetMiB of guest RAM to the host. Implements vmm.Ballooner.
//
// THE UNITS ARE INVERTED, and this is the single most dangerous conversion in
// the package. vmm.Ballooner speaks in RAM RECLAIMED FROM the guest, MiB, with
// 0 meaning "fully deflated, the guest has everything". QEMU's `balloon`
// command takes the guest's REMAINING VISIBLE RAM, in bytes. So the value we
// send is the complement:
//
//	balloon value = (st.memMB - clamp(targetMiB, 0, st.memMB-1)) << 20
//
// The upper end of that clamp is st.memMB-1, not st.memMB: QEMU refuses a
// target of zero, so "give the host everything" has to mean one MiB short of
// everything. See the clamp below.
//
// Forwarding targetMiB straight through as bytes would set an 8 GiB guest's RAM
// to 256 MiB when the reaper asked to reclaim 256 MiB. Nothing on the host side
// errors on that — QEMU accepts it, the balloon inflates, and the guest starts
// OOM-killing. The measurement behind this is docs/qemu-spike.md: a -m 1024
// guest reports query-balloon actual 1073741824 at rest, exactly 1024 MiB.
//
// The baseline is st.memMB, the configured -m value, and never the guest's
// stat-total-memory (see BalloonStats).
func (d *Driver) SetBalloonTarget(ctx context.Context, name string, targetMiB int64) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	st, ok := d.vms[name]
	if !ok || st.cmd == nil || st.qmp == nil {
		return fmt.Errorf("vm %q not running", name)
	}
	if targetMiB < 0 {
		targetMiB = 0
	}
	// Asking for the guest's whole allocation (or more) is a caller bug, and
	// this clamp is the only thing between it and the guest. It leaves ONE MiB
	// visible rather than clamping to st.memMB, because the complement of
	// st.memMB is zero and `balloon` with value 0 is not "reclaim everything" —
	// qmp_balloon rejects a non-positive target outright, so the caller would
	// get "Parameter 'target' expects a size", an error naming nothing it
	// passed, instead of the near-total inflation this clamp promises.
	// host.Manager never gets here (workingSetFloor keeps its target below
	// MemMB), which is exactly why the path has to behave sanely unattended.
	if targetMiB >= st.memMB {
		targetMiB = st.memMB - 1
	}
	visibleMiB := st.memMB - targetMiB
	return st.qmp.SetBalloonBytes(ctx, uint64(visibleMiB)<<20)
}

// BalloonStats reports the guest's current memory picture. Implements
// vmm.Ballooner. Errors if the VM isn't running, if the guest has no balloon
// device, or if the guest has not yet published a stats sample.
//
// Two QMP reads, and they differ in freshness. The caller cannot tell them
// apart from the returned struct, so:
//
//   - ActualMiB (and therefore TargetMiB) comes from query-balloon, which reads
//     the device's own accounting synchronously. It is current as of this call,
//     which is what makes polling it after SetBalloonTarget a meaningful way to
//     watch an inflation land.
//   - FreeMiB and AvailableMiB come from the guest, via
//     qom-get guest-stats. The guest pushes a sample every
//     balloonStatsIntervalSecs (1s) and qom-get returns the LAST one it pushed,
//     so these two fields are up to about a second stale in the healthy case —
//     and arbitrarily stale in the unhealthy one, because a guest that has
//     stopped updating (frozen, balloon driver unloaded, kernel wedged) keeps
//     returning its final sample forever with no error. The only staleness
//     signal QEMU offers is the sample's `last-update` timestamp, which qmpConn
//     consumes; if this driver ever needs to distinguish "idle" from "wedged",
//     that timestamp is where it has to come from.
//
// A missing sample is an ERROR, not a well-formed struct of zeros, and that
// choice is load-bearing rather than fastidious. host.Manager.MemStats computes
//
//	used = memMB - ActualMiB - (AvailableMiB or FreeMiB)
//
// so zeroed stats report every sandbox as using its entire configured ceiling.
// The reaper then sees a fleet-wide overage that does not exist and balloons
// innocent sandboxes to relieve it. An error, by contrast, makes MemStats
// return ok=false and the caller keeps its last real reading — the failure mode
// is a stale number instead of a destructive one. (This is exactly the choice
// Cloud Hypervisor could not offer at all, and one of the reasons QEMU is the
// second backend: docs/qemu-spike.md, docs/vmm-choice.md.)
//
// The first sample lands one polling interval after boot enables it. boot
// re-issues the qom-set after EVERY successful start, restore included, because
// guest-stats-polling-interval is a QOM property and not part of
// virtio-balloon's migrated device state — so the blind window is ~1s from the
// start of a boot that takes 2.3s to reach SSH, and ~1s after a resume. This
// method deliberately does not wait it out: it holds the driver-wide mutex, and
// sleeping in here would serialize every Create on the host behind a guest that
// may never answer. On a restore that re-issue is allowed to fail — losing a
// guest's restored RAM over a stats property is the worse trade — and st.statsErr
// is how that arrives here as the same "no sample" error rather than as a
// pre-pause reading with no way to tell.
func (d *Driver) BalloonStats(ctx context.Context, name string) (vmm.BalloonStats, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	st, ok := d.vms[name]
	if !ok || st.cmd == nil || st.qmp == nil {
		return vmm.BalloonStats{}, fmt.Errorf("vm %q not running", name)
	}
	// A restore whose guest-stats property would not go back on. qom-get still
	// answers — with the sample the guest pushed before it was paused, complete
	// with a plausible last-update — so the only way not to serve stale numbers
	// as fresh ones is to remember that boot could not re-arm the refresh.
	if st.statsErr != nil {
		return vmm.BalloonStats{}, fmt.Errorf("vm %q balloon stats: %w (%v)", name, errBalloonNoSample, st.statsErr)
	}

	actualBytes, err := st.qmp.BalloonActualBytes(ctx)
	if err != nil {
		return vmm.BalloonStats{}, fmt.Errorf("vm %q balloon: %w", name, err)
	}
	free, available, err := st.qmp.GuestStats(ctx)
	if err != nil {
		// No sample, or a sample of QEMU's 0xFFFFFFFFFFFFFFFF "unset"
		// sentinels. Fail rather than report zeros; see above.
		return vmm.BalloonStats{}, fmt.Errorf("vm %q balloon stats: %w", name, err)
	}

	// Round actual to the nearest MiB rather than truncating. The balloon moves
	// whole pages, so a fully deflated guest reports actual == ram_size exactly
	// and either rounding gives 0 — but a device that ever settles one page
	// short would, under truncation, report 1 MiB held forever, and both the
	// parity suite's deflate assertion and the reaper's arithmetic want the
	// honest 0.
	actualMiB := st.memMB - int64((actualBytes+(1<<19))>>20)
	if actualMiB < 0 {
		actualMiB = 0
	}
	if actualMiB > st.memMB {
		actualMiB = st.memMB
	}

	return vmm.BalloonStats{
		// QEMU exposes no way to read back the balloon's requested target, so
		// TargetMiB mirrors ActualMiB. The firecracker driver does the same
		// thing for the same reason (fc.go:1119) — keeping the two drivers
		// identically wrong here is better than inventing driver-side
		// bookkeeping that only one of them has.
		TargetMiB:    actualMiB,
		ActualMiB:    actualMiB,
		FreeMiB:      int64(free >> 20),
		AvailableMiB: int64(available >> 20),
	}, nil
}
