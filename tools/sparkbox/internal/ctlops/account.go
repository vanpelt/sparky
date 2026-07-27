package ctlops

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	xssh "golang.org/x/crypto/ssh"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/edgeauth"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/oidc"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/users"
)

// keyVia labels keys this package links. It is a constant rather than a
// parameter because the value is an audit hint, not policy, and threading a
// transport name through every caller would invite one of them to lie about it.
const keyVia = "ctl"

func (o *Ops) Whoami(ctx context.Context, c Caller) (Whoami, error) {
	const op = "whoami"
	u, err := o.accounts.Get(c.Handle)
	if err != nil {
		return Whoami{}, verbatim(Fail(op, err))
	}
	return Whoami{
		Handle:           u.Handle,
		Status:           u.Status,
		Operator:         u.IsOperator(),
		Email:            u.Email,
		GitHubLogin:      u.GitHubLogin,
		GitHubVerifiedAt: u.GitHubVerifiedAt,
		GitHubVia:        u.GitHubVia,
		Subject:          oidc.SubjectFor(u.Handle),
		KeyFP:            c.KeyFP,
	}, nil
}

func (o *Ops) ListKeys(ctx context.Context, c Caller) ([]KeyInfo, error) {
	const op = "keys.list"
	keys, err := o.accounts.Keys(c.Handle)
	if err != nil {
		return nil, Fail(op, err)
	}
	out := make([]KeyInfo, 0, len(keys))
	for _, k := range keys {
		out = append(out, KeyInfo{
			FP: k.FP, Label: k.Label, Via: k.Via, AddedAt: k.AddedAt,
			Current: c.KeyFP != "" && k.FP == c.KeyFP,
		})
	}
	return out, nil
}

// AddKey parses one authorized_keys line. A malformed line is KindInvalid with
// Exit 1 — the CLI has always answered 1 there where every other bad invocation
// answers 2, and other tooling may key off it — while HTTP correctly sees 400.
func (o *Ops) AddKey(ctx context.Context, c Caller, authorizedKeyLine string) (KeyInfo, error) {
	const op = "keys.add"
	key, comment, _, _, err := xssh.ParseAuthorizedKey([]byte(authorizedKeyLine))
	if err != nil {
		e := Invalid(op, "bad_key", "that isn't a valid authorized_keys line: %v", err)
		e.Exit, e.Status = 1, 400
		return KeyInfo{}, e
	}
	if err := o.accounts.AddKey(c.Handle, key, comment, keyVia); err != nil {
		return KeyInfo{}, verbatim(Fail(op, err))
	}
	fp := xssh.FingerprintSHA256(key)
	o.log.Info("key added", "user", c.Handle, "fp", fp)
	return o.keyInfo(c, fp, comment), nil
}

// keyInfo re-reads the stored row so AddedAt is the store's timestamp — AddKey
// is idempotent, so re-adding an existing key must not claim it was added now.
// A read failure falls back to a synthesized record rather than failing a write
// that already succeeded.
func (o *Ops) keyInfo(c Caller, fp, label string) KeyInfo {
	if keys, err := o.accounts.Keys(c.Handle); err == nil {
		for _, k := range keys {
			if k.FP == fp {
				return KeyInfo{FP: k.FP, Label: k.Label, Via: k.Via, AddedAt: k.AddedAt,
					Current: c.KeyFP != "" && k.FP == c.KeyFP}
			}
		}
	}
	return KeyInfo{FP: fp, Label: label, Via: keyVia, AddedAt: o.now().UTC(),
		Current: c.KeyFP != "" && fp == c.KeyFP}
}

func (o *Ops) RemoveKey(ctx context.Context, c Caller, fp string) error {
	const op = "keys.rm"
	if fp == "" {
		return Invalid(op, "missing_fingerprint", "a key fingerprint is required")
	}
	if err := o.accounts.RemoveKey(c.Handle, fp); err != nil {
		// ErrLastKey is a conflict; a typo'd fingerprint is the masked
		// not-found. Anything else is a store fault the caller can't act on.
		if errors.Is(err, users.ErrLastKey) {
			return AsError(op, err)
		}
		if !o.hasKey(c.Handle, fp) {
			return NotFound(op, "key", fp)
		}
		return verbatim(Fail(op, err))
	}
	o.log.Info("key removed", "user", c.Handle, "fp", fp)
	return nil
}

