package launch

import (
	"crypto/sha256"
	"encoding/base64"
	"flag"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/repos"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
)

// updateGolden writes the rendered screens into testdata/ so a developer can
// open them in a browser and look at them.
//
// It exists because hack/preview-console.py cannot preview this page and never
// will: that harness serves a static index.html and injects a mock fetch shim,
// and its entire purpose is stubbing the XHR a single-page console makes. This
// page makes none — it is server-rendered html/template whose content comes out
// of three SQLite reads — so the harness would feed it the user console's
// console-shaped mock and show nothing. `go test ./internal/launch -update`
// then `open internal/launch/testdata/*.golden.html` is the substitute.
//
// The files are not compared against anything. They are a viewing aid, not a
// golden test: a byte-for-byte comparison of a page whose whole point is to be
// redesigned would fail on every wording change and teach whoever hit it to run
// -update without reading the diff, which is worse than no assertion at all.
// The properties that must hold are asserted by name in the tests above.
var updateGolden = flag.Bool("update", false, "write testdata/*.golden.html for eyeballing the rendered pages")

// attachment is the repository these tests hang everything off: attached under
// the casing its owner used, on one tag, with a default branch recorded.
func attachment(ref string, tags ...string) repos.Repo {
	return repos.Repo{
		Owner: testHandle, Host: gitHubHost, Slug: testSlug, Ref: ref,
		Access: "read", Tags: tags,
	}
}

// TestGetIsSideEffectFree is the security boundary of this package, made
// executable.
//
// A launch link lives in a public comment, where it is fetched by link
// scanners, unfurlers, security crawlers and browser prefetchers that nobody
// consented to. Every one of those is a GET. So every GET route on this door is
// driven here against a fake Sandboxes whose Create fails the test on sight,
// over every interesting state: no session, a session with no attachment, an
// attachment with no sandbox, and an attachment with a matching sandbox.
//
// The Attachments interface makes the other half structural rather than tested:
// it declares three methods and all three are reads, so nothing in this package
// CAN attach a repository or write a tag row.
func TestGetIsSideEffectFree(t *testing.T) {
	store := attached(testHandle, attachment("main", "hm"), map[string][]repos.Repo{
		"crafty-axolotl": {{Host: gitHubHost, Slug: testSlug, Ref: "main"}},
	})
	ops := &fakeOps{t: t, boxes: []ctlops.SandboxInfo{
		box("crafty-axolotl", string(vmm.StateRunning), false, time.Unix(1000, 0)),
	}}
	h := newHandler(t, ops, store)

	for _, tc := range []struct {
		name string
		url  string
		opts []func(*http.Request)
	}{
		{"the badge, as camo fetches it", "https://go.example.test/badge.svg", nil},
		{"an unauthenticated scanner", "https://go.example.test/wandb/hivemind", []func(*http.Request){asBrowser}},
		{"a signed-in visitor with a match", "https://go.example.test/wandb/hivemind", []func(*http.Request){asBrowser, signedIn(t, testHandle)}},
		{"a signed-in visitor with no match", "https://go.example.test/wandb/hivemind?ref=feat/x", []func(*http.Request){asBrowser, signedIn(t, testHandle)}},
		{"a repository they never attached", "https://go.example.test/wandb/other", []func(*http.Request){asBrowser, signedIn(t, testHandle)}},
		{"the bare door", "https://go.example.test/", []func(*http.Request){asBrowser, signedIn(t, testHandle)}},
		{"a path that is not a repository", "https://go.example.test/deep/er/still", []func(*http.Request){asBrowser, signedIn(t, testHandle)}},
		{"a malformed slug", "https://go.example.test/-nope/x", []func(*http.Request){asBrowser, signedIn(t, testHandle)}},
		{"a malformed ref", "https://go.example.test/wandb/hivemind?ref=--upload-pack=evil", []func(*http.Request){asBrowser, signedIn(t, testHandle)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := serveLaunch(t, h, http.MethodGet, tc.url, tc.opts...)
			// A GET must never mint or extend a session either: the only thing
			// on this door allowed to write a cookie is the login page, which
			// is a different host entirely.
			if got := rec.Header().Values("Set-Cookie"); len(got) != 0 {
				t.Errorf("a GET set a cookie (%v); only the login page may", got)
			}
		})
	}
	if n := ops.createCount(); n != 0 {
		t.Fatalf("%d sandboxes were built by GET requests, want 0", n)
	}
}

