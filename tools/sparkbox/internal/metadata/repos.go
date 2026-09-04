package metadata

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ghapp"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/ghuser"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/repos"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/users"
)

// RepoAccess is the repository view of the calling sandbox, resolved by the
// machine that holds the ledger. Manifest answers "what should be checked out
// in here"; Credential answers "what may clone it, for the next hour".
//
// It is a SIBLING of Identity and deliberately not two more methods on it.
// Three types implement Identity — Local, the gRPC relay and the node's
// uplink relay — and internal/hivemindpresence declares its own structurally
// identical one-method copy of it, which a wider Identity would silently stop
// satisfying. A capability with its own Options field also gets to be absent:
// a host with no repos store answers 501 on these two routes and keeps minting
// id tokens, where a fatter Identity would have to fake half of itself.
//
// Both methods take a context because a node's implementation is a network
// call, and both take the sandbox record rather than its name for the reason
// Identity states: the local implementation already has the record, and the
// relay sends only the name, so nothing a node asserts about a sandbox decides
// which repositories it gets.
type RepoAccess interface {
	Manifest(ctx context.Context, box *host.Sandbox) (Manifest, error)
	Credential(ctx context.Context, box *host.Sandbox, slug string) (Credential, error)
}

type RepoAuthorizer interface {
	StartAuthorization(ctx context.Context, box *host.Sandbox, slug string) (AuthorizationStart, error)
	PollAuthorization(ctx context.Context, box *host.Sandbox, id string) (AuthorizationStatus, error)
}

type AuthorizationStart struct {
	ID              string    `json:"id"`
	UserCode        string    `json:"user_code"`
	VerificationURI string    `json:"verification_uri"`
	IntervalSeconds int       `json:"interval_seconds"`
	ExpiresAt       time.Time `json:"expires_at"`
}

type AuthorizationStatus struct {
	State string `json:"state"`
	Slug  string `json:"slug,omitempty"`
}

// Manifest is what the guest's clone-at-boot unit reads. It is configuration,
// not a secret — a repository slug is a fact about which code belongs here —
// so it is returned in full and is safe in a boot log.
type Manifest struct {
	Repos []RepoEntry `json:"repos"`
}

// RepoEntry is one attachment as the guest sees it. It restates the store's
// columns rather than embedding repos.Repo: the guest has no business knowing
// the row id, the owning handle or the tags that put it here, and a manifest
// that carried them would be a list of one user's tag names handed to whatever
// runs in the box.
type RepoEntry struct {
	Host   string `json:"host"`
	Slug   string `json:"slug"`
	Ref    string `json:"ref,omitempty"`
	Path   string `json:"path,omitempty"`
	Access string `json:"access"`

	// Instance says this sandbox carries a ref override for this attachment —
	// somebody named this repository's branch when the box was made, through a
	// launch link, a `--ref <owner>/<repo>=<branch>` at create, or a fork.
	//
	// It is the answer to "which of these is the one I came for", and it is the
	// only honest one available. A tag's attachments are otherwise symmetric:
	// two repositories both riding `hm` are both wanted, in an order that means
	// nothing. Naming a branch is not symmetric — it is a person saying what
	// they intend to work on — so the guest starts a login shell in that
	// checkout and leaves the others where they are.
	//
	// The effective Ref above already folds the override in (a launch link on
	// the default branch produces the same string an attachment would), which
	// is why the fact needs its own field rather than being read off it.
	Instance bool `json:"instance,omitempty"`
}

// Credential is one short-lived token in the shape git's credential protocol
// wants. The token is never stored in the guest: not in ~/.git-credentials and
// not in a remote URL. It is either an installation token minted on demand or
// a repository-restricted user token whose rotating grant stays encrypted on
// the gateway.
type Credential struct {
	Username  string    `json:"username"`
	Password  string    `json:"password"`
	ExpiresAt time.Time `json:"expires_at"`
}

// credentialUsername is what git sends as the basic-auth user for an
// installation token. GitHub ignores the value, but git requires one, and
// x-access-token is the string GitHub's own documentation uses — so it is what
// shows up in anybody's tcpdump and in every worked example.
const credentialUsername = "x-access-token"

