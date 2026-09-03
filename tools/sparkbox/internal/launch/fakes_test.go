package launch

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/edgeauth"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/repos"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/users"
)

// The real stores must keep satisfying this package's two interfaces
// structurally. That assertion is the entire justification for declaring narrow
// interfaces instead of taking the concrete types: a signature drift in ctlops
// or repos should fail the build of THIS package's tests, where the person who
// changed it can see why, rather than the integrator's.
//
// The idiom is ctlops' own (internal/ctlops/fakes_test.go:26-41). It lives in a
// _test.go file because a production import of *repos.Store into an interface
// assertion buys nothing at runtime, and because internal/ctlops/fakes_test.go
// is `package ctlops` and so cannot be imported from here at all.
var (
	_ Sandboxes    = (*ctlops.Ops)(nil)
	_ Environments = (*ctlops.Ops)(nil)
	_ Attachments  = (*repos.Store)(nil)
	// The optional interfaces are pinned here too, and they are the ones that
	// most need it: nothing fails at compile time when an ASSERTION stops
	// matching, so a renamed method on *ctlops.Ops would silently turn
	// environment-aware tag selection back into the whole tag set, and every
	// test would still pass because the fallback is the old behaviour.
	_ envAwaiter        = (*ctlops.Ops)(nil)
	_ environmentLister = (*ctlops.Ops)(nil)
)

// errStore is what a fake returns when a test wants the store to fail.
var errStore = errors.New("store is on fire")

// fakeOps is an in-memory Sandboxes.
//
// Its Create calls t.Fatal by default, which is not a convenience: it is how
// every read path in this package is pinned side-effect free. A GET that grew a
// create — through a refactor, through a helper that "just resumes it" — fails
// the test that drove it rather than shipping and being discovered by a link
// scanner that walked somebody's pull request.
type fakeOps struct {
	t *testing.T

	mu           sync.Mutex
	boxes        []ctlops.SandboxInfo
	listErr      error
	environments map[string]ctlops.EnvironmentInfo
	envErr       error

	// allowCreate opts one test into exercising the create path. Every GET test
	// leaves it false, which is what pins the read paths side-effect free.
	allowCreate bool
	created     []ctlops.CreateArgs
	// createErr, when set, is what Create fails with — the way a test reaches
	// the KindLimit screen without a host manager, an admission budget or a VM.
	createErr error

	// entered is closed by the first Create, and held blocks that Create until
	// the test closes it. Together they are how the singleflight test observes
	// a create in flight: the leader parks inside the group, so a second
	// request arriving in that window has to be a follower or the collapse did
	// not happen. Both are nil for every other test.
	entered chan struct{}
	held    chan struct{}

	// awaited records the names AwaitEnv was called for, and awaitErr is what
	// it fails with. The fake implements envAwaiter unconditionally: the
	// barrier is asserted by what it recorded, not by whether the assertion in
	// awaitEnv found a method.
	awaited  []string
	awaitErr error

	// envs is what EnvironmentsForTags answers with, filtered to the tags asked
	// about, and envsErr is what it fails with. Nil envs with no error is the
	// ordinary host where the attachment's tags name no environment — which is
	// also, deliberately, the same answer as a host too old to have the method,
	// so the fallback path and the empty path are exercised by the same tests.
	envs    []ctlops.EnvironmentInfo
	envsErr error
}

func (f *fakeOps) GetEnvironment(_ ctlops.Caller, name string) (ctlops.EnvironmentInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.envErr != nil {
		return ctlops.EnvironmentInfo{}, f.envErr
	}
	for key, env := range f.environments {
		if strings.EqualFold(key, name) {
			return env, nil
		}
	}
	return ctlops.EnvironmentInfo{}, ctlops.NotFound("env.get", "environment", name)
}

func (f *fakeOps) List(_ context.Context, _ ctlops.Caller) ([]ctlops.SandboxInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := make([]ctlops.SandboxInfo, len(f.boxes))
	copy(out, f.boxes)
	return out, nil
}

func (f *fakeOps) Get(_ context.Context, _ ctlops.Caller, name string) (ctlops.SandboxInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, b := range f.boxes {
		if b.Name == name {
			return b, nil
		}
	}
	return ctlops.SandboxInfo{}, ctlops.NotFound(op, "sandbox", name)
}

