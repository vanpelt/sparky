package mock

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	xssh "golang.org/x/crypto/ssh"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
)

// Compile-time capability checks: every optional interface in vmm, not the four
// somebody happened to write down. The mock must offer all of them or the
// mock-driven manager tests silently skip the paths behind the ones it lost —
// which is the same failure as the firecracker driver losing one, arriving as a
// green test run instead of a degraded fleet.
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

// newTestDriver returns a mock driver in a temp state dir plus an
// authorized_keys line to hand Create as the gateway key.
func newTestDriver(t *testing.T) (*Driver, string) {
	t.Helper()
	_, hostPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	hostSigner, err := xssh.NewSignerFromKey(hostPriv)
	if err != nil {
		t.Fatal(err)
	}
	_, gwPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	gwSigner, err := xssh.NewSignerFromKey(gwPriv)
	if err != nil {
		t.Fatal(err)
	}
	d := New(t.TempDir(), hostSigner)
	t.Cleanup(func() { d.Close() }) //nolint:errcheck
	return d, string(xssh.MarshalAuthorizedKey(gwSigner.PublicKey()))
}

func create(t *testing.T, d *Driver, gwKey, name string) *vmm.Instance {
	t.Helper()
	inst, err := d.Create(context.Background(), vmm.Config{Name: name, MemMB: 512, GatewayPublicKey: gwKey})
	if err != nil {
		t.Fatalf("Create %s: %v", name, err)
	}
	return inst
}