// The repo endpoints' own sliding window, deliberately not the mint budget.
//
// /token's window is 10 a minute, sized for a guest that refreshes an id token
// every 45 minutes. Git is nothing like that: one `git clone` with submodules
// is many helper invocations, and a build loop fetches on a timer. Sharing the
// window would let an ordinary fetch loop exhaust it and starve the OIDC
// refresh — whose systemd unit carries StartLimitBurst=10/300s, so the guest
// would burn that too and stop having an identity at all. A minute of git is
// cheap on the host because a mint is cached until five minutes before expiry;
// what this bounds is a guest looping on a *cold* cache.
const (
	credWindow = time.Minute
	credBurst  = 60
)

// githubBudget bounds the GitHub half of a credential mint. ListenAndServe
// gives the handler a 10s WriteTimeout and the guest calls with
// `curl --max-time 10`, while a cold mint is two internet round trips — resolve
// the installation, then mint — behind a fleet hop on a node. Left to the
// request's own deadline a slow GitHub produces a truncated response at the
// write timeout, which reads to the guest as a broken host. Cut short here it
// produces a 503 with a sentence, which is the answer the guest's retry is for.
const githubBudget = 6 * time.Second

// githubAuthorizationBudget covers a cold interactive flow, which may resolve
// an installation and repository on start, then exchange and verify a token on
// poll. It remains below the metadata server's ten-second write deadline so a
// slow GitHub answer becomes a complete 503 rather than a truncated response.
const githubAuthorizationBudget = 8 * time.Second

// ErrNotEnabled is what a host without the backing configuration returns: no
// repos store, or no GitHub App key. It is answered 501, the same shape the
// route self-service already uses, because it is a statement about the
// deployment and no amount of retrying changes it.
var ErrNotEnabled = errors.New("repositories are not enabled on this host")

// ErrNoSuchRepo is the answer to a credential request for a repository this
// sandbox cannot have one for: no attachment, or an attachment the App cannot
// reach. Answered 404, and deliberately the same 404 for "no such repository
// anywhere" as for "attached to somebody else": the guest asks about a slug it
// chose, and the endpoint must not become an oracle for which repositories
// other people have attached.
//
// Git consults the credential helper about every host it touches, so a miss is
// the ordinary answer here rather than an incident, and it is the status a node
// already gives for the same conditions — see internal/fleet's ErrNoSuchRepo.
var ErrNoSuchRepo = errors.New("this sandbox has no attachment for that repository")

// ErrRepoDenied is the refusal that is about the ACCOUNT rather than the
// repository: the owner's GitHub link is not strong enough to reach a verb that
// grants access, or the installation belongs to a different GitHub account than
// the one this handle is linked to. Answered 403, with the wrapped sentence
// carried through, because each has a different fix and the guest's boot log is
// where somebody will read it.
var ErrRepoDenied = errors.New("this sandbox may not have a credential for that repository")

// ErrUpstream is GitHub being slow, down or rate-limiting. Answered 503 for the
// same reason ErrNoIssuer is: the guest's own retry is the repair, and a 500
// invites it to give up instead.
var ErrUpstream = errors.New("the credential could not be minted right now")

// LocalRepos is the RepoAccess of a host that holds both halves: the repos
// store and the App key. It mirrors Local — same position in the design, same
// rule that the caller names only itself and the host decides everything else.
//
// On a fleet it runs on the gateway only, and for the same reason Local does:
// the App key mints for every installation of the App, so it belongs where the
// OIDC key already is, and a node relays.
type LocalRepos struct {
	// Repos is the attachment ledger. Nil means this host was built without
	// one, which is ErrNotEnabled rather than an empty manifest — an empty
	// manifest is a claim that the owner attached nothing.
	Repos *repos.Store
	// Users resolves the owner's account. Required for Credential and unused by
	// Manifest: a slug is configuration, a credential is access.
	Users Accounts
	// App mints installation tokens. Nil is a fleet that has attachments but no
	// GitHub App — a supported state, in which public repositories still clone
	// and Credential answers ErrNotEnabled.
	App      *ghapp.App
	UserAuth *ghuser.Manager
	// Log is optional; nil discards. It exists for exactly one line — the
	// fallback from a user-attributed token to the App bot, which is otherwise
	// an invisible change of who GitHub thinks is acting.
	Log *slog.Logger
}

