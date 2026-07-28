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

// The bug this fixes: a long-lived connection re-resolves rarely, its allow-set
// entry lapses and is swept, and the reporter — joining CUMULATIVE counters at
// report time — then had no name for the address at all. A sandbox's busiest
// destination redrew itself as a bare IP.
func TestLabelOutlivesTheAllowSet(t *testing.T) {
	m, now := clocked()
	m.MinTTL, m.Grace = 0, 0
	api := netip.MustParseAddr("160.79.104.10")

	m.Record("api.anthropic.com", []netip.Addr{api}, 60*time.Second)
	*now = now.Add(90 * time.Second)
	if dropped := m.Sweep(); len(dropped) != 1 {
		t.Fatalf("Sweep dropped %v, want the expired address", dropped)
	}

	// Gone from the allow-set — that part must not soften.
	if m.Allowed(api) {
		t.Error("expired address still allowed")
	}
	if _, ok := m.Domain(api); ok {
		t.Error("Domain still answers for an expired address")
	}
	// ...but the panel can still say who it was.
	if d, ok := m.Label(api); !ok || d != "api.anthropic.com" {
		t.Fatalf("Label = %q,%v; want api.anthropic.com,true", d, ok)
	}

	// It does not remember forever.
	*now = now.Add(m.LabelTTL + time.Minute)
	if d, ok := m.Label(api); ok {
		t.Errorf("Label = %q after LabelTTL; want it forgotten", d)
	}
}

// A live answer wins over a remembered one, so a re-pointed CDN address reports
// under the name it currently serves.
func TestLabelPrefersTheLiveAnswer(t *testing.T) {
	m, now := clocked()
	m.MinTTL, m.Grace = 0, 0
	a := netip.MustParseAddr("151.101.1.1")

	m.Record("old.example", []netip.Addr{a}, 60*time.Second)
	*now = now.Add(90 * time.Second)
	m.Record("new.example", []netip.Addr{a}, 60*time.Second)

	if d, ok := m.Label(a); !ok || d != "new.example" {
		t.Fatalf("Label = %q,%v; want new.example,true", d, ok)
	}
}

// Label memory is bounded: a sandbox resolving endless unique addresses must
// not grow it without limit. Oldest names go first.
func TestLabelMemoryIsCapped(t *testing.T) {
	m, now := clocked()
	m.MinTTL, m.Grace = 0, 0
	m.MaxLabels = 4

	for i := range 10 {
		a := netip.AddrFrom4([4]byte{198, 51, 100, byte(i)})
		m.Record("host.example", []netip.Addr{a}, time.Minute)
		*now = now.Add(time.Second)
	}
	m.mu.RLock()
	n := len(m.labels)
	m.mu.RUnlock()
	if n != 4 {
		t.Fatalf("labels retained = %d, want the cap of 4", n)
	}

	// Retire every allow-set entry so Label can only answer from memory, which
	// is where the cap applies.
	*now = now.Add(2 * time.Minute)
	m.Sweep()
	if _, ok := m.Label(netip.AddrFrom4([4]byte{198, 51, 100, 9})); !ok {
		t.Error("newest label was evicted")
	}
	if _, ok := m.Label(netip.AddrFrom4([4]byte{198, 51, 100, 0})); ok {
		t.Error("oldest label survived the cap")
	}
}

// Pinned infrastructure is never handed out by DNS; it must still label.
func TestPinIsLabelled(t *testing.T) {
	m, _ := clocked()
	gw := netip.MustParseAddr("172.30.5.1")
	m.Pin("gateway", gw)
	if d, ok := m.Label(gw); !ok || d != "gateway" {
		t.Fatalf("Label = %q,%v; want gateway,true", d, ok)
	}
}
