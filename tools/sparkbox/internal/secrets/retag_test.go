package secrets

import (
	"errors"
	"slices"
	"testing"
)

// RetagSecret is the one secret write that needs no value, which is exactly why
// it exists: values are write-only from every API's point of view, so without
// it "also give this token the tag web" could only be expressed by asking the
// user to paste the token again.
func TestRetagSecretKeepsTheValueAndTheVersion(t *testing.T) {
	s := openTemp(t)
	must(t, s.PutSecret("alice", "GH_TOKEN", "ghp_original", []string{"web"}))
	must(t, s.PutSecret("alice", "GH_TOKEN", "ghp_second", []string{"web"})) // version 2
	must(t, s.SetTags("alicevm", "alice", []string{"ci"}))

	before := metaFor(t, s, "alice", "GH_TOKEN")
	if before.Version != 2 {
		t.Fatalf("fixture version = %d, want 2", before.Version)
	}

	must(t, s.RetagSecret("alice", "GH_TOKEN", []string{"web", "ci"}))

	after := metaFor(t, s, "alice", "GH_TOKEN")
	if !slices.Equal(after.Tags, []string{"ci", "web"}) {
		t.Fatalf("tags = %v, want the normalized [ci web]", after.Tags)
	}
	// The version counts changes to the MATERIAL, and the material did not
	// change: a guest that re-read this row would find the same string.
	if after.Version != before.Version {
		t.Fatalf("version = %d, want %d unchanged — a retag is not a new secret",
			after.Version, before.Version)
	}
	// And the value survived, which is the whole point.
	env, err := s.EnvForSandbox("alicevm", "alice")
	must(t, err)
	if env["GH_TOKEN"] != "ghp_second" {
		t.Fatalf("value after retag = %q, want the stored one", env["GH_TOKEN"])
	}
}

// The retag is owner-scoped in the SELECT, so another owner's secret of the
// same name is simply not found — the same answer as a name nobody holds.
func TestRetagSecretIsOwnerScoped(t *testing.T) {
	s := openTemp(t)
	must(t, s.PutSecret("alice", "GH_TOKEN", "ghp_x", []string{"web"}))

	if err := s.RetagSecret("bob", "GH_TOKEN", []string{"web"}); !errors.Is(err, ErrNoSuchSecret) {
		t.Fatalf("err = %v, want ErrNoSuchSecret", err)
	}
	if err := s.RetagSecret("alice", "NOPE", []string{"web"}); !errors.Is(err, ErrNoSuchSecret) {
		t.Fatalf("err = %v, want ErrNoSuchSecret", err)
	}
	// alice's row is untouched by either attempt.
	if tags := metaFor(t, s, "alice", "GH_TOKEN").Tags; !slices.Equal(tags, []string{"web"}) {
		t.Fatalf("tags = %v", tags)
	}
}

// An empty tag set defaults to DefaultTag for PutSecret's reason: a secret
// tagged with nothing reaches no sandbox at all, and that failure is silent
// until somebody's agent asks them to log in.
func TestRetagSecretDefaultsAnEmptyTagSet(t *testing.T) {
	s := openTemp(t)
	must(t, s.PutSecret("alice", "GH_TOKEN", "ghp_x", []string{"web"}))
	must(t, s.RetagSecret("alice", "GH_TOKEN", nil))
	if tags := metaFor(t, s, "alice", "GH_TOKEN").Tags; !slices.Equal(tags, []string{DefaultTag}) {
		t.Fatalf("tags = %v, want [%s]", tags, DefaultTag)
	}
}

func metaFor(t *testing.T, s *Store, owner, name string) SecretMeta {
	t.Helper()
	list, err := s.ListSecrets(owner)
	must(t, err)
	for _, m := range list {
		if m.Name == name {
			return m
		}
	}
	t.Fatalf("no secret %q for %q in %+v", name, owner, list)
	return SecretMeta{}
}