func (l LocalRepos) log() *slog.Logger {
	if l.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return l.Log
}

func (l LocalRepos) StartAuthorization(ctx context.Context, box *host.Sandbox, slug string) (AuthorizationStart, error) {
	if l.UserAuth == nil {
		return AuthorizationStart{}, fmt.Errorf("%w: github user authorization is not enabled", ErrNotEnabled)
	}
	subject, err := l.authorizationSubject(ctx, box, slug)
	if err != nil {
		return AuthorizationStart{}, err
	}
	started, err := l.UserAuth.Start(ctx, subject)
	if err != nil {
		return AuthorizationStart{}, fmt.Errorf("%w: start github authorization: %v", ErrUpstream, err)
	}
	return AuthorizationStart{ID: started.ID, UserCode: started.UserCode, VerificationURI: started.VerificationURI,
		IntervalSeconds: started.IntervalSeconds, ExpiresAt: started.ExpiresAt}, nil
}

func (l LocalRepos) PollAuthorization(ctx context.Context, box *host.Sandbox, id string) (AuthorizationStatus, error) {
	if l.UserAuth == nil {
		return AuthorizationStatus{}, fmt.Errorf("%w: github user authorization is not enabled", ErrNotEnabled)
	}
	ctx, cancel := context.WithTimeout(ctx, githubAuthorizationBudget)
	defer cancel()
	status, err := l.UserAuth.Poll(ctx, box.Owner, id)
	if err != nil {
		switch {
		case errors.Is(err, ghuser.ErrDenied), errors.Is(err, ghuser.ErrExpired),
			errors.Is(err, ghuser.ErrWrongUser), errors.Is(err, ghuser.ErrWrongScope),
			errors.Is(err, ghuser.ErrNoRefresh):
			return AuthorizationStatus{}, fmt.Errorf("%w: %v", ErrRepoDenied, err)
		default:
			return AuthorizationStatus{}, fmt.Errorf("%w: finish github authorization: %v", ErrUpstream, err)
		}
	}
	return AuthorizationStatus{State: status.State, Slug: status.Slug}, nil
}

func (l LocalRepos) authorizationSubject(ctx context.Context, box *host.Sandbox, slug string) (ghuser.Subject, error) {
	if l.Repos == nil || l.App == nil || l.Users == nil {
		return ghuser.Subject{}, ErrNotEnabled
	}
	if box == nil || box.Owner == "" {
		return ghuser.Subject{}, fmt.Errorf("%w: sandbox has no owner", ErrRepoDenied)
	}
	u, err := l.Users.Get(box.Owner)
	if err != nil || !u.Active() || u.GitHubVerifiedAt == nil || !users.StrongGitHubLink(u.GitHubVia) || u.GitHubID <= 0 {
		return ghuser.Subject{}, fmt.Errorf("%w: this sandbox's owner has no verified github link", ErrRepoDenied)
	}
	attached, err := l.Repos.ReposForSandbox(box.Name, box.Owner)
	if err != nil {
		return ghuser.Subject{}, err
	}
	var entry repos.Repo
	for _, candidate := range attached {
		if strings.EqualFold(candidate.Slug, slug) {
			entry = candidate
			break
		}
	}
	if entry.Slug == "" {
		return ghuser.Subject{}, fmt.Errorf("%w: %s", ErrNoSuchRepo, slug)
	}
	if entry.Access != repos.AccessWrite {
		return ghuser.Subject{}, fmt.Errorf("%w: %s is attached read-only; user authorization is only used for write attachments", ErrRepoDenied, entry.Slug)
	}
	owner, name, ok := repos.SplitSlug(entry.Slug)
	if !ok {
		return ghuser.Subject{}, fmt.Errorf("invalid stored slug %q", entry.Slug)
	}
	ctx, cancel := context.WithTimeout(ctx, githubAuthorizationBudget)
	defer cancel()
	inst, err := l.App.InstallationFor(ctx, owner, name)
	if err != nil {
		return ghuser.Subject{}, githubError(err)
	}
	if err := l.App.Authorize(ctx, inst, u.GitHubID, u.GitHubLogin); err != nil {
		return ghuser.Subject{}, githubError(err)
	}
	repoID, err := l.App.RepositoryID(ctx, inst, owner, name)
	if err != nil {
		return ghuser.Subject{}, githubError(err)
	}
	// Write, because this path only runs for a write attachment (checked
	// above) and the consent screen has to cover what the credential path will
	// later ask for — see ghapp.MintPermissions on why a consent set and a mint
	// set that disagree expire into a bot token.
	perms := inst.Narrow(ghapp.MintPermissions(ghapp.PermWrite))
	perms["contents"] = ghapp.PermWrite
	return ghuser.Subject{Owner: box.Owner, GitHubID: u.GitHubID, InstallationID: inst.ID, RepoID: repoID,
		Slug: entry.Slug, Target: inst.AccountLogin, Permissions: perms}, nil
}

