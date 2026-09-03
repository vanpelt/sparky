package secrets

import (
	"errors"
	"slices"
	"testing"
)

// The AAD binds a ciphertext to (owner, env name, id) — that is what stops a
// row being re-homed under another name by editing the database. So a rename
// is not an UPDATE of one column: the value has to be unsealed under the old
// name and re-sealed under the new one, and this asserts the value survives
// that round trip and is delivered under the new name.
func TestRenameSecretKeepsTheValueUnderTheNewName(t *testing.T) {
	s := openTemp(t)
	must(t, s.PutSecret("alice", "GH_TOKEN", "ghp_original", []string{"web"}))
	must(t, s.PutSecret("alice", "GH_TOKEN", "ghp_second", []string{"web"})) // version 2
	must(t, s.SetTags("alicevm", "alice", []string{"web"}))

	must(t, s.RenameSecret("alice", "GH_TOKEN", "GITHUB_TOKEN"))

	env, err := s.EnvForSandbox("alicevm", "alice")
	must(t, err)
	if env["GITHUB_TOKEN"] != "ghp_second" {
		t.Fatalf("value under the new name = %q, want the stored one — the re-seal lost it",
			env["GITHUB_TOKEN"])
	}
	if _, ok := env["GH_TOKEN"]; ok {
		t.Fatal("the old name is still delivered; a rename is a move, not a copy")
	}

	after := metaFor(t, s, "alice", "GITHUB_TOKEN")
	if !slices.Equal(after.Tags, []string{"web"}) {
		t.Fatalf("tags = %v, want the original [web] carried across", after.Tags)
	}
	// Same reason RetagSecret does not bump: version counts changes to the
	// MATERIAL, and this is the same value under a different name.
	if after.Version != 2 {
		t.Fatalf("version = %d, want 2 unchanged — a rename is not a new secret", after.Version)
	}
}

// Renaming onto a name the owner already holds would have to either destroy
// that value or merge two rows. Neither is a rename's decision to make.
func TestRenameSecretRefusesANameInUse(t *testing.T) {
	s := openTemp(t)
	must(t, s.PutSecret("alice", "GH_TOKEN", "ghp_a", []string{"web"}))
	must(t, s.PutSecret("alice", "NPM_TOKEN", "npm_b", []string{"web"}))

	if err := s.RenameSecret("alice", "GH_TOKEN", "NPM_TOKEN"); !errors.Is(err, ErrSecretNameInUse) {
		t.Fatalf("rename onto a held name = %v, want ErrSecretNameInUse", err)
	}
	// And nothing moved: both rows are still readable under their own names.
	must(t, s.SetTags("alicevm", "alice", []string{"web"}))
	env, err := s.EnvForSandbox("alicevm", "alice")
	must(t, err)
	if env["GH_TOKEN"] != "ghp_a" || env["NPM_TOKEN"] != "npm_b" {
		t.Fatalf("a refused rename moved something: %v", env)
	}
}

// Owner-scoped in the SELECT, so another owner's secret of the same name is
// simply not found — the same answer as a name nobody holds.
func TestRenameSecretIsOwnerScoped(t *testing.T) {
	s := openTemp(t)
	must(t, s.PutSecret("alice", "GH_TOKEN", "ghp_x", []string{"web"}))

	if err := s.RenameSecret("bob", "GH_TOKEN", "GITHUB_TOKEN"); !errors.Is(err, ErrNoSuchSecret) {
		t.Fatalf("cross-owner rename = %v, want ErrNoSuchSecret", err)
	}
}

// The new name has to satisfy the same rules PutSecret would apply to it —
// otherwise a rename is a way to smuggle in a name that could never be
// written, and one of those (PATH) breaks the delivery channel itself.
func TestRenameSecretValidatesTheNewName(t *testing.T) {
	s := openTemp(t)
	must(t, s.PutSecret("alice", "GH_TOKEN", "ghp_x", []string{"web"}))

	for _, bad := range []string{"lowercase", "HAS-DASH", "PATH", ""} {
		if err := s.RenameSecret("alice", "GH_TOKEN", bad); err == nil {
			t.Fatalf("rename to %q was accepted", bad)
		}
	}
}

// Renaming to the name it already has is a no-op rather than an error: the
// console sends the name field on every save, so the common case is "unchanged".
func TestRenameSecretToItsOwnNameIsANoOp(t *testing.T) {
	s := openTemp(t)
	must(t, s.PutSecret("alice", "GH_TOKEN", "ghp_x", []string{"web"}))
	must(t, s.RenameSecret("alice", "GH_TOKEN", "GH_TOKEN"))

	must(t, s.SetTags("alicevm", "alice", []string{"web"}))
	env, err := s.EnvForSandbox("alicevm", "alice")
	must(t, err)
	if env["GH_TOKEN"] != "ghp_x" {
		t.Fatalf("value after a self-rename = %q", env["GH_TOKEN"])
	}
}