func (o *Ops) hasKey(handle, fp string) bool {
	keys, err := o.accounts.Keys(handle)
	if err != nil {
		return true // can't tell; don't turn a store fault into a false 404
	}
	for _, k := range keys {
		if k.FP == fp {
			return true
		}
	}
	return false
}

func (o *Ops) ImportGitHubKeys(ctx context.Context, c Caller) (ImportResult, error) {
	const op = "keys.import-github"
	u, err := o.accounts.Get(c.Handle)
	if err != nil {
		return ImportResult{}, Fail(op, err)
	}
	if u.GitHubLogin == "" {
		return ImportResult{}, &Error{
			Kind: KindConflict, Op: op, Code: "github_not_linked",
			Msg:      "no GitHub account linked",
			Hint:     "link one with: github link",
			Verbatim: true,
		}
	}
	// The sharp edge of the whole feature, and the reason the link records HOW
	// it was proved.
	//
	// This verb adopts every key github.com lists for the linked login onto this
	// account, and an adopted key authenticates. So a link established by a
	// channel that could be wrong about which human is on the other end — a
	// third party's signed word for it, say — must not reach this verb: it would
	// let somebody claim a stranger's login, pre-load the stranger's public keys
	// onto their own account, and collect that stranger the next time they
	// connected. Only evidence that came from GitHub itself about the person
	// holding THIS account qualifies.
	if !users.StrongGitHubLink(u.GitHubVia) {
		return ImportResult{}, &Error{
			Kind: KindDenied, Op: op, Code: "github_link_too_weak",
			Msg:      fmt.Sprintf("the link to github.com/%s was not proved directly with GitHub, so it cannot adopt keys", u.GitHubLogin),
			Hint:     "re-link with `github link` (or `keys verify-github`) and run this again.",
			Verbatim: true,
		}
	}
	keys, err := o.github.Fetch(ctx, u.GitHubLogin)
	if err != nil {
		return ImportResult{}, &Error{
			Kind: KindUpstream, Op: op, Code: "github_unreachable",
			Msg: err.Error(), Verbatim: true, Err: err,
		}
	}
	res := ImportResult{Login: u.GitHubLogin, Listed: len(keys), Skipped: []string{}}
	for _, k := range keys {
		switch err := o.accounts.AddKey(c.Handle, k, "github:"+u.GitHubLogin, "github-import"); {
		case err == nil:
			// AddKey is a no-op for keys already on the account, so this counts
			// only genuinely new ones and re-running the import is free.
			res.Imported++
		case errors.Is(err, users.ErrKeyLinked):
			res.Skipped = append(res.Skipped, xssh.FingerprintSHA256(k))
		default:
			return ImportResult{}, Fail(op, err)
		}
	}
	o.log.Info("github keys imported", "user", c.Handle, "login", u.GitHubLogin, "imported", res.Imported)
	return res, nil
}

// VerifyGitHub proves the link by finding one of the caller's ALREADY-REGISTERED
// keys on github.com/<login>.keys. proofFP names which one: the SSH path passes
// the session key's fingerprint, and an HTTP caller must name one explicitly
// because there is no session key to imply. The fingerprint is checked against
// the caller's own key list first — otherwise anyone could nominate a stranger's
// key and claim that stranger's login. An empty login falls back to the stored
// one.
func (o *Ops) VerifyGitHub(ctx context.Context, c Caller, login, proofFP string) (Whoami, error) {
	const op = "keys.verify-github"
	if login == "" {
		if u, err := o.accounts.Get(c.Handle); err == nil {
			login = u.GitHubLogin
		}
	}
	if login == "" {
		return Whoami{}, Invalid(op, "missing_login", "a GitHub login is required")
	}
	if proofFP == "" {
		proofFP = c.KeyFP
	}
	if proofFP == "" {
		return Whoami{}, Invalid(op, "missing_fingerprint",
			"name which of your keys to prove the link with")
	}

	key, err := o.callerKey(op, c, proofFP)
	if err != nil {
		return Whoami{}, err
	}
	ok, err := o.github.Verify(ctx, login, key)
	if err != nil {
		return Whoami{}, &Error{
			Kind: KindUpstream, Op: op, Code: "github_unreachable",
			Msg: err.Error(), Verbatim: true, Err: err,
		}
	}
	if !ok {
		return Whoami{}, Denied(op, "github_key_not_listed",
			fmt.Sprintf("%s isn't listed on github.com/%s.keys — add it there, then retry.", proofFP, login))
	}
	return o.link(ctx, op, c, users.GitHubProfile{Login: login}, users.GitHubViaKeys)
}

