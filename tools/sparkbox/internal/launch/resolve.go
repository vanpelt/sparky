package launch

import (
	"context"
	"slices"
	"sort"
	"strings"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/envs"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/repos"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
)

// op is the name every *ctlops.Error this package raises is stamped with. It is
// the <what> in the SSH channel's "sparkbox: <what> failed" line and the "op"
// field of the JSON error body, so naming it once here keeps the three from
// drifting the way three string literals would.
const op = "launch"

// codeNoAttachment is the error code resolve raises when the caller has nothing
// attached to the repository in the link. It is a machine token rather than a
// sentinel error value because that is how this tree distinguishes failures
// that need different pages: the confirm page checks it to decide whether to
// render the "attach it first" screen — which has no button — instead of the
// ordinary create screen.
//
// It is deliberately a distinct code from a missing sandbox. This is the ONLY
// place that can tell "you have never attached this repository" from "you have
// attached it but no sandbox of yours carries a tag that selects it", because
// SandboxesForRepo returns nil for both, and those two states want completely
// different sentences from the page.
const codeNoAttachment = "no_attachment"

// codeUntaggedAttachment is raised when the repository IS attached but its
// attachment carries no tag, which makes it unreachable by any sandbox. See
// resolve for the full mechanism; the short version is that a tag is the only
// thing connecting an attachment to a sandbox, so an attachment with none is
// configuration that can never take effect, and a create offered on top of it
// would build a box with an empty working tree every single time.
const codeUntaggedAttachment = "untagged_attachment"

const (
	codeEnvRepoMismatch = "environment_repo_mismatch"
	codeEnvNotReady     = "environment_not_ready"
)

// target is a launch link's whole meaning: repository, branch, and an optional
// owner-scoped environment selection that must be confirmed by the clicker.
type target struct {
	// Slug is "owner/name" as the URL spelled it, already validated. It is used
	// for matching, which is case-insensitive; anything DISPLAYED comes from the
	// attachment's stored Slug instead, so a link written with the wrong casing
	// still shows the repository the way its owner attached it.
	Slug string
	// Ref is the raw ?ref= value, "" when the link carried none. Empty means
	// "whatever the attachment says", which is not the same as any particular
	// branch name — see normalizeRef.
	Ref string
	// Env is the raw ?env= value after whitespace/case normalization. Its row,
	// ownership, readiness, and repository membership are resolved by ctlops.
	Env string
	// Fresh is `?new=1`: build a sandbox even though one that matches already
	// exists. It is the second button on the confirm screen and nothing else —
	// no badge mints it, and `ctl badge` never emits it — because the default
	// this door was built around is right nearly every time: clicking the same
	// link twice should land in the same box.
	//
	// It exists because "nearly every time" is not every time. The environment
	// this box was forked from has been rebuilt since; the disk has drifted
	// somewhere unrecoverable; two agents want the same branch. Before it, the
	// only way to say so was to leave the page, open a terminal, and create by
	// hand — which is the exact moment the door was supposed to remove.
	//
	// A GET carrying it still writes nothing: it suppresses the automatic
	// handoff and shows the confirm screen, which is a page with a button on
	// it, not a create. Only the POST creates, and only after that button.
	Fresh bool
}

// freshParam is the query parameter behind target.Fresh, and `1` is the only
// value that turns it on.
//
// Anything else — `true`, `yes`, an empty value — is IGNORED rather than
// refused, which is the same promise parseTarget makes about parameters it does
// not know at all: these URLs sit in comments that outlive the deployment, and
// a link must never 400 on a spelling. The only producer of this parameter is
// the form on this door's own confirm screen, so there is exactly one spelling
// to honour.
const freshParam = "new"

// candidate is one of the caller's existing sandboxes that holds this
// repository, paired with the branch it actually holds it on.
type candidate struct {
	Info ctlops.SandboxInfo
	// Ref is the EFFECTIVE ref: the value ReposForSandbox returned, which is
	// already COALESCE(NULLIF(o.ref,''), r.ref) — the sandbox's own override if
	// it has one, else the attachment's default. It is not the override row and
	// must never be rebuilt from one.
	Ref string
}

