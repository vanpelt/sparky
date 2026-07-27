package users

// The device flow, against a fake github.com.
//
// Every case here is one of GitHub's documented answers, because the whole
// protocol is a loop over answers we do not control: the interesting behaviour
// is not "does it work" but what it does with pending, with slow_down, with a
// person who declines, and with a code that expired while nobody was looking.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeGitHubOAuth is github.com's half of the flow: it hands out one code pair
// and then answers the poll from a script, one entry per request.
type fakeGitHubOAuth struct {
	mu sync.Mutex
	// polls is the sequence of token-endpoint bodies to answer with. The last
	// entry repeats, so a test that only cares about the first answer need not
	// count the retries.
	polls []string
	seen  int
	// user is the /user body, and userAuth records the credential it was asked
	// with so a test can prove the token is actually spent.
	user     string
	userAuth string
	// startBody overrides the code response.
	startBody string
}

func (f *fakeGitHubOAuth) server(t *testing.T) *GitHubDevice {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /login/device/code", func(w http.ResponseWriter, r *http.Request) {
		body := f.startBody
		if body == "" {
			body = `{"device_code":"secret-dc","user_code":"WDJB-MJHT",
			         "verification_uri":"https://github.com/login/device",
			         "expires_in":900,"interval":0}`
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, body)
	})
	mux.HandleFunc("POST /login/oauth/access_token", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		i := f.seen
		f.seen++
		if i >= len(f.polls) {
			i = len(f.polls) - 1
		}
		body := f.polls[i]
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, body)
	})
	mux.HandleFunc("GET /user", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.userAuth = r.Header.Get("Authorization")
		body := f.user
		f.mu.Unlock()
		if body == "" {
			body = `{"login":"octocat","id":583231,"email":"octo@github.com"}`
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, body)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	d := NewGitHubDevice("Iv-test")
	d.codeURL = srv.URL + "/login/device/code"
	d.tokenURL = srv.URL + "/login/oauth/access_token"
	d.userURL = srv.URL + "/user"
	// The real floor is a second per poll, which would make every case below
	// take a second it has nothing to do with.
	d.pollFloor = time.Millisecond
	return d
}

func (f *fakeGitHubOAuth) requests() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.seen
}

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// The item in one test: a person authorizes, and we learn who they are from
// GitHub rather than from anything the caller said.
func TestDeviceFlowIdentifiesWhoAuthorized(t *testing.T) {
	gh := &fakeGitHubOAuth{polls: []string{
		`{"error":"authorization_pending"}`,
		`{"error":"authorization_pending"}`,
		`{"access_token":"gho_secret"}`,
	}}
	d := gh.server(t)
	ctx := testContext(t)

	dc, err := d.Start(ctx)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if dc.UserCode != "WDJB-MJHT" {
		t.Errorf("user code = %q, want the one GitHub sent", dc.UserCode)
	}
	if dc.Code != "secret-dc" {
		t.Errorf("device code = %q, want the one GitHub sent", dc.Code)
	}
	// An interval of zero is what the fake sends, and polling on it would be a
	// spin against github.com rather than a flow.
	if dc.Interval <= 0 {
		t.Errorf("interval = %v, want a floor rather than GitHub's zero", dc.Interval)
	}

	p, err := d.Wait(ctx, dc)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if p.Login != "octocat" || p.ID != 583231 {
		t.Errorf("profile = %+v, want octocat/583231", p)
	}
	if p.Email != "octo@github.com" {
		t.Errorf("email = %q, want the profile's", p.Email)
	}
	if gh.requests() != 3 {
		t.Errorf("polled %d times, want 3 — pending must be a retry, not an answer", gh.requests())
	}
	// The token is spent once, on the one question this flow exists to ask, and
	// nothing keeps it.
	if gh.userAuth != "Bearer gho_secret" {
		t.Errorf("identified with %q, want the token GitHub minted", gh.userAuth)
	}
}

// slow_down is not advice. Keeping the old cadence earns the same answer
// indefinitely, so the interval has to actually grow.
func TestDeviceFlowBacksOffWhenToldTo(t *testing.T) {
	gh := &fakeGitHubOAuth{polls: []string{
		`{"error":"slow_down"}`,
		`{"access_token":"gho_secret"}`,
	}}
	d := gh.server(t)
	// Scaled down from GitHub's real five seconds: what is under test is that
	// the interval GROWS by the back-off, not the number of seconds in it, and
	// a test that proved the number would spend it.
	d.pollFloor = time.Millisecond
	d.slowDown = 200 * time.Millisecond
	ctx := testContext(t)

	dc, err := d.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	dc.Interval = time.Millisecond
	start := time.Now()
	if _, err := d.Wait(ctx, dc); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if elapsed := time.Since(start); elapsed < d.slowDown {
		t.Errorf("second poll came after %v, want the back-off (%v) honoured", elapsed, d.slowDown)
	}
}

