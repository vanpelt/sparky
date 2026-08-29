package templates

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

	"github.com/vanpelt/sparky/tools/sparkbox/internal/secrets"
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

func isInvalid(err error) bool { return errors.Is(err, ErrInvalidBinding) }

// rowCount counts template_tags rows, so "nothing was written" is an assertion
// about the table rather than about what a listing chose to show.
func (s *Store) rowCount(t *testing.T) int {
	t.Helper()
	var n int
	must(t, s.db.QueryRow(`SELECT COUNT(*) FROM template_tags`).Scan(&n))
	return n
}

func TestBindUnbindRoundTrip(t *testing.T) {
	s := openTest(t)

	b, prev, err := s.Bind("alice", "cuda", "cuda12")
	must(t, err)
	if prev != "" {
		t.Errorf("first bind reported a previous snapshot %q", prev)
	}
	if b.Owner != "alice" || b.Tag != "cuda" || b.Snapshot != "cuda12" || b.CreatedAt.IsZero() {
		t.Errorf("binding = %+v", b)
	}

	list, err := s.BindingsForOwner("alice")
	must(t, err)
	if !reflect.DeepEqual(list, []Binding{b}) {
		t.Errorf("BindingsForOwner = %+v, want %+v", list, []Binding{b})
	}

	// A re-point must hand back what it replaced: otherwise the person who
	// typed `bind` cannot tell "I created a binding" from "I silently
	// changed what every future box on this tag boots from".
	time.Sleep(2 * time.Millisecond)
	after, prev, err := s.Bind("alice", "cuda", "cuda13")
	must(t, err)
	if prev != "cuda12" {
		t.Errorf("re-point previous = %q, want cuda12", prev)
	}
	if n := s.rowCount(t); n != 1 {
		t.Errorf("re-point left %d rows, want 1 — (owner, tag) is the primary key", n)
	}
	// created_at is refreshed, deliberately unlike repos.PutRepo: the
	// column answers "since when has this tag booted from THIS snapshot",
	// and the snapshot just changed.
	if !after.CreatedAt.After(b.CreatedAt) {
		t.Errorf("re-point kept created_at %v (was %v)", after.CreatedAt, b.CreatedAt)
	}
	list, err = s.BindingsForOwner("alice")
	must(t, err)
	if len(list) != 1 || list[0].Snapshot != "cuda13" || !list[0].CreatedAt.Equal(after.CreatedAt) {
		t.Errorf("stored binding after re-point = %+v", list)
	}

	gone, err := s.Unbind("alice", "cuda")
	must(t, err)
	if gone.Snapshot != "cuda13" {
		t.Errorf("Unbind returned %+v, want the cuda13 binding", gone)
	}
	if _, err := s.Unbind("alice", "cuda"); !errors.Is(err, ErrNoSuchBinding) {
		t.Errorf("second unbind = %v, want ErrNoSuchBinding", err)
	}
	if n := s.rowCount(t); n != 0 {
		t.Errorf("%d rows survived the unbind", n)
	}
}

func TestBindRefusesTheDefaultTag(t *testing.T) {
	s := openTest(t)
	for _, spelling := range []string{secrets.DefaultTag, "DEFAULT", " Default "} {
		_, _, err := s.Bind("alice", spelling, "cuda12")
		if err == nil {
			t.Fatalf("bind to %q was accepted", spelling)
		}
		// The console maps the whole validation family to 400 by errors.Is,
		// never by matching message text.
		if !isInvalid(err) {
			t.Errorf("bind to %q: %v does not wrap ErrInvalidBinding", spelling, err)
		}
		// And the message must be the one that explains the blast radius,
		// not tagRe's character-set lecture — an uppercase spelling would
		// be refused by the grammar too, and that answer teaches nothing.
		if !strings.Contains(err.Error(), "base image for all of them") {
			t.Errorf("bind to %q: %v does not explain why `default` is the refused word", spelling, err)
		}
	}
	if n := s.rowCount(t); n != 0 {
		t.Errorf("a refused bind wrote %d rows", n)
	}
	// The refusal is about the word, not about binding: the same snapshot
	// on a real tag is fine.
	if _, _, err := s.Bind("alice", "cuda", "cuda12"); err != nil {
		t.Errorf("bind to a normal tag: %v", err)
	}
}

