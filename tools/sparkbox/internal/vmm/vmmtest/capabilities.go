package vmmtest

import (
	"context"
	"fmt"
	"io"
	"os"
	"testing"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
)

// testCheckpointRestore covers vmm.Archivable's pack/unpack pair — the archive
// tier, where a sandbox's rootfs leaves the host entirely and comes back.
//
// The interesting half is the ORDER: pack, then destroy, then unpack into a name
// with no disk at all. Anything that quietly relies on the old rootfs still
// being there passes a test that unpacks over the top of it.
func testCheckpointRestore(t *testing.T, newFixture NewFixture) {
	f := newFixture(t)
	ar := requireCap[vmm.Archivable](t, f.Driver)
	ctx := context.Background()

	b := newBox(t, f, uniq(t, "arch"))
	b.create(true)
	c := b.dial()
	run(t, c, "echo checkpointed > $HOME/parity-archive && sync")

	// Pack refuses a running VM: it runs e2fsck and zerofree over the image.
	_, err := ar.PackRootfs(ctx, b.name)
	wantErr(t, err, "PackRootfs of a running sandbox")

	b.pause()
	packCtx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	path, err := ar.PackRootfs(packCtx, b.name)
	if err != nil {
		t.Fatalf("PackRootfs: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("packed artifact %s: %v", path, err)
	}
	if fi.Size() == 0 {
		t.Fatalf("packed artifact %s is empty", path)
	}
	t.Logf("packed %s -> %s (%d bytes)", b.name, path, fi.Size())

	// The artifact must survive Destroy of the VM it came from: the manager
	// uploads it after destroying the hot copy.
	b.destroy()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("Destroy removed the packed artifact: %v", err)
	}
	t.Cleanup(func() { os.Remove(path) }) //nolint:errcheck

	if p, ok := f.Driver.(vmm.RootfsPresencer); ok {
		if present, _ := p.RootfsPresent(b.name); present {
			t.Errorf("RootfsPresent = true after Destroy, before restore")
		}
	}
	unpackCtx, cancel2 := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel2()
	if err := ar.UnpackRootfs(unpackCtx, b.name, path); err != nil {
		t.Fatalf("UnpackRootfs: %v", err)
	}
	// The restored sandbox is one the ledger knows, so it cold-boots rather
	// than being treated as new.
	b.create(false)
	c2 := b.dial()
	if got := run(t, c2, "cat $HOME/parity-archive"); got != "checkpointed" {
		t.Errorf("restored sandbox marker = %q, want %q", got, "checkpointed")
	}
}

// testForkFromTemplate covers the other half of vmm.Archivable: Snapshot mints
// a template, and Create resolves it like any image. That is the fork feature.
//
// Three claims, and the second is the one a careless implementation breaks:
//
//  1. the fork carries the source's files,
//  2. the SOURCE still works afterwards — Snapshot must not mutate the disk it
//     was taken from,
//  3. the fork is not the source: sanitisation replaced the per-guest identity
//     (Traits.SanitizesForks), or two sandboxes share an SSH host key.
func testForkFromTemplate(t *testing.T, newFixture NewFixture) {
	f := newFixture(t)
	ar := requireCap[vmm.Archivable](t, f.Driver)
	ctx := context.Background()

	src := newBox(t, f, uniq(t, "src"))
	src.create(true)
	c := src.dial()
	run(t, c, "echo forked > $HOME/parity-fork && sync")
	srcHostKey := src.lastHostKey()

	wantErr(t, ar.Snapshot(ctx, src.name, f.template("running")), "Snapshot of a running sandbox")

	src.pause()
	image := f.template(uniq(t, "tpl"))
	snapCtx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	if err := ar.Snapshot(snapCtx, src.name, image); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	t.Cleanup(func() { ar.RemoveTemplate(context.Background(), image) }) //nolint:errcheck

	fork := newBox(t, f, uniq(t, "fork"))
	fork.createFrom(image)
	cf := fork.dial()
	if got := run(t, cf, "cat $HOME/parity-fork"); got != "forked" {
		t.Errorf("fork marker = %q, want %q", got, "forked")
	}
	// The fork must be its own sandbox, not a copy that still believes it is
	// the source.
	if f.Traits.RealGuest {
		if got := run(t, cf, "cat /proc/sys/kernel/hostname"); got != fork.name {
			t.Errorf("fork hostname = %q, want %q", got, fork.name)
		}
	}
	if f.Traits.SanitizesForks {
		if fk := fork.lastHostKey(); fk != "" && fk == srcHostKey {
			t.Errorf("fork presents the source's SSH host key (%s): identity was not stripped", fk)
		}
	}

	// And the source survives being snapshotted.
	src.resume()
	cs := src.dial()
	if got := run(t, cs, "cat $HOME/parity-fork"); got != "forked" {
		t.Errorf("source sandbox after Snapshot: marker = %q", got)
	}

	// A template that was removed must stop resolving, or a deleted snapshot
	// keeps costing disk forever.
	if err := ar.RemoveTemplate(ctx, image); err != nil {
		t.Fatalf("RemoveTemplate: %v", err)
	}
	if err := ar.RemoveTemplate(ctx, image); err != nil {
		t.Errorf("RemoveTemplate of a missing template must not be an error: %v", err)
	}
}