// TestUnauthenticatedGetBouncesWithTheQuery: the sign-in detour must be
// invisible.
//
// Somebody clicks a badge, signs in, and has to land on the page the badge
// pointed at — the same repository AND the same branch. A return URL that lost
// the query would drop them on the repository's default branch, silently, and
// they would create a sandbox on the wrong one without ever knowing a parameter
// had been eaten.
func TestUnauthenticatedGetBouncesWithTheQuery(t *testing.T) {
	h := newHandler(t, &fakeOps{t: t}, &fakeRepos{})

	rec := serveLaunch(t, h, http.MethodGet, "https://go.example.test/wandb/hivemind?ref=feat/x", asBrowser)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303 to the login page", rec.Code)
	}
	loc := rec.Header().Get("Location")
	const prefix = "https://login.example.test/?return="
	if !strings.HasPrefix(loc, prefix) {
		t.Fatalf("Location = %q, want it to start with %q", loc, prefix)
	}
	back, err := url.QueryUnescape(strings.TrimPrefix(loc, prefix))
	if err != nil {
		t.Fatalf("the return URL is not escaped legibly: %v", err)
	}
	if want := "https://go.example.test/wandb/hivemind?ref=feat/x"; back != want {
		t.Fatalf("return = %q, want %q — path AND query", back, want)
	}
}

// TestNonHtmlGetIs401: a client that did not ask for HTML is not a browser, and
// bouncing it through a login page it cannot render is worse than saying no.
func TestNonHtmlGetIs401(t *testing.T) {
	h := newHandler(t, &fakeOps{t: t}, &fakeRepos{})

	rec := serveLaunch(t, h, http.MethodGet, "https://go.example.test/wandb/hivemind")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for a client that wants no HTML", rec.Code)
	}
}

// TestMatchRedirectsToTerminalURL is the click that happens fifty times a day:
// the visitor already has this sandbox, so no page paints at all.
//
// The Location is asserted to be the fake's TerminalURL byte-for-byte, which is
// the assertion that the handler took it verbatim from SandboxInfo rather than
// rebuilding it from the literal "xterm" — a URL that would 404 on any host
// that relabelled its terminals.
func TestMatchRedirectsToTerminalURL(t *testing.T) {
	store := attached(testHandle, attachment("main", "hm"), map[string][]repos.Repo{
		"crafty-axolotl": {{Host: gitHubHost, Slug: testSlug, Ref: "main"}},
	})
	ops := &fakeOps{t: t, boxes: []ctlops.SandboxInfo{
		box("crafty-axolotl", string(vmm.StateRunning), false, time.Unix(1000, 0)),
	}}
	h := newHandler(t, ops, store)

	rec := serveLaunch(t, h, http.MethodGet, "https://go.example.test/wandb/hivemind", asBrowser, signedIn(t, testHandle))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303 straight into the terminal", rec.Code)
	}
	if want := "https://crafty-axolotl-xterm.example.test/"; rec.Header().Get("Location") != want {
		t.Fatalf("Location = %q, want the SandboxInfo's own %q", rec.Header().Get("Location"), want)
	}
	if rec.Body.Len() != 0 && strings.Contains(rec.Body.String(), "<form") {
		t.Error("the fast path painted a page with a create form on it")
	}
	// 301 would be cached by the browser and would outlive the sandbox it
	// points at, with no way to take it back.
	if rec.Code == http.StatusMovedPermanently {
		t.Error("a permanent redirect to one sandbox cannot be revoked")
	}
}

