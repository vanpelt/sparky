package launch

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/repos"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
)

const postLink = "https://go.example.test/wandb/hivemind?ref=feat/x"

// creatable wires the state the POST tests share: the caller has the
// repository attached and no sandbox holding it, so pressing the button builds
// one.
func creatable(t *testing.T, ops *fakeOps) *Handler {
	t.Helper()
	return newHandler(t, ops, &fakeRepos{attachments: map[string][]repos.Repo{
		testHandle: {attachment("main", "hm")},
	}})
}

// TestMutationRefusedWithoutProof: a create that carries no evidence it came
// from this page is refused, and nothing is built.
//
// The session cookie rides every same-site request and SameSite=Lax does not
// help — sandbox subdomains are same-site with this door — so the cookie alone
// is not proof of intent. A cross-site form POST from evil.example carries the
// INITIATING document's Origin, which matches neither clause; a request with no
// Origin at all is refused rather than waved through, because "omit the header"
// must never be the way past a check.
func TestMutationRefusedWithoutProof(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts []func(*http.Request)
	}{
		{"no Origin at all", nil},
		{"somebody else's page", []func(*http.Request){func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") }}},
		{"a lookalike host", []func(*http.Request){func(r *http.Request) { r.Header.Set("Origin", "https://go.example.test.evil.example") }}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// allowCreate stays false: reaching Create at all fails the test.
			ops := &fakeOps{t: t}
			h := creatable(t, ops)
			opts := append([]func(*http.Request){signedIn(t, testHandle)}, tc.opts...)

			rec := serveLaunch(t, h, http.MethodPost, postLink, opts...)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403", rec.Code)
			}
			if n := ops.createCount(); n != 0 {
				t.Fatalf("%d sandboxes built by a refused create, want 0", n)
			}
			// The refusal must not echo the attacker's Origin back, and must
			// not name which clause failed.
			if strings.Contains(rec.Body.String(), "evil.example") {
				t.Error("the refusal echoed the attacker's own Origin back to them")
			}
		})
	}
}

// TestFormPostWithOriginSucceeds is the ordinary path: the confirm page's form,
// on a properly configured host.
func TestFormPostWithOriginSucceeds(t *testing.T) {
	ops := &fakeOps{t: t, allowCreate: true}
	h := creatable(t, ops)

	rec := serveLaunch(t, h, http.MethodPost, postLink, signedIn(t, testHandle), fromThePage)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d (%s), want 303 into the new sandbox's terminal", rec.Code, rec.Body)
	}
	if want := "https://created-box-xterm.example.test/"; rec.Header().Get("Location") != want {
		t.Fatalf("Location = %q, want the created sandbox's own %q", rec.Header().Get("Location"), want)
	}
	if n := ops.createCount(); n != 1 {
		t.Fatalf("created %d sandboxes, want 1", n)
	}
}

// TestFormPostOnPlainHttpDevLoopSucceeds is the test edgeauth.RequireMutation
// would fail, and the entire reason this package has its own first-party check.
//
// On a --proxy-tls=false development run the browser is on
// http://go.localtest.me:8081 and sends that as its Origin. RequireMutation
// compares against a hardcoded `https://<sub>.<domain>` with no port, which can
// never match, and its documented remedy — send X-Sparkbox-Console: 1 — is
// impossible for a page with no JavaScript. Under it the create button on every
// dev host would be permanently 403, and the fix available to whoever hit it
// would be to add a script and weaken the CSP.
func TestFormPostOnPlainHttpDevLoopSucceeds(t *testing.T) {
	ops := &fakeOps{t: t, allowCreate: true}
	h := creatable(t, ops)

	rec := serveLaunch(t, h, http.MethodPost, "http://go.localtest.me:8081/wandb/hivemind?ref=feat/x",
		signedIn(t, testHandle),
		func(r *http.Request) { r.Header.Set("Origin", "http://go.localtest.me:8081") })
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d (%s), want 303 — a plain-http dev loop must be able to press the button", rec.Code, rec.Body)
	}
	if n := ops.createCount(); n != 1 {
		t.Fatalf("created %d sandboxes, want 1", n)
	}
}

// TestForwardedProtoIsHonoured: behind an edge that terminates TLS, r.TLS is
// nil and X-Forwarded-Proto is the only thing that says https. A check that
// assumed the connection's own scheme would refuse every create on every
// production deployment behind a proxy.
func TestForwardedProtoIsHonoured(t *testing.T) {
	ops := &fakeOps{t: t, allowCreate: true}
	h := creatable(t, ops)

	rec := serveLaunch(t, h, http.MethodPost, "http://go.example.test/wandb/hivemind?ref=feat/x",
		signedIn(t, testHandle),
		func(r *http.Request) {
			r.Header.Set("Origin", "https://go.example.test")
			r.Header.Set("X-Forwarded-Proto", "https, http")
		})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d (%s), want 303", rec.Code, rec.Body)
	}
}

