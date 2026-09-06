//go:build linux

package qemu

// The host-side half of the capability surface: Archivable, DiskReporter,
// TemplateReporter, RootfsPresencer, Renamer and DiskResizer.
//
// Almost none of this is QEMU. It is cp --reflink, loop mounts, e2fsck,
// zerofree, zstd, a passive ext4 superblock read and os.Rename — which is why
// it is lifted from internal/vmm/firecracker/fc.go rather than rewritten, with
// the comments intact. Those comments record measurements and fixed bugs (why
// reflinkClone trims cp's trailing newline, why ensureGuestDir re-chmods ~/.ssh
// alone, why templatePath resolves ImageDir before TemplateDir, why the two
// e2fsck exit-code thresholds differ), and a copy stripped of them is the
// version somebody later "simplifies" back into the bug.
//
// TWO THINGS ARE NOT LIFTED, both because QEMU's snapshot is ONE file where
// Firecracker's is a mem.snap/state.snap pair:
//
//   - RenameVM's refusal predicate. fc.go:1612 stats that literal pair. Under
//     QEMU neither name ever exists, so the stat matches nothing and the
//     refusal silently stops refusing the renames it exists to refuse. It goes
//     through d.hasSnapshot instead.
//   - PackRootfs's snapshot drop. Same cause, opposite symptom: the two
//     os.Removes at fc.go:1191-1192 name files that are not there, the real
//     snapshot survives the pack, and the disk pausing spent is never
//     reclaimed. It goes through d.snapshotFiles instead.
//
// (A third VMM can spring an ENOTEMPTY variant of the same trap: if its
// snapshot is a *directory*, os.Remove fails on it and only os.RemoveAll works.
// QEMU's state.migrate is a plain regular file, so os.Remove is correct here;
// the trap is the naming, not the removal call.)
//
// Every snapshot path in this file comes from lifecycle.go's snapshotPath /
// snapshotNextPath / snapshotFiles / hasSnapshot helpers. No file outside
// lifecycle.go spells a snapshot filename, which is what makes a future second
// snapshot file a one-line change rather than a fifth silent regression.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm/guestdisk"
)

// --- Archivable + DiskReporter: the disk-lifecycle capabilities ------------

// stoppedRootfs returns name's rootfs path, refusing a *running* VM: archive and
// snapshot run e2fsck/zerofree/mount against the ext4, which would corrupt a
// live guest's disk. The manager pauses before calling, so a paused (cmd ==
// nil) or post-restart (no driver entry) VM is fine.
//
// It releases d.mu before returning, deliberately: PackRootfs, Snapshot and
// ResizeDisk then run multi-second e2fsck/zerofree/zstd work with no lock held.
// Nothing stops a concurrent Create from booting the VM mid-compact, and
// closing that TOCTOU by holding the driver-wide mutex through an archive would
// serialize the whole fleet behind it.
func (d *Driver) stoppedRootfs(name string) (string, error) {
	d.mu.Lock()
	st, ok := d.vms[name]
	running := ok && st.cmd != nil
	d.mu.Unlock()
	if running {
		return "", fmt.Errorf("vm %q is running; pause it first", name)
	}
	rootfs := d.rootfsPath(name)
	if _, err := os.Stat(rootfs); err != nil {
		return "", fmt.Errorf("no rootfs for %q: %w", name, err)
	}
	return rootfs, nil
}

// PackRootfs implements vmm.Archivable: compact the stopped VM's rootfs, drop
// its memory snapshot, and zstd it into a sibling of the VM dir (so Destroy(dir)
// won't clobber it before the manager uploads).
func (d *Driver) PackRootfs(ctx context.Context, name string) (string, error) {
	rootfs, err := d.stoppedRootfs(name)
	if err != nil {
		return "", err
	}
	if err := guestdisk.Compact(ctx, rootfs); err != nil {
		return "", err
	}
	// Archive is a cold restore, so the memory snapshot is dead weight —
	// dropping it is exactly the disk pausing spent that we now reclaim.
	//
	// Via snapshotFiles, never a literal name: fc.go removes "mem.snap" and
	// "state.snap" here, and lifting those two lines would leave QEMU's
	// state.migrate sitting in the dir with nothing removed and nothing said.
	// The transient .next goes too — a Pause that died mid-migration leaves a
	// full-size partial memory image behind, and this is the one verb whose
	// entire job is reclaiming that space.
	for _, f := range append(d.snapshotFiles(name), d.snapshotNextPath(name)) {
		os.Remove(f) //nolint:errcheck
	}
	out := d.vmDir(name) + packSuffix
	if o, err := exec.CommandContext(ctx, "zstd", "-T0", "-10", "-f", rootfs, "-o", out).CombinedOutput(); err != nil {
		return "", fmt.Errorf("compress rootfs: %v: %s", err, o)
	}
	return out, nil
}