// TestConfirmPageRenders walks the screen a visitor actually reads before
// pressing the button.
func TestConfirmPageRenders(t *testing.T) {
	store := &fakeRepos{attachments: map[string][]repos.Repo{testHandle: {
		// Attached in mixed case on purpose: the URL below is all lower-case,
		// and what the page shows must be the casing its OWNER used.
		{Owner: testHandle, Host: gitHubHost, Slug: "WandB/HiveMind", Ref: "main", Tags: []string{"hm"}},
		// A second repository on the `default` tag, which every create carries
		// whether or not the attachment does. This is the surprise the
		// "also clones" row exists to remove.
		{Owner: testHandle, Host: gitHubHost, Slug: "wandb/notebooks", Tags: []string{"default"}},
	}}}
	h := newHandler(t, &fakeOps{t: t}, store)

	rec := serveLaunch(t, h, http.MethodGet, "https://go.example.test/wandb/hivemind?ref=feat/x", asBrowser, signedIn(t, testHandle))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()

	for _, want := range []string{
		// The stored casing, not the URL's.
		"WandB/HiveMind",
		"feat/x",
		// The full tag set the create will carry: the attachment's own tag AND
		// the `default` ctlops.defaultTags stamps on every create.
		">hm<", ">default<",
		// The other repository those tags drag in.
		"wandb/notebooks",
		// One form, no fields, posting back to this same door.
		`<form method="post" action="/wandb/hivemind?ref=feat%2Fx" data-busy="Creating…">`,
		"Create a sandbox",
		// The sentence about running a branch somebody else chose.
		"chose the branch",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the confirm page does not contain %q", want)
		}
	}
	// The URL's own casing must not appear: showing it would let a link author
	// choose how the repository is spelled on the page the clicker reads.
	if strings.Contains(body, "wandb/hivemind<") {
		t.Error("the page echoed the URL's casing instead of the attachment's")
	}
	if strings.Contains(body, "<input") {
		t.Error("the form grew a field; the server re-parses the path and query and needs nothing from the body")
	}
	assertOneScript(t, body)
	assertPageHeaders(t, rec.Header())

	if *updateGolden {
		writeGolden(t, "page.golden.html", body)
	}
}

// TestConfirmPageListsTheEscapeHatch covers the one branch case no fold can
// decide, and the page's answer to it.
//
// The attachment records no default branch (meaning "whatever GitHub says it
// is") and the link spells out ?ref=main. Nothing in this tree knows what a
// repository's default branch is called, so the two cannot be folded together
// and the resolve correctly reports "no match" — which without this list would
// mean a second sandbox on what is very probably the same branch. A human
// reading "crafty-axolotl · default branch · running" closes that gap in one
// click, which is why the near-miss list is not decoration.
func TestConfirmPageListsTheEscapeHatch(t *testing.T) {
	store := attached(testHandle, attachment("", "hm"), map[string][]repos.Repo{
		"crafty-axolotl": {{Host: gitHubHost, Slug: testSlug, Ref: ""}},
		"brave-otter":    {{Host: gitHubHost, Slug: testSlug, Ref: "feat/y"}},
	})
	ops := &fakeOps{t: t, boxes: []ctlops.SandboxInfo{
		box("crafty-axolotl", string(vmm.StateRunning), false, time.Unix(2000, 0)),
		box("brave-otter", string(vmm.StatePaused), true, time.Unix(1000, 0)),
	}}
	h := newHandler(t, ops, store)

	rec := serveLaunch(t, h, http.MethodGet, "https://go.example.test/wandb/hivemind?ref=main",
		asBrowser, signedIn(t, testHandle))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want the confirm page", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"Your sandboxes on this repository",
		`href="https://crafty-axolotl-xterm.example.test/"`,
		"crafty-axolotl", "default branch", ">running<",
		// The unreachable one stays in the list — this is a list a human reads,
		// and "you have one on a machine that is not answering" is information
		// even though redirecting to it would be a hang.
		"brave-otter", "feat/y", ">offline<",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the escape-hatch list does not contain %q", want)
		}
	}
	if *updateGolden {
		writeGolden(t, "page-others.golden.html", body)
	}
}

// TestNotAttachedPageTeaches covers the state this door will be in more often
// than any other: a first-time visitor, arriving from a public comment, with an
// account and no attachment.
//
// It must be a screen that explains what an attachment is and hands over the
// one command that fixes it — and it must NOT offer a create button, because a
// sandbox built with nothing selecting this repository comes up with an empty
// working tree and looks like the product is broken.
func TestNotAttachedPageTeaches(t *testing.T) {
	h := newHandler(t, &fakeOps{t: t}, &fakeRepos{attachments: map[string][]repos.Repo{
		testHandle: {attachment("", "hm")},
	}})

	rec := serveLaunch(t, h, http.MethodGet, "https://go.example.test/wandb/other", asBrowser, signedIn(t, testHandle))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	body := rec.Body.String()
	if want := "ssh ctl@example.test repo add wandb/other --tag &lt;t&gt;"; !strings.Contains(body, want) {
		t.Errorf("the page does not carry the attach command %q", want)
	}
	if strings.Contains(body, "<form") || strings.Contains(body, "<button") {
		t.Error("the not-attached page offers a create button; it would build a sandbox with an empty working tree")
	}
	assertOneScript(t, body)
	assertPageHeaders(t, rec.Header())
}

