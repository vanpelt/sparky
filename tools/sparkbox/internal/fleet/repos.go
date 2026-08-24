package fleet

// Repo attachments and GitHub credentials for a sandbox on another machine.
//
// This is the third node -> gateway capability, and it is here for both of the
// reasons the first two are, at once.
//
// The credential half is identity.go's argument verbatim with one word changed:
// the GitHub App private key mints installation tokens for every repository
// anybody has granted the app, it lives on the gateway, and it must not be
// copied to a machine that merely runs VMs. The manifest half is selfservice.go's
// argument: a repo attachment hangs off a TAG, tags are per-owner, and the owner
// is a ledger column. No node holds either table, so there is nothing on a node
// to resolve a manifest from and nothing to fall back to when the link is down —
// which is the correct shape, because a cached, node-resolved repo list is
// exactly the node-asserted repo list that Part 3.5 of the design forbids.
//
// # What a node may say
//
// A sandbox NAME, and — for a credential — a repository SLUG. That is the whole
// request. The owner comes from Fleet.Get, which runs every remote record
// through serve() and stamps the ledger's owner over whatever the machine
// reported; if it did not, a node would be choosing whose private repositories
// its guests are handed. The slug is not an authorization input either: it
// narrows a token that would otherwise have to cover everything the sandbox may
// reach, and it is checked against the attachment set this gateway resolved for
// itself before it reaches the minter.
//
// The check that carries the weight is selfServiceBox, and it is the same one,
// in the same position, as in the other two files: the ledger must place that
// sandbox on the machine that asked. Without it, any linked machine reads any
// user's repositories by guessing a sandbox name — and unlike a forged id token,
// which at least expires into a fleet that can rotate its issuer, a leaked
// installation token is read access to somebody's source code for an hour.

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/nodelink"
)

// Repos resolves what a sandbox should have checked out, and mints the
// credential for one of those repositories. It is the gateway's own path,
// reached from here on behalf of a machine that holds neither the tags nor the
// App key.
//
// It takes the sandbox record rather than its name for the reason
// metadata.Identity does: the implementation needs the owner, and the record in
// hand already carries the LEDGER's owner. Handing it a name would mean a second
// lookup that could resolve differently.
//
// Satisfied on a gateway by an adapter over the same metadata.LocalRepos that
// serves this machine's own guests. One resolver for both, deliberately: a
// sandbox must get the same manifest whichever machine it happens to be on.
type Repos interface {
	Manifest(ctx context.Context, box *host.Sandbox) ([]RepoAttachment, error)
	Credential(ctx context.Context, box *host.Sandbox, slug string) (RepoCredential, error)
}

// RepoAttachment is one repository a sandbox should hold. Its fields restate
// internal/repos' columns rather than importing them, so this package stays
// free of the store the way it is free of the OIDC issuer.
type RepoAttachment struct {
	Host   string
	Slug   string
	Ref    string
	Path   string
	Access string
}

// RepoCredential is what git is told, and it is a secret with an expiry. It is
// never logged, never cached here, and never recorded in the ledger: the whole
// argument for installation tokens over a stored PAT is that there is nothing
// left behind to steal.
type RepoCredential struct {
	Username  string
	Password  string
	ExpiresAt time.Time
}

// The three ways a repo request fails that are NOT this gateway malfunctioning.
// A Repos implementation returns them; the classification below turns them into
// the stable codes a node maps onto a guest's HTTP status. Anything else is a
// fault and is reported as one.
var (
	// ErrNoSuchRepo is a slug the sandbox has no attachment to. It is answered
	// 404 rather than 403 because git consults the credential helper about
	// every host it touches: an unattached repository is an ordinary miss.
	ErrNoSuchRepo = errors.New("that sandbox has no attachment to that repository")
	// ErrReposDisabled is a deployment with no attachment store or no GitHub
	// App key. Permanent until an operator changes it, and said in a sentence
	// so that a guest is told 501 instead of being left to infer it from a
	// timeout.
	ErrReposDisabled = errors.New("this gateway serves no repo attachments")
	// ErrRepoDenied is a repository the sandbox IS attached to and this fleet
	// will still not mint for: an owner whose GitHub link is not strong enough
	// to reach an access verb, an account that is not active, an App that is not
	// installed there. Nobody's outage, and each has its own fix, so the
	// sentence travels with it.
	ErrRepoDenied = errors.New("this sandbox may not have a credential for that repository")
	// ErrRepoUpstream is github.com. It is separate from a fault because the
	// repair is to wait, and a 500 would talk the guest's retry out of it.
	ErrRepoUpstream = errors.New("github.com could not be reached")
)

