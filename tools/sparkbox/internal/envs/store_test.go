package envs

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
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

// rowCount counts environments rows, so "nothing was written" is an assertion
// about the table rather than about what a listing chose to show.
func (s *Store) rowCount(t *testing.T) int {
	t.Helper()
	var n int
	must(t, s.db.QueryRow(`SELECT COUNT(*) FROM environments`).Scan(&n))
	return n
}

func names(list []Environment) []string {
	out := make([]string, 0, len(list))
	for _, e := range list {
		out = append(out, e.Name)
	}
	return out
}

func TestPutGetListDeleteRoundTrip(t *testing.T) {
	s := openTest(t)

	env, err := s.Put("alice", "web", "  the frontend  ", nil)
	must(t, err)
	if env.Owner != "alice" || env.Name != "web" {
		t.Errorf("env = %+v", env)
	}
	if env.Description != "the frontend" {
		t.Errorf("description = %q, want the trimmed form", env.Description)
	}
	if env.State != StateDraft {
		t.Errorf("state = %q, want %q", env.State, StateDraft)
	}
	if env.BuiltAt != nil {
		t.Errorf("BuiltAt = %v on a fresh environment, want nil", env.BuiltAt)
	}
	if env.CreatedAt.IsZero() || env.UpdatedAt.IsZero() {
		t.Errorf("timestamps = %v / %v", env.CreatedAt, env.UpdatedAt)
	}

	got, err := s.Get("alice", "web")
	must(t, err)
	if !reflect.DeepEqual(got, env) {
		t.Errorf("Get = %+v, want %+v", got, env)
	}

	must(t, mustPut(t, s, "alice", "api", ""))
	list, err := s.List("alice")
	must(t, err)
	if want := []string{"api", "web"}; !reflect.DeepEqual(names(list), want) {
		t.Errorf("List = %v, want %v sorted by name", names(list), want)
	}

	must(t, s.Delete("alice", "web"))
	if _, err := s.Get("alice", "web"); !errors.Is(err, ErrNoSuchEnvironment) {
		t.Errorf("Get after Delete = %v, want ErrNoSuchEnvironment", err)
	}
	if err := s.Delete("alice", "web"); !errors.Is(err, ErrNoSuchEnvironment) {
		t.Errorf("second Delete = %v, want ErrNoSuchEnvironment", err)
	}
	if n := s.rowCount(t); n != 1 {
		t.Errorf("rows after delete = %d, want 1", n)
	}
	if _, err := s.Get("alice", "nope"); !errors.Is(err, ErrNoSuchEnvironment) {
		t.Errorf("Get(missing) = %v, want ErrNoSuchEnvironment", err)
	}
	// An empty listing is a slice, not nil, so a JSON encoding is [].
	empty, err := s.List("nobody")
	must(t, err)
	if empty == nil || len(empty) != 0 {
		t.Errorf("List(nobody) = %#v, want an empty non-nil slice", empty)
	}
}

func mustPut(t *testing.T, s *Store, owner, name, desc string) error {
	t.Helper()
	_, err := s.Put(owner, name, desc, nil)
	return err
}

// TestPutRefusesDefault is the environments half of the refusal internal/netrules
// and internal/templates already make. An environment's name IS its tag, so
// `default` would attach the whole object — base image included — to every
// sandbox its owner ever creates.
func TestPutRefusesDefault(t *testing.T) {
	s := openTest(t)
	for _, name := range []string{"default", "DEFAULT", "Default", " default "} {
		t.Run(name, func(t *testing.T) {
			_, err := s.Put("alice", name, "", nil)
			if !errors.Is(err, ErrReservedName) {
				t.Fatalf("Put(%q) = %v, want ErrReservedName", name, err)
			}
			// The message is the whole point of a separate sentinel: a
			// person who typed this needs to be told what it would have
			// done, not that the name is taken.
			msg := err.Error()
			if len(msg) < 120 {
				t.Errorf("refusal is too terse to explain itself (%d chars): %q", len(msg), msg)
			}
			for _, want := range []string{secrets.DefaultTag, "every", "tag"} {
				if !strings.Contains(strings.ToLower(msg), strings.ToLower(want)) {
					t.Errorf("refusal does not mention %q: %q", want, msg)
				}
			}
		})
	}
	if n := s.rowCount(t); n != 0 {
		t.Errorf("rows after refused Puts = %d, want 0", n)
	}
}

