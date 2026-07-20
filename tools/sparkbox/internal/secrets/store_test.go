package secrets

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

var (
	testKEK  = DeriveKEK([]byte("test-oidc-scalar"))
	otherKEK = DeriveKEK([]byte("rotated-oidc-scalar"))
)

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func openAt(t *testing.T, path string, kek []byte) *Store {
	t.Helper()
	s, err := Open(path, kek, discard())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func openTemp(t *testing.T) *Store {
	t.Helper()
	return openAt(t, filepath.Join(t.TempDir(), "sparkbox.db"), testKEK)
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestRoundTrip(t *testing.T) {
	s := openTemp(t)
	must(t, s.PutSecret("alice", "API_KEY", "hunter2", []string{"web"}))
	must(t, s.SetTags("myvm", "alice", []string{"web", "gpu"}))

	env, err := s.EnvForSandbox("myvm", "alice")
	must(t, err)
	if len(env) != 1 || env["API_KEY"] != "hunter2" {
		t.Fatalf("unexpected env: %v", env)
	}

	// Update re-encrypts and bumps the version.
	must(t, s.PutSecret("alice", "API_KEY", "hunter3", []string{"web"}))
	env, err = s.EnvForSandbox("myvm", "alice")
	must(t, err)
	if env["API_KEY"] != "hunter3" {
		t.Fatalf("update not visible: %v", env)
	}
	metas, err := s.ListSecrets("alice")
	must(t, err)
	if len(metas) != 1 || metas[0].Name != "API_KEY" || metas[0].Version != 2 {
		t.Fatalf("unexpected metadata: %+v", metas)
	}
	if len(metas[0].Tags) != 1 || metas[0].Tags[0] != "web" {
		t.Fatalf("unexpected tags: %+v", metas[0].Tags)
	}
}

func TestTagsOps(t *testing.T) {
	s := openTemp(t)
	must(t, s.SetTags("myvm", "alice", []string{"web", "gpu", "web"})) // dupes collapse
	tags, err := s.TagsFor("myvm")
	must(t, err)
	if len(tags) != 2 || tags[0] != "gpu" || tags[1] != "web" {
		t.Fatalf("unexpected tags: %v", tags)
	}

	// Replace-set removes what's absent from the new set.
	must(t, s.SetTags("myvm", "alice", []string{"web"}))
	tags, _ = s.TagsFor("myvm")
	if len(tags) != 1 || tags[0] != "web" {
		t.Fatalf("replace failed: %v", tags)
	}

	must(t, s.RenameSandbox("myvm", "newvm"))
	if tags, _ = s.TagsFor("myvm"); len(tags) != 0 {
		t.Fatalf("old name still tagged: %v", tags)
	}
	if tags, _ = s.TagsFor("newvm"); len(tags) != 1 {
		t.Fatalf("tags did not move: %v", tags)
	}

	must(t, s.DeleteBySandbox("newvm"))
	if tags, _ = s.TagsFor("newvm"); len(tags) != 0 {
		t.Fatalf("tags survived delete: %v", tags)
	}
}

func TestRenameSandboxMovesEnv(t *testing.T) {
	s := openTemp(t)
	must(t, s.PutSecret("alice", "TOKEN", "v", []string{"web"}))
	must(t, s.SetTags("myvm", "alice", []string{"web"}))
	must(t, s.RenameSandbox("myvm", "newvm"))

	env, err := s.EnvForSandbox("newvm", "alice")
	must(t, err)
	if env["TOKEN"] != "v" {
		t.Fatalf("env lost across rename: %v", env)
	}
	env, err = s.EnvForSandbox("myvm", "alice")
	must(t, err)
	if len(env) != 0 {
		t.Fatalf("old name still resolves secrets: %v", env)
	}
}

func TestCrossOwnerIsolation(t *testing.T) {
	s := openTemp(t)
	// Same tag string, different owners: the join must never cross.
	must(t, s.PutSecret("alice", "ALICE_KEY", "a", []string{"shared"}))
	must(t, s.PutSecret("bob", "BOB_KEY", "b", []string{"shared"}))
	must(t, s.SetTags("alicevm", "alice", []string{"shared"}))

	env, err := s.EnvForSandbox("alicevm", "alice")
	must(t, err)
	if _, leaked := env["BOB_KEY"]; leaked {
		t.Fatal("bob's secret leaked into alice's sandbox")
	}
	if env["ALICE_KEY"] != "a" {
		t.Fatalf("alice's own secret missing: %v", env)
	}

	// Wrong owner argument for the sandbox yields nothing, not bob's values.
	env, err = s.EnvForSandbox("alicevm", "bob")
	must(t, err)
	if len(env) != 0 {
		t.Fatalf("cross-owner query returned values: %v", env)
	}
}

func TestAADTamperSwappedCiphertexts(t *testing.T) {
	s := openTemp(t)
	must(t, s.PutSecret("alice", "KEY_A", "value-a", []string{"t"}))
	must(t, s.PutSecret("bob", "KEY_B", "value-b", []string{"t"}))
	must(t, s.SetTags("alicevm", "alice", []string{"t"}))

	// Splice bob's ciphertext into alice's row at the database level. The AAD
	// binds owner|env_name|id, so the row must refuse to decrypt.
	_, err := s.db.Exec(`
		UPDATE secrets SET ciphertext = (SELECT ciphertext FROM secrets WHERE owner = 'bob')
		WHERE owner = 'alice'`)
	must(t, err)

	if _, err := s.EnvForSandbox("alicevm", "alice"); !errors.Is(err, ErrUndecryptable) {
		t.Fatalf("expected ErrUndecryptable, got %v", err)
	}
}

func TestAllOrNothing(t *testing.T) {
	s := openTemp(t)
	must(t, s.PutSecret("alice", "GOOD", "ok", []string{"t"}))
	must(t, s.PutSecret("alice", "BAD", "ok", []string{"t"}))
	must(t, s.SetTags("myvm", "alice", []string{"t"}))

	_, err := s.db.Exec(`UPDATE secrets SET ciphertext = x'00' WHERE env_name = 'BAD'`)
	must(t, err)

	// One bad row fails the whole computation — never a partial environment.
	if _, err := s.EnvForSandbox("myvm", "alice"); !errors.Is(err, ErrUndecryptable) {
		t.Fatalf("expected ErrUndecryptable, got %v", err)
	}
}

func TestKeycheckRecoversAfterRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sparkbox.db")
	s := openAt(t, path, testKEK)
	must(t, s.PutSecret("alice", "KEY", "v", []string{"t"}))
	must(t, s.PutSecret("alice", "STALE", "old", []string{"t"}))
	must(t, s.SetTags("myvm", "alice", []string{"t"}))
	must(t, s.Close())

	// Reopen under a rotated key: Open succeeds, orphaned rows fail delivery,
	// but listing/deleting/re-entering all work so the store heals in place.
	s2 := openAt(t, path, otherKEK)
	if _, err := s2.EnvForSandbox("myvm", "alice"); !errors.Is(err, ErrUndecryptable) {
		t.Fatalf("EnvForSandbox: expected ErrUndecryptable, got %v", err)
	}
	metas, err := s2.ListSecrets("alice")
	must(t, err)
	if len(metas) != 2 {
		t.Fatalf("orphaned rows must stay listable: %+v", metas)
	}
	must(t, s2.DeleteSecret("alice", "STALE"))
	must(t, s2.PutSecret("alice", "KEY", "v2", []string{"t"})) // re-seals under the current KEK
	env, err := s2.EnvForSandbox("myvm", "alice")
	must(t, err)
	if env["KEY"] != "v2" {
		t.Fatalf("re-entered value not delivered: %v", env)
	}
	must(t, s2.SetTags("othervm", "alice", []string{"other"}))
	must(t, s2.Close())

	// The sentinel was rewritten at detection, so a fresh open under the
	// current key is clean and delivery keeps working.
	s3 := openAt(t, path, otherKEK)
	env, err = s3.EnvForSandbox("myvm", "alice")
	must(t, err)
	if env["KEY"] != "v2" {
		t.Fatalf("healed store lost data: %v", env)
	}
}

func TestDisabledGatesDeliveryOnly(t *testing.T) {
	s := openTemp(t)
	must(t, s.PutSecret("alice", "KEY", "v", []string{"t"}))
	must(t, s.SetTags("myvm", "alice", []string{"t"}))

	// disabled is only set when the keycheck sentinel could not be rewritten;
	// it must gate delivery alone — writes and listing are the recovery path.
	s.disabled = true
	if _, err := s.EnvForSandbox("myvm", "alice"); !errors.Is(err, ErrNotEnabled) {
		t.Fatalf("EnvForSandbox: expected ErrNotEnabled, got %v", err)
	}
	if _, err := s.SandboxesForSecret("alice", "KEY"); !errors.Is(err, ErrNotEnabled) {
		t.Fatalf("SandboxesForSecret: expected ErrNotEnabled, got %v", err)
	}
	must(t, s.PutSecret("alice", "KEY", "v2", []string{"t"}))
	metas, err := s.ListSecrets("alice")
	must(t, err)
	if len(metas) != 1 || metas[0].Version != 2 {
		t.Fatalf("writes/listing must work while disabled: %+v", metas)
	}
	must(t, s.DeleteSecret("alice", "KEY"))
}

func TestDeleteSecret(t *testing.T) {
	s := openTemp(t)
	must(t, s.PutSecret("alice", "KEY", "v", []string{"a", "b"}))
	must(t, s.DeleteSecret("alice", "KEY"))

	if err := s.DeleteSecret("alice", "KEY"); !errors.Is(err, ErrNoSuchSecret) {
		t.Fatalf("expected ErrNoSuchSecret, got %v", err)
	}
	// Tag associations must not survive the secret.
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM secret_tags`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("expected 0 orphaned secret_tags rows, got %d", n)
	}
}

func TestSandboxesForSecret(t *testing.T) {
	s := openTemp(t)
	must(t, s.PutSecret("alice", "KEY", "v", []string{"web", "gpu"}))
	must(t, s.SetTags("vm-a", "alice", []string{"web"}))
	must(t, s.SetTags("vm-b", "alice", []string{"gpu", "web"})) // two matching tags, one row
	must(t, s.SetTags("vm-c", "alice", []string{"db"}))
	must(t, s.SetTags("bobvm", "bob", []string{"web"}))

	boxes, err := s.SandboxesForSecret("alice", "KEY")
	must(t, err)
	if len(boxes) != 2 || boxes[0] != "vm-a" || boxes[1] != "vm-b" {
		t.Fatalf("unexpected fan-out: %v", boxes)
	}
}

func TestValidation(t *testing.T) {
	s := openTemp(t)
	badPuts := []struct {
		owner, name, value string
		tags               []string
	}{
		{"", "KEY", "v", nil},                                     // no owner
		{"alice", "lower", "v", nil},                              // lowercase env name
		{"alice", "1KEY", "v", nil},                               // leading digit
		{"alice", "WITH-DASH", "v", nil},                          // dash
		{"alice", "K" + strings.Repeat("E", 64), "v", nil},        // too long
		{"alice", "KEY", "line1\nline2", nil},                     // newline
		{"alice", "KEY", "nul\x00", nil},                          // NUL
		{"alice", "KEY", strings.Repeat("x", maxValueLen+1), nil}, // oversize
		{"alice", "KEY", "v", []string{"Bad_Tag"}},                // invalid tag
		{"alice", "KEY", "abc#def", nil},                          // '#': pam_env comments even inside quotes
		{"alice", "PATH", "/opt/bin", nil},                        // reserved name
		{"alice", "LD_PRELOAD", "/tmp/e.so", nil},                 // reserved name
		{"alice", "LD_LIBRARY_PATH", "/tmp", nil},                 // reserved name
	}
	for i, c := range badPuts {
		if err := s.PutSecret(c.owner, c.name, c.value, c.tags); err == nil {
			t.Fatalf("case %d (%s): expected validation error", i, c.name)
		}
	}
	// Boundary values pass.
	must(t, s.PutSecret("alice", "K"+strings.Repeat("E", 63), strings.Repeat("x", maxValueLen), nil))

	if err := s.SetTags("myvm", "alice", []string{"-bad"}); err == nil {
		t.Fatal("expected tag validation error")
	}
	if err := s.SetTags("", "alice", []string{"ok"}); err == nil {
		t.Fatal("expected sandbox validation error")
	}
}

func TestPragmasOnEveryConnection(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()

	// Hold several pool connections open at once so each is a distinct
	// sqlite connection, then verify the DSN pragmas took on all of them —
	// a db.Exec pragma binds to only one.
	var conns []*sql.Conn
	for i := 0; i < 3; i++ {
		c, err := s.db.Conn(ctx)
		must(t, err)
		defer c.Close() //nolint:errcheck
		conns = append(conns, c)
	}
	for i, c := range conns {
		var busy, fk int
		must(t, c.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&busy))
		must(t, c.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&fk))
		if busy != 5000 || fk != 1 {
			t.Fatalf("conn %d: busy_timeout=%d foreign_keys=%d (want 5000/1)", i, busy, fk)
		}
	}
}

func TestConcurrentStoresNoBusyErrors(t *testing.T) {
	// Two Stores on one file mirror production: main.go opens separate pools
	// (users/secrets/routes) on the same sparkbox.db. Without busy_timeout on
	// every pooled connection, overlapping write transactions fail instantly
	// with SQLITE_BUSY instead of waiting.
	path := filepath.Join(t.TempDir(), "sparkbox.db")
	a := openAt(t, path, testKEK)
	b := openAt(t, path, testKEK)

	hammer := func(s *Store, owner string) error {
		for i := 0; i < 25; i++ {
			if err := s.PutSecret(owner, "KEY", fmt.Sprintf("v%d", i), []string{"t"}); err != nil {
				return err
			}
			if err := s.SetTags(owner+"vm", owner, []string{"t"}); err != nil {
				return err
			}
			if _, err := s.ListSecrets(owner); err != nil {
				return err
			}
		}
		return nil
	}
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, run := range []struct {
		s     *Store
		owner string
	}{{a, "alice"}, {b, "bob"}} {
		wg.Add(1)
		go func(s *Store, owner string) {
			defer wg.Done()
			errs <- hammer(s, owner)
		}(run.s, run.owner)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		must(t, err)
	}
}

func TestRenameSandboxOverStaleRows(t *testing.T) {
	s := openTemp(t)
	// Stale rows under the target name (a destroy whose best-effort tag
	// cleanup failed) must be dropped, not merged, and must not abort the
	// move on the (sandbox, tag) primary key.
	must(t, s.SetTags("web", "mallory", []string{"prod", "old"}))
	must(t, s.SetTags("api", "alice", []string{"prod"}))

	must(t, s.RenameSandbox("api", "web"))
	tags, err := s.TagsFor("web")
	must(t, err)
	if len(tags) != 1 || tags[0] != "prod" {
		t.Fatalf("stale rows leaked into renamed sandbox: %v", tags)
	}
	if tags, _ := s.TagsFor("api"); len(tags) != 0 {
		t.Fatalf("old name still tagged: %v", tags)
	}
	// The moved row carries the renaming owner, not the stale one.
	var owner string
	must(t, s.db.QueryRow(`SELECT owner FROM sandbox_tags WHERE sandbox = 'web'`).Scan(&owner))
	if owner != "alice" {
		t.Fatalf("renamed row has wrong owner: %q", owner)
	}
}

func TestSecretValueRedacted(t *testing.T) {
	v := secretValue("hunter2")
	if got := v.LogValue().String(); got != "[redacted]" {
		t.Fatalf("LogValue leaks: %q", got)
	}
	if got := v.String(); got != "[redacted]" {
		t.Fatalf("String leaks: %q", got)
	}
}