// An attachment carrying NO tag gets its own screen, and no create button.
//
// This is the state that used to be worst: internal/repos deliberately does not
// stamp `default` on an untagged attachment, so a tagless one is selected by no
// sandbox at all. The tags a create carries come only from the attachment, so
// the box would come up tagged `default` alone, ReposForSandbox would return no
// row, the repository would never be cloned — and SandboxesForRepo could not
// find that box afterwards either, so the NEXT click would build another one.
// Every click, a new checkout-less sandbox, until the running limit stopped it
// with a message about the wrong thing.
//
// It is a distinct screen from the not-attached one because the remedy differs:
// this visitor did the thing that page asks for, and being told to attach a
// repository they have already attached reads as the door being broken.
func TestUntaggedAttachmentRefusesRatherThanBuildingAnEmptyBox(t *testing.T) {
	ops := &fakeOps{t: t} // Create here fails the test outright
	h := newHandler(t, ops, &fakeRepos{attachments: map[string][]repos.Repo{
		testHandle: {{Owner: testHandle, Host: gitHubHost, Slug: testSlug, Ref: "main"}}, // no Tags
	}})

	rec := serveLaunch(t, h, http.MethodGet, "https://go.example.test/"+testSlug, asBrowser, signedIn(t, testHandle))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "<form") || strings.Contains(body, "<button") {
		t.Error("the untagged-attachment page offers a create; that sandbox provably cannot clone the repository")
	}
	// It must name the real problem — the missing tag — rather than repeating
	// the attach instruction the visitor has already followed.
	if !strings.Contains(body, "no tag") && !strings.Contains(body, "No tag") {
		t.Errorf("the page does not say the attachment carries no tag:\n%s", body)
	}
	if want := "ssh ctl@example.test repo add " + testSlug + " --tag &lt;t&gt;"; !strings.Contains(body, want) {
		t.Errorf("the page does not carry the tagging command %q", want)
	}
	assertOneScript(t, body)
	assertPageHeaders(t, rec.Header())
}

// TestCrossOwnerIsIndistinguishable is the isolation assertion, and the one
// that matters most on a surface reached from a public URL.
//
// Another account has this repository attached AND has a running sandbox on it.
// The visitor must get the ordinary create page — byte-identical to the one
// they would get if that other account had never existed. Anything else, down
// to a differing byte, would turn this door into an oracle for who is working
// on what.
func TestCrossOwnerIsIndistinguishable(t *testing.T) {
	mine := attachment("main", "hm")
	lonely := newHandler(t, &fakeOps{t: t}, &fakeRepos{
		attachments: map[string][]repos.Repo{testHandle: {mine}},
	})
	crowded := newHandler(t, &fakeOps{t: t, boxes: []ctlops.SandboxInfo{
		box("someone-elses-box", string(vmm.StateRunning), false, time.Unix(2000, 0)),
	}}, &fakeRepos{
		attachments: map[string][]repos.Repo{
			testHandle:  {mine},
			otherHandle: {{Owner: otherHandle, Host: gitHubHost, Slug: testSlug, Ref: "main", Tags: []string{"hm"}}},
		},
		boxes: map[string]map[string][]repos.Repo{otherHandle: {
			"someone-elses-box": {{Host: gitHubHost, Slug: testSlug, Ref: "main"}},
		}},
	})

	const link = "https://go.example.test/wandb/hivemind"
	alone := serveLaunch(t, lonely, http.MethodGet, link, asBrowser, signedIn(t, testHandle))
	shared := serveLaunch(t, crowded, http.MethodGet, link, asBrowser, signedIn(t, testHandle))

	if alone.Code != http.StatusOK || shared.Code != http.StatusOK {
		t.Fatalf("statuses = %d and %d, want 200 both", alone.Code, shared.Code)
	}
	if alone.Body.String() != shared.Body.String() {
		t.Fatal("another account's sandbox on the same repository changed the page; this door must not be an oracle")
	}
	if strings.Contains(shared.Body.String(), "someone-elses-box") {
		t.Fatal("the page named another account's sandbox")
	}
}