func TestPutNameGrammar(t *testing.T) {
	cases := []struct {
		name string
		ok   bool
	}{
		{"web", true},
		{"a", true},
		{"0", true},
		{"my-env-2", true},
		{" web ", true}, // trimmed, then valid
		{strings.Repeat("a", 40), true},
		{"", false},
		{"-web", false},
		{"Web", false},
		{"web_env", false},
		{"web env", false},
		{"web.env", false},
		{"web/env", false},
		{strings.Repeat("a", 41), false},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%q", tc.name), func(t *testing.T) {
			s := openTest(t)
			env, err := s.Put("alice", tc.name, "", nil)
			if tc.ok {
				must(t, err)
				if env.Name != strings.TrimSpace(tc.name) {
					t.Errorf("stored name = %q, want %q", env.Name, strings.TrimSpace(tc.name))
				}
				return
			}
			if !errors.Is(err, ErrInvalidName) {
				t.Fatalf("Put(%q) = %v, want ErrInvalidName", tc.name, err)
			}
			if n := s.rowCount(t); n != 0 {
				t.Errorf("rows after refused Put = %d, want 0", n)
			}
		})
	}
}

func TestPutNeedsAnOwner(t *testing.T) {
	s := openTest(t)
	if _, err := s.Put("", "web", "", nil); !errors.Is(err, ErrInvalidName) {
		t.Fatalf("Put with no owner = %v, want ErrInvalidName", err)
	}
}

// TestPutUpdatesDescriptionOnly proves the re-create path is idempotent and
// non-destructive: fixing a typo in a description must not reset an environment
// somebody is already running back to draft.
func TestPutUpdatesDescriptionOnly(t *testing.T) {
	s := openTest(t)
	first, err := s.Put("alice", "web", "old", nil)
	must(t, err)
	must(t, s.SetScript("alice", "web", "#!/bin/sh\nnpm ci\n", SetupFromRepo))
	must(t, s.SetState("alice", "web", StateReady, "", ""))
	built, err := s.Get("alice", "web")
	must(t, err)

	time.Sleep(2 * time.Millisecond)
	again, err := s.Put("alice", "web", "new", nil)
	must(t, err)

	if n := s.rowCount(t); n != 1 {
		t.Fatalf("rows = %d, want 1 (Put must update, not insert)", n)
	}
	if again.Description != "new" {
		t.Errorf("description = %q, want new", again.Description)
	}
	if !again.CreatedAt.Equal(first.CreatedAt) {
		t.Errorf("created_at moved: %v -> %v", first.CreatedAt, again.CreatedAt)
	}
	if !again.UpdatedAt.After(built.UpdatedAt) {
		t.Errorf("updated_at = %v, want later than %v", again.UpdatedAt, built.UpdatedAt)
	}
	if again.State != StateReady {
		t.Errorf("state = %q, want the build state left alone (%q)", again.State, StateReady)
	}
	if again.SetupScript != built.SetupScript || again.SetupFrom != SetupFromRepo {
		t.Errorf("script clobbered: %q/%q", again.SetupScript, again.SetupFrom)
	}
	if again.BuiltAt == nil || !again.BuiltAt.Equal(*built.BuiltAt) {
		t.Errorf("built_at = %v, want %v", again.BuiltAt, built.BuiltAt)
	}

	// And the returned row is what a subsequent Get says.
	got, err := s.Get("alice", "web")
	must(t, err)
	if !reflect.DeepEqual(got, again) {
		t.Errorf("Put returned %+v, Get says %+v", again, got)
	}
}