// parseTarget validates the path/ref grammar and normalizes the optional
// environment name. The environment itself is resolved under the caller by
// resolve; unknown parameters are ignored by never looking at them.
//
// Unknown query parameters are not read and not refused. That is a
// forward-compatibility promise rather than laxness: this URL goes into
// comments that outlive the deployment, so a host built years from now must
// still honour a link written today, and a link written today must not 400 on a
// host that learned a parameter after it was written.
func parseTarget(owner, repo, ref, env, fresh string) (target, *ctlops.Error) {
	slug := owner + "/" + repo
	// repos.ValidSlug, never a hand-rolled regexp. The two halves are not the
	// same grammar — the owner is a GitHub login and the name additionally
	// admits '.' and '_' and may lead with a digit — and rolling our own here is
	// exactly the mistake that rejects node.js. Asking the store that will hold
	// the answer also means a slug this door accepts is a slug that store would.
	// SplitSlug rather than ValidSlug, and the difference is not cosmetic.
	// ValidSlug answers about the TRIMMED form and hands back nothing, so
	// keeping the raw concatenation would let whitespace smuggled through the
	// path — %20, or %0A, which Go's ServeMux unescapes into the path value —
	// pass validation and then travel on into the attachment lookup, which
	// compares with strings.EqualFold and would never match the stored slug.
	// A visitor who genuinely has the repository attached would be shown the
	// "nothing of yours is attached" screen instead, from a link in a comment
	// nobody can edit; and the copy-pasteable `repo add` command on that screen
	// would carry the newline too. Rebuilding the slug from the two validated
	// halves means the value this function returns is the canonical one.
	owner, name, ok := repos.SplitSlug(slug)
	if !ok {
		return target{}, ctlops.Invalid(op, "bad_slug",
			"%q is not a repository name — a launch link ends in <owner>/<name>, like wandb/hivemind.", slug)
	}
	slug = owner + "/" + name
	// An empty ref is not an invalid ref: it is the link saying nothing about a
	// branch, which is the common and preferred form. repos.ValidRef refuses ""
	// because to the store an empty string is simply not a ref, so the
	// emptiness check has to happen out here, at the one place that knows "" is
	// meaningful.
	//
	// A non-empty one is checked by repos.ValidRef and by nothing else. Its
	// leading-alphanumeric rule is the load-bearing half: this value reaches the
	// guest as the argument of `git clone --branch <ref>`, where a leading '-'
	// is an option and not a branch, and its separate ".." refusal is a Go-side
	// check the regexp cannot express. A caller that reached for the regexp
	// alone would enforce exactly half the rule and believe it had all of it.
	if ref != "" && !repos.ValidRef(ref) {
		return target{}, ctlops.Invalid(op, "bad_ref",
			"%q is not a branch or tag name — it has to start with a letter or a digit, and it cannot contain \"..\".", ref)
	}
	return target{
		Slug:  slug,
		Ref:   ref,
		Env:   strings.ToLower(strings.TrimSpace(env)),
		Fresh: fresh == "1",
	}, nil
}

// fresh returns this target with the create-a-new-one flag set, so the confirm
// screen can build the second button's action out of the first one's target
// rather than reassembling a URL beside it.
func (t target) fresh() target {
	t.Fresh = true
	return t
}

