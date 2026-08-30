//go:build linux

package firecracker

import (
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
	return &Driver{guestNet: guestnet.MustParse("")}
}