// TestUnknownParamsAreIgnored is the forward-compatibility promise, tested.
//
// The URL goes into comments that outlive the deployment they were written for.
// A host built years from now must still honour a link written today, and a
// link a tracker decorated with utm_source must behave exactly as if it had
// not. So an unknown parameter is neither read nor refused — and the proof is
// that the two pages are byte-identical.
func TestUnknownParamsAreIgnored(t *testing.T) {
	newH := func() *Handler {
		return newHandler(t, &fakeOps{t: t}, &fakeRepos{attachments: map[string][]repos.Repo{
			testHandle: {attachment("main", "hm")},
		}})
	}
	plain := serveLaunch(t, newH(), http.MethodGet,
		"https://go.example.test/wandb/hivemind?ref=feat/x", asBrowser, signedIn(t, testHandle))
	decorated := serveLaunch(t, newH(), http.MethodGet,
		"https://go.example.test/wandb/hivemind?ref=feat/x&utm_source=slack&v=9&tag=prod", asBrowser, signedIn(t, testHandle))

	if plain.Code != decorated.Code {
		t.Fatalf("statuses differ: %d vs %d", plain.Code, decorated.Code)
	}
	if plain.Body.String() != decorated.Body.String() {
		t.Fatal("an unknown query parameter changed the page")
	}
	// The one that would be a security bug rather than a compatibility bug: a
	// tag in a public link is a selector over the CLICKER's decrypted secrets.
	if strings.Contains(decorated.Body.String(), ">prod<") {
		t.Fatal("a tag= parameter reached the page; tags come only from the attachment")
	}
}

// TestPageIsSelfContained pins the two properties that make the strict
// Content-Security-Policy possible at all.
func TestPageIsSelfContained(t *testing.T) {
	h := newHandler(t, &fakeOps{t: t}, &fakeRepos{attachments: map[string][]repos.Repo{
		testHandle: {attachment("main", "hm")},
	}})
	rec := serveLaunch(t, h, http.MethodGet, "https://go.example.test/wandb/hivemind?ref=feat/x", asBrowser, signedIn(t, testHandle))
	body := rec.Body.String()

	// webui.Compose ran: the marker is gone and the design system is in.
	if strings.Contains(body, "/*SHARED_CSS*/") {
		t.Error("the shared-CSS marker survived into the page; Compose did not run")
	}
	if !strings.Contains(body, "--muted-foreground") {
		t.Error("the shared design tokens are missing; this page would not look like the rest of the product")
	}
	// Nothing is fetched from anywhere. An external stylesheet or font would be
	// blocked by default-src 'none' and render as an unstyled page, and an
	// external image would leak the visit to a third party.
	for _, banned := range []string{"<link rel=\"stylesheet\"", "@font-face", "@import", "//fonts.", "//cdn."} {
		if strings.Contains(body, banned) {
			t.Errorf("the page pulls in %q; default-src 'none' would block it and the page would render bare", banned)
		}
	}
	assertOneScript(t, body)
}

// TestNotFoundIsPlainAndUngated: a path that is not a repository is not a thing
// to sign anybody in for.
//
// Bouncing an unknown URL through the login page — so the visitor
// authenticates, comes back, and is then told the page does not exist — spends
// somebody's credentials on a typo.
func TestNotFoundIsPlainAndUngated(t *testing.T) {
	h := newHandler(t, &fakeOps{t: t}, &fakeRepos{})

	rec := serveLaunch(t, h, http.MethodGet, "https://go.example.test/deep/er/still", asBrowser)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 with no sign-in detour", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain", ct)
	}
	if !strings.Contains(rec.Body.String(), "https://go.example.test/<owner>/<repo>") {
		t.Errorf("the 404 does not show the link grammar: %q", rec.Body.String())
	}
}

