// Package launch serves the one-click door at https://<launch>.<domain>: a
// button somebody pastes into a pull request comment, and the two-read resolve
// behind it that either hands the clicker back a sandbox they already have on
// that repository or offers to build them one.
//
// # What a launch link is
//
// A launch link is a URL that outlives the deployment it was written for. It
// goes into a comment on a pull request, a README, a design doc — places whose
// text is effectively immutable and whose readers arrive months later. Its
// canonical form is
//
//	https://<launch>.<domain>/<owner>/<repo>[?ref=<git ref>]
//
// and its promise is narrow on purpose: whoever clicks it signs in as
// THEMSELVES, and lands in THEIR OWN sandbox with that repository checked out
// on that branch. Nothing in the URL names a person, a machine, a sandbox or a
// command, so the link is the same link for everybody and carries no authority
// of its own. Every fact about the clicker — who they are, which repositories
// they have attached, which tags those attachments carry — comes from the
// verified edgeauth session and from internal/repos, never from the request
// line, because the request line is attacker-supplied text from a public
// comment.
//
// # Why the repository is in the path and not a query parameter
//
// `?repo=wandb/hivemind&ref=feat/x` and `/wandb/hivemind?ref=feat/x` reach the
// same handler, and the second is the one that survives a human. Markdown in a
// comment escapes `&` as `&amp;` — GitHub's sanitizer does it for you when it
// renders, and a person retyping the link by hand does not. Putting the repo in
// the path means the common form (no ref) contains NO `&` at all, and the ref
// form contains exactly one, so there is exactly one place in the whole string
// where escaping can go wrong. It also makes the link readable as a sentence,
// which matters when the thing being reviewed is the link itself.
//
// The path namespace this consumes is not consumed permanently. Go's ServeMux
// prefers the more specific pattern, so a future literal two-segment route
// registers beside "/{owner}/{repo}" and wins with no panic; and by contract
// any future non-repo first segment starts with '_' or '.', which
// users.ValidGitHubOrg already refuses, so no such path can ever be shadowed by
// a real GitHub owner.
//
// # Why the badge is static and parameterless
//
// The button image is ONE embedded SVG, identical for every repository, every
// branch and every viewer, served outside every auth wrapper. See badge.go for
// the full argument; the short form is that GitHub rewrites every image src to
// its own camo proxy, which sends no cookies and no identity and caches by URL,
// so a per-repo badge would multiply that cache by one object per comment for
// information the reader is already looking at — and a badge that rendered
// caller-supplied text would be an injection primitive on the one route in this
// package with no session behind it.
//
// # Why no tag may ever be a URL parameter
//
// This is the hard rule of the package, and it is the reason `tag=`, `node=`,
// `name=` and a command are all absent from the grammar rather than merely
// unimplemented.
//
// internal/ctlops/sandbox.go:36 states it from the other side: "Create stamps
// tags BEFORE Sandboxes.Create, because Create fires the secret-env push
// asynchronously and the tags decide its contents". The push it fires reads
// secrets.Store.EnvForSandbox, whose query joins sandbox_tags to secret_tags
// and hands the guest every secret of the sandbox's owner that shares a tag
// with it. A tag is therefore a selector over the OWNER's decrypted secrets.
//
// Put that selector in a URL and you have handed the author of a public,
// immutable comment the ability to choose which of the CLICKER's secrets are
// decrypted into a VM whose working tree sits at a branch the same author
// chose. Narrowing the parameter to "a tag that selects the named attachment"
// does not close it — a repository carried on both `dev` and `prod` still lets
// the author pick — and it would put the tag rule in a second place, where it
// goes stale the moment the attachment's tags change. So tags come from exactly
// one place: the matched attachment's stored Tags, read from the store under
// the session's own handle.
//
// # What a GET may do
//
// Nothing that writes. A link in a public comment is fetched by scanners,
// previewers and prefetchers that nobody consented to, so GET here never
// creates a sandbox, never resumes or restores one, never writes a tag or a ref
// row, never attaches a repository, never mints or extends a session and never
// consumes a quota slot. The create is a POST from a form on the confirm page.
package launch

import (
	"context"
	"log/slog"
	"net/http"
	"strings"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/edgeauth"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/repos"
	"golang.org/x/sync/singleflight"
)

// DefaultSubdomain is the label the edge dispatches on, so a link reads
// https://go.<domain>/<owner>/<repo>. It is a constant here rather than a bare
// string in main for the reason ghwebhook.DefaultSubdomain is one: the flag
// default, internal/reserved's claim on the name and this package's own idea of
// where it lives cannot then drift apart.
//
// Moving this label breaks every comment already written, which is a cost no
// other subdomain in this tree has — a sandbox's hostname dies with the
// sandbox, and a launch link is meant to outlive everything.
const DefaultSubdomain = "go"

// gitHubHost is the only forge internal/repos stores attachments for; its own
// defaultHost constant is unexported, and normSlug/normHost would reject
// anything else anyway. Named here rather than passed as a literal at the call
// site so the day a second forge exists there is one place to look.
const gitHubHost = "github.com"

