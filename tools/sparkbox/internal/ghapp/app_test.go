package ghapp

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fixedNow anchors the clock so JWT claims are assertable to the second and no
// cache can expire behind a test's back.
var fixedNow = time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)

// clock is the movable version, for the two tests that need a cache to age.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock { return &clock{t: fixedNow} }

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// call is one request the stub saw. It keeps the headers because every request
// this package makes is required to carry the two github ones, and the
// Authorization value is how a test tells an app assertion from an installation
// token.
type call struct {
	method  string
	path    string
	auth    string
	accept  string
	version string
	body    string
}

// stub stands in for api.github.com. Handlers are registered per pattern; every
// request is recorded first.
type stub struct {
	t   *testing.T
	mux *http.ServeMux
	srv *httptest.Server

	mu    sync.Mutex
	calls []call
}

func newStub(t *testing.T) *stub {
	t.Helper()
	s := &stub{t: t, mux: http.NewServeMux()}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		s.mu.Lock()
		s.calls = append(s.calls, call{
			method: r.Method, path: r.URL.Path, auth: r.Header.Get("Authorization"),
			accept: r.Header.Get("Accept"), version: r.Header.Get("X-GitHub-Api-Version"),
			body: string(body),
		})
		s.mu.Unlock()
		r.Body = io.NopCloser(bytes.NewReader(body))
		s.mux.ServeHTTP(w, r)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *stub) handle(pattern string, h http.HandlerFunc) { s.mux.HandleFunc(pattern, h) }

// json registers a handler that answers one status and one body, forever.
func (s *stub) json(pattern string, status int, body string) {
	s.handle(pattern, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	})
}

func (s *stub) seen() []call {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]call(nil), s.calls...)
}

// count returns how many recorded requests hit a path containing frag.
func (s *stub) count(frag string) int {
	n := 0
	for _, c := range s.seen() {
		if strings.Contains(c.path, frag) {
			n++
		}
	}
	return n
}

func (s *stub) last(frag string) call {
	s.t.Helper()
	for i := len(s.seen()) - 1; i >= 0; i-- {
		if c := s.seen()[i]; strings.Contains(c.path, frag) {
			return c
		}
	}
	s.t.Fatalf("no request to %q was made", frag)
	return call{}
}

func newApp(t *testing.T, s *stub, now func() time.Time) *App {
	t.Helper()
	if now == nil {
		now = func() time.Time { return fixedNow }
	}
	app, err := New(Config{
		ClientID: "Iv23liTESTCLIENT",
		Key:      testKey(),
		BaseURL:  s.srv.URL,
		Logger:   slog.New(slog.DiscardHandler),
		Now:      now,
	})
	if err != nil {
		t.Fatal(err)
	}
	return app
}

// The canned answers. The token expires an hour after fixedNow, which is what
// github does and what the five-minute refresh lead is measured against.
const (
	userInstallation = `{"id":42,"app_slug":"sparky-repos","account":{"id":7,"login":"vanpelt","type":"User"}}`
	orgInstallation  = `{"id":99,"app_slug":"sparky-repos","account":{"id":800,"login":"wandb","type":"Organization"}}`
	tokenBody        = `{"token":"ghs_supersecret","expires_at":"2026-08-24T13:00:00Z"}`
)

var tokenExpiry = time.Date(2026, 8, 24, 13, 0, 0, 0, time.UTC)

// verifyAssertion checks an Authorization header the way github does: split the
// compact JWS, re-hash the signing input, and verify the RS256 signature
// against the app's public key. It deliberately does not reuse signJWT.
func verifyAssertion(t *testing.T, auth string, pub *rsa.PublicKey) map[string]any {
	t.Helper()
	token, ok := strings.CutPrefix(auth, "Bearer ")
	if !ok {
		t.Fatalf("Authorization %q is not a bearer credential", auth)
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("want 3 JWS parts, got %d", len(parts))
	}
	var header map[string]string
	decodeJSON(t, parts[0], &header)
	if header["alg"] != "RS256" || header["typ"] != "JWT" {
		t.Errorf("header = %v, want RS256/JWT", header)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("signature is not base64url: %v", err)
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, digest[:], sig); err != nil {
		t.Fatalf("signature does not verify: %v", err)
	}
	var claims map[string]any
	decodeJSON(t, parts[1], &claims)
	return claims
}