// normalizeRef folds a ref that is merely the attachment's own default down to
// "", so that "say nothing" and "say the default out loud" compare equal.
//
// This is the whole trick of the feature, and it is applied to BOTH sides of
// every comparison — the branch a link asks for and the branch a sandbox is
// already on — which is the only way it can be consistent.
//
// The naive rule, comparing the sandbox's effective ref to the link's raw ref,
// is wrong, and wrong in the direction that hurts. ReposForSandbox selects
//
//	COALESCE(NULLIF(o.ref, ''), r.ref)
//
// (internal/repos/store.go:399-411), so a
// sandbox with no per-instance override, on an attachment created as
// `repo add wandb/hivemind --ref main`, reports its effective ref as "main" and
// not as "". A badge with no ?ref= at all would therefore fail to match that
// sandbox and would create a duplicate on every single click — the exact
// failure this function exists to prevent, and the reason the match table in
// resolve_test.go has a row for it.
//
// The symmetry also does the emitting half's job: `ctl badge` runs the same
// fold before it prints a link, so a ?ref= equal to the attachment's own
// default is never minted in the first place.
//
// One case remains that no fold can close: an attachment whose Ref is ""
// (meaning "the repository's default branch, whatever GitHub says it is") does
// not match a link that spells that branch out as ?ref=main, because nothing in
// this tree knows what a repository's default branch is called. Resolving it
// would take a GitHub App round trip on the click path. The mitigation is at
// both ends instead — `ctl badge` prefers to omit the ref, and the confirm page
// lists the caller's near-miss sandboxes so a human can land on one directly.
func normalizeRef(attRef, x string) string {
	if attRef != "" && x == attRef {
		return ""
	}
	return x
}

// findRepo picks the caller's attachment for slug out of a list, case-folded.
//
// EqualFold rather than == because the store's slug columns are COLLATE NOCASE
// (internal/repos/store.go:194 and :213), so `WANDB/Hivemind` and
// `wandb/hivemind` are one row there and must be one row here too — otherwise a
// link written with different casing than the attachment would read as "never
// attached" while the store happily holds it.
//
// It is restated here rather than imported because ctlops has its own copy and
// that copy is package-private (internal/ctlops/reporef.go:178). Two small
// functions with one rule between them is the lesser evil against exporting a
// helper from a package whose whole design is a narrow surface.
func findRepo(list []repos.Repo, slug string) (repos.Repo, bool) {
	for _, r := range list {
		if strings.EqualFold(r.Slug, slug) {
			return r, true
		}
	}
	return repos.Repo{}, false
}

// environmentLister is the optional half of Sandboxes: the control plane's
// answer to "which of these tags are environments, and when did they last
// change".
//
// It is taken by ASSERTION off h.ops rather than added to Sandboxes, the same
// shape and the same reasoning as envAwaiter (create.go). Sandboxes is narrow
// on purpose, *ctlops.Ops always has this method, and a test fake that does not
// is a fake with no environments to choose between — so the fallback below is
// the honest behaviour for it rather than a capability check anybody deploys.
type environmentLister interface {
	EnvironmentsForTags(c ctlops.Caller, tags []string) ([]ctlops.EnvironmentInfo, error)
}

// tagChoice is the tag set a launch link will create with, and the reasoning
// the confirm page renders next to it.
type tagChoice struct {
	// Tags is what Create is handed. It is att.Tags with at most one
	// environment left in it.
	Tags []string
	// Chosen is the environment that survived, "" when no choice was made —
	// either because none of the tags is an environment, or because only one
	// is. It is the ordinary case and the page says nothing about it.
	Chosen string
	// Dropped are the environments left off, so the page can name them. A
	// visitor who lands in `web` when they were thinking of `ci` needs to be
	// told, and this is the only screen that can tell them.
	Dropped []string
}

// launchTags decides which of an attachment's tags a create should carry.
//
// THE PROBLEM IT SOLVES. A repository attachment can carry several tags and a
// sandbox has exactly one rootfs, so when two of those tags are environments
// with different base images, ctlops.resolveTemplate refuses the create as
// `template_ambiguous` and says "create it with only one of those tags" — sound
// advice for somebody at a terminal, and useless to somebody who clicked a link
// in a comment. A repository attached to two built environments had a
// permanently dead launch link.
//
// THE RULE. Every tag that is not an environment rides along untouched; among
// the ones that are, the most recently updated wins. That is the environment
// its owner was last working in, which is the best available guess at the one
// they meant, and it is a guess that the confirm page shows and their other
// sandboxes escape.
//
// NEVER FATAL. A host whose ops predates this method, or a store that stumbles,
// falls back to the whole tag set — which is exactly today's behaviour, up to
// and including the 409 — because a launch door that refused to open on a
// degraded read would be worse than one that occasionally asks the visitor to
// pick. Same discipline as alsoClones and awaitEnv.
func (h *Handler) launchTags(c ctlops.Caller, att repos.Repo) tagChoice {
	all := slices.Clone(att.Tags)
	lister, ok := h.ops.(environmentLister)
	if !ok {
		return tagChoice{Tags: all}
	}
	list, err := lister.EnvironmentsForTags(c, att.Tags)
	if err != nil {
		h.log.Warn("launch could not read the environments on an attachment's tags",
			"user", c.Handle, "slug", att.Slug, "err", err)
		return tagChoice{Tags: all}
	}
	return pickTags(all, list)
}