// testRootfsPresence pins the guard that stops a lost hot tier being silently
// re-created from the base image. Absence must mean "restore required", so the
// answer has to be false when there is genuinely no disk and true when there is.
func testRootfsPresence(t *testing.T, newFixture NewFixture) {
	f := newFixture(t)
	p := requireCap[vmm.RootfsPresencer](t, f.Driver)

	name := uniq(t, "pres")
	present, err := p.RootfsPresent(name)
	if err != nil {
		t.Fatalf("RootfsPresent before create: %v", err)
	}
	if present {
		t.Fatalf("RootfsPresent = true for a name that has never existed")
	}
	b := newBox(t, f, name)
	b.create(true)
	if present, err := p.RootfsPresent(name); err != nil || !present {
		t.Fatalf("RootfsPresent after create = %v (err %v), want true", present, err)
	}
	b.pause()
	if present, err := p.RootfsPresent(name); err != nil || !present {
		t.Fatalf("RootfsPresent while paused = %v (err %v), want true: a paused sandbox still has its disk", present, err)
	}
}

// testReboot covers vmm.Rebooter, which is how Manager.Reboot restarts a guest:
// pause, drop the memory snapshot, start again. The point of the capability is
// that the next start is a COLD BOOT of the preserved rootfs, and on a real
// guest that is checkable rather than assumed — the boot id changes and tmpfs
// is empty, while the disk is untouched.
func testReboot(t *testing.T, newFixture NewFixture) {
	f := newFixture(t)
	rb := requireCap[vmm.Rebooter](t, f.Driver)

	b := newBox(t, f, uniq(t, "reboot"))
	b.create(true)
	c := b.dial()
	run(t, c, "echo survives > $HOME/parity-reboot && sync")
	var bootID string
	if f.Traits.RealGuest {
		bootID = run(t, c, "cat /proc/sys/kernel/random/boot_id")
		run(t, c, "echo ram > /dev/shm/parity-ram")
	}

	// Dropping a running VM's snapshot must be refused: the manager pauses
	// first, and doing it under a live guest would leave an unresumable record.
	wantErr(t, rb.DropSnapshots(b.name), "DropSnapshots of a running sandbox")

	b.pause()
	if err := rb.DropSnapshots(b.name); err != nil {
		t.Fatalf("DropSnapshots: %v", err)
	}
	b.create(false)
	c2 := b.dial()
	if got := run(t, c2, "cat $HOME/parity-reboot"); got != "survives" {
		t.Errorf("after reboot, disk marker = %q, want %q", got, "survives")
	}
	if !f.Traits.RealGuest {
		return
	}
	if got := run(t, c2, "cat /proc/sys/kernel/random/boot_id"); got == bootID {
		t.Errorf("boot id unchanged (%q) after DropSnapshots + start: the guest resumed, it did not reboot", got)
	}
	if _, err := tryRun(c2, "test -e /dev/shm/parity-ram"); err == nil {
		t.Errorf("tmpfs marker survived a reboot: memory was restored when it should not have been")
	}
}