// TestExpiredSessionPostIs303NotForbidden pins the ORDER of the two gates.
//
// Somebody leaves the confirm page open, their session expires, they press the
// button. Auth runs first, so they get the sign-in bounce — which returns them
// here as a GET, so they land back on the page and press it again. If the
// first-party check ran first they would get a 403 about cross-origin requests,
// which is both untrue and unactionable on a page with no script to obey a
// remedy.
func TestExpiredSessionPostIs303NotForbidden(t *testing.T) {
	ops := &fakeOps{t: t}
	h := creatable(t, ops)

	// A correct Origin and no session at all: the only thing that can decide
	// this response is which gate runs first.
	rec := serveLaunch(t, h, http.MethodPost, postLink, asBrowser, fromThePage)
	if rec.Code == http.StatusForbidden {
		t.Fatal("an expired session was answered 403 by the CSRF gate; auth has to run first")
	}
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303 to the login page", rec.Code)
	}
	if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, "https://login.example.test/?return=") {
		t.Fatalf("Location = %q, want the login bounce", loc)
	}
	if n := ops.createCount(); n != 0 {
		t.Fatalf("%d sandboxes built without a session, want 0", n)
	}
}

// TestDoubleClickCreatesOne is the idempotency, under -race.
//
// A create takes up to fifteen seconds and a zero-JavaScript page has no
// spinner, so pressing the button twice is not a pathological case, it is what
// people do. Without singleflight each press is a whole extra sandbox: a VM, a
// disk, a slot against --max-running-per-owner and a secret push.
//
// The choreography: the fake's Create parks inside the singleflight function
// and tells the test it is there, so the second request provably arrives while
// the first is still in flight. The short wait afterwards is the window for
// that second goroutine to reach the group and become a follower — microseconds
// of work against fifty milliseconds of budget. If the collapse fails, the
// follower builds a second sandbox and the count assertion catches it.
func TestDoubleClickCreatesOne(t *testing.T) {
	entered, held := make(chan struct{}), make(chan struct{})
	ops := &fakeOps{t: t, allowCreate: true, entered: entered, held: held}
	h := creatable(t, ops)
	cookie := signedIn(t, testHandle)

	var wg sync.WaitGroup
	recs := make([]*httptest.ResponseRecorder, 2)
	press := func(i int) {
		defer wg.Done()
		recs[i] = serveLaunch(t, h, http.MethodPost, postLink, cookie, fromThePage)
	}

	wg.Add(1)
	go press(0)
	<-entered // the first create is inside the group and parked

	wg.Add(1)
	go press(1)
	time.Sleep(50 * time.Millisecond)
	close(held)
	wg.Wait()

	if n := ops.createCount(); n != 1 {
		t.Fatalf("two presses of one button built %d sandboxes, want 1", n)
	}
	for i, rec := range recs {
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("press %d: status = %d (%s), want 303", i, rec.Code, rec.Body)
		}
	}
	if a, b := recs[0].Header().Get("Location"), recs[1].Header().Get("Location"); a != b {
		t.Fatalf("the two presses landed in different places: %q and %q", a, b)
	}
}

// Two links that name the SAME effective branch in different words collapse
// into one create, which is the whole reason the singleflight key is built from
// the normalized ref rather than the raw query value.
//
// The attachment's default branch is "main". A README carries the no-ref badge
// and a comment carries a hand-written ?ref=main one; the reuse rule already
// treats those as one target. Keyed on the raw value they would be two keys,
// two leaders, and — because each re-resolves before the other commits — two
// sandboxes on one branch, which on a default-configured host is both slots of
// --max-running-per-owner spent by one person clicking two links.
func TestTwoSpellingsOfOneBranchCreateOne(t *testing.T) {
	entered, held := make(chan struct{}), make(chan struct{})
	ops := &fakeOps{t: t, allowCreate: true, entered: entered, held: held}
	h := creatable(t, ops) // attachment("main", "hm")
	cookie := signedIn(t, testHandle)

	var wg sync.WaitGroup
	recs := make([]*httptest.ResponseRecorder, 2)
	press := func(i int, link string) {
		defer wg.Done()
		recs[i] = serveLaunch(t, h, http.MethodPost, link, cookie, fromThePage)
	}

	wg.Add(1)
	go press(0, "https://go.example.test/"+testSlug) // no ?ref= at all
	<-entered

	wg.Add(1)
	go press(1, "https://go.example.test/"+testSlug+"?ref=main") // the attachment's own default
	time.Sleep(50 * time.Millisecond)
	close(held)
	wg.Wait()

	if n := ops.createCount(); n != 1 {
		t.Fatalf("two spellings of one branch built %d sandboxes, want 1", n)
	}
	if a, b := recs[0].Header().Get("Location"), recs[1].Header().Get("Location"); a != b {
		t.Fatalf("the two links landed in different places: %q and %q", a, b)
	}
}

