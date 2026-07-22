package ipmap

import (
	"net/netip"
	"testing"
	"time"
)

func addrs(ss ...string) []netip.Addr {
	out := make([]netip.Addr, len(ss))
	for i, s := range ss {
		out[i] = netip.MustParseAddr(s)
	}
	return out
}

// clocked returns a Map whose clock is driven by the returned pointer, so tests
// can advance time deterministically.
func clocked() (*Map, *time.Time) {
	now := time.Unix(1_700_000_000, 0)
	m := New()
	m.now = func() time.Time { return now }
	return m, &now
}

func TestRecordAndExpire(t *testing.T) {
	m, now := clocked()
	m.MinTTL = 0 // don't clamp; exercise the raw ttl
	m.Grace = 0

	m.Record("github.com", addrs("140.82.112.3", "140.82.112.4"), 60*time.Second)

	if d, ok := m.Domain(netip.MustParseAddr("140.82.112.3")); !ok || d != "github.com" {
		t.Fatalf("Domain = %q,%v; want github.com,true", d, ok)
	}
	if !m.Allowed(netip.MustParseAddr("140.82.112.4")) {
		t.Fatal("expected .4 allowed")
	}

	*now = now.Add(59 * time.Second)
	if !m.Allowed(netip.MustParseAddr("140.82.112.3")) {
		t.Fatal("should still be allowed just before expiry")
	}

	*now = now.Add(2 * time.Second) // now 61s in, past expiry
	if m.Allowed(netip.MustParseAddr("140.82.112.3")) {
		t.Fatal("should be expired")
	}
	dropped := m.Sweep()
	if len(dropped) != 2 {
		t.Fatalf("Sweep dropped %d, want 2", len(dropped))
	}
	if m.Len() != 0 {
		t.Fatalf("Len after sweep = %d, want 0", m.Len())
	}
}

func TestTTLClamp(t *testing.T) {
	m, now := clocked()
	m.MinTTL = 5 * time.Minute
	m.Grace = 0
	m.Record("slow.example", addrs("1.1.1.1"), time.Second) // tiny ttl, clamped up

	*now = now.Add(4 * time.Minute)
	if !m.Allowed(netip.MustParseAddr("1.1.1.1")) {
		t.Fatal("MinTTL clamp should keep it alive at 4m")
	}
	*now = now.Add(2 * time.Minute)
	if m.Allowed(netip.MustParseAddr("1.1.1.1")) {
		t.Fatal("should expire past the 5m clamp")
	}
}

func TestPinNeverExpires(t *testing.T) {
	m, now := clocked()
	m.Pin("gateway", netip.MustParseAddr("172.30.5.1"))
	*now = now.Add(1000 * time.Hour)
	if d, ok := m.Domain(netip.MustParseAddr("172.30.5.1")); !ok || d != "gateway" {
		t.Fatalf("pinned entry vanished: %q,%v", d, ok)
	}
	if got := m.Sweep(); len(got) != 0 {
		t.Fatalf("Sweep removed a pinned entry: %v", got)
	}
}

func TestRecordExtendsAndRelabels(t *testing.T) {
	m, now := clocked()
	m.Grace = 0
	m.Record("old.example", addrs("9.9.9.9"), 10*time.Minute)
	// A newer answer for the same IP under a different name relabels it.
	m.Record("new.example", addrs("9.9.9.9"), 10*time.Minute)
	if d, _ := m.Domain(netip.MustParseAddr("9.9.9.9")); d != "new.example" {
		t.Fatalf("relabel failed: %q", d)
	}
	*now = now.Add(9 * time.Minute)
	if !m.Allowed(netip.MustParseAddr("9.9.9.9")) {
		t.Fatal("expiry should have been extended by the second Record")
	}
}

func TestV4MappedNormalisation(t *testing.T) {
	m, _ := clocked()
	// Record as v4-mapped v6; lookups by plain v4 must still hit.
	mapped := netip.MustParseAddr("::ffff:8.8.8.8")
	m.Record("dns.google", []netip.Addr{mapped}, time.Hour)
	if !m.Allowed(netip.MustParseAddr("8.8.8.8")) {
		t.Fatal("v4-mapped record not reachable via plain v4 lookup")
	}
}
