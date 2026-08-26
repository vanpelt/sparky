package metadata

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ghapp"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/repos"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/users"
)

// fakeRepoAccess stands in for the gateway resolver (or the node's relay to
// one), so the handler tests are about the HTTP surface — statuses, headers,
// budgets — and not about GitHub. It records what it was asked, because half of
// what these tests assert is that it was NOT asked.
type fakeRepoAccess struct {
	mu       sync.Mutex
	manifest Manifest
	cred     Credential
	err      error
	calls    []string
}

func (f *fakeRepoAccess) Manifest(_ context.Context, box *host.Sandbox) (Manifest, error) {
	f.record("manifest " + box.Name)
	return f.manifest, f.err
}

func (f *fakeRepoAccess) Credential(_ context.Context, box *host.Sandbox, slug string) (Credential, error) {
	f.record("credential " + box.Name + " " + slug)
	return f.cred, f.err
}

func (f *fakeRepoAccess) record(call string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
}

func (f *fakeRepoAccess) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// withRepos is fixture plus a RepoAccess, so the repo endpoints are exercised
// over the same two sandboxes (alice in slot 5, bob in slot 9) and the same
// working issuer the token tests use — the budget test below needs /token to
// actually mint.
func withRepos(t *testing.T, access RepoAccess) *Server {
	t.Helper()
	s := fixture(t)
	s.repoAccess = access
	return s
}

func TestManifestListsTheCallingSandboxsRepos(t *testing.T) {
	access := &fakeRepoAccess{manifest: Manifest{Repos: []RepoEntry{
		{Host: "github.com", Slug: "wandb/hivemind", Ref: "main", Access: repos.AccessRead},
	}}}
	s := withRepos(t, access)

	rec := request(s, "/repos", "172.30.5.2", "172.30.5.1")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /repos = %d: %s", rec.Code, rec.Body)
	}
	var got Manifest
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Repos) != 1 || got.Repos[0].Slug != "wandb/hivemind" || got.Repos[0].Ref != "main" {
		t.Errorf("manifest = %+v", got.Repos)
	}
	if store := rec.Header().Get("Cache-Control"); store != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", store)
	}
	if seen := access.seen(); len(seen) != 1 || seen[0] != "manifest alice-box" {
		t.Errorf("resolver saw %v", seen)
	}
}

// The guest's clone unit walks this with jq. `null` and `[]` are the difference
// between "nothing attached" and an error message about null.
func TestEmptyManifestIsAnArrayNotNull(t *testing.T) {
	s := withRepos(t, &fakeRepoAccess{})
	rec := request(s, "/repos", "172.30.5.2", "172.30.5.1")
	if body := strings.TrimSpace(rec.Body.String()); body != `{"repos":[]}` {
		t.Errorf("empty manifest = %s", body)
	}
}

func TestCredentialIsServedForTheCallingSandbox(t *testing.T) {
	exp := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	access := &fakeRepoAccess{cred: Credential{Username: "x-access-token", Password: "ghs_secret", ExpiresAt: exp}}
	s := withRepos(t, access)

	rec := request(s, "/github/credential?slug=wandb/hivemind", "172.30.5.2", "172.30.5.1")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /github/credential = %d: %s", rec.Code, rec.Body)
	}
	var got Credential
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Username != "x-access-token" || got.Password != "ghs_secret" || !got.ExpiresAt.Equal(exp) {
		t.Errorf("credential = %+v", got)
	}
	if store := rec.Header().Get("Cache-Control"); store != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store — a cached credential is a credential that outlives its hour", store)
	}
	if seen := access.seen(); len(seen) != 1 || seen[0] != "credential alice-box wandb/hivemind" {
		t.Errorf("resolver saw %v", seen)
	}
}