func (f *fakeOps) Create(_ context.Context, _ ctlops.Caller, a ctlops.CreateArgs) (ctlops.SandboxInfo, error) {
	if !f.allowCreate {
		// t.Fatal from the goroutine running the handler is deliberate: this is
		// the pin that says a GET never writes, and it must fail the test that
		// reached here rather than be swallowed into a 500 the assertions might
		// not notice.
		f.t.Helper()
		f.t.Errorf("Create must not be reached: a read path built a sandbox (args %+v)", a)
		return ctlops.SandboxInfo{}, errStore
	}

	// Announce the create and park, holding no lock, so the test can start a
	// second request while this one is still inside the singleflight group.
	// The channels are read under the mutex because a collapse that FAILED
	// would put two goroutines here at once, and a data race is a much worse
	// way to learn that than the create count the test is about to assert.
	f.mu.Lock()
	entered, held := f.entered, f.held
	f.entered = nil
	f.mu.Unlock()
	if entered != nil {
		close(entered)
	}
	if held != nil {
		<-held
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return ctlops.SandboxInfo{}, f.createErr
	}
	f.created = append(f.created, a)
	info := ctlops.SandboxInfo{Name: "created-box", Owner: testHandle, State: "running",
		TerminalURL: "https://created-box-xterm.example.test/"}
	f.boxes = append(f.boxes, info)
	return info, nil
}

// createCount reports how many sandboxes the fake built, for the tests that
// care about exactly one.
func (f *fakeOps) createCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.created)
}

// fakeRepos is an in-memory Attachments.
//
// It mirrors the real store's two properties that this package's logic actually
// depends on, and no others: slug comparison is case-insensitive (the columns
// are COLLATE NOCASE), and ReposForSandbox reports the EFFECTIVE ref — the
// per-sandbox override already folded over the attachment's default — rather
// than the override row. A fake that returned the override instead would let
// the naive `eff == want` rule pass its tests and fail in production, which is
// precisely the bug the match table exists to catch.
type fakeRepos struct {
	// attachments is what ListRepos returns, keyed by handle.
	attachments map[string][]repos.Repo
	// boxes maps a handle to that handle's sandbox names, each with the
	// manifest ReposForSandbox should report for it.
	boxes map[string]map[string][]repos.Repo

	listErr     error
	forRepoErr  error
	forSandErr  error
	sandboxSeen []string
}

func (f *fakeRepos) ListRepos(owner string) ([]repos.Repo, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.attachments[owner], nil
}

func (f *fakeRepos) SandboxesForRepo(owner, host, slug string) ([]string, error) {
	if f.forRepoErr != nil {
		return nil, f.forRepoErr
	}
	if host != gitHubHost {
		return nil, errors.New("fakeRepos: unexpected host " + host)
	}
	var out []string
	for name, manifest := range f.boxes[owner] {
		for _, r := range manifest {
			if strings.EqualFold(r.Slug, slug) {
				out = append(out, name)
				break
			}
		}
	}
	// The real query is ORDER BY bt.sandbox, and the ranking's stable-sort
	// tie-break leans on that determinism, so the fake sorts too — a map range
	// would otherwise make a tie-break test pass or fail at random.
	sort.Strings(out)
	return out, nil
}

func (f *fakeRepos) ReposForSandbox(sandbox, owner string) ([]repos.Repo, error) {
	if f.forSandErr != nil {
		return nil, f.forSandErr
	}
	f.sandboxSeen = append(f.sandboxSeen, sandbox)
	return f.boxes[owner][sandbox], nil
}

// testAccounts is an edgeauth.Accounts that knows the two handles these tests
// sign in as. It matters that it is not nil: Require reads the record to refuse
// an account disabled since its cookie was issued, and a handler wired without
// one would make everybody a non-operator by a different code path than
// production takes.
type testAccounts map[string]users.User

func (a testAccounts) Get(handle string) (users.User, error) {
	u, ok := a[handle]
	if !ok {
		return users.User{}, users.ErrNoSuchUser
	}
	return u, nil
}