// ---------------------------------------------------------------------------
// The device flow
// ---------------------------------------------------------------------------

// StartGitHubLink asks GitHub for a code pair to show the caller.
//
// It is split from the wait because the two halves belong to different actors:
// this one produces something for a human to read, and the human then goes and
// does something in a browser that takes as long as it takes. A single blocking
// call would have nothing to print until it was already over.
//
// Nothing is reserved or written here. An abandoned flow leaves no state on this
// side at all — GitHub expires the code on its own — which is why there is no
// cancel verb and no cleanup to forget.
func (o *Ops) StartGitHubLink(ctx context.Context, c Caller) (users.DeviceCode, error) {
	const op = "github.link"
	if o.ghDevice == nil {
		return users.DeviceCode{}, &Error{
			Kind: KindDisabled, Op: op, Code: "github_device_disabled",
			Msg:      "this host has no GitHub app configured, so it cannot run the browser sign-in",
			Hint:     "link by publishing an SSH key on GitHub instead: keys verify-github <login>",
			Verbatim: true,
		}
	}
	dc, err := o.ghDevice.Start(ctx)
	if err != nil {
		return users.DeviceCode{}, deviceFail(op, err)
	}
	// The user code is logged and the device code never is: the first is a
	// throwaway string a person reads off their screen, the second is the
	// credential that collects the token this flow mints.
	o.log.Info("github device flow started", "user", c.Handle, "user_code", dc.UserCode)
	return dc, nil
}

// FinishGitHubLink waits for the caller to authorize on github.com, then links
// whichever account GitHub says authorized it.
//
// The login is GitHub's answer and never the caller's claim — there is no login
// parameter, deliberately. That is the difference in kind between this and the
// key check: the key check verifies a login somebody typed, and this one is
// TOLD the login by the party that knows. A caller cannot aim it at an account
// they do not control, so there is nothing here to authorize beyond being
// signed in.
//
// It blocks for as long as ctx allows, which is the caller's patience rather
// than GitHub's fifteen minutes; a caller that gives up leaves nothing behind.
func (o *Ops) FinishGitHubLink(ctx context.Context, c Caller, dc users.DeviceCode) (Whoami, error) {
	const op = "github.link"
	if o.ghDevice == nil {
		return Whoami{}, Invalid(op, "github_device_disabled", "no GitHub app is configured on this host")
	}
	if dc.Code == "" {
		return Whoami{}, Invalid(op, "missing_device_code", "that sign-in was never started")
	}
	profile, err := o.ghDevice.Wait(ctx, dc)
	if err != nil {
		return Whoami{}, deviceFail(op, err)
	}
	return o.link(ctx, op, c, profile, users.GitHubViaDevice)
}

// link is the one place a GitHub account becomes this account's, whichever path
// proved it.
//
// It fills in the immutable account number when the proving path did not carry
// one — the key check proves a login and learns nothing else — because a login
// is renameable and, once released, re-registerable by a stranger. That fetch is
// explicitly best-effort: the verification has already happened, and refusing to
// record a proved link because api.github.com was slow would be trading a real
// fact for an optional one.
func (o *Ops) link(ctx context.Context, op string, c Caller, p users.GitHubProfile, via string) (Whoami, error) {
	if p.ID == 0 {
		if full, err := o.github.Profile(ctx, p.Login); err == nil {
			p.ID = full.ID
			if p.Email == "" {
				p.Email = full.Email
			}
		} else {
			o.log.Warn("github profile lookup failed; linking without the account number",
				"user", c.Handle, "login", p.Login, "err", err)
		}
	}
	if err := o.accounts.LinkGitHub(c.Handle, p.Login, via, p.ID); err != nil {
		return Whoami{}, Fail(op, err)
	}
	o.log.Info("github linked", "user", c.Handle, "login", p.Login, "github_id", p.ID, "via", via)
	return o.Whoami(ctx, c)
}