// Manifest lists what this sandbox's tags say should be checked out in it.
//
// The join is owner-scoped inside the store, and the owner comes from the
// sandbox record this host resolved from the tap — never from the request. On a
// node that record came through the placement ledger, which stamps the ledger's
// owner over whatever the node's inventory reported.
func (l LocalRepos) Manifest(_ context.Context, box *host.Sandbox) (Manifest, error) {
	if l.Repos == nil {
		return Manifest{}, ErrNotEnabled
	}
	if box.Owner == "" {
		// An unowned sandbox has nobody's attachments. Returning early rather
		// than querying with an empty handle keeps a row ever written under an
		// empty owner from becoming everyone's.
		return Manifest{Repos: []RepoEntry{}}, nil
	}
	attached, err := l.Repos.ReposForSandbox(box.Name, box.Owner)
	if err != nil {
		return Manifest{}, err
	}
	// Which of these the box was created for, read from the one table in the
	// store that is about an INSTANCE rather than a configuration. Its error is
	// returned rather than swallowed: it is the same sqlite file the query
	// above just read, so a failure here is not "no overrides", it is a store
	// in trouble, and answering with a confident empty mark would be a guess.
	refs, err := l.Repos.SandboxRefs(box.Owner, box.Name)
	if err != nil {
		return Manifest{}, err
	}
	chosen := map[string]bool{}
	for _, r := range refs {
		chosen[instanceKey(r.Host, r.Slug)] = true
	}
	out := make([]RepoEntry, 0, len(attached))
	for _, r := range attached {
		out = append(out, RepoEntry{
			Host: r.Host, Slug: r.Slug, Ref: r.Ref, Path: r.Path, Access: r.Access,
			Instance: chosen[instanceKey(r.Host, r.Slug)],
		})
	}
	return Manifest{Repos: out}, nil
}

// instanceKey matches an override row to an attachment. Slugs are stored
// COLLATE NOCASE and GitHub treats them case-insensitively, so `Wandb/Hivemind`
// in a launch URL has to find the attachment spelled `wandb/hivemind` — a
// case-sensitive compare here would silently drop the mark for exactly the
// people who typed the repository's display name.
func instanceKey(host, slug string) string {
	return strings.ToLower(host) + "\x00" + strings.ToLower(slug)
}

