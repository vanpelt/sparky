package ctlops

// The owner-scoped repo verbs: attaching a GitHub repository to a tag so that
// every sandbox carrying that tag arrives with the checkout already in it.
//
// The shape is deliberately the secrets shape, because it is the same object
// seen a third time: an owner's thing, selected by a tag, meeting the sandboxes
// that carry it. What is different is what a mistake costs. A secret that
// reaches no sandbox is an inconvenience the user notices when an agent asks
// them to log in; a repo attached to `default` reaches EVERY sandbox they make
// from then on and clones into a home directory somebody is using. So this
// layer defaults the empty tag set the way PutSecret does — the two halves of
// the default have to meet or the feature does not work — and hands the caller
// a flag saying it did, because the transports are required to say so out loud
// at attach time. See docs/github-repos-design.md §2.2.
//
// Nothing here mints a credential. The token that clones a private repository
// is minted in internal/metadata, for a guest that identified itself on a
// channel it cannot forge, and it is never stored. What this file can do is ask
// the App whether it could mint one — which is the whole of `repo check`, and
// the reason `repo add` asks the same question inline: the failure mode of the
// design is "the App is not installed on that repository", and without the
// inline check it stays invisible until a clone fails inside a VM at boot, in a
// log nobody is watching.

import (
	"context"
	"errors"
	"strings"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ghapp"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/repos"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/secrets"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/users"
)

// RepoArgs is one attachment as the caller stated it. Host is empty for
// github.com, which is the only host the store takes today.
type RepoArgs struct {
	Slug  string   `json:"slug"`
	Host  string   `json:"host,omitempty"`
	Ref   string   `json:"ref,omitempty"`
	Path  string   `json:"path,omitempty"`
	Write bool     `json:"write,omitempty"`
	Tags  []string `json:"tags,omitempty"`
}

// RepoResult is what an attach reports back: the row as it was stored, the
// sandboxes that now select it, and the two things the caller cannot see for
// themselves.
//
// Defaulted is the first: an attachment with no tag went on `default`, which
// every new sandbox of theirs carries, and a transport that does not say so has
// silently signed them up to clone this into everything they create. Check is
// the second — see checkRepo.
type RepoResult struct {
	Repo      repos.Repo `json:"repo"`
	Sandboxes []string   `json:"sandboxes"` // never nil
	Defaulted bool       `json:"defaulted"`
	Check     RepoCheck  `json:"check"`
	// Notes is what could not be said in Sandboxes: one line per already-running
	// sandbox that selects this attachment and could not be nudged into checking
	// it out. Empty is the normal case — a box either took the job or was not
	// running, and neither is worth a line.
	Notes []string `json:"notes,omitempty"`
}

// RepoCheck is one attachment's answer to "can the App actually reach this".
//
// Checked and Reachable are two bits rather than one because they fail
// differently and are fixed differently. Checked=false means this host has no
// App at all, so only public repositories will clone and nothing is wrong with
// the attachment; Reachable=false means there IS an App and it cannot reach
// this repository, which is a thing the user can go and fix — usually by
// installing it, which is what InstallURL is for.
type RepoCheck struct {
	Host       string   `json:"host"`
	Slug       string   `json:"slug"`
	Tags       []string `json:"tags"`
	Access     string   `json:"access"`
	Checked    bool     `json:"checked"`
	Reachable  bool     `json:"reachable"`
	Reason     string   `json:"reason,omitempty"`
	InstallURL string   `json:"install_url,omitempty"`
	// Missing names the permissions this host would mint for the repository
	// that the installation was never granted, sorted. It is never a Reason:
	// the attachment is reachable and clones fine without any of them.
	//
	// It is here because the alternative is silence. A minted token is narrowed
	// to what the installation holds before it is requested — necessarily, since
	// GitHub refuses a request naming a permission it lacks outright rather than
	// trimming it — and a permission the operator never granted is dropped in
	// exactly the same way as one whose name this code spells wrong. Both
	// produce a working, quieter token and a 403 inside a sandbox hours later.
	Missing []string `json:"missing_permissions,omitempty"`
}

// ListRepos returns the caller's attachments. Reading is not gated on the
// GitHub link: what is listed is configuration this account wrote, and a user
// whose link has gone stale still needs to be able to see what is attached in
// order to detach it.
func (o *Ops) ListRepos(c Caller) ([]repos.Repo, error) {
	const op = "repo.list"
	if o.repos == nil {
		return nil, Disabled(op, "repo attachments are not enabled on this host")
	}
	list, err := o.repos.ListRepos(c.Handle)
	if err != nil {
		return nil, Fail(op, err)
	}
	if list == nil {
		list = []repos.Repo{}
	}
	return list, nil
}

