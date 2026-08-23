package main

import "testing"

func TestAdvertisedPort(t *testing.T) {
	tests := []struct {
		name       string
		advertised int
		listen     string
		want       int
	}{
		{name: "listen port by default", listen: ":8081", want: 8081},
		{name: "public load balancer port", advertised: 443, listen: ":8081", want: 443},
		{name: "concrete listen address", listen: "127.0.0.1:2222", want: 2222},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := advertisedPort(tt.advertised, tt.listen); got != tt.want {
				t.Fatalf("advertisedPort(%d, %q) = %d; want %d",
					tt.advertised, tt.listen, got, tt.want)
			}
		})
	}
}

func TestAdvertisedHost(t *testing.T) {
	if got := advertisedHost("", "example.com"); got != "example.com" {
		t.Fatalf("empty override = %q; want fallback", got)
	}
	if got := advertisedHost("ssh.example.com", "example.com"); got != "ssh.example.com" {
		t.Fatalf("explicit override = %q; want ssh.example.com", got)
	}
}
