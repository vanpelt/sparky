package repos

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func openAt(t *testing.T, path string) *Store {
	t.Helper()
	s, err := Open(path, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func openTest(t *testing.T) *Store {
	t.Helper()
	return openAt(t, filepath.Join(t.TempDir(), "sparkbox.db"))
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
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
	r := Repo{Slug: "wandb/hivemind", Ref: "main", Path: "src/hivemind"}
	must(t, s.PutRepo("alice", r, []string{"ci", "build", "ci"}))

	list, err := s.ListRepos("alice")
	must(t, err)
	if len(list) != 1 {
		t.Fatalf("want 1 repo, got %d", len(list))
	}
	got := list[0]
	if got.Owner != "alice" || got.Host != "github.com" || got.Slug != "wandb/hivemind" {
		t.Errorf("meta = %+v", got)
	}
	if got.Ref != "main" || got.Path != "src/hivemind" {
		t.Errorf("ref/path = %q/%q", got.Ref, got.Path)
	}
	// An unspecified access is read, never write.
	if got.Access != AccessRead {
		t.Errorf("access = %q, want %q", got.Access, AccessRead)
	}
	if got.ID == "" || got.CreatedAt.IsZero() {
		t.Errorf("id/created_at not populated: %+v", got)
	}
	// Deduped + sorted.
	if !reflect.DeepEqual(got.Tags, []string{"build", "ci"}) {
		t.Errorf("tags = %v", got.Tags)
	}

	// Update replaces the tag set wholesale and keeps the row's identity.
	must(t, s.PutRepo("alice", Repo{Slug: "wandb/hivemind", Access: AccessWrite}, []string{"ci"}))
	list, err = s.ListRepos("alice")
	must(t, err)
	after := list[0]
	if after.ID != got.ID || !after.CreatedAt.Equal(got.CreatedAt) {
		t.Errorf("update changed the row's identity: %+v -> %+v", got, after)
	}
	if after.Access != AccessWrite || after.Ref != "" || after.Path != "" {
		t.Errorf("update did not replace the body: %+v", after)
	}
	if !reflect.DeepEqual(after.Tags, []string{"ci"}) {
		t.Errorf("tags after update = %v", after.Tags)
	}

	must(t, s.DeleteRepo("alice", "github.com", "wandb/hivemind"))
	if err := s.DeleteRepo("alice", "github.com", "wandb/hivemind"); err != ErrNoSuchRepo {
		t.Errorf("second delete = %v, want ErrNoSuchRepo", err)
	}
	// The tag rows went with it, not just the repo row.
	var n int
	must(t, s.db.QueryRow(`SELECT COUNT(*) FROM repo_tags`).Scan(&n))
	if n != 0 {
		t.Errorf("%d orphaned repo_tags rows after delete", n)
	}
}

func TestReposForSandboxJoinsOnTagsAndScopesByOwner(t *testing.T) {
	s := openTest(t)
	// alice: a repo tagged "ci" applies to her sandbox tagged "ci".
	must(t, s.PutRepo("alice", Repo{Slug: "alice/app"}, []string{"ci"}))
	// alice: a repo tagged "gpu" that the sandbox does NOT carry.
	must(t, s.PutRepo("alice", Repo{Slug: "alice/models"}, []string{"gpu"}))
	// bob owns a private repo tagged "ci" too — must never reach alice's
	// sandbox. This is the whole reason the join carries bt.owner = r.owner
	// as well as r.owner = ?: without the first term, a tag name two people
	// happen to share hands alice a slug she has no access to and a
	// credential request for it.
	must(t, s.PutRepo("bob", Repo{Slug: "bob/secrets"}, []string{"ci"}))
	s.tag(t, "alice-box", "alice", "ci")

	got, err := s.ReposForSandbox("alice-box", "alice")
	must(t, err)
	if len(got) != 1 || got[0].Slug != "alice/app" {
		t.Fatalf("ReposForSandbox = %+v, want just alice/app", got)
	}
	if got[0].Owner != "alice" {
		t.Fatalf("cross-tenant leak: %+v", got[0])
	}

	// bob's own sandbox, carrying the same tag name, sees only bob's repo.
	s.tag(t, "bob-box", "bob", "ci")
	got, err = s.ReposForSandbox("bob-box", "bob")
	must(t, err)
	if len(got) != 1 || got[0].Slug != "bob/secrets" {
		t.Fatalf("bob's manifest = %+v", got)
	}

	// Asking for alice's sandbox under bob's handle yields nothing, rather
	// than bob's repos through alice's tag rows.
	got, err = s.ReposForSandbox("alice-box", "bob")
	must(t, err)
	if len(got) != 0 {
		t.Fatalf("owner/sandbox mismatch returned %+v, want nothing", got)
	}

	boxes, err := s.SandboxesForRepo("alice", "github.com", "alice/app")
	must(t, err)
	if !reflect.DeepEqual(boxes, []string{"alice-box"}) {
		t.Errorf("SandboxesForRepo = %v", boxes)
	}
	// bob's identically-tagged repo does not fan out to alice's box either.
	boxes, err = s.SandboxesForRepo("bob", "github.com", "bob/secrets")
	must(t, err)
	if !reflect.DeepEqual(boxes, []string{"bob-box"}) {
		t.Errorf("SandboxesForRepo(bob) = %v", boxes)
	}
}

func TestDomainsForSandbox(t *testing.T) {
	s := openTest(t)
	must(t, s.PutRepo("alice", Repo{Slug: "alice/app"}, []string{"ci"}))
	must(t, s.PutRepo("bob", Repo{Slug: "bob/secrets"}, []string{"ci"}))
	s.tag(t, "alice-box", "alice", "ci")
	s.tag(t, "plain-box", "alice", "gpu")

	want := []string{"codeload.github.com", "github.com", "objects.githubusercontent.com"}
	got, err := s.DomainsForSandbox("alice-box", "alice")
	must(t, err)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("domains = %v, want %v", got, want)
	}

	// A sandbox whose tags reach no repo gets an empty slice, never nil:
	// the overlay must be able to tell "no repos" from "no answer", and the
	// JSON must render [] rather than null.
	got, err = s.DomainsForSandbox("plain-box", "alice")
	must(t, err)
	if got == nil || len(got) != 0 {
		t.Errorf("unattached sandbox domains = %#v, want empty non-nil", got)
	}
	got, err = s.DomainsForSandbox("no-such-box", "alice")
	must(t, err)
	if len(got) != 0 {
		t.Errorf("unknown sandbox domains = %v", got)
	}

	// Bob's repo does not open holes in alice's box, and vice versa.
	got, err = s.DomainsForSandbox("alice-box", "bob")
	must(t, err)
	if len(got) != 0 {
		t.Errorf("owner mismatch opened %v", got)
	}

	// The returned slice is the caller's to keep: a pusher that sorts or
	// appends in place must not corrupt the package-level list.
	mine, err := s.DomainsForSandbox("alice-box", "alice")
	must(t, err)
	mine[0] = "evil.example"
	again, err := s.DomainsForSandbox("alice-box", "alice")
	must(t, err)
	if !reflect.DeepEqual(again, want) {
		t.Errorf("domains aliased the package list: %v", again)
	}
}

func TestSlugGrammar(t *testing.T) {
	// The name half is NOT the login half. Every entry here is a real
	// GitHub-shaped name that the login grammar would have rejected, or a
	// form GitHub itself refuses.
	for _, ok := range []string{
		"wandb/hivemind",
		"vanpelt/node.js",
		"vanpelt/my_repo",
		"vanpelt/2fa",
		"vanpelt/.github",
		"vanpelt/a",
		"a-b/c-d",
		"vanpelt/" + strings.Repeat("x", 100),
	} {
		if !ValidSlug(ok) {
			t.Errorf("ValidSlug(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{
		"",
		"hivemind",           // no owner half
		"/hivemind",          // empty owner
		"wandb/",             // empty name
		"wandb/hive/mind",    // two slashes
		"wandb/.",            // a directory entry, not a name
		"wandb/..",           // ditto, and the reason path traversal exists
		"wandb/...",          // all dots
		"wandb/hivemind.git", // the suffix a pasted clone URL leaves behind
		"wandb/hivemind.GIT",
		"wandb/hive mind",
		"wandb/hive~mind",
		"-wandb/hivemind", // logins may not lead with a hyphen
		"wan--db/hivemind",
		"wandb.io/hivemind", // a login may not carry a dot
		"https://github.com/wandb/hivemind",
		"wandb/" + strings.Repeat("x", 101),
		strings.Repeat("w", 40) + "/hivemind",
	} {
		if ValidSlug(bad) {
			t.Errorf("ValidSlug(%q) = true, want false", bad)
		}
	}

	owner, name, ok := SplitSlug("  wandb/hive.mind  ")
	if !ok || owner != "wandb" || name != "hive.mind" {
		t.Errorf("SplitSlug = %q, %q, %v", owner, name, ok)
	}
}

func TestValidationRejectsAndWrapsErrInvalidRepo(t *testing.T) {
	s := openTest(t)
	for _, tc := range []struct {
		why  string
		call func() error
	}{
		{"no owner", func() error { return s.PutRepo("", Repo{Slug: "a/b"}, nil) }},
		{"owner mismatch", func() error { return s.PutRepo("alice", Repo{Owner: "bob", Slug: "a/b"}, nil) }},
		{"bad slug", func() error { return s.PutRepo("alice", Repo{Slug: "a/b.git"}, nil) }},
		{"other host", func() error { return s.PutRepo("alice", Repo{Host: "ghe.corp", Slug: "a/b"}, nil) }},
		{"bad access", func() error { return s.PutRepo("alice", Repo{Slug: "a/b", Access: "admin"}, nil) }},
		{"option-shaped ref", func() error { return s.PutRepo("alice", Repo{Slug: "a/b", Ref: "--upload-pack=sh"}, nil) }},
		{"traversing ref", func() error { return s.PutRepo("alice", Repo{Slug: "a/b", Ref: "a/../b"}, nil) }},
		{"absolute path", func() error { return s.PutRepo("alice", Repo{Slug: "a/b", Path: "/etc/ssh"}, nil) }},
		{"traversing path", func() error { return s.PutRepo("alice", Repo{Slug: "a/b", Path: "src/../../etc"}, nil) }},
		{"home-relative path", func() error { return s.PutRepo("alice", Repo{Slug: "a/b", Path: "~/src"}, nil) }},
		{"uppercase tag", func() error { return s.PutRepo("alice", Repo{Slug: "a/b"}, []string{"BadTag"}) }},
	} {
		err := tc.call()
		if err == nil {
			t.Errorf("%s: accepted", tc.why)
			continue
		}
		// The whole validation family must be recognisable by errors.Is so
		// the console maps it to 400 without matching on message text.
		if !isInvalid(err) {
			t.Errorf("%s: %v does not wrap ErrInvalidRepo", tc.why, err)
		}
	}

	// The valid shapes those near-misses were testing against.
	must(t, s.PutRepo("alice", Repo{Slug: "a/b", Ref: "feature/x", Path: "src/b", Access: AccessWrite}, []string{"ci"}))

	if _, err := NormalizeAccess(""); err != nil {
		t.Errorf("empty access must default: %v", err)
	}
	if got, _ := NormalizeAccess(" WRITE "); got != AccessWrite {
		t.Errorf("NormalizeAccess(\" WRITE \") = %q", got)
	}
	if _, err := NormalizeAccess("admin"); !isInvalid(err) {
		t.Errorf("NormalizeAccess(admin) = %v", err)
	}
}

func isInvalid(err error) bool { return errors.Is(err, ErrInvalidRepo) }

func TestSlugCaseFoldsToOneAttachment(t *testing.T) {
	// github.com is case-insensitive on both halves, so these are one
	// repository. Two rows would mean two tag sets and two answers to "is it
	// attached"; the stored spelling stays whatever was typed last.
	s := openTest(t)
	must(t, s.PutRepo("alice", Repo{Slug: "wandb/hivemind"}, []string{"ci"}))
	must(t, s.PutRepo("alice", Repo{Slug: "WandB/HiveMind"}, []string{"gpu"}))

	list, err := s.ListRepos("alice")
	must(t, err)
	if len(list) != 1 {
		t.Fatalf("case variants made %d rows: %+v", len(list), list)
	}
	if list[0].Slug != "WandB/HiveMind" {
		t.Errorf("slug = %q, want the case last written", list[0].Slug)
	}
	if !reflect.DeepEqual(list[0].Tags, []string{"gpu"}) {
		t.Errorf("tags = %v, want the replacement set", list[0].Tags)
	}
	must(t, s.DeleteRepo("alice", "github.com", "wandb/hivemind"))
}

func TestPerOwnerCap(t *testing.T) {
	s := openTest(t)
	for i := 0; i < maxReposPerOwner; i++ {
		must(t, s.PutRepo("alice", Repo{Slug: fmt.Sprintf("alice/r%d", i)}, nil))
	}
	if err := s.PutRepo("alice", Repo{Slug: "alice/one-too-many"}, nil); !isInvalid(err) {
		t.Errorf("over the cap = %v, want ErrInvalidRepo", err)
	}
	// An owner at the cap can still fix an existing attachment.
	must(t, s.PutRepo("alice", Repo{Slug: "alice/r0", Ref: "main"}, []string{"ci"}))
	// And the cap is per owner, not global.
	must(t, s.PutRepo("bob", Repo{Slug: "bob/app"}, nil))
}

func TestPragmasOnEveryConnection(t *testing.T) {
	s := openTest(t)
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
	// Two Stores on one file mirror production, where main.go opens separate
	// pools (users/secrets/routes/netrules) on the same sparkbox.db and this
	// package makes it one more. Without busy_timeout on every pooled
	// connection, overlapping write transactions fail instantly with
	// SQLITE_BUSY instead of waiting.
	path := filepath.Join(t.TempDir(), "sparkbox.db")
	a := openAt(t, path)
	b := openAt(t, path)

	hammer := func(s *Store, owner string) error {
		for i := 0; i < 25; i++ {
			if err := s.PutRepo(owner, Repo{Slug: owner + "/app", Ref: fmt.Sprintf("v%d", i)}, []string{"t"}); err != nil {
				return err
			}
			if _, err := s.ListRepos(owner); err != nil {
				return err
			}
			if _, err := s.ReposForSandbox(owner+"vm", owner); err != nil {
				return err
			}
			if err := s.DeleteRepo(owner, "github.com", owner+"/app"); err != nil {
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

// The per-sandbox ref overlay: one instance asking for a branch that is not the
// attachment's, without disturbing the attachment or any other sandbox on the
// same tag.
func TestSandboxRefOverrideAppliesToOneSandboxOnly(t *testing.T) {
	s := openTest(t)
	s.tag(t, "alpha", "van", "web")
	s.tag(t, "beta", "van", "web")
	if err := s.PutRepo("van", Repo{Host: "github.com", Slug: "wandb/hivemind", Ref: "main"}, []string{"web"}); err != nil {
		t.Fatal(err)
	}

	if err := s.SetSandboxRefs("van", "alpha", []SandboxRef{{Slug: "wandb/hivemind", Ref: "feat/x"}}); err != nil {
		t.Fatal(err)
	}

	if got := refFor(t, s, "alpha", "van"); got != "feat/x" {
		t.Errorf("alpha's manifest ref = %q, want the override feat/x", got)
	}
	if got := refFor(t, s, "beta", "van"); got != "main" {
		t.Errorf("beta's manifest ref = %q; an override reached a sandbox that never asked for it", got)
	}
	// The attachment itself is configuration and must be untouched: the next
	// sandbox on this tag still starts where the tag says.
	list, err := s.ListRepos("van")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Ref != "main" {
		t.Errorf("the override rewrote the attachment: %+v", list)
	}
}

// Sandbox names are reusable, so a row that outlives its sandbox decides what a
// DIFFERENT sandbox checks out. This is the reason the store has a lifecycle at
// all, and host.Manager is what calls these two.
func TestSandboxRefOverrideDiesWithItsSandbox(t *testing.T) {
	s := openTest(t)
	s.tag(t, "alpha", "van", "web")
	if err := s.PutRepo("van", Repo{Host: "github.com", Slug: "wandb/hivemind", Ref: "main"}, []string{"web"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSandboxRefs("van", "alpha", []SandboxRef{{Slug: "wandb/hivemind", Ref: "feat/x"}}); err != nil {
		t.Fatal(err)
	}

	if err := s.RenameSandbox("alpha", "gamma"); err != nil {
		t.Fatal(err)
	}
	s.tag(t, "gamma", "van", "web")
	if got := refFor(t, s, "gamma", "van"); got != "feat/x" {
		t.Errorf("after a rename the sandbox lost the branch it asked for: %q", got)
	}

	if err := s.DeleteBySandbox("gamma"); err != nil {
		t.Fatal(err)
	}
	if got := refFor(t, s, "gamma", "van"); got != "main" {
		t.Errorf("a destroyed sandbox's override survived and now decides what its name-successor checks out: %q", got)
	}
	if left, err := s.SandboxRefs("van", "gamma"); err != nil || len(left) != 0 {
		t.Errorf("SandboxRefs after delete = %+v, %v", left, err)
	}
}

// An override that overrides nothing is a row that can only confuse the next
// reader, and a ref the guest would hand to git as an option is the thing refRe
// exists to stop.
func TestSandboxRefOverrideRefusesEmptyAndHostileRefs(t *testing.T) {
	s := openTest(t)
	for _, ref := range []string{"", "   ", "--upload-pack=sh", "feat/../x"} {
		if err := s.SetSandboxRefs("van", "alpha", []SandboxRef{{Slug: "wandb/hivemind", Ref: ref}}); err == nil {
			t.Errorf("SetSandboxRefs accepted ref %q", ref)
		} else if !errors.Is(err, ErrInvalidRepo) {
			t.Errorf("ref %q: err = %v, want ErrInvalidRepo", ref, err)
		}
	}
}

// A second SetSandboxRefs states the whole answer for that sandbox, so dropping
// an override is passing a list without it.
func TestSandboxRefOverrideIsReplacedWholesale(t *testing.T) {
	s := openTest(t)
	s.tag(t, "alpha", "van", "web")
	if err := s.PutRepo("van", Repo{Host: "github.com", Slug: "wandb/hivemind", Ref: "main"}, []string{"web"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSandboxRefs("van", "alpha", []SandboxRef{{Slug: "wandb/hivemind", Ref: "feat/x"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSandboxRefs("van", "alpha", nil); err != nil {
		t.Fatal(err)
	}
	if got := refFor(t, s, "alpha", "van"); got != "main" {
		t.Errorf("clearing the overrides left %q behind", got)
	}
}

func refFor(t *testing.T, s *Store, sandbox, owner string) string {
	t.Helper()
	list, err := s.ReposForSandbox(sandbox, owner)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("ReposForSandbox(%s) = %d repos, want 1", sandbox, len(list))
	}
	return list[0].Ref
}