// testRename covers vmm.Renamer. The contract has a precondition that is easy
// to implement and easy to forget: a memory snapshot embeds absolute paths into
// the old VM directory, so renaming with one present produces a sandbox that
// cannot be resumed. The driver must refuse rather than produce it.
func testRename(t *testing.T, newFixture NewFixture) {
	f := newFixture(t)
	rn := requireCap[vmm.Renamer](t, f.Driver)
	rb := requireCap[vmm.Rebooter](t, f.Driver)

	oldBox := newBox(t, f, uniq(t, "old"))
	newName := uniq(t, "new")
	newBoxRec := newBox(t, f, newName) // registers teardown for the destination
	oldBox.create(true)
	c := oldBox.dial()
	run(t, c, "echo renamed > $HOME/parity-rename && sync")

	wantErr(t, rn.RenameVM(oldBox.name, newName), "RenameVM of a running sandbox")
	oldBox.pause()
	if f.Traits.PreservesMemory {
		// Only meaningful on a driver that has a memory snapshot to embed
		// stale paths in; a driver that keeps none cannot have one present.
		wantErr(t, rn.RenameVM(oldBox.name, newName), "RenameVM with a memory snapshot present")
	}

	if err := rb.DropSnapshots(oldBox.name); err != nil {
		t.Fatalf("DropSnapshots: %v", err)
	}
	if err := rn.RenameVM(oldBox.name, newName); err != nil {
		t.Fatalf("RenameVM: %v", err)
	}
	newBoxRec.create(false)
	c2 := newBoxRec.dial()
	if got := run(t, c2, "cat $HOME/parity-rename"); got != "renamed" {
		t.Errorf("renamed sandbox marker = %q, want %q", got, "renamed")
	}
	if f.Traits.RealGuest {
		if got := run(t, c2, "cat /proc/sys/kernel/hostname"); got != newName {
			t.Errorf("renamed guest still calls itself %q, want %q", got, newName)
		}
	}
	// The old name must be gone, not merely unreferenced.
	if p, ok := f.Driver.(vmm.RootfsPresencer); ok {
		if present, _ := p.RootfsPresent(oldBox.name); present {
			t.Errorf("RootfsPresent(%q) = true after rename: the old disk was copied, not moved", oldBox.name)
		}
	}
}

// testDiskReport covers vmm.DiskReporter, which feeds pooled per-owner
// accounting and the console's disk meter. The contract is specific about what
// the two numbers mean: usage is what the guest's own filesystem says it has
// spent, capacity is the ceiling it cannot grow past.
func testDiskReport(t *testing.T, newFixture NewFixture) {
	f := newFixture(t)
	dr := requireCap[vmm.DiskReporter](t, f.Driver)
	ctx := context.Background()

	b := newBox(t, f, uniq(t, "disk"))
	b.create(true)
	c := b.dial()

	used, err := dr.DiskUsageMB(ctx, b.name)
	if err != nil {
		t.Fatalf("DiskUsageMB: %v", err)
	}
	capacity, err := dr.DiskCapacityMB(ctx, b.name)
	if err != nil {
		t.Fatalf("DiskCapacityMB: %v", err)
	}
	t.Logf("disk: used %d MiB of %d MiB", used, capacity)
	if used < 0 {
		t.Errorf("DiskUsageMB = %d", used)
	}
	if f.Traits.RealGuest {
		if capacity <= 0 {
			t.Errorf("DiskCapacityMB = %d for a real guest", capacity)
		}
		if used > capacity {
			t.Errorf("used %d MiB exceeds capacity %d MiB", used, capacity)
		}
	}

	// The number a user sees has to be the number the guest would report. Read
	// both and print the gap, whatever the driver claims — a meter that is
	// merely stale looks identical to a correct one until they are compared.
	if f.Traits.RealGuest {
		guestUsed := run(t, c, "df -Pm / | awk 'NR==2 {print $3}'")
		t.Logf("guest df says %s MiB used; driver says %d MiB", guestUsed, used)
	}

	const writeMB = 64
	run(t, c, fmt.Sprintf("dd if=/dev/zero of=$HOME/parity-fill bs=1M count=%d status=none && sync", writeMB))
	if f.Traits.RealGuest {
		t.Logf("after writing %d MiB, guest df says %s MiB used",
			writeMB, run(t, c, "df -Pm / | awk 'NR==2 {print $3}'"))
	}
	// Pause so the guest has flushed and the on-disk superblock is quiesced:
	// reading a live ext4's counters is a race, not a measurement.
	b.pause()
	after, err := dr.DiskUsageMB(ctx, b.name)
	if err != nil {
		t.Fatalf("DiskUsageMB after write: %v", err)
	}
	t.Logf("driver disk usage after writing %d MiB: %d MiB (was %d)", writeMB, after, used)

	if !f.Traits.LiveDiskUsage {
		// Not a skip and not a pass: the driver declared it does not track a
		// running guest, and this is the run that says by how much.
		t.Logf("driver does not claim LiveDiskUsage — its reading moved %d MiB "+
			"for a %d MiB write. See docs/vmm-parity-harness.md.", after-used, writeMB)
		return
	}
	if after < used+writeMB/2 {
		t.Errorf("DiskUsageMB moved from %d to %d after a %d MiB write: the meter does not track the guest",
			used, after, writeMB)
	}
}

