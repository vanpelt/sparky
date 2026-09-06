//go:build linux

package firecracker

import (
	"context"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm/hostnet"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/guestnet"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
)

// A destroy must reclaim the rootfs even when the driver has no record of the VM.
//
// This is the bug that left eight orphaned rootfs images — over 100 GiB of real
// blocks — on the live CKS node. d.vms is per-process and nothing rehydrates it,
// so after the controller Pod is replaced every sandbox it has not booted since
// is missing from the map while its disk sits in VMStateDir exactly where the
// previous process left it. Destroy read the map, found nothing, and returned
// nil. The manager took that for success, deleted its ledger row, and freed the
// name — leaving a 25 GiB disk with no owner and no way to find it but ls.
func TestDestroyReclaimsARootfsTheDriverHasNoRecordOf(t *testing.T) {
	d := &Driver{opts: Options{VMStateDir: t.TempDir()}}

	dir := d.vmDir("brave-otter")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"rootfs.ext4", "mem.snap", "state.snap"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("the owner's work"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	if err := d.Destroy(context.Background(), "brave-otter"); err != nil {
		t.Fatalf("destroy with no driver record: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("destroy reported success but %s survives (stat err %v); "+
			"the disk is leaked and the next sandbox to claim this name inherits it", dir, err)
	}
}

// Create must refuse a rootfs sitting under a name the ledger says is free.
//
// Defence in depth behind the Destroy fix. Any other route to a stray disk — a
// destroy interrupted partway, a ledger restored from a backup, the orphans that
// predate this fix — ends the same way if Create adopts what it finds: the
// os.Stat guard skips the reflink, the sandbox boots the previous tenant's
// filesystem, and because no template was cloned it is not even marked fresh.
// That was confirmed on the live node by git reflog: a recreated sandbox carried
// the destroyed one's checkout history.
func TestCreateRefusesToAdoptAStrayRootfsUnderANewName(t *testing.T) {
	// A usable guest network so nothing incidental fails first: the refusal has
	// to be what stops this create, not a driver that was too bare to get going.
	d := &Driver{opts: Options{VMStateDir: t.TempDir()}, net: hostnet.Plumbing{Net: guestnet.MustParse(""), TapPrefix: tapPrefix}}

	stray := filepath.Join(d.vmDir("brave-otter"), "rootfs.ext4")
	if err := os.MkdirAll(filepath.Dir(stray), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stray, []byte("the previous owner's work"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := d.Create(context.Background(), vmm.Config{
		Name: "brave-otter", Image: "universal", VCPUs: 1, MemMB: 512, NewSandbox: true,
	})
	if err == nil {
		t.Fatal("create adopted a stray rootfs under a new sandbox's name")
	}
	// Not merely "some error": the create must stop ON the stray disk, before it
	// takes a slot or a tap. Any other failure here means the disk got past the
	// gate and something further down happened to object.
	if !strings.Contains(err.Error(), "destroyed") {
		t.Fatalf("create failed with %q, not the adoption refusal; the stray rootfs got past the gate", err)
	}
	if !strings.Contains(err.Error(), stray) {
		t.Errorf("refusal %q does not name the path an operator has to go look at", err)
	}
	// The evidence must survive the refusal: whose disk this was is only
	// answerable while it still exists.
	if _, err := os.Stat(stray); err != nil {
		t.Errorf("the refused create deleted the stray rootfs: %v", err)
	}
}

// Which Create call sites claim a name is new.
//
// NewSandbox is load-bearing in both directions and the two mistakes are not
// symmetric. Forgetting it on a genuinely new sandbox re-opens the adoption
// hole this fix closes. Setting it on the recreate path is far worse: every
// cold boot after a controller restart would refuse, and a node full of paused
// sandboxes would never come back. Neither shows up in a build.
//
// Structural rather than behavioural because Create needs KVM. It pins the
// count so a third call site cannot appear and quietly default to zero.
func TestOnlyANewNameClaimsToBeANewSandbox(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "../../host/manager.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	calls, claiming := 0, 0
	ast.Inspect(file, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		sel, ok := lit.Type.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Config" {
			return true
		}
		if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "vmm" {
			return true
		}
		calls++
		for _, elt := range lit.Elts {
			kv, ok := elt.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			key, ok := kv.Key.(*ast.Ident)
			if !ok || key.Name != "NewSandbox" {
				continue
			}
			val, ok := kv.Value.(*ast.Ident)
			if !ok || val.Name != "true" {
				t.Errorf("vmm.Config at %s computes NewSandbox; it must be the literal true, at the one site that just proved the name is free",
					fset.Position(lit.Pos()))
				continue
			}
			claiming++
		}
		return true
	})
	if calls != 2 {
		t.Errorf("manager.go builds %d vmm.Configs, want 2 (Manager.Create and resumeOrRecreate). A new one must decide NewSandbox deliberately", calls)
	}
	if claiming != 1 {
		t.Errorf("%d of them set NewSandbox: true, want exactly 1. Zero re-opens disk adoption under a re-used name; two refuses every cold boot after a restart", claiming)
	}
}
