package launch

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/repos"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
)

const (
	testHandle = "vanpelt"
	testSlug   = "wandb/hivemind"
)

var testCaller = ctlops.Caller{Handle: testHandle}

// matchRow is one line of the blueprint's match-rule table: what the link said,
// what the attachment's own default ref is, what branch the caller's existing
// sandbox is effectively on, and whether clicking should land them back in that
// sandbox instead of building a second one.
type matchRow struct {
	name    string
	linkRef string // the raw ?ref= value; "" is a link that named no branch
	attRef  string // repos.Repo.Ref on the attachment; "" is "the repo's default branch"
	boxEff  string // what ReposForSandbox reports for the existing box
	reuse   bool
}

// matchTable is the whole rule, every row of it.
//
// The second row is the one a previous design got wrong and the reason this
// table is written out rather than summarised. ReposForSandbox coalesces the
// per-sandbox override over the attachment's default
// (internal/repos/store.go:399-411), so a sandbox with no override at all, on
// an attachment created with `--ref main`, reports its effective ref as "main"
// and not as "". A naive `eff == want` therefore fails to match a badge that
// named no branch, and every click builds another sandbox.
var matchTable = []matchRow{
	{name: "no ref anywhere", linkRef: "", attRef: "", boxEff: "", reuse: true},
	{name: "no ref in the link, attachment defaults to main", linkRef: "", attRef: "main", boxEff: "main", reuse: true},
	{name: "the link spells out the attachment's own default", linkRef: "main", attRef: "main", boxEff: "main", reuse: true},
	{name: "the link and the box agree on a branch off the default", linkRef: "feat/x", attRef: "main", boxEff: "feat/x", reuse: true},
	{name: "the link wants a branch the box is not on", linkRef: "feat/x", attRef: "", boxEff: "", reuse: false},
	{name: "the residual: an unresolvable default branch named out loud", linkRef: "main", attRef: "", boxEff: "", reuse: false},
}

// TestNormalizeRefMatchTable checks the fold itself, symmetrically, on every
// row — which is the only way it can be right, since the same function decides
// what a link asks for and what a sandbox already has.
func TestNormalizeRefMatchTable(t *testing.T) {
	for _, row := range matchTable {
		t.Run(row.name, func(t *testing.T) {
			got := normalizeRef(row.attRef, row.boxEff) == normalizeRef(row.attRef, row.linkRef)
			if got != row.reuse {
				t.Fatalf("normalizeRef(%q, %q)==normalizeRef(%q, %q) is %v, want %v",
					row.attRef, row.boxEff, row.attRef, row.linkRef, got, row.reuse)
			}
		})
	}
}

// TestResolveMatchTable runs the same table end to end, because a fold that is
// right in isolation and wired backwards into the lookup is still a duplicate
// sandbox on every click.
func TestResolveMatchTable(t *testing.T) {
	for _, row := range matchTable {
		t.Run(row.name, func(t *testing.T) {
			ops := &fakeOps{t: t, boxes: []ctlops.SandboxInfo{
				box("crafty-axolotl", string(vmm.StateRunning), false, time.Unix(1000, 0)),
			}}
			store := &fakeRepos{
				attachments: map[string][]repos.Repo{testHandle: {
					{Owner: testHandle, Host: gitHubHost, Slug: testSlug, Ref: row.attRef, Tags: []string{"hm"}},
				}},
				boxes: map[string]map[string][]repos.Repo{testHandle: {
					"crafty-axolotl": {{Host: gitHubHost, Slug: testSlug, Ref: row.boxEff}},
				}},
			}
			h := newHandler(t, ops, store)

			att, best, others, err := h.resolve(context.Background(), testCaller, target{Slug: testSlug, Ref: row.linkRef})
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if att.Slug != testSlug {
				t.Fatalf("attachment slug = %q, want %q", att.Slug, testSlug)
			}
			if got := best != nil; got != row.reuse {
				t.Fatalf("reuse = %v, want %v (link %q, attachment %q, box %q)",
					got, row.reuse, row.linkRef, row.attRef, row.boxEff)
			}
			if row.reuse {
				if best.Info.Name != "crafty-axolotl" {
					t.Errorf("matched %q, want crafty-axolotl", best.Info.Name)
				}
				if len(others) != 0 {
					t.Errorf("others = %v, want none once the only box is the match", others)
				}
				return
			}
			// A near miss is still the caller's sandbox on this repository, and
			// the confirm page lists it as the human escape hatch for exactly
			// the residual row above.
			if len(others) != 1 || others[0].Info.Name != "crafty-axolotl" {
				t.Errorf("others = %v, want the near-miss box listed for the page", others)
			}
			if len(others) == 1 && others[0].Ref != row.boxEff {
				t.Errorf("near-miss branch = %q, want the effective %q", others[0].Ref, row.boxEff)
			}
		})
	}
}

