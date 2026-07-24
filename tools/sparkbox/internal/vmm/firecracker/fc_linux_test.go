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
	capacity, err := d.DiskCapacityMB(context.Background(), "box")
	if err != nil {
		t.Fatal(err)
	}
	if capacity != 25_600 {
		t.Fatalf("DiskCapacityMB = %d, want 25600", capacity)
	}
}

func TestExt4UsageRejectsInvalidSuperblock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not-ext4")
	if err := os.WriteFile(path, make([]byte, 2048), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ext4UsageMB(path); err == nil || !strings.Contains(err.Error(), "magic") {
		t.Fatalf("ext4UsageMB error = %v, want invalid-magic diagnostic", err)
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
