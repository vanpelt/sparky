package secrets

import (
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// TestVarReachesSandboxThroughTag is the delivery path: a plain var joins to a
// sandbox through sandbox_tags exactly as a secret does, and the join is
// owner-scoped on BOTH sides — the sandbox's owner and the var's owner.
func TestVarReachesSandboxThroughTag(t *testing.T) {
	s := openTemp(t)
	must(t, s.PutVar("alice", "web", "NODE_ENV", "production"))
	must(t, s.PutVar("bob", "web", "BOB_ONLY", "b")) // same tag string, other owner
	must(t, s.SetTags("alicevm", "alice", []string{"web"}))

	vars, err := s.VarsForSandbox("alicevm", "alice")
	must(t, err)
	if len(vars) != 1 || vars["NODE_ENV"] != "production" {
		t.Fatalf("unexpected vars: %v", vars)
	}
	if _, leaked := vars["BOB_ONLY"]; leaked {
		t.Fatal("bob's var leaked into alice's sandbox")
	}

	// The same var arrives through the single delivery query envsync uses.
	env, err := s.EnvForSandbox("alicevm", "alice")
	must(t, err)
	if env["NODE_ENV"] != "production" {
		t.Fatalf("var missing from EnvForSandbox: %v", env)
	}

	// Wrong owner for the sandbox yields nothing, not bob's rows.
	for _, c := range []struct{ sandbox, owner string }{
		{"alicevm", "bob"},   // bob asking about alice's sandbox
		{"bobvm", "alice"},   // alice asking about a sandbox she has not tagged
		{"alicevm", "carol"}, // a stranger entirely
	} {
		vars, err := s.VarsForSandbox(c.sandbox, c.owner)
		must(t, err)
		if len(vars) != 0 {
			t.Fatalf("VarsForSandbox(%q, %q) returned %v", c.sandbox, c.owner, vars)
		}
		env, err := s.EnvForSandbox(c.sandbox, c.owner)
		must(t, err)
		if len(env) != 0 {
			t.Fatalf("EnvForSandbox(%q, %q) returned %v", c.sandbox, c.owner, env)
		}
	}

	// Untagging the sandbox withdraws the var, same as it does a secret.
	must(t, s.SetTags("alicevm", "alice", []string{"other"}))
	vars, err = s.VarsForSandbox("alicevm", "alice")
	must(t, err)
	if len(vars) != 0 {
		t.Fatalf("var survived retagging: %v", vars)
	}
}

// TestVarPerTagValues is the reason env_vars is keyed by tag and secrets are
// not: one owner, one name, two environments, two values.
func TestVarPerTagValues(t *testing.T) {
	s := openTemp(t)
	must(t, s.PutVar("alice", "staging", "NODE_ENV", "staging"))
	must(t, s.PutVar("alice", "dev", "NODE_ENV", "development"))
	must(t, s.SetTags("stagingvm", "alice", []string{"staging"}))
	must(t, s.SetTags("devvm", "alice", []string{"dev"}))

	for _, c := range []struct{ box, want string }{
		{"stagingvm", "staging"},
		{"devvm", "development"},
	} {
		env, err := s.EnvForSandbox(c.box, "alice")
		must(t, err)
		if env["NODE_ENV"] != c.want {
			t.Fatalf("%s: NODE_ENV=%q want %q", c.box, env["NODE_ENV"], c.want)
		}
	}

	// Both rows coexist, and ListVars orders tag then name.
	all, err := s.ListVars("alice")
	must(t, err)
	if len(all) != 2 || all[0].Tag != "dev" || all[1].Tag != "staging" {
		t.Fatalf("unexpected ListVars: %+v", all)
	}
	if all[0].Value != "development" || all[1].Value != "staging" {
		t.Fatalf("values crossed tags: %+v", all)
	}

	// VarsForTag sees one tag only.
	only, err := s.VarsForTag("alice", "dev")
	must(t, err)
	if len(only) != 1 || only[0].Name != "NODE_ENV" || only[0].Value != "development" {
		t.Fatalf("unexpected VarsForTag: %+v", only)
	}

	// An update rewrites in place and leaves the sibling tag alone.
	must(t, s.PutVar("alice", "dev", "NODE_ENV", "dev2"))
	only, err = s.VarsForTag("alice", "dev")
	must(t, err)
	if len(only) != 1 || only[0].Value != "dev2" {
		t.Fatalf("update did not replace: %+v", only)
	}
	if only[0].UpdatedAt.Before(only[0].CreatedAt) {
		t.Fatalf("timestamps went backwards: %+v", only[0])
	}
	staging, err := s.VarsForTag("alice", "staging")
	must(t, err)
	if len(staging) != 1 || staging[0].Value != "staging" {
		t.Fatalf("sibling tag disturbed: %+v", staging)
	}
}

// TestSecretWinsOverVar pins the merge order in EnvForSandbox. A plaintext row
// must never shadow an encrypted one.
func TestSecretWinsOverVar(t *testing.T) {
	s := openTemp(t)
	must(t, s.PutVar("alice", "web", "API_KEY", "placeholder"))
	must(t, s.PutSecret("alice", "API_KEY", "hunter2", []string{"web"}))
	must(t, s.PutVar("alice", "web", "LOG_LEVEL", "debug"))
	must(t, s.SetTags("myvm", "alice", []string{"web"}))

	env, err := s.EnvForSandbox("myvm", "alice")
	must(t, err)
	if env["API_KEY"] != "hunter2" {
		t.Fatalf("var shadowed the secret: API_KEY=%q", env["API_KEY"])
	}
	if env["LOG_LEVEL"] != "debug" {
		t.Fatalf("non-colliding var lost: %v", env)
	}
	if len(env) != 2 {
		t.Fatalf("unexpected env: %v", env)
	}

	// The var row itself is untouched — deleting the secret uncovers it again,
	// which is the only way the collision is observable.
	must(t, s.DeleteSecret("alice", "API_KEY"))
	env, err = s.EnvForSandbox("myvm", "alice")
	must(t, err)
	if env["API_KEY"] != "placeholder" {
		t.Fatalf("var row was clobbered by the secret: %v", env)
	}
}

// TestEnvForSandboxStillAllOrNothing: one undecryptable secret fails the whole
// call, and the vars that had already been collected do not leak out as a
// partial environment.
func TestEnvForSandboxStillAllOrNothing(t *testing.T) {
	s := openTemp(t)
	must(t, s.PutVar("alice", "t", "LOG_LEVEL", "debug"))
	must(t, s.PutSecret("alice", "BAD", "ok", []string{"t"}))
	must(t, s.SetTags("myvm", "alice", []string{"t"}))

	_, err := s.db.Exec(`UPDATE secrets SET ciphertext = x'00' WHERE env_name = 'BAD'`)
	must(t, err)

	env, err := s.EnvForSandbox("myvm", "alice")
	if !errors.Is(err, ErrUndecryptable) {
		t.Fatalf("expected ErrUndecryptable, got %v", err)
	}
	if env != nil {
		t.Fatalf("partial environment returned: %v", env)
	}
	// Vars remain reachable on their own path.
	vars, err := s.VarsForSandbox("myvm", "alice")
	must(t, err)
	if vars["LOG_LEVEL"] != "debug" {
		t.Fatalf("undecryptable secret disabled vars: %v", vars)
	}
}

// TestVarsSurviveKeyRotation: vars are plaintext by declaration, so nothing on
// their path touches the AEAD or the keycheck sentinel. A rotated OIDC key
// orphans every secret and must leave vars fully working.
func TestVarsSurviveKeyRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sparkbox.db")
	s := openAt(t, path, testKEK)
	must(t, s.PutVar("alice", "t", "LOG_LEVEL", "debug"))
	must(t, s.PutSecret("alice", "KEY", "v", []string{"t"}))
	must(t, s.SetTags("myvm", "alice", []string{"t"}))
	must(t, s.Close())

	s2 := openAt(t, path, otherKEK)
	vars, err := s2.VarsForSandbox("myvm", "alice")
	must(t, err)
	if vars["LOG_LEVEL"] != "debug" {
		t.Fatalf("vars lost across key rotation: %v", vars)
	}
	must(t, s2.PutVar("alice", "t", "REGION", "us-east"))
	all, err := s2.ListVars("alice")
	must(t, err)
	if len(all) != 2 {
		t.Fatalf("var writes blocked by the orphaned secret: %+v", all)
	}
	// The orphaned secret still fails the merged delivery query, all-or-nothing.
	if _, err := s2.EnvForSandbox("myvm", "alice"); !errors.Is(err, ErrUndecryptable) {
		t.Fatalf("expected ErrUndecryptable, got %v", err)
	}
}