// testTemplateUsage covers vmm.TemplateReporter, whose contract has one clause
// that is the opposite of DiskReporter's and exists for a reason: a MISSING
// template is an error, not zero. Deleting a snapshot does not retroactively
// make its blocks the fork's fault, so callers keep their last baseline instead
// of spiking every fork's pooled charge.
func testTemplateUsage(t *testing.T, newFixture NewFixture) {
	f := newFixture(t)
	tr := requireCap[vmm.TemplateReporter](t, f.Driver)
	ctx := context.Background()

	if f.Traits.BaseImageIsTemplate {
		used, err := tr.TemplateUsageMB(ctx, f.BaseImage)
		if err != nil {
			t.Fatalf("TemplateUsageMB(%q): %v", f.BaseImage, err)
		}
		t.Logf("template %s uses %d MiB", f.BaseImage, used)
		if used <= 0 {
			t.Errorf("TemplateUsageMB(%q) = %d, want a positive baseline", f.BaseImage, used)
		}
	} else {
		t.Logf("driver does not claim BaseImageIsTemplate; only the missing-template clause runs")
	}
	if _, err := tr.TemplateUsageMB(ctx, f.template("definitely-not-a-template")); err == nil {
		t.Errorf("TemplateUsageMB of a missing template returned no error; " +
			"callers would charge every fork for blocks the deleted template held")
	}
}

// testDiskResize covers vmm.DiskResizer. Grow only, on a stopped VM, with the
// memory snapshot already discarded — all three are in the contract, and all
// three are refusals a driver has to make rather than assume the caller made.
func testDiskResize(t *testing.T, newFixture NewFixture) {
	f := newFixture(t)
	rz := requireCap[vmm.DiskResizer](t, f.Driver)
	dr := requireCap[vmm.DiskReporter](t, f.Driver)
	rb := requireCap[vmm.Rebooter](t, f.Driver)
	ctx := context.Background()

	b := newBox(t, f, uniq(t, "resize"))
	b.create(true)
	b.dial()
	before, err := dr.DiskCapacityMB(ctx, b.name)
	if err != nil {
		t.Fatalf("DiskCapacityMB: %v", err)
	}

	// A live guest has the filesystem metadata cached; rewriting it underneath
	// is how a disk gets destroyed.
	wantErr(t, rz.ResizeDisk(ctx, b.name, before+1024), "ResizeDisk of a running sandbox")

	b.pause()
	if err := rb.DropSnapshots(b.name); err != nil {
		t.Fatalf("DropSnapshots: %v", err)
	}
	// Shrink must be refused outright: it fails if the data does not fit below
	// the new boundary, and a half-completed shrink is a destroyed disk.
	if before > 2 {
		wantErr(t, rz.ResizeDisk(ctx, b.name, before/2), "ResizeDisk shrinking a sandbox")
	}

	want := before + 1024
	resizeCtx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	if err := rz.ResizeDisk(resizeCtx, b.name, want); err != nil {
		t.Fatalf("ResizeDisk %d -> %d MiB: %v", before, want, err)
	}
	after, err := dr.DiskCapacityMB(ctx, b.name)
	if err != nil {
		t.Fatalf("DiskCapacityMB after resize: %v", err)
	}
	t.Logf("capacity %d MiB -> %d MiB (asked for %d)", before, after, want)
	if after <= before {
		t.Fatalf("DiskCapacityMB did not grow: %d -> %d", before, after)
	}

	// The ceiling the guest sees is the one that matters — a host-side resize
	// the guest never learns about buys nothing.
	b.create(false)
	c := b.dial()
	if f.Traits.RealGuest {
		out := run(t, c, "df -Pm / | awk 'NR==2 {print $2}'")
		t.Logf("guest df reports %s MiB total", out)
	}
}

