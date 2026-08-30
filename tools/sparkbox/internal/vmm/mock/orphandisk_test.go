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

// restartedDriver returns a second driver over the same state dir — the shape a
// controller comes back in after its process (or its Pod) is replaced. The disks
// are all still there; the in-memory record of them is gone.
func restartedDriver(t *testing.T, stateDir string) *Driver {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := xssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	d := New(stateDir, signer)
	t.Cleanup(func() { d.Close() }) //nolint:errcheck
	return d
}

// A destroy must reclaim the disk even when the driver has never seen the VM.
//
// This is the bug that put eight orphaned rootfs images — over 100 GiB — on the
// live node. d.vms is per-process and nothing rehydrates it, so every sandbox
// the controller has not booted since its last restart is absent from the map
// while its disk sits in the state dir untouched. Destroy read the map, found
// nothing, and reported success. The manager believed it, dropped its ledger
// entry, and released the name.
func TestDestroyReclaimsADiskTheDriverHasNoRecordOf(t *testing.T) {
	d, gwKey := newTestDriver(t)
	if _, err := d.Create(context.Background(), vmm.Config{
		Name: "brave-otter", MemMB: 512, GatewayPublicKey: gwKey, NewSandbox: true,
	}); err != nil {
		t.Fatal(err)
	}
	disk := d.workdirFor("brave-otter")
	if err := os.WriteFile(filepath.Join(disk, "secrets.txt"), []byte("the owner's work"), 0o600); err != nil {
		t.Fatal(err)
	}

	restarted := restartedDriver(t, d.stateDir)
	if err := restarted.Destroy(context.Background(), "brave-otter"); err != nil {
		t.Fatalf("destroy after a restart: %v", err)
	}
	if _, err := os.Stat(disk); !os.IsNotExist(err) {
		t.Fatalf("destroy reported success but %s survives (stat err %v); "+
			"the disk is leaked and the next sandbox to claim this name inherits it", disk, err)
	}
}

// Create must refuse a disk sitting under a name the ledger says is free.
//
// Defence in depth behind the Destroy fix: any *other* way a stray disk appears
// — a destroy interrupted midway, a node restored from an older ledger, the
// orphans that predate the fix — ends the same way if Create adopts it. The name
// is reusable by anybody, so adopting means handing one tenant the previous
// tenant's home directory.
func TestCreateRefusesToAdoptAStrayDiskUnderANewName(t *testing.T) {
	d, gwKey := newTestDriver(t)
	stray := d.workdirFor("brave-otter")
	if err := os.MkdirAll(stray, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stray, "secrets.txt"), []byte("the previous owner's work"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := d.Create(context.Background(), vmm.Config{
		Name: "brave-otter", MemMB: 512, GatewayPublicKey: gwKey, NewSandbox: true,
	})
	if err == nil {
		t.Fatal("create adopted a stray disk under a new sandbox's name")
	}
	if !strings.Contains(err.Error(), "destroyed") {
		t.Errorf("refusal %q does not tell the operator what happened", err)
	}
	// The evidence has to survive the refusal: an operator can only judge whose
	// disk this was if we did not delete it on the way out.
	if _, err := os.Stat(filepath.Join(stray, "secrets.txt")); err != nil {
		t.Errorf("the refused create destroyed the stray disk: %v", err)
	}
}

// The other half of the same switch: a cold boot of a sandbox the ledger DOES
// know must still start from the disk it already has. Getting this backwards
// costs more than the bug being fixed — every sandbox on a node would refuse to
// come back after a controller restart.
func TestCreateStillColdBootsAKnownSandboxsDisk(t *testing.T) {
	d, gwKey := newTestDriver(t)
	if _, err := d.Create(context.Background(), vmm.Config{
		Name: "brave-otter", MemMB: 512, GatewayPublicKey: gwKey, NewSandbox: true,
	}); err != nil {
		t.Fatal(err)
	}
	disk := d.workdirFor("brave-otter")
	if err := os.WriteFile(filepath.Join(disk, "work.txt"), []byte("mid-flight"), 0o600); err != nil {
		t.Fatal(err)
	}

	// A restart, then the manager's recreate path: same name, NewSandbox unset.
	restarted := restartedDriver(t, d.stateDir)
	if _, err := restarted.Create(context.Background(), vmm.Config{
		Name: "brave-otter", MemMB: 512, GatewayPublicKey: gwKey,
	}); err != nil {
		t.Fatalf("cold boot of a known sandbox: %v", err)
	}
	if _, err := os.Stat(filepath.Join(disk, "work.txt")); err != nil {
		t.Fatalf("the cold boot did not keep the sandbox's own disk: %v", err)
	}
}
