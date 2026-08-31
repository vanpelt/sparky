package edgeauth

// The federated sign-in door: somebody already signed in to HiveMind clicks a
// button there and arrives here, signed in as themselves, with an account made
// for them if they had none.
//
// # Why this lives on the login handler and not on a door of its own
//
// Because it is a login. Everything a sign-in has to get right on this host is
// already written a few lines up in login.go — what a session cookie looks like
// (setSessionCookie), which return URLs belong to us (safeReturn), and the
// one-time passkey offer that gives a first-time visitor a credential they can
// come back with. A second package would have needed all three exported, and a
// second implementation of any of them is how one door ends up scoped to
// `.<domain>` while another quietly is not.
//
// # Why the POST carries no CSRF proof, and why that is not an oversight
//
// internal/launch/csrf.go and RequireMutation both refuse a state-changing POST
// that cannot show a first-party Origin. This handler refuses nothing of the
// sort, and the reason is that those checks exist to stop a cross-site page
// spending AMBIENT authority — a cookie the browser attaches whether or not the
// visitor meant it. There is no ambient authority in this request. Its entire
// authority is a single-use code in its body, which HiveMind minted for one
// person and will honour exactly once. A cross-site page that posts here
// without a code achieves nothing, and one that posts here WITH a code is
// holding a credential, which is a different problem (see below) that an Origin
// header cannot touch — the attacker sends whichever Origin they like, or none.
//
// Adding the check anyway would be worse than leaving it out. It would read as
// a guarantee, it would break the moment HiveMind set `Referrer-Policy:
// no-referrer` (which makes Firefox send `Origin: null` — the exact scar
// internal/launch carries), and it would tempt the next reader to relax the
// code's single-use rule because "the Origin check has us covered".
//
// # The residual risk this door has, stated plainly
//
// An unsolicited cross-site POST is the shape of every IdP-initiated SSO, and
// it brings login CSRF with it: somebody who holds a valid handoff code for
// THEIR account can make a victim's browser spend it, and the victim then works
// — sets a secret, attaches a repository, opens a sandbox — inside an account
// the attacker can read. Three things narrow it here:
//
//  1. HiveMind fixes `dest` when it mints the code, so a stolen handoff cannot
//     be re-aimed at a destination the attacker chose.
//  2. A handoff never silently REPLACES a session. Landing on somebody who is
//     already signed in as a different handle raises the interstitial, which
//     turns a silent swap into a question with two named accounts in it.
//  3. A handoff never silently CREATES an account. The first one asks too, which
//     is the moment the visitor sees the handle and the GitHub login they are
//     about to become.
//
// What is left is the case where the victim has no session and already has an
// account: that one goes through, and the mitigation is the code's sixty-second
// life and its single use. Closing it properly needs a sparkbox-initiated flow
// with a state nonce, which costs the click-a-button-in-HiveMind ergonomics
// this whole door exists for. See docs/hivemind-signin-design.md §4.1.

import (
	"context"
	_ "embed"
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/hivemindsignin"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/users"
)

//go:embed handoff.html
var handoffHTML string

var handoffTpl = template.Must(template.New("handoff").Parse(handoffHTML))

// handoffView is everything the one template renders. Three pages come out of
// it — the question, the notice, the refusal — because they are the same card
// with different fields filled in, and three templates that had to stay in
// visual sync would not.
//
// Every field is plain text and html/template escapes all of them. That matters
// more here than on most pages: Body and Detail carry a GitHub login and an
// error sentence that originated off this host.
type handoffView struct {
	Heading string
	Body    string
	Detail  string
	// Ticket, when set, renders the confirm form.
	Ticket  string
	Confirm string
	// StayAs names the account a visitor is already signed in as, offering the
	// door back out of a switch they did not ask for.
	StayAs string
	Dest   string
	// Continue renders a plain link forward — the notice page, where the
	// session is already established and there is nothing left to submit.
	Continue string
	Failed   bool
}

// handoffPage renders one card at the given status.
//
// The status is a parameter rather than the caller's own WriteHeader because
// the order is load-bearing and easy to get wrong: every header — the content
// type, the no-store, and on the notice page the Set-Cookie that has already
// been queued — is dropped if WriteHeader has run first. Passing the status in
// means there is one place that gets the sequence right.
func (h *LoginHandler) handoffPage(w http.ResponseWriter, status int, v handoffView) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := handoffTpl.Execute(w, v); err != nil && h.cfg.Logger != nil {
		h.cfg.Logger.Error("handoff page render failed", "err", err)
	}
}