// Credential returns a GitHub token scoped to one repository this sandbox is
// actually attached to. A valid user grant wins for write attachments;
// otherwise it mints an installation token.
//
// The order of the checks is the design: resolve the owner, decide whether that
// owner may reach an access verb at all, confirm the attachment from this
// host's own ledger, and only then ask GitHub anything. The slug in the request
// is used to SELECT among the attachments this sandbox has, never to widen
// them — a guest naming a repository it has no attachment to is a 404 whether
// or not the App could have minted for it.
func (l LocalRepos) Credential(ctx context.Context, box *host.Sandbox, slug string) (Credential, error) {
	if l.Repos == nil {
		return Credential{}, ErrNotEnabled
	}
	if l.App == nil {
		// A fleet with attachments and no App is a supported state, not a
		// broken one: public repositories clone with no credential at all, and
		// this is the sentence that says so rather than a mint that fails.
		return Credential{}, fmt.Errorf("%w: no github app key is installed, so no repository credential can be minted", ErrNotEnabled)
	}
	if l.Users == nil {
		// Fail closed. Without the user store there is no way to establish
		// whose GitHub account this sandbox's owner is, and a mint without that
		// question answered is the cross-tenant hole the whole design exists to
		// avoid.
		return Credential{}, fmt.Errorf("%w: this host cannot resolve accounts", ErrRepoDenied)
	}
	if box.Owner == "" {
		return Credential{}, fmt.Errorf("%w: %q has no owner", ErrRepoDenied, box.Name)
	}
	u, err := l.Users.Get(box.Owner)
	if err != nil {
		return Credential{}, fmt.Errorf("%w: this sandbox's owner is not an account on this host: %w", ErrRepoDenied, err)
	}
	if !u.Active() {
		return Credential{}, fmt.Errorf("%w: this sandbox's owner's account is %s", ErrRepoDenied, u.Status)
	}
	// Strong provenance only, and this is the load-bearing half of the
	// condition rather than a refinement of it.
	//
	// A GitHub link records HOW it was proved (users.GitHubVia): a key found on
	// github.com/<login>.keys, the OAuth device flow, or a third party's signed
	// assertion that it knows this person. The first two are proofs the platform
	// witnessed itself. The third is somebody's word, and §2.3 of
	// docs/github-linking-design.md already refused to let it adopt a key for
	// exactly this reason: a channel that could be wrong about which human is
	// on the other end must not reach a verb that grants access.
	//
	// This is that verb, and it is the sharpest instance of it. What follows is
	// a token for private source code, bound to the linked account's github_id;
	// if the link is wrong, the binding is wrong in the one direction that
	// matters, and one user's agent gets another user's repositories. So an
	// `assertion` link is a fine thing for a console to display and not a thing
	// that mints a credential. Attachment enforces the same predicate at the
	// ctl surface; this is the check at the moment of use, because a link can
	// weaken after an attachment was made.
	if u.GitHubVerifiedAt == nil || !users.StrongGitHubLink(u.GitHubVia) {
		return Credential{}, fmt.Errorf("%w: this sandbox's owner has no verified github link — link one with the device flow", ErrRepoDenied)
	}

	attached, err := l.Repos.ReposForSandbox(box.Name, box.Owner)
	if err != nil {
		return Credential{}, err
	}
	var entry repos.Repo
	found := false
	for _, r := range attached {
		// The store's slug column compares NOCASE because github.com does;
		// Go does not, so fold here too or wandb/Hivemind misses the attachment
		// its own clone URL produced.
		if strings.EqualFold(r.Slug, slug) {
			entry, found = r, true
			break
		}
	}
	if !found {
		return Credential{}, fmt.Errorf("%w: %s", ErrNoSuchRepo, slug)
	}
	owner, name, ok := repos.SplitSlug(entry.Slug)
	if !ok {
		return Credential{}, fmt.Errorf("stored attachment %q is not an owner/name slug", entry.Slug)
	}

	// Everything past here is GitHub's, on GitHub's schedule. See githubBudget.
	ctx, cancel := context.WithTimeout(ctx, githubBudget)
	defer cancel()

	inst, err := l.App.InstallationFor(ctx, owner, name)
	if err != nil {
		return Credential{}, githubError(err)
	}
	// THE binding. Forward only: this handle's stored github_id against the
	// installation's account. See ghapp.Authorize for why the reverse lookup is
	// not a question this platform can answer.
	if err := l.App.Authorize(ctx, inst, u.GitHubID, u.GitHubLogin); err != nil {
		return Credential{}, githubError(err)
	}

	// One repository, and the permissions an attachment's access level implies.
	//
	// `contents` alone is what a clone, a fetch and a push of code need, and for
	// a long time it was all this minted. It is not all a person working in the
	// sandbox needs: `gh` runs on the same token — see the guest wrapper in
	// deploy/install-guest-identity.sh — and `gh pr create`, `gh pr list` and
	// `gh issue` are most of why anybody wants it there. A token that can push a
	// branch but not open a pull request for it is a strange half-grant, so the
	// set follows the attachment: read attachments read, write attachments write.
	// ghapp.MintPermissions holds the list, and the read-only tier it adds on top
	// (Dependabot alerts, CI, deployments) is there for the same reason.
	//
	// The installation-token fallback is still one repository, one hour and
	// never written down. A write attachment may instead have an encrypted,
	// repository-restricted user grant so GitHub attributes API actions to its
	// owner; the branch below selects it without changing the permission ceiling.
	perm := ghapp.PermRead
	if entry.Access == repos.AccessWrite {
		perm = ghapp.PermWrite
	}
	want := ghapp.MintPermissions(perm)
	// Narrowed to what the App was actually granted, because GitHub refuses a
	// token request naming a permission the installation lacks OUTRIGHT rather
	// than trimming it — so asking for pull_requests from an App that was never
	// given it would lose the contents half too, and break every clone on a
	// deployment whose App predates this. `contents` is restored unconditionally
	// after the intersection: it is the one permission the feature cannot work
	// without, and if the installation truly lacks it the mint should fail
	// saying so rather than silently produce a token good for nothing.
	perms := inst.Narrow(want)
	perms["contents"] = perm
	if entry.Access == repos.AccessWrite && l.UserAuth != nil {
		subject := ghuser.Subject{Owner: box.Owner, GitHubID: u.GitHubID,
			InstallationID: inst.ID, Slug: entry.Slug, Target: inst.AccountLogin, Permissions: perms}
		userToken, ok, userErr := l.UserAuth.Token(ctx, subject)
		switch {
		case userErr == nil && ok:
			return Credential{Username: credentialUsername, Password: userToken.AccessToken,
				ExpiresAt: userToken.AccessExpiresAt}, nil
		case userErr != nil:
			// Falling through to the bot is the right behaviour — a repository
			// that still clones beats one that does not — but it is a SILENT
			// change of identity: commits stop being attributed to the owner
			// who deliberately asked for attribution, and nothing upstream says
			// why. ghuser logs its own reasons; this line is what ties one to
			// the sandbox that lost the attribution.
			l.log().Warn("github user attribution unavailable; falling back to the app bot",
				"owner", box.Owner, "sandbox", box.Name, "repo", entry.Slug, "err", userErr)
		}
	}
	tok, err := l.App.MintToken(ctx, inst, []string{name}, perms)
	if err != nil {
		return Credential{}, githubError(err)
	}
	return Credential{Username: credentialUsername, Password: tok.Token, ExpiresAt: tok.ExpiresAt}, nil
}