// newHandler builds a Handler over fakes directly rather than through New,
// because New requires the concrete *ctlops.Ops and *repos.Store that these
// tests exist to avoid needing. That is the reason the Handler's own fields are
// the interfaces and not the concrete types.
//
// The signer and accounts are real, not fakes: the auth gate is half of what
// these tests assert (the bounce, its return URL, the ordering against the
// first-party check), and a stubbed one would prove none of it.
func newHandler(t *testing.T, ops *fakeOps, store *fakeRepos) *Handler {
	t.Helper()
	return &Handler{
		ops:      ops,
		envs:     ops,
		repos:    store,
		accounts: testAccounts{testHandle: {Handle: testHandle, Status: users.StatusActive, InvitedBy: "someoneelse"}, otherHandle: {Handle: otherHandle, Status: users.StatusActive, InvitedBy: "someoneelse"}},
		signer:   testSigner,
		// A leading "go." on the origin and a bare zone: the same shape New
		// builds, so the first-party check is compared against what production
		// would compare against.
		subdomain: "go",
		domain:    "example.test",
		origin:    "https://go.example.test",
		// Composed the way New composes it, from the same zone, so the header
		// tests compare against a real policy rather than the zero value a
		// struct literal would otherwise leave here.
		csp:      pageCSP("example.test"),
		loginURL: "https://login.example.test/",
		homeURL:  "https://my.example.test/",
		log:      slog.New(slog.DiscardHandler),
	}
}

// testSigner mints the session cookies these tests sign in with. One signer for
// the whole package so a token minted by a helper verifies in any handler the
// package builds.
var testSigner = edgeauth.NewSigner([]byte("launch-test-key-material"))

// otherHandle is a second, unrelated account. It exists for one assertion that
// matters more than any other on this surface: a launch link is a public URL,
// and the sandboxes it resolves must be the CLICKER's and nobody else's.
const otherHandle = "someone-else"

// signedIn returns a request option that presents handle's session cookie.
func signedIn(t *testing.T, handle string) func(*http.Request) {
	t.Helper()
	tok, _, err := testSigner.Mint(edgeauth.Identity{Handle: handle}, time.Hour)
	if err != nil {
		t.Fatalf("mint session for %s: %v", handle, err)
	}
	return func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: edgeauth.CookieName, Value: tok})
	}
}

// asBrowser is what makes the auth gate take its redirect branch rather than
// its 401 branch: edgeauth.challenge tests Accept for text/html.
func asBrowser(r *http.Request) { r.Header.Set("Accept", "text/html,application/xhtml+xml,*/*;q=0.8") }

// fromThePage sets the Origin a real form POST from this door would carry.
func fromThePage(r *http.Request) { r.Header.Set("Origin", "https://go.example.test") }

// serveLaunch drives the real mux, which is the only way to assert that a route
// is mounted behind the gate it is supposed to be behind rather than merely
// that its handler function is correct.
func serveLaunch(t *testing.T, h *Handler, method, url string, opts ...func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, url, nil)
	for _, o := range opts {
		o(req)
	}
	rec := httptest.NewRecorder()
	h.Handler().ServeHTTP(rec, req)
	return rec
}

// attached wires one handle with one attachment and, optionally, sandboxes
// holding it. It is the shape almost every page test needs and writing it out
// per test buried the one line each test actually varies.
func attached(handle string, att repos.Repo, boxes map[string][]repos.Repo) *fakeRepos {
	return &fakeRepos{
		attachments: map[string][]repos.Repo{handle: {att}},
		boxes:       map[string]map[string][]repos.Repo{handle: boxes},
	}
}

// box is a terse SandboxInfo for the ranking tests.
func box(name, state string, unreachable bool, lastActive time.Time) ctlops.SandboxInfo {
	return ctlops.SandboxInfo{
		Name: name, Owner: "vanpelt", State: state, Unreachable: unreachable,
		LastActive: lastActive, TerminalURL: "https://" + name + "-xterm.example.test/",
	}
}

// AwaitEnv records the barrier the launch door puts in front of its redirect.
// It appends under the same mutex the create path uses so the ordering a test
// reads back is the ordering the handler produced.
func (f *fakeOps) AwaitEnv(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.awaited = append(f.awaited, name)
	return f.awaitErr
}

// EnvironmentsForTags is the control plane's answer to "which of these tags are
// environments". It filters by the tags asked about, the way the real one does,
// so a test can hand the fake a whole world and still see the narrowing.
//
// It does NOT sort. The real method returns newest-first, and pickTags is
// written not to depend on that; answering in the order a test declared them is
// what keeps that promise honest.
func (f *fakeOps) EnvironmentsForTags(_ ctlops.Caller, tags []string) ([]ctlops.EnvironmentInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.envsErr != nil {
		return nil, f.envsErr
	}
	var out []ctlops.EnvironmentInfo
	for _, e := range f.envs {
		if slices.Contains(tags, e.Name) {
			out = append(out, e)
		}
	}
	return out, nil
}

// awaitedNames is the recorded barrier, copied under the lock.
func (f *fakeOps) awaitedNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.awaited...)
}
