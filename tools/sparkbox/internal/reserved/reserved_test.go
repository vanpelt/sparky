package reserved

import "testing"

// The doors that exist today. A regression here is not a style problem: each of
// these has a handler mounted at it, and reserved dispatch runs before the route
// lookup, so a sandbox or route allowed to take one would be created, listed,
// and then never served.
//
// `go` joined this list when cmd/sparkbox began mounting internal/launch there
// (--launch-subdomain), and it is the one entry whose reservation outlives the
// deployment: the button pasted into a public GitHub comment points at
// go.<domain>, and that comment is immutable. Freeing this label would not
// merely relabel a surface — it would hand whoever took it every click of every
// link already written.
func TestLiveDoorsAreReserved(t *testing.T) {
	for _, n := range []string{
		"console", "my", "api", "login", "oidc", "xterm", "hooks", "go", // proxy edge
		"new", "ctl", "signup", "node", // ssh gateway usernames
	} {
		if !Name(n) {
			t.Errorf("Name(%q) = false — something answers there today", n)
		}
	}
}

// Names claimed BEFORE anything is mounted at them. This is the direction the
// package doc says is cheap — a name with no handler is refused rather than
// silently shadowed — and it is the only direction that works, because a label
// cannot be taken back from whoever created a sandbox at it first.
//
// `go` used to be here and has moved to TestLiveDoorsAreReserved, because
// cmd/sparkbox now mounts internal/launch at it. `launch` stays: it is the word
// somebody types when they half-remember the door, and nothing answers there.
func TestNamesClaimedAheadOfTheirHandler(t *testing.T) {
	for _, n := range []string{
		"launch",                   // the word people guess for the launch door at `go`
		"terminal", "shell", "ssh", // synonyms for surfaces that exist
		"webhook", "webhooks", "nodes", "sandbox", "sandboxes",
		"secrets", "keys", "token", "tokens", "id", "identity",
		"repos", "tags", "agent", "agents", "mcp", "badge", "badges",
		"github", "git", // trusted-by-familiarity on our zone
		"secure", "update", "updates", "download", "downloads", "install",
		"pay", "payment", "payments", "checkout",
		"wpad", "mta-sts", "acme", "localhost", // protocol auto-discovery
	} {
		if !Name(n) {
			t.Errorf("Name(%q) = false — this label is claimed", n)
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
		// The short additions are the ones most at risk of sweeping up an
		// ordinary name, and `go` is two characters long. Exact match is what
		// keeps these free; a careless switch to prefix matching takes them all.
		"gopher", "going", "golang", "webapp", "gitea", "appetite",
		// `app` and `web` are claimed by NOBODY, on purpose — see the note
		// beside "github" in reserved.go. internal/routes and internal/nodelink
		// both name their fixture sandbox "web", so this line is what tells a
		// future reader that their passing tests are a decision, not an accident.
		"app", "web",
		// Words that merely CONTAIN a claimed one, from the groups added
		// alongside the launch door. A person's sandbox is allowed to be about
		// tokens without being the platform's page about tokens.
		"token-service", "my-secrets", "update-checker", "dev", "test", "staging",
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