// TestListScopesByOwner: two owners hold the SAME environment name, because a
// tag namespace is per-handle. Nothing but the owner term in the query keeps
// their rows apart.
func TestListScopesByOwner(t *testing.T) {
	s := openTest(t)
	must(t, mustPut(t, s, "alice", "web", "alice's frontend"))
	must(t, mustPut(t, s, "bob", "web", "bob's frontend"))
	must(t, mustPut(t, s, "bob", "batch", "bob's jobs"))

	got, err := s.List("alice")
	must(t, err)
	if len(got) != 1 || got[0].Owner != "alice" || got[0].Description != "alice's frontend" {
		t.Fatalf("List(alice) = %+v", got)
	}
	got, err = s.List("bob")
	must(t, err)
	if want := []string{"batch", "web"}; !reflect.DeepEqual(names(got), want) {
		t.Fatalf("List(bob) = %v, want %v", names(got), want)
	}
	for _, e := range got {
		if e.Owner != "bob" {
			t.Fatalf("cross-tenant leak: %+v", e)
		}
	}

	// One owner's write and delete leave the other's row untouched.
	must(t, mustPut(t, s, "alice", "web", "renamed"))
	must(t, s.Delete("alice", "web"))
	bobWeb, err := s.Get("bob", "web")
	must(t, err)
	if bobWeb.Description != "bob's frontend" {
		t.Errorf("bob's row = %+v after alice's edits", bobWeb)
	}
	if _, err := s.Get("alice", "batch"); !errors.Is(err, ErrNoSuchEnvironment) {
		t.Errorf("Get(alice, batch) = %v, want ErrNoSuchEnvironment (bob's row)", err)
	}
	// Deleting under the wrong handle must not reach across either.
	if err := s.Delete("alice", "batch"); !errors.Is(err, ErrNoSuchEnvironment) {
		t.Errorf("Delete(alice, batch) = %v, want ErrNoSuchEnvironment", err)
	}
	if _, err := s.Get("bob", "batch"); err != nil {
		t.Errorf("bob's batch environment was deleted under alice's handle: %v", err)
	}
}

// TestEnvironmentsForSandboxScopesByOwner is this package's copy of
// TestReposForSandboxScopesByOwner: the join carries bt.owner = e.owner as well
// as e.owner = ?, and without the first term a tag name two people share hands
// one of them the other's environment.
func TestEnvironmentsForSandboxScopesByOwner(t *testing.T) {
	s := openTest(t)
	must(t, mustPut(t, s, "alice", "web", "alice's frontend"))
	must(t, mustPut(t, s, "alice", "gpu", "not on this box"))
	must(t, mustPut(t, s, "bob", "web", "bob's frontend"))
	s.tag(t, "alice-box", "alice", "web")
	s.tag(t, "alice-box", "alice", secrets.DefaultTag)

	got, err := s.EnvironmentsForSandbox("alice-box", "alice")
	must(t, err)
	if len(got) != 1 || got[0].Name != "web" || got[0].Owner != "alice" {
		t.Fatalf("EnvironmentsForSandbox(alice) = %+v, want just alice's web", got)
	}
	if got[0].Description != "alice's frontend" {
		t.Fatalf("cross-tenant leak: %+v", got[0])
	}

	// bob's own sandbox, carrying the same tag name, sees only bob's row.
	s.tag(t, "bob-box", "bob", "web")
	got, err = s.EnvironmentsForSandbox("bob-box", "bob")
	must(t, err)
	if len(got) != 1 || got[0].Owner != "bob" || got[0].Description != "bob's frontend" {
		t.Fatalf("EnvironmentsForSandbox(bob) = %+v", got)
	}

	// Asking for alice's sandbox under bob's handle yields nothing, rather
	// than bob's environment through alice's tag rows.
	got, err = s.EnvironmentsForSandbox("alice-box", "bob")
	must(t, err)
	if len(got) != 0 {
		t.Fatalf("owner/sandbox mismatch returned %+v, want nothing", got)
	}

	// A sandbox with no matching tag composes no environment.
	s.tag(t, "plain-box", "alice", secrets.DefaultTag)
	got, err = s.EnvironmentsForSandbox("plain-box", "alice")
	must(t, err)
	if len(got) != 0 {
		t.Fatalf("untagged box = %+v, want nothing", got)
	}
}

func TestMaxEnvironmentsPerOwner(t *testing.T) {
	s := openTest(t)
	for i := 0; i < maxEnvironmentsPerOwner; i++ {
		if err := mustPut(t, s, "alice", fmt.Sprintf("env-%d", i), ""); err != nil {
			t.Fatalf("Put #%d: %v", i, err)
		}
	}
	if _, err := s.Put("alice", "one-too-many", "", nil); !errors.Is(err, ErrTooManyEnvironments) {
		t.Fatalf("Put at the cap = %v, want ErrTooManyEnvironments", err)
	}
	if n := s.rowCount(t); n != maxEnvironmentsPerOwner {
		t.Errorf("rows = %d, want %d", n, maxEnvironmentsPerOwner)
	}
	// The cap is on the insert path only: an owner at the cap must still be
	// able to edit what they have, or a full list becomes a frozen one.
	env, err := s.Put("alice", "env-0", "still editable", nil)
	must(t, err)
	if env.Description != "still editable" {
		t.Errorf("update at the cap = %+v", env)
	}
	must(t, s.SetState("alice", "env-0", StateBuilding, "builder-box", ""))
	// The cap is per owner, so it does not spill onto anybody else.
	must(t, mustPut(t, s, "bob", "web", ""))
}