// Sandboxes is the slice of *ctlops.Ops this door needs: read the caller's
// boxes, read one back after building it, and build one. Deliberately narrow —
// nothing here can pause, destroy, re-tag, archive or resume a sandbox, and in
// particular Resume is absent on purpose. Blocking a browser on a resume would
// sit behind ctlops.ArchiveTimeout (fifteen minutes) while hiding progress the
// browser terminal already renders on its own WebSocket, so the handoff is a
// redirect and internal/xterm owns the boot.
type Sandboxes interface {
	List(ctx context.Context, c ctlops.Caller) ([]ctlops.SandboxInfo, error)
	Get(ctx context.Context, c ctlops.Caller, name string) (ctlops.SandboxInfo, error)
	Create(ctx context.Context, c ctlops.Caller, a ctlops.CreateArgs) (ctlops.SandboxInfo, error)
}

// Attachments is the slice of *repos.Store this door needs.
//
// It exists because *ctlops.Ops cannot answer the only question this package
// has: "does this person already have a sandbox holding this repository at this
// branch?" The ctlops.Repos interface (internal/ctlops/ops.go:132-149) leaves
// ReposForSandbox out deliberately, and its comment defends that narrowness —
// "nothing on this surface resolves a guest's manifest: that is
// internal/metadata's job, over the guest's own tap, and a control-plane verb
// that could ask 'what does that sandbox get' would be a second answer to a
// question with one authority."
//
// We respect that narrowness rather than widening it. Ops keeps the surface it
// has; this package holds the concrete *repos.Store behind its own interface,
// declared to the three methods it reads and nothing more — the same shape the
// user console takes for the same reason. Every method here is a READ. Nothing
// in this package can write an attachment, and a reviewer can confirm that from
// this declaration alone rather than by auditing call sites.
//
// ReposForSandbox is the load-bearing one. Its ref column is, verbatim,
//
//	COALESCE(NULLIF(o.ref, ''), r.ref)
//
// (internal/repos/store.go:399-411) — the per-sandbox override already folded
// over the attachment's default — and it is the same query the guest's checkout
// manifest is built from. Matching against it means
// matching the one authority on which branch a box is actually on, rather than
// reconstructing that answer from override rows and getting it wrong for every
// sandbox that never overrode anything.
type Attachments interface {
	ListRepos(owner string) ([]repos.Repo, error)
	SandboxesForRepo(owner, host, slug string) ([]string, error)
	ReposForSandbox(sandbox, owner string) ([]repos.Repo, error)
}

// Config wires the handler. Ops, Repos and Signer are required and New panics
// without them, at startup and in one place, rather than nil-dereferencing on
// the first click an hour later — the internal/xterm precedent.
type Config struct {
	// Ops is the control-plane core, taken concrete the way restapi.Config
	// takes it: main holds exactly one *ctlops.Ops and every surface shares it,
	// so there is one place where ownership, timeout budgets and the
	// tags-before-create ordering are decided.
	Ops *ctlops.Ops
	// Repos is the attachment store. It is separate from Ops and it has to be —
	// see Attachments for why widening ctlops.Repos was rejected.
	Repos *repos.Store
	// Accounts resolves operator status and, more importantly here, an account
	// that has been disabled since its cookie was issued. Nil makes nobody an
	// operator, which fails closed.
	Accounts edgeauth.Accounts
	// Signer verifies the edge session carried by the cookie. Required: without
	// it there is no way to tell whose sandboxes we are about to list.
	Signer *edgeauth.Signer

	// Subdomain is the label this handler answers under. Empty takes
	// DefaultSubdomain.
	Subdomain string
	// Domain is the base zone ("catnip.sh"). A leading dot is tolerated, the
	// way it is everywhere else in the tree.
	Domain string
	// LoginURL is where an unauthenticated browser is sent; edgeauth's
	// challenge appends the URL to come back to. Empty turns the bounce into a
	// plain 401, which is what a test wants and what no deployment wants.
	LoginURL string
	// HomeURL is the user console, where somebody who trimmed a launch link
	// back to its bare hostname should end up, and where every screen's
	// secondary link points.
	//
	// It is configuration rather than a hostname this package builds, because
	// the user console's label is its own flag: composing "https://my." + zone
	// here would hard-code a literal that a relabelled host has already moved,
	// which is the exact mistake the badge markdown avoids by threading both
	// labels through from the flags that own them.
	//
	// Empty is a supported state and not a broken one: GET / then renders an
	// explainer describing the link grammar instead of redirecting, and the
	// screens simply carry no secondary link. That is the honest answer on a
	// host with no user console, and it is what a test gets by default.
	HomeURL string

	Log *slog.Logger
}

