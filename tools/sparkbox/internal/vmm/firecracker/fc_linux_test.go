//go:build linux

package firecracker

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	sdk "github.com/firecracker-microvm/firecracker-go-sdk"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
)

// Compile-time capability checks, mirroring the mock's: the manager
// type-asserts for these, so losing one silently degrades the fleet.
var (
	_ vmm.Renamer    = (*Driver)(nil)
	_ vmm.Rebooter   = (*Driver)(nil)
	_ vmm.CPUStatser = (*Driver)(nil)
)

// TestV6Addressing checks the per-slot /127 carving from the delegated /64.
// Constructs the Driver directly to avoid New()'s /dev/kvm requirement.
func TestV6Addressing(t *testing.T) {
	_, ipNet, err := net.ParseCIDR("2001:bc8:702:1c7::/64")
	if err != nil {
		t.Fatal(err)
	}
	d := &Driver{prefix6: ipNet.IP.To16()}

	cases := []struct {
		idx         int
		host, guest string
	}{
		{1, "2001:bc8:702:1c7::2", "2001:bc8:702:1c7::3"},
		{2, "2001:bc8:702:1c7::4", "2001:bc8:702:1c7::5"},
		{255, "2001:bc8:702:1c7::1fe", "2001:bc8:702:1c7::1ff"},
	}
	for _, c := range cases {
		if got := d.hostIP6(c.idx); got != c.host {
			t.Errorf("idx %d hostIP6 = %s, want %s", c.idx, got, c.host)
		}
		if got := d.guestIP6(c.idx); got != c.guest {
			t.Errorf("idx %d guestIP6 = %s, want %s", c.idx, got, c.guest)
		}
		// host must be the even (network) address of the /127, guest the odd one.
		if hi := net.ParseIP(c.host).To16()[15]; hi%2 != 0 {
			t.Errorf("idx %d host address not /127-aligned: %s", c.idx, c.host)
		}
	}
}

// newTestDriver constructs the Driver directly (avoiding New()'s /dev/kvm
// requirement) over a temp state dir, for the capabilities that are pure
// filesystem work.
func newTestDriver(t *testing.T) *Driver {
	t.Helper()
	return &Driver{opts: Options{StateDir: t.TempDir()}, vms: map[string]*vmState{}}
}

