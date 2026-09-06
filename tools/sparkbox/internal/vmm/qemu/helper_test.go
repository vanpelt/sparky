//go:build linux

package qemu

import (
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm/hostnet"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// The driver has to derive the path of a socket a DIFFERENT container created,
// for a VMM it did not start. Both halves of that path are agreed with
// internal/vmhelper: the "qemu" component is a fixed constant there rather than
// the emulator's basename, precisely so this container never needs QEMU's path.
func TestJailedDriverDerivesTheHelpersMonitorSocket(t *testing.T) {
	d := &Driver{opts: Options{
		VMStateDir:             "/var/lib/sparkbox/hot/controller",
		PrivilegedHelperSocket: "/run/sparkbox-vmm/helper.sock",
		JailerChrootBase:       "/var/lib/sparkbox/hot/jailer",
	}}
	if !d.jailed() {
		t.Fatal("a driver with a helper socket does not report itself jailed")
	}
	st := &vmState{idx: 3}
	if got, want := d.monitorSocket("box", st), "/var/lib/sparkbox/hot/jailer/qemu/sparkbox-3/root/qmp.sock"; got != want {
		t.Errorf("monitorSocket = %q, want %q", got, want)
	}
	// The direct launcher keeps the socket beside the sandbox's own files.
	direct := &Driver{opts: Options{VMStateDir: "/state"}}
	if direct.jailed() {
		t.Fatal("a driver with no helper socket reports itself jailed")
	}
	if got, want := direct.monitorSocket("box", st), "/state/qemu-vms/box/qmp.sock"; got != want {
		t.Errorf("monitorSocket = %q, want %q", got, want)
	}
}

// The tap name is the same on both paths, and the same as the firecracker
// driver's. It used not to be, and the divergence was invisible from inside
// this package: everything that CONSUMES a tap name — netpush, sluice's meter,
// deploy/sparkbox-net.sh — lives elsewhere and hardcodes the other one, so a
// direct-launch QEMU node had egress control that silently applied to nothing.
func TestTapNameIsTheNameEveryConsumerAssumes(t *testing.T) {
	jailed, err := newForTapName(Options{PrivilegedHelperSocket: "/run/helper.sock"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := jailed.tapName(7), "sbtap7"; got != want {
		t.Errorf("jailed tapName = %q, want %q", got, want)
	}
	// The Plumbing carries the prefix; New always sets it from
	// Options.TapPrefix or defaultTapPrefix.
	direct := &Driver{net: hostnet.Plumbing{TapPrefix: defaultTapPrefix}}
	if got, want := direct.tapName(7), "sbtap7"; got != want {
		t.Errorf("direct tapName = %q, want %q", got, want)
	}

	// The override exists only so the parity suite can run both drivers on one
	// host. It must still reach tapName, or that suite silently stops being a
	// two-driver test.
	odd := &Driver{net: hostnet.Plumbing{TapPrefix: "sbqtap"}}
	if got, want := odd.tapName(7), "sbqtap7"; got != want {
		t.Errorf("overridden tapName = %q, want %q", got, want)
	}
}

// newForTapName resolves the prefix the way New does without needing a binary,
// a kernel or a subnet on the machine running the test.
func newForTapName(opts Options) (*Driver, error) {
	d := &Driver{opts: opts, net: hostnet.Plumbing{TapPrefix: opts.TapPrefix}}
	if d.net.TapPrefix == "" {
		d.net.TapPrefix = defaultTapPrefix
	}
	return d, nil
}

// boundedLog is written by a child process and read by whichever goroutine is
// building the failure message, so the race detector is the point of this test
// as much as the truncation is.
func TestBoundedLogKeepsTheHeadAndNeverShortWrites(t *testing.T) {
	var b boundedLog
	const refusal = "launch refused: slot already belongs to other\n"
	n, err := b.Write([]byte(refusal))
	if err != nil || n != len(refusal) {
		t.Fatalf("Write = %d, %v; want %d, nil", n, err, len(refusal))
	}
	// A short write would make the child believe its stderr broke.
	flood := make([]byte, boundedLogBytes*2)
	for i := range flood {
		flood[i] = 'x'
	}
	if n, err := b.Write(flood); err != nil || n != len(flood) {
		t.Fatalf("Write of %d bytes returned %d, %v", len(flood), n, err)
	}
	got := b.String()
	if len(got) > boundedLogBytes {
		t.Errorf("boundedLog kept %d bytes, cap is %d", len(got), boundedLogBytes)
	}
	// The refusal is at the TOP, which is why the head is what is kept.
	if !strings.HasPrefix(got, strings.TrimSpace(refusal)) {
		t.Errorf("the first message was lost: %.60q", got)
	}

	var concurrent boundedLog
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 100 {
			concurrent.Write([]byte("helper: refused\n")) //nolint:errcheck
		}
	}()
	for range 100 {
		_ = concurrent.String()
	}
	<-done
}

// TestTapLifecycleGoesThroughTheGuardedWrappers is a tripwire, and it exists
// because the thing it guards was already broken once by a clean rebase.
//
// Under the privileged helper the tap belongs to the helper: it is created in
// the shared Pod network namespace by a process that has NET_ADMIN, and this
// process does not. d.createTap and d.deleteTap are the whole of that
// knowledge — they return early when d.jailed(). Calling d.net.CreateTap
// directly bypasses the check, and the failure is not subtle in the way a
// missed nil check is: every sandbox create fails at
// "ioctl(TUNSETIFF): Operation not permitted".
//
// What made it worth a test rather than a review note is HOW it broke. One
// branch moved the tap plumbing into internal/vmm/hostnet and rewrote these
// call sites to d.net.*; a branch stacked on top of it added the d.jailed()
// guard to the wrappers. Neither change conflicted with the other, git merged
// both, and the guard ended up in a function nothing called. No unit test
// noticed, because a fake network has no permissions to lack — it took booting
// a real guest on a real node to see it.
func TestTapLifecycleGoesThroughTheGuardedWrappers(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "lifecycle.go", nil, 0)
	if err != nil {
		t.Fatalf("parse lifecycle.go: %v", err)
	}

	// The wrappers are the one place the direct calls belong: they are what
	// the guard guards.
	allowed := map[string]bool{"createTap": true, "deleteTap": true}

	ast.Inspect(file, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || allowed[fn.Name.Name] {
			return true
		}
		ast.Inspect(fn, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			// d.net.CreateTap(...) — a selector on a selector.
			outer, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			inner, ok := outer.X.(*ast.SelectorExpr)
			if !ok || inner.Sel.Name != "net" {
				return true
			}
			if outer.Sel.Name == "CreateTap" || outer.Sel.Name == "DeleteTap" {
				t.Errorf("%s: %s calls d.net.%s directly.\n"+
					"Use d.%s instead: under the privileged helper this process has no "+
					"NET_ADMIN and the tap is the helper's to create. Read this test's "+
					"doc comment before changing it.",
					fset.Position(call.Pos()), fn.Name.Name, outer.Sel.Name,
					strings.ToLower(outer.Sel.Name[:1])+outer.Sel.Name[1:])
			}
			return true
		})
		return true
	})
}