// twoBoxes wires a handle with one attachment and the named sandboxes, each on
// the attachment's own default branch so every one of them matches a ref-less
// link and only the ranking decides.
func twoBoxes(t *testing.T, attRef string, infos ...ctlops.SandboxInfo) *Handler {
	t.Helper()
	manifests := map[string][]repos.Repo{}
	for _, info := range infos {
		manifests[info.Name] = []repos.Repo{{Host: gitHubHost, Slug: testSlug, Ref: attRef}}
	}
	return newHandler(t,
		&fakeOps{t: t, boxes: infos},
		&fakeRepos{
			attachments: map[string][]repos.Repo{testHandle: {
				{Owner: testHandle, Host: gitHubHost, Slug: testSlug, Ref: attRef, Tags: []string{"hm"}},
			}},
			boxes: map[string]map[string][]repos.Repo{testHandle: manifests},
		})
}

func resolveOK(t *testing.T, h *Handler, ref string) (repos.Repo, *candidate, []candidate) {
	t.Helper()
	att, best, others, err := h.resolve(context.Background(), testCaller, target{Slug: testSlug, Ref: ref})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	return att, best, others
}

// TestResolveRanksByHowLongTheVisitorWaits: running, then paused (a warm
// restore), then archived (an object-storage download and a cold boot). All
// three are theirs and all three match; the order is the wait.
func TestResolveRanksByHowLongTheVisitorWaits(t *testing.T) {
	now := time.Unix(5000, 0)
	h := twoBoxes(t, "",
		box("archived-one", string(vmm.StateArchived), false, now),
		box("paused-one", string(vmm.StatePaused), false, now),
		box("running-one", string(vmm.StateRunning), false, now.Add(-time.Hour)),
	)

	_, best, others := resolveOK(t, h, "")
	if best == nil || best.Info.Name != "running-one" {
		t.Fatalf("best = %v, want running-one even though it is the least recently active", best)
	}
	if len(others) != 2 || others[0].Info.Name != "paused-one" || others[1].Info.Name != "archived-one" {
		t.Fatalf("others = %v, want paused before archived", others)
	}
}

// TestResolveDropsUnreachableWhenAReachableBoxExists: a redirect is a promise
// the destination works, and <name>-xterm resolves through the same control
// plane that is not answering for this node — so the terminal would hang rather
// than boot, with nothing on screen to say why.
func TestResolveDropsUnreachableWhenAReachableBoxExists(t *testing.T) {
	now := time.Unix(9000, 0)
	h := twoBoxes(t, "",
		box("stranded", string(vmm.StateRunning), true, now),
		box("healthy", string(vmm.StatePaused), false, now.Add(-time.Hour)),
	)

	_, best, _ := resolveOK(t, h, "")
	if best == nil || best.Info.Name != "healthy" {
		t.Fatalf("best = %v, want the reachable paused box over the unreachable running one", best)
	}
}