// The three answers a caller renders differently. Everything else is an
// upstream fault and keeps GitHub's own wording.
func TestDeviceFlowDecisionsAreTyped(t *testing.T) {
	cases := []struct {
		name string
		body string
		want error
	}{
		{"declined", `{"error":"access_denied"}`, ErrDeviceDenied},
		{"expired", `{"error":"expired_token"}`, ErrDeviceExpired},
		{"app misconfigured", `{"error":"device_flow_disabled"}`, ErrDeviceUnsupported},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gh := &fakeGitHubOAuth{polls: []string{tc.body}}
			d := gh.server(t)
			ctx := testContext(t)
			dc, err := d.Start(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := d.Wait(ctx, dc); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

// A code that ran out while nobody was watching stops the loop on this side
// rather than polling GitHub forever for an answer that cannot change.
func TestDeviceFlowStopsAtItsOwnExpiry(t *testing.T) {
	gh := &fakeGitHubOAuth{polls: []string{`{"error":"authorization_pending"}`}}
	d := gh.server(t)
	ctx := testContext(t)

	dc, err := d.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	dc.ExpiresAt = time.Now().Add(-time.Second)
	if _, err := d.Wait(ctx, dc); !errors.Is(err, ErrDeviceExpired) {
		t.Fatalf("err = %v, want %v", err, ErrDeviceExpired)
	}
	if gh.requests() != 0 {
		t.Errorf("polled %d times past the expiry, want none", gh.requests())
	}
}

// A caller giving up is its own context's error and never a decision GitHub
// made — a timed-out dialog must not tell somebody they declined.
func TestDeviceFlowReportsTheCallersOwnCancellation(t *testing.T) {
	gh := &fakeGitHubOAuth{polls: []string{`{"error":"authorization_pending"}`}}
	d := gh.server(t)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	dc, err := d.Start(testContext(t))
	if err != nil {
		t.Fatal(err)
	}
	_, err = d.Wait(ctx, dc)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the caller's own deadline", err)
	}
	if errors.Is(err, ErrDeviceDenied) {
		t.Error("a caller's timeout was reported as the person declining")
	}
}

// GitHub naming an account this platform cannot record is refused rather than
// linked. "GitHub would never" is not a property this process can check, and the
// login becomes a URL we fetch and a string every console prints.
func TestDeviceFlowRefusesAnImpossibleLogin(t *testing.T) {
	gh := &fakeGitHubOAuth{
		polls: []string{`{"access_token":"gho_secret"}`},
		user:  `{"login":"not a login","id":1}`,
	}
	d := gh.server(t)
	ctx := testContext(t)
	dc, err := d.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Wait(ctx, dc); err == nil {
		t.Fatal("a login this platform cannot record was accepted")
	} else if !strings.Contains(err.Error(), "cannot record") {
		t.Errorf("err = %v, want it to name the refusal", err)
	}
}

// The device code is the credential half of the flow: anyone holding it can
// collect the token. It must reach the token endpoint and nothing else.
func TestDeviceCodeGoesOnlyToGitHub(t *testing.T) {
	gh := &fakeGitHubOAuth{polls: []string{`{"access_token":"gho_secret"}`}}
	d := gh.server(t)
	ctx := testContext(t)
	dc, err := d.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Whatever a caller logs or prints comes from this struct, so the check is
	// that the secret is not hiding in a field a caller would reasonably show.
	shown, err := json.Marshal(struct {
		UserCode, VerificationURI string
	}{dc.UserCode, dc.VerificationURI})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(shown), dc.Code) {
		t.Errorf("the device code is inside what a caller displays: %s", shown)
	}
}

// A started flow reserves nothing on this side, which is why there is no cancel
// verb and no cleanup for an abandoned dialog to forget.
func TestStartingAFlowWritesNothing(t *testing.T) {
	gh := &fakeGitHubOAuth{polls: []string{`{"error":"authorization_pending"}`}}
	d := gh.server(t)
	if _, err := d.Start(testContext(t)); err != nil {
		t.Fatal(err)
	}
	if gh.requests() != 0 {
		t.Errorf("starting a flow polled %d times; it should only mint a code", gh.requests())
	}
}