func TestValidationRejectsAndWrapsErrInvalidBinding(t *testing.T) {
	s := openTest(t)
	for _, tc := range []struct {
		why  string
		call func() error
	}{
		{"no owner", func() error { _, _, err := s.Bind("", "cuda", "cuda12"); return err }},
		{"empty tag", func() error { _, _, err := s.Bind("alice", "", "cuda12"); return err }},
		{"uppercase tag", func() error { _, _, err := s.Bind("alice", "CUDA", "cuda12"); return err }},
		{"tag leading hyphen", func() error { _, _, err := s.Bind("alice", "-cuda", "cuda12"); return err }},
		{"tag too long", func() error { _, _, err := s.Bind("alice", strings.Repeat("x", 41), "cuda12"); return err }},
		{"empty snapshot", func() error { _, _, err := s.Bind("alice", "cuda", ""); return err }},
		{"uppercase snapshot", func() error { _, _, err := s.Bind("alice", "cuda", "Cuda12"); return err }},
		{"traversing snapshot", func() error { _, _, err := s.Bind("alice", "cuda", "../../etc/passwd"); return err }},
		{"snapshot too long", func() error { _, _, err := s.Bind("alice", "cuda", strings.Repeat("x", 42)); return err }},
		{"unbind no owner", func() error { _, err := s.Unbind("", "cuda"); return err }},
		{"unbind bad tag", func() error { _, err := s.Unbind("alice", "Cuda!"); return err }},
	} {
		err := tc.call()
		if err == nil {
			t.Errorf("%s: accepted", tc.why)
			continue
		}
		if !isInvalid(err) {
			t.Errorf("%s: %v does not wrap ErrInvalidBinding", tc.why, err)
		}
	}
	if n := s.rowCount(t); n != 0 {
		t.Errorf("refused bindings wrote %d rows", n)
	}

	// The valid shapes those near-misses were testing against. The snapshot
	// grammar is one character longer than the tag grammar on purpose
	// (host/snapshot.go:16), so the 41-character name is accepted where the
	// 41-character tag was not.
	if _, _, err := s.Bind("alice", strings.Repeat("x", 40), strings.Repeat("y", 41)); err != nil {
		t.Errorf("longest legal tag/snapshot pair: %v", err)
	}
	// Surrounding whitespace is trimmed rather than refused: it is what a
	// copy-paste out of `snapshot ls` leaves behind.
	b, _, err := s.Bind("alice", " gpu ", " cuda12 ")
	must(t, err)
	if b.Tag != "gpu" || b.Snapshot != "cuda12" {
		t.Errorf("whitespace not trimmed: %+v", b)
	}
}

func TestTemplatesForSandboxJoinsOnTagsAndScopesByOwner(t *testing.T) {
	s := openTest(t)
	// alice: a template bound to "ci" applies to her sandbox tagged "ci".
	_, _, err := s.Bind("alice", "ci", "alice-base")
	must(t, err)
	// alice: a template bound to "gpu" that the sandbox does NOT carry.
	_, _, err = s.Bind("alice", "gpu", "alice-cuda")
	must(t, err)
	// bob binds the same tag name to his own snapshot — it must never reach
	// alice's sandbox. This is the whole reason the join carries
	// bt.owner = tt.owner as well as tt.owner = ?: without the first term, a
	// tag name two people happen to share hands alice a snapshot she has no
	// claim on, and this is the query that decides which disk boots.
	_, _, err = s.Bind("bob", "ci", "bob-private")
	must(t, err)
	s.tag(t, "alice-box", "alice", "ci")

	got, err := s.TemplatesForSandbox("alice-box", "alice")
	must(t, err)
	if len(got) != 1 || got[0].Snapshot != "alice-base" {
		t.Fatalf("TemplatesForSandbox = %+v, want just alice-base", got)
	}
	if got[0].Owner != "alice" {
		t.Fatalf("cross-tenant leak: %+v", got[0])
	}

	// bob's own sandbox, carrying the same tag name, sees only bob's binding.
	s.tag(t, "bob-box", "bob", "ci")
	got, err = s.TemplatesForSandbox("bob-box", "bob")
	must(t, err)
	if len(got) != 1 || got[0].Snapshot != "bob-private" {
		t.Fatalf("bob's templates = %+v", got)
	}

	// Asking for alice's sandbox under bob's handle yields nothing, rather
	// than bob's binding through alice's tag rows.
	got, err = s.TemplatesForSandbox("alice-box", "bob")
	must(t, err)
	if len(got) != 0 {
		t.Fatalf("owner/sandbox mismatch returned %+v, want nothing", got)
	}
	// And a sandbox with no tags at all reaches no template.
	got, err = s.TemplatesForSandbox("no-such-box", "alice")
	must(t, err)
	if len(got) != 0 {
		t.Fatalf("untagged sandbox reached %+v", got)
	}
}