// HandoffPath is where HiveMind posts. Named here because it is half of a
// contract with another codebase: the form action rendered by the HiveMind
// dashboard has to match it, and a literal in two repositories is a literal
// that drifts.
const HandoffPath = "/handoff"

// handoffBudget bounds one sign-in end to end — the redeem, the github.com key
// read, and the account write. A visitor is watching an empty page for all of
// it, so it is short; it is not shorter because the github.com half is the slow
// half and failing a first sign-in over a sluggish upstream would strand
// somebody with no account and no idea why.
const handoffBudget = 25 * time.Second

// Admission is what an Admitter resolved a vouched-for GitHub login to.
//
// It is declared here rather than in internal/ctlops, which produces it,
// because ctlops already imports this package for *Signer and the reverse would
// be an import cycle. See the alias and its reasoning in ctlops/federated.go.
type Admission struct {
	// Handle is the sparkbox account. Always set on success.
	Handle string
	// Created is true when this admission brought the account into existence.
	Created bool
	// Strong reports whether the account's GitHub link is one
	// users.StrongGitHubLink admits. False means github.com publishes no ssh
	// key for the login, so the account is browser-only: it cannot attach
	// repositories and its sandboxes carry no `github` claim. The door says so
	// out loud rather than letting somebody discover it at the launch page.
	Strong bool
	// Keys is how many ssh keys the account holds afterwards.
	Keys int
}

// Admitter turns a GitHub login somebody has vouched for into an account.
// *ctlops.Ops satisfies it. It is called only after Redeemer has spoken and the
// org allowlist has passed — this interface asserts nothing about who asked.
type Admitter interface {
	AdmitGitHubLogin(ctx context.Context, login, email string) (Admission, error)
}

// Redeemer spends a single-use handoff code. *hivemindsignin.Redeemer satisfies
// it.
type Redeemer interface {
	Redeem(ctx context.Context, code string) (hivemindsignin.Claims, error)
}

// HandoffConfig turns the federated door on. Every field is required; a nil
// HandoffConfig on LoginConfig means the routes are not registered at all,
// which is what a host that has not configured this serves — not an endpoint
// that accepts anything.
type HandoffConfig struct {
	// Redeem is the back channel to the identity provider.
	Redeem Redeemer
	// Admit resolves the vouched-for login to an account.
	Admit Admitter
	// Accounts answers one question: does this handle exist yet? That is
	// presentation, not policy — it decides whether the visitor is shown the
	// "create an account?" page — and every refusal still comes from Admit, so
	// there is exactly one place that knows when a handle may not be used.
	Accounts Accounts
	// Orgs is the GitHub organisation allowlist. A visitor must be in at least
	// one. An empty slice is a configuration error and NewLoginHandler refuses
	// it: a door whose allowlist matched everything would admit every HiveMind
	// user on the internet, and "the operator left the list empty" must never
	// be the way that happens.
	Orgs []string
}

// handoffRoutes registers the door on the login mux when it is configured.
func (h *LoginHandler) handoffRoutes(mux *http.ServeMux) {
	if h.cfg.Handoff == nil {
		return
	}
	mux.HandleFunc("POST "+HandoffPath, h.handoff)
	mux.HandleFunc("POST "+HandoffPath+"/confirm", h.handoffConfirm)
}