// maxRepoRefusalText bounds the one refusal sentence that crosses the link
// verbatim. It is measured against what a person reads in a terminal, not
// against what fits in a frame: the sentences this fleet actually sends name a
// missing GitHub link or an install URL, and anything longer is not describing
// one of those.
const maxRepoRefusalText = 240

// SetRepos installs the resolver. Nil — the default — means this deployment
// attaches no repositories, and a node that asks is told so rather than left
// waiting on a hook that will never answer.
func (f *Fleet) SetRepos(r Repos) { f.repos = r }

// SelfRepos answers a node asking what one of its sandboxes should have checked
// out.
//
// node is the AUTHENTICATED link name, and req.Sandbox is the entire rest of the
// request: everything that decides the answer — the owner, the owner's tags, the
// rows those tags carry — this gateway resolves for itself.
func (f *Fleet) SelfRepos(ctx context.Context, node string, req nodelink.SelfReposReq) (nodelink.SelfReposResp, error) {
	box, err := f.selfServiceBox(node, req.Sandbox)
	if err != nil {
		return nodelink.SelfReposResp{}, err
	}
	if f.repos == nil {
		return nodelink.SelfReposResp{}, noRepos()
	}
	rows, err := f.repos.Manifest(ctx, box)
	if err != nil {
		return nodelink.SelfReposResp{}, repoFailure(err)
	}
	out := make([]nodelink.SelfRepoEntry, 0, len(rows))
	for _, r := range rows {
		out = append(out, nodelink.SelfRepoEntry{
			Host: r.Host, Slug: r.Slug, Ref: r.Ref, Path: r.Path, Access: r.Access,
		})
	}
	f.log.Debug("resolved a repo manifest for a sandbox on another machine",
		"sandbox", box.Name, "owner", box.Owner, "node", node, "repos", len(out))
	return nodelink.SelfReposResp{Repos: out}, nil
}

// SelfRepoCredential mints a git credential for one of a sandbox's repositories.
//
// The slug is checked against the manifest this gateway just resolved before it
// reaches the minter, and that check is the reason this method does two lookups
// where one would do. The minter is asked for a narrowly scoped token and can be
// expected to refuse an unattached repository on its own — but "can be expected
// to" is not a property this file can see, and it is the file whose job is to
// make sure a machine cannot read a repository it was not given. Doing it here
// costs one local query on a path that is already crossing the internet twice.
//
// The comparison is case-INSENSITIVE because github.com is: `wandb/Hivemind` and
// `wandb/hivemind` are one repository, the attachment store folds case in SQL,
// and Go does not. A case-sensitive check here would refuse a guest whose remote
// URL merely differs in capitalisation from what its owner typed at attach time.
func (f *Fleet) SelfRepoCredential(ctx context.Context, node string, req nodelink.SelfRepoCredReq) (nodelink.SelfRepoCredResp, error) {
	box, err := f.selfServiceBox(node, req.Sandbox)
	if err != nil {
		return nodelink.SelfRepoCredResp{}, err
	}
	if f.repos == nil {
		return nodelink.SelfRepoCredResp{}, noRepos()
	}
	rows, err := f.repos.Manifest(ctx, box)
	if err != nil {
		return nodelink.SelfRepoCredResp{}, repoFailure(err)
	}
	attached := ""
	for _, r := range rows {
		if strings.EqualFold(r.Slug, req.Slug) {
			attached = r.Slug
			break
		}
	}
	if attached == "" {
		// The same sentence a gateway's own guest gets, and deliberately no
		// hint about whether the repository exists on github.com at all: this
		// endpoint answers what THIS sandbox is attached to and nothing else.
		f.log.Warn("refused a repo credential for a repository the sandbox is not attached to",
			"node", node, "sandbox", box.Name, "owner", box.Owner, "slug", req.Slug)
		return nodelink.SelfRepoCredResp{}, &ctlops.Error{
			Kind: ctlops.KindNotFound, Op: nodelink.OpLink, Code: nodelink.CodeNoSuchRepo, Verbatim: true,
			Msg: "this sandbox has no attachment to " + req.Slug + ".",
		}
	}
	// The stored spelling, not the guest's: whatever reaches the GitHub API
	// should be what an owner attached, so a log line on either side names the
	// same string.
	cred, err := f.repos.Credential(ctx, box, attached)
	if err != nil {
		return nodelink.SelfRepoCredResp{}, repoFailure(err)
	}
	// Expiry and repository, never the token. This is the same discipline the
	// id-token mint keeps one file over, and it matters more here: an
	// installation token in a gateway log is read access to somebody's source
	// code for as long as the log is readable.
	f.log.Info("minted a git credential for a sandbox on another machine",
		"sandbox", box.Name, "owner", box.Owner, "node", node,
		"slug", attached, "exp", cred.ExpiresAt)
	return nodelink.SelfRepoCredResp{
		Username: cred.Username, Password: cred.Password, ExpiresAt: cred.ExpiresAt,
	}, nil
}