func TestSetScriptRoundTrip(t *testing.T) {
	s := openTest(t)
	must(t, mustPut(t, s, "alice", "web", ""))

	script := "#!/bin/sh\nset -eu\nnpm ci\n"
	must(t, s.SetScript("alice", "web", script, SetupFromRepo))
	got, err := s.Get("alice", "web")
	must(t, err)
	if got.SetupScript != script || got.SetupFrom != SetupFromRepo {
		t.Fatalf("script = %q from %q", got.SetupScript, got.SetupFrom)
	}
	if got.State != StateDraft {
		t.Errorf("SetScript moved the state to %q", got.State)
	}

	// An empty script from a known source is "we looked and there is none",
	// which must be distinguishable from never having looked.
	must(t, s.SetScript("alice", "web", "", SetupFromAgent))
	got, err = s.Get("alice", "web")
	must(t, err)
	if got.SetupScript != "" || got.SetupFrom != SetupFromAgent {
		t.Fatalf("cleared script = %q from %q", got.SetupScript, got.SetupFrom)
	}

	if err := s.SetScript("alice", "web", script, "telepathy"); !errors.Is(err, ErrInvalidName) {
		t.Errorf("SetScript with an unknown source = %v, want ErrInvalidName", err)
	}
	if err := s.SetScript("alice", "nope", script, SetupFromManual); !errors.Is(err, ErrNoSuchEnvironment) {
		t.Errorf("SetScript on a missing environment = %v, want ErrNoSuchEnvironment", err)
	}
	if err := s.SetScript("bob", "web", script, SetupFromManual); !errors.Is(err, ErrNoSuchEnvironment) {
		t.Errorf("SetScript across owners = %v, want ErrNoSuchEnvironment", err)
	}
}

func TestSetStateRules(t *testing.T) {
	s := openTest(t)
	must(t, mustPut(t, s, "alice", "web", ""))

	must(t, s.SetState("alice", "web", StateBuilding, "builder-1", "ignored"))
	got, err := s.Get("alice", "web")
	must(t, err)
	if got.State != StateBuilding || got.BuildBox != "builder-1" {
		t.Fatalf("building = %+v", got)
	}
	if got.BuildError != "" {
		t.Errorf("build_error = %q on a non-failed state, want cleared", got.BuildError)
	}
	if got.BuiltAt != nil {
		t.Errorf("built_at = %v before any ready state", got.BuiltAt)
	}

	must(t, s.SetState("alice", "web", StateFailed, "builder-1", "npm ci exited 1"))
	got, err = s.Get("alice", "web")
	must(t, err)
	if got.State != StateFailed || got.BuildError != "npm ci exited 1" {
		t.Fatalf("failed = %+v", got)
	}
	if got.BuiltAt != nil {
		t.Errorf("a failure stamped built_at = %v", got.BuiltAt)
	}

	before := time.Now().UTC().Add(-time.Second)
	must(t, s.SetState("alice", "web", StateReady, "", ""))
	ready, err := s.Get("alice", "web")
	must(t, err)
	if ready.State != StateReady || ready.BuildBox != "" {
		t.Fatalf("ready = %+v", ready)
	}
	if ready.BuildError != "" {
		t.Errorf("stale build_error survived into ready: %q", ready.BuildError)
	}
	if ready.BuiltAt == nil || ready.BuiltAt.Before(before) {
		t.Fatalf("built_at = %v, want a fresh stamp", ready.BuiltAt)
	}

	// A later failed rebuild keeps the timestamp of the disk that IS bound.
	must(t, s.SetState("alice", "web", StateFailed, "builder-2", "boom"))
	after, err := s.Get("alice", "web")
	must(t, err)
	if after.BuiltAt == nil || !after.BuiltAt.Equal(*ready.BuiltAt) {
		t.Fatalf("built_at = %v after a failed rebuild, want %v", after.BuiltAt, ready.BuiltAt)
	}
	// ...and a rebuild that succeeds moves it forward again.
	time.Sleep(2 * time.Millisecond)
	must(t, s.SetState("alice", "web", StateReady, "", ""))
	rebuilt, err := s.Get("alice", "web")
	must(t, err)
	if rebuilt.BuiltAt == nil || !rebuilt.BuiltAt.After(*ready.BuiltAt) {
		t.Fatalf("built_at = %v after a successful rebuild, want later than %v", rebuilt.BuiltAt, ready.BuiltAt)
	}

	if err := s.SetState("alice", "web", State("wedged"), "", ""); !errors.Is(err, ErrInvalidName) {
		t.Errorf("SetState with an unknown state = %v, want ErrInvalidName", err)
	}
	if err := s.SetState("alice", "nope", StateReady, "", ""); !errors.Is(err, ErrNoSuchEnvironment) {
		t.Errorf("SetState on a missing environment = %v, want ErrNoSuchEnvironment", err)
	}
	if err := s.SetState("bob", "web", StateReady, "", ""); !errors.Is(err, ErrNoSuchEnvironment) {
		t.Errorf("SetState across owners = %v, want ErrNoSuchEnvironment", err)
	}
}

