package ctlops

// Operator-driven account provisioning from GitHub.
//
// # Why this exists
//
// The three doors into this platform were: seeded in users.conf (which makes an
// operator), an invite code typed into `ssh signup@`, or --open-signup (which
// admits the internet). Onboarding a team through the middle one is a code per
// person, delivered out of band, expiring in a week, each redeemed through an
// interactive dialog. That is fine for one guest and does not scale to a
// company.
//
// # Why importing published keys is a sound way in
//
// It is the same claim the platform already accepts from `ctl keys
// verify-github`, made in the other direction. GitHub publishes an account's
// public keys at github.com/<login>.keys, and holding the private half of one
// is what GitHub itself accepts to authenticate a git push. So an account whose
// keys are exactly what GitHub publishes for <login> can be authenticated by
// exactly one person: whoever GitHub would let push as <login>.
//
// The direction reversal is the only genuinely new thing. `verify-github` is a
// user proving a login they typed; this is an operator asserting a login and
// letting GitHub supply the keys. The operator is trusted to name their own
// colleagues, and the worst case if they name the wrong one is an unused
// account — not access, because no key the operator controls is involved.
//
// # What a provisioned account is NOT
//
// It is not an operator. Accounts land with InvitedBy set to the provisioning
// operator's handle, never users.OperatorInviter, and the distinction is load
// bearing: IsOperator() gates node administration and, via proxy.mayView, the
// ability to open ANY user's private sandbox URLs. Seeding a colleague into
// users.conf — the obvious shortcut — would hand them the fleet. This is the
// door that does not.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	xssh "golang.org/x/crypto/ssh"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/users"
)

// Provision outcomes, as reported per login. Strings rather than an enum
// because their only consumer is a terminal column and a JSON field.
const (
	// OutcomeCreated: the account did not exist and now does.
	OutcomeCreated = "created"
	// OutcomeKeysAdded: the account existed and gained keys GitHub lists that
	// it did not have. Re-running a sync is how a colleague's new laptop key
	// reaches the platform.
	OutcomeKeysAdded = "keys-added"
	// OutcomeCurrent: the account exists and already holds every listed key.
	OutcomeCurrent = "current"
	// OutcomeNoKeys: GitHub publishes no usable key for the login. Nothing is
	// wrong with the account — this person simply has to come in through
	// `ssh signup@` with an invite, or publish a key.
	OutcomeNoKeys = "no-keys"
	// OutcomeSkipped: something about the login makes it unusable here — a
	// handle that cannot be derived, a name already taken by somebody else, or
	// a key another account already claims.
	OutcomeSkipped = "skipped"
	// OutcomeFailed: GitHub or the database refused. Recorded per login rather
	// than aborting, so one unreachable profile does not sink a sync of 200.
	OutcomeFailed = "failed"
)

// ProvisionedUser is one login's result.
type ProvisionedUser struct {
	Login   string `json:"login"`
	Handle  string `json:"handle,omitempty"`
	Keys    int    `json:"keys"`
	Outcome string `json:"outcome"`
	Note    string `json:"note,omitempty"`
}

// ProvisionResult is the whole run. Counts are derived from Users but carried
// explicitly so a caller can print a summary without re-tallying.
type ProvisionResult struct {
	Org      string            `json:"org,omitempty"`
	DryRun   bool              `json:"dry_run"`
	Users    []ProvisionedUser `json:"users"`
	Created  int               `json:"created"`
	Updated  int               `json:"updated"`
	Skipped  int               `json:"skipped"`
	Examined int               `json:"examined"`
}

// maxProvisionLogins bounds one run: a typo must not start a thousand requests
// to github.com, and a company org is not a guest list.
//
// A real org overshoots this easily — the one this was built for has 617
// members, nearly all of whom will never open a sandbox — and that refusal is
// the intended outcome, not a limit to raise. The escape hatch is --team, which
// names the group that actually wants the thing, or `user add` for the handful
// of people who asked.
const maxProvisionLogins = 200