// UnpackRootfs implements vmm.Archivable: decompress a packed artifact into
// name's VM dir so the next Create cold-boots it (Create skips its reflink copy
// when rootfs.ext4 already exists).
func (d *Driver) UnpackRootfs(ctx context.Context, name, inPath string) error {
	d.mu.Lock()
	st, ok := d.vms[name]
	running := ok && st.cmd != nil
	d.mu.Unlock()
	if running {
		return fmt.Errorf("vm %q is running", name)
	}
	dir := d.vmDir(name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	rootfs := d.rootfsPath(name)
	tmp, err := os.CreateTemp(dir, ".rootfs-restoring-*.ext4")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath) //nolint:errcheck
		return err
	}
	os.Remove(tmpPath)       //nolint:errcheck // zstd creates its output itself.
	defer os.Remove(tmpPath) //nolint:errcheck
	if o, err := exec.CommandContext(ctx, "zstd", "-d", "-f", "--sparse", inPath, "-o", tmpPath).CombinedOutput(); err != nil {
		return fmt.Errorf("decompress rootfs: %v: %s", err, o)
	}
	// Rename on the VM-state filesystem atomically replaces the old rootfs only
	// after zstd has validated and fully decompressed the checkpoint.
	if err := os.Rename(tmpPath, rootfs); err != nil {
		return fmt.Errorf("install restored rootfs: %w", err)
	}
	return nil
}

