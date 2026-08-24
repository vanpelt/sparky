// Package ghapp mints GitHub App installation tokens: hour-long credentials
// scoped to exactly the repositories a sandbox is attached to, carrying exactly
// the permissions those attachments declare, and stored nowhere.
//
// It exists to replace the credential that shipped first, which was a personal
// access token pasted in by hand: scoped to every repository the human can
// reach, sealed into the sandbox's /etc/environment, and alive until somebody
// remembers to revoke it. An installation token is the opposite trade on all
// three axes — its repository list is chosen by this host and not by the guest,
// its lifetime is an hour, and there is nothing left in the guest afterwards
// worth stealing. docs/github-repos-design.md Part 3 argues it out.
//
// The one durable secret is the App's private key. It lives on the gateway and
// never leaves it: on a fleet, a node relays the *request* and the gateway
// resolves the scope from its own placement ledger (design §3.5), because a
// node that could name its own repositories is a cross-tenant hole. The key is
// also the only fleet secret that is revocable in one click — regenerate it in
// the App's settings and every copy is dead — which is why nothing here is
// derived from the OIDC key, whose loss is permanent.
//
// No JWT dependency. internal/oidc hand-rolls its ES256 compact JWS in twenty
// readable lines and this hand-rolls the RS256 one beside it, for the same
// reason: a signing routine you can read start to finish is worth more on a
// security boundary than a library you would have to go and read anyway.
package ghapp

import (
	"context"
	"crypto/rsa"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
)

var (
	// ErrNotConfigured means this host has no App at all: no key was delivered,
	// or no client id was set. Callers turn it into "repo credentials are not
	// enabled here", the same shape tagging already uses, rather than a failure.
	ErrNotConfigured = errors.New("no GitHub App is configured on this host")

	// ErrNotInstalled is the signature failure mode of this whole feature: the
	// attachment is fine, the key is fine, and the App has simply never been
	// installed on that repository. It is invisible until a clone fails inside
	// a VM at boot, so every message wrapping it names the install URL.
	ErrNotInstalled = errors.New("the GitHub App is not installed on that repository")

	// ErrForbidden covers both halves of "no": GitHub refusing the App, and
	// this package refusing to bind a caller to an installation. The sentences
	// are what distinguish them, and they are deliberately all different.
	ErrForbidden = errors.New("the GitHub App may not do that")

	// ErrUpstream is github.com being down, slow, or rate-limiting. It is the
	// one class here that is worth retrying, which is why it is separated from
	// the refusals rather than folded into a generic error.
	ErrUpstream = errors.New("github.com did not answer")
)

// Config describes the App this host mints for. Everything but ClientID and Key
// has a working default, so the common construction is two fields.
type Config struct {
	// ClientID is the App's client id (the "Iv23li…" string), used verbatim as
	// the JWT's `iss`. GitHub also accepts the numeric app id there; the client
	// id is what the App settings page shows first and what the linking flow
	// already carries, so it is what this takes.
	ClientID string
	// Key is the App's RSA private key, as parsed by LoadKey.
	Key *rsa.PrivateKey
	// BaseURL overrides https://api.github.com. Tests point it at an httptest
	// server; a GitHub Enterprise Server deployment would point it at its own
	// /api/v3, which is the only reason this is not a constant.
	BaseURL string
	// HTTPClient overrides the default, which carries a 10s timeout. The
	// timeout matters more than usual: the guest that ends up here is calling
	// under a write timeout of its own.
	HTTPClient *http.Client
	// Logger receives one line per mint. It never receives a token.
	Logger *slog.Logger
	// Now overrides time.Now, so a test can assert on JWT claims and drive the
	// caches past their expiry without sleeping.
	Now func() time.Time
}

// App mints tokens for one GitHub App. It is safe for concurrent use, and it is
// meant to be a single long-lived instance per process: the caches it holds are
// what keep a guest's `git fetch` loop from becoming one mint per fetch.
type App struct {
	clientID string
	key      *rsa.PrivateKey
	baseURL  string
	hc       *http.Client
	log      *slog.Logger
	now      func() time.Time

	// The App assertion is minted at most once every jwtReuse, under its own
	// lock: signing is cheap but not free, and every API call needs one.
	jwtMu   sync.Mutex
	jwt     string
	jwtGood time.Time

	// The App's slug, learned rather than configured — see InstallURL.
	slugMu    sync.Mutex
	appSlug   string
	slugAfter time.Time

	insts   *cache[Installation]
	tokens  *cache[Token]
	members *cache[string]
}

