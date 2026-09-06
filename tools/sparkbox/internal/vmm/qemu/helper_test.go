//go:build linux

package qemu

import (
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
	direct := &Driver{tapPrefix: defaultTapPrefix}
	if got, want := direct.tapName(7), "sbtap7"; got != want {
		t.Errorf("direct tapName = %q, want %q", got, want)
	}

	// The override exists only so the parity suite can run both drivers on one
	// host. It must still reach tapName, or that suite silently stops being a
	// two-driver test.
	odd := &Driver{tapPrefix: "sbqtap"}
	if got, want := odd.tapName(7), "sbqtap7"; got != want {
		t.Errorf("overridden tapName = %q, want %q", got, want)
	}
}

// newForTapName resolves the prefix the way New does without needing a binary,
// a kernel or a subnet on the machine running the test.
func newForTapName(opts Options) (*Driver, error) {
	d := &Driver{opts: opts, tapPrefix: opts.TapPrefix}
	if d.tapPrefix == "" {
		d.tapPrefix = defaultTapPrefix
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