// RootfsPresent reports whether the sandbox's hot disk exists. The manager
// uses it to refuse a base-image recreate when durable checkpoint metadata says
// a missing disk must be restored instead.
func (d *Driver) RootfsPresent(name string) (bool, error) {
	_, err := os.Stat(d.rootfsPath(name))
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

// Snapshot implements vmm.Archivable: promote the stopped VM's rootfs into a new
// reusable ImageDir template. Reflink-copies first (never mutates the source
// VM's disk), sanitizes per-guest identity, compacts, then renames into place.
//
// Nothing here touches the memory snapshot either, which is what lets the
// source resume with its RAM intact afterwards — the parity suite's
// ForkFromTemplate case resumes the source after the fork and reads its marker
// back, so a Snapshot that promoted the live state.migrate rather than a clone
// of the disk would fail there and nowhere else.
func (d *Driver) Snapshot(ctx context.Context, name, newImage string) error {
	if !guestdisk.ValidImageName(newImage) {
		return fmt.Errorf("invalid snapshot image name %q", newImage)
	}
	rootfs, err := d.stoppedRootfs(name)
	if err != nil {
		return err
	}
	if d.opts.ImageDir == "" {
		return fmt.Errorf("no image dir configured; cannot snapshot")
	}
	out := d.captureDir()
	tmp := filepath.Join(out, "."+newImage+".ext4.tmp")
	final := filepath.Join(out, newImage+".ext4")
	os.Remove(tmp) //nolint:errcheck // clear any torn prior attempt
	if err := guestdisk.Clone(ctx, rootfs, tmp); err != nil {
		return err
	}
	// Belt-and-braces, and only where the belt exists.
	//
	// Every per-guest identity this removes is already cleared inside the guest
	// by the pre-pack hook. sparkbox-identity-reset remains the boot-time backstop
	// for SSH keys and old templates, and the blank machine id makes PID 1 honor
	// systemd.machine_id= before journald starts. So a template is safe to boot
	// whether or not this host-side pass ran — which is what lets a host that
	// cannot mount capture one at all.
	//
	// It still runs where mounting is allowed, because a template that is also
	// clean AT REST is worth the two seconds: it means the bytes a fork copies
	// do not contain the parent's host keys even briefly, and it keeps the
	// standalone path working unchanged for images whose guest payload predates
	// the reset hook. Where mounting is refused (CKS, --disable-host-rootfs-
	// mounts) the capture is a reflink of opaque bytes plus e2fsck and zerofree,
	// none of which parse a guest-authored tree in the host kernel.
	//
	// See docs/cks-snapshot-design.md.
	if !d.opts.DisableHostRootfsMounts {
		if err := guestdisk.Sanitize(ctx, tmp); err != nil {
			os.Remove(tmp) //nolint:errcheck
			return err
		}
	}
	if err := guestdisk.Compact(ctx, tmp); err != nil {
		os.Remove(tmp) //nolint:errcheck
		return err
	}
	if err := os.Rename(tmp, final); err != nil {
		os.Remove(tmp) //nolint:errcheck
		return err
	}
	// Login-user sidecar (build-rootfs.sh writes the same file next to templates
	// it builds) so the gateway logs into forks as the right account.
	user := d.opts.LoginUser
	if user == "" {
		user = "root"
	}
	os.WriteFile(final+loginUserSuffix, []byte(user+"\n"), 0o644) //nolint:errcheck
	return nil
}

// captureDir is where a new template is written. See Options.TemplateDir.
func (d *Driver) captureDir() string {
	if d.opts.TemplateDir != "" {
		return d.opts.TemplateDir
	}
	return d.opts.ImageDir
}

// templatePath resolves an image name to the file backing it, looking in the
// operator's ImageDir FIRST and only then in the writable capture dir.
//
// The order is the security half. A capture dir is writable by this process; an
// ImageDir on a hardened node is not. Resolving the writable one first would let
// anything able to write a file there shadow `universal` — the trusted base
// every fresh sandbox boots from — which is precisely the substitution the
// read-only mount exists to prevent. Names cannot collide in practice either
// (a capture is always `snap-<owner>-<name>`), so this ordering costs nothing
// and closes the case where that stops being true.
func (d *Driver) templatePath(image string) string {
	base := filepath.Join(d.opts.ImageDir, image+".ext4")
	if d.opts.TemplateDir == "" {
		return base
	}
	if _, err := os.Stat(base); err == nil {
		return base
	}
	return filepath.Join(d.opts.TemplateDir, image+".ext4")
}

// RemoveTemplate implements vmm.Archivable: delete a snapshot template + sidecar.
// A missing template is not an error — the parity suite calls this twice and
// asserts the second call succeeds — so ENOENT is swallowed rather than wrapped.
func (d *Driver) RemoveTemplate(_ context.Context, image string) error {
	if !guestdisk.ValidImageName(image) {
		return fmt.Errorf("invalid template name %q", image)
	}
	if d.opts.ImageDir == "" {
		return nil
	}
	// captureDir, not templatePath: this deletes, and the operator's base images
	// are not the control plane's to delete. A name that resolves into ImageDir
	// simply is not found here, which is the right answer for a verb whose only
	// caller is `snapshot rm`.
	final := filepath.Join(d.captureDir(), image+".ext4")
	os.Remove(final + loginUserSuffix) //nolint:errcheck
	if err := os.Remove(final); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// DiskUsageMB implements vmm.DiskReporter: blocks used *inside* the sandbox's
// ext4 filesystem. A missing image (archived / destroyed) is zero, not an
// error, and the read works on a running sandbox as well as a paused one —
// the opposite polarity from CPUTimeNanos, NetBytes and BalloonStats, which
// must all refuse a paused VM.
//
// Host-side `du` is not this measurement. It counts shared reflink extents once
// for every clone, and a decompressor that materializes the template's zeroes
// makes an almost-empty 25 GiB filesystem look 25 GiB full. Reading the ext4
// superblock gives the value users expect beside the filesystem's hard ceiling
// and makes pooled accounting independent of sparse/reflink representation.
//
// FRESHNESS IS NOT PART OF THIS, and this driver does not deliver it either.
// Linux does not write s_free_blocks_count back while a filesystem is mounted,
// so from the moment a guest boots until it is next stopped this reports the
// figure the TEMPLATE had. The parity harness measured it on the firecracker
// driver — a guest wrote 256 MiB and synced, moving its own df by 273 MiB,
// while the driver's reading did not move at all across four minutes of
// repeated syncs and a pause (docs/vmm-parity-harness.md). Nothing about that
// is Firecracker-specific: it is a property of reading the superblock of a
// mounted ext4, and this is the same read of the same field. The QEMU fixture
// must therefore set vmmtest.Traits.LiveDiskUsage FALSE. Setting it true out of
// optimism does not make the number fresher; it just fails the DiskReport case.
//
// Deliberately ignore the memory snapshot: it is a transient host
// implementation detail, not durable sandbox storage, and is discarded on the
// next cold boot.
func (d *Driver) DiskUsageMB(_ context.Context, name string) (int64, error) {
	rootfs := d.rootfsPath(name)
	if _, err := os.Stat(rootfs); err != nil {
		return 0, nil
	}
	used, _, err := guestdisk.DiskMB(rootfs)
	return used, err
}

// DiskCapacityMB implements vmm.DiskReporter: the guest's hard ceiling, read
// from the same superblock as DiskUsageMB so the two agree — the space `df`
// inside the guest calls the filesystem total, which is the image minus the
// metadata overhead the guest can never spend. Discovered per sandbox rather
// than configured, so boxes created from differently-sized templates each
// report their own ceiling. 0 when the image is missing; an image that is
// present but unreadable is an error, as it is for DiskUsageMB.
func (d *Driver) DiskCapacityMB(_ context.Context, name string) (int64, error) {
	rootfs := d.rootfsPath(name)
	if _, err := os.Stat(rootfs); err != nil {
		return 0, nil
	}
	_, capacity, err := guestdisk.DiskMB(rootfs)
	return capacity, err
}

// TemplateUsageMB implements vmm.TemplateReporter: the used blocks of a base or
// snapshot template in ImageDir, read through the SAME ext4DiskMB DiskUsageMB
// uses. That identity is the whole point — the two figures come off the same
// superblock fields on the same basis, so the manager can subtract one from the
// other and get the blocks a fork actually wrote.
//
// A template is a better subject for this passive read than a live rootfs:
// nothing writes one in place. Snapshot stages under a dotted .tmp name and
// renames, and deploy/refresh-agent-tools.sh patches base images by
// reflink/mount/atomic-rename, so the superblock we read is always quiesced.
//
// image arrives from a persisted sandbox record, so re-apply imageNameRe before
// joining it: a record carrying "../../etc/passwd" must not walk out of
// ImageDir. (Create's own template join deliberately does not do this and is
// left alone — tightening it is an unrelated behaviour change.)
//
// A missing template is an error, not zero, per the vmm.TemplateReporter
// contract: the manager keeps its last baseline rather than spiking every
// fork's pooled charge when a snapshot is deleted. That asymmetry with
// DiskUsageMB, which returns zero for a missing rootfs, is deliberate on both
// sides and the parity suite checks it.
func (d *Driver) TemplateUsageMB(_ context.Context, image string) (int64, error) {
	if !guestdisk.ValidImageName(image) {
		return 0, fmt.Errorf("invalid template image name %q", image)
	}
	if d.opts.ImageDir == "" {
		return 0, errors.New("no image dir configured; cannot measure a template")
	}
	used, _, err := guestdisk.DiskMB(d.templatePath(image))
	return used, err
}

// ResizeDisk implements vmm.DiskResizer: grow a stopped sandbox's rootfs to
// sizeMB. The image is a bare ext4 (no partition table), so this is the same
// three steps we run on a template — fsck, extend the file, extend the
// filesystem into it — in the order that keeps the disk consistent if we die
// partway: a truncate that lands without the resize2fs just leaves unused tail
// bytes, whereas the reverse would leave a filesystem larger than its device.
//
// stoppedRootfs is the guard against resizing under a live guest. It is the
// second half of the safety pairing; the first (dropping the memory snapshot)
// belongs to the manager, which is the only layer that knows about snapshots —
// and it matters more here than under Firecracker, because whether QEMU's
// -incoming tolerates a disk that changed size underneath it is the one
// snapshot-safety question docs/qemu-spike.md explicitly did not test.
func (d *Driver) ResizeDisk(ctx context.Context, name string, sizeMB int64) error {
	rootfs, err := d.stoppedRootfs(name)
	if err != nil {
		return err
	}
	fi, err := os.Stat(rootfs)
	if err != nil {
		return err
	}
	want := sizeMB * 1024 * 1024
	if want <= fi.Size() {
		return fmt.Errorf("disk is already %d MB; resize only grows (asked for %d MB)",
			fi.Size()/(1024*1024), sizeMB)
	}
	// -f because the image is unmounted but not necessarily marked clean, and
	// resize2fs refuses a dirty filesystem outright.
	if out, err := exec.CommandContext(ctx, "e2fsck", "-fy", rootfs).CombinedOutput(); err != nil {
		// e2fsck exits 1 when it fixed something, which is success for us; only
		// 4+ (uncorrected errors) is fatal. Note this threshold is >2 while
		// compact's is >=4: they are load-bearing where they are, and copying
		// one over the other changes which repairs each path tolerates.
		if code := guestdisk.ExitCode(err); code > 2 {
			return fmt.Errorf("e2fsck: %v: %s", err, out)
		}
	}
	if err := os.Truncate(rootfs, want); err != nil {
		return fmt.Errorf("truncate: %w", err)
	}
	if out, err := exec.CommandContext(ctx, "resize2fs", rootfs).CombinedOutput(); err != nil {
		return fmt.Errorf("resize2fs: %v: %s", err, out)
	}
	return nil
}

// --- Rebooter --------------------------------------------------------------

// DropSnapshots implements vmm.Rebooter: delete the stopped VM's memory
// snapshot and forget the driver's record of it, so the next start cold-boots
// the preserved rootfs.ext4. Dropping the record is not incidental. A paused
// record with no snapshot behind it is a trap: Resume fails on the missing
// file, and the manager's recreate path then trips Create's already-exists
// check. Forgetting the name leaves the VM in exactly the shape a controller
// restart leaves it in, which is the shape resumeOrRecreate knows how to
// cold-boot — and the delete is also what releases the slot, and with it the
// tap name, the addresses and the MAC, back to freeSlot.
//
// The removal goes through d.snapshotFiles for the same reason RenameVM's
// refusal goes through d.hasSnapshot: fc.go removes the literal pair
// mem.snap/state.snap, neither of which QEMU ever writes, so the lift would be
// a silent no-op that leaves state.migrate in place for the very Resume this
// call exists to prevent. snapshotNextPath goes too — a Pause that died
// mid-migration leaves a full-size partial image, and there is no reader for
// it once the record is gone.
func (d *Driver) DropSnapshots(name string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if st, ok := d.vms[name]; ok && st.cmd != nil {
		return fmt.Errorf("vm %q is running; pause it first", name)
	}
	if err := d.removeSnapshotFiles(name); err != nil {
		return err
	}
	delete(d.vms, name)
	d.slots.Release(name)
	return nil
}

// --- Renamer ---------------------------------------------------------------

// RenameVM implements vmm.Renamer: move the stopped VM's state dir to the new
// name. Refuses while a memory snapshot exists, and the refusal is the point:
// the rename drops this driver's record of the old name (releasing its network
// slot), and Resume needs a record, so a snapshot carried across the move would
// be unresumable state nothing could ever load. The manager calls DropSnapshots
// first, which drops the record too, so the next start cold-boots the moved
// rootfs.ext4 under the new name.
//
// The predicate goes through d.hasSnapshot rather than stat-ing filenames.
// fc.go stats the literal pair mem.snap/state.snap here; lifted unchanged
// against QEMU's single state.migrate that stat matches nothing and this
// function silently stops refusing anything — which is exactly what the parity
// suite's Rename case (gated on Traits.PreservesMemory) exists to catch.
func (d *Driver) RenameVM(oldName, newName string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if st, ok := d.vms[oldName]; ok && st.cmd != nil {
		return fmt.Errorf("vm %q is running; pause it first", oldName)
	}
	if _, ok := d.vms[newName]; ok {
		return fmt.Errorf("vm %q already exists", newName)
	}
	snapshotted, err := d.hasSnapshot(oldName)
	if err != nil {
		return err
	}
	if snapshotted {
		return fmt.Errorf("vm %q has a memory snapshot; drop snapshots before renaming", oldName)
	}
	oldDir, newDir := d.vmDir(oldName), d.vmDir(newName)
	if _, err := os.Stat(newDir); err == nil {
		return fmt.Errorf("vm dir for %q already exists", newName)
	}
	if err := os.Rename(oldDir, newDir); err != nil {
		return err
	}
	delete(d.vms, oldName)
	d.slots.Release(oldName)
	return nil
}

// --- the shared disk plumbing ----------------------------------------------

// --- guest rootfs mounts: key injection and template sanitization ----------
//
// Both callers are gated on !Options.DisableHostRootfsMounts, and that gate is
// what CKS depends on: on a node where it is set, nothing in this process ever
// parses a guest-authored filesystem in the host kernel.