// TestCreatePassesScopedRefAndAttachmentTags reads the arguments handed to
// ctlops.Create, because two of them are load-bearing in ways nothing else
// would catch.
//
// The ref must be SCOPED — {Slug, Ref} and not a bare {Ref}. An unscoped
// override applies to every repository the create's tags select, so the day
// somebody adds a second repository to `default`, resolveRepoRefs starts
// refusing it as ambiguous_ref and every launch link in every comment written
// so far begins failing.
//
// The tags must be the attachment's own, verbatim. NormalizeTags does not
// validate the charset, so a tag synthesised from a slug or a branch would sail
// past it and die inside stampTags as a 500 on a half-built sandbox.
func TestCreatePassesScopedRefAndAttachmentTags(t *testing.T) {
	ops := &fakeOps{t: t, allowCreate: true}
	h := creatable(t, ops)

	rec := serveLaunch(t, h, http.MethodPost, postLink, signedIn(t, testHandle), fromThePage)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d (%s), want 303", rec.Code, rec.Body)
	}
	if len(ops.created) != 1 {
		t.Fatalf("created %d sandboxes, want 1", len(ops.created))
	}
	args := ops.created[0]
	want := []ctlops.RepoRef{{Slug: testSlug, Ref: "feat/x"}}
	if len(args.Refs) != 1 || args.Refs[0] != want[0] {
		t.Errorf("Refs = %+v, want the scoped %+v", args.Refs, want)
	}
	if len(args.Tags) != 1 || args.Tags[0] != "hm" {
		t.Errorf("Tags = %v, want the attachment's own [hm] and nothing derived", args.Tags)
	}
	// The things a link is never allowed to choose.
	if args.Name != "" || args.Node != "" {
		t.Errorf("a link chose a name (%q) or a machine (%q)", args.Name, args.Node)
	}
}

// TestCreateOmitsARefThatIsTheAttachmentDefault: an override row that merely
// restates the attachment's own branch says nothing, and repos.SetSandboxRefs
// refuses an empty one. Folding it away here is the emitting half of the same
// rule that makes the reuse lookup work.
func TestCreateOmitsARefThatIsTheAttachmentDefault(t *testing.T) {
	ops := &fakeOps{t: t, allowCreate: true}
	h := creatable(t, ops)

	rec := serveLaunch(t, h, http.MethodPost, "https://go.example.test/wandb/hivemind?ref=main",
		signedIn(t, testHandle), fromThePage)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d (%s), want 303", rec.Code, rec.Body)
	}
	if len(ops.created) != 1 {
		t.Fatalf("created %d sandboxes, want 1", len(ops.created))
	}
	if refs := ops.created[0].Refs; refs != nil {
		t.Errorf("Refs = %+v, want nil — the link named the attachment's own branch", refs)
	}
}

// TestRefEqualToAttachmentDefaultReusesRatherThanCreates is the row a naive
// `eff == want` comparison gets wrong, driven through the POST.
//
// ReposForSandbox coalesces the per-sandbox override over the attachment's
// default, so a box with no override at all, on an attachment created with
// --ref main, reports its effective ref as "main". A link that spells out
// ?ref=main must therefore land in it — and a link that says nothing must land
// in it too, or every click builds another sandbox.
func TestRefEqualToAttachmentDefaultReusesRatherThanCreates(t *testing.T) {
	for _, link := range []string{
		"https://go.example.test/wandb/hivemind",
		"https://go.example.test/wandb/hivemind?ref=main",
	} {
		t.Run(link, func(t *testing.T) {
			// allowCreate stays false: a create on either of these is the bug.
			ops := &fakeOps{t: t, boxes: []ctlops.SandboxInfo{
				box("crafty-axolotl", string(vmm.StateRunning), false, time.Unix(1000, 0)),
			}}
			h := newHandler(t, ops, attached(testHandle, attachment("main", "hm"),
				map[string][]repos.Repo{"crafty-axolotl": {{Host: gitHubHost, Slug: testSlug, Ref: "main"}}}))

			rec := serveLaunch(t, h, http.MethodPost, link, signedIn(t, testHandle), fromThePage)
			if rec.Code != http.StatusSeeOther {
				t.Fatalf("status = %d (%s), want 303", rec.Code, rec.Body)
			}
			if want := "https://crafty-axolotl-xterm.example.test/"; rec.Header().Get("Location") != want {
				t.Fatalf("Location = %q, want the existing sandbox %q", rec.Header().Get("Location"), want)
			}
		})
	}
}