// AttachRepo records one attachment and reports whether the App can reach it.
//
// The check runs AFTER the write, and its failure is never the command's
// failure. That ordering is the design's (Part 6): a repository the App cannot
// see yet is a normal state — the user is about to go and install it — and
// refusing the write would leave them with nothing recorded and a URL to visit,
// after which they would have to remember to come back and type this again. So
// the attachment lands, the sentence says what is missing, and `repo check`
// says it again later. The one thing that must not happen is silence.
func (o *Ops) AttachRepo(ctx context.Context, c Caller, a RepoArgs) (RepoResult, error) {
	const op = "repo.add"
	if o.repos == nil {
		return RepoResult{}, Disabled(op, "repo attachments are not enabled on this host")
	}
	slug := strings.TrimSpace(a.Slug)
	if slug == "" {
		return RepoResult{}, Invalid(op, "missing_slug", "a repository is required, as <owner>/<name>")
	}
	if !repos.ValidSlug(slug) {
		return RepoResult{}, Invalid(op, "bad_slug",
			"%q is not an owner/name repository — write it the way github.com does, e.g. wandb/hivemind", a.Slug)
	}
	want, err := NormalizeTags(a.Tags)
	if err != nil {
		return RepoResult{}, Invalid(op, "bad_tag", "%v", err)
	}
	// The store does not stamp the default tag the way PutSecret does, and it
	// is right not to: it cannot print the warning that has to come with it.
	// The choice is made here, once, and reported in Defaulted so that every
	// transport can say it.
	defaulted := len(want) == 0
	if defaulted {
		want = []string{secrets.DefaultTag}
	}
	access := repos.AccessRead
	if a.Write {
		access = repos.AccessWrite
	}
	u, err := o.attachIdentity(op, c)
	if err != nil {
		return RepoResult{}, err
	}

	r := repos.Repo{Host: a.Host, Slug: slug, Ref: a.Ref, Path: a.Path, Access: access}
	// The store's sentences about a slug, a ref, a path or a host it cannot
	// take are already user-facing and more specific than anything this layer
	// could say about the same argument, so they pass through verbatim.
	if err := o.repos.PutRepo(c.Handle, r, want); err != nil {
		return RepoResult{}, verbatim(Invalid(op, "bad_repo", "%v", err))
	}
	// Read back what was actually stored — the store normalizes the host, folds
	// the slug's case for lookups and keeps created_at across an update — so the
	// caller is shown the row rather than their own arguments echoed at them.
	r = o.storedRepo(c.Handle, r, want)

	affected, err := o.repos.SandboxesForRepo(c.Handle, r.Host, r.Slug)
	if err != nil {
		// The attachment is written. Failing the command on the fan-out query
		// would tell the user it did not save, which is false and would have
		// them run it again.
		o.log.Warn("could not resolve sandboxes for repo", "user", c.Handle, "slug", r.Slug, "err", err)
		affected = nil
	}
	if affected == nil {
		affected = []string{}
	}
	// An upstream failure is not this command's failure either: github.com not
	// answering says nothing about the attachment, and the check is repeatable.
	check, _ := o.checkRepo(ctx, u, r)

	// The boxes that already carry one of these tags are running RIGHT NOW with
	// a manifest that just changed under them. Attaching is the same event as
	// retagging seen from the other side — one changes which repos a tag names,
	// the other which tags a box has, and both end with a guest whose checkouts
	// no longer match what its owner asked for. Only the boot pass used to
	// reconcile that, which meant an attachment made after a box started did
	// nothing visible until somebody restarted it.
	notes := o.syncReposFanout(ctx, affected)

	o.log.Info("repo attached", "user", c.Handle, "slug", r.Slug, "tags", want,
		"access", r.Access, "sandboxes", len(affected), "reachable", check.Reachable)
	return RepoResult{Repo: r, Sandboxes: affected, Defaulted: defaulted, Check: check, Notes: notes}, nil
}