// The same argument as TestGuestCannotMintATokenForAnotherSandbox, applied to
// the two new routes: a credential handed to the caller named by the
// (attacker-chosen) destination address is one user's private code in another
// user's box.
func TestGuestCannotReadAnotherSandboxsReposOrCredential(t *testing.T) {
	access := &fakeRepoAccess{}
	s := withRepos(t, access)
	for _, path := range []string{"/repos", "/github/credential?slug=wandb/hivemind"} {
		if rec := request(s, path, "172.30.5.2", "172.30.9.1"); rec.Code != http.StatusForbidden {
			t.Errorf("cross-slot GET %s = %d, want 403", path, rec.Code)
		}
	}
	if seen := access.seen(); len(seen) != 0 {
		t.Errorf("resolver was reached across slots: %v", seen)
	}
}

func TestRepoRoutesAreDisabledWithoutAResolver(t *testing.T) {
	s := fixture(t) // no Options.Repos: a host with no store and no App
	for _, path := range []string{"/repos", "/github/credential?slug=wandb/hivemind"} {
		rec := request(s, path, "172.30.5.2", "172.30.5.1")
		if rec.Code != http.StatusNotImplemented {
			t.Errorf("GET %s = %d, want 501", path, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "not enabled") {
			t.Errorf("GET %s said %q", path, rec.Body)
		}
	}
}

func TestCredentialRequiresAWellFormedSlug(t *testing.T) {
	access := &fakeRepoAccess{}
	s := withRepos(t, access)
	for _, query := range []string{
		"",                                    // no ?slug= at all
		"?slug=",                              // present and empty
		"?slug=hivemind",                      // a name with no owner
		"?slug=wandb/hivemind.git",            // what a pasted clone URL leaves
		"?slug=" + "https://github.com/a/b",   // a whole URL
		"?slug=" + "wandb/hivemind/../../etc", // a path, hopefully
	} {
		rec := request(s, "/github/credential"+query, "172.30.5.2", "172.30.5.1")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("GET /github/credential%s = %d, want 400", query, rec.Code)
		}
	}
	if seen := access.seen(); len(seen) != 0 {
		t.Errorf("junk reached the resolver: %v", seen)
	}
}

// The regression this endpoint exists to avoid: a `git fetch` loop must not
// cost the guest its identity. sparkbox-token.service carries
// StartLimitBurst=10/300s, so a shared window would take the OIDC refresh down
// with it.
func TestCredentialBudgetIsSeparateFromTheTokenBudget(t *testing.T) {
	s := withRepos(t, &fakeRepoAccess{cred: Credential{Username: "x-access-token", Password: "ghs_secret"}})
	for i := 0; i < credBurst; i++ {
		if rec := request(s, "/github/credential?slug=wandb/hivemind", "172.30.5.2", "172.30.5.1"); rec.Code != http.StatusOK {
			t.Fatalf("credential %d = %d: %s", i, rec.Code, rec.Body)
		}
	}
	if rec := request(s, "/github/credential?slug=wandb/hivemind", "172.30.5.2", "172.30.5.1"); rec.Code != http.StatusTooManyRequests {
		t.Errorf("over-budget credential = %d, want 429", rec.Code)
	}
	if rec := request(s, "/token", "172.30.5.2", "172.30.5.1"); rec.Code != http.StatusOK {
		t.Errorf("/token after a fetch loop = %d, want 200 — the two windows must not be one", rec.Code)
	}
	// And the manifest keeps its own half of the repo window, so a clone loop
	// cannot make a box forget what it should have checked out.
	if rec := request(s, "/repos", "172.30.5.2", "172.30.5.1"); rec.Code != http.StatusOK {
		t.Errorf("/repos after a fetch loop = %d, want 200", rec.Code)
	}
	// The budget is per sandbox, like the mint's.
	if rec := request(s, "/github/credential?slug=wandb/hivemind", "172.30.9.2", "172.30.9.1"); rec.Code != http.StatusOK {
		t.Errorf("bob = %d, want 200 (the limit must be per-sandbox)", rec.Code)
	}
}