// TestResolveKeepsTheOnlyBoxEvenWhenUnreachable: creating a second sandbox
// would leave them with two the moment the node answers again, so an
// unreachable box is still the right destination when it is the only one.
func TestResolveKeepsTheOnlyBoxEvenWhenUnreachable(t *testing.T) {
	h := twoBoxes(t, "", box("stranded", string(vmm.StateRunning), true, time.Unix(9000, 0)))

	_, best, _ := resolveOK(t, h, "")
	if best == nil || best.Info.Name != "stranded" {
		t.Fatalf("best = %v, want the unreachable box when it is the only one", best)
	}
}

// TestResolveBreaksTiesOnLastActive: two boxes in the same state are equally
// good to the machine, so the tie goes to the one the human touched most
// recently — that is the one they meant.
func TestResolveBreaksTiesOnLastActive(t *testing.T) {
	now := time.Unix(7000, 0)
	h := twoBoxes(t, "",
		box("stale", string(vmm.StateRunning), false, now.Add(-2*time.Hour)),
		box("fresh", string(vmm.StateRunning), false, now),
	)

	_, best, others := resolveOK(t, h, "")
	if best == nil || best.Info.Name != "fresh" {
		t.Fatalf("best = %v, want the most recently active box", best)
	}
	if len(others) != 1 || others[0].Info.Name != "stale" {
		t.Fatalf("others = %v, want the older box listed once", others)
	}
}

// TestResolveIsStableAcrossCalls: the same inputs must produce the same
// redirect. A click that lands somewhere different each time is worse than one
// that lands somewhere slow.
func TestResolveIsStableAcrossCalls(t *testing.T) {
	same := time.Unix(3000, 0)
	h := twoBoxes(t, "",
		box("bravo", string(vmm.StateRunning), false, same),
		box("alpha", string(vmm.StateRunning), false, same),
	)

	_, first, _ := resolveOK(t, h, "")
	_, second, _ := resolveOK(t, h, "")
	if first == nil || second == nil || first.Info.Name != second.Info.Name {
		t.Fatalf("two identical resolves picked %v then %v", first, second)
	}
	if first.Info.Name != "alpha" {
		t.Errorf("tie broke to %q, want the deterministic name order", first.Info.Name)
	}
}

// TestResolveTellsNeverAttachedFromNoTaggedBox is the distinction the whole
// two-call shape exists for: SandboxesForRepo answers nil for both, and the two
// states want completely different pages — one offers a create button, the
// other cannot, because there would be nothing to check out.
func TestResolveTellsNeverAttachedFromNoTaggedBox(t *testing.T) {
	t.Run("never attached", func(t *testing.T) {
		h := newHandler(t, &fakeOps{t: t}, &fakeRepos{})
		_, _, _, err := h.resolve(context.Background(), testCaller, target{Slug: testSlug})
		var ce *ctlops.Error
		if !errors.As(err, &ce) {
			t.Fatalf("err = %v, want a classified *ctlops.Error", err)
		}
		if ce.Code != codeNoAttachment {
			t.Errorf("code = %q, want %q", ce.Code, codeNoAttachment)
		}
		if ce.HTTPStatus() != 400 {
			t.Errorf("status = %d, want 400", ce.HTTPStatus())
		}
	})

	t.Run("attached with no tagged sandbox", func(t *testing.T) {
		h := newHandler(t,
			&fakeOps{t: t},
			&fakeRepos{attachments: map[string][]repos.Repo{testHandle: {
				{Owner: testHandle, Host: gitHubHost, Slug: testSlug, Tags: []string{"hm"}},
			}}})
		att, best, others, err := h.resolve(context.Background(), testCaller, target{Slug: testSlug})
		if err != nil {
			t.Fatalf("resolve: %v, want a clean create", err)
		}
		if att.Slug != testSlug {
			t.Errorf("attachment = %q, want it returned so the page can name its tags", att.Slug)
		}
		if best != nil || others != nil {
			t.Errorf("best/others = %v/%v, want both empty", best, others)
		}
	})
}