func decodeJSON(t *testing.T, seg string, into any) {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(seg)
	if err != nil {
		t.Fatalf("segment is not base64url: %v", err)
	}
	if err := json.Unmarshal(raw, into); err != nil {
		t.Fatalf("segment is not json: %v", err)
	}
}

func TestNewRefusesAnIncompleteConfig(t *testing.T) {
	if _, err := New(Config{ClientID: "Iv23li"}); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("no key: err = %v, want ErrNotConfigured", err)
	}
	if _, err := New(Config{Key: testKey(), ClientID: "  "}); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("no client id: err = %v, want ErrNotConfigured", err)
	}
}

// The assertion is the credential the whole package rests on, so it is verified
// from scratch, and the three claims github checks are asserted exactly: iss is
// the client id, exp is inside github's ten-minute cap, and iat is BEHIND now —
// a forward iat is rejected with an error that names only the claim.
func TestAppAssertionIsAVerifiableRS256JWT(t *testing.T) {
	s := newStub(t)
	s.json("GET /repos/wandb/hivemind/installation", 200, userInstallation)
	app := newApp(t, s, nil)

	if _, err := app.InstallationFor(context.Background(), "wandb", "hivemind"); err != nil {
		t.Fatal(err)
	}
	claims := verifyAssertion(t, s.last("/installation").auth, &testKey().PublicKey)

	if claims["iss"] != "Iv23liTESTCLIENT" {
		t.Errorf("iss = %v, want the client id", claims["iss"])
	}
	iat := int64(claims["iat"].(float64))
	exp := int64(claims["exp"].(float64))
	if want := fixedNow.Add(-jwtBackdate).Unix(); iat != want {
		t.Errorf("iat = %d, want %d (now backdated by a minute)", iat, want)
	}
	if iat >= fixedNow.Unix() {
		t.Errorf("iat %d is not behind now %d; github rejects a future iat", iat, fixedNow.Unix())
	}
	if want := fixedNow.Add(jwtTTL).Unix(); exp != want {
		t.Errorf("exp = %d, want %d", exp, want)
	}
	if exp > fixedNow.Add(10*time.Minute).Unix() {
		t.Errorf("exp is more than 10 minutes out; github refuses that")
	}
}

// One assertion is reused across calls, but never past the window it is good
// for. Eight minutes of a ten-minute token leaves slack for clock skew.
func TestAppAssertionIsReusedButNotForever(t *testing.T) {
	s := newStub(t)
	s.json("GET /repos/wandb/a/installation", 200, userInstallation)
	s.json("GET /repos/wandb/b/installation", 200, userInstallation)
	s.json("GET /repos/wandb/c/installation", 200, userInstallation)
	c := newClock()
	app := newApp(t, s, c.now)

	if _, err := app.InstallationFor(context.Background(), "wandb", "a"); err != nil {
		t.Fatal(err)
	}
	first := s.last("/installation").auth

	c.advance(jwtReuse - time.Second)
	if _, err := app.InstallationFor(context.Background(), "wandb", "b"); err != nil {
		t.Fatal(err)
	}
	if got := s.last("/installation").auth; got != first {
		t.Error("a fresh assertion was minted while the held one was still good")
	}

	// A third repository, not a repeat of the first: that lookup is cached by
	// now and would make no request at all to carry an assertion.
	c.advance(2 * time.Second)
	if _, err := app.InstallationFor(context.Background(), "wandb", "c"); err != nil {
		t.Fatal(err)
	}
	if got := s.last("/installation").auth; got == first {
		t.Error("the same assertion was reused past its reuse window")
	}
}