func TestBindingsForTagsIsOwnerScopedAndOrdered(t *testing.T) {
	s := openTest(t)
	_, _, err := s.Bind("alice", "zeta", "z-snap")
	must(t, err)
	_, _, err = s.Bind("alice", "alpha", "a-snap")
	must(t, err)
	_, _, err = s.Bind("bob", "alpha", "bob-snap")
	must(t, err)

	// Ordering is load-bearing: ctlops refuses a create whose tags resolve
	// to two snapshots and names both, and that message has to read the same
	// way every time the same mistake is made. The tags are passed in the
	// wrong order on purpose.
	got, err := s.BindingsForTags("alice", []string{"zeta", "alpha", "unbound"})
	must(t, err)
	var tags, snaps []string
	for _, b := range got {
		tags = append(tags, b.Tag)
		snaps = append(snaps, b.Snapshot)
	}
	if !reflect.DeepEqual(tags, []string{"alpha", "zeta"}) {
		t.Errorf("tags = %v, want alpha then zeta", tags)
	}
	if !reflect.DeepEqual(snaps, []string{"a-snap", "z-snap"}) {
		t.Errorf("snapshots = %v", snaps)
	}

	// bob's identically-named tag is not alice's template.
	got, err = s.BindingsForTags("bob", []string{"alpha", "zeta"})
	must(t, err)
	if len(got) != 1 || got[0].Snapshot != "bob-snap" {
		t.Errorf("bob's resolution = %+v", got)
	}

	// A handle with no bindings resolves nothing rather than somebody else's.
	got, err = s.BindingsForTags("carol", []string{"alpha", "zeta"})
	must(t, err)
	if len(got) != 0 {
		t.Errorf("carol resolved %+v", got)
	}

	// The overwhelmingly common create — no tags at all — answers nil
	// without a query. A closed database is how the test proves the DB was
	// never touched.
	closed := openAt(t, filepath.Join(t.TempDir(), "sparkbox.db"))
	must(t, closed.Close())
	got, err = closed.BindingsForTags("alice", nil)
	if err != nil || got != nil {
		t.Errorf("empty tag list = %+v, %v; want nil, nil without touching the DB", got, err)
	}
}

func TestPerOwnerCap(t *testing.T) {
	s := openTest(t)
	for i := 0; i < maxTemplatesPerOwner; i++ {
		if _, _, err := s.Bind("alice", fmt.Sprintf("t%d", i), "base"); err != nil {
			t.Fatalf("bind %d: %v", i, err)
		}
	}
	if _, _, err := s.Bind("alice", "one-too-many", "base"); !isInvalid(err) {
		t.Errorf("over the cap = %v, want ErrInvalidBinding", err)
	}
	// An owner at the cap can still move an existing tag onto another
	// snapshot — otherwise somebody who filled their list could not point a
	// tag off a template they had just deleted.
	if _, prev, err := s.Bind("alice", "t0", "replacement"); err != nil || prev != "base" {
		t.Errorf("re-point at the cap = %v (prev %q)", err, prev)
	}
	// And the cap is per owner, not global.
	if _, _, err := s.Bind("bob", "cuda", "bob-base"); err != nil {
		t.Errorf("bob's first bind: %v", err)
	}
}

func TestBindingsSurviveReopen(t *testing.T) {
	// The bindings decide which disk a sandbox boots from, so they have to
	// outlive the process that wrote them — a restart that quietly re-bases
	// everyone on the stock image is exactly the silent failure this design
	// refuses.
	path := filepath.Join(t.TempDir(), "sparkbox.db")
	first := openAt(t, path)
	want, _, err := first.Bind("alice", "cuda", "cuda12")
	must(t, err)
	must(t, first.Close())

	again := openAt(t, path)
	got, err := again.BindingsForTags("alice", []string{"cuda"})
	must(t, err)
	if len(got) != 1 || got[0].Snapshot != want.Snapshot || got[0].Tag != want.Tag {
		t.Fatalf("after reopen = %+v, want %+v", got, want)
	}
	if !got[0].CreatedAt.Equal(want.CreatedAt) {
		t.Errorf("created_at after reopen = %v, want %v", got[0].CreatedAt, want.CreatedAt)
	}
	// The reopened store creates the schema with CREATE TABLE IF NOT EXISTS,
	// so a second Open must not have dropped the shared sandbox_tags table
	// out from under internal/secrets either.
	again.tag(t, "alice-box", "alice", "cuda")
	joined, err := again.TemplatesForSandbox("alice-box", "alice")
	must(t, err)
	if len(joined) != 1 {
		t.Errorf("join after reopen = %+v", joined)
	}
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
	// pools (users/secrets/routes/netrules/repos) on the same sparkbox.db and
	// this package makes it one more. Without busy_timeout on every pooled
	// connection, overlapping write transactions fail instantly with
	// SQLITE_BUSY instead of waiting.
	path := filepath.Join(t.TempDir(), "sparkbox.db")
	a := openAt(t, path)
	b := openAt(t, path)

	hammer := func(s *Store, owner string) error {
		for i := 0; i < 25; i++ {
			if _, _, err := s.Bind(owner, "ci", fmt.Sprintf("snap%d", i)); err != nil {
				return err
			}
			if _, err := s.BindingsForOwner(owner); err != nil {
				return err
			}
			if _, err := s.BindingsForTags(owner, []string{"ci", "gpu"}); err != nil {
				return err
			}
			if _, err := s.TemplatesForSandbox(owner+"vm", owner); err != nil {
				return err
			}
			if _, err := s.Unbind(owner, "ci"); err != nil {
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