// pickTags is launchTags' whole decision, separated from the read so it can be
// tabled.
//
// It does NOT rely on the order it is handed. EnvironmentsForTags already sorts
// newest-first, and depending on that here would make this function's answer a
// property of two places instead of one — so the winner is computed again,
// with the same rule: latest UpdatedAt, name ascending to break a tie.
//
// The tiebreak is load-bearing rather than tidy. Two environments saved in the
// same write second is an ordinary thing, and without a total order the same
// link would open a different sandbox on alternate clicks — the exact
// duplicate-per-click failure normalizeRef exists to prevent, arriving by
// another road.
func pickTags(tags []string, list []ctlops.EnvironmentInfo) tagChoice {
	if len(list) < 2 {
		return tagChoice{Tags: tags}
	}
	best := list[0]
	for _, e := range list[1:] {
		switch {
		case e.UpdatedAt.After(best.UpdatedAt):
			best = e
		case e.UpdatedAt.Equal(best.UpdatedAt) && e.Name < best.Name:
			best = e
		}
	}
	drop := make(map[string]bool, len(list))
	var dropped []string
	for _, e := range list {
		if e.Name != best.Name {
			drop[e.Name] = true
			dropped = append(dropped, e.Name)
		}
	}
	sort.Strings(dropped)
	out := make([]string, 0, len(tags))
	for _, t := range tags {
		if !drop[t] {
			out = append(out, t)
		}
	}
	return tagChoice{Tags: out, Chosen: best.Name, Dropped: dropped}
}