// DetachRepo removes one attachment and names the sandboxes that were selecting
// it.
//
// The fan-out is computed BEFORE the delete for DeleteSecret's reason:
// afterwards the row is gone and nothing can say which boxes used to select it.
// Unlike a secret, nothing is stripped from those guests — a clone is a
// directory somebody may be working in, and deleting it out from under them
// would be a far bigger action than the one they asked for. Detaching stops the
// repository being cloned into anything NEW, and the names are how the caller
// finds the checkouts already made.
func (o *Ops) DetachRepo(ctx context.Context, c Caller, host, slug string) ([]string, error) {
	const op = "repo.rm"
	if o.repos == nil {
		return nil, Disabled(op, "repo attachments are not enabled on this host")
	}
	if strings.TrimSpace(slug) == "" {
		return nil, Invalid(op, "missing_slug", "a repository is required, as <owner>/<name>")
	}
	affected, err := o.repos.SandboxesForRepo(c.Handle, host, slug)
	if err != nil {
		if errors.Is(err, repos.ErrInvalidRepo) {
			return nil, verbatim(Invalid(op, "bad_repo", "%v", err))
		}
		return nil, Fail(op, err)
	}
	if err := o.repos.DeleteRepo(c.Handle, host, slug); err != nil {
		// "not attached" and "attached by somebody else" are the same answer
		// from the same line, as they are for every other object here: the
		// store's query is owner-scoped, so a stranger's slug simply is not
		// found, and NotFound is what says so without confirming it exists.
		if errors.Is(err, repos.ErrNoSuchRepo) {
			return nil, NotFound(op, "repo", slug)
		}
		return nil, verbatim(Invalid(op, "bad_repo", "%v", err))
	}
	if affected == nil {
		affected = []string{}
	}
	o.log.Info("repo detached", "user", c.Handle, "slug", slug, "sandboxes", len(affected))
	return affected, nil
}

// CheckRepos reports, per attachment, whether the App can actually reach it.
//
// It earns its place because every other surface reports success. The store
// took the row, `repo ls` prints it, the manifest carries it — and none of that
// knows whether the App was ever installed on the repository, which is the one
// thing that decides whether the clone works. Without this verb that answer
// arrives inside a VM, at boot, in a log.
func (o *Ops) CheckRepos(ctx context.Context, c Caller) ([]RepoCheck, error) {
	const op = "repo.check"
	if o.repos == nil {
		return nil, Disabled(op, "repo attachments are not enabled on this host")
	}
	if o.ghApp == nil {
		return nil, Disabled(op, "no GitHub App is configured on this host")
	}
	list, err := o.repos.ListRepos(c.Handle)
	if err != nil {
		return nil, Fail(op, err)
	}
	u, err := o.accounts.Get(c.Handle)
	if err != nil {
		return nil, Fail(op, err)
	}
	out := make([]RepoCheck, 0, len(list))
	for _, r := range list {
		check, upstream := o.checkRepo(ctx, u, r)
		if upstream != nil {
			// github.com did not answer, so every remaining row would be
			// reported unreachable on no evidence at all — which reads as "your
			// attachments are broken" when the true answer is "ask again in a
			// minute". One honest 502 beats a page of confident wrong lines.
			return nil, &Error{
				Kind: KindUpstream, Op: op, Code: "github_unreachable",
				Msg: upstream.Error(), Verbatim: true, Err: upstream,
			}
		}
		out = append(out, check)
	}
	return out, nil
}

// GitHubInstallURL is where the caller installs this host's App. It is a verb
// rather than a line of documentation because the URL is derived from the App
// this host actually holds, and a URL somebody copied out of a runbook installs
// somebody else's App.
func (o *Ops) GitHubInstallURL(c Caller) (string, error) {
	const op = "github.install"
	if o.ghApp == nil {
		return "", Disabled(op, "no GitHub App is configured on this host")
	}
	return o.ghApp.InstallURL(), nil
}

// attachIdentity resolves the GitHub identity an attachment will be checked and
// later minted against, refusing a caller whose link cannot carry one.
//
// This is the gate docs/github-repos-design.md §3.3 requires, and it is the
// same gate `keys import-github` applies for the same reason. A repo attachment
// is the record that decides which installation gets used on this handle's
// behalf, and an installation token reads private source. So the evidence that
// this handle IS github.com/<login> has to have come from GitHub itself, about
// the person holding THIS account: a published key they control, or a device
// flow they completed. A link recorded `assertion` is a third party's word for
// it, and whoever holds that third party's key speaks for every user — which is
// exactly the channel that must not reach a verb that grants access.
//
// GitHubVerifiedAt is checked beside the login because the two are written
// together by every path that links one: a row with a login and no timestamp is
// not a link this can reason about, and reading it as one would let a partially
// written row stand in for proof.
func (o *Ops) attachIdentity(op string, c Caller) (users.User, error) {
	u, err := o.accounts.Get(c.Handle)
	if err != nil {
		return users.User{}, Fail(op, err)
	}
	if err := AttachGate(op, u); err != nil {
		return users.User{}, err
	}
	return u, nil
}

