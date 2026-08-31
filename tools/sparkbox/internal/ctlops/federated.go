package ctlops

// Admitting somebody another service has vouched for.
//
// # How this differs from user.go, which it shares almost all of its body with
//
// `ctl user add <login>` (user.go) is an OPERATOR asserting a GitHub login and
// letting github.com supply the keys. Its header defends that: "The operator is
// trusted to name their own colleagues, and the worst case if they name the
// wrong one is an unused account — not access, because no key the operator
// controls is involved."
//
// This is the same assertion made by a federated identity provider instead of a
// person — HiveMind, today, over internal/edgeauth's handoff. The mechanics are
// therefore identical and are shared (provisionOne's helpers do the work), and
// the two things that differ are worth stating rather than leaving to be
// inferred from the diff:
//
//  1. **There is no Caller.** Every other method here takes one because the
//     transport proved who is asking. Here the thing that was proved is not a
//     caller at all — it is a redeemed handoff naming a GitHub login — and the
//     authorization decision that rides on it (which orgs may come in) is
//     policy the edge holds, not policy this package can restate. So the caller
//     is absent on purpose, and the doc comment on the method is the contract:
//     whoever calls it has already established the login.
//
//  2. **It may create an account with no key.** `user add` reports
//     OutcomeNoKeys and moves on, because an operator syncing 200 colleagues
//     wants the ones who publish keys and nothing else. A person who has just
//     clicked a link and is waiting on a page cannot be told "no" for a reason
//     they have never heard of, so the keyless account (users.CreateKeyless) is
//     the floor, and the published keys are the upgrade.
//
// The provenance rule from docs/github-linking-design.md §2.3 is what makes
// that upgrade honest rather than generous. An account created from published
// keys holds nothing BUT those keys, so `github-keys` is a true statement about
// it — the same statement `user add` makes. An account created keyless holds no
// evidence from GitHub at all, so its link is `assertion`, which
// users.StrongGitHubLink refuses and internal/metadata keeps out of the
// `github` claim. Nothing here relaxes that gate, and nothing here should.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/edgeauth"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/users"
)

// FederatedInviter is what InvitedBy records for an account admitted this way.
//
// It is not users.OperatorInviter, and that is the same load-bearing
// distinction user.go's header draws: that constant is what IsOperator() tests,
// and it carries node administration and, through proxy.mayView, the ability to
// open any user's private sandbox URLs. A federated sign-in must not be able to
// mint one. It is not a real handle either — ValidHandle refuses the '@' — so
// it can never collide with an account that could be logged into.
const FederatedInviter = "federated@hivemind"

// Admission is edgeauth.Admission, aliased so a reader of this file does not
// have to go looking for the return type of the method below.
//
// The struct is declared in internal/edgeauth rather than here, which is the
// wrong way round for a producer and is deliberate: ctlops already imports
// edgeauth (for the session *Signer), so edgeauth cannot import ctlops without
// a cycle, and the door is the only consumer this result will ever have. Put
// it there and *Ops satisfies edgeauth's Admitter structurally, with no adapter
// in cmd/sparkbox to keep in sync — which is the same trade every other narrow
// interface in ops.go makes.
type Admission = edgeauth.Admission