// testBalloon covers vmm.Ballooner, the live-overcommit lever: an idle-but-warm
// sandbox hands most of its RAM back while it keeps running. "While it keeps
// running" is the part worth testing — a balloon that only works on a stopped
// guest is not this capability.
func testBalloon(t *testing.T, newFixture NewFixture) {
	f := newFixture(t)
	bl := requireCap[vmm.Ballooner](t, f.Driver)
	ctx := context.Background()

	b := newBox(t, f, uniq(t, "balloon"))
	b.create(true)
	c := b.dial()

	base, err := bl.BalloonStats(ctx, b.name)
	if err != nil {
		t.Fatalf("BalloonStats: %v", err)
	}
	t.Logf("balloon at rest: target %d actual %d free %d available %d MiB",
		base.TargetMiB, base.ActualMiB, base.FreeMiB, base.AvailableMiB)
	if f.Traits.RealGuest && base.AvailableMiB <= 0 {
		t.Errorf("guest reports %d MiB available: the balloon device has no stats", base.AvailableMiB)
	}

	target := f.MemMB / 4
	if target < 64 {
		target = 64
	}
	if err := bl.SetBalloonTarget(ctx, b.name, target); err != nil {
		t.Fatalf("SetBalloonTarget(%d): %v", target, err)
	}
	// Inflation is asynchronous — the guest hands pages back as it finds them —
	// so poll rather than assert immediately.
	got := pollBalloon(t, bl, b.name, func(s vmm.BalloonStats) bool { return s.ActualMiB >= target*3/4 })
	t.Logf("balloon inflated to %d MiB (asked %d)", got.ActualMiB, target)
	if got.ActualMiB < target*3/4 {
		t.Errorf("balloon reached only %d MiB of a %d MiB target", got.ActualMiB, target)
	}
	// The guest must still be alive and answering while ballooned: that is the
	// whole difference between this and a pause.
	if out := run(t, c, "echo still-here"); out != "still-here" {
		t.Errorf("guest unreachable while ballooned: %q", out)
	}

	if err := bl.SetBalloonTarget(ctx, b.name, 0); err != nil {
		t.Fatalf("SetBalloonTarget(0): %v", err)
	}
	got = pollBalloon(t, bl, b.name, func(s vmm.BalloonStats) bool { return s.ActualMiB == 0 })
	if got.ActualMiB != 0 {
		t.Errorf("balloon did not deflate: actual %d MiB", got.ActualMiB)
	}

	// A paused sandbox has no guest to reclaim from.
	b.pause()
	wantErr(t, bl.SetBalloonTarget(ctx, b.name, target), "SetBalloonTarget on a paused sandbox")
	_, err = bl.BalloonStats(ctx, b.name)
	wantErr(t, err, "BalloonStats of a paused sandbox")
}

func pollBalloon(t *testing.T, bl vmm.Ballooner, name string, ok func(vmm.BalloonStats) bool) vmm.BalloonStats {
	t.Helper()
	var last vmm.BalloonStats
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		s, err := bl.BalloonStats(context.Background(), name)
		if err != nil {
			t.Fatalf("BalloonStats: %v", err)
		}
		last = s
		if ok(s) {
			return s
		}
		time.Sleep(time.Second)
	}
	return last
}

