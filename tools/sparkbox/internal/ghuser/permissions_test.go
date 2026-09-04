package ghuser

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ghapp"
)

// scopeStub is a GitHub good enough for the derive path: it hands out a broad
// token, scopes it, and answers the two verification reads. refuse decides
// which permission sets it rejects, which is the only thing these tests vary.
type scopeStub struct {
	srv *httptest.Server

	mu     sync.Mutex
	scopes []map[string]string
}

func newScopeStub(t *testing.T, refuse func(map[string]string) bool) *scopeStub {
	t.Helper()
	s := &scopeStub{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /login/device/code", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"device_code":"dc","user_code":"ABCD-EFGH","verification_uri":"https://github.com/login/device","expires_in":900,"interval":0}`)
	})
	mux.HandleFunc("POST /login/oauth/access_token", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"access_token":"ghu_broad","expires_in":28800,"refresh_token":"ghr_refresh","refresh_token_expires_in":15897600}`)
	})
	mux.HandleFunc("POST /applications/Iv23liTEST/token/scoped", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Permissions map[string]string `json:"permissions"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		s.mu.Lock()
		s.scopes = append(s.scopes, body.Permissions)
		s.mu.Unlock()
		if refuse != nil && refuse(body.Permissions) {
			// What GitHub answers for a permission outside the grant.
			w.WriteHeader(http.StatusUnprocessableEntity)
			fmt.Fprint(w, `{"message":"Validation Failed"}`)
			return
		}
		fmt.Fprint(w, `{"token":"ghu_scoped","expires_at":"2026-09-01T20:00:00Z"}`)
	})
	mux.HandleFunc("GET /user", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"id":7}`) })
	mux.HandleFunc("GET /repositories/99", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"id":99}`) })
	s.srv = httptest.NewServer(mux)
	t.Cleanup(s.srv.Close)
	return s
}

func (s *scopeStub) seen() []map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]map[string]string(nil), s.scopes...)
}

