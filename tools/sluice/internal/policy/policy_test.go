package policy

import (
	"net/netip"
	"testing"
	"time"

	"github.com/vanpelt/sparky/tools/sluice/internal/allowlist"
	"github.com/vanpelt/sparky/tools/sluice/internal/ipmap"
)

func mustList(t *testing.T, patterns ...string) *allowlist.List {
	t.Helper()
	l, err := allowlist.New(patterns)
	if err != nil {
		t.Fatalf("allowlist.New(%v): %v", patterns, err)
	}
	return l
}

func addr(s string) netip.Addr { return netip.MustParseAddr(s) }

func TestAllowedUnion(t *testing.T) {
	p := New(mustList(t, "base.com"))
	p.ReplaceTaps(map[string]*allowlist.List{
		"sbtap3": mustList(t, "github.com"),
		"sbtap4": mustList(t, "example.org"),
	})
	cases := map[string]bool{
		"base.com":       true,  // base
		"api.github.com": true,  // a tap's subdomain rule
		"example.org":    true,  // another tap
		"nope.net":       false, // no one
	}
	for name, want := range cases {
		if got, _ := p.Allowed(name); got != want {
			t.Errorf("Allowed(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestBaseGrantsIncludesPinnedAndBaseMatches(t *testing.T) {
	p := New(mustList(t, "github.com"))
	future := time.Now().Add(time.Hour)
	snap := []ipmap.Entry{
		{Addr: addr("10.0.0.1"), Domain: "gateway", Expire: time.Time{}},   // pinned infra
		{Addr: addr("140.82.112.3"), Domain: "github.com", Expire: future}, // base match
		{Addr: addr("1.2.3.4"), Domain: "youtube.com", Expire: future},     // not in base
	}
	got := map[netip.Addr]bool{}
	for _, a := range p.BaseGrants(snap) {
		got[a] = true
	}
	if !got[addr("10.0.0.1")] {
		t.Error("pinned infra must always be granted fleet-wide")
	}
	if !got[addr("140.82.112.3")] {
		t.Error("base-matched resolved address must be granted")
	}
	if got[addr("1.2.3.4")] {
		t.Error("address for a non-base domain must not be in the wildcard set")
	}
}

func TestTapGrantsAreScopedAndSkipInfra(t *testing.T) {
	p := New(mustList(t, "github.com"))
	p.ReplaceTaps(map[string]*allowlist.List{
		"sbtap3": mustList(t, "youtube.com"),
	})
	future := time.Now().Add(time.Hour)
	snap := []ipmap.Entry{
		{Addr: addr("10.0.0.1"), Domain: "gateway", Expire: time.Time{}},   // infra: wildcard, not per-tap
		{Addr: addr("1.2.3.4"), Domain: "youtube.com", Expire: future},     // tap3's domain
		{Addr: addr("140.82.112.3"), Domain: "github.com", Expire: future}, // base only, not tap3
	}
	got := map[netip.Addr]bool{}
	for _, a := range p.TapGrants("sbtap3", snap) {
		got[a] = true
	}
	if !got[addr("1.2.3.4")] {
		t.Error("tap must be granted its own policy's resolved address")
	}
	if got[addr("10.0.0.1")] {
		t.Error("infra belongs to the wildcard, not per-tap grants")
	}
	if got[addr("140.82.112.3")] {
		t.Error("a base-only domain must not leak into a tap's grants")
	}
	// A tap with no policy grants nothing extra.
	if g := p.TapGrants("sbtap9", snap); len(g) != 0 {
		t.Errorf("unconfigured tap should grant nothing, got %v", g)
	}
}

func TestAllowedForPerClient(t *testing.T) {
	p := New(mustList(t, "base.com"))
	p.ReplaceTaps(map[string]*allowlist.List{
		"sbtap3": mustList(t, "github.com"), // policied VM in /30 slot 3
	})

	policied := addr("172.30.0.14") // sbtap3, has a policy
	untagged := addr("172.30.0.30") // sbtap7, no policy
	host := addr("10.9.9.9")        // not a guest at all

	// Default mode (defaultAllow=false): a tap with no policy is base-only.
	if ok, _ := p.AllowedFor(untagged, "youtube.com"); ok {
		t.Error("classic mode: untagged tap must not resolve a non-base name")
	}
	if ok, _ := p.AllowedFor(untagged, "base.com"); !ok {
		t.Error("classic mode: base list is a floor for every tap")
	}

	// Now open-untagged: a tap with no policy resolves anything.
	p.SetDefaultAllow(true)
	if ok, _ := p.AllowedFor(untagged, "youtube.com"); !ok {
		t.Error("open-untagged: a tap with no policy must resolve any name")
	}
	if ok, _ := p.AllowedFor(host, "anything.example"); !ok {
		t.Error("open-untagged: a non-guest client must resolve any name")
	}

	// A policied tap resolves only base ∪ its own list, regardless of mode.
	if ok, _ := p.AllowedFor(policied, "api.github.com"); !ok {
		t.Error("policied tap must resolve its own allowlisted subdomain")
	}
	if ok, _ := p.AllowedFor(policied, "base.com"); !ok {
		t.Error("policied tap must still resolve the base list")
	}
	if ok, _ := p.AllowedFor(policied, "youtube.com"); ok {
		t.Error("policied tap must NOT resolve a name outside its list even in open-untagged mode")
	}
}

func TestIsEnforcedTracksPerTapPolicy(t *testing.T) {
	p := New(nil)
	if p.IsEnforced("sbtap3") {
		t.Error("a tap with no policy must not be enforced")
	}
	// An empty allowlist is still a policy — a deny-all — so it enforces.
	p.ReplaceTaps(map[string]*allowlist.List{"sbtap3": mustList(t)})
	if !p.IsEnforced("sbtap3") {
		t.Error("a tap present in the policy set is enforced, even with an empty allowlist")
	}
	if p.IsEnforced("sbtap4") {
		t.Error("an unrelated tap is not enforced")
	}
}

func TestTapForGuest(t *testing.T) {
	p := New(nil)
	cases := map[string]string{
		"172.30.0.2":  "sbtap0",
		"172.30.0.14": "sbtap3",
		"172.30.5.2":  "sbtap320",
		"172.30.0.13": "", // gateway, not the guest
		"172.30.0.15": "", // broadcast, not the guest
		"10.0.0.2":    "", // not the sandbox subnet
		"::1":         "", // IPv6 loopback
	}
	for in, want := range cases {
		if got := p.TapForClient(addr(in)); got != want {
			t.Errorf("tapForGuest(%s) = %q, want %q", in, got, want)
		}
	}
	// A v4-mapped v6 address should still resolve to its tap.
	if got := p.TapForClient(netip.MustParseAddr("::ffff:172.30.5.2")); got != "sbtap320" {
		t.Errorf("tapForGuest(v4-mapped) = %q, want sbtap320", got)
	}
	if err := p.SetGuestSubnet("10.44.16.0/20"); err != nil {
		t.Fatal(err)
	}
	if got := p.TapForClient(addr("10.44.17.6")); got != "sbtap65" {
		t.Errorf("configured subnet mapping = %q, want sbtap65", got)
	}
}

func TestReplaceTapsIsAtomicSwap(t *testing.T) {
	p := New(nil)
	p.ReplaceTaps(map[string]*allowlist.List{"sbtap3": mustList(t, "a.com")})
	if pats := p.TapPatterns("sbtap3"); len(pats) != 1 || pats[0] != "a.com" {
		t.Fatalf("TapPatterns = %v, want [a.com]", pats)
	}
	p.ReplaceTaps(map[string]*allowlist.List{"sbtap4": mustList(t, "b.com")})
	if p.TapPatterns("sbtap3") != nil {
		t.Error("replaced-away tap should have no patterns")
	}
	if pats := p.TapPatterns("sbtap4"); len(pats) != 1 || pats[0] != "b.com" {
		t.Errorf("TapPatterns(sbtap4) = %v, want [b.com]", pats)
	}
}

// The meter attaches to interfaces by prefix and this file derives per-tap
// policy names by prefix, from two different places. They used to be able to
// disagree — the flag moved one and the other was a literal — and the symptom
// was silent: every per-tap rule looked up under a name no interface had, so
// every DNS decision fell through to the base list and every denial was
// attributed to "".
func TestTapPrefixFollowsTheConfiguredOne(t *testing.T) {
	p := New(nil)
	if got := p.TapForClient(addr("172.30.0.2")); got != "sbtap0" {
		t.Fatalf("default TapForClient = %q, want sbtap0", got)
	}

	p.SetTapPrefix("sbqtap")
	if got := p.TapForClient(addr("172.30.0.2")); got != "sbqtap0" {
		t.Errorf("after SetTapPrefix, TapForClient = %q, want sbqtap0", got)
	}
	// Per-tap policy has to be reachable under the configured name, which is
	// the thing that was actually broken rather than the string itself.
	p.SetTapPrefix("")
	if got := p.TapForClient(addr("172.30.0.2")); got != "sbtap0" {
		t.Errorf("an empty prefix = %q, want the default sbtap0", got)
	}
}