// TestBuildSessionOutlivesTheBuilder. The transcript is recorded while the
// builder still exists and read long after it is gone, so the one property
// worth pinning is that SetState's clearing of build_box does not take it too.
func TestBuildSessionOutlivesTheBuilder(t *testing.T) {
	s := openTest(t)
	must(t, mustPut(t, s, "alice", "web", ""))
	must(t, s.SetState("alice", "web", StateBuilding, "web-build", ""))
	must(t, s.SetBuildSession("alice", "web", "https://hivemind.example/sessions/a"))

	must(t, s.SetState("alice", "web", StateReady, "", ""))
	got, err := s.Get("alice", "web")
	must(t, err)
	if got.BuildBox != "" {
		t.Fatalf("build_box = %q, want cleared", got.BuildBox)
	}
	if got.BuildSession != "https://hivemind.example/sessions/a" {
		t.Errorf("build_session = %q, want it to survive the finished build", got.BuildSession)
	}

	// A rebuild that produced no session clears it, rather than leaving the
	// previous build's transcript standing as an account of the current disk.
	must(t, s.SetBuildSession("alice", "web", ""))
	got, err = s.Get("alice", "web")
	must(t, err)
	if got.BuildSession != "" {
		t.Errorf("build_session = %q, want cleared", got.BuildSession)
	}

	// A row that is not there is not an error: this is colour on a build whose
	// outcome SetState writes, and an environment deleted mid-build must not
	// turn into a failure the caller has to reason about.
	if err := s.SetBuildSession("bob", "web", "https://x.example/1"); err != nil {
		t.Errorf("SetBuildSession across owners = %v, want nil", err)
	}
	if err := s.SetBuildSession("alice", "gone", "https://x.example/1"); err != nil {
		t.Errorf("SetBuildSession on a missing environment = %v, want nil", err)
	}
}

func TestBuildDenialsRoundTripAndClearAtNextBuild(t *testing.T) {
	s := openTest(t)
	must(t, mustPut(t, s, "alice", "web", ""))
	must(t, s.SetState("alice", "web", StateBuilding, "web-build", ""))
	want := []BuildDeniedDomain{{
		Domain: "registry.npmjs.org", Queries: 3, QTypes: []string{"A", "AAAA"},
		FirstSeenUnix: 100, LastSeenUnix: 102,
	}}
	must(t, s.SetBuildDenials("alice", "web", want, 4))
	must(t, s.SetState("alice", "web", StateFailed, "web-build", "npm failed"))

	got, err := s.Get("alice", "web")
	must(t, err)
	if !reflect.DeepEqual(got.BuildDenials, want) || got.BuildDenialOverflow != 4 {
		t.Fatalf("denials = %+v overflow=%d", got.BuildDenials, got.BuildDenialOverflow)
	}

	must(t, s.SetState("alice", "web", StateBuilding, "web-build", ""))
	got, err = s.Get("alice", "web")
	must(t, err)
	if len(got.BuildDenials) != 0 || got.BuildDenialOverflow != 0 {
		t.Fatalf("stale denials survived rebuild: %+v overflow=%d", got.BuildDenials, got.BuildDenialOverflow)
	}
}