// TestCrossOwnerPostDoesNotReuse: another account's sandbox on the same
// repository is not a sandbox this caller may be redirected into, and a POST
// must build them their own instead.
//
// This is the same isolation the GET asserts, tested again on the write path
// because a redirect into somebody else's box would be an account takeover
// rather than an information leak.
func TestCrossOwnerPostDoesNotReuse(t *testing.T) {
	ops := &fakeOps{t: t, allowCreate: true, boxes: []ctlops.SandboxInfo{
		box("someone-elses-box", string(vmm.StateRunning), false, time.Unix(2000, 0)),
	}}
	h := newHandler(t, ops, &fakeRepos{
		attachments: map[string][]repos.Repo{
			testHandle:  {attachment("main", "hm")},
			otherHandle: {{Owner: otherHandle, Host: gitHubHost, Slug: testSlug, Ref: "main", Tags: []string{"hm"}}},
		},
		boxes: map[string]map[string][]repos.Repo{otherHandle: {
			"someone-elses-box": {{Host: gitHubHost, Slug: testSlug, Ref: "main"}},
		}},
	})

	rec := serveLaunch(t, h, http.MethodPost, "https://go.example.test/wandb/hivemind",
		signedIn(t, testHandle), fromThePage)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d (%s), want 303", rec.Code, rec.Body)
	}
	if loc := rec.Header().Get("Location"); strings.Contains(loc, "someone-elses-box") {
		t.Fatalf("Location = %q — this caller was redirected into another account's sandbox", loc)
	}
	if n := ops.createCount(); n != 1 {
		t.Fatalf("created %d sandboxes, want 1 of their own", n)
	}
}

// TestLimitScreenNamesThePauseCommand covers the failure a click-to-create
// button hits routinely.
//
// --max-running-per-owner defaults to 2, enforced by host.Manager.admitCost as
// a *host.LimitError, which ctlops classifies as KindLimit and answers 429
// with. The visitor must get a screen that names one of THEIR sandboxes and the
// command that frees a slot — not a bare "429 Too Many Requests", which reads
// as rate limiting and tells them nothing they can act on.
func TestLimitScreenNamesThePauseCommand(t *testing.T) {
	ops := &fakeOps{t: t, allowCreate: true, createErr: &host.LimitError{
		Max: 2, Running: []string{"crafty-axolotl", "brave-otter"},
	}}
	h := creatable(t, ops)

	rec := serveLaunch(t, h, http.MethodPost, postLink, signedIn(t, testHandle), fromThePage)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 (ctlops maps KindLimit there)", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		// The sentence ctlops built, naming the limit and the sandboxes.
		"you already have 2 running sandboxes (max 2)",
		// A command with a real name in it, not a placeholder: the visitor can
		// select the line and run it.
		"ssh ctl@example.test pause crafty-axolotl",
		// Pausing is not losing anything, which is the thing somebody about to
		// be told "pause one" needs to hear.
		"keeps its disk",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the limit screen does not say %q", want)
		}
	}
	// And the button is still there, because the remedy is elsewhere and the
	// next thing they do is come back and press it again.
	if !strings.Contains(body, `<form method="post"`) {
		t.Error("the limit screen dropped the retry button")
	}
	assertOneScript(t, body)
	assertPageHeaders(t, rec.Header())
}

// TestPostToAnUnattachedRepoTeachesRatherThanFails: the not-attached screen is
// reachable from the POST too — an attachment can be removed between the page
// rendering and the button being pressed — and it must be the same teaching
// screen, not a raw 400.
func TestPostToAnUnattachedRepoTeachesRatherThanFails(t *testing.T) {
	ops := &fakeOps{t: t}
	h := newHandler(t, ops, &fakeRepos{})

	rec := serveLaunch(t, h, http.MethodPost, postLink, signedIn(t, testHandle), fromThePage)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "repo add wandb/hivemind") {
		t.Error("the POST's refusal does not carry the attach command the GET's does")
	}
	if n := ops.createCount(); n != 0 {
		t.Fatalf("%d sandboxes built with nothing attached, want 0", n)
	}
}