// AdmitGitHubLogin resolves a GitHub login, vouched for by a federated identity
// provider, to the sparkbox account that login should sign in as — creating one
// if there is none.
//
// THE CALLER MUST HAVE VERIFIED THE LOGIN. This method performs no
// authentication and applies no membership policy; it takes "the person at the
// keyboard is github.com/<login>" as settled and does the account half. The
// only caller today is internal/edgeauth's handoff handler, which redeems a
// single-use code from HiveMind and applies the operator's org allowlist before
// it gets here.
//
// It is idempotent in the way a sign-in door needs: a second click for an
// account that already exists adopts any newly-published keys and returns the
// same handle, rather than refusing because the account is there.
func (o *Ops) AdmitGitHubLogin(ctx context.Context, login, email string) (Admission, error) {
	const op = "signin.admit"
	login = strings.TrimSpace(login)
	if !users.ValidGitHubLogin(login) {
		return Admission{}, Invalid(op, "bad_login", "%q is not a GitHub login", login)
	}
	handle := users.HandleForGitHubLogin(login)
	if handle == "" {
		// Reachable: a login may be longer than a handle may be, and it may be
		// a reserved name. Neither is the visitor's fault and neither has a
		// remedy they can apply, so the sentence names the operator's door.
		return Admission{}, verbatim(Invalid(op, "no_handle",
			"no sparkbox account name can be derived from github.com/%s — "+
				"an operator has to create one with `ctl user add`", login))
	}

	existing, err := o.accounts.Get(handle)
	switch {
	case errors.Is(err, users.ErrNoSuchUser):
		return o.admitNew(ctx, op, handle, login, email)
	case err != nil:
		return Admission{}, Fail(op, err)
	}

	// The handle is taken. It is only THIS person's if the account already
	// carries this GitHub login — otherwise it belongs to a stranger who
	// happens to have picked the name, and signing the visitor into it would be
	// handing them somebody else's sandboxes, secrets and repositories. This is
	// provisionOne's rule (user.go), and it is the check that keeps a handle
	// collision from being an account takeover.
	if !strings.EqualFold(existing.GitHubLogin, login) {
		return Admission{}, verbatim(&Error{
			Kind: KindConflict, Op: op, Code: "handle_taken", Verbatim: true,
			Msg: fmt.Sprintf("the sparkbox account %q already belongs to somebody else — "+
				"an operator has to sort this out before github.com/%s can sign in", handle, login),
		})
	}
	if !existing.Active() {
		// Same sentence edgeauth.Require gives an outstanding cookie on a
		// disabled account, for the same reason: a door that said "disabled" in
		// two different words would read as two different states.
		return Admission{}, verbatim(&Error{
			Kind: KindDenied, Op: op, Code: "account_disabled", Verbatim: true,
			Msg: "sparkbox: this account is disabled",
		})
	}

	// Fill in an email the account is missing, and never overwrite one it has.
	// The account's own record is what `ctl session-token` mints from, so an
	// address that only ever rode the federated session would make
	// X-Forwarded-Email appear and disappear depending on which door was used.
	// Never overwriting is the other half: the address somebody set here is
	// theirs, and a federated sign-in is not a reason to replace it.
	if existing.Email == "" && users.ValidEmail(email) {
		if err := o.accounts.SetEmail(handle, email); err != nil {
			o.log.Warn("could not record email during federated sign-in", "handle", handle, "err", err)
		}
	}

	// Adopt anything github.com has started publishing since last time. This is
	// what makes a re-click the way a new laptop key reaches the platform, and
	// it is also how an account that came in keyless earns a strong link the
	// first time its owner publishes a key.
	admission := Admission{Handle: handle, Strong: users.StrongGitHubLink(existing.GitHubVia)}
	keys, err := o.github.Fetch(ctx, login)
	if err != nil {
		// Not fatal, and deliberately so: github.com being slow must not stop a
		// person signing in to an account that already exists. The key list is
		// an upgrade, and the next click retries it.
		o.log.Warn("could not read published github keys during federated sign-in",
			"handle", handle, "login", login, "err", err)
		held, kerr := o.accounts.Keys(handle)
		if kerr == nil {
			admission.Keys = len(held)
		}
		return admission, nil
	}
	if len(keys) > 0 {
		if _, note := o.adoptKeys(handle, login, keys); note != "" {
			o.log.Warn("adopting published github keys during federated sign-in",
				"handle", handle, "login", login, "err", note)
		}
		// The link is re-recorded rather than left alone: an account admitted
		// keyless carries `assertion`, and now that it holds keys github.com
		// publishes for this login, `github-keys` is the true statement about
		// it. linkProvisioned writes exactly that.
		if !admission.Strong {
			o.linkProvisioned(ctx, handle, login)
			if u, err := o.accounts.Get(handle); err == nil {
				admission.Strong = users.StrongGitHubLink(u.GitHubVia)
			}
		}
	}
	held, err := o.accounts.Keys(handle)
	if err == nil {
		admission.Keys = len(held)
	}
	o.log.Info("federated sign-in resolved an existing account",
		"handle", handle, "login", login, "keys", admission.Keys, "strong", admission.Strong)
	return admission, nil
}