func TestEveryRequestCarriesTheGitHubHeaders(t *testing.T) {
	s := newStub(t)
	s.json("GET /repos/wandb/hivemind/installation", 200, userInstallation)
	s.json("POST /app/installations/42/access_tokens", 200, tokenBody)
	app := newApp(t, s, nil)

	inst, err := app.InstallationFor(context.Background(), "wandb", "hivemind")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.MintToken(context.Background(), inst, []string{"hivemind"}, map[string]string{"contents": "read"}); err != nil {
		t.Fatal(err)
	}
	seen := s.seen()
	if len(seen) != 2 {
		t.Fatalf("got %d requests, want 2", len(seen))
	}
	for _, c := range seen {
		if c.accept != "application/vnd.github+json" {
			t.Errorf("%s %s: Accept = %q", c.method, c.path, c.accept)
		}
		if c.version != "2022-11-28" {
			t.Errorf("%s %s: X-GitHub-Api-Version = %q", c.method, c.path, c.version)
		}
	}
}

func TestInstallationForResolvesAndCaches(t *testing.T) {
	s := newStub(t)
	s.json("GET /repos/wandb/hivemind/installation", 200, orgInstallation)
	app := newApp(t, s, nil)

	want := Installation{ID: 99, AccountID: 800, AccountLogin: "wandb", AccountType: "Organization"}
	for i := 0; i < 3; i++ {
		got, err := app.InstallationFor(context.Background(), "wandb", "hivemind")
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("call %d: got %+v, want %+v", i, got, want)
		}
	}
	if n := s.count("/installation"); n != 1 {
		t.Errorf("made %d lookups, want 1 — the rest must come from the cache", n)
	}
}

// The signature failure mode of the feature. It must name the repository and
// the URL that fixes it, and it must not be remembered: somebody who installs
// the app and retries immediately has to be believed.
func TestInstallationForNotInstalled(t *testing.T) {
	s := newStub(t)
	s.json("GET /repos/wandb/hivemind/installation", 404, `{"message":"Not Found"}`)
	s.json("GET /app", 200, `{"slug":"sparky-repos"}`)
	app := newApp(t, s, nil)

	_, err := app.InstallationFor(context.Background(), "wandb", "hivemind")
	if !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("err = %v, want ErrNotInstalled", err)
	}
	if !strings.Contains(err.Error(), "wandb/hivemind") {
		t.Errorf("error %q does not name the repository", err)
	}
	if !strings.Contains(err.Error(), "https://github.com/apps/sparky-repos/installations/new") {
		t.Errorf("error %q does not name the install url", err)
	}

	if _, err := app.InstallationFor(context.Background(), "wandb", "hivemind"); !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("second call: err = %v", err)
	}
	if n := s.count("/installation"); n != 2 {
		t.Errorf("made %d lookups, want 2 — a failure must not be cached", n)
	}
	if n := s.count("/app"); n > 1 {
		t.Errorf("asked for the app record %d times; it is rate limited to once", n)
	}
}

// Until github names the app's slug there is still a working install URL to
// print, built from the client id alone.
func TestInstallURLFallsBackToTheClientID(t *testing.T) {
	s := newStub(t)
	app := newApp(t, s, nil)
	if got, want := app.InstallURL(), "https://github.com/login/oauth/authorize?client_id=Iv23liTESTCLIENT"; got != want {
		t.Errorf("InstallURL() = %q, want %q", got, want)
	}
	s.json("GET /repos/wandb/hivemind/installation", 200, userInstallation)
	if _, err := app.InstallationFor(context.Background(), "wandb", "hivemind"); err != nil {
		t.Fatal(err)
	}
	if got, want := app.InstallURL(), "https://github.com/apps/sparky-repos/installations/new"; got != want {
		t.Errorf("after a lookup, InstallURL() = %q, want %q", got, want)
	}
}