// handoff receives the browser's cross-site POST: redeem, authorize, and either
// sign the visitor in or ask them the one question this handoff needs.
func (h *LoginHandler) handoff(w http.ResponseWriter, r *http.Request) {
	cfg := h.cfg.Handoff
	w.Header().Set("Cache-Control", "no-store")
	if err := r.ParseForm(); err != nil {
		h.handoffFail(w, "That sign-in link was malformed.", "parse form", err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), handoffBudget)
	defer cancel()

	claims, err := cfg.Redeem.Redeem(ctx, r.PostForm.Get("code"))
	switch {
	case errors.Is(err, hivemindsignin.ErrRefused):
		// Unknown, spent, expired: one sentence for all three, because the
		// person holding it cannot tell them apart and the remedy is the same.
		h.handoffFail(w, "That sign-in link has already been used or has expired. "+
			"Go back to HiveMind and click it again.", "redeem refused", nil)
		return
	case err != nil:
		h.handoffFail(w, "HiveMind could not be reached to complete this sign-in. "+
			"Try again in a moment.", "redeem failed", err)
		return
	}

	if !h.handoffOrgOK(claims) {
		// Names the requirement, not the visitor's orgs: the list HiveMind
		// returned is theirs, and reflecting it back onto a page reached by an
		// unauthenticated POST would echo one person's memberships to whoever
		// made the request.
		h.handoffFail(w, "This sparkbox is limited to members of "+
			strings.Join(cfg.Orgs, ", ")+" on GitHub.", "org refused", nil)
		h.logf("handoff refused: not in an allowed org", "login", claims.GitHub)
		return
	}

	handle := users.HandleForGitHubLogin(claims.GitHub)
	if handle == "" {
		h.handoffFail(w, "No sparkbox account name can be made from github.com/"+
			claims.GitHub+". An operator has to create one.", "no handle", nil)
		return
	}

	// safeReturn is applied HERE, once, and the result is what travels on — into
	// the ticket, into the interstitial's form, into the Location. HiveMind
	// chose this URL and HiveMind is not entitled to send a browser off our own
	// zone; running the guard once and carrying the sanitised value means no
	// later step can be the one that forgot.
	dest := h.safeReturn(claims.Dest)

	// Already signed in as this very account: nothing to decide, and nothing to
	// ask. Deliberately no admission call on this path — it would spend a
	// github.com round trip on every click of a button people press often, and
	// the key-adoption it would do happens on the next sign-in that actually
	// mints a session.
	if current, ok := h.cfg.Signer.IdentityFrom(r); ok && current.Handle == handle {
		http.Redirect(w, r, dest, http.StatusSeeOther)
		return
	} else if ok {
		// Signed in as somebody else. Ask, rather than swapping underneath them.
		h.handoffAsk(w, r, claims, handle, dest, current.Handle, false)
		return
	}

	// Does the account exist? Only to choose the page; every refusal still
	// comes from the admission below.
	if _, err := cfg.Accounts.Get(handle); err != nil {
		if !errors.Is(err, users.ErrNoSuchUser) {
			h.handoffFail(w, "This sparkbox could not read its account list.", "accounts.Get", err)
			return
		}
		h.handoffAsk(w, r, claims, handle, dest, "", true)
		return
	}

	h.handoffAdmit(ctx, w, r, Ticket{
		Handle: handle, Login: claims.GitHub, Email: claims.Email, Dest: dest,
	})
}

// handoffConfirm is the interstitial's answer: the visitor said yes, so do the
// thing the first request declined to do silently.
//
// The ticket is the whole authority, exactly as the code was for /handoff, and
// for the same reason there is no Origin check: a signed, short-lived, purpose-
// separated credential in the body is what says this is allowed.
func (h *LoginHandler) handoffConfirm(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if err := r.ParseForm(); err != nil {
		h.handoffFail(w, "That form was malformed.", "parse form", err)
		return
	}
	ticket, ok := h.cfg.Signer.VerifyTicket(r.PostForm.Get("ticket"))
	if !ok {
		h.handoffFail(w, "That took too long. Go back to HiveMind and click the link again.",
			"ticket rejected", nil)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), handoffBudget)
	defer cancel()
	h.handoffAdmit(ctx, w, r, ticket)
}

// handoffAdmit resolves the account and signs the visitor in.
func (h *LoginHandler) handoffAdmit(ctx context.Context, w http.ResponseWriter, r *http.Request, t Ticket) {
	admission, err := h.cfg.Handoff.Admit.AdmitGitHubLogin(ctx, t.Login, t.Email)
	if err != nil {
		// ctlops errors carry a sentence written for a person; the door shows
		// it rather than inventing a second vocabulary for the same refusals.
		h.handoffFail(w, err.Error(), "admission refused", err)
		return
	}
	if admission.Handle != t.Handle {
		// Cannot happen: both are users.HandleForGitHubLogin of the same login.
		// Refused rather than trusted, because the one thing this door must
		// never do is mint a session for an account nobody decided on.
		h.handoffFail(w, "This sign-in could not be completed.", "handle disagreement", nil)
		return
	}

	token, _, err := h.cfg.Signer.Mint(Identity{Handle: admission.Handle, Email: t.Email}, h.cfg.TTL)
	if err != nil {
		h.handoffFail(w, "This sparkbox could not issue a session.", "mint", err)
		return
	}
	h.setSessionCookie(w, token)
	h.logf("federated session established", "handle", admission.Handle,
		"login", t.Login, "created", admission.Created, "strong", admission.Strong)

	next := h.handoffNext(admission.Handle, t.Dest)
	if admission.Created && !admission.Strong {
		// A brand-new account with no ssh key behind it. This is the one thing
		// a visitor genuinely needs told — it is why the launch door will not
		// have a repository for them — and telling them at the moment it
		// becomes true beats letting them find out at a page that can only say
		// "nothing of yours is attached".
		h.handoffPage(w, http.StatusOK, handoffView{
			Heading:  "You're signed in as " + admission.Handle,
			Body:     "github.com publishes no SSH key for " + t.Login + ", so this account starts out browser-only — sparkbox can't attach GitHub repositories to it yet.",
			Detail:   "To fix that, run  ssh " + h.portFlag() + "ctl@" + h.cfg.Gateway + " github link  from a terminal, or publish an SSH key on GitHub and sign in here again.",
			Continue: next,
		})
		return
	}
	http.Redirect(w, r, next, http.StatusSeeOther)
}