// Installation is one account's installation of the App. AccountID, not
// AccountLogin, is the thing worth binding to: a login is renameable and, once
// released, re-registerable by somebody else, while the id is permanent.
type Installation struct {
	ID           int64  `json:"id"`
	AccountID    int64  `json:"account_id"`
	AccountLogin string `json:"account_login"`
	AccountType  string `json:"account_type"` // "User" | "Organization"
	// Permissions is what this installation actually holds, as GitHub reported
	// it: permission name to "read" or "write".
	//
	// It exists so a caller can ask for a WIDER token than `contents` without
	// betting the whole mint on the App having been granted every part of it.
	// A token request naming a permission the installation lacks is refused
	// wholesale with a 422 — not trimmed — so a `gh` token asking for
	// pull_requests from an App that was never given it would fail completely,
	// including the contents half that would have worked. Narrow() does the
	// intersection up front instead. Empty when GitHub reported none, which
	// Narrow treats as "grant nothing beyond what is asked for by name".
	Permissions map[string]string `json:"permissions,omitempty"`
}

// Narrow returns the subset of want this installation can actually be asked
// for, downgrading a requested "write" to "read" where that is all the App
// holds. A permission the installation does not have at all is dropped.
//
// The caller decides what an empty result means. For a `gh` token it means the
// broad request collapsed to nothing and the narrow one should be used instead;
// no caller should send an empty permission map to MintToken, which refuses it
// precisely because an absent list means EVERY permission the installation has.
func (i Installation) Narrow(want map[string]string) map[string]string {
	out := make(map[string]string, len(want))
	for name, level := range want {
		held, ok := i.Permissions[name]
		if !ok {
			continue
		}
		if level == PermWrite && held != PermWrite {
			level = held
		}
		out[name] = level
	}
	return out
}

// PermRead and PermWrite are the two levels this package asks for. Exported
// because the caller that decides which one an attachment implies lives in
// internal/metadata. GitHub also issues "admin" on some permissions; nothing
// here requests it, and Narrow passes an "admin" grant through unchanged when
// "read" was asked for, which is what the caller wanted.
const (
	PermRead  = "read"
	PermWrite = "write"
)

// Token is an installation access token and the moment it dies. It is passed
// around by value and never persisted; the only copy that outlives a request is
// the one in this package's cache, in memory, on the gateway.
type Token struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

// The two account types GitHub issues installations to. Compared exactly, not
// case-insensitively: an account type this package does not recognise must
// refuse rather than fall through to a binding rule written for another kind.
const (
	accountUser = "User"
	accountOrg  = "Organization"
)

// New returns an App, or ErrNotConfigured when this host has no App to mint
// for. Callers build one exactly when the optional fleet key was delivered and
// pass nil otherwise, so that "no App here" is a capability a verb reports as
// Disabled rather than an error every code path has to carry.
//
// It touches no network. A gateway constructs this at startup, on a host that
// may not have working egress yet, and a constructor that validated the key
// against github.com would turn a slow morning at api.github.com into a boot
// failure.
func New(cfg Config) (*App, error) {
	if cfg.Key == nil {
		return nil, fmt.Errorf("%w: no app private key was loaded", ErrNotConfigured)
	}
	if strings.TrimSpace(cfg.ClientID) == "" {
		return nil, fmt.Errorf("%w: no app client id was set", ErrNotConfigured)
	}
	a := &App{
		clientID: strings.TrimSpace(cfg.ClientID),
		key:      cfg.Key,
		baseURL:  strings.TrimSuffix(cfg.BaseURL, "/"),
		hc:       cfg.HTTPClient,
		log:      cfg.Logger,
		now:      cfg.Now,
		insts:    newCache[Installation](maxCacheEntries),
		tokens:   newCache[Token](maxCacheEntries),
		members:  newCache[string](maxCacheEntries),
	}
	if a.baseURL == "" {
		a.baseURL = defaultBaseURL
	}
	if a.hc == nil {
		a.hc = &http.Client{Timeout: requestTimeout}
	}
	if a.log == nil {
		a.log = slog.New(slog.DiscardHandler)
	}
	if a.now == nil {
		a.now = time.Now
	}
	return a, nil
}