// Each of these statuses is load-bearing on the guest side: 404 is "you do not
// have that repo" and stops the clone, 403 is "somebody has to fix a link or an
// installation", 503 is the one the guest's own retry repairs, and 500 is the
// one that must not be any of the others.
func TestRepoFailuresMapToStatuses(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want int
	}{
		{fmt.Errorf("%w: wandb/hivemind", ErrNoSuchRepo), http.StatusNotFound},
		{fmt.Errorf("%w: no verified github link", ErrRepoDenied), http.StatusForbidden},
		{ErrNotEnabled, http.StatusNotImplemented},
		{fmt.Errorf("%w: %w", ErrUpstream, ghapp.ErrUpstream), http.StatusServiceUnavailable},
		{fmt.Errorf("%w: the gateway is down", ErrNoIssuer), http.StatusServiceUnavailable},
		{errors.New("sqlite: disk I/O error"), http.StatusInternalServerError},
	} {
		s := withRepos(t, &fakeRepoAccess{err: tc.err})
		rec := request(s, "/github/credential?slug=wandb/hivemind", "172.30.5.2", "172.30.5.1")
		if rec.Code != tc.want {
			t.Errorf("credential with %v = %d, want %d", tc.err, rec.Code, tc.want)
		}
		if rec := request(s, "/repos", "172.30.5.2", "172.30.5.1"); rec.Code != tc.want {
			t.Errorf("manifest with %v = %d, want %d", tc.err, rec.Code, tc.want)
		}
	}
	// The 500 keeps the host's own words to itself.
	s := withRepos(t, &fakeRepoAccess{err: errors.New("sqlite: disk I/O error")})
	rec := request(s, "/repos", "172.30.5.2", "172.30.5.1")
	if strings.Contains(rec.Body.String(), "sqlite") {
		t.Errorf("500 body leaked the host's error: %s", rec.Body)
	}
}

func TestCredentialIsNeverLogged(t *testing.T) {
	var logged bytes.Buffer
	s := withRepos(t, &fakeRepoAccess{cred: Credential{
		Username: "x-access-token", Password: "ghs_thisisthesecret", ExpiresAt: time.Now().Add(time.Hour),
	}})
	s.log = slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug}))

	if rec := request(s, "/github/credential?slug=wandb/hivemind", "172.30.5.2", "172.30.5.1"); rec.Code != http.StatusOK {
		t.Fatalf("GET /github/credential = %d: %s", rec.Code, rec.Body)
	}
	if strings.Contains(logged.String(), "ghs_thisisthesecret") {
		t.Fatalf("the token reached the log: %s", logged.String())
	}
	if !strings.Contains(logged.String(), "wandb/hivemind") {
		t.Errorf("the audit line names neither the repo nor anything else: %s", logged.String())
	}
}

// --- LocalRepos: the gateway implementation ---------------------------------

// testAppKey is generated once for the package. RSA keygen is the slowest thing
// in this file by an order of magnitude and none of these tests care which key
// it is.
var testAppKey = sync.OnceValue(func() *rsa.PrivateKey {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return key
})

// githubStub stands in for api.github.com with just the two endpoints a mint
// touches. It counts calls, because several tests below assert that GitHub was
// never reached at all — a refusal that still costs a round trip is a refusal
// that can be used to probe.
type githubStub struct {
	srv *httptest.Server

	mu    sync.Mutex
	calls int
	mints []string // the JSON body of each token request
}

func newGitHubStub(t *testing.T, installation, token http.HandlerFunc) *githubStub {
	t.Helper()
	g := &githubStub{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{name}/installation", installation)
	mux.HandleFunc("POST /app/installations/{id}/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<16))
		g.mu.Lock()
		g.mints = append(g.mints, string(body))
		g.mu.Unlock()
		token(w, r)
	})
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		g.calls++
		g.mu.Unlock()
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(g.srv.Close)
	return g
}

func (g *githubStub) seen() (int, []string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls, append([]string(nil), g.mints...)
}