// testCPUStats covers vmm.CPUStatser. The counter follows the VMM process, so
// the only portable claim is that it is cumulative and moves when the guest
// works — which is exactly what the vitals sampler derives utilization from.
func testCPUStats(t *testing.T, newFixture NewFixture) {
	f := newFixture(t)
	cs := requireCap[vmm.CPUStatser](t, f.Driver)
	ctx := context.Background()

	b := newBox(t, f, uniq(t, "cpu"))
	b.create(true)
	c := b.dial()

	first, err := cs.CPUTimeNanos(ctx, b.name)
	if err != nil {
		t.Fatalf("CPUTimeNanos: %v", err)
	}
	if f.Traits.RealGuest {
		// Burn a measurable amount inside the guest, bounded so a wedged
		// session cannot hang the suite.
		run(t, c, "timeout 3 sh -c 'while :; do :; done' || true")
	} else {
		run(t, c, "true")
	}
	second, err := cs.CPUTimeNanos(ctx, b.name)
	if err != nil {
		t.Fatalf("CPUTimeNanos (second): %v", err)
	}
	t.Logf("cpu time %d -> %d ns", first, second)
	if second <= first {
		t.Errorf("CPUTimeNanos did not advance across guest work: %d -> %d", first, second)
	}

	b.pause()
	_, err = cs.CPUTimeNanos(ctx, b.name)
	wantErr(t, err, "CPUTimeNanos of a paused sandbox")
}

// testNetStats covers vmm.NetStatser, whose contract contains a direction swap
// that is easy to get backwards: rx is what the GUEST received, which is the
// mirror image of what the host-side tap reports.
//
// The two directions carry DIFFERENT payload sizes on purpose. An earlier
// version pushed the same 8 MiB each way and compared both against one
// threshold, which meant a driver reporting a single counter for both
// directions — or reporting them swapped — passed green. The asymmetry is the
// whole assertion: the guest sends eight times what it receives, so a reversed
// pair cannot clear both floors.
func testNetStats(t *testing.T, newFixture NewFixture) {
	f := newFixture(t)
	ns := requireCap[vmm.NetStatser](t, f.Driver)
	ctx := context.Background()

	b := newBox(t, f, uniq(t, "net"))
	b.create(true)
	c := b.dial()

	rx0, tx0, err := ns.NetBytes(ctx, b.name)
	if err != nil {
		t.Fatalf("NetBytes: %v", err)
	}
	if !f.Traits.RealGuest {
		t.Logf("net counters readable (rx %d tx %d); traffic assertions need a real guest", rx0, tx0)
		return
	}
	const (
		uploadMB   = 4  // host -> guest: the guest RECEIVES this, so it lands in rx
		downloadMB = 32 // guest -> host: the guest SENDS this, so it lands in tx
	)
	sess, err := c.NewSession()
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	sess.Stdin = io.LimitReader(zeroSource{}, uploadMB<<20)
	if err := sess.Run("cat > /dev/null"); err != nil {
		t.Fatalf("upload into guest: %v", err)
	}
	out, err := c.NewSession()
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	if _, err := out.Output(fmt.Sprintf("dd if=/dev/zero bs=1M count=%d status=none", downloadMB)); err != nil {
		t.Fatalf("download from guest: %v", err)
	}

	rx1, tx1, err := ns.NetBytes(ctx, b.name)
	if err != nil {
		t.Fatalf("NetBytes after traffic: %v", err)
	}
	t.Logf("net rx %d -> %d, tx %d -> %d bytes", rx0, rx1, tx0, tx1)
	// Half of each payload, to leave room for framing and for the acks the
	// opposite direction carries.
	if grew := rx1 - rx0; grew < uint64(uploadMB)<<19 {
		t.Errorf("guest rx grew by %d bytes after receiving %d MiB", grew, uploadMB)
	}
	if grew := tx1 - tx0; grew < uint64(downloadMB)<<19 {
		t.Errorf("guest tx grew by %d bytes after sending %d MiB", grew, downloadMB)
	}
	// The payloads differ 8x, so this is what a swapped or single-source pair
	// fails on even if it somehow cleared both floors.
	if tx1-tx0 <= rx1-rx0 {
		t.Errorf("guest tx grew by %d and rx by %d after sending %d MiB and receiving %d MiB: the counters look swapped or identical",
			tx1-tx0, rx1-rx0, downloadMB, uploadMB)
	}

	b.pause()
	_, _, err = ns.NetBytes(ctx, b.name)
	wantErr(t, err, "NetBytes of a paused sandbox")
}

// zeroSource is an endless run of zero bytes; the suite wraps it in an
// io.LimitReader to push a payload of a chosen size without allocating it.
type zeroSource struct{}

func (zeroSource) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}