// resolve answers the only question a click asks: does this person already have
// a sandbox holding this repository on this branch, and if not, what do we need
// to tell them before offering to build one.
//
// It is reads and no writes, and that is a contract rather than an
// implementation detail. A link in a public comment is fetched by scanners,
// previewers and prefetchers nobody consented to, so the GET behind it must not
// create a sandbox, wake a paused one, write a tag or a ref row, or consume a
// quota slot.
//
// The caller comes from the verified edgeauth session and never from the URL.
// Every store call below is scoped to c.Handle, and the two *ForSandbox queries
// carry the owner on both sides of their joins inside the SQL itself — see the
// comment on repos.ReposForSandbox for what leaks when a join loses its owner
// term, which is a private repository's name and then a credential minted for
// it.
//
// The returns are: the matched attachment (the display authority for the slug
// and the ordinary path's source of tags), the best existing sandbox to
// hand the visitor back if there is one, and every OTHER sandbox of theirs on
// this repository — which the confirm page renders as the human escape hatch
// for the one branch case no fold can decide.
func (h *Handler) resolve(ctx context.Context, c ctlops.Caller, t target) (repos.Repo, *candidate, []candidate, error) {
	list, err := h.repos.ListRepos(c.Handle)
	if err != nil {
		return repos.Repo{}, nil, nil, ctlops.Fail(op, err)
	}
	att, ok := findRepo(list, t.Slug)
	if !ok {
		// The most-visited state this door will ever have: a first-time visitor
		// arriving from a public comment, who has an account and no attachment.
		// It is a distinct code because it is a distinct page — there is nothing
		// for a sandbox to check out, so offering a create button would build a
		// box with an empty working tree.
		return repos.Repo{}, nil, nil, ctlops.Invalid(op, codeNoAttachment,
			"nothing of yours is attached to %s, so there is nothing for a sandbox to check out.", t.Slug)
	}
	// An attachment carrying NO tag is attached to nothing, and this door is the
	// one place where that reads as a working configuration right up until it
	// silently is not.
	//
	// internal/repos deliberately does not stamp `default` on an untagged
	// attachment the way internal/secrets does on an untagged secret — its own
	// comment says why: an untagged secret that lands in every sandbox is an
	// environment variable, and an untagged repository that lands in every
	// sandbox is a checkout nobody asked for in a home directory they are using.
	// The consequence lands HERE. The tags a create carries come only from this
	// attachment, so a tagless one produces a sandbox tagged `default` alone,
	// which does not select the attachment, so ReposForSandbox returns no row
	// and the repository is never cloned. SandboxesForRepo then cannot find that
	// box either, so the next click does not reuse it — it builds another one,
	// and another, until --max-running-per-owner stops the visitor with a
	// message about a limit rather than about the real problem.
	//
	// Refusing here, before the confirm page is ever composed, is the only
	// honest answer: the page's whole promise is "this will be cloned", and
	// there is no tag by which it could be. It is its own code and its own
	// screen because the remedy is different from the no-attachment one — the
	// attachment exists and needs a tag, rather than not existing.
	if len(att.Tags) == 0 {
		return att, nil, nil, ctlops.Invalid(op, codeUntaggedAttachment,
			"%s is attached but carries no tag, so no sandbox will ever clone it.", att.Slug)
	}
	if t.Env != "" {
		if h.envs == nil {
			return att, nil, nil, ctlops.Disabled(op,
				"this host does not support environment launch links.")
		}
		env, err := h.envs.GetEnvironment(c, t.Env)
		if err != nil {
			return att, nil, nil, err
		}
		if env.State != string(envs.StateReady) {
			return att, nil, nil, &ctlops.Error{
				Kind: ctlops.KindConflict, Op: op, Code: codeEnvNotReady,
				Msg: "environment " + env.Name + " is not ready yet; finish its build before launching it.",
			}
		}
		if !containsFold(att.Tags, env.Name) {
			return att, nil, nil, ctlops.Invalid(op, codeEnvRepoMismatch,
				"%s is not attached to environment %s.", att.Slug, env.Name)
		}
		// Keep the canonical environment name returned by ctlops. envName folds
		// case and whitespace there too, and Create must receive the same name.
		t.Env = env.Name
	}

	names, err := h.repos.SandboxesForRepo(c.Handle, gitHubHost, att.Slug)
	if err != nil {
		return att, nil, nil, ctlops.Fail(op, err)
	}
	if len(names) == 0 {
		// Attached, but no sandbox of theirs carries a tag that selects it.
		// That is a create, not a refusal, and it is why the not-attached case
		// above had to be told apart from this one: SandboxesForRepo answers nil
		// for both.
		return att, nil, nil, nil
	}

	// One List for the whole set. internal/repos has no state column and no
	// join to the host ledger, so state, reachability, last-active and the
	// terminal URL can only come from here — and asking once and indexing beats
	// a Get per name, which on a fleet is a round trip per name.
	boxes, err := h.ops.List(ctx, c)
	if err != nil {
		return att, nil, nil, ctlops.Fail(op, err)
	}
	byName := make(map[string]ctlops.SandboxInfo, len(boxes))
	for _, b := range boxes {
		byName[b.Name] = b
	}

	want := normalizeRef(att.Ref, t.Ref)
	var matches, all []candidate
	for _, name := range names {
		info, live := byName[name]
		if !live {
			// A tag row survives its sandbox in a window the manager closes
			// asynchronously, and List is the ledger. A name with no record is
			// a box we cannot redirect anybody to.
			continue
		}
		if t.Env != "" && !containsFold(info.Tags, t.Env) {
			continue
		}
		manifest, err := h.repos.ReposForSandbox(name, c.Handle)
		if err != nil {
			return att, nil, nil, ctlops.Fail(op, err)
		}
		// The manifest entry, not an override row. Its Ref is already the
		// effective one, and it is the same query internal/metadata builds the
		// guest's checkout list from — so a match here means a match against
		// the branch that box will actually be sitting on, decided by one
		// authority rather than reconstructed by a second.
		entry, held := findRepo(manifest, att.Slug)
		if !held {
			continue
		}
		cand := candidate{Info: info, Ref: entry.Ref}
		all = append(all, cand)
		if normalizeRef(att.Ref, entry.Ref) == want {
			matches = append(matches, cand)
		}
	}

	var best *candidate
	if ranked := rank(reachableOnly(matches)); len(ranked) > 0 {
		top := ranked[0]
		best = &top
	}

	// others is every box of theirs on this repository except the one we are
	// about to hand them, ranked the same way. Unreachable boxes are NOT
	// dropped here the way they are from the match set: this list is read by a
	// human deciding what to do, and "you have one on feat/y, on a node that
	// is not answering" is information, whereas a redirect to it would be a
	// hang.
	others := make([]candidate, 0, len(all))
	for _, cand := range rank(all) {
		if best != nil && cand.Info.Name == best.Info.Name {
			continue
		}
		others = append(others, cand)
	}
	if len(others) == 0 {
		others = nil
	}
	return att, best, others, nil
}