// deviceFail maps the flow's three decisions to error kinds a transport already
// knows how to render, and everything else to an upstream fault. A person
// declining on github.com is not this platform failing, and it must not read
// like it — but it is also not success, so it cannot be swallowed.
func deviceFail(op string, err error) error {
	switch {
	case errors.Is(err, users.ErrDeviceDenied):
		return Denied(op, "github_denied", "that GitHub sign-in was declined; nothing was linked.")
	case errors.Is(err, users.ErrDeviceExpired):
		return &Error{
			Kind: KindConflict, Op: op, Code: "github_code_expired",
			Msg: "that code expired before it was entered", Hint: "run it again for a fresh one.",
			Verbatim: true,
		}
	case errors.Is(err, users.ErrDeviceUnsupported):
		// An operator fault, and the only one here a user can do nothing about.
		return &Error{
			Kind: KindDisabled, Op: op, Code: "github_device_disabled",
			Msg:      "this host's GitHub app does not have the device flow enabled",
			Hint:     "an operator has to turn it on; link with `keys verify-github <login>` meanwhile.",
			Verbatim: true,
		}
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	}
	return &Error{
		Kind: KindUpstream, Op: op, Code: "github_unreachable",
		Msg: err.Error(), Verbatim: true, Err: err,
	}
}

// callerKey resolves a fingerprint to a key the CALLER owns. Resolving through
// the caller's own list is the whole authorization step of VerifyGitHub: a
// fingerprint belonging to someone else is answered with the masked not-found,
// so it cannot be used as an oracle either.
func (o *Ops) callerKey(op string, c Caller, fp string) (xssh.PublicKey, error) {
	keys, err := o.accounts.Keys(c.Handle)
	if err != nil {
		return nil, Fail(op, err)
	}
	for _, k := range keys {
		if k.FP != fp {
			continue
		}
		pub, _, _, _, err := xssh.ParseAuthorizedKey([]byte(k.AuthorizedKey))
		if err != nil {
			return nil, Fail(op, err)
		}
		return pub, nil
	}
	return nil, NotFound(op, "key", fp)
}

func (o *Ops) ListPasskeys(ctx context.Context, c Caller) ([]PasskeyInfo, error) {
	const op = "passkey.list"
	pks, err := o.accounts.Passkeys(c.Handle)
	if err != nil {
		return nil, Fail(op, err)
	}
	out := make([]PasskeyInfo, 0, len(pks))
	for _, p := range pks {
		out = append(out, PasskeyInfo{
			ID: p.ID, Label: p.Label, CreatedAt: p.CreatedAt, LastUsedAt: p.LastUsedAt,
		})
	}
	return out, nil
}

func (o *Ops) RemovePasskey(ctx context.Context, c Caller, idPrefix string) error {
	const op = "passkey.rm"
	if idPrefix == "" {
		return Invalid(op, "missing_id", "a passkey id (or unique prefix) is required")
	}
	switch err := o.accounts.RemovePasskey(c.Handle, idPrefix); {
	case err == nil:
		o.log.Info("passkey removed", "user", c.Handle, "id", idPrefix)
		return nil
	case errors.Is(err, users.ErrNoSuchPasskey):
		return NotFound(op, "passkey", idPrefix)
	case errors.Is(err, users.ErrAmbiguousPasskey):
		e := &Error{
			Kind: KindConflict, Op: op, Code: "passkey_ambiguous",
			Msg:      fmt.Sprintf("%q matches more than one passkey — use more of the id", idPrefix),
			Verbatim: true, Err: err,
		}
		// The matching ids are what a client needs to disambiguate, and they are
		// all the caller's own, so listing them leaks nothing.
		if pks, lerr := o.accounts.Passkeys(c.Handle); lerr == nil {
			var matches []string
			for _, p := range pks {
				if strings.HasPrefix(p.ID, idPrefix) {
					matches = append(matches, p.ID)
				}
			}
			if len(matches) > 0 {
				e.Details = map[string]any{"matches": matches}
			}
		}
		return e
	default:
		return Fail(op, err)
	}
}

func (o *Ops) Email(ctx context.Context, c Caller) (string, error) {
	const op = "email.get"
	u, err := o.accounts.Get(c.Handle)
	if err != nil {
		return "", verbatim(Fail(op, err))
	}
	return u.Email, nil
}

