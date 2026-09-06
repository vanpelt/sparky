//go:build linux

package firecracker

import (
	"testing"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/guestnet"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/netpush"
)

// The firecracker half of internal/vmm/qemu's TestTapNamesAgreeWithNetpush.
// This driver's prefix was always the one netpush assumes; the test exists so
// that stays true from both ends rather than by coincidence.
func TestTapNamesAgreeWithNetpush(t *testing.T) {
	const subnet = "172.30.0.0/20"
	network := guestnet.MustParse(subnet)

	for _, idx := range []int{0, 1, 7, 64, 1023} {
		slot, err := network.Slot(idx)
		if err != nil {
			t.Fatalf("slot %d: %v", idx, err)
		}
		guestIP := slot.Guest.String()
		want := tapName(idx)
		got, ok := netpush.TapNameForSubnet(subnet, guestIP)
		if !ok {
			t.Fatalf("netpush could not derive a tap for slot %d (%s)", idx, guestIP)
		}
		if got != want {
			t.Errorf("slot %d: the driver creates %q and netpush pushes policy for %q", idx, want, got)
		}
	}
}
