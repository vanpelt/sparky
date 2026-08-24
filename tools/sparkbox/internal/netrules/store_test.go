package netrules

import (
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "sparkbox.db"), nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// tag directly seeds a sandbox_tags row (owned by internal/secrets in prod).
func (s *Store) tag(t *testing.T, sandbox, owner, tag string) {
	t.Helper()
	if _, err := s.db.Exec(`INSERT INTO sandbox_tags (sandbox, owner, tag, created_at) VALUES (?,?,?,?)`,
		sandbox, owner, tag, time.Now().UTC()); err != nil {
		t.Fatalf("seed tag: %v", err)
	}
}

func TestPutListDeleteRoundTrip(t *testing.T) {
	s := openTest(t)
	spec := RuleSpec{Allow: []string{"github.com", "*.githubusercontent.com", "github.com"}}
	if err := s.PutRule("alice", "CI egress", spec, []string{"ci", "build"}); err != nil {
		t.Fatalf("PutRule: %v", err)
	}
	rules, err := s.ListRules("alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 1 {
		t.Fatalf("want 1 rule, got %d", len(rules))
	}
	r := rules[0]
	if r.Name != "CI egress" || r.Version != 1 {
		t.Errorf("meta = %+v", r)
	}
	// Deduped + sorted.
	if !reflect.DeepEqual(r.Spec.Allow, []string{"*.githubusercontent.com", "github.com"}) {
		t.Errorf("allow = %v", r.Spec.Allow)
	}
	if !reflect.DeepEqual(r.Tags, []string{"build", "ci"}) {
		t.Errorf("tags = %v", r.Tags)
	}

	// Update bumps version and replaces tags.
	if err := s.PutRule("alice", "CI egress", RuleSpec{Allow: []string{"pypi.org"}}, []string{"ci"}); err != nil {
		t.Fatal(err)
	}
	rules, _ = s.ListRules("alice")
	if rules[0].Version != 2 || !reflect.DeepEqual(rules[0].Tags, []string{"ci"}) {
		t.Errorf("after update: %+v", rules[0])
	}

	if err := s.DeleteRule("alice", "CI egress"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteRule("alice", "CI egress"); err != ErrNoSuchRule {
		t.Errorf("second delete = %v, want ErrNoSuchRule", err)
	}
}

func TestRulesForSandboxJoinsOnTagsAndScopesByOwner(t *testing.T) {
	s := openTest(t)
	// alice: a rule tagged "ci" applies to her sandbox tagged "ci".
	if err := s.PutRule("alice", "ci", RuleSpec{Allow: []string{"github.com"}}, []string{"ci"}); err != nil {
		t.Fatal(err)
	}
	// alice: a rule tagged "gpu" that the sandbox does NOT carry.
	if err := s.PutRule("alice", "gpu", RuleSpec{Allow: []string{"huggingface.co"}}, []string{"gpu"}); err != nil {
		t.Fatal(err)
	}
	// bob owns a rule tagged "ci" too — must never reach alice's sandbox.
	if err := s.PutRule("bob", "ci", RuleSpec{Allow: []string{"evil.example"}}, []string{"ci"}); err != nil {
		t.Fatal(err)
	}
	s.tag(t, "alice-box", "alice", "ci")

	rules, err := s.RulesForSandbox("alice-box", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 1 || rules[0].Name != "ci" {
		t.Fatalf("RulesForSandbox = %+v, want just alice's ci rule", rules)
	}

	allow, governed, err := s.AllowForSandbox("alice-box", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if !governed {
		t.Error("a sandbox with a tag bound to a rule must be governed")
	}
	if !reflect.DeepEqual(allow, []string{"github.com"}) {
		t.Errorf("merged allow = %v, want [github.com] (bob's + untagged rule excluded)", allow)
	}

	// A sandbox with no rule-bound tag is ungoverned (→ unrestricted egress).
	_, governed, err = s.AllowForSandbox("no-such-box", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if governed {
		t.Error("a sandbox with no governing rule must report ungoverned")
	}

	boxes, err := s.SandboxesForRule("alice", "ci")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(boxes, []string{"alice-box"}) {
		t.Errorf("SandboxesForRule = %v", boxes)
	}
}

func TestAllowForSandboxMergesMultipleRules(t *testing.T) {
	s := openTest(t)
	s.PutRule("alice", "base", RuleSpec{Allow: []string{"github.com"}}, []string{"ci"})
	s.PutRule("alice", "extra", RuleSpec{Allow: []string{"pypi.org", "github.com"}}, []string{"ci"})
	s.tag(t, "box", "alice", "ci")
	allow, governed, err := s.AllowForSandbox("box", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if !governed {
		t.Error("box tagged into two rules must be governed")
	}
	if !reflect.DeepEqual(allow, []string{"github.com", "pypi.org"}) {
		t.Errorf("merged allow = %v", allow)
	}
}

func TestValidation(t *testing.T) {
	s := openTest(t)
	if err := s.PutRule("alice", "bad name!!", RuleSpec{}, nil); err == nil {
		t.Error("bad rule name should fail")
	}
	if err := s.PutRule("alice", "ok", RuleSpec{Allow: []string{"http://x"}}, nil); err == nil {
		t.Error("pattern with scheme should fail")
	}
	if err := s.PutRule("alice", "ok", RuleSpec{Allow: []string{"*"}}, nil); err == nil {
		t.Error("bare '*' should fail")
	}
	if err := s.PutRule("alice", "ok", RuleSpec{Allow: []string{"github.com"}}, []string{"BadTag"}); err == nil {
		t.Error("uppercase tag should fail")
	}
	if err := s.PutRule("alice", "ok", RuleSpec{Allow: []string{"*.example.com", "example.org"}}, []string{"ci"}); err != nil {
		t.Errorf("valid rule rejected: %v", err)
	}
}

// fakeRepoDomains stands in for *repos.Store. It answers for one (sandbox,
// owner) pair only, so a test that quietly asked about the wrong sandbox — the
// exact mistake a dropped owner term makes — gets an empty overlay rather than
// a passing assertion.
type fakeRepoDomains struct {
	sandbox, owner string
	domains        []string
	err            error
	calls          int
}

func (f *fakeRepoDomains) DomainsForSandbox(sandbox, owner string) ([]string, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	if sandbox != f.sandbox || owner != f.owner {
		return []string{}, nil
	}
	return append([]string{}, f.domains...), nil
}

func TestAllowForSandboxOverlaysRepoDomains(t *testing.T) {
	s := openTest(t)
	// The user wrote pypi.org, and github.com by hand — the overlay must merge
	// with the first and de-duplicate against the second.
	if err := s.PutRule("alice", "ci", RuleSpec{Allow: []string{"pypi.org", "github.com"}}, []string{"ci"}); err != nil {
		t.Fatal(err)
	}
	s.tag(t, "alice-box", "alice", "ci")
	src := &fakeRepoDomains{sandbox: "alice-box", owner: "alice",
		domains: []string{"codeload.github.com", "github.com", "objects.githubusercontent.com"}}
	s.SetRepoDomains(src)

	allow, governed, err := s.AllowForSandbox("alice-box", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if !governed {
		t.Error("a sandbox governed by a rule stays governed with an overlay")
	}
	want := []string{"codeload.github.com", "github.com", "objects.githubusercontent.com", "pypi.org"}
	if !reflect.DeepEqual(allow, want) {
		t.Errorf("allow = %v, want %v (merged, de-duped, sorted)", allow, want)
	}

	// The other half: the overlay must not have reached the stored rule-set.
	// The console renders ListRules into a textarea and PUTs it straight back,
	// so a leak here would persist the GitHub domains into the user's own rule
	// and detaching the repo would leave the holes behind.
	rules, err := s.ListRules("alice")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(rules[0].Spec.Allow, []string{"github.com", "pypi.org"}) {
		t.Errorf("ListRules allow = %v, want only what the user wrote", rules[0].Spec.Allow)
	}
	forBox, err := s.RulesForSandbox("alice-box", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(forBox[0].Spec.Allow, []string{"github.com", "pypi.org"}) {
		t.Errorf("RulesForSandbox allow = %v, want only what the user wrote", forBox[0].Spec.Allow)
	}
}

// TestRepoOverlayNeverGovernsAnUngovernedSandbox pins the decision the overlay
// exists under. governed is what puts a sandbox in the policy snapshot at all,
// and in sluice's --open-untagged mode an absent sandbox is UNFILTERED. If a
// repo attachment could set governed, `repo add` would firewall a VM that had
// unrestricted internet down to three GitHub domains — which looks like a
// security improvement in a diff and reads as "npm install hangs" on the box.
func TestRepoOverlayNeverGovernsAnUngovernedSandbox(t *testing.T) {
	s := openTest(t)
	s.tag(t, "loose-box", "alice", "ci") // tagged, but no rule-set uses that tag
	src := &fakeRepoDomains{sandbox: "loose-box", owner: "alice", domains: []string{"github.com"}}
	s.SetRepoDomains(src)

	allow, governed, err := s.AllowForSandbox("loose-box", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if governed {
		t.Fatal("a repo attachment must not make an ungoverned sandbox governed")
	}
	if len(allow) != 0 {
		t.Errorf("allow = %v, want none: an ungoverned sandbox already reaches github", allow)
	}
	if src.calls != 0 {
		t.Errorf("repo source consulted %d time(s) for an ungoverned sandbox", src.calls)
	}
}

// TestRepoOverlayIsOwnerScopedAndOptional covers the two shapes the wiring can
// take: no source at all (every deployment before repos existed), and a source
// that has nothing to say about this sandbox.
func TestRepoOverlayIsOwnerScopedAndOptional(t *testing.T) {
	s := openTest(t)
	s.PutRule("alice", "ci", RuleSpec{Allow: []string{"pypi.org"}}, []string{"ci"})
	s.tag(t, "alice-box", "alice", "ci")

	allow, governed, err := s.AllowForSandbox("alice-box", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if !governed || !reflect.DeepEqual(allow, []string{"pypi.org"}) {
		t.Fatalf("with no repo source: allow = %v, governed = %v", allow, governed)
	}

	// A source that answers for bob's sandbox must not widen alice's.
	s.SetRepoDomains(&fakeRepoDomains{sandbox: "bob-box", owner: "bob", domains: []string{"github.com"}})
	allow, _, err = s.AllowForSandbox("alice-box", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(allow, []string{"pypi.org"}) {
		t.Errorf("allow = %v, want [pypi.org]", allow)
	}
}

// TestRepoOverlayFailureKeepsTheRuleSetEnforced is the fail-safe direction.
// Both pushers drop a sandbox whose allow-set errors, and a dropped sandbox is
// omitted from the snapshot, which sluice reads as unrestricted. A broken repo
// lookup must therefore cost the clone, never the firewall.
func TestRepoOverlayFailureKeepsTheRuleSetEnforced(t *testing.T) {
	s := openTest(t)
	s.PutRule("alice", "ci", RuleSpec{Allow: []string{"pypi.org"}}, []string{"ci"})
	s.tag(t, "alice-box", "alice", "ci")
	s.SetRepoDomains(&fakeRepoDomains{err: errors.New("database is locked")})

	allow, governed, err := s.AllowForSandbox("alice-box", "alice")
	if err != nil {
		t.Fatalf("AllowForSandbox = %v, want the overlay failure swallowed", err)
	}
	if !governed || !reflect.DeepEqual(allow, []string{"pypi.org"}) {
		t.Errorf("allow = %v, governed = %v, want the user's rule still enforced", allow, governed)
	}
}

// TestRepoOverlayRejectsPatternsSluiceWouldNot guards the one thing the overlay
// skips that normSpec would have caught on the write path: it bypasses normSpec
// entirely, so nothing else lower-cases or validates what the source hands over.
func TestRepoOverlayRejectsPatternsSluiceWouldNot(t *testing.T) {
	s := openTest(t)
	s.PutRule("alice", "ci", RuleSpec{Allow: []string{"pypi.org"}}, []string{"ci"})
	s.tag(t, "alice-box", "alice", "ci")
	s.SetRepoDomains(&fakeRepoDomains{sandbox: "alice-box", owner: "alice",
		domains: []string{"https://github.com", "GitHub.com", "*", "github.com"}})

	allow, _, err := s.AllowForSandbox("alice-box", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(allow, []string{"github.com", "pypi.org"}) {
		t.Errorf("allow = %v, want only the pattern sluice can parse", allow)
	}
}
