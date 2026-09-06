//go:build linux

package hostnet

import (
	"net"
	"testing"
)

// A /112 prefix leaves only 16 host bits, so bytes 12 and 13 of the address
// carry PREFIX, not offset. Assigning the offset across all four low bytes
// silently moves the guest outside the prefix; this pins the OR.
func TestAddr6PreservesPrefixBitsBelow96(t *testing.T) {
	for _, tc := range []struct {
		name   string
		prefix string
		off    int
		want   string
	}{
		{"64 bit prefix", "2001:db8::", 3, "2001:db8::3"},
		{"112 bit prefix keeps bytes 12-13", "2001:db8::abcd:0", 3, "2001:db8::abcd:3"},
		{"112 bit prefix, larger offset", "2001:db8::abcd:0", 0x102, "2001:db8::abcd:102"},
		{"offset spanning three bytes", "2001:db8::", 0x010203, "2001:db8::1:203"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := Plumbing{Prefix6: net.ParseIP(tc.prefix).To16()}
			if got := p.Addr6(tc.off); got != tc.want {
				t.Errorf("Addr6(%d) on %s = %s, want %s", tc.off, tc.prefix, got, tc.want)
			}
		})
	}
}

// The host takes the even address of each slot's pair and the guest the odd
// one, and ::1 stays free for the edge. Every layer below derives addresses
// from this, so it is pinned rather than left to a comment.
func TestSlotAddressConvention(t *testing.T) {
	p := Plumbing{Prefix6: net.ParseIP("2001:db8::").To16()}
	for idx, want := range map[int][2]string{
		0: {"2001:db8::2", "2001:db8::3"},
		1: {"2001:db8::4", "2001:db8::5"},
		7: {"2001:db8::10", "2001:db8::11"},
	} {
		if got := p.HostIP6(idx); got != want[0] {
			t.Errorf("HostIP6(%d) = %s, want %s", idx, got, want[0])
		}
		if got := p.GuestIP6(idx); got != want[1] {
			t.Errorf("GuestIP6(%d) = %s, want %s", idx, got, want[1])
		}
	}
}

// The two drivers must never hand out the same MAC on a host where both run.
func TestMACSeparatesDrivers(t *testing.T) {
	if got, want := MAC(0x00, 0), "02:5b:00:00:00:00"; got != want {
		t.Errorf("MAC = %s, want %s", got, want)
	}
	if got, want := MAC(0x01, 258), "02:5b:01:00:01:02"; got != want {
		t.Errorf("MAC = %s, want %s", got, want)
	}
	if MAC(0x00, 5) == MAC(0x01, 5) {
		t.Error("the same slot must not produce the same MAC under two drivers")
	}
}

func TestTapNameUsesThePrefix(t *testing.T) {
	if got := (Plumbing{TapPrefix: "sbtap"}).TapName(12); got != "sbtap12" {
		t.Errorf("TapName = %s", got)
	}
	// Neither prefix may be a prefix of the other, or one driver's sweep eats
	// the other's live taps.
	if got := (Plumbing{TapPrefix: "sbqtap"}).TapName(12); got != "sbqtap12" {
		t.Errorf("TapName = %s", got)
	}
}