// InstallationFor resolves owner/name to the installation that covers it.
//
// Cached for installTTL: installations change when a human clicks something on
// github.com, which is rare, and this lookup sits in front of every mint. A
// failure is never cached — somebody who installs the App and immediately
// retries must not be told for ten minutes that they did not.
func (a *App) InstallationFor(ctx context.Context, owner, name string) (Installation, error) {
	slug, err := checkSlug(owner, name)
	if err != nil {
		return Installation{}, err
	}
	return a.insts.get(ctx, a.now, strings.ToLower(slug), func(ctx context.Context) (Installation, time.Time, error) {
		auth, err := a.appAuth()
		if err != nil {
			return Installation{}, time.Time{}, err
		}
		var body struct {
			ID      int64  `json:"id"`
			AppSlug string `json:"app_slug"`
			Account struct {
				ID    int64  `json:"id"`
				Login string `json:"login"`
				Type  string `json:"type"`
			} `json:"account"`
			Permissions map[string]string `json:"permissions"`
		}
		path := fmt.Sprintf(installationPath, url.PathEscape(owner), url.PathEscape(name))
		op := "resolving the installation for " + slug
		if err := a.do(ctx, http.MethodGet, path, auth, op, nil, &body); err != nil {
			var ae *apiError
			if errors.As(err, &ae) && ae.status == http.StatusNotFound {
				// 404 here is one of two things and the caller cannot tell them
				// apart either: no installation, or an installation that cannot
				// see this repository. Both are fixed at the same URL, so both
				// get it.
				a.learnSlugFromAPI(ctx)
				return Installation{}, time.Time{}, fmt.Errorf("%w: %s — install it (or add that repository to the installation) at %s",
					ErrNotInstalled, slug, a.InstallURL())
			}
			return Installation{}, time.Time{}, err
		}
		a.learnSlug(body.AppSlug)
		if body.ID == 0 {
			return Installation{}, time.Time{}, fmt.Errorf("%w: github returned an installation with no id for %s", ErrUpstream, slug)
		}
		inst := Installation{
			ID:           body.ID,
			AccountID:    body.Account.ID,
			AccountLogin: body.Account.Login,
			AccountType:  body.Account.Type,
			Permissions:  body.Permissions,
		}
		return inst, a.now().Add(installTTL), nil
	})
}

// MintToken returns an installation access token scoped to repoNames — bare
// repository names, not slugs, which is what the API takes and which saves a
// second round trip to turn slugs into numeric ids.
//
// Both arguments are required, and that is the security posture rather than an
// input check: GitHub reads an absent `repositories` as *every* repository the
// installation covers and an absent `permissions` as *every* permission it
// holds, so the natural failure of a caller that forgot to pass its scope is
// the widest possible token. Refusing here means a bug in a caller costs a
// failed request instead of a credential nobody meant to mint.
//
// Cached per (installation, sorted repoNames, sorted permissions) until
// tokenRefreshLead before expiry. The cache is load-bearing, not an
// optimization: the guest calls under a 10s HTTP write timeout, and on a node
// the path is guest -> node -> gateway -> github, so a `git fetch` loop that
// minted per fetch would spend that budget on GitHub's rate limiter.
func (a *App) MintToken(ctx context.Context, inst Installation, repoNames []string, perms map[string]string) (Token, error) {
	if len(repoNames) == 0 {
		return Token{}, errors.New("an installation token must name at least one repository: a token with no repository list reaches every repository the installation covers")
	}
	if len(perms) == 0 {
		return Token{}, errors.New("an installation token must name its permissions: a token with no permission list carries every permission the installation holds")
	}
	for _, n := range repoNames {
		if !validRepoName(n) {
			return Token{}, fmt.Errorf("%q is not a github repository name", n)
		}
	}
	return a.token(ctx, inst.ID, repoNames, perms)
}