// TestMalformedLinkIsRefusedWithTheGrammar covers the two parameter refusals.
//
// A leading '-' in a ref is the load-bearing one: the value reaches the guest
// as the argument of `git clone --branch <ref>`, where a leading dash is an
// option and not a branch.
func TestMalformedLinkIsRefusedWithTheGrammar(t *testing.T) {
	h := newHandler(t, &fakeOps{t: t}, &fakeRepos{attachments: map[string][]repos.Repo{
		testHandle: {attachment("main", "hm")},
	}})

	for _, tc := range []struct{ name, url, want string }{
		{"a slug that is not one", "https://go.example.test/-nope/x", "is not a repository name"},
		{"a ref that is an option", "https://go.example.test/wandb/hivemind?ref=--upload-pack=evil", "is not a branch or tag name"},
		{"a ref that traverses", "https://go.example.test/wandb/hivemind?ref=a..b", "is not a branch or tag name"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := serveLaunch(t, h, http.MethodGet, tc.url, asBrowser, signedIn(t, testHandle))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", rec.Code)
			}
			if !strings.Contains(rec.Body.String(), tc.want) {
				t.Errorf("the refusal does not say %q", tc.want)
			}
			if !strings.Contains(rec.Body.String(), "https://go.example.test/&lt;owner&gt;/&lt;repo&gt;") {
				t.Error("a malformed link's page does not show the correct grammar, which is the whole answer")
			}
		})
	}
}

// TestHomeRedirectsToTheConsole: somebody who trimmed a launch link back to its
// hostname wants to know what it is, and their own console is the better
// answer than a page about URL grammar.
func TestHomeRedirectsToTheConsole(t *testing.T) {
	h := newHandler(t, &fakeOps{t: t}, &fakeRepos{})
	rec := serveLaunch(t, h, http.MethodGet, "https://go.example.test/", asBrowser, signedIn(t, testHandle))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rec.Code)
	}
	if got := rec.Header().Get("Location"); got != "https://my.example.test/" {
		t.Errorf("Location = %q, want the configured console", got)
	}

	// With no console configured there is no hostname to guess, so the door
	// explains itself instead of redirecting somewhere invented.
	h.homeURL = ""
	rec = serveLaunch(t, h, http.MethodGet, "https://go.example.test/", asBrowser, signedIn(t, testHandle))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want the explainer page when no console is configured", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Launch links") {
		t.Error("the explainer does not explain")
	}
}

// assertOneScript is the invariant the whole design rests on.
//
// The page carries exactly one script — progress.js, inline — and the policy
// admits it by the sha256 of its bytes and by nothing else. That is what keeps
// `default-src 'none'` an honest statement rather than a decorative one: an
// injected <script>, an onclick, a javascript: URL and a second file all remain
// refused by the browser, because none of them hash to the one digest in the
// header.
//
// So this asserts the three halves of that claim together — one element, no
// remote source, no inline-handler surface — and then does the arithmetic the
// browser will do, over the body as it was actually rendered. A template edit
// that so much as reindents the script breaks the digest, and this is where
// that is discovered rather than on a stranger's first click.
func assertOneScript(t *testing.T, body string) {
	t.Helper()
	lower := strings.ToLower(body)
	if n := strings.Count(lower, "<script"); n != 1 {
		t.Errorf("the page has %d <script> elements; the policy admits exactly one, by hash", n)
		return
	}
	// Every way of running code that a hash cannot cover. An attribute handler
	// and a javascript: URL are governed by 'unsafe-inline' and 'unsafe-hashes',
	// neither of which this policy grants, so any of these would silently not
	// run — and a page whose behaviour depends on something the CSP refuses is
	// worse than one with no behaviour at all.
	for _, banned := range []string{"javascript:", "onclick=", "onload=", "onsubmit=", "onerror=", "<script src", "<script  src"} {
		if strings.Contains(lower, banned) {
			t.Errorf("the page contains %q, which this Content-Security-Policy does not admit", banned)
		}
	}

	inline, err := inlineScript([]byte(body))
	if err != nil {
		t.Fatalf("the page's script: %v", err)
	}
	sum := sha256.Sum256(inline)
	want := "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
	if want != progressJSHash {
		t.Errorf("the rendered script hashes to %s, but the policy admits %s — the browser would refuse to run it",
			want, progressJSHash)
	}
	if !strings.Contains(pageCSP("example.test"), "script-src "+progressJSHash+";") {
		t.Errorf("the policy does not admit the page's own script: %s", pageCSP("example.test"))
	}
}

