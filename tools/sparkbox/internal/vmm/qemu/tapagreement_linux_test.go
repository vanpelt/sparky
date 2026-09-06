//go:build linux

package qemu

import (
	"testing"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/guestnet"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/netpush"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm/hostnet"
)

// THE TEST THAT WOULD HAVE CAUGHT IT.
//
// A tap name is agreed between processes that never call each other: this
// driver creates the device, netpush pushes per-tap egress policy for it, and
// sluice's meter attaches to anything with the prefix. Each of them derived the
// name independently and nothing compared the answers, so when the direct path
// used a different prefix the only symptom was egress policy that named a
// device that did not exist — no error, no log line, just a sandbox with
// unfiltered egress.
//
// Comparing them here is cheap and the failure is legible. Doing it per SLOT
// rather than per prefix also pins the derivation, not just the string: netpush
// starts from a guest IP and works back to an index, which is the step that
// could silently disagree if either side's slot arithmetic moved.
func TestTapNamesAgreeWithNetpush(t *testing.T) {
	const subnet = "172.30.0.0/20"
	network := guestnet.MustParse(subnet)
	d := &Driver{net: hostnet.Plumbing{Net: network, TapPrefix: defaultTapPrefix}}

	for _, idx := range []int{0, 1, 7, 64, 1023} {
		slot, err := network.Slot(idx)
		if err != nil {
			t.Fatalf("slot %d: %v", idx, err)
		}
		guestIP := slot.Guest.String()
		want := d.tapName(idx)
		got, ok := netpush.TapNameForSubnet(subnet, guestIP)
		if !ok {
			t.Fatalf("netpush could not derive a tap for slot %d (%s)", idx, guestIP)
		}
		if got != want {
			t.Errorf("slot %d: the driver creates %q and netpush pushes policy for %q", idx, want, got)
		}
	}
}