// TestResolveFoldsSlugCaseAndDisplaysTheStoredOne: the store's slug columns are
// COLLATE NOCASE, so a link written with different casing is the same
// attachment there and has to be here too — but anything shown to a human comes
// from the stored casing, never from the URL a stranger wrote.
func TestResolveFoldsSlugCaseAndDisplaysTheStoredOne(t *testing.T) {
	ops := &fakeOps{t: t, boxes: []ctlops.SandboxInfo{
		box("crafty-axolotl", string(vmm.StateRunning), false, time.Unix(1, 0)),
	}}
	store := &fakeRepos{
		attachments: map[string][]repos.Repo{testHandle: {
			{Owner: testHandle, Host: gitHubHost, Slug: "wandb/HiveMind", Tags: []string{"hm"}},
		}},
		boxes: map[string]map[string][]repos.Repo{testHandle: {
			"crafty-axolotl": {{Host: gitHubHost, Slug: "WANDB/hivemind"}},
		}},
	}
	h := newHandler(t, ops, store)

	att, best, _, err := h.resolve(context.Background(), testCaller, target{Slug: "WaNdB/hIvEmInD"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if att.Slug != "wandb/HiveMind" {
		t.Errorf("displayed slug = %q, want the stored casing", att.Slug)
	}
	if best == nil {
		t.Fatal("no match: a differently-cased link failed to find the box the store would have matched")
	}
}

// TestResolveSkipsSandboxesTheLedgerDoesNotKnow: a tag row can outlive its
// sandbox for as long as the manager takes to clean up, and List is the ledger.
// Redirecting to a name with no record would be a promise nothing can keep.
func TestResolveSkipsSandboxesTheLedgerDoesNotKnow(t *testing.T) {
	h := twoBoxes(t, "", box("alive", string(vmm.StateRunning), false, time.Unix(1, 0)))
	// A second name only the repo store knows about.
	h.repos.(*fakeRepos).boxes[testHandle]["ghost"] = []repos.Repo{{Host: gitHubHost, Slug: testSlug}}

	_, best, others := resolveOK(t, h, "")
	if best == nil || best.Info.Name != "alive" {
		t.Fatalf("best = %v, want the box the ledger knows", best)
	}
	for _, c := range others {
		if c.Info.Name == "ghost" {
			t.Error("a sandbox with no ledger record was offered to the visitor")
		}
	}
}

// TestResolveSurfacesStoreFailures: a sqlite fault must not read as "you have
// no sandboxes", which would offer a create and quietly duplicate a box the
// caller already owns.
func TestResolveSurfacesStoreFailures(t *testing.T) {
	for _, tc := range []struct {
		name   string
		broken func(*fakeRepos, *fakeOps)
	}{
		{"ListRepos", func(r *fakeRepos, _ *fakeOps) { r.listErr = errStore }},
		{"SandboxesForRepo", func(r *fakeRepos, _ *fakeOps) { r.forRepoErr = errStore }},
		{"ReposForSandbox", func(r *fakeRepos, _ *fakeOps) { r.forSandErr = errStore }},
		{"Ops.List", func(_ *fakeRepos, o *fakeOps) { o.listErr = errStore }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := twoBoxes(t, "", box("alive", string(vmm.StateRunning), false, time.Unix(1, 0)))
			tc.broken(h.repos.(*fakeRepos), h.ops.(*fakeOps))
			if _, _, _, err := h.resolve(context.Background(), testCaller, target{Slug: testSlug}); err == nil {
				t.Fatal("resolve swallowed a store failure and reported a clean create")
			}
		})
	}
}

// TestParseTargetAcceptsWhatGitHubIssues guards the two grammars against being
// re-invented here. The owner half is a login; the name half is a repository
// name, which additionally admits '.' and '_' and may lead with a digit — so a
// hand-rolled regexp is the mistake that rejects node.js.
func TestParseTargetAcceptsWhatGitHubIssues(t *testing.T) {
	for _, tc := range []struct{ owner, repo, ref string }{
		{"wandb", "hivemind", ""},
		{"wandb", "hivemind", "feat/x"},
		{"nodejs", "node.js", "v1.2.3"},
		{"my-org", "my_repo", "2fa"},
		{"a", "9", "main"},
	} {
		got, err := parseTarget(tc.owner, tc.repo, tc.ref)
		if err != nil {
			t.Errorf("parseTarget(%q, %q, %q): %v", tc.owner, tc.repo, tc.ref, err)
			continue
		}
		if want := tc.owner + "/" + tc.repo; got.Slug != want {
			t.Errorf("slug = %q, want %q", got.Slug, want)
		}
		// The ref is carried byte for byte. Nothing in this tree folds a ref,
		// and feat/X and feat/x are two different branches.
		if got.Ref != tc.ref {
			t.Errorf("ref = %q, want the URL's %q unchanged", got.Ref, tc.ref)
		}
	}
}

// A slug that arrives with whitespace around it comes back canonical.
//
// Go's ServeMux unescapes path segments, so `/wandb/hivemind%0A` reaches the
// handler as repo == "hivemind\n". repos.ValidSlug would ACCEPT that — it trims
// before checking — and keeping the raw concatenation would then carry the
// newline into findRepo, which compares with EqualFold and would never match
// the stored "wandb/hivemind". The visitor, who genuinely has the repository
// attached, would be shown the "nothing of yours is attached" screen, from a
// link in a comment nobody can edit.
func TestParseTargetCanonicalisesTheSlug(t *testing.T) {
	for _, tc := range []struct{ name, owner, repo string }{
		{"trailing newline", "wandb", "hivemind\n"},
		{"leading space", " wandb", "hivemind"},
		{"trailing space", "wandb", "hivemind "},
		{"both ends", "\twandb", "hivemind\r"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseTarget(tc.owner, tc.repo, "")
			if err != nil {
				t.Fatalf("parseTarget refused %q/%q: %v", tc.owner, tc.repo, err)
			}
			if got.Slug != "wandb/hivemind" {
				t.Errorf("Slug = %q, want %q — the raw form would never match the stored attachment",
					got.Slug, "wandb/hivemind")
			}
		})
	}
}