// githubError translates ghapp's sentinels into this package's, so that a
// gateway-local failure and the same failure relayed from a node reach the
// guest as the same status. The relay cannot carry ghapp's errors — they cross
// a wire as a code — so the classification has to live on this side of the
// interface on both paths, not in the handler. It matches internal/fleet's
// classification of the same three conditions deliberately: a guest must not be
// able to tell which machine its sandbox landed on from the status it got.
//
// ErrNotInstalled reads as "no such attachment" and not as a refusal, for the
// same reason the fleet's does: the repository is attached and this fleet's App
// simply cannot reach it, which is the owner's to fix, and git asks the helper
// about every host it touches so a miss is the ordinary answer. The sentence
// naming the install URL rides along either way.
func githubError(err error) error {
	switch {
	case errors.Is(err, ghapp.ErrNotConfigured):
		return fmt.Errorf("%w: %w", ErrNotEnabled, err)
	case errors.Is(err, ghapp.ErrNotInstalled):
		return fmt.Errorf("%w: %w", ErrNoSuchRepo, err)
	case errors.Is(err, ghapp.ErrForbidden):
		return fmt.Errorf("%w: %w", ErrRepoDenied, err)
	case errors.Is(err, ghapp.ErrUpstream), errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("%w: %w", ErrUpstream, err)
	}
	return err
}