// touch creates name's VM dir containing the given files.
func touch(t *testing.T, d *Driver, name string, files ...string) {
	t.Helper()
	dir := d.vmDir(name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(dir, f), []byte(f), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRootfsLoginIdentity(t *testing.T) {
	passwd := []byte("root:x:0:0:root:/root:/bin/bash\nsparky:x:1000:1001::/home/sparky:/bin/bash\n")

	got, err := rootfsLoginIdentity(passwd, "sparky")
	if err != nil {
		t.Fatal(err)
	}
	if got.home != "/home/sparky" || got.uid != 1000 || got.gid != 1001 {
		t.Fatalf("identity = %+v", got)
	}

	got, err = rootfsLoginIdentity(passwd, "")
	if err != nil {
		t.Fatal(err)
	}
	if got.home != "/root" || got.uid != 0 || got.gid != 0 {
		t.Fatalf("default identity = %+v", got)
	}

	for _, tc := range []struct {
		name   string
		passwd string
		user   string
	}{
		{"missing user", string(passwd), "nobody"},
		{"bad uid", "sparky:x:nope:1000::/home/sparky:/bin/sh\n", "sparky"},
		{"unsafe home", "sparky:x:1000:1000::/:/bin/sh\n", "sparky"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := rootfsLoginIdentity([]byte(tc.passwd), tc.user); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestInstallAuthorizedKeyReportsParseFailure(t *testing.T) {
	err := installAuthorizedKey(context.Background(), "/not-mounted", "sparky", "not-an-ssh-key")
	if err == nil {
		t.Fatal("expected invalid gateway key to fail before mounting")
	}
	if !strings.Contains(err.Error(), "ssh:") {
		t.Fatalf("error %q does not preserve the SSH parser reason", err)
	}
}

// fakeRootfs builds an unmounted stand-in for a guest rootfs: just the
// /etc/passwd that writeAuthorizedKey reads, with the login user owned by
// whoever runs the test so the chowns succeed unprivileged.
func fakeRootfs(t *testing.T) string {
	t.Helper()
	mnt := t.TempDir()
	if err := os.MkdirAll(filepath.Join(mnt, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	line := fmt.Sprintf("sparky:x:%d:%d::/home/sparky:/bin/bash\n", os.Getuid(), os.Getgid())
	if err := os.WriteFile(filepath.Join(mnt, "etc", "passwd"), []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	return mnt
}

const testGuestKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIHqQ0kZ3vJZmZ1TzjJ4mUq8kMuQm/kZPq6ZQ0mVUOwvE gateway"

// A guest owns its own rootfs, so it can replace ~/.ssh with a symlink and try
// to steer the root-owned gateway's next cold-boot write onto the host. It must
// not land outside the image.
func TestWriteAuthorizedKeyRefusesSymlinkEscape(t *testing.T) {
	for _, tc := range []struct{ name, target string }{
		{"absolute", ""}, // filled in below with a path outside the rootfs
		{"relative climb", "../../../../../../etc/ssh"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mnt := fakeRootfs(t)
			outside := filepath.Join(t.TempDir(), "host-etc-ssh")
			if err := os.MkdirAll(outside, 0o755); err != nil {
				t.Fatal(err)
			}
			target := tc.target
			if target == "" {
				target = outside
			}
			if err := os.MkdirAll(filepath.Join(mnt, "home", "sparky"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, filepath.Join(mnt, "home", "sparky", ".ssh")); err != nil {
				t.Fatal(err)
			}

			if err := writeAuthorizedKey(mnt, "sparky", testGuestKey); err != nil {
				t.Fatalf("writeAuthorizedKey: %v", err)
			}

			// Nothing outside the rootfs was created or touched.
			if entries, err := os.ReadDir(outside); err != nil || len(entries) != 0 {
				t.Fatalf("escaped the rootfs: %v entries=%v", err, entries)
			}
			// The planted symlink was replaced by a real directory holding the key.
			sshDir := filepath.Join(mnt, "home", "sparky", ".ssh")
			fi, err := os.Lstat(sshDir)
			if err != nil {
				t.Fatal(err)
			}
			if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
				t.Fatalf(".ssh mode = %v, want a real directory", fi.Mode())
			}
			got, err := os.ReadFile(filepath.Join(sshDir, "authorized_keys"))
			if err != nil {
				t.Fatal(err)
			}
			if strings.TrimSpace(string(got)) != testGuestKey {
				t.Fatalf("authorized_keys = %q", got)
			}
		})
	}
}

// Create is re-run on an existing rootfs whenever a resume fails, so keys the
// user added inside their own sandbox have to survive the cold boot.
func TestWriteAuthorizedKeyPreservesUserKeys(t *testing.T) {
	mnt := fakeRootfs(t)
	sshDir := filepath.Join(mnt, "home", "sparky", ".ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		t.Fatal(err)
	}
	const laptop = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIP3Yd0ZGPdmZLQAKk8xLmqL/9Zr5rWQNqjRLYh7lHcVn laptop"
	// The gateway key is already present under a different comment — it must be
	// recognised as the same key and not duplicated.
	prior := laptop + "\n" + strings.TrimSuffix(testGuestKey, " gateway") + " stale-comment\n"
	if err := os.WriteFile(filepath.Join(sshDir, "authorized_keys"), []byte(prior), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := writeAuthorizedKey(mnt, "sparky", testGuestKey); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(filepath.Join(sshDir, "authorized_keys"))
	if err != nil {
		t.Fatal(err)
	}
	want := laptop + "\n" + testGuestKey + "\n"
	if string(got) != want {
		t.Fatalf("authorized_keys =\n%q\nwant\n%q", got, want)
	}
}

// A rootfs whose login user has no home yet must come out with one the user
// owns; sshd's StrictModes rejects pubkey auth on a root-owned home, and the
// only symptom is an SSH attach that hangs.
func TestWriteAuthorizedKeyCreatesOwnedHome(t *testing.T) {
	mnt := fakeRootfs(t)
	if err := writeAuthorizedKey(mnt, "sparky", testGuestKey); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		path string
		perm os.FileMode
	}{
		{filepath.Join(mnt, "home", "sparky"), 0o755},
		{filepath.Join(mnt, "home", "sparky", ".ssh"), 0o700},
	} {
		fi, err := os.Stat(tc.path)
		if err != nil {
			t.Fatal(err)
		}
		if !fi.IsDir() || fi.Mode().Perm() != tc.perm {
			t.Fatalf("%s mode = %v, want dir %v", tc.path, fi.Mode(), tc.perm)
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !ok {
			t.Fatalf("%s: no stat", tc.path)
		}
		if int(st.Uid) != os.Getuid() || int(st.Gid) != os.Getgid() {
			t.Fatalf("%s owned by %d:%d, want the login user %d:%d",
				tc.path, st.Uid, st.Gid, os.Getuid(), os.Getgid())
		}
	}
}

func TestDiskUsageReadsExt4UsedBlocks(t *testing.T) {
	d := newTestDriver(t)
	dir := d.vmDir("box")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := d.rootfsPath("box")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	// Match the live macOS PoC geometry: a 25 GiB filesystem with only about
	// 2.9 GiB in use. Materialize the outer file sparsely to prove the result
	// comes from ext4 counters rather than host allocation.
	const blocks = uint32(6_553_600)
	const free = uint32(5_789_327)
	const overhead = uint32(146_887)
	if err := f.Truncate(int64(blocks) * 4096); err != nil {
		t.Fatal(err)
	}
	sb := make([]byte, 1024)
	binary.LittleEndian.PutUint32(sb[0x04:0x08], blocks)
	binary.LittleEndian.PutUint32(sb[0x0c:0x10], free)
	binary.LittleEndian.PutUint32(sb[0x18:0x1c], 2) // 1024 << 2 = 4096
	binary.LittleEndian.PutUint16(sb[0x38:0x3a], 0xef53)
	binary.LittleEndian.PutUint32(sb[0x248:0x24c], overhead)
	if _, err := f.WriteAt(sb, 1024); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	got, err := d.DiskUsageMB(context.Background(), "box")
	if err != nil {
		t.Fatal(err)
	}
	const want = int64(2_411)
	if got != want {
		t.Fatalf("DiskUsageMB = %d, want %d", got, want)
	}
	// Capacity excludes the same metadata overhead usage does, so the meter's
	// two halves share a basis: 25 GiB of image, minus 146887 overhead blocks,
	// is the 25026 MiB `df` reports inside the guest — not the raw 25600.
	capacity, err := d.DiskCapacityMB(context.Background(), "box")
	if err != nil {
		t.Fatal(err)
	}
	if capacity != 25_026 {
		t.Fatalf("DiskCapacityMB = %d, want 25026", capacity)
	}
	if got > capacity {
		t.Fatalf("usage %d exceeds capacity %d", got, capacity)
	}
}

// A full filesystem has to read as full: with no free blocks, usage and
// capacity must land on the same number or the console meter can never reach
// 100%.
func TestDiskUsageReachesCapacityWhenFull(t *testing.T) {
	d := newTestDriver(t)
	if err := os.MkdirAll(d.vmDir("box"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeExt4Superblock(t, d.rootfsPath("box"), 6_553_600, 0, 146_887)

	used, err := d.DiskUsageMB(context.Background(), "box")
	if err != nil {
		t.Fatal(err)
	}
	capacity, err := d.DiskCapacityMB(context.Background(), "box")
	if err != nil {
		t.Fatal(err)
	}
	if used != capacity {
		t.Fatalf("full filesystem reports %d/%d MB, want them equal", used, capacity)
	}
}

func TestExt4DiskRejectsInvalidSuperblock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not-ext4")
	if err := os.WriteFile(path, make([]byte, 2048), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ext4DiskMB(path); err == nil || !strings.Contains(err.Error(), "magic") {
		t.Fatalf("ext4DiskMB error = %v, want invalid-magic diagnostic", err)
	}
}

// writeExt4Superblock lays down a sparse image carrying just the superblock
// fields the disk accounting reads, with 4 KiB blocks.
func writeExt4Superblock(t *testing.T, path string, blocks, free, overhead uint32) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close() //nolint:errcheck
	if err := f.Truncate(int64(blocks) * 4096); err != nil {
		t.Fatal(err)
	}
	sb := make([]byte, 1024)
	binary.LittleEndian.PutUint32(sb[0x04:0x08], blocks)
	binary.LittleEndian.PutUint32(sb[0x0c:0x10], free)
	binary.LittleEndian.PutUint32(sb[0x18:0x1c], 2) // 1024 << 2 = 4096
	binary.LittleEndian.PutUint16(sb[0x38:0x3a], 0xef53)
	binary.LittleEndian.PutUint32(sb[0x248:0x24c], overhead)
	if _, err := f.WriteAt(sb, 1024); err != nil {
		t.Fatal(err)
	}
}

func TestDropSnapshots(t *testing.T) {
	d := newTestDriver(t)
	touch(t, d, "box", "rootfs.ext4", "mem.snap", "state.snap")
	d.vms["box"] = &vmState{idx: 1, paused: true}

	if err := d.DropSnapshots("box"); err != nil {
		t.Fatal(err)
	}
	dir := d.vmDir("box")
	for _, f := range []string{"mem.snap", "state.snap"} {
		if _, err := os.Stat(filepath.Join(dir, f)); !os.IsNotExist(err) {
			t.Errorf("%s still present: %v", f, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "rootfs.ext4")); err != nil {
		t.Errorf("rootfs removed: %v", err)
	}
	// The paused record must be gone so resumeOrRecreate's Create cold-boots
	// instead of tripping the already-exists check.
	if _, ok := d.vms["box"]; ok {
		t.Error("driver record not dropped")
	}

	// Idempotent: no snapshots, no record — still fine.
	if err := d.DropSnapshots("box"); err != nil {
		t.Fatal(err)
	}
}

func TestDropSnapshotsRefusesRunning(t *testing.T) {
	d := newTestDriver(t)
	touch(t, d, "box", "mem.snap", "state.snap")
	d.vms["box"] = &vmState{idx: 1, machine: &sdk.Machine{}}
	if err := d.DropSnapshots("box"); err == nil {
		t.Fatal("expected error on running vm")
	}
}

func TestRenameVMRefusesWithSnapshot(t *testing.T) {
	d := newTestDriver(t)
	// state.snap embeds absolute paths into the old dir; a rename would leave a
	// resume pointing at a dir that no longer exists.
	touch(t, d, "old", "rootfs.ext4", "state.snap")
	d.vms["old"] = &vmState{idx: 1, paused: true}
	err := d.RenameVM("old", "new")
	if err == nil || !strings.Contains(err.Error(), "snapshot") {
		t.Fatalf("expected snapshot-refusal error, got %v", err)
	}
	if _, err := os.Stat(d.vmDir("old")); err != nil {
		t.Fatalf("old dir moved despite refusal: %v", err)
	}
}

func TestRenameVM(t *testing.T) {
	d := newTestDriver(t)
	touch(t, d, "old", "rootfs.ext4")
	d.vms["old"] = &vmState{idx: 1, paused: true}

	if err := d.RenameVM("old", "new"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(d.vmDir("old")); !os.IsNotExist(err) {
		t.Errorf("old dir still present: %v", err)
	}
	if _, err := os.Stat(filepath.Join(d.vmDir("new"), "rootfs.ext4")); err != nil {
		t.Errorf("rootfs missing after rename: %v", err)
	}
	if _, ok := d.vms["old"]; ok {
		t.Error("old driver record not dropped")
	}
	if _, ok := d.vms["new"]; ok {
		t.Error("rename must leave the post-restart shape, not rekey the record")
	}
}

func TestRenameVMRefusals(t *testing.T) {
	d := newTestDriver(t)
	touch(t, d, "old", "rootfs.ext4")
	d.vms["old"] = &vmState{idx: 1, machine: &sdk.Machine{}}
	if err := d.RenameVM("old", "new"); err == nil {
		t.Fatal("expected error renaming a running vm")
	}
	d.vms["old"].machine = nil
	d.vms["old"].paused = true

	touch(t, d, "taken", "rootfs.ext4")
	if err := d.RenameVM("old", "taken"); err == nil {
		t.Fatal("expected error renaming onto an existing vm dir")
	}
	d.vms["live"] = &vmState{idx: 2, paused: true}
	if err := d.RenameVM("old", "live"); err == nil {
		t.Fatal("expected error renaming onto an existing vm record")
	}
}

// TestFreeSlotReuse checks that slot allocation reclaims indices released by
// dropped records — Destroy, DropSnapshots (every reboot), and RenameVM all
// delete the vmState, and a monotonic counter would burn a slot each time
// until Create failed with an unroutable 172.30.256.1.
func TestFreeSlotReuse(t *testing.T) {
	d := newTestDriver(t)
	d.vms["a"] = &vmState{idx: 1, paused: true}
	d.vms["c"] = &vmState{idx: 3, paused: true}

	// Lowest free slot, including gaps between live records.
	if idx, err := d.freeSlot(); err != nil || idx != 2 {
		t.Fatalf("freeSlot = %d, %v; want 2", idx, err)
	}

	// Dropping a record (here via DropSnapshots, the reboot path) releases its
	// slot for the next Create.
	touch(t, d, "a", "rootfs.ext4")
	if err := d.DropSnapshots("a"); err != nil {
		t.Fatal(err)
	}
	if idx, err := d.freeSlot(); err != nil || idx != 1 {
		t.Fatalf("freeSlot after DropSnapshots = %d, %v; want 1", idx, err)
	}

	// RenameVM likewise.
	touch(t, d, "c", "rootfs.ext4")
	if err := d.RenameVM("c", "c2"); err != nil {
		t.Fatal(err)
	}
	d.vms["a"] = &vmState{idx: 1, paused: true}
	d.vms["b"] = &vmState{idx: 2, paused: true}
	if idx, err := d.freeSlot(); err != nil || idx != 3 {
		t.Fatalf("freeSlot after RenameVM = %d, %v; want 3", idx, err)
	}

	// Exhaustion must error rather than mint an out-of-range address.
	for i := 1; i <= 255; i++ {
		d.vms[fmt.Sprintf("v%d", i)] = &vmState{idx: i, paused: true}
	}
	if _, err := d.freeSlot(); err == nil {
		t.Fatal("expected error with all 255 slots in use")
	}
}

func TestProcStatCPUTicks(t *testing.T) {
	// comm ("fire cr) acker") contains a space and a ')': fields must be
	// counted from the LAST ')'. utime=150, stime=25 (fields 14/15).
	line := "1234 (fire cr) acker) S 10 10 10 0 -1 4194560 500 0 0 0 150 25 12 3 20 0 4 0 100000 0 0"
	got, err := procStatCPUTicks(line)
	if err != nil {
		t.Fatal(err)
	}
	if got != 175 {
		t.Errorf("ticks = %d, want 175", got)
	}

	for _, bad := range []string{
		"no closing paren",
		"1234 (fc) S 10 10", // too few fields after comm
	} {
		if _, err := procStatCPUTicks(bad); err == nil {
			t.Errorf("procStatCPUTicks(%q) accepted malformed input", bad)
		}
	}
}

// A guest may legitimately keep a 0750 home; installing the gateway key is not
// a reason to widen it. ~/.ssh is the deliberate exception — sshd ignores
// authorized_keys unless that directory is the login user's and private.
func TestWriteAuthorizedKeyLeavesExistingHomeModeAlone(t *testing.T) {
	mnt := fakeRootfs(t)
	home := filepath.Join(mnt, "home", "sparky")
	if err := os.MkdirAll(filepath.Join(home, ".ssh"), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(home, 0o750); err != nil {
		t.Fatal(err)
	}

	if err := writeAuthorizedKey(mnt, "sparky", testGuestKey); err != nil {
		t.Fatal(err)
	}

	fi, err := os.Stat(home)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o750 {
		t.Fatalf("home mode = %v, want the guest's own 0750 untouched", fi.Mode().Perm())
	}
	if fi, err = os.Stat(filepath.Join(home, ".ssh")); err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o700 {
		t.Fatalf(".ssh mode = %v, want 0700 for sshd StrictModes", fi.Mode().Perm())
	}
}