func scopeFixture(t *testing.T, stub *scopeStub) (*Manager, *Store) {
	t.Helper()
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	client, err := NewClient(Config{
		ClientID: "Iv23liTEST", ClientSecret: "client-secret", HTTPClient: stub.srv.Client(),
		CodeURL: stub.srv.URL + "/login/device/code", TokenURL: stub.srv.URL + "/login/oauth/access_token",
		APIURL: stub.srv.URL, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	store, err := Open(filepath.Join(t.TempDir(), "grants.db"), DeriveKEK([]byte("test signing material")))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return NewManager(client, store, nil), store
}

func wideSubject() Subject {
	return Subject{Owner: "alice", GitHubID: 7, InstallationID: 42, RepoID: 99, Slug: "wandb/hivemind",
		Target: "wandb", Permissions: ghapp.MintPermissions(ghapp.PermWrite)}
}

func authorize(t *testing.T, mgr *Manager, subject Subject) {
	t.Helper()
	started, err := mgr.Start(context.Background(), subject)
	if err != nil {
		t.Fatal(err)
	}
	status, err := mgr.Poll(context.Background(), subject.Owner, started.ID)
	if err != nil {
		t.Fatal(err)
	}
	if status.State != "authorized" {
		t.Fatalf("state = %q", status.State)
	}
}

// A user authorization consented to under the old permission set cannot be
// re-scoped to a wider one. The failure mode that matters is not the 422 — it
// is what the 422 used to cost: the grant was deleted and every later request
// silently came from the App bot, taking attribution away from the person who
// went out of their way to ask for it, an hour after a deploy they did not make.
func TestScopeRefusalKeepsTheGrantAndRetriesWithTheCoreSet(t *testing.T) {
	stub := newScopeStub(t, func(p map[string]string) bool { return !ghapp.IsCoreOnly(p) })
	mgr, store := scopeFixture(t, stub)
	authorize(t, mgr, wideSubject())

	tries := stub.seen()
	if len(tries) != 2 {
		t.Fatalf("scoped token requests = %d, want the wide one and one narrower retry: %v", len(tries), tries)
	}
	if _, ok := tries[0]["vulnerability_alerts"]; !ok {
		t.Errorf("first request did not ask for the wide set: %v", tries[0])
	}
	if !ghapp.IsCoreOnly(tries[1]) {
		t.Errorf("retry was not the core set: %v", tries[1])
	}
	// The retry must never ask for MORE than the original on a permission it
	// keeps, or a read attachment would be quietly promoted by a refusal.
	for name, level := range tries[1] {
		if tries[0][name] != level {
			t.Errorf("retry raised %s from %q to %q", name, tries[0][name], level)
		}
	}

	tok, ok, err := mgr.Token(context.Background(), Subject{
		Owner: "alice", GitHubID: 7, InstallationID: 42, Slug: "wandb/hivemind"})
	if err != nil || !ok || tok.AccessToken != "ghu_scoped" {
		t.Fatalf("Token = (%+v, %v, %v), want the narrower grant kept and used", tok, ok, err)
	}

	// What was stored is what GitHub actually issued, not what was asked for —
	// otherwise the console's nudge would be based on a wish.
	g, err := store.GetBySlug("alice", "wandb/hivemind")
	if err != nil {
		t.Fatal(err)
	}
	if !ghapp.IsCoreOnly(g.Permissions) {
		t.Errorf("stored permissions = %v, want the set that was actually granted", g.Permissions)
	}
	if mgr.Covers("alice", "wandb/hivemind", 7, ghapp.MintPermissions(ghapp.PermWrite)) {
		t.Error("a grant narrower than the minted set reported itself as covering it")
	}
	if !mgr.Covers("alice", "wandb/hivemind", 7, ghapp.CoreMintPermissions(ghapp.PermWrite)) {
		t.Error("a grant reported itself as not covering what it does hold")
	}
}

// The retry is for a refusal, not for every failure. A 500 is GitHub being
// unwell and a second request only doubles the load.
func TestScopeRetriesOnlyOnARefusal(t *testing.T) {
	mux := http.NewServeMux()
	var calls int
	var mu sync.Mutex
	mux.HandleFunc("POST /login/device/code", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"device_code":"dc","user_code":"ABCD-EFGH","verification_uri":"https://github.com/login/device","expires_in":900,"interval":0}`)
	})
	mux.HandleFunc("POST /login/oauth/access_token", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"access_token":"ghu_broad","expires_in":28800,"refresh_token":"ghr_refresh","refresh_token_expires_in":15897600}`)
	})
	mux.HandleFunc("POST /applications/Iv23liTEST/token/scoped", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		w.WriteHeader(http.StatusInternalServerError)
	})
	mux.HandleFunc("GET /user", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"id":7}`) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	stub := &scopeStub{srv: srv}
	mgr, _ := scopeFixture(t, stub)
	started, err := mgr.Start(context.Background(), wideSubject())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.Poll(context.Background(), "alice", started.ID); err == nil {
		t.Fatal("a 500 from the scope endpoint was reported as success")
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Fatalf("scope calls = %d, want 1: a 5xx is not a permission problem", calls)
	}
}

// A grant that GitHub is happy to widen records the wide set, and stops the
// console nudging about it.
func TestAcceptedScopeRecordsTheFullSet(t *testing.T) {
	stub := newScopeStub(t, nil)
	mgr, store := scopeFixture(t, stub)
	authorize(t, mgr, wideSubject())

	if got := len(stub.seen()); got != 1 {
		t.Fatalf("scope calls = %d, want 1: nothing was refused", got)
	}
	g, err := store.GetBySlug("alice", "wandb/hivemind")
	if err != nil {
		t.Fatal(err)
	}
	if g.Permissions["vulnerability_alerts"] != ghapp.PermRead {
		t.Fatalf("stored permissions = %v, want the full minted set", g.Permissions)
	}
	if !mgr.Covers("alice", "wandb/hivemind", 7, ghapp.MintPermissions(ghapp.PermWrite)) {
		t.Error("a grant holding the full set reported itself as stale")
	}
}

func TestCoversIsFalseForAnAbsentGrant(t *testing.T) {
	stub := newScopeStub(t, nil)
	mgr, _ := scopeFixture(t, stub)
	if mgr.Covers("alice", "wandb/hivemind", 7, ghapp.MintPermissions(ghapp.PermWrite)) {
		t.Error("a repository with no grant at all reported itself as covered")
	}
	authorize(t, mgr, wideSubject())
	// Right repository, wrong person: the same answer as no grant.
	if mgr.Covers("alice", "wandb/hivemind", 8, ghapp.MintPermissions(ghapp.PermWrite)) {
		t.Error("another account's github id was reported as covered")
	}
}

// A row written before the permissions column existed still opens, still hands
// out its token, and — crucially — does not nag. Reporting an unknown set as
// stale would ask every user who has ever authorized a repository to
// re-authorize on no evidence at all.
func TestGrantsWrittenBeforeThePermissionsColumnStillWork(t *testing.T) {
	path := filepath.Join(t.TempDir(), "grants.db")
	legacy, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(`
		CREATE TABLE github_scoped_user_grants (
			owner TEXT NOT NULL,
			github_id INTEGER NOT NULL,
			installation_id INTEGER NOT NULL,
			repo_id INTEGER NOT NULL,
			slug TEXT NOT NULL COLLATE NOCASE,
			access_token BLOB NOT NULL,
			refresh_token BLOB NOT NULL,
			access_expires_at TIMESTAMP NOT NULL,
			refresh_expires_at TIMESTAMP NOT NULL,
			updated_at TIMESTAMP NOT NULL,
			PRIMARY KEY (owner, slug),
			UNIQUE (owner, repo_id)
		);`); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := Open(path, DeriveKEK([]byte("test signing material")))
	if err != nil {
		t.Fatalf("opening a pre-migration database: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	future := time.Now().Add(24 * time.Hour)
	if err := store.Put(Grant{Owner: "alice", GitHubID: 7, InstallationID: 42, RepoID: 99,
		Slug: "wandb/hivemind", Token: Token{AccessToken: "ghu_a", RefreshToken: "ghr_a",
			AccessExpiresAt: future, RefreshExpiresAt: future}}); err != nil {
		t.Fatal(err)
	}
	g, err := store.GetBySlug("alice", "wandb/hivemind")
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Permissions) != 0 {
		t.Fatalf("permissions = %v, want empty: a backfilled guess would be wrong in the direction that breaks a refresh", g.Permissions)
	}

	mgr := NewManager(&Client{clientID: "Iv23liTEST", now: time.Now}, store, nil)
	if !mgr.Covers("alice", "wandb/hivemind", 7, ghapp.MintPermissions(ghapp.PermWrite)) {
		t.Error("an unknown permission set was reported as stale; every existing user would be nagged")
	}
}

// Reopening must be idempotent: the migration runs on every boot.
func TestOpeningTwiceDoesNotFailOnTheAddedColumn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "grants.db")
	kek := DeriveKEK([]byte("test signing material"))
	for i := range 3 {
		store, err := Open(path, kek)
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPermissionsRoundTripThroughTheColumn(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "grants.db"), DeriveKEK([]byte("test signing material")))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	future := time.Now().Add(24 * time.Hour)
	perms := ghapp.MintPermissions(ghapp.PermWrite)
	if err := store.Put(Grant{Owner: "alice", GitHubID: 7, InstallationID: 42, RepoID: 99,
		Slug: "wandb/hivemind", Permissions: perms, Token: Token{AccessToken: "ghu_a", RefreshToken: "ghr_a",
			AccessExpiresAt: future, RefreshExpiresAt: future}}); err != nil {
		t.Fatal(err)
	}
	g, err := store.Get("alice", 99)
	if err != nil {
		t.Fatal(err)
	}
	for name, level := range perms {
		if g.Permissions[name] != level {
			t.Errorf("%s = %q after a round trip, want %q", name, g.Permissions[name], level)
		}
	}
	// Stored as readable text, not a blob: a permission name is configuration,
	// and somebody debugging this with sqlite3 should be able to read it.
	var raw string
	if err := store.db.QueryRow(`SELECT permissions FROM github_scoped_user_grants`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if raw != "actions=read,checks=read,contents=write,deployments=read,issues=write,pull_requests=write,security_events=read,statuses=read,vulnerability_alerts=read" {
		t.Fatalf("stored permissions column = %q", raw)
	}
}

func TestDecodePermissionsSkipsNonsenseRatherThanFailingTheRead(t *testing.T) {
	got := decodePermissions("contents=write,,garbage,=read,issues=")
	if len(got) != 1 || got["contents"] != "write" {
		t.Fatalf("decode = %v, want just the one well-formed entry", got)
	}
	if decodePermissions("") != nil || decodePermissions("   ") != nil {
		t.Error("an empty column decoded to a non-nil map; it must read as unknown")
	}
}
