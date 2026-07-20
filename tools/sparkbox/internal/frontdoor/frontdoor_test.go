package frontdoor

import (
	"net"
	"strings"
	"testing"
)

const testSubnet = "2001:db8:702:1c7::/64"

func mustMapper(t *testing.T) *Mapper {
	t.Helper()
	m, err := New(testSubnet)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestAddrDeterministicAndDistinct(t *testing.T) {
	m := mustMapper(t)
	a1 := m.Addr("fuzzy-otter")
	a2 := m.Addr("fuzzy-otter")
	if !a1.Equal(a2) {
		t.Fatalf("same name gave different addresses: %s vs %s", a1, a2)
	}
	if a1.Equal(m.Addr("zesty-meteor")) {
		t.Fatalf("different names collided on %s", a1)
	}
	// A second mapper (fresh process) must agree — the address is what DNS
	// will publish, so it can never drift.
	if got := mustMapper(t).Addr("fuzzy-otter"); !got.Equal(a1) {
		t.Fatalf("derivation not stable across mappers: %s vs %s", got, a1)
	}
}

func TestAddrLandsInUpperHalf(t *testing.T) {
	m := mustMapper(t)
	for _, name := range []string{"a", "new", "ctl", "fuzzy-otter", "x-1-y-2"} {
		ip := m.Addr(name)
		if !strings.HasPrefix(ip.String(), "2001:db8:702:1c7:") {
			t.Fatalf("Addr(%q) = %s escaped the /64", name, ip)
		}
		if ip[8]&0x80 == 0 {
			t.Fatalf("Addr(%q) = %s is in the driver's lower /65", name, ip)
		}
		if !m.Contains(ip) {
			t.Fatalf("Contains(Addr(%q)) = false", name)
		}
	}
}

func TestContainsRejectsDriverAndForeignAddrs(t *testing.T) {
	m := mustMapper(t)
	for _, s := range []string{
		"2001:db8:702:1c7::1",      // host edge address
		"2001:db8:702:1c7::3",      // guest address (low 32 bits, slot 1)
		"2001:db8:702:1c8::",       // adjacent /64
		"2001:db8:702:1c8:8000::1", // upper half of the WRONG /64
		"::1",
		"127.0.0.1",
	} {
		if m.Contains(net.ParseIP(s)) {
			t.Fatalf("Contains(%s) = true, want false", s)
		}
	}
}

func TestRange(t *testing.T) {
	m := mustMapper(t)
	if got, want := m.Range(), "2001:db8:702:1c7:8000::/65"; got != want {
		t.Fatalf("Range() = %q, want %q", got, want)
	}
	// Every derived address must fall inside the claimed range.
	_, ipNet, err := net.ParseCIDR(m.Range())
	if err != nil {
		t.Fatal(err)
	}
	if !ipNet.Contains(m.Addr("fuzzy-otter")) {
		t.Fatal("Addr not contained in Range")
	}
}

func TestResolve(t *testing.T) {
	m := mustMapper(t)
	names := []string{"fuzzy-otter", "zesty-meteor"}

	if got, ok := m.Resolve(m.Addr("zesty-meteor"), names); !ok || got != "zesty-meteor" {
		t.Fatalf("Resolve = %q, %v", got, ok)
	}
	// In range, but no candidate matches.
	if got, ok := m.Resolve(m.Addr("ghost"), names); ok {
		t.Fatalf("Resolve of unknown name = %q, want miss", got)
	}
	// Out of range entirely.
	if _, ok := m.Resolve(net.ParseIP("2001:db8:702:1c7::3"), names); ok {
		t.Fatal("Resolve matched a non-front-door address")
	}
}

func TestNewRejectsBadSubnets(t *testing.T) {
	for _, s := range []string{
		"",
		"not-a-cidr",
		"2001:db8::/80", // narrower than /64: interface ID not ours
		"10.0.0.0/8",    // IPv4
	} {
		if _, err := New(s); err == nil {
			t.Fatalf("New(%q) accepted, want error", s)
		}
	}
	// Wider than /64 is fine.
	if _, err := New("2001:db8::/56"); err != nil {
		t.Fatalf("New(/56): %v", err)
	}
}
