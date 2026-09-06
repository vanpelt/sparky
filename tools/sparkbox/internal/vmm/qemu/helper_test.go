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

// Pause writes to one path and promotes another. Under the helper QEMU is
// chrooted into its jail, so the runtime `migrate uri=file:` — resolved from
// the monitor long AFTER the chroot — must name the file relatively, while the
// rename operates on the hardlink the helper put in the VM directory.
func TestJailedSnapshotTargetIsRelativeAndPromotionIsNot(t *testing.T) {
	d := &Driver{opts: Options{
		VMStateDir:             "/state",
		PrivilegedHelperSocket: "/run/helper.sock",
		JailerChrootBase:       "/state/jailer",
	}}
	if got, want := d.snapshotTarget("box"), "state.migrate.next"; got != want {
		t.Errorf("snapshotTarget = %q, want %q", got, want)
	}
	if got := d.snapshotNextPath("box"); !strings.HasPrefix(got, "/state/qemu-vms/box/") {
		t.Errorf("snapshotNextPath = %q, want a path in the VM directory", got)
	}
	direct := &Driver{opts: Options{VMStateDir: "/state"}}
	if got, want := direct.snapshotTarget("box"), direct.snapshotNextPath("box"); got != want {
		t.Errorf("the direct launcher migrates to %q but promotes %q", got, want)
	}
}

// Under the helper the tap is created by the helper, in the Pod network
// namespace, and it is called sbtap<slot> — the name internal/netpush, sluice's
// --tap-prefix and deploy/sparkbox-net.sh all hardcode. On the direct path the
// prefix must stay distinct from the firecracker driver's, or one driver's
// startup sweep deletes the other's live networking.
func TestTapNameFollowsWhoeverCreatesTheDevice(t *testing.T) {
	jailed := &Driver{opts: Options{PrivilegedHelperSocket: "/run/helper.sock"}}
	if got, want := jailed.tapName(7), "sbtap7"; got != want {
		t.Errorf("jailed tapName = %q, want %q", got, want)
	}
	direct := &Driver{}
	if got, want := direct.tapName(7), "sbqtap7"; got != want {
		t.Errorf("direct tapName = %q, want %q", got, want)
	}
	// Neither prefix may be a prefix of the other, because each driver's sweep
	// matches its own by prefix.
	if strings.HasPrefix(tapPrefix, helperTapPrefix) || strings.HasPrefix(helperTapPrefix, tapPrefix) {
		t.Errorf("tap prefixes %q and %q overlap; one driver's sweep would eat the other's taps",
			tapPrefix, helperTapPrefix)
	}
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