// orgLabel names the roster being read — the org, or "org/team" when narrowed —
// so a message and a result field can say which without either re-deriving it.
// Unquoted: it is data, and the messages that want quotes use %q.
func orgLabel(org, team string) string {
	if team == "" {
		return org
	}
	return org + "/" + team
}

// ProvisionGitHubOrg creates an account for every member of org who publishes
// an SSH key, using the operator's own GitHub token to read the roster.
//
// The token arrives as an argument and is never stored, logged, or echoed. That
// is the design's central trade — see internal/users/githuborg.go for why the
// alternative (a standing read:org credential on the gateway) was rejected, and
// for what a snapshot-shaped membership check does and does not promise.
func (o *Ops) ProvisionGitHubOrg(ctx context.Context, c Caller, org, team, token string, dryRun bool) (ProvisionResult, error) {
	const op = "user.sync-github-org"
	if err := o.operatorOnly(op, c, "only operators can provision accounts."); err != nil {
		return ProvisionResult{}, err
	}
	if !users.ValidGitHubOrg(org) {
		return ProvisionResult{}, Invalid(op, "bad_org", "%q is not a GitHub organization name", org)
	}
	if team != "" && !users.ValidGitHubTeam(team) {
		return ProvisionResult{}, Invalid(op, "bad_team", "%q is not a GitHub team slug", team)
	}
	if strings.TrimSpace(token) == "" {
		return ProvisionResult{}, Invalid(op, "missing_token",
			"a GitHub token with read:org is required — pipe one in, e.g. `gh auth token | ssh …`")
	}
	logins, err := o.orgMembers(ctx, org, team, token)
	if err != nil {
		return ProvisionResult{}, verbatim(&Error{
			Kind: KindUpstream, Op: op, Code: "github_unreachable", Msg: err.Error(), Err: err,
		})
	}
	if len(logins) == 0 {
		// Distinguished from a clean run because it usually is not one: a token
		// without read:org sees only public members, and public membership is
		// opt-in and rare. Reporting "0 accounts, all good" would send the
		// operator looking for the bug in the wrong place.
		return ProvisionResult{}, verbatim(Invalid(op, "no_members",
			"github listed no members of %q for this token — it needs the read:org scope "+
				"(and SAML authorization, if the org enforces it)", orgLabel(org, team)))
	}
	if len(logins) > maxProvisionLogins {
		return ProvisionResult{}, verbatim(Invalid(op, "org_too_large",
			"%q has %d members, more than this will provision at once (%d). "+
				"a team is usually the group that actually wants this: --team <slug>",
			orgLabel(org, team), len(logins), maxProvisionLogins))
	}
	res, err := o.provision(ctx, op, c, logins, dryRun)
	if err != nil {
		return ProvisionResult{}, err
	}
	res.Org = orgLabel(org, team)
	o.log.Info("github org provisioned", "by", c.Handle, "org", org, "team", team,
		"examined", res.Examined, "created", res.Created, "updated", res.Updated, "dry_run", dryRun)
	return res, nil
}

// ProvisionGitHubUsers does the same for an explicit list of logins, needing no
// token at all: github.com/<login>.keys is public. It is the path for admitting
// one person, and the one to reach for when the org's roster is not readable.
func (o *Ops) ProvisionGitHubUsers(ctx context.Context, c Caller, logins []string, dryRun bool) (ProvisionResult, error) {
	const op = "user.add"
	if err := o.operatorOnly(op, c, "only operators can provision accounts."); err != nil {
		return ProvisionResult{}, err
	}
	if len(logins) == 0 {
		return ProvisionResult{}, Invalid(op, "missing_login", "name at least one GitHub login")
	}
	if len(logins) > maxProvisionLogins {
		return ProvisionResult{}, Invalid(op, "too_many",
			"that is more logins than this command will provision at once (%d)", maxProvisionLogins)
	}
	res, err := o.provision(ctx, op, c, logins, dryRun)
	if err != nil {
		return ProvisionResult{}, err
	}
	o.log.Info("github users provisioned", "by", c.Handle,
		"examined", res.Examined, "created", res.Created, "updated", res.Updated, "dry_run", dryRun)
	return res, nil
}

