package launch

// `?new=1`: the second button on the reuse screen, and the note that explains
// why somebody would press it.

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/repos"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
)

// freshRig is the state every test below starts from: one attachment, one
// sandbox already holding it on the same branch under environment `hivemind`,
// and an environment row the caller can adjust before the request.
func freshRig(t *testing.T, builtAt *time.Time, created time.Time) (*Handler, *fakeOps) {
	t.Helper()
	store := attached(testHandle, attachment("main", "hivemind"), map[string][]repos.Repo{
		"existing": {{Host: gitHubHost, Slug: testSlug, Ref: "main"}},
	})
	env := readyEnvironment("hivemind")
	row := env["hivemind"]
	row.BuiltAt = builtAt
	env["hivemind"] = row

	b := box("existing", string(vmm.StateRunning), false, created)
	b.Tags = []string{"hivemind"}
	b.CreatedAt = created
	ops := &fakeOps{t: t, environments: env, boxes: []ctlops.SandboxInfo{b}}
	return newHandler(t, ops, store), ops
}

// TestReuseScreenOffersToCreateAnotherAnyway.
//
// Before this the screen had exactly one door — open the box you have — and
// somebody who wanted a second one on the same branch had to leave the page.
func TestReuseScreenOffersToCreateAnotherAnyway(t *testing.T) {
	h, _ := freshRig(t, nil, time.Unix(1000, 0).UTC())

	rec := serveLaunch(t, h, http.MethodGet,
		"https://go.example.test/wandb/hivemind?env=hivemind", asBrowser, signedIn(t, testHandle))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s), want the confirm screen", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"Open existing",
		"Create a new one",
		// The second form posts to the SAME door with the flag in the query,
		// because this page's forms carry no fields at all.
		`action="/wandb/hivemind?env=hivemind&amp;new=1"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the reuse screen does not contain %q", want)
		}
	}
	if strings.Contains(body, "<input") {
		t.Error("a form grew a field; both buttons must re-derive everything from path, query and session")
	}
	assertOneScript(t, body)
}

// TestFreshGetSuppressesTheHandoffAndKeepsTheBoxOnThePage.
//
// The no-environment link is the one that redirects without painting anything,
// so this is where `?new=1` has to prove it interrupts that — and that it
// demotes the match rather than hiding it, since the box is still theirs and
// still the one they were probably looking at.
func TestFreshGetSuppressesTheHandoffAndKeepsTheBoxOnThePage(t *testing.T) {
	store := attached(testHandle, attachment("main", "hm"), map[string][]repos.Repo{
		"crafty-axolotl": {{Host: gitHubHost, Slug: testSlug, Ref: "main"}},
	})
	ops := &fakeOps{t: t, boxes: []ctlops.SandboxInfo{
		box("crafty-axolotl", string(vmm.StateRunning), false, time.Unix(1000, 0)),
	}}
	h := newHandler(t, ops, store)

	rec := serveLaunch(t, h, http.MethodGet,
		"https://go.example.test/wandb/hivemind?new=1", asBrowser, signedIn(t, testHandle))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want the confirm screen rather than a redirect", rec.Code)
	}
	if got := rec.Header().Get("Location"); got != "" {
		t.Fatalf("Location = %q; ?new=1 must not hand the visitor their existing box", got)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "crafty-axolotl") {
		t.Error("the box the visitor already has is not on the page; it was dropped rather than demoted")
	}
	if !strings.Contains(body, "Create a sandbox") {
		t.Error("the screen does not offer to create")
	}
	// A GET is still a GET. fakeOps fails the test if Create is reached.
	assertOneScript(t, body)
}

// TestFreshPostBuildsASecondSandbox. The reuse branch is the whole of the
// door's idempotency, so this is the one place it is deliberately off.
func TestFreshPostBuildsASecondSandbox(t *testing.T) {
	h, ops := freshRig(t, nil, time.Unix(1000, 0).UTC())
	ops.allowCreate = true

	rec := serveLaunch(t, h, http.MethodPost,
		"https://go.example.test/wandb/hivemind?env=hivemind&new=1", signedIn(t, testHandle), fromThePage)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d (%s), want a redirect into the new sandbox", rec.Code, rec.Body)
	}
	if len(ops.created) != 1 {
		t.Fatalf("created %d sandboxes, want 1: %+v", len(ops.created), ops.created)
	}
	if got := rec.Header().Get("Location"); strings.Contains(got, "existing-xterm") {
		t.Fatalf("Location = %q; the visitor was handed the box they asked not to reuse", got)
	}
}

// TestPostWithoutTheFlagStillReuses is the guard on the change above: the
// default is what nearly every click wants, and it must not have moved.
func TestPostWithoutTheFlagStillReuses(t *testing.T) {
	h, ops := freshRig(t, nil, time.Unix(1000, 0).UTC())

	rec := serveLaunch(t, h, http.MethodPost,
		"https://go.example.test/wandb/hivemind?env=hivemind", signedIn(t, testHandle), fromThePage)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d (%s), want a redirect", rec.Code, rec.Body)
	}
	if len(ops.created) != 0 {
		t.Fatalf("created %+v; an ordinary press must reuse", ops.created)
	}
	if want := "https://existing-xterm.example.test/"; rec.Header().Get("Location") != want {
		t.Fatalf("Location = %q, want %q", rec.Header().Get("Location"), want)
	}
}

// TestAStaleSandboxSaysSo: the box predates its environment's current build, so
// it cannot be running that build's disk.
func TestAStaleSandboxSaysSo(t *testing.T) {
	built := time.Unix(9000, 0).UTC()
	h, _ := freshRig(t, &built, time.Unix(1000, 0).UTC())

	rec := serveLaunch(t, h, http.MethodGet,
		"https://go.example.test/wandb/hivemind?env=hivemind", asBrowser, signedIn(t, testHandle))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	for _, want := range []string{"existing", "was last built", "older disk", "Create a new one"} {
		if !strings.Contains(body, want) {
			t.Errorf("the stale note does not contain %q", want)
		}
	}
}

// TestACurrentSandboxSaysNothing.
//
// The half that matters more. This screen is the first thing a stranger sees of
// the product, and a warning that fires on a perfectly current box is worse
// than no warning at all — so every case where the comparison cannot be made
// honestly renders nothing.
func TestACurrentSandboxSaysNothing(t *testing.T) {
	built := time.Unix(1000, 0).UTC()
	for _, tc := range []struct {
		name    string
		builtAt *time.Time
		created time.Time
	}{
		{"created after the build", &built, time.Unix(9000, 0).UTC()},
		{"created at the same instant", &built, built},
		{"an environment that has never been built", nil, time.Unix(1000, 0).UTC()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := freshRig(t, tc.builtAt, tc.created)
			rec := serveLaunch(t, h, http.MethodGet,
				"https://go.example.test/wandb/hivemind?env=hivemind", asBrowser, signedIn(t, testHandle))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d (%s)", rec.Code, rec.Body)
			}
			if strings.Contains(rec.Body.String(), "older disk") {
				t.Error("a current sandbox was reported as stale")
			}
		})
	}
}

// TestOnlyOneSpellingTurnsTheFlagOn. Everything else is ignored rather than
// refused: these URLs live in comments that outlive the deployment.
func TestOnlyOneSpellingTurnsTheFlagOn(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  bool
	}{
		{"1", true},
		{"true", false},
		{"", false},
		{"0", false},
	} {
		got, err := parseTarget("wandb", "hivemind", "", "", tc.value)
		if err != nil {
			t.Fatalf("parseTarget(new=%q): %v", tc.value, err)
		}
		if got.Fresh != tc.want {
			t.Errorf("parseTarget(new=%q).Fresh = %v, want %v", tc.value, got.Fresh, tc.want)
		}
	}
}