// installedFor answers the installation lookup with a personal installation of
// the given account id — the id, not the login, being what Authorize binds on.
func installedFor(accountID int64, login string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":42,"app_slug":"sparkbox","account":{"id":%d,"login":%q,"type":"User"}}`, accountID, login)
	}
}

// installedWithPermissions is installedFor for a deployment whose App was
// granted more than `contents` — which is what decides how wide a token
// Credential may ask for. See ghapp.Installation.Narrow.
func installedWithPermissions(accountID int64, login string, perms map[string]string) http.HandlerFunc {
	blob, err := json.Marshal(perms)
	if err != nil {
		panic(err)
	}
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":42,"app_slug":"sparkbox","account":{"id":%d,"login":%q,"type":"User"},"permissions":%s}`,
			accountID, login, blob)
	}
}

func mintsToken(expires time.Time) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"token":"ghs_minted","expires_at":%q}`, expires.Format(time.RFC3339))
	}
}

func refuses(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, body, status)
	}
}

// localSetup is a LocalRepos plus the one thing the store does not expose: the
// path of its database, so a test can seed the sandbox_tags rows internal/repos
// only ever reads.
type localSetup struct {
	LocalRepos
	db string
}

// localFixture builds a LocalRepos over a real store on a temp file, one user
// record, and a real ghapp.App pointed at the stub (or no App at all when stub
// is nil, which is the fleet that never set one up).
func localFixture(t *testing.T, u users.User, stub *githubStub) localSetup {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sparkbox.db")
	store, err := repos.Open(path, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	local := localSetup{LocalRepos: LocalRepos{Repos: store, Users: fakeAccounts{u.Handle: u}}, db: path}
	if stub != nil {
		app, err := ghapp.New(ghapp.Config{
			ClientID: "Iv23liTEST", Key: testAppKey(), BaseURL: stub.srv.URL,
			Logger: slog.New(slog.DiscardHandler),
		})
		if err != nil {
			t.Fatal(err)
		}
		local.App = app
	}
	return local
}

// seedTag writes a sandbox_tags row directly. internal/secrets owns that table
// in production and internal/repos only ever reads it, so a test that needs the
// join has to put the row there itself.
func seedTag(t *testing.T, dbPath, sandbox, owner, tag string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO sandbox_tags (sandbox, owner, tag, created_at) VALUES (?,?,?,?)`,
		sandbox, owner, tag, time.Now().UTC()); err != nil {
		t.Fatalf("seed tag: %v", err)
	}
}

func verifiedUser(handle string, id int64, login string) users.User {
	verified := time.Now().UTC()
	return users.User{
		Handle: handle, Status: "active", GitHubLogin: login, GitHubID: id,
		GitHubVerifiedAt: &verified, GitHubVia: users.GitHubViaKeys,
	}
}

func aliceBox() *host.Sandbox {
	return &host.Sandbox{Name: "alice-box", Owner: "alice", Image: "universal", HostIP: "172.30.5.2"}
}

func TestLocalManifestIsScopedToTheOwnerNotJustTheTag(t *testing.T) {
	local := localFixture(t, verifiedUser("alice", 4242, "alice-gh"), nil)
	seedTag(t, local.db, "alice-box", "alice", "ci")
	if err := local.Repos.PutRepo("alice", repos.Repo{Slug: "wandb/hivemind", Ref: "main"}, []string{"ci"}); err != nil {
		t.Fatal(err)
	}
	// bob's tag is spelled the same. It is not the same tag.
	if err := local.Repos.PutRepo("bob", repos.Repo{Slug: "bob/secrets"}, []string{"ci"}); err != nil {
		t.Fatal(err)
	}
	manifest, err := local.Manifest(context.Background(), aliceBox())
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Repos) != 1 || manifest.Repos[0].Slug != "wandb/hivemind" {
		t.Fatalf("manifest = %+v — a shared tag name must not join two owners", manifest.Repos)
	}
	if manifest.Repos[0].Ref != "main" || manifest.Repos[0].Access != repos.AccessRead {
		t.Errorf("entry = %+v", manifest.Repos[0])
	}
}

