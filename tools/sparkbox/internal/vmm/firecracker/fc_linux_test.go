//go:build linux

package firecracker

import (
	"net"
	"testing"
)

// TestV6Addressing checks the per-slot /127 carving from the delegated /64.
// Constructs the Driver directly to avoid New()'s /dev/kvm requirement.
func TestV6Addressing(t *testing.T) {
	_, ipNet, err := net.ParseCIDR("2001:bc8:702:1c7::/64")
	if err != nil {
		t.Fatal(err)
	}
	d := &Driver{prefix6: ipNet.IP.To16()}

	cases := []struct {
		idx         int
		host, guest string
	}{
		{1, "2001:bc8:702:1c7::2", "2001:bc8:702:1c7::3"},
		{2, "2001:bc8:702:1c7::4", "2001:bc8:702:1c7::5"},
		{255, "2001:bc8:702:1c7::1fe", "2001:bc8:702:1c7::1ff"},
	}
	for _, c := range cases {
		if got := d.hostIP6(c.idx); got != c.host {
			t.Errorf("idx %d hostIP6 = %s, want %s", c.idx, got, c.host)
		}
		if got := d.guestIP6(c.idx); got != c.guest {
			t.Errorf("idx %d guestIP6 = %s, want %s", c.idx, got, c.guest)
		}
		// host must be the even (network) address of the /127, guest the odd one.
		if hi := net.ParseIP(c.host).To16()[15]; hi%2 != 0 {
			t.Errorf("idx %d host address not /127-aligned: %s", c.idx, c.host)
		}
	}
}