// noRepos is the answer for a deployment that attaches no repositories. It is
// built here as well as in nodelink because the hook is wired unconditionally:
// a nil resolver has to produce the same code a nil hook would, or the same
// deployment would answer two different ways depending on which was left unset.
func noRepos() error {
	return &ctlops.Error{
		Kind: ctlops.KindDisabled, Op: nodelink.OpLink, Code: nodelink.CodeNoRepos, Verbatim: true,
		Msg: "this gateway serves no repo attachments: it has no attachment store or no GitHub App key configured.",
	}
}

// repoFailure keeps a typed refusal typed, classifies the three the resolver
// states, and turns everything else into an internal fault with nothing of the
// cause in it.
//
// The last clause is not tidiness. The errors a minter produces quote what
// github.com said, and a resolver's quote the store — neither is written for a
// guest, and both cross a link that ends at a VM the gateway does not trust. The
// sentence an operator needs is in this gateway's own log.
func repoFailure(err error) error {
	var typed *ctlops.Error
	if errors.As(err, &typed) {
		return err
	}
	switch {
	case errors.Is(err, ErrNoSuchRepo):
		return &ctlops.Error{
			Kind: ctlops.KindNotFound, Op: nodelink.OpLink, Code: nodelink.CodeNoSuchRepo, Verbatim: true,
			Msg: "this sandbox has no attachment to that repository.",
		}
	case errors.Is(err, ErrReposDisabled):
		return noRepos()
	case errors.Is(err, ErrRepoDenied):
		// The one class whose sentence is carried rather than replaced. Each of
		// its causes has a different fix — verify a GitHub link, reactivate an
		// account, install the App — and a guest told only "denied" leaves
		// somebody reading a gateway log they may not have. It is bounded and
		// scrubbed on the way out because it ends up in a terminal, and it names
		// nothing about any other tenant.
		return &ctlops.Error{
			Kind: ctlops.KindDenied, Op: nodelink.OpLink, Code: nodelink.CodeRepoDenied, Verbatim: true,
			Msg: ctlops.SafeText(err.Error(), maxRepoRefusalText),
		}
	case errors.Is(err, ErrRepoUpstream):
		return &ctlops.Error{
			Kind: ctlops.KindUpstream, Op: nodelink.OpLink, Code: nodelink.CodeRepoUpstream, Verbatim: true,
			Msg: "this gateway could not reach github.com to answer that; try again in a moment.",
		}
	}
	return &ctlops.Error{
		Kind: ctlops.KindInternal, Op: nodelink.OpLink, Code: "repo_resolve_failed", Verbatim: true,
		Msg: "this gateway could not resolve that sandbox's repositories.",
	}
}
