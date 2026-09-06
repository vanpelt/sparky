//go:build linux

package guestargs

import "testing"

func TestGuestDNSArg(t *testing.T) {
	cases := []struct {
		name     string
		guestDNS string
		gateway  string
		want     string
		wantErr  bool
	}{
		{name: "disabled", guestDNS: "", gateway: "172.30.7.1", want: ""},
		{name: "gateway sentinel expands", guestDNS: "gateway", gateway: "172.30.7.1", want: " sparkbox_dns=172.30.7.1"},
		{name: "literal address verbatim", guestDNS: "10.0.0.53", gateway: "172.30.7.1", want: " sparkbox_dns=10.0.0.53"},
		{name: "ipv6 literal", guestDNS: "2001:4860:4860::8888", gateway: "172.30.7.1", want: " sparkbox_dns=2001:4860:4860::8888"},
		{name: "hostname rejected", guestDNS: "dns.example.com", gateway: "172.30.7.1", wantErr: true},
		{name: "whitespace injection rejected", guestDNS: "1.1.1.1 init=/bin/sh", gateway: "172.30.7.1", wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := DNSArg(c.guestDNS, c.gateway)
			if c.wantErr {
				if err == nil {
					t.Fatalf("DNSArg(%q, %q) = %q, want error", c.guestDNS, c.gateway, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("DNSArg(%q, %q) unexpected error: %v", c.guestDNS, c.gateway, err)
			}
			if got != c.want {
				t.Errorf("DNSArg(%q, %q) = %q, want %q", c.guestDNS, c.gateway, got, c.want)
			}
		})
	}
}