// TestABuildSessionColumnIsAddedToAnOlderDatabase. CREATE TABLE IF NOT EXISTS
// is a no-op on a database that already has the table, so a column added to the
// schema reaches new installs only — and the host that has been building
// environments the longest is exactly the one that would never get it. This
// builds the pre-migration table by hand and opens the store over it.
func TestABuildSessionColumnIsAddedToAnOlderDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sparkbox.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	if _, err := db.Exec(`
		CREATE TABLE environments (
			id          TEXT PRIMARY KEY,
			owner       TEXT NOT NULL,
			name        TEXT NOT NULL,
			description TEXT NOT NULL DEFAULT '',
			setup_sh    TEXT NOT NULL DEFAULT '',
			setup_from  TEXT NOT NULL DEFAULT '',
			build_state TEXT NOT NULL DEFAULT 'draft',
			build_box   TEXT NOT NULL DEFAULT '',
			build_error TEXT NOT NULL DEFAULT '',
			built_at    TIMESTAMP,
			created_at  TIMESTAMP NOT NULL,
			updated_at  TIMESTAMP NOT NULL,
			UNIQUE (owner, name)
		);
		INSERT INTO environments (id, owner, name, created_at, updated_at)
		VALUES ('deadbeef', 'alice', 'web', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP);`); err != nil {
		t.Fatalf("seed the old schema: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close raw: %v", err)
	}

	s := openAt(t, path)
	got, err := s.Get("alice", "web")
	must(t, err)
	if got.BuildSession != "" {
		t.Errorf("build_session = %q on a migrated row, want empty", got.BuildSession)
	}
	must(t, s.SetBuildSession("alice", "web", "https://hivemind.example/sessions/a"))
	got, err = s.Get("alice", "web")
	must(t, err)
	if got.BuildSession != "https://hivemind.example/sessions/a" {
		t.Errorf("build_session = %q after a write to a migrated row", got.BuildSession)
	}
}

// TestBuildingSpansOwners: the restart reconciler is not acting for a user, so
// this is the one query with no owner term.
func TestBuildingSpansOwners(t *testing.T) {
	s := openTest(t)
	must(t, mustPut(t, s, "alice", "web", ""))
	must(t, mustPut(t, s, "alice", "gpu", ""))
	must(t, mustPut(t, s, "bob", "batch", ""))
	must(t, s.SetState("alice", "web", StateBuilding, "b1", ""))
	must(t, s.SetState("bob", "batch", StateBuilding, "b2", ""))
	must(t, s.SetState("alice", "gpu", StateReady, "", ""))

	got, err := s.Building()
	must(t, err)
	if len(got) != 2 {
		t.Fatalf("Building = %+v, want 2 rows", got)
	}
	if got[0].Owner != "alice" || got[0].Name != "web" || got[0].BuildBox != "b1" {
		t.Errorf("Building[0] = %+v", got[0])
	}
	if got[1].Owner != "bob" || got[1].Name != "batch" || got[1].BuildBox != "b2" {
		t.Errorf("Building[1] = %+v", got[1])
	}

	must(t, s.SetState("alice", "web", StateFailed, "", "reconciled away"))
	got, err = s.Building()
	must(t, err)
	if len(got) != 1 || got[0].Owner != "bob" {
		t.Errorf("Building after reconcile = %+v", got)
	}
}

// TestEnvironmentsSurviveReopen: the DB file is a compatibility surface, and
// every store in this family opens it with CREATE TABLE IF NOT EXISTS.
func TestEnvironmentsSurviveReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sparkbox.db")
	first := openAt(t, path)
	want, err := first.Put("alice", "web", "the frontend", nil)
	must(t, err)
	must(t, first.SetScript("alice", "web", "npm ci\n", SetupFromRepo))
	must(t, first.SetState("alice", "web", StateReady, "", ""))
	want, err = first.Get("alice", "web")
	must(t, err)
	first.tag(t, "alice-box", "alice", "web")
	must(t, first.Close())

	again := openAt(t, path)
	got, err := again.Get("alice", "web")
	must(t, err)
	if got.Name != want.Name || got.Description != want.Description || got.State != want.State ||
		got.SetupScript != want.SetupScript || got.SetupFrom != want.SetupFrom {
		t.Fatalf("after reopen = %+v, want %+v", got, want)
	}
	if !got.CreatedAt.Equal(want.CreatedAt) || !got.UpdatedAt.Equal(want.UpdatedAt) {
		t.Errorf("timestamps after reopen = %v/%v, want %v/%v", got.CreatedAt, got.UpdatedAt, want.CreatedAt, want.UpdatedAt)
	}
	if got.BuiltAt == nil || !got.BuiltAt.Equal(*want.BuiltAt) {
		t.Errorf("built_at after reopen = %v, want %v", got.BuiltAt, want.BuiltAt)
	}
	// The reopened store re-runs the shared sandbox_tags DDL; the seeded
	// row must still be there and must still join.
	joined, err := again.EnvironmentsForSandbox("alice-box", "alice")
	must(t, err)
	if len(joined) != 1 || joined[0].Name != "web" {
		t.Errorf("join after reopen = %+v", joined)
	}
}

