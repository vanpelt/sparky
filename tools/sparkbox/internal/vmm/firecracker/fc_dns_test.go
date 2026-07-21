//go:build linux

package firecracker

import "testing"

func TestGuestDNSArg(t *testing.T) {
	cases := []struct {
		name     string
		guestDNS string
		gateway  string
		want     string
	}{
		{"disabled", "", "172.30.7.1", ""},
		{"gateway sentinel expands", "gateway", "172.30.7.1", " sparkbox_dns=172.30.7.1"},
		{"literal address verbatim", "10.0.0.53", "172.30.7.1", " sparkbox_dns=10.0.0.53"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := guestDNSArg(c.guestDNS, c.gateway); got != c.want {
				t.Errorf("guestDNSArg(%q, %q) = %q, want %q", c.guestDNS, c.gateway, got, c.want)
			}
		})
	}
}