// provision is the shared body: one login at a time, sequentially.
//
// Sequential on purpose. The work is a handful of requests per person against
// github.com and a write to a single-writer sqlite file, and a sync of a few
// hundred people is a thing an operator runs occasionally and watches finish.
// Concurrency here would buy seconds and cost the ability to read the output as
// a list of what happened, in order, to whom.
func (o *Ops) provision(ctx context.Context, op string, c Caller, logins []string, dryRun bool) (ProvisionResult, error) {
	res := ProvisionResult{DryRun: dryRun, Users: make([]ProvisionedUser, 0, len(logins))}
	for _, login := range logins {
		if err := ctx.Err(); err != nil {
			// The caller hung up or the budget ran out. Report what was done
			// rather than discarding it: the writes already happened.
			res.Users = append(res.Users, ProvisionedUser{
				Login: login, Outcome: OutcomeFailed, Note: "cancelled before this login was reached"})
			break
		}
		res.Examined++
		u := o.provisionOne(ctx, c, login, dryRun)
		switch u.Outcome {
		case OutcomeCreated:
			res.Created++
		case OutcomeKeysAdded:
			res.Updated++
		case OutcomeCurrent:
		default:
			res.Skipped++
		}
		res.Users = append(res.Users, u)
	}
	sort.Slice(res.Users, func(i, j int) bool { return res.Users[i].Login < res.Users[j].Login })
	return res, nil
}

// provisionOne admits a single GitHub login, reporting rather than returning an
// error: one bad login must not sink a whole sync.
func (o *Ops) provisionOne(ctx context.Context, c Caller, login string, dryRun bool) ProvisionedUser {
	out := ProvisionedUser{Login: login}
	handle := users.HandleForGitHubLogin(login)
	if handle == "" {
		out.Outcome = OutcomeSkipped
		out.Note = "no valid handle can be derived from that login (too long, or a reserved name)"
		return out
	}
	out.Handle = handle

	keys, err := o.github.Fetch(ctx, login)
	if err != nil {
		out.Outcome = OutcomeFailed
		out.Note = err.Error()
		return out
	}
	out.Keys = len(keys)
	if len(keys) == 0 {
		out.Outcome = OutcomeNoKeys
		out.Note = "github.com publishes no ssh key for this account"
		return out
	}

	existing, err := o.accounts.Get(handle)
	switch {
	case errors.Is(err, users.ErrNoSuchUser):
		if dryRun {
			out.Outcome = OutcomeCreated
			return out
		}
		out.Outcome, out.Note = o.createFromGitHub(ctx, c, handle, login, keys)
		return out
	case err != nil:
		out.Outcome = OutcomeFailed
		out.Note = err.Error()
		return out
	}

	// The handle is taken. It is only THIS person's if the account is already
	// linked to this GitHub login — otherwise it is a stranger who happens to
	// have picked the name, and adopting keys onto their account would be
	// handing it away.
	if !strings.EqualFold(existing.GitHubLogin, login) {
		out.Outcome = OutcomeSkipped
		out.Note = fmt.Sprintf("handle %q already belongs to another account", handle)
		return out
	}
	if dryRun {
		out.Outcome = OutcomeKeysAdded
		return out
	}
	added, note := o.adoptKeys(handle, login, keys)
	switch {
	case note != "":
		out.Outcome, out.Note = OutcomeFailed, note
	case added > 0:
		out.Outcome = OutcomeKeysAdded
		out.Note = fmt.Sprintf("%d new key(s)", added)
	default:
		out.Outcome = OutcomeCurrent
	}
	return out
}