// assertPageHeaders pins the response headers byte-for-byte.
//
// The CSP is compared against the package's own constant rather than a literal
// copy, because a second copy in a test drifts into agreement with whatever the
// handler was changed to say — which is the one thing a header test must not
// do.
func assertPageHeaders(t *testing.T, head http.Header) {
	t.Helper()
	for header, want := range map[string]string{
		"Content-Type":            "text/html; charset=utf-8",
		"Cache-Control":           "no-store",
		"Content-Security-Policy": pageCSP("example.test"),
		"X-Frame-Options":         "DENY",
		"Referrer-Policy":         "same-origin",
		"X-Content-Type-Options":  "nosniff",
	} {
		if got := head.Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
	// Spelled out separately because it is the clause the design buys and the
	// one a future edit is most likely to relax without noticing.
	policy := pageCSP("example.test")
	if !strings.HasPrefix(policy, "default-src 'none'") {
		t.Error("the policy no longer starts from default-src 'none'")
	}
	// The one script clause, in the only form that is narrower than nothing at
	// all: a digest, with no host and no 'unsafe-inline' beside it. Both of
	// those would admit an injected script as readily as our own.
	if !strings.Contains(policy, "script-src 'sha256-") {
		t.Error("script-src no longer admits the page's script by hash")
	}
	// Read against the script-src clause alone: style-src legitimately carries
	// 'unsafe-inline' for the composed <style>, and a substring search over the
	// whole policy would call that a script hazard.
	scriptSrc := policy[strings.Index(policy, "script-src "):]
	scriptSrc = scriptSrc[:strings.Index(scriptSrc, ";")]
	for _, loose := range []string{"'unsafe-inline'", "'unsafe-eval'", "'unsafe-hashes'", "'self'", "*", "https:"} {
		if strings.Contains(scriptSrc, loose) {
			t.Errorf("script-src grew %q (%q); the hash alone is what makes default-src 'none' honest", loose, scriptSrc)
		}
	}
	// form-action has to reach the terminal host a successful create redirects
	// to. Firefox applies this directive to every hop of the redirect chain, so
	// a bare 'self' blocks the handoff AFTER the VM exists — and Chrome and
	// Safari would never show it.
	if !strings.Contains(policy, "form-action 'self' https://*.example.test") {
		t.Errorf("form-action no longer reaches the terminal host: %q", policy)
	}
	// The header below is load-bearing for the create, not a privacy nicety.
	// See TestTheReferrerPolicyStillYieldsAnOrigin.
	if head.Get("Referrer-Policy") == "no-referrer" {
		t.Error("Referrer-Policy is no-referrer, which makes a browser send Origin: null and 403s every create")
	}
}

// The create is a plain form POST, and firstParty (csrf.go) authenticates it by
// its Origin header. The Fetch standard computes that header from the page's
// own referrer policy: under "no-referrer" a non-CORS, non-GET request
// serializes its origin as the literal `null`, which firstParty refuses — so
// shipping that value would mean the "Create a sandbox" button 403s in every
// spec-compliant browser and no badge ever built anything.
//
// No unit test can observe this, because a test writes the Origin header itself
// instead of deriving it the way a browser does; this is the guard that stands
// in for the browser. The allowed set is the policies that keep a real origin on
// a same-origin POST.
func TestTheReferrerPolicyStillYieldsAnOrigin(t *testing.T) {
	store := &fakeRepos{attachments: map[string][]repos.Repo{testHandle: {attachment("main", "hm")}}}
	h := newHandler(t, &fakeOps{t: t}, store)

	rec := serveLaunch(t, h, http.MethodGet, "https://go.example.test/"+testSlug+"?ref=feat/x",
		asBrowser, signedIn(t, testHandle))

	got := rec.Header().Get("Referrer-Policy")
	switch got {
	case "same-origin", "strict-origin", "strict-origin-when-cross-origin", "origin", "no-referrer-when-downgrade":
		// All of these leave a same-origin form POST with a real Origin.
	case "":
		t.Error("no Referrer-Policy at all; the page URL names a repository and should not leak off-origin")
	default:
		t.Errorf("Referrer-Policy = %q, which does not provably leave an Origin header on this page's own "+
			"form POST. firstParty authenticates the create with that header, so the button would 403.", got)
	}
}

// writeGolden drops one rendered screen on disk under -update. The name is a
// parameter because more than one test renders a screen worth looking at, and
// two tests writing the same file would make the survivor depend on which ran
// last.
func writeGolden(t *testing.T, name, body string) {
	t.Helper()
	dir := "testdata"
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir testdata: %v", err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	t.Logf("wrote %s — open it in a browser to look at the page", path)
}
