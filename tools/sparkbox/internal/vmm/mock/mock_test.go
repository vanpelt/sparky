package mock

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	xssh "golang.org/x/crypto/ssh"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
)

// Compile-time capability checks: the mock must offer everything the manager
// type-asserts for, or the mock-driven manager tests silently skip paths.
var (
	_ vmm.Renamer    = (*Driver)(nil)
	_ vmm.Rebooter   = (*Driver)(nil)
	_ vmm.CPUStatser = (*Driver)(nil)
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