// SetEmail records the address the edge forwards to private apps as
// X-Forwarded-Email; "" clears it.
func (o *Ops) SetEmail(ctx context.Context, c Caller, addr string) (string, error) {
	const op = "email.set"
	addr = strings.TrimSpace(addr)
	// Validating here as well as in the store is what makes a typo a 400 rather
	// than a 500, while keeping the store's sentence so ctl reads the same.
	if addr != "" && !users.ValidEmail(addr) {
		e := Invalid(op, "bad_email", "that doesn't look like an email address")
		e.Exit = 1 // ctl has always answered 1 for a rejected address
		return "", e
	}
	if err := o.accounts.SetEmail(c.Handle, addr); err != nil {
		return "", verbatim(Fail(op, err))
	}
	o.log.Info("email set", "user", c.Handle, "cleared", addr == "")
	return addr, nil
}

// MintSessionToken silently clamps ttl to SessionTokenMaxTTL, as ctl does — a
// week-and-a-day request is a rounding error, not a user error. A ttl <= 0 takes
// DefaultSessionTokenTTL.
func (o *Ops) MintSessionToken(ctx context.Context, c Caller, ttl time.Duration) (TokenResult, error) {
	const op = "session-token"
	if o.sessions == nil {
		return TokenResult{}, Disabled(op, "authenticated forwarding isn't enabled on this host.")
	}
	if ttl <= 0 {
		ttl = DefaultSessionTokenTTL
	}
	if ttl > SessionTokenMaxTTL {
		ttl = SessionTokenMaxTTL
	}
	u, err := o.accounts.Get(c.Handle)
	if err != nil {
		return TokenResult{}, Fail(op, err)
	}
	// A session that can mint its own successor is an unbounded credential, so
	// this is the one place the account's own state has to be re-read. Every
	// other credential-issuing path already refuses a disabled account —
	// users.Store.Lookup will not authenticate its SSH key, and the passkey
	// assertion will not accept its passkey — but an outstanding cookie carries
	// no such check, and the MAC key is fleet-wide, so a renewable token could
	// only be revoked by rotating the OIDC key for everyone.
	if !u.Active() {
		return TokenResult{}, Denied(op, "account_disabled", "this account is disabled.")
	}
	tok, exp, err := o.sessions.Mint(edgeauth.Identity{Handle: c.Handle, Email: u.Email}, ttl)
	if err != nil {
		return TokenResult{}, Fail(op, err)
	}
	o.log.Info("session token minted", "user", c.Handle, "exp", exp)
	return TokenResult{Token: tok, ExpiresAt: exp}, nil
}

// Invite is the ONLY operator-gated operation in ctlops. Operator status is
// resolved from the account store inside this method, never taken from the
// caller, so no transport can assert it.
func (o *Ops) Invite(ctx context.Context, c Caller) (InviteResult, error) {
	const op = "invite"
	u, err := o.accounts.Get(c.Handle)
	if err != nil {
		return InviteResult{}, Fail(op, err)
	}
	if !u.IsOperator() {
		if o.invitesPerUser <= 0 {
			return InviteResult{}, Denied(op, "not_operator", "only operators can mint invite codes here.")
		}
		n, err := o.accounts.InviteCount(c.Handle)
		if err != nil {
			return InviteResult{}, Fail(op, err)
		}
		if n >= o.invitesPerUser {
			e := Denied(op, "invite_quota_exhausted",
				fmt.Sprintf("you've used all %d of your invites.", o.invitesPerUser))
			e.Details = map[string]any{"used": n, "max": o.invitesPerUser}
			return InviteResult{}, e
		}
	}
	code, err := o.accounts.NewInvite(c.Handle)
	if err != nil {
		return InviteResult{}, Fail(op, err)
	}
	o.log.Info("invite minted", "by", c.Handle)
	return InviteResult{Code: code, ExpiresAt: o.now().UTC().Add(users.InviteTTL)}, nil
}

// verbatim marks an error whose Msg is already the whole sentence ctl prints,
// for the handful of store failures the CLI reports as `sparkbox: <err>` rather
// than `sparkbox: <op> failed: <err>`. Keeping those byte-identical is the point
// of the flag.
func verbatim(e *Error) *Error {
	if e != nil {
		e.Verbatim = true
	}
	return e
}