// Authorize decides whether the sparkbox account identified by (githubID,
// githubLogin) may use inst. This is the security boundary of the repos
// feature: getting it wrong hands one user's private repositories to another
// user's agent, so the rules are written out rather than inferred.
//
// The binding is FORWARD ONLY — handle to stored github_id to installation
// account id. The reverse ("which handle holds this account id?") is not a
// question this platform can answer: there is no unique index on
// users.github_id and nothing stops two handles from carrying the same one.
//
// Every refusal wraps ErrForbidden and every refusal has its own sentence. A
// caller can therefore match one error, and an operator reading the message
// still learns which of the four refusals they hit — in particular that "the
// App cannot read this org's membership" is a missing permission to grant and
// not a person to add.
func (a *App) Authorize(ctx context.Context, inst Installation, githubID int64, githubLogin string) error {
	if githubID == 0 {
		// Zero is UNKNOWN, not an account. It is what every link made before
		// the profile fetch existed carries, and what ctlops records when
		// api.github.com is slow at link time. Left unchecked it matches an
		// installation whose account id failed to decode, which is exactly the
		// pair of unknowns that must never be treated as agreement.
		return fmt.Errorf("%w: this account's github link carries no account number, so it cannot be matched to an installation — re-link it with the device flow", ErrForbidden)
	}
	switch inst.AccountType {
	case accountUser:
		if inst.AccountID == githubID {
			return nil
		}
		return fmt.Errorf("%w: that installation belongs to a different github account than this sparkbox account is linked to", ErrForbidden)

	case accountOrg:
		if !validLogin(inst.AccountLogin) {
			return fmt.Errorf("%w: github named %q as the installation's org, which is not a name it could have issued", ErrForbidden, inst.AccountLogin)
		}
		if !validLogin(githubLogin) {
			return fmt.Errorf("%w: this account has no usable github login to check its membership of %s with", ErrForbidden, inst.AccountLogin)
		}
		state, err := a.membership(ctx, inst, githubLogin)
		if err != nil {
			return err
		}
		switch state {
		case "active":
			return nil
		case "pending":
			// An invitation that has not been accepted is not membership, and
			// saying so beats "not a member" — the fix is one click by the
			// person being refused, not a mail to an org admin.
			return fmt.Errorf("%w: %s has been invited to %s but has not accepted yet", ErrForbidden, githubLogin, inst.AccountLogin)
		default:
			return fmt.Errorf("%w: %s is not a member of %s", ErrForbidden, githubLogin, inst.AccountLogin)
		}

	case "":
		return fmt.Errorf("%w: that installation record carries no account type, so there is no rule that says who may use it", ErrForbidden)
	default:
		return fmt.Errorf("%w: that installation is owned by a %q, which is not an account kind this host knows how to bind to a handle", ErrForbidden, inst.AccountType)
	}
}

// InstallURL is where a user installs this App.
//
// The one-click URL needs the App's *slug*, which the client id does not
// contain and which no flag carries. So it is learned: every installation
// record github returns names it (`app_slug`), and a lookup that 404s asks
// GET /app for it, which is exactly the moment this string gets printed.
//
// Until it is known the fallback is the user-authorization URL, which the
// client id does build and which github answers with the install screen when
// the App is not installed for that account. Both land the user in the right
// place; only the first lands them there without also asking for a user token
// nothing here wants.
func (a *App) InstallURL() string {
	a.slugMu.Lock()
	slug := a.appSlug
	a.slugMu.Unlock()
	if slug != "" {
		return "https://github.com/apps/" + slug + "/installations/new"
	}
	return "https://github.com/login/oauth/authorize?client_id=" + url.QueryEscape(a.clientID)
}

// token is MintToken without the scope check, so that the membership probe in
// Authorize can ask for the one token this package mints with no repository
// list at all: `members: read` and nothing else, a credential that cannot read
// a line of code. Every other caller comes through MintToken.
func (a *App) token(ctx context.Context, instID int64, repoNames []string, perms map[string]string) (Token, error) {
	names := slices.Clone(repoNames)
	slices.Sort(names)
	names = slices.Compact(names)
	return a.tokens.get(ctx, a.now, tokenKey(instID, names, perms), func(ctx context.Context) (Token, time.Time, error) {
		auth, err := a.appAuth()
		if err != nil {
			return Token{}, time.Time{}, err
		}
		body := struct {
			Repositories []string          `json:"repositories,omitempty"`
			Permissions  map[string]string `json:"permissions,omitempty"`
		}{names, perms}
		var out struct {
			Token     string    `json:"token"`
			ExpiresAt time.Time `json:"expires_at"`
		}
		op := fmt.Sprintf("minting a token for installation %d", instID)
		if err := a.do(ctx, http.MethodPost, fmt.Sprintf(accessTokensPath, instID), auth, op, body, &out); err != nil {
			return Token{}, time.Time{}, mintError(err, instID, names, a.InstallURL())
		}
		if out.Token == "" {
			return Token{}, time.Time{}, fmt.Errorf("%w: github returned an empty installation token", ErrUpstream)
		}
		if out.ExpiresAt.IsZero() {
			// Refuse rather than guess: the expiry is what the cache and the
			// guest both plan around, and a credential of unknown lifetime is
			// worse than one more request.
			return Token{}, time.Time{}, fmt.Errorf("%w: github returned an installation token with no expiry", ErrUpstream)
		}
		// The audit line, and the only line this package ever writes about a
		// mint. It must stay safe to ship to a log collector, so it names the
		// installation, the scope and the expiry, and never the token.
		a.log.Info("minted a github installation token",
			"installation", instID, "repos", names, "permissions", perms, "expires_at", out.ExpiresAt)
		return Token{Token: out.Token, ExpiresAt: out.ExpiresAt}, out.ExpiresAt.Add(-tokenRefreshLead), nil
	})
}