// handoffNext is where a completed sign-in lands: the passkey offer when the
// account has none, otherwise the destination.
//
// The offer matters more here than on any other door. A federated account may
// hold no ssh key at all, which makes a passkey the only credential its owner
// can come back with when HiveMind is not in front of them.
func (h *LoginHandler) handoffNext(handle, dest string) string {
	if h.wa == nil {
		return dest
	}
	if has, err := h.cfg.Passkeys.HasPasskeys(handle); err != nil || has {
		return dest
	}
	return "/enroll?return=" + url.QueryEscape(dest)
}

// handoffAsk renders the interstitial and mints the ticket that carries the
// redeemed handoff across it.
func (h *LoginHandler) handoffAsk(
	w http.ResponseWriter, r *http.Request,
	claims hivemindsignin.Claims, handle, dest, current string, create bool,
) {
	ticket, err := h.cfg.Signer.MintTicket(Ticket{
		Handle: handle, Login: claims.GitHub, Email: claims.Email, Dest: dest, Create: create,
	})
	if err != nil {
		h.handoffFail(w, "This sign-in could not be completed.", "mint ticket", err)
		return
	}
	view := handoffView{Ticket: ticket, Dest: dest}
	switch {
	case current != "":
		view.Heading = "Switch to " + handle + "?"
		view.Body = "You're signed in to sparkbox as " + current + ". This link from HiveMind signs you in as " +
			handle + " instead, for github.com/" + claims.GitHub + "."
		view.Detail = "If you weren't expecting this, stay as " + current + "."
		view.Confirm = "Sign in as " + handle
		view.StayAs = current
	default:
		view.Heading = "Create a sparkbox account?"
		view.Body = "HiveMind says you're github.com/" + claims.GitHub +
			". sparkbox has no account for you yet — this creates one named " + handle + "."
		view.Confirm = "Create " + handle + " and continue"
	}
	h.handoffPage(w, http.StatusOK, view)
}

// handoffOrgOK applies the operator's allowlist.
func (h *LoginHandler) handoffOrgOK(claims hivemindsignin.Claims) bool {
	for _, org := range h.cfg.Handoff.Orgs {
		if claims.HasOrg(org) {
			return true
		}
	}
	return false
}

// handoffFail renders one refusal page and logs the real reason.
//
// The split is the point: `shown` is a sentence for a person and never names
// which internal check failed, because this page is reached by an
// unauthenticated cross-site POST and is read by whoever sent it. `why` and err
// go to the operator's log, where they are the only record of why a sign-in
// did not happen.
func (h *LoginHandler) handoffFail(w http.ResponseWriter, shown, why string, err error) {
	h.logf("handoff refused", "reason", why, "err", err)
	h.handoffPage(w, http.StatusForbidden, handoffView{
		Heading: "Can't sign you in", Body: shown, Failed: true,
	})
}

func (h *LoginHandler) logf(msg string, args ...any) {
	if h.cfg.Logger != nil {
		h.cfg.Logger.Info(msg, args...)
	}
}

// portFlag renders the -p<port> the mint instructions need when the gateway is
// not on 22, shared with the login page so the two cannot print different
// commands for the same host.
func (h *LoginHandler) portFlag() string {
	if h.cfg.GatewayPort == 0 || h.cfg.GatewayPort == 22 {
		return ""
	}
	return "-p" + strconv.Itoa(h.cfg.GatewayPort) + " "
}