// TestMutationHeaderIsAcceptedProof: the house's own CSRF proof still works
// here, so a script or a test that holds it is not forced to synthesise an
// Origin. The page itself never sends it — it has no JavaScript to send it
// with.
func TestMutationHeaderIsAcceptedProof(t *testing.T) {
	ops := &fakeOps{t: t, allowCreate: true}
	h := creatable(t, ops)

	rec := serveLaunch(t, h, http.MethodPost, postLink, signedIn(t, testHandle),
		func(r *http.Request) { r.Header.Set("X-Sparkbox-Console", "1") })
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d (%s), want 303", rec.Code, rec.Body)
	}
}

// TestCreateAwaitsSecretsBeforeRedirecting is the regression for a sandbox that
// came up through this door holding none of its owner's secrets.
//
// pam_env reads /etc/environment once, at session setup, and ctlops.Create
// pushes the secret block asynchronously. Both attach paths know this and put a
// synchronous barrier in front of the session — but internal/xterm gates its
// barrier on `box.State != StateRunning` (ws.go:189), and a sandbox this door
// just built is already running by the time the browser follows the 303. The
// gate reads false, the terminal opens, and the push lands in a file that
// session will never read again: observed live as a shell whose `claude` asked
// the user to log in on a box they had already tokenised, with the block on
// disk 180ms after the terminal attached.
//
// The SSH door does not have the bug only because it carries a second term for
// exactly this case (`viaNewDoor`, gateway.go:547). This is that term, for the
// door that redirects instead of dialling.
func TestCreateAwaitsSecretsBeforeRedirecting(t *testing.T) {
	ops := &fakeOps{t: t, allowCreate: true}
	h := creatable(t, ops)

	rec := serveLaunch(t, h, http.MethodPost, postLink, signedIn(t, testHandle), fromThePage)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d (%s), want 303", rec.Code, rec.Body)
	}
	got := ops.awaitedNames()
	if len(got) != 1 || got[0] != "created-box" {
		t.Fatalf("AwaitEnv calls = %v, want the new sandbox's secrets delivered before the 303", got)
	}
}

// TestReuseDoesNotAwaitSecrets: the barrier belongs to the create and to
// nothing else. A reused sandbox is either warm — its environment already
// pushed, so an exec into the guest buys nothing — or it is paused, and then
// internal/xterm's own gate opens because the state check that misses a fresh
// create is exactly right for a resume. Waiting here too would put a redundant
// SSH round trip in front of every click on a badge for a box that exists.
func TestReuseDoesNotAwaitSecrets(t *testing.T) {
	// allowCreate stays false: a create on this link is a different bug.
	ops := &fakeOps{t: t, boxes: []ctlops.SandboxInfo{
		box("crafty-axolotl", string(vmm.StateRunning), false, time.Unix(1000, 0)),
	}}
	h := newHandler(t, ops, attached(testHandle, attachment("main", "hm"),
		map[string][]repos.Repo{"crafty-axolotl": {{Host: gitHubHost, Slug: testSlug, Ref: "main"}}}))

	rec := serveLaunch(t, h, http.MethodPost, "https://go.example.test/wandb/hivemind",
		signedIn(t, testHandle), fromThePage)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d (%s), want 303", rec.Code, rec.Body)
	}
	if got := ops.awaitedNames(); len(got) != 0 {
		t.Fatalf("AwaitEnv calls = %v, want none on a reuse", got)
	}
}

// TestUndeliveredSecretsStillLaunch: the barrier is never fatal. A guest that
// cannot be reached for the push is still a sandbox worth handing over — the
// next transition to running pushes again — and a button that answers an error
// screen because one SSH exec timed out is a worse failure than the one this
// path exists to prevent.
func TestUndeliveredSecretsStillLaunch(t *testing.T) {
	ops := &fakeOps{t: t, allowCreate: true, awaitErr: errStore}
	h := creatable(t, ops)

	rec := serveLaunch(t, h, http.MethodPost, postLink, signedIn(t, testHandle), fromThePage)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d (%s), want 303 despite the failed delivery", rec.Code, rec.Body)
	}
	if want := "https://created-box-xterm.example.test/"; rec.Header().Get("Location") != want {
		t.Errorf("Location = %q, want %q", rec.Header().Get("Location"), want)
	}
}