func TestLocalManifestOfAnUnownedSandboxIsEmpty(t *testing.T) {
	local := localFixture(t, verifiedUser("alice", 4242, "alice-gh"), nil)
	manifest, err := local.Manifest(context.Background(), &host.Sandbox{Name: "orphan"})
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Repos == nil || len(manifest.Repos) != 0 {
		t.Errorf("manifest = %+v, want an empty list", manifest.Repos)
	}
}

func TestLocalCredentialIsScopedToTheOneRepository(t *testing.T) {
	expires := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	stub := newGitHubStub(t, installedFor(4242, "alice-gh"), mintsToken(expires))
	local := localFixture(t, verifiedUser("alice", 4242, "alice-gh"), stub)
	seedTag(t, local.db, "alice-box", "alice", "ci")
	if err := local.Repos.PutRepo("alice", repos.Repo{Slug: "wandb/hivemind"}, []string{"ci"}); err != nil {
		t.Fatal(err)
	}
	if err := local.Repos.PutRepo("alice", repos.Repo{Slug: "wandb/other"}, []string{"ci"}); err != nil {
		t.Fatal(err)
	}

	cred, err := local.Credential(context.Background(), aliceBox(), "wandb/hivemind")
	if err != nil {
		t.Fatal(err)
	}
	if cred.Username != "x-access-token" || cred.Password != "ghs_minted" || !cred.ExpiresAt.Equal(expires) {
		t.Errorf("credential = %+v", cred)
	}
	_, mints := stub.seen()
	if len(mints) != 1 {
		t.Fatalf("mints = %v", mints)
	}
	// The scope is the point: one repository, read on contents, and nothing
	// else the installation happens to cover.
	if !strings.Contains(mints[0], `["hivemind"]`) || strings.Contains(mints[0], "other") {
		t.Errorf("token was not scoped to the one repository asked for: %s", mints[0])
	}
	if !strings.Contains(mints[0], `"contents":"read"`) {
		t.Errorf("permissions = %s", mints[0])
	}
}

func TestLocalCredentialAsksForWriteOnlyWhenTheAttachmentSaysSo(t *testing.T) {
	stub := newGitHubStub(t, installedFor(4242, "alice-gh"), mintsToken(time.Now().Add(time.Hour)))
	local := localFixture(t, verifiedUser("alice", 4242, "alice-gh"), stub)
	seedTag(t, local.db, "alice-box", "alice", "ci")
	if err := local.Repos.PutRepo("alice", repos.Repo{Slug: "wandb/hivemind", Access: repos.AccessWrite}, []string{"ci"}); err != nil {
		t.Fatal(err)
	}
	if _, err := local.Credential(context.Background(), aliceBox(), "wandb/hivemind"); err != nil {
		t.Fatal(err)
	}
	_, mints := stub.seen()
	if len(mints) != 1 || !strings.Contains(mints[0], `"contents":"write"`) {
		t.Errorf("mint body = %v", mints)
	}
	// This installation declares no permissions at all, which is every App that
	// predates the widened set. It must still get a working `contents` token
	// rather than a request GitHub refuses wholesale — the fallback that keeps
	// clones working on an older deployment. See ghapp.Installation.Narrow.
	if strings.Contains(mints[0], "pull_requests") {
		t.Errorf("asked an installation for a permission it never declared: %s", mints[0])
	}
}