// membership answers GitHub's org-membership question with an installation
// token, because there is no read:org credential on a gateway to answer it with
// — internal/users/githuborg.go explains at length why one is not kept here.
//
// Both answers are cached, the negative as well as the positive: a user who is
// not in the org retrying in a loop must not turn into a request per retry. A
// 403 is not an answer and is not cached, because it is a permission the
// operator can grant in the next minute.
func (a *App) membership(ctx context.Context, inst Installation, login string) (string, error) {
	key := fmt.Sprintf("%d/%s/%s", inst.ID, strings.ToLower(inst.AccountLogin), strings.ToLower(login))
	return a.members.get(ctx, a.now, key, func(ctx context.Context) (string, time.Time, error) {
		tok, err := a.token(ctx, inst.ID, nil, map[string]string{"members": "read"})
		if err != nil {
			// The mint is where a missing `members: read` actually surfaces,
			// and it surfaces wearing the wrong hat. GitHub answers a request
			// for a permission the installation was never granted with 422,
			// and mintError reads a 422 as "the installation does not cover
			// what was asked for" — true of a repository scope, badly wrong
			// here, where it becomes "the app is not installed on that
			// repository" and sends the operator to reinstall an app that is
			// installed perfectly well. Re-dress it before it escapes, so the
			// same sentence comes out of the mint failure and the 403 below.
			var ae *apiError
			if errors.As(err, &ae) &&
				(ae.status == http.StatusUnprocessableEntity || ae.status == http.StatusForbidden) {
				// Wrap the bare apiError, NOT err: mintError has already
				// decorated err with ErrNotInstalled, and a chain carrying both
				// sentinels is decided by whichever the caller tests first —
				// metadata's mapper tests ErrNotInstalled, so the 404 would win
				// and the sentence below would never be seen.
				return "", time.Time{}, fmt.Errorf("%w: the app cannot read %s's membership — give it the organization `Members: read` permission and have %s accept the updated permissions (%w)",
					ErrForbidden, inst.AccountLogin, inst.AccountLogin, ae)
			}
			return "", time.Time{}, err
		}
		var out struct {
			State string `json:"state"`
		}
		path := fmt.Sprintf(membershipPath, url.PathEscape(inst.AccountLogin), url.PathEscape(login))
		op := fmt.Sprintf("checking %s's membership of %s", login, inst.AccountLogin)
		err = a.do(ctx, http.MethodGet, path, "Bearer "+tok.Token, op, nil, &out)
		var ae *apiError
		switch {
		case err == nil:
			return out.State, a.now().Add(membershipTTL), nil
		case errors.As(err, &ae) && ae.status == http.StatusNotFound:
			// GitHub's way of saying "no such membership". An answer, cached
			// like one.
			return "", a.now().Add(membershipTTL), nil
		case errors.As(err, &ae) && ae.status == http.StatusForbidden:
			// The operator did not grant the App `members: read`. This must
			// never read as "not a member": one is a checkbox on the App's
			// permissions page followed by the org accepting the update, the
			// other is a person to invite, and telling an operator the wrong
			// one costs them an afternoon.
			return "", time.Time{}, fmt.Errorf("%w: the app cannot read %s's membership — give it the organization `Members: read` permission and have %s accept the updated permissions",
				ErrForbidden, inst.AccountLogin, inst.AccountLogin)
		}
		return "", time.Time{}, err
	})
}

// mintError translates the mint endpoint's refusals. 404 and 422 both mean the
// installation does not cover what was asked for — the App was uninstalled, or
// the repository was removed from it — and both are the same fix as a missing
// installation, so they carry the same sentinel and the same URL.
func mintError(err error, instID int64, names []string, installURL string) error {
	var ae *apiError
	if !errors.As(err, &ae) {
		return err
	}
	switch ae.status {
	case http.StatusNotFound:
		return fmt.Errorf("%w: installation %d no longer exists — the app was probably uninstalled; reinstall it at %s (%w)",
			ErrNotInstalled, instID, installURL, err)
	case http.StatusUnprocessableEntity:
		return fmt.Errorf("%w: installation %d does not cover %s — add it at %s (%w)",
			ErrNotInstalled, instID, strings.Join(names, ", "), installURL, err)
	}
	return err
}