// createFromGitHub registers the account on its first published key and adopts
// the rest, then records the GitHub link.
func (o *Ops) createFromGitHub(ctx context.Context, c Caller, handle, login string, keys []xssh.PublicKey) (outcome, note string) {
	// InvitedBy is the provisioning operator, NOT users.OperatorInviter. See
	// this file's header: that constant is what IsOperator() tests, and it
	// carries node administration and cross-tenant route visibility with it.
	err := o.accounts.Create(handle, keys[0], "github:"+login, "github-import", c.Handle)
	switch {
	case errors.Is(err, users.ErrKeyLinked):
		return OutcomeSkipped, "a key github.com lists for this login already belongs to another account"
	case errors.Is(err, users.ErrHandleTaken):
		return OutcomeSkipped, fmt.Sprintf("handle %q was claimed while this ran", handle)
	case err != nil:
		return OutcomeFailed, err.Error()
	}
	if added, n := o.adoptKeys(handle, login, keys[1:]); n != "" {
		o.log.Warn("adopting remaining github keys", "handle", handle, "login", login, "err", n)
	} else if added > 0 {
		note = fmt.Sprintf("%d keys", added+1)
	}
	o.linkProvisioned(ctx, handle, login)
	return OutcomeCreated, note
}

// adoptKeys adds every key not already on the account, returning how many were
// genuinely new.
//
// The account's current keys are read first because AddKey cannot answer the
// question on its own: it returns nil both for a key it inserted and for one
// the account already held (that idempotence is what makes re-running a sync
// safe), so counting its successes would report every re-sync as having added
// keys. The count is what tells an operator whether a re-run did anything, so
// it has to be the truth rather than the call count.
//
// A key another account claims is skipped rather than fatal — see AddKey's
// ErrKeyLinked — because one shared deploy key in a roster should not stop the
// other 199 people.
func (o *Ops) adoptKeys(handle, login string, keys []xssh.PublicKey) (added int, failure string) {
	existing, err := o.accounts.Keys(handle)
	if err != nil {
		return 0, err.Error()
	}
	have := make(map[string]bool, len(existing))
	for _, k := range existing {
		have[k.FP] = true
	}
	for _, k := range keys {
		if have[xssh.FingerprintSHA256(k)] {
			continue
		}
		switch err := o.accounts.AddKey(handle, k, "github:"+login, "github-import"); {
		case err == nil:
			added++
		case errors.Is(err, users.ErrKeyLinked):
		default:
			return added, err.Error()
		}
	}
	return added, ""
}

// linkProvisioned records the GitHub link on a freshly created account.
//
// The provenance is users.GitHubViaKeys, and that is a statement of fact rather
// than a convenience: the account's keys ARE the keys github.com publishes for
// the login, so "one of this account's registered keys is on
// github.com/<login>.keys" — the exact thing that provenance asserts — is true
// by construction. Which in turn means StrongGitHubLink holds, and the user can
// run `keys import-github` themselves later to pick up a newly added key
// without an operator.
//
// Best-effort, and last: an account that exists with the right keys is the
// thing that matters, and a github.com hiccup while fetching an account number
// must not undo it.
func (o *Ops) linkProvisioned(ctx context.Context, handle, login string) {
	var id int64
	if p, err := o.github.Profile(ctx, login); err == nil {
		id = p.ID
	} else {
		o.log.Warn("could not fetch github profile while provisioning", "login", login, "err", err)
	}
	if err := o.accounts.LinkGitHub(handle, login, users.GitHubViaKeys, id); err != nil {
		o.log.Warn("could not record github link while provisioning", "handle", handle, "login", login, "err", err)
	}
}

// ListAccounts returns every account on the host. Operator-only: the roster is
// the guest list, and an ordinary user has no business enumerating it.
func (o *Ops) ListAccounts(c Caller) ([]users.User, error) {
	const op = "user.ls"
	if err := o.operatorOnly(op, c, "only operators can list accounts."); err != nil {
		return nil, err
	}
	list, err := o.accounts.List()
	if err != nil {
		return nil, Fail(op, err)
	}
	return list, nil
}
