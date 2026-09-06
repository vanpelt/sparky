//go:build linux

package firecracker

import (
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm/hostnet"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/guestnet"
)

// sparkbox_fresh is the guest's only permission to move a git checkout it did
// not make. It has to appear on exactly the boots where a rootfs was just
// copied from a template and on no others, because the alternative — a resume
// or a reboot carrying it — is a branch switched under somebody who is working
// in the tree.
//
// The value is a marker, not data: the guest tests for the literal `=1`, so a
// spelling change here silently turns adoption off rather than failing.
func TestFreshMarkerRidesOnlyAFirstBoot(t *testing.T) {
	d := testDriver(t)

	first, err := d.kernelArgs("brave-otter", 0, true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(first, " sparkbox_fresh=1") {
		t.Errorf("a first boot carries no sparkbox_fresh marker, so a fork can never adopt its inherited checkout:\n%s", first)
	}

	again, err := d.kernelArgs("brave-otter", 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(again, "sparkbox_fresh") {
		t.Errorf("a reused disk carries the fresh marker; the guest would switch a branch under whoever is working in it:\n%s", again)
	}

	// The rest of the line must not move when the marker does — sparkbox_host
	// is what the identity reset compares against, and the machine id is what
	// keeps a fork from being its parent's twin.
	for _, want := range []string{"sparkbox_host=brave-otter", "systemd.machine_id="} {
		if !strings.Contains(first, want) || !strings.Contains(again, want) {
			t.Errorf("kernel args lost %q", want)
		}
	}

	// Tokens are space-separated and the guest splits /proc/cmdline on
	// whitespace; a marker glued to the previous argument reaches nothing.
	for _, tok := range strings.Fields(first) {
		if tok == "sparkbox_fresh=1" {
			return
		}
	}
	t.Errorf("sparkbox_fresh=1 is not its own cmdline token:\n%s", first)
}

func testDriver(t *testing.T) *Driver {
	t.Helper()
	return &Driver{net: hostnet.Plumbing{Net: guestnet.MustParse(""), TapPrefix: tapPrefix}}
}

// Who may pass fresh=true to boot.
//
// The marker is the guest's only permission to move a git checkout it did not
// make, so the question "which boot paths claim a disk is new" has to have a
// visible, short answer. Today it is one: Create, and only when its own
// os.Stat found no rootfs and it reflinked one. Every other path — Resume, and
// the cold boots that Create itself serves for restore-from-archive,
// restore-from-checkpoint and reboot — reuses a disk that already exists.
//
// A structural test rather than a behavioural one because boot needs a VMM.
// What it protects against is a third call site appearing and defaulting to
// the wrong thing: `true` here is a branch switched under somebody working in
// the tree, and nothing else in the system would notice.
func TestOnlyCreateClaimsADiskIsFresh(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "fc.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "boot" {
			return true
		}
		calls++
		last := call.Args[len(call.Args)-1]
		switch arg := last.(type) {
		case *ast.Ident:
			if arg.Name == "false" {
				return true
			}
			if arg.Name != "fresh" {
				t.Errorf("d.boot at %s passes %q as fresh; it must be the literal false or Create's own reflink result",
					fset.Position(call.Pos()), arg.Name)
			}
		default:
			t.Errorf("d.boot at %s computes its fresh argument inline; keep it a plain false or Create's reflink result so this stays readable",
				fset.Position(call.Pos()))
		}
		return true
	})
	if calls != 2 {
		t.Errorf("fc.go has %d calls to boot, want 2 (Create and Resume). A new one must decide fresh deliberately: true is permission to move somebody's checkout", calls)
	}
}