func TestInstallationForMapsRefusals(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		body    string
		want    error
		mention string
	}{
		{"unauthorized", 401, `{"message":"A JSON web token could not be decoded"}`, ErrForbidden, "clock"},
		{"forbidden", 403, `{"message":"Resource not accessible by integration"}`, ErrForbidden, "not accessible"},
		{"rate limited", 429, `{"message":"API rate limit exceeded"}`, ErrUpstream, "rate limit"},
		{"server error", 500, `{"message":"Server Error"}`, ErrUpstream, ""},
		{"bad gateway", 502, ``, ErrUpstream, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newStub(t)
			s.json("GET /repos/wandb/hivemind/installation", tc.status, tc.body)
			app := newApp(t, s, nil)

			_, err := app.InstallationFor(context.Background(), "wandb", "hivemind")
			if !errors.Is(err, tc.want) {
				t.Fatalf("status %d: err = %v, want %v", tc.status, err, tc.want)
			}
			if tc.mention != "" && !strings.Contains(err.Error(), tc.mention) {
				t.Errorf("error %q does not mention %q", err, tc.mention)
			}
		})
	}
}

// A transport that never answers is upstream being down, which is the one class
// a caller is allowed to retry.
func TestInstallationForMapsTransportFailure(t *testing.T) {
	s := newStub(t)
	app := newApp(t, s, nil)
	s.srv.Close() // nothing is listening now

	if _, err := app.InstallationFor(context.Background(), "wandb", "hivemind"); !errors.Is(err, ErrUpstream) {
		t.Fatalf("err = %v, want ErrUpstream", err)
	}
}

func TestInstallationForRefusesAnUnusableSlug(t *testing.T) {
	s := newStub(t)
	app := newApp(t, s, nil)
	for _, tc := range [][2]string{
		{"", "hivemind"},
		{"wandb/x", "hivemind"},
		{"-wandb", "hivemind"},
		{"wandb", ""},
		{"wandb", ".."},
		{"wandb", "hive/mind"},
	} {
		if _, err := app.InstallationFor(context.Background(), tc[0], tc[1]); err == nil {
			t.Errorf("%q/%q was accepted", tc[0], tc[1])
		}
	}
	if n := len(s.seen()); n != 0 {
		t.Errorf("made %d requests for slugs that never should have left this host", n)
	}
}

func TestMintTokenAsksForRepositoriesByName(t *testing.T) {
	s := newStub(t)
	s.json("POST /app/installations/42/access_tokens", 200, tokenBody)
	app := newApp(t, s, nil)

	inst := Installation{ID: 42, AccountID: 7, AccountLogin: "vanpelt", AccountType: "User"}
	tok, err := app.MintToken(context.Background(), inst, []string{"hivemind", "catnip"}, map[string]string{"contents": "write"})
	if err != nil {
		t.Fatal(err)
	}
	if tok.Token != "ghs_supersecret" || !tok.ExpiresAt.Equal(tokenExpiry) {
		t.Fatalf("token = %+v", tok)
	}

	c := s.last("access_tokens")
	var body struct {
		Repositories []string          `json:"repositories"`
		Permissions  map[string]string `json:"permissions"`
	}
	if err := json.Unmarshal([]byte(c.body), &body); err != nil {
		t.Fatal(err)
	}
	// Bare names, not slugs: that is what the endpoint takes, and it is what
	// saves a second lookup to turn slugs into numeric ids.
	if len(body.Repositories) != 2 || body.Repositories[0] != "catnip" || body.Repositories[1] != "hivemind" {
		t.Errorf("repositories = %v, want the two bare names, sorted", body.Repositories)
	}
	if body.Permissions["contents"] != "write" || len(body.Permissions) != 1 {
		t.Errorf("permissions = %v", body.Permissions)
	}
	// Minting is an app-level call: it must present the assertion, never a
	// token minted from it.
	verifyAssertion(t, c.auth, &testKey().PublicKey)
}