// TestOpensAlongsideTheOtherTagStores proves the sandbox_tags DDL is compatible
// with the copy internal/secrets owns: a DB this package created must open
// under a store that expects secrets' schema, and vice versa, because whichever
// package boots first is the one that creates the table.
func TestOpensAlongsideTheOtherTagStores(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sparkbox.db")
	sec, err := secrets.Open(path, []byte(strings.Repeat("k", 32)), nil)
	if err != nil {
		t.Fatalf("secrets.Open: %v", err)
	}
	t.Cleanup(func() { sec.Close() })
	must(t, sec.SetTags("alice-box", "alice", []string{"web"}))

	s := openAt(t, path)
	must(t, mustPut(t, s, "alice", "web", ""))
	got, err := s.EnvironmentsForSandbox("alice-box", "alice")
	must(t, err)
	if len(got) != 1 || got[0].Name != "web" {
		t.Fatalf("join over secrets-written tags = %+v", got)
	}
}

// TestAdoptionRecordRoundTrips pins the column an `env rm` reads to decide what
// it may destroy.
//
// The record says what the tag was ALREADY carrying when the environment was
// created over it. Getting it wrong in either direction is a real loss: too
// little and a delete takes somebody's variables with it, too much and an
// environment's own variables outlive it as rows nobody can reach.
func TestAdoptionRecordRoundTrips(t *testing.T) {
	s := openTest(t)

	want := &Adopted{Vars: []string{"INHERITED", "OLD"}, Snapshot: "old-disk"}
	if _, err := s.Put("alice", "web", "", want); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get("alice", "web")
	if err != nil {
		t.Fatal(err)
	}
	if got.Adopted == nil {
		t.Fatal("adoption record did not survive the write")
	}
	if got.Adopted.Snapshot != "old-disk" || len(got.Adopted.Vars) != 2 ||
		got.Adopted.Vars[0] != "INHERITED" || got.Adopted.Vars[1] != "OLD" {
		t.Errorf("adopted = %+v, want %+v", got.Adopted, want)
	}

	// An UPDATE must not restate it. Put is create-and-update in one verb, and
	// `env set web --description "..."` typed months later has no idea what the
	// tag held on the day — so honouring its argument would let an ordinary
	// edit rewrite history, and the window in which the record is wrong is the
	// window in which a delete destroys the wrong thing.
	if _, err := s.Put("alice", "web", "a new description", &Adopted{Vars: []string{"WRONG"}}); err != nil {
		t.Fatal(err)
	}
	after, err := s.Get("alice", "web")
	if err != nil {
		t.Fatal(err)
	}
	if after.Adopted == nil || len(after.Adopted.Vars) != 2 || after.Adopted.Vars[0] != "INHERITED" {
		t.Errorf("an update rewrote the adoption record: %+v", after.Adopted)
	}
	if after.Description != "a new description" {
		t.Errorf("the update did not take: description = %q", after.Description)
	}
}

// TestAdoptingNothingStoresNoRecord keeps "adopted nothing" and "adopted an
// empty set" from being two states. They are the same fact, the ordinary
// environment is the one that adopted nothing, and a record that says so would
// be a row every reader has to unwrap to learn it means no.
func TestAdoptingNothingStoresNoRecord(t *testing.T) {
	s := openTest(t)
	for _, in := range []*Adopted{nil, {}, {Vars: []string{}}} {
		if _, err := s.Put("alice", "web", "", in); err != nil {
			t.Fatal(err)
		}
		got, err := s.Get("alice", "web")
		if err != nil {
			t.Fatal(err)
		}
		if got.Adopted != nil {
			t.Errorf("Put(%+v) stored %+v, want no record at all", in, got.Adopted)
		}
		if err := s.Delete("alice", "web"); err != nil {
			t.Fatal(err)
		}
	}
}