// repoManifest answers GET /repos: the list this sandbox should have checked
// out. No credential is involved and nothing here is secret, which is why it is
// not rate-limited alongside the mint — but it does leave the machine on a
// node, so it takes its own budget from the same window.
func (s *Server) repoManifest(w http.ResponseWriter, r *http.Request) {
	box, err := s.caller(r)
	if err != nil {
		http.Error(w, "sparkbox: "+err.Error(), http.StatusForbidden)
		return
	}
	if s.repoAccess == nil {
		http.Error(w, "sparkbox: repo attachment is not enabled", http.StatusNotImplemented)
		return
	}
	if !s.allowRepoCall(box.Name + " manifest") {
		http.Error(w, "sparkbox: too many repo requests", http.StatusTooManyRequests)
		return
	}
	manifest, err := s.repoAccess.Manifest(r.Context(), box)
	if err != nil {
		s.failRepos(w, "manifest", box, err)
		return
	}
	if manifest.Repos == nil {
		// The guest's clone unit iterates this with jq; `null` and `[]` are the
		// difference between "no repos" and an error message about null.
		manifest.Repos = []RepoEntry{}
	}
	s.writeJSON(w, manifest)
}

// githubCredential answers GET /github/credential?slug=owner/name: one
// short-lived token for one repository.
//
// The slug is checked for shape here rather than only at the far end because on
// a node this handler is the last thing before a fleet hop and two GitHub round
// trips, and because a guest that sends junk should be told so at once instead
// of through a 502 four layers away. It is not an authorization check: which
// attachments exist is decided by the machine that holds the ledger.
func (s *Server) githubCredential(w http.ResponseWriter, r *http.Request) {
	box, err := s.caller(r)
	if err != nil {
		s.log.Warn("metadata credential refused", "remote", r.RemoteAddr, "err", err)
		http.Error(w, "sparkbox: "+err.Error(), http.StatusForbidden)
		return
	}
	slug := strings.TrimSpace(r.URL.Query().Get("slug"))
	if slug == "" {
		http.Error(w, "sparkbox: ?slug=owner/name is required", http.StatusBadRequest)
		return
	}
	if !repos.ValidSlug(slug) {
		http.Error(w, "sparkbox: ?slug= must be owner/name, e.g. wandb/hivemind", http.StatusBadRequest)
		return
	}
	if s.repoAccess == nil {
		http.Error(w, "sparkbox: github credentials are not enabled", http.StatusNotImplemented)
		return
	}
	// Taken before the mint, like /token's: past here the call leaves the
	// machine — to the gateway on a node, and to github.com on either — so this
	// is also what bounds how much of somebody else's budget one guest spends.
	if !s.allowRepoCall(box.Name + " credential") {
		http.Error(w, "sparkbox: too many credential requests", http.StatusTooManyRequests)
		return
	}
	cred, err := s.repoAccess.Credential(r.Context(), box, slug)
	if err != nil {
		s.failRepos(w, "credential", box, err)
		return
	}
	// The audit line. Repository and expiry, never the token — the same rule
	// the id-token mint follows, and the reason this endpoint is safe to run
	// with a log collector attached.
	s.log.Info("minted github credential", "sandbox", box.Name, "owner", box.Owner, "repo", slug, "exp", cred.ExpiresAt)
	s.writeJSON(w, cred)
}

func (s *Server) startGithubAuthorization(w http.ResponseWriter, r *http.Request) {
	box, err := s.caller(r)
	if err != nil {
		http.Error(w, "sparkbox: "+err.Error(), http.StatusForbidden)
		return
	}
	slug := strings.TrimSpace(r.URL.Query().Get("slug"))
	if !repos.ValidSlug(slug) {
		http.Error(w, "sparkbox: ?slug= must be owner/name", http.StatusBadRequest)
		return
	}
	if s.repoAuthorizer == nil {
		http.Error(w, "sparkbox: github user authorization is not enabled", http.StatusNotImplemented)
		return
	}
	if !s.allowRepoCall(box.Name + " authorization-start") {
		http.Error(w, "sparkbox: too many authorization requests", http.StatusTooManyRequests)
		return
	}
	started, err := s.repoAuthorizer.StartAuthorization(r.Context(), box, slug)
	if err != nil {
		s.failRepos(w, "authorize", box, err)
		return
	}
	s.writeJSON(w, started)
}