// The scope is not optional, and this is the reason: github reads an absent
// repository or permission list as "all of them", so a caller that forgot its
// scope would silently get the widest token the installation can produce.
func TestMintTokenRefusesAnUnscopedRequest(t *testing.T) {
	s := newStub(t)
	app := newApp(t, s, nil)
	inst := Installation{ID: 42, AccountType: "User"}
	perms := map[string]string{"contents": "read"}

	if _, err := app.MintToken(context.Background(), inst, nil, perms); err == nil {
		t.Error("a token with no repository list was minted")
	}
	if _, err := app.MintToken(context.Background(), inst, []string{"hivemind"}, nil); err == nil {
		t.Error("a token with no permission list was minted")
	}
	if _, err := app.MintToken(context.Background(), inst, []string{"hive mind"}, perms); err == nil {
		t.Error("a bogus repository name was accepted")
	}
	if n := len(s.seen()); n != 0 {
		t.Errorf("made %d requests, want 0", n)
	}
}

// The cache is what keeps a `git fetch` loop from minting per fetch, so it has
// to survive the two things a caller varies for free: repeating itself, and
// listing the same repositories in a different order.
func TestMintTokenCaches(t *testing.T) {
	s := newStub(t)
	s.json("POST /app/installations/42/access_tokens", 200, tokenBody)
	app := newApp(t, s, nil)
	inst := Installation{ID: 42, AccountType: "User"}
	read := map[string]string{"contents": "read"}

	mint := func(names []string, perms map[string]string) {
		t.Helper()
		if _, err := app.MintToken(context.Background(), inst, names, perms); err != nil {
			t.Fatal(err)
		}
	}
	mint([]string{"hivemind", "catnip"}, read)
	mint([]string{"catnip", "hivemind"}, read)
	mint([]string{"hivemind", "catnip", "hivemind"}, read)
	if n := s.count("access_tokens"); n != 1 {
		t.Errorf("made %d mints for one scope, want 1", n)
	}

	mint([]string{"hivemind"}, read)                                             // narrower scope
	mint([]string{"hivemind", "catnip"}, map[string]string{"contents": "write"}) // different permission
	if n := s.count("access_tokens"); n != 3 {
		t.Errorf("made %d mints, want 3 — a different scope is a different token", n)
	}
}

// A cached token is retired five minutes before github kills it, so a token
// handed to a guest is always good long enough to finish the clone.
func TestMintTokenRefreshesBeforeExpiry(t *testing.T) {
	s := newStub(t)
	s.json("POST /app/installations/42/access_tokens", 200, tokenBody)
	c := newClock()
	app := newApp(t, s, c.now)
	inst := Installation{ID: 42, AccountType: "User"}

	mint := func() {
		t.Helper()
		if _, err := app.MintToken(context.Background(), inst, []string{"hivemind"}, map[string]string{"contents": "read"}); err != nil {
			t.Fatal(err)
		}
	}
	mint()
	c.advance(time.Hour - tokenRefreshLead - time.Second)
	mint()
	if n := s.count("access_tokens"); n != 1 {
		t.Fatalf("made %d mints while the token was still fresh, want 1", n)
	}
	c.advance(2 * time.Second)
	mint()
	if n := s.count("access_tokens"); n != 2 {
		t.Errorf("made %d mints, want 2 — the cached token was inside its refresh lead", n)
	}
}

// Five sandboxes coming back from a pause at once, or one boot cloning five
// repositories in parallel, must be one mint and not five.
func TestMintTokenSingleFlights(t *testing.T) {
	s := newStub(t)
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	s.handle("POST /app/installations/42/access_tokens", func(w http.ResponseWriter, _ *http.Request) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, tokenBody)
	})
	app := newApp(t, s, nil)
	inst := Installation{ID: 42, AccountType: "User"}

	var wg sync.WaitGroup
	got := make([]Token, 16)
	start := func(i int) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tok, err := app.MintToken(context.Background(), inst, []string{"hivemind"}, map[string]string{"contents": "read"})
			if err != nil {
				t.Error(err)
				return
			}
			got[i] = tok
		}()
	}
	start(0)
	<-entered // the leader is inside the handler; the entry is in flight
	for i := 1; i < len(got); i++ {
		start(i)
	}
	// Long enough for the followers to reach the wait; the assertion below
	// holds either way, since a follower arriving late finds a cached token.
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()

	if n := s.count("access_tokens"); n != 1 {
		t.Errorf("made %d mints for 16 concurrent callers, want 1", n)
	}
	for i, tok := range got {
		if tok.Token != "ghs_supersecret" {
			t.Fatalf("caller %d got %q", i, tok.Token)
		}
	}
}