// TestVarValidation reuses the secret rules wholesale — same helper, so a var
// can never carry a payload a secret could not.
func TestVarValidation(t *testing.T) {
	s := openTemp(t)
	bad := []struct {
		what                    string
		owner, tag, name, value string
	}{
		{"no owner", "", "t", "KEY", "v"},
		{"lowercase name", "alice", "t", "lower", "v"},
		{"leading digit", "alice", "t", "1KEY", "v"},
		{"dash in name", "alice", "t", "WITH-DASH", "v"},
		{"name too long", "alice", "t", "K" + strings.Repeat("E", 64), "v"},
		{"newline", "alice", "t", "KEY", "line1\nline2"},
		{"carriage return", "alice", "t", "KEY", "line1\rline2"},
		{"NUL", "alice", "t", "KEY", "nul\x00"},
		{"oversize value", "alice", "t", "KEY", strings.Repeat("x", maxValueLen+1)},
		{"hash: pam_env comments even inside quotes", "alice", "t", "KEY", "abc#def"},
		{"reserved PATH", "alice", "t", "PATH", "/opt/bin"},
		{"reserved LD_PRELOAD", "alice", "t", "LD_PRELOAD", "/tmp/e.so"},
		{"reserved LD_LIBRARY_PATH", "alice", "t", "LD_LIBRARY_PATH", "/tmp"},
		{"invalid tag", "alice", "Bad_Tag", "KEY", "v"},
		{"tag with leading dash", "alice", "-bad", "KEY", "v"},
	}
	for _, c := range bad {
		if err := s.PutVar(c.owner, c.tag, c.name, c.value); err == nil {
			t.Fatalf("%s: expected validation error", c.what)
		}
	}
	// Nothing above was written.
	all, err := s.ListVars("alice")
	must(t, err)
	if len(all) != 0 {
		t.Fatalf("a refused var was stored: %+v", all)
	}
	// Boundary values pass, exactly as for a secret.
	must(t, s.PutVar("alice", "t", "K"+strings.Repeat("E", 63), strings.Repeat("x", maxValueLen)))
	// An unnamed tag defaults, so a var can never be attached to nothing.
	must(t, s.PutVar("alice", "", "REGION", "us-east"))
	def, err := s.VarsForTag("alice", DefaultTag)
	must(t, err)
	if len(def) != 1 || def[0].Name != "REGION" {
		t.Fatalf("empty tag did not default: %+v", def)
	}
}