func TestCPUTimeNanos(t *testing.T) {
	d, gwKey := newTestDriver(t)
	create(t, d, gwKey, "box")

	first, err := d.CPUTimeNanos(context.Background(), "box")
	if err != nil {
		t.Fatal(err)
	}
	second, err := d.CPUTimeNanos(context.Background(), "box")
	if err != nil {
		t.Fatal(err)
	}
	if second <= first {
		t.Fatalf("counter not monotonic: %d then %d", first, second)
	}
	if second-first != mockCPUTickNanos {
		t.Fatalf("delta = %d, want the fixed tick %d", second-first, mockCPUTickNanos)
	}

	if err := d.Pause(context.Background(), "box"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.CPUTimeNanos(context.Background(), "box"); err == nil {
		t.Fatal("expected error on paused vm")
	}
	if _, err := d.CPUTimeNanos(context.Background(), "nope"); err == nil {
		t.Fatal("expected error on missing vm")
	}

	// Resume keeps the counter running — like a firecracker snapshot restore,
	// where the same VMM process picture carries forward.
	if _, err := d.Resume(context.Background(), "box"); err != nil {
		t.Fatal(err)
	}
	third, err := d.CPUTimeNanos(context.Background(), "box")
	if err != nil {
		t.Fatal(err)
	}
	if third != second+mockCPUTickNanos {
		t.Fatalf("counter after resume = %d, want %d", third, second+mockCPUTickNanos)
	}
}

func TestDropSnapshots(t *testing.T) {
	d, gwKey := newTestDriver(t)
	create(t, d, gwKey, "box")

	// Running VM: refuse, like firecracker.
	if err := d.DropSnapshots("box"); err == nil {
		t.Fatal("expected error on running vm")
	}

	workdir := filepath.Join(d.stateDir, "mock-vms", "box")
	if err := os.WriteFile(filepath.Join(workdir, "marker"), []byte("disk"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := d.Pause(context.Background(), "box"); err != nil {
		t.Fatal(err)
	}
	if err := d.DropSnapshots("box"); err != nil {
		t.Fatal(err)
	}

	// The driver forgot the VM (Resume fails), so the manager's
	// resumeOrRecreate falls through to Create — which must cold-boot the
	// persisted workdir, not re-seed it.
	if _, err := d.Resume(context.Background(), "box"); err == nil {
		t.Fatal("expected Resume to fail after DropSnapshots")
	}
	create(t, d, gwKey, "box")
	got, err := os.ReadFile(filepath.Join(workdir, "marker"))
	if err != nil || string(got) != "disk" {
		t.Fatalf("workdir not preserved across drop+recreate: %v %q", err, got)
	}

	// A VM the driver has no record of (post-restart shape) is fine to drop.
	if err := d.DropSnapshots("unknown"); err != nil {
		t.Fatal(err)
	}
}

func TestRenameVM(t *testing.T) {
	d, gwKey := newTestDriver(t)
	create(t, d, gwKey, "old")

	if err := d.RenameVM("old", "new"); err == nil {
		t.Fatal("expected error renaming a running vm")
	}

	oldDir := filepath.Join(d.stateDir, "mock-vms", "old")
	newDir := filepath.Join(d.stateDir, "mock-vms", "new")
	if err := os.WriteFile(filepath.Join(oldDir, "marker"), []byte("disk"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := d.Pause(context.Background(), "old"); err != nil {
		t.Fatal(err)
	}
	if err := d.RenameVM("old", "new"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(oldDir); !os.IsNotExist(err) {
		t.Fatalf("old workdir still present: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(newDir, "marker")); err != nil || string(got) != "disk" {
		t.Fatalf("workdir contents lost in rename: %v %q", err, got)
	}

	// The record was rekeyed, so Resume under the new name works and reports it.
	inst, err := d.Resume(context.Background(), "new")
	if err != nil {
		t.Fatal(err)
	}
	if inst.Name != "new" {
		t.Fatalf("instance name = %q, want %q", inst.Name, "new")
	}
	if inst.State != vmm.StateRunning {
		t.Fatalf("state = %q, want running", inst.State)
	}
}

func TestRenameVMRefusesTakenName(t *testing.T) {
	d, gwKey := newTestDriver(t)
	create(t, d, gwKey, "a")
	create(t, d, gwKey, "b")
	if err := d.Pause(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	if err := d.Pause(context.Background(), "b"); err != nil {
		t.Fatal(err)
	}
	err := d.RenameVM("a", "b")
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("expected already-exists error, got %v", err)
	}
}

// TestTemplateUsageMB: the baseline the pooled accounting subtracts has to be
// measured the same way as the figure it is subtracted FROM, or the difference
// is not a number of blocks anybody wrote. Under firecracker that identity is
// one ext4 superblock read serving both DiskUsageMB and TemplateUsageMB; here
// it is the same tree walk over a workdir the mock's Create copyTree'd out of
// the template. A fresh fork must therefore net to exactly zero pooled MB.
func TestTemplateUsageMB(t *testing.T) {
	d, gwKey := newTestDriver(t)
	ctx := context.Background()
	create(t, d, gwKey, "src")

	// 33 MiB, sparse: big enough that the two sides cannot agree by accident.
	// A round 6 MiB reads back as 6 whether the driver divides by 1024*1024 or
	// by 1000*1000, so the agreement assertion below could not see the two
	// measurements drift apart; 33 MiB is 33 one way and 34 the other.
	workdir := filepath.Join(d.stateDir, "mock-vms", "src")
	f, err := os.Create(filepath.Join(workdir, "rootfs"))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(33 << 20); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := d.Pause(ctx, "src"); err != nil {
		t.Fatal(err)
	}
	if err := d.Snapshot(ctx, "src", "snap-alice-cuda"); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	tplMB, err := d.TemplateUsageMB(ctx, "snap-alice-cuda")
	if err != nil {
		t.Fatalf("TemplateUsageMB: %v", err)
	}
	if tplMB != 33 {
		t.Fatalf("template measured %d MB, want the 33 MiB written into it", tplMB)
	}

	// A fork OF the template. Its usage and its baseline are the
	// same bytes, so the owner is charged nothing for blocks the clone shares.
	inst, err := d.Create(ctx, vmm.Config{Name: "clone", MemMB: 512, Image: "snap-alice-cuda", GatewayPublicKey: gwKey})
	if err != nil {
		t.Fatalf("Create clone: %v", err)
	}
	if inst == nil {
		t.Fatal("Create returned no instance")
	}
	cloneMB, err := d.DiskUsageMB(ctx, "clone")
	if err != nil {
		t.Fatal(err)
	}
	if cloneMB != tplMB {
		t.Fatalf("clone uses %d MB against a %d MB template: the two measurements have drifted apart and their difference is meaningless", cloneMB, tplMB)
	}

	// A template that is gone must be an ERROR, never a zero. host.Manager
	// keeps the last-known baseline on an error precisely so that deleting a
	// snapshot does not re-charge every fork of it for the whole template.
	if err := d.RemoveTemplate(ctx, "snap-alice-cuda"); err != nil {
		t.Fatal(err)
	}
	if mb, err := d.TemplateUsageMB(ctx, "snap-alice-cuda"); err == nil {
		t.Fatalf("a deleted template reported %d MB and no error", mb)
	} else if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("deleted template: %v, want an error wrapping os.ErrNotExist", err)
	}
	if _, err := d.TemplateUsageMB(ctx, "never-existed"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("unknown template: %v, want an error wrapping os.ErrNotExist", err)
	}
}