// TestLocalCredentialWidensToWhatTheAppHolds is the `gh` half of the feature.
// The CLI speaks no credential-helper protocol, so it runs on this same token
// (see the wrapper in deploy/install-guest-identity.sh) — and `gh pr create` on
// a token that can push the branch but not open the pull request is a strange
// half-grant. The set follows the attachment's access level, intersected with
// what the App was actually granted.
func TestLocalCredentialWidensToWhatTheAppHolds(t *testing.T) {
	stub := newGitHubStub(t,
		installedWithPermissions(4242, "alice-gh", map[string]string{
			"contents":      "write",
			"pull_requests": "write",
			// `issues` is deliberately absent: a permission the App does not
			// hold must be dropped from the request, not fail the whole mint.
		}),
		mintsToken(time.Now().Add(time.Hour)))
	local := localFixture(t, verifiedUser("alice", 4242, "alice-gh"), stub)
	seedTag(t, local.db, "alice-box", "alice", "ci")
	if err := local.Repos.PutRepo("alice", repos.Repo{Slug: "wandb/hivemind", Access: repos.AccessWrite}, []string{"ci"}); err != nil {
		t.Fatal(err)
	}
	if _, err := local.Credential(context.Background(), aliceBox(), "wandb/hivemind"); err != nil {
		t.Fatal(err)
	}
	_, mints := stub.seen()
	if len(mints) != 1 {
		t.Fatalf("mints = %v", mints)
	}
	for _, want := range []string{`"contents":"write"`, `"pull_requests":"write"`} {
		if !strings.Contains(mints[0], want) {
			t.Errorf("mint body %s is missing %s", mints[0], want)
		}
	}
	if strings.Contains(mints[0], "issues") {
		t.Errorf("asked for a permission the installation does not hold: %s", mints[0])
	}
	// Still one repository. Widening what the token may DO must never widen
	// what it may do it TO.
	if !strings.Contains(mints[0], `["hivemind"]`) {
		t.Errorf("token was not scoped to the one repository: %s", mints[0])
	}
}

// A read attachment stays read on every permission in the set: a token that
// could open a pull request but not push is not what `--write` was withheld for.
func TestLocalCredentialNeverWidensAReadAttachmentToWrite(t *testing.T) {
	stub := newGitHubStub(t,
		installedWithPermissions(4242, "alice-gh", map[string]string{
			"contents": "write", "pull_requests": "write",
		}),
		mintsToken(time.Now().Add(time.Hour)))
	local := localFixture(t, verifiedUser("alice", 4242, "alice-gh"), stub)
	seedTag(t, local.db, "alice-box", "alice", "ci")
	if err := local.Repos.PutRepo("alice", repos.Repo{Slug: "wandb/hivemind"}, []string{"ci"}); err != nil {
		t.Fatal(err)
	}
	if _, err := local.Credential(context.Background(), aliceBox(), "wandb/hivemind"); err != nil {
		t.Fatal(err)
	}
	_, mints := stub.seen()
	if len(mints) != 1 || strings.Contains(mints[0], `"write"`) {
		t.Errorf("a read attachment minted a write token: %v", mints)
	}
}

// The query parameter selects among this sandbox's attachments; it never adds
// one. A repository the owner has attached to a DIFFERENT tag is as unreachable
// here as a repository they never attached at all.
func TestLocalCredentialRefusesARepositoryThisSandboxIsNotAttachedTo(t *testing.T) {
	stub := newGitHubStub(t, installedFor(4242, "alice-gh"), mintsToken(time.Now().Add(time.Hour)))
	local := localFixture(t, verifiedUser("alice", 4242, "alice-gh"), stub)
	seedTag(t, local.db, "alice-box", "alice", "ci")
	if err := local.Repos.PutRepo("alice", repos.Repo{Slug: "wandb/hivemind"}, []string{"ci"}); err != nil {
		t.Fatal(err)
	}
	if err := local.Repos.PutRepo("alice", repos.Repo{Slug: "wandb/elsewhere"}, []string{"prod"}); err != nil {
		t.Fatal(err)
	}
	for _, slug := range []string{"wandb/elsewhere", "someone/private"} {
		if _, err := local.Credential(context.Background(), aliceBox(), slug); !errors.Is(err, ErrNoSuchRepo) {
			t.Errorf("credential for %s = %v, want ErrNoSuchRepo", slug, err)
		}
	}
	if calls, _ := stub.seen(); calls != 0 {
		t.Errorf("github was asked about a repository this sandbox has no attachment to (%d calls)", calls)
	}
}