// AttachGate is the rule that decides whether an account may attach a
// repository, exported because there is more than one door onto that verb —
// ctl, the REST API and the user console — and a rule copied into three places
// is a rule that will hold in two of them.
//
// The reasoning is the one docs/github-linking-design.md §2.3 gives for key
// adoption and §3.3 restates for repos: a link established by a channel that
// could be wrong about which human is on the other end must not reach a verb
// that grants access to somebody's source code. That is why an `assertion`
// link is refused here while still being perfectly displayable elsewhere.
//
// It is defence in depth rather than the last line — every credential mint
// re-checks this against the sandbox's owner — but an attachment the platform
// will never honour is a promise it cannot keep, and attaching also widens the
// tag's effective egress through the netrules overlay.
func AttachGate(op string, u users.User) error {
	if u.GitHubLogin == "" || u.GitHubVerifiedAt == nil {
		return &Error{
			Kind: KindConflict, Op: op, Code: "github_not_linked",
			Msg:      "no GitHub account linked, so there is nobody to clone this as",
			Hint:     "link one with: github link",
			Verbatim: true,
		}
	}
	if !users.StrongGitHubLink(u.GitHubVia) {
		return &Error{
			Kind: KindDenied, Op: op, Code: "github_link_too_weak",
			Msg: "the link to github.com/" + u.GitHubLogin +
				" was not proved directly with GitHub, so it cannot attach repos",
			Hint:     "re-link with `github link` (or `keys verify-github`) and run this again.",
			Verbatim: true,
		}
	}
	return nil
}

// checkRepo asks the App whether it could serve this attachment for this user:
// is it installed on the repository, and may this account's GitHub identity use
// that installation.
//
// The second error return is non-nil only when github.com itself did not answer
// — the one failure that says nothing about the attachment. Everything else
// (not installed, not a member, the App missing a permission) is a fact about
// this repository and belongs in the row as a Reason the user can read, not in
// an error that would replace every row with one sentence.
//
// The Authorize call is the security boundary and it is deliberately made here,
// where nothing is minted, as well as at mint time: this is the check saying
// "you would not be able to use this", early enough to be useful, and it must
// never be mistaken for the check that hands the token out.
func (o *Ops) checkRepo(ctx context.Context, u users.User, r repos.Repo) (RepoCheck, error) {
	check := RepoCheck{Host: r.Host, Slug: r.Slug, Tags: r.Tags, Access: r.Access}
	if o.ghApp == nil {
		// Checked stays false: this host has no App, which is a statement about
		// the host and not about the repository.
		return check, nil
	}
	check.Checked = true
	owner, name, ok := repos.SplitSlug(r.Slug)
	if !ok {
		// A stored slug that no longer parses is a store the code got ahead of;
		// report it on the row rather than reaching github.com with nonsense.
		check.Reason = "this attachment's slug is not an owner/name repository"
		return check, nil
	}
	inst, err := o.ghApp.InstallationFor(ctx, owner, name)
	if err != nil {
		if errors.Is(err, ghapp.ErrUpstream) {
			return check, err
		}
		check.Reason = err.Error()
		if errors.Is(err, ghapp.ErrNotInstalled) {
			check.InstallURL = o.ghApp.InstallURL()
		}
		return check, nil
	}
	if err := o.ghApp.Authorize(ctx, inst, u.GitHubID, u.GitHubLogin); err != nil {
		if errors.Is(err, ghapp.ErrUpstream) {
			return check, err
		}
		check.Reason = err.Error()
		return check, nil
	}
	check.Reachable = true
	// Measured against the write set whatever the attachment says, because the
	// question is which permission NAMES the App is short of; Missing does not
	// compare levels, and a read attachment on a read-granted App is short of
	// nothing.
	check.Missing = ghapp.Missing(inst.Permissions, ghapp.MintPermissions(ghapp.PermWrite))
	return check, nil
}

// storedRepo re-reads the row a write just produced, falling back to the
// arguments when the read fails.
//
// It exists because the store normalizes more than a caller can predict — an
// empty host becomes github.com, created_at survives an update, an id is
// assigned — and a result built from the request would show the user what they
// typed rather than what is now true. The fallback keeps a read failure from
// turning a successful write into an error: the attachment is stored either
// way, and the worst case is a result that is slightly less canonical than the
// row behind it.
func (o *Ops) storedRepo(owner string, want repos.Repo, tags []string) repos.Repo {
	list, err := o.repos.ListRepos(owner)
	if err == nil {
		for _, r := range list {
			// The store compares slugs case-insensitively (github.com does), so
			// this has to as well or a case correction reads as a second repo.
			if !strings.EqualFold(r.Slug, want.Slug) {
				continue
			}
			if want.Host == "" || strings.EqualFold(r.Host, want.Host) {
				return r
			}
		}
	}
	want.Owner = owner
	want.Tags = tags
	if want.Host == "" {
		want.Host = defaultRepoHost
	}
	return want
}

// defaultRepoHost is what an empty host means, restated here only for the
// fallback above. internal/repos owns the real default (normHost) and is the
// only thing that writes it; if a second host ever becomes storable, this
// constant is a place that has to be found, which is why it is named rather
// than inlined.
const defaultRepoHost = "github.com"
