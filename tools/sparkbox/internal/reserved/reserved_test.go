package reserved

import "testing"

// The doors that exist today. A regression here is not a style problem: each of
// these has a handler mounted at it, and reserved dispatch runs before the route
// lookup, so a sandbox or route allowed to take one would be created, listed,
// and then never served.
func TestLiveDoorsAreReserved(t *testing.T) {
	for _, n := range []string{
		"console", "my", "api", "login", "oidc", "xterm", "hooks", // proxy edge
		"new", "ctl", "signup", "node", // ssh gateway usernames
	} {
		if !Name(n) {
			t.Errorf("Name(%q) = false — something answers there today", n)
		}
	}
}

func TestSuffixesAreClaimedHoweverTheyBegin(t *testing.T) {
	for _, n := range []string{"demo-xterm", "web-xterm", "fuzzy-otter-2-xterm", "a.b-xterm"} {
		if !Name(n) {
			t.Errorf("Name(%q) = false, want true", n)
		}
	}
}

// The reservation is the whole name or a whole trailing segment — never a
// substring. Over-reserving is a silent, unexplained rejection at create time,
// which is its own kind of bad.
func TestOrdinaryNamesAreNotSweptUp(t *testing.T) {
	for _, n := range []string{
		"xtermite", "myxterm", "xterm-web", "admin-panel", "apis", "console-2",
		"crafty-axolotl", "demo", "foo", "bar", "fancy", "bubbly-wren",
	} {
		if Name(n) {
			t.Errorf("Name(%q) = true, want false", n)
		}
	}
}

// A dotted route subdomain is matched whole, because that is exactly how the
// edge dispatches: proxy.Server.reserved is an exact-match lookup on the entire
// subdomain string, so "api.myvm" reaches the route table and only a bare "api"
// is claimed. Reserving the dotted form would refuse a route nothing shadows.
func TestDottedSubdomainsMatchTheEdge(t *testing.T) {
	if Name("api.myvm") {
		t.Error(`Name("api.myvm") = true, but the edge would route it`)
	}
	if !Name("api") {
		t.Error(`Name("api") = false`)
	}
}

func TestNameIsCaseInsensitive(t *testing.T) {
	for _, n := range []string{"API", "Console", "DEMO-XTERM"} {
		if !Name(n) {
			t.Errorf("Name(%q) = false, want true", n)
		}
	}
}

// All and Suffixes are the operator-facing view; a caller must not be able to
// mutate the lists through them.
func TestAccessorsDoNotAliasTheLists(t *testing.T) {
	if got := Suffixes(); len(got) == 0 {
		t.Fatal("Suffixes() is empty")
	} else {
		got[0] = "-clobbered"
		if !Name("demo-xterm") {
			t.Error("mutating the Suffixes() result changed the reservation")
		}
	}
	if len(All()) < len(names) {
		t.Errorf("All() returned %d of %d names", len(All()), len(names))
	}
}