// github.com folds case on both halves of a slug and the store's column is
// NOCASE; Go is not, so the match has to fold too or the clone URL a user
// pasted misses its own attachment.
func TestLocalCredentialMatchesTheAttachmentCaseInsensitively(t *testing.T) {
	stub := newGitHubStub(t, installedFor(4242, "alice-gh"), mintsToken(time.Now().Add(time.Hour)))
	local := localFixture(t, verifiedUser("alice", 4242, "alice-gh"), stub)
	seedTag(t, local.db, "alice-box", "alice", "ci")
	if err := local.Repos.PutRepo("alice", repos.Repo{Slug: "wandb/Hivemind"}, []string{"ci"}); err != nil {
		t.Fatal(err)
	}
	if _, err := local.Credential(context.Background(), aliceBox(), "WandB/hivemind"); err != nil {
		t.Fatalf("case-folded credential = %v", err)
	}
}

// The StrongGitHubLink gate, at the moment of use rather than only at attach:
// a link can weaken (or an account be suspended) after an attachment was made.
func TestLocalCredentialRefusesAnOwnerWhoseLinkIsNotStrong(t *testing.T) {
	verified := time.Now().UTC()
	for _, tc := range []struct {
		name string
		user users.User
	}{
		{"never linked", users.User{Handle: "alice", Status: "active"}},
		{"asserted by a third party", users.User{
			Handle: "alice", Status: "active", GitHubLogin: "alice-gh", GitHubID: 4242,
			GitHubVerifiedAt: &verified, GitHubVia: users.GitHubViaAssertion,
		}},
		{"recorded but unverified", users.User{
			Handle: "alice", Status: "active", GitHubLogin: "alice-gh", GitHubID: 4242,
			GitHubVia: users.GitHubViaKeys,
		}},
		{"suspended account", func() users.User {
			u := verifiedUser("alice", 4242, "alice-gh")
			u.Status = "suspended"
			return u
		}()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := newGitHubStub(t, installedFor(4242, "alice-gh"), mintsToken(time.Now().Add(time.Hour)))
			local := localFixture(t, tc.user, stub)
			seedTag(t, local.db, "alice-box", "alice", "ci")
			if err := local.Repos.PutRepo("alice", repos.Repo{Slug: "wandb/hivemind"}, []string{"ci"}); err != nil {
				t.Fatal(err)
			}
			if _, err := local.Credential(context.Background(), aliceBox(), "wandb/hivemind"); !errors.Is(err, ErrRepoDenied) {
				t.Fatalf("credential = %v, want ErrRepoDenied", err)
			}
			if calls, _ := stub.seen(); calls != 0 {
				t.Errorf("github was reached before the link was checked (%d calls)", calls)
			}
		})
	}
}

// The binding, from this side: the installation belongs to a different GitHub
// account than the owner's link names, so no token is minted for it.
func TestLocalCredentialRefusesAnInstallationBoundToSomebodyElse(t *testing.T) {
	stub := newGitHubStub(t, installedFor(9999, "someone-else"), mintsToken(time.Now().Add(time.Hour)))
	local := localFixture(t, verifiedUser("alice", 4242, "alice-gh"), stub)
	seedTag(t, local.db, "alice-box", "alice", "ci")
	if err := local.Repos.PutRepo("alice", repos.Repo{Slug: "wandb/hivemind"}, []string{"ci"}); err != nil {
		t.Fatal(err)
	}
	_, err := local.Credential(context.Background(), aliceBox(), "wandb/hivemind")
	if !errors.Is(err, ErrRepoDenied) {
		t.Fatalf("credential = %v, want ErrRepoDenied", err)
	}
	if _, mints := stub.seen(); len(mints) != 0 {
		t.Errorf("a token was minted for somebody else's installation: %v", mints)
	}
}

