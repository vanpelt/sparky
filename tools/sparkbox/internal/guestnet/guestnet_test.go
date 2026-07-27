package guestnet

import (
	"net/netip"
	"testing"
)

func TestParseNormalizesAndDefaults(t *testing.T) {
	tests := []struct {
		raw      string
		want     string
		capacity int
	}{
		{"", DefaultPrefix, 16384},
		{"10.24.7.9/20", "10.24.0.0/20", 1024},
		{"192.0.2.0/30", "192.0.2.0/30", 1},
	}
	for _, test := range tests {
		network, err := Parse(test.raw)
		if err != nil {
			t.Fatalf("Parse(%q): %v", test.raw, err)
		}
		if got := network.String(); got != test.want {
			t.Errorf("Parse(%q) = %s, want %s", test.raw, got, test.want)
		}
		if got := network.Capacity(); got != test.capacity {
			t.Errorf("Parse(%q) capacity = %d, want %d", test.raw, got, test.capacity)
		}
	}
}

func TestParseRejectsInvalidGuestNetworks(t *testing.T) {
	for _, raw := range []string{"not-a-cidr", "2001:db8::/64", "10.0.0.0/31", "10.0.0.1"} {
		if _, err := Parse(raw); err == nil {
			t.Errorf("Parse(%q) succeeded", raw)
		}
	}
}

func TestSlotAddressingAndReverseLookup(t *testing.T) {
	network := MustParse("10.44.16.0/20")
	tests := []struct {
		index       int
		prefix      string
		host, guest string
	}{
		{0, "10.44.16.0/30", "10.44.16.1", "10.44.16.2"},
		{1, "10.44.16.4/30", "10.44.16.5", "10.44.16.6"},
		{1023, "10.44.31.252/30", "10.44.31.253", "10.44.31.254"},
	}
	for _, test := range tests {
		slot, err := network.Slot(test.index)
		if err != nil {
			t.Fatalf("Slot(%d): %v", test.index, err)
		}
		if slot.Prefix.String() != test.prefix || slot.Host.String() != test.host || slot.Guest.String() != test.guest {
			t.Errorf("Slot(%d) = {%s %s %s}, want {%s %s %s}",
				test.index, slot.Prefix, slot.Host, slot.Guest, test.prefix, test.host, test.guest)
		}
		if got, ok := network.SlotForGuest(slot.Guest); !ok || got != test.index {
			t.Errorf("SlotForGuest(%s) = %d, %v", slot.Guest, got, ok)
		}
		if got, ok := network.SlotForHost(slot.Host); !ok || got != test.index {
			t.Errorf("SlotForHost(%s) = %d, %v", slot.Host, got, ok)
		}
	}
	if _, err := network.Slot(1024); err == nil {
		t.Fatal("Slot(capacity) succeeded")
	}
	for _, raw := range []string{"10.44.16.1", "10.44.16.3", "10.44.32.2", "127.0.0.1"} {
		if _, ok := network.SlotForGuest(netip.MustParseAddr(raw)); ok {
			t.Errorf("SlotForGuest(%s) succeeded", raw)
		}
	}
	if got, ok := network.SlotContaining(netip.MustParseAddr("10.44.17.5")); !ok || got != 65 {
		t.Errorf("SlotContaining(10.44.17.5) = %d, %v; want 65, true", got, ok)
	}
}

func TestOverlaps(t *testing.T) {
	tests := []struct {
		a, b string
		want bool
	}{
		{"10.20.0.0/20", "10.20.8.0/21", true},
		{"10.20.0.0/20", "10.20.16.0/20", false},
		{"10.20.0.1/24", "10.20.0.240/28", true},
		{"10.20.0.0/20", "2001:db8::/64", false},
	}
	for _, test := range tests {
		if got := Overlaps(netip.MustParsePrefix(test.a), netip.MustParsePrefix(test.b)); got != test.want {
			t.Errorf("Overlaps(%s, %s) = %v, want %v", test.a, test.b, got, test.want)
		}
	}
}