func TestParseTargetRefuses(t *testing.T) {
	for _, tc := range []struct {
		name, owner, repo, ref, code string
	}{
		{"empty owner", "", "hivemind", "", "bad_slug"},
		{"empty name", "wandb", "", "", "bad_slug"},
		{"dot name", "wandb", ".", "", "bad_slug"},
		{"dotdot name", "wandb", "..", "", "bad_slug"},
		{"a clone URL's .git suffix", "wandb", "hivemind.git", "", "bad_slug"},
		{"an owner GitHub could not issue", "_internal", "hivemind", "", "bad_slug"},
		{"a slash inside the name", "wandb", "hive/mind", "", "bad_slug"},
		// A leading '-' is the load-bearing refusal: this value becomes the
		// argument of `git clone --branch <ref>`, where it is an option.
		{"an option pretending to be a branch", "wandb", "hivemind", "-upload-pack=evil", "bad_ref"},
		{"a traversing ref", "wandb", "hivemind", "a/../../b", "bad_ref"},
		{"a bare dotdot ref", "wandb", "hivemind", "..", "bad_ref"},
		{"a ref past the length bound", "wandb", "hivemind", longRef(300), "bad_ref"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseTarget(tc.owner, tc.repo, tc.ref)
			if err == nil {
				t.Fatalf("parseTarget(%q, %q, %q) was accepted", tc.owner, tc.repo, tc.ref)
			}
			if err.Code != tc.code {
				t.Errorf("code = %q, want %q", err.Code, tc.code)
			}
			if err.Kind != ctlops.KindInvalid {
				t.Errorf("kind = %v, want KindInvalid so the page answers 400", err.Kind)
			}
			if err.Op != op {
				t.Errorf("op = %q, want %q", err.Op, op)
			}
		})
	}
}

func longRef(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = 'a'
	}
	return string(b)
}