func TestLocalCredentialTranslatesGitHubsRefusals(t *testing.T) {
	for _, tc := range []struct {
		name         string
		installation http.HandlerFunc
		want         error
	}{
		// Not installed is a 404 and not a refusal, and it is a 404 on a node
		// too — see githubError.
		{"not installed", refuses(http.StatusNotFound, `{"message":"Not Found"}`), ErrNoSuchRepo},
		{"github is unwell", refuses(http.StatusBadGateway, "bad gateway"), ErrUpstream},
		{"rate limited", refuses(http.StatusTooManyRequests, "slow down"), ErrUpstream},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := newGitHubStub(t, tc.installation, mintsToken(time.Now().Add(time.Hour)))
			local := localFixture(t, verifiedUser("alice", 4242, "alice-gh"), stub)
			seedTag(t, local.db, "alice-box", "alice", "ci")
			if err := local.Repos.PutRepo("alice", repos.Repo{Slug: "wandb/hivemind"}, []string{"ci"}); err != nil {
				t.Fatal(err)
			}
			if _, err := local.Credential(context.Background(), aliceBox(), "wandb/hivemind"); !errors.Is(err, tc.want) {
				t.Fatalf("credential = %v, want %v", err, tc.want)
			}
		})
	}
}

// A fleet with attachments and no App key is a supported state: public
// repositories still clone, and the credential endpoint says so in the shape
// the guest already understands rather than failing a mint.
func TestLocalCredentialWithoutAnAppIsNotEnabled(t *testing.T) {
	local := localFixture(t, verifiedUser("alice", 4242, "alice-gh"), nil)
	seedTag(t, local.db, "alice-box", "alice", "ci")
	if err := local.Repos.PutRepo("alice", repos.Repo{Slug: "wandb/hivemind"}, []string{"ci"}); err != nil {
		t.Fatal(err)
	}
	if _, err := local.Credential(context.Background(), aliceBox(), "wandb/hivemind"); !errors.Is(err, ErrNotEnabled) {
		t.Errorf("credential without an app = %v, want ErrNotEnabled", err)
	}
	// And a host with no store at all is the same answer on both methods.
	empty := LocalRepos{}
	if _, err := empty.Manifest(context.Background(), aliceBox()); !errors.Is(err, ErrNotEnabled) {
		t.Errorf("manifest with no store = %v, want ErrNotEnabled", err)
	}
	if _, err := empty.Credential(context.Background(), aliceBox(), "wandb/hivemind"); !errors.Is(err, ErrNotEnabled) {
		t.Errorf("credential with no store = %v, want ErrNotEnabled", err)
	}
}

// Without a user store there is no way to establish whose GitHub account the
// owner is, and a mint with that question unanswered is the cross-tenant hole
// the whole design exists to avoid. Fail closed.
func TestLocalCredentialWithoutAUserStoreRefuses(t *testing.T) {
	stub := newGitHubStub(t, installedFor(4242, "alice-gh"), mintsToken(time.Now().Add(time.Hour)))
	local := localFixture(t, verifiedUser("alice", 4242, "alice-gh"), stub)
	local.Users = nil
	seedTag(t, local.db, "alice-box", "alice", "ci")
	if err := local.Repos.PutRepo("alice", repos.Repo{Slug: "wandb/hivemind"}, []string{"ci"}); err != nil {
		t.Fatal(err)
	}
	if _, err := local.Credential(context.Background(), aliceBox(), "wandb/hivemind"); !errors.Is(err, ErrRepoDenied) {
		t.Errorf("credential with no accounts = %v, want ErrRepoDenied", err)
	}
	if calls, _ := stub.seen(); calls != 0 {
		t.Errorf("github was reached with no account to bind to (%d calls)", calls)
	}
}