// 404 and 422 both mean the installation does not cover what was asked for, and
// both are fixed at the install URL, so both carry ErrNotInstalled.
func TestMintTokenMapsRefusals(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		body    string
		want    error
		mention string
	}{
		{"gone", 404, `{"message":"Not Found"}`, ErrNotInstalled, "no longer exists"},
		{"repo not covered", 422, `{"message":"There is at least one repository that does not exist"}`, ErrNotInstalled, "hivemind"},
		{"forbidden", 403, `{"message":"Resource not accessible by integration"}`, ErrForbidden, ""},
		{"upstream", 503, ``, ErrUpstream, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newStub(t)
			s.json("POST /app/installations/42/access_tokens", tc.status, tc.body)
			app := newApp(t, s, nil)

			_, err := app.MintToken(context.Background(), Installation{ID: 42}, []string{"hivemind"}, map[string]string{"contents": "read"})
			if !errors.Is(err, tc.want) {
				t.Fatalf("status %d: err = %v, want %v", tc.status, err, tc.want)
			}
			if tc.mention != "" && !strings.Contains(err.Error(), tc.mention) {
				t.Errorf("error %q does not mention %q", err, tc.mention)
			}
		})
	}
}

// An answer github truncates is an answer nobody can use as a credential.
func TestMintTokenRefusesAnUnusableAnswer(t *testing.T) {
	for name, body := range map[string]string{
		"no token":  `{"expires_at":"2026-08-24T13:00:00Z"}`,
		"no expiry": `{"token":"ghs_supersecret"}`,
	} {
		t.Run(name, func(t *testing.T) {
			s := newStub(t)
			s.json("POST /app/installations/42/access_tokens", 200, body)
			app := newApp(t, s, nil)
			_, err := app.MintToken(context.Background(), Installation{ID: 42}, []string{"hivemind"}, map[string]string{"contents": "read"})
			if !errors.Is(err, ErrUpstream) {
				t.Fatalf("err = %v, want ErrUpstream", err)
			}
		})
	}
}

// Nothing this package writes or returns may carry a token. The mint log line
// is the audit trail and has to stay safe to ship to a collector; an error
// raised after a successful mint must not quote what was minted.
func TestNothingLeaksTheToken(t *testing.T) {
	var logged bytes.Buffer
	s := newStub(t)
	s.json("POST /app/installations/99/access_tokens", 200, tokenBody)
	s.json("GET /orgs/wandb/memberships/vanpelt", 403, `{"message":"Resource not accessible by integration"}`)
	app, err := New(Config{
		ClientID: "Iv23liTESTCLIENT", Key: testKey(), BaseURL: s.srv.URL,
		Logger: slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Now:    func() time.Time { return fixedNow },
	})
	if err != nil {
		t.Fatal(err)
	}

	inst := Installation{ID: 99, AccountID: 800, AccountLogin: "wandb", AccountType: "Organization"}
	if _, err := app.MintToken(context.Background(), inst, []string{"hivemind"}, map[string]string{"contents": "read"}); err != nil {
		t.Fatal(err)
	}
	authErr := app.Authorize(context.Background(), inst, 7, "vanpelt")
	if authErr == nil {
		t.Fatal("the membership 403 was not an error")
	}
	if strings.Contains(authErr.Error(), "ghs_") {
		t.Errorf("an error carried the token: %q", authErr)
	}
	if strings.Contains(logged.String(), "ghs_") {
		t.Errorf("a log line carried the token: %q", logged.String())
	}
	if !strings.Contains(logged.String(), "minted a github installation token") {
		t.Errorf("the mint was not recorded at all: %q", logged.String())
	}
}