func containsFold(values []string, want string) bool {
	for _, value := range values {
		if strings.EqualFold(value, want) {
			return true
		}
	}
	return false
}

// reachableOnly drops sandboxes whose node is not answering the control plane,
// unless every candidate is in that state.
//
// A redirect is a promise that the destination works. <name>-xterm resolves
// through the same control plane, so handing someone an unreachable box means a
// terminal that hangs rather than one that boots — and the visitor has no way
// to tell that from a slow start. When one is reachable it is strictly the
// better answer; when none is, the unreachable box is still their sandbox and
// still the right destination, because creating a second one would leave them
// with two the moment the node comes back.
func reachableOnly(in []candidate) []candidate {
	var reachable []candidate
	for _, c := range in {
		if !c.Info.Unreachable {
			reachable = append(reachable, c)
		}
	}
	if len(reachable) == 0 {
		return in
	}
	return reachable
}

// rank orders candidates best-first: a running box before a paused one before
// an archived one, and within a state the most recently active first.
//
// The order is the visitor's waiting time, in order. A running sandbox is a
// WebSocket away. A paused one is a warm restore. An archived one is a download
// from object storage and a cold boot, which internal/xterm budgets fifteen
// minutes for — so it is last even though it is just as much theirs. Last-active
// breaks the tie because the box somebody touched an hour ago is the one they
// meant, and the name breaks that tie so the same inputs always produce the
// same redirect rather than a different sandbox on every click.
//
// sort.SliceStable rather than sort.Slice for the same reason: two boxes that
// tie on every key keep the order SandboxesForRepo returned them in, which is
// itself sorted, instead of whatever the sort happened to do this time.
func rank(in []candidate) []candidate {
	out := make([]candidate, len(in))
	copy(out, in)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i].Info, out[j].Info
		if a.Unreachable != b.Unreachable {
			return !a.Unreachable
		}
		if ra, rb := stateRank(a.State), stateRank(b.State); ra != rb {
			return ra < rb
		}
		if !a.LastActive.Equal(b.LastActive) {
			return a.LastActive.After(b.LastActive)
		}
		return a.Name < b.Name
	})
	return out
}

// stateRank turns a sandbox state into how long the visitor will wait for it.
//
// The states are quoted from internal/vmm rather than written out as string
// literals, because vmm is where they are defined and a renamed state would
// then be a compile error here instead of a sandbox that silently ranks below
// an archived one. An unrecognised state sorts last for that same reason: a
// state this function has not been taught about is not one to redirect somebody
// into.
func stateRank(state string) int {
	switch state {
	case string(vmm.StateRunning):
		return 0
	case string(vmm.StatePaused):
		return 1
	case string(vmm.StateArchived):
		return 2
	default:
		return 3
	}
}
