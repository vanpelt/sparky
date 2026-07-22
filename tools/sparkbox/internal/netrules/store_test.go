package netrules

import (
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