// admitNew creates the account, preferring the keys github.com publishes and
// falling back to no key at all.
func (o *Ops) admitNew(ctx context.Context, op, handle, login, email string) (Admission, error) {
	keys, err := o.github.Fetch(ctx, login)
	if err != nil {
		// Unlike the existing-account path above, this one cannot shrug: there
		// is no account yet, and creating a keyless one because github.com
		// timed out would permanently record `assertion` provenance for
		// somebody who publishes keys and would have got `github-keys`. A
		// retry is one click and gets the right answer.
		return Admission{}, verbatim(&Error{
			Kind: KindUpstream, Op: op, Code: "github_unreachable", Verbatim: true,
			Msg: "github.com could not be reached to set this account up — try the link again",
			Err: err,
		})
	}

	if len(keys) == 0 {
		// No published key: the account exists, in the browser, and says so.
		if err := o.accounts.CreateKeyless(handle, FederatedInviter); err != nil {
			return Admission{}, admitCreateError(op, handle, err)
		}
		// `assertion` is the weak provenance, and recording it is the honest
		// thing rather than the generous one — see this file's header and
		// docs/github-linking-design.md §2.3. It is recorded at all so the
		// console can show whose account this is and offer the device flow.
		if err := o.accounts.LinkGitHub(handle, login, users.GitHubViaAssertion, o.githubID(ctx, login)); err != nil {
			o.log.Warn("could not record asserted github link", "handle", handle, "login", login, "err", err)
		}
		o.setAdmittedEmail(handle, email)
		o.log.Info("federated sign-in created a keyless account",
			"handle", handle, "login", login, "via", users.GitHubViaAssertion)
		return Admission{Handle: handle, Created: true}, nil
	}

	// Published keys: this is `ctl user add`, with the identity provider where
	// the operator normally stands. The account holds nothing but the keys
	// github.com publishes for this login, so the link is `github-keys`.
	if err := o.accounts.Create(handle, keys[0], "github:"+login, "github-import", FederatedInviter); err != nil {
		return Admission{}, admitCreateError(op, handle, err)
	}
	added, note := o.adoptKeys(handle, login, keys[1:])
	if note != "" {
		o.log.Warn("adopting remaining github keys during federated sign-in",
			"handle", handle, "login", login, "err", note)
	}
	o.linkProvisioned(ctx, handle, login)
	o.setAdmittedEmail(handle, email)
	strong := false
	if u, err := o.accounts.Get(handle); err == nil {
		strong = users.StrongGitHubLink(u.GitHubVia)
	}
	o.log.Info("federated sign-in created an account from published github keys",
		"handle", handle, "login", login, "keys", added+1, "strong", strong)
	return Admission{Handle: handle, Created: true, Strong: strong, Keys: added + 1}, nil
}

// setAdmittedEmail records an email on an account that has just been created.
// Best-effort in the same sense linkProvisioned is: an account without an
// address works, and refusing a sign-in because a display string would not
// store would be trading the thing that matters for the thing that does not.
func (o *Ops) setAdmittedEmail(handle, email string) {
	if !users.ValidEmail(email) {
		return
	}
	if err := o.accounts.SetEmail(handle, email); err != nil {
		o.log.Warn("could not record email during federated sign-in", "handle", handle, "err", err)
	}
}

// githubID is the account number, best-effort, for the keyless path — which has
// no linkProvisioned to fetch it. Zero when github.com will not say, which is
// the same "unknown" every link made before the profile fetch existed carries.
func (o *Ops) githubID(ctx context.Context, login string) int64 {
	p, err := o.github.Profile(ctx, login)
	if err != nil {
		o.log.Warn("could not fetch github profile during federated sign-in", "login", login, "err", err)
		return 0
	}
	return p.ID
}

// admitCreateError renders the two races Create can lose. Both mean somebody
// else got the name between the Get above and the Create here, and both are
// worth distinguishing from an internal fault because neither is one.
func admitCreateError(op, handle string, err error) error {
	switch {
	case errors.Is(err, users.ErrHandleTaken):
		return verbatim(&Error{
			Kind: KindConflict, Op: op, Code: "handle_taken", Verbatim: true,
			Msg: fmt.Sprintf("the sparkbox account %q was claimed while this ran — try the link again", handle),
		})
	case errors.Is(err, users.ErrKeyLinked):
		return verbatim(&Error{
			Kind: KindConflict, Op: op, Code: "key_linked", Verbatim: true,
			Msg: "a key github.com publishes for this login already belongs to another sparkbox account — " +
				"an operator has to sort this out",
		})
	default:
		return Fail(op, err)
	}
}