// Handler serves the badge, the confirm page and the create.
//
// The two store fields are interfaces rather than the concrete pointers Config
// carries, so this package's tests drive the whole resolve against in-memory
// fakes with no sqlite file and no VM driver anywhere.
type Handler struct {
	ops   Sandboxes
	repos Attachments

	accounts edgeauth.Accounts
	signer   *edgeauth.Signer

	subdomain string
	domain    string
	// origin is this door's own scheme+host with no trailing slash, and it is
	// what the create's first-party check compares an Origin header against.
	origin   string
	loginURL string
	homeURL  string
	// csp is the Content-Security-Policy every screen carries, composed once by
	// pageCSP because its form-action has to name the zone this host actually
	// serves. See pageCSP for why that directive cannot simply be 'self'.
	csp string

	// creates collapses concurrent creates for the same handle, repository and
	// branch into one. It is a FIELD and not a package-level var so that two
	// handlers in one process — which is what every test binary has — cannot
	// serialise against each other, and so that nothing survives a handler
	// being dropped. singleflight.Group's zero value is ready to use, which is
	// what lets the in-package tests build a Handler as a struct literal.
	creates singleflight.Group

	log *slog.Logger
}

// New builds the handler for <Subdomain>.<Domain>.
func New(cfg Config) *Handler {
	// These three are checked BEFORE the assignment below, and that ordering is
	// the whole point rather than a style preference. Assigning a nil
	// *ctlops.Ops into the Sandboxes field would produce an interface value
	// that is NOT nil — a typed nil — so a later `if h.ops == nil` guard would
	// pass and the first click would panic in a goroutine instead of the
	// process failing to start. cmd/sparkbox/main.go:1178-1181 documents the
	// same trap for restapi's Terminal field; this is the version of it that
	// cannot be worked around at the call site, so it is enforced here.
	if cfg.Ops == nil {
		panic("launch: Ops is required")
	}
	if cfg.Repos == nil {
		panic("launch: Repos store is required")
	}
	if cfg.Signer == nil {
		panic("launch: Signer is required")
	}
	sub := cfg.Subdomain
	if sub == "" {
		sub = DefaultSubdomain
	}
	sub = strings.ToLower(sub)
	// A leading-dot --proxy-domain (".catnip.sh") is tolerated everywhere else
	// in the tree; normalize it here too, or the origin this door compares a
	// form POST's Origin against would carry a dot no browser will ever send
	// and every create would be refused as cross-origin.
	domain := strings.ToLower(strings.Trim(cfg.Domain, "."))
	log := cfg.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}

	origin := "https://" + sub
	if domain != "" {
		origin = "https://" + sub + "." + domain
	}

	return &Handler{
		ops:       cfg.Ops,
		repos:     cfg.Repos,
		accounts:  cfg.Accounts,
		signer:    cfg.Signer,
		subdomain: sub,
		domain:    domain,
		origin:    origin,
		csp:       pageCSP(domain),
		loginURL:  cfg.LoginURL,
		homeURL:   cfg.HomeURL,
		log:       log,
	}
}

// Subdomain is the label this handler answers under, so main's collision
// warning and the edge quote the handler rather than a second copy of the
// string — the internal/xterm precedent.
func (h *Handler) Subdomain() string { return h.subdomain }

// Handler returns the mux, matching the shape main already uses to mount the
// consoles (px.SetReserved(sub, x.Handler())).
//
// Route gating is per-route rather than one middleware around the whole mux,
// which is what internal/restapi does with its authPublic rows and what
// internal/xterm does for its asset subtree. That is not a stylistic choice
// here: /badge.svg MUST be reachable with no session at all, because
// edgeauth.Require stamps Cache-Control: no-store on every response it allows
// and answers an uncredentialed fetch with either a login redirect or a 401 —
// and GitHub's camo proxy fetches the badge with no cookies. A gated badge is a
// broken image, so the gate cannot be allowed to close over it by accident. See
// badge.go.
func (h *Handler) Handler() http.Handler {
	mux := http.NewServeMux()
	// /badge.svg is one path segment and /{owner}/{repo} is two, so the button
	// image can never collide with a repository named "badge.svg" — there is no
	// such repository, because a slug always has an owner in front of it.
	mux.HandleFunc("GET /badge.svg", h.badge)

	require := edgeauth.Require(h.signer, h.accounts, h.loginURL)
	mux.Handle("GET /{$}", require(http.HandlerFunc(h.home)))
	mux.Handle("GET /{owner}/{repo}", require(http.HandlerFunc(h.get)))
	// Require wraps mutation and not the other way round, and the order is the
	// whole point. A visitor whose session expired while they were reading the
	// confirm page presses the button and must get the sign-in bounce, land
	// back on this page and press it again — not a 403 telling them their
	// request looked cross-origin, which is both wrong and unactionable on a
	// page with no JavaScript to obey a remedy. See csrf.go.
	mux.Handle("POST /{owner}/{repo}", require(h.mutation(http.HandlerFunc(h.post))))
	// Everything else, ungated: a path that is not a repository is not a thing
	// to sign anybody in for. Registered LAST here only for readability — Go's
	// ServeMux picks the most specific pattern regardless of registration
	// order, so "/" cannot shadow the three above it.
	mux.Handle("GET /", http.HandlerFunc(h.notFound))
	return mux
}