// TestDeleteVar covers the single-row delete and its ErrNoSuchVar.
func TestDeleteVar(t *testing.T) {
	s := openTemp(t)
	must(t, s.PutVar("alice", "dev", "NODE_ENV", "development"))
	must(t, s.PutVar("alice", "prod", "NODE_ENV", "production"))

	must(t, s.DeleteVar("alice", "dev", "NODE_ENV"))
	if err := s.DeleteVar("alice", "dev", "NODE_ENV"); !errors.Is(err, ErrNoSuchVar) {
		t.Fatalf("expected ErrNoSuchVar, got %v", err)
	}
	// The same name under another tag is a different row.
	rest, err := s.ListVars("alice")
	must(t, err)
	if len(rest) != 1 || rest[0].Tag != "prod" {
		t.Fatalf("delete crossed tags: %+v", rest)
	}
	// And another owner's identical row is not reachable.
	must(t, s.PutVar("bob", "prod", "NODE_ENV", "bobs"))
	if err := s.DeleteVar("alice", "prod", "NOPE"); !errors.Is(err, ErrNoSuchVar) {
		t.Fatalf("expected ErrNoSuchVar, got %v", err)
	}
	must(t, s.DeleteVar("alice", "prod", "NODE_ENV"))
	bobs, err := s.ListVars("bob")
	must(t, err)
	if len(bobs) != 1 || bobs[0].Value != "bobs" {
		t.Fatalf("alice's delete reached bob: %+v", bobs)
	}
}

// TestDeleteVarsForTagScoped: the environment-deletion cleanup removes exactly
// one owner's rows for one tag.
func TestDeleteVarsForTagScoped(t *testing.T) {
	s := openTemp(t)
	must(t, s.PutVar("alice", "web", "NODE_ENV", "production"))
	must(t, s.PutVar("alice", "web", "LOG_LEVEL", "info"))
	must(t, s.PutVar("alice", "gpu", "NODE_ENV", "production"))
	must(t, s.PutVar("bob", "web", "NODE_ENV", "bobs"))

	must(t, s.DeleteVarsForTag("alice", "web"))

	left, err := s.ListVars("alice")
	must(t, err)
	if len(left) != 1 || left[0].Tag != "gpu" {
		t.Fatalf("wrong rows removed: %+v", left)
	}
	bobs, err := s.ListVars("bob")
	must(t, err)
	if len(bobs) != 1 || bobs[0].Value != "bobs" {
		t.Fatalf("another owner's tag was cleared: %+v", bobs)
	}
	// Deleting a tag that holds nothing is not an error (it is a cleanup).
	must(t, s.DeleteVarsForTag("alice", "web"))
}

// TestOpensPreVarsDatabase is the compatibility surface: a sparkbox.db written
// before env_vars existed must open, keep delivering its secrets, and gain the
// new table.
func TestOpensPreVarsDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sparkbox.db")
	s := openAt(t, path, testKEK)
	must(t, s.PutSecret("alice", "API_KEY", "hunter2", []string{"web"}))
	must(t, s.SetTags("myvm", "alice", []string{"web"}))
	must(t, s.Close())

	// Roll the file back to the pre-change schema.
	raw, err := sql.Open("sqlite", "file:"+path)
	must(t, err)
	if _, err := raw.Exec(`DROP INDEX env_vars_owner; DROP TABLE env_vars;`); err != nil {
		t.Fatal(err)
	}
	must(t, raw.Close())

	s2 := openAt(t, path, testKEK)
	env, err := s2.EnvForSandbox("myvm", "alice")
	must(t, err)
	if len(env) != 1 || env["API_KEY"] != "hunter2" {
		t.Fatalf("pre-vars database lost its secrets: %v", env)
	}
	// The table came back and works.
	must(t, s2.PutVar("alice", "web", "LOG_LEVEL", "info"))
	env, err = s2.EnvForSandbox("myvm", "alice")
	must(t, err)
	if env["LOG_LEVEL"] != "info" || env["API_KEY"] != "hunter2" {
		t.Fatalf("unexpected env after upgrade: %v", env)
	}
}