func (s *Server) pollGithubAuthorization(w http.ResponseWriter, r *http.Request) {
	box, err := s.caller(r)
	if err != nil {
		http.Error(w, "sparkbox: "+err.Error(), http.StatusForbidden)
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if len(id) < 20 || len(id) > 128 {
		http.Error(w, "sparkbox: invalid authorization id", http.StatusBadRequest)
		return
	}
	if s.repoAuthorizer == nil {
		http.Error(w, "sparkbox: github user authorization is not enabled", http.StatusNotImplemented)
		return
	}
	if !s.allowRepoCall(box.Name + " authorization-poll") {
		http.Error(w, "sparkbox: too many authorization polls", http.StatusTooManyRequests)
		return
	}
	status, err := s.repoAuthorizer.PollAuthorization(r.Context(), box, id)
	if err != nil {
		s.failRepos(w, "authorize", box, err)
		return
	}
	s.writeJSON(w, status)
}

// failRepos maps a RepoAccess error onto a status. It is a sibling of fail
// rather than a case in it because fail's default sentence is about identity,
// and a guest told "could not establish this sandbox's identity" when its
// clone failed would go looking in entirely the wrong place.
func (s *Server) failRepos(w http.ResponseWriter, what string, box *host.Sandbox, err error) {
	switch {
	case errors.Is(err, ErrNoSuchRepo):
		http.Error(w, "sparkbox: "+err.Error(), http.StatusNotFound)
	case errors.Is(err, ErrRepoDenied):
		s.log.Warn("repo access refused", "op", what, "sandbox", box.Name, "owner", box.Owner, "err", err)
		http.Error(w, "sparkbox: "+err.Error(), http.StatusForbidden)
	case errors.Is(err, ErrNotEnabled):
		http.Error(w, "sparkbox: "+err.Error(), http.StatusNotImplemented)
	case errors.Is(err, ErrUpstream), errors.Is(err, ErrNoIssuer):
		s.log.Warn("repo service unavailable", "op", what, "sandbox", box.Name, "err", err)
		http.Error(w, "sparkbox: "+err.Error(), http.StatusServiceUnavailable)
	default:
		s.log.Error("repo service failed", "op", what, "sandbox", box.Name, "err", err)
		http.Error(w, "sparkbox: could not answer this sandbox's repo request", http.StatusInternalServerError)
	}
}

// allowRepoCall is the repo endpoints' sliding window. It is a second copy of
// allow's shape rather than a call into it on purpose: the point is that the
// two budgets cannot touch, and a shared helper over a shared map would be one
// refactor away from being one budget again. The key carries the operation as
// well as the sandbox, so a clone loop cannot spend the manifest's budget
// either.
//
// The /tools endpoints take this window too, under the key "<sandbox> tools".
// They are the same CLASS of traffic as a clone — bulk, guest-initiated, and
// nobody's identity depends on it — so they belong on this side of the fence
// rather than on the mint's, where a guest pulling five artifacts could cost
// itself an OIDC refresh.
//
// Note what this does NOT bound: credBurst is requests per window, not
// simultaneous streams. A fleet-wide `sparkbox update-tools` is N guests each
// pulling ~150MB off one host at the same time, and nothing here says no to
// that. Acceptable for a command somebody runs by hand; think again before
// putting it on a timer.
func (s *Server) allowRepoCall(key string) bool {
	s.credMu.Lock()
	defer s.credMu.Unlock()
	now := time.Now()
	cutoff := now.Add(-credWindow)

	if s.credRecent == nil {
		s.credRecent = map[string][]time.Time{}
	}
	// Sweep callers that have stopped asking, for the reason allow does: a
	// sandbox that was destroyed never returns, and nothing else would drop it.
	for k, times := range s.credRecent {
		if len(times) == 0 || times[len(times)-1].Before(cutoff) {
			delete(s.credRecent, k)
		}
	}
	kept := s.credRecent[key][:0]
	for _, t := range s.credRecent[key] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= credBurst {
		s.credRecent[key] = kept
		return false
	}
	s.credRecent[key] = append(kept, now)
	return true
}
