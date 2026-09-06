package ctlops

// The owner-scoped environment verbs.
//
// An environment is the name a person puts on a way of working: "the hivemind
// checkout, my GitHub token, NODE_ENV=test, egress limited to npm and github,
// booting from the disk we built last Tuesday." Every one of those pieces
// already existed and was already selected by a tag; what did not exist was a
// word for the whole, or anywhere to say what the word meant. So an environment
// OWNS EXACTLY ONE TAG AND ITS NAME IS THAT TAG. Nothing about the four
// existing joins changes — this package is a fifth reader and a composer, not a
// new indirection.
//
// That decision is what makes this file mostly bookkeeping rather than policy.
// `env create web --repo wandb/hivemind --secret GH_TOKEN` does not invent a
// relationship: it adds the tag `web` to an attachment and to a secret that
// were already tag-selected, and records that the word `web` is a thing
// somebody meant. Which is why ATTACHING IS ALWAYS A UNION. A secret can belong
// to three environments at once, and an environment that replaced a secret's
// tag set would silently remove it from the other two — the user asked to add a
// grouping, not to move a credential out of the places it was already reaching.
//
// The same asymmetry governs deletion, and it is the sharpest edge here:
// DeleteEnvironment removes the environment and the vars that only ever existed
// under its tag, and it removes NOTHING ELSE. A secret is a credential somebody
// pasted; a repo attachment is a checkout somebody may be working in; a
// rule-set is a policy that may govern other tags too. Deleting a grouping must
// never destroy any of them, so what `env rm` does instead is REPORT what still
// carries the tag, and let the transport print it.
//
// Best-effort composition, everywhere it is read. `env ls` gathers repos,
// secrets, rule-sets, vars and the bound snapshot from five stores, and a
// hiccup in any one of them degrades that field rather than turning the listing
// into an error — Ops.info's discipline, for the same reason: the composition
// is decoration on a row whose subject is the environment itself.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/envs"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/netrules"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/repos"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/secrets"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
)

// EnvVar is one plain (non-secret) environment variable of an environment.
//
// The value IS rendered, unlike a secret's, and that is the whole distinction
// between the two objects rather than an oversight: a var is a declaration that
// this string is configuration and not a credential. Anything that must not
// appear in `env show` is a secret.
type EnvVar struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// EnvironmentInfo is one environment as every transport renders it: the row,
// plus the composition gathered from the other four tag readers.
//
// SetupScript is deliberately NOT here. A setup script is a page of shell, and
// a listing that inlined it would be unreadable at three environments; what a
// caller needs in a list is whether there is one and roughly how big — hence
// the bool and the byte count, which are also exactly the two facts a "is this
// environment configured yet" line is built from. A verb that shows the script
// itself belongs beside the build that captures it (Phase B).
type EnvironmentInfo struct {
	Name        string `json:"name"` // IS the tag
	Description string `json:"description,omitempty"`
	State       string `json:"state"`
	SetupFrom   string `json:"setup_from,omitempty"`
	HasSetup    bool   `json:"has_setup"`
	SetupBytes  int    `json:"setup_bytes"`
	// ScriptDrift says whether this environment's script still agrees with the
	// .sparkbox/setup.sh in the repository it came from — one of
	// ScriptDriftMatch, ScriptDriftRepoAhead, ScriptDriftDiverged,
	// ScriptDriftRepoOnly, or "" for no answer (envdrift.go). The empty string
	// is common and means "not known", never "fine", so a surface renders it as
	// nothing rather than as reassurance.
	ScriptDrift string `json:"script_drift,omitempty"`
	// ScriptDriftRepo is the slug the comparison was made against, set whenever
	// ScriptDrift is.
	ScriptDriftRepo string `json:"script_drift_repo,omitempty"`
	BuildBox        string `json:"build_box,omitempty"`
	BuildError      string `json:"build_error,omitempty"`
	// BuildSession links to the HiveMind transcript of the agent that built
	// this environment. Empty for a script build, for a host with no
	// --hivemind-api, and while an agent build is still in flight — the row
	// learns it when the build reports, so a surface that wants to link a build
	// IN PROGRESS reads the builder sandbox's own live session instead.
	BuildSession        string                   `json:"build_session,omitempty"`
	BuildDenials        []envs.BuildDeniedDomain `json:"build_denials,omitempty"`
	BuildDenialOverflow uint64                   `json:"build_denial_overflow,omitempty"`
	BuiltAt             *time.Time               `json:"built_at,omitempty"`
	CreatedAt           time.Time                `json:"created_at"`
	// UpdatedAt is when this environment's composition was last changed. It is
	// on the public shape for a reader that has to choose BETWEEN environments
	// rather than display one — the launch door, which turns a repository
	// attached to several into the one sandbox its owner most recently worked
	// on (see EnvironmentsForTags).
	UpdatedAt time.Time `json:"updated_at"`

	// The composition. Every one of these is what carries this environment's
	// tag right now, read from the store that owns it — never a copy kept here,
	// because a copy is a second answer that goes stale the first time somebody
	// tags something by hand.
	Repos   []string `json:"repos"`   // slugs, never nil
	Secrets []string `json:"secrets"` // names only, never values, never nil
	Rules   []string `json:"rules"`   // rule-set names, never nil
	Vars    []EnvVar `json:"vars"`    // never nil
	// Snapshot is the base image bound to this tag, "" when the environment has
	// none and boots from the operator default.
	Snapshot string `json:"snapshot,omitempty"`
}

// EnvArgs is create-and-update in one verb, because the two are the same
// gesture: `env create web --repo x` typed a second time with `--secret Y` is a
// person adding to the thing they named, not re-declaring it. Nothing here
// removes an attachment — detaching is Phase A's deliberate omission, and a
// half-built `--unattach` that took a tag off a secret the user shares with
// another environment is exactly the mistake this file's union rule exists to
// prevent.
type EnvArgs struct {
	Name string
	// Description is a POINTER because "" is a real value and this struct is
	// create-and-update in one. `env set web --var LOG_LEVEL=debug` sends no
	// description and means "leave it alone"; sending the empty string means
	// "clear it". Before this was a pointer the two were the same request, so
	// any edit to anything else silently wiped the description — which is
	// exactly the field somebody wrote once and never types again.
	Description *string
	// Repos are attachments to give this environment's tag, with the per-repo
	// write/ref/path passthrough `repo add` already takes. Any Tags an entry
	// carries are honoured too and unioned with the environment's name, so a
	// caller that means "and also tag it ci" is not silently ignored.
	Repos []RepoArgs
	// Secrets and Rules name objects that must already exist. Naming one that
	// does not is a refusal rather than a create: an environment that quietly
	// invented an empty rule-set would be a subtractive policy nobody wrote.
	Secrets []string
	Rules   []string
	// OpenEgress opts OUT of the default egress rule-set a new environment
	// gets. It is a per-call gesture and is never stored: what it means is
	// "do not create one now", not "this environment is permanently open".
	OpenEgress bool
	// Adopt agrees to create an environment over a tag that is ALREADY carrying
	// somebody's repos, secrets, rule-sets, vars, base image or sandboxes. It
	// is a per-call gesture like OpenEgress and is never stored — what IS
	// stored is what was adopted, which is a different fact and is the store's
	// (see envs.Adopted).
	//
	// It only ever applies to a create. On an update the environment already
	// owns its tag, so there is nothing left to consent to and the flag is
	// silently irrelevant rather than an error — `env set web --adopt` typed out
	// of habit is not a mistake worth refusing.
	Adopt bool
	// Vars are set outright — a var IS keyed by (owner, tag, name), so there is
	// no other object to union with.
	Vars []EnvVar
}

// EnvDeleteResult is what `env rm` reports: the environment is gone, and these
// things are not.
//
// Repos, Secrets and Rules are the objects that still carry the tag after the
// deletion, and they are the point of the whole result. Removing a grouping
// must never destroy a user's credential or a checkout they may be working in,
// so this is the list a transport prints under "these still carry the tag `x`"
// — both to reassure the person that nothing was thrown away and to tell them
// where to look if what they actually wanted was to throw it away.
type EnvDeleteResult struct {
	Name    string   `json:"name"`
	Repos   []string `json:"repos"`   // never nil
	Secrets []string `json:"secrets"` // never nil
	Rules   []string `json:"rules"`   // never nil
	Unbound string   `json:"unbound,omitempty"`
	// RemovedRule is the auto-created egress rule-set this delete took back,
	// and it is the one thing here that WAS deleted. Empty for every rule-set
	// somebody wrote, which are reported in Rules like everything else.
	RemovedRule string   `json:"removed_rule,omitempty"`
	Resynced    []string `json:"resynced"` // sandboxes whose environment was re-pushed
	// KeptVars and KeptSnapshot are the vars and the base image this delete did
	// NOT take, because the environment adopted them rather than created them
	// (see envs.Adopted). They are reported for the same reason Repos and
	// Secrets are — so nobody has to guess whether their configuration survived
	// — and their emptiness is the normal case.
	KeptVars     []string `json:"kept_vars,omitempty"`
	KeptSnapshot string   `json:"kept_snapshot,omitempty"`
}

// resolveRunner is the VMM the tags require, and "" when they require nothing.
//
// It mirrors resolveTemplate's rule exactly, because it is the same shape of
// question asked of the same tag list: several tags agreeing is ordinary and
// fine — an owner may well want `web` and `ci` to be qemu environments — so
// what is refused is more than one DISTINCT runner, not more than one
// environment carrying one.
//
// Nothing is refused for a tag that names no environment, or an environment
// that requires nothing. Those are the overwhelming majority and they must
// stay on exactly the path they were on.
func (o *Ops) resolveRunner(op, owner string, tags []string) (vmm.Runner, error) {
	if o.envs == nil || len(tags) == 0 {
		return "", nil
	}
	list, err := o.envs.List(owner)
	if err != nil {
		return "", Fail(op, err)
	}
	var (
		want vmm.Runner
		from string
	)
	for _, e := range list {
		if e.Runner == "" || !slices.Contains(tags, e.Name) {
			continue
		}
		if want == "" {
			want, from = e.Runner, e.Name
			continue
		}
		if e.Runner != want {
			return "", &Error{
				Kind: KindConflict, Op: op, Code: "ambiguous_runner",
				Msg: fmt.Sprintf("environment %q requires %s and environment %q requires %s, so this sandbox has nowhere to run",
					from, want, e.Name, e.Runner),
				Hint:     "Use one of them, or change one with `ctl env set <name> --runner`.",
				Verbatim: true,
				Exit:     1,
				Status:   http.StatusConflict,
			}
		}
	}
	return want, nil
}

// runnerAgrees is resolveRunner's answer checked against a deployment that has
// exactly one machine, and so exactly one VMM.
//
// A fleet gets a sentence naming the machines it looked at; here there are none
// to name, and the honest sentence is about the host itself. An empty `have` is
// a host that was never told which VMM it runs, which is every mock and every
// test double — unknown is not a refusal.
func runnerAgrees(op string, want, have vmm.Runner) error {
	if want == "" || have == "" || want.SatisfiedBy(have) {
		return nil
	}
	return &Error{
		Kind: KindConflict, Op: op, Code: "no_node_runs_runner",
		Msg: fmt.Sprintf("this host runs %s and this sandbox's environment requires %s", have, want),
		Hint: "Change the environment's runner with `ctl env set <name> --runner`, " +
			"or run this host with --driver " + want.String() + ".",
		Details:  map[string]any{"runner": want.String()},
		Verbatim: true,
		Exit:     1,
		Status:   http.StatusConflict,
	}
}

// ---------------------------------------------------------------------------
// Read
// ---------------------------------------------------------------------------

// ListEnvironments returns the caller's environments with their composition.
func (o *Ops) ListEnvironments(c Caller) ([]EnvironmentInfo, error) {
	const op = "env.list"
	if o.envs == nil {
		return nil, Disabled(op, envDisabledSentence)
	}
	list, err := o.envs.List(c.Handle)
	if err != nil {
		return nil, Fail(op, err)
	}
	// One pass over each store for the whole listing rather than five queries
	// per environment: `env ls` on a person with a dozen environments would
	// otherwise be sixty round trips to answer a question every store can
	// answer once.
	comp := o.composition(c.Handle)
	out := make([]EnvironmentInfo, 0, len(list))
	for _, e := range list {
		out = append(out, comp.info(e))
	}
	// One bounded pass for the whole listing, on the same argument as the
	// composition above: a dozen environments must not be a dozen serial round
	// trips to github, and the listing renders whether or not the answers
	// arrive in time (envdrift.go).
	o.annotateScriptDrift(list, out)
	return out, nil
}

// GetEnvironment returns one environment, or the masked not-found.
func (o *Ops) GetEnvironment(c Caller, name string) (EnvironmentInfo, error) {
	const op = "env.get"
	if o.envs == nil {
		return EnvironmentInfo{}, Disabled(op, envDisabledSentence)
	}
	name, err := envName(op, name)
	if err != nil {
		return EnvironmentInfo{}, err
	}
	e, err := o.envs.Get(c.Handle, name)
	if err != nil {
		return EnvironmentInfo{}, envStoreError(op, name, err)
	}
	out := []EnvironmentInfo{o.composition(c.Handle).info(e)}
	o.annotateScriptDrift([]envs.Environment{e}, out)
	return out[0], nil
}

// EnvironmentsForTags returns the caller's environments whose name appears in
// tags, newest first — the answer to "of the tags on this thing, which are
// environments, and which did they last work on".
//
// It exists for a reader that has to CHOOSE between environments rather than
// display one. A repository attachment can carry several tags, and a sandbox
// has exactly one rootfs, so something must decide; the launch door is the
// caller today (internal/launch), and its rule is the most recently updated
// wins.
//
// No new store query. An owner has at most a few dozen environments and List is
// one indexed read, so the intersection happens here — the same bargain
// composition already makes, and a narrower one than a SQL IN would be, because
// the sort is this package's policy and not the store's.
//
// The composition IS filled in, even though the caller that motivated this
// method reads two fields of it. An EnvironmentInfo with empty Repos and
// Secrets is not a cheaper answer, it is a wrong one — the type means "an
// environment and everything carrying its tag" everywhere else it appears, and a
// second, hollow spelling of it would be found by the next caller rather than by
// a test. The cost is four indexed reads over small per-owner tables plus one
// vars lookup per MATCHED environment, against a launch click that already takes
// three repo queries and a sandbox listing.
//
// This is NOT the shape envs.EnvironmentsForSandbox was kept off the
// Environments interface for. That refusal is about resolving what a RUNNING
// GUEST sees, which is internal/metadata's authority; this is owner-and-tag
// scoped, decides nothing about any existing sandbox, and is a read of the same
// configuration `env ls` already returns in full.
func (o *Ops) EnvironmentsForTags(c Caller, tags []string) ([]EnvironmentInfo, error) {
	const op = "env.for-tags"
	if o.envs == nil {
		return nil, Disabled(op, envDisabledSentence)
	}
	if len(tags) == 0 {
		return nil, nil
	}
	list, err := o.envs.List(c.Handle)
	if err != nil {
		return nil, Fail(op, err)
	}
	comp := o.composition(c.Handle)
	out := make([]EnvironmentInfo, 0, len(tags))
	for _, e := range list {
		if slices.Contains(tags, e.Name) {
			out = append(out, comp.info(e))
		}
	}
	// Most recently updated first, name ascending to break a tie. The tiebreak
	// is not cosmetic: a caller picking the head of this slice must get the same
	// answer every time it asks, or a launch link would land in a different
	// sandbox on alternate clicks.
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].UpdatedAt.Equal(out[j].UpdatedAt) {
			return out[i].UpdatedAt.After(out[j].UpdatedAt)
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// ---------------------------------------------------------------------------
// Write
// ---------------------------------------------------------------------------

// PutEnvironment creates the environment or adds to one that exists, and
// re-pushes every sandbox the change reaches.
//
// The refusals all run BEFORE the first write, which for this verb means before
// envs.Put: an attachment naming a secret that does not exist, or a repo the
// caller's account may not attach, cannot possibly do what was asked, and it
// must not leave a half-composed environment row behind for the user to
// discover and wonder about. That is Create's ordering rule applied to a
// different set of stores.
func (o *Ops) PutEnvironment(ctx context.Context, c Caller, a EnvArgs) (EnvironmentInfo, error) {
	const op = "env.set"
	if o.envs == nil {
		return EnvironmentInfo{}, Disabled(op, envDisabledSentence)
	}
	name, err := envName(op, a.Name)
	if err != nil {
		return EnvironmentInfo{}, err
	}

	// ---- refusals, before anything is written ----

	if len(a.Repos) > 0 {
		if o.repos == nil {
			return EnvironmentInfo{}, Disabled(op, "repo attachments are not enabled on this host")
		}
		// Grammar before identity, in that order, because `repo add` refuses in
		// exactly that order (see AttachRepo) and two verbs answering the same
		// bad slug with two different errors is the kind of drift nobody finds
		// until a script depends on one of them.
		for _, r := range a.Repos {
			if err := checkRepoArgs(op, r); err != nil {
				return EnvironmentInfo{}, err
			}
		}
		// The same gate `repo add` applies, for the same reason: an attachment
		// decides which GitHub installation reads somebody's private source,
		// and a link this host did not prove directly must not reach it. An
		// environment is not a side door onto that verb.
		if _, err := o.attachIdentity(op, c); err != nil {
			return EnvironmentInfo{}, err
		}
	}
	// Both halves of the secret store are required: one to find out that the
	// name is real, the other to add the tag. A host with only one of them
	// cannot honour the request, and saying so is better than a nil panic.
	if len(a.Secrets) > 0 && (o.secretTags == nil || o.secrets == nil) {
		return EnvironmentInfo{}, Disabled(op, "secrets are not enabled on this host")
	}
	if len(a.Rules) > 0 && o.netrules == nil {
		return EnvironmentInfo{}, Disabled(op, "egress rule-sets are not enabled on this host")
	}
	if len(a.Vars) > 0 && o.envVars == nil {
		return EnvironmentInfo{}, Disabled(op, envVarsDisabledSentence)
	}
	// Resolve the named secrets and rule-sets to the rows they must already be,
	// so a typo is refused here rather than recorded as an environment
	// composing something that does not exist.
	wantSecrets, err := o.resolveEnvSecrets(op, c.Handle, a.Secrets)
	if err != nil {
		return EnvironmentInfo{}, err
	}
	wantRules, err := o.resolveEnvRules(op, c.Handle, a.Rules)
	if err != nil {
		return EnvironmentInfo{}, err
	}
	// The var grammar the store below would apply, applied HERE — by calling
	// the store's own validator, so the two cannot drift. A reserved name or a
	// '#' in a value is a request that cannot possibly succeed, and discovering
	// it from PutVar would mean reporting failure after envs.Put and
	// RetagSecret had already committed. There is no transaction spanning five
	// stores, so this ordering is the only thing that makes "it failed" mean
	// "nothing moved". The repo half of the same rule is above, next to the
	// gate it shares with `repo add`.
	for _, v := range a.Vars {
		if strings.TrimSpace(v.Name) == "" {
			return EnvironmentInfo{}, Invalid(op, "missing_var_name",
				"an environment variable needs a name, as NAME=value")
		}
		if err := secrets.ValidateVar(v.Name, v.Value); err != nil {
			return EnvironmentInfo{}, verbatim(Invalid(op, "bad_var", "%v", err))
		}
	}

	// Whether this call CREATES the environment, decided before anything is
	// written, because after `Put` there is no way to tell: the store's Put is
	// an upsert and `create`, `set` and `add` are one verb on one handler.
	//
	// Only the default egress rule-set reads it, and it is the difference
	// between a default and a change. Without it, somebody who deliberately
	// removed that rule-set to open their environment's egress would have it
	// silently put back by their next `env set web --var LOG_LEVEL=debug`.
	existing, getErr := o.envs.Get(c.Handle, name)
	creating := getErr != nil && errors.Is(getErr, envs.ErrNoSuchEnvironment)

	// THE ADOPTION GATE. A tag that is already carrying somebody's
	// configuration is not a free name, and an environment named after one
	// inherits the lot the moment the row exists — every composition read joins
	// on the name, so there is no "half created" state in which to notice.
	//
	// It runs with the other refusals, before the first write, and only on a
	// create: once the environment owns its tag there is nothing left to
	// consent to.
	//
	// The carriers named in THIS command are deliberately not subtracted.
	// `env create web --repo wandb/hivemind` where that repo is already tagged
	// `web` is the commonest adoption there is — somebody tagged things by hand
	// for months and now wants a word for the grouping — and it is exactly the
	// case that should be confirmed rather than waved through.
	var adopted *envs.Adopted
	if creating {
		carriers := o.carriersOfTag(c.Handle, name)
		if !carriers.empty() {
			if !a.Adopt {
				return EnvironmentInfo{}, envTagInUse(op, name, carriers)
			}
			// Written in the same INSERT as the row below, so `env rm` can
			// tell what this environment brought from what it found.
			adopted = carriers.adopted()
			o.log.Info("environment adopting an existing tag", "user", c.Handle, "env", name,
				"repos", len(carriers.Repos), "secrets", len(carriers.Secrets),
				"rules", len(carriers.Rules), "vars", len(carriers.Vars),
				"snapshot", carriers.Snapshot, "sandboxes", len(carriers.Sandboxes))
		}
	}

	// ---- writes ----

	// The store's Put sets the description outright, so an absent one has to be
	// filled in with what is already there rather than passed through as "".
	// existing is the zero Environment when creating, whose Description is ""
	// — which is the right answer for a create that named none.
	description := existing.Description
	if a.Description != nil {
		description = *a.Description
	}
	if _, err := o.envs.Put(c.Handle, name, description, adopted); err != nil {
		return EnvironmentInfo{}, envStoreError(op, name, err)
	}
	for _, r := range a.Repos {
		if err := o.attachRepoToEnv(op, c.Handle, name, r); err != nil {
			return EnvironmentInfo{}, err
		}
	}
	for _, s := range wantSecrets {
		if err := o.secretTags.RetagSecret(c.Handle, s.Name, union(s.Tags, name)); err != nil {
			return EnvironmentInfo{}, verbatim(Invalid(op, "bad_secret", "%v", err))
		}
	}
	for _, r := range wantRules {
		// The spec is carried straight back from the row that was just read:
		// this verb changes WHO a rule-set governs, never what it allows.
		if err := o.netrules.PutRule(c.Handle, r.Name, r.Spec, union(r.Tags, name)); err != nil {
			return EnvironmentInfo{}, verbatim(Invalid(op, "bad_rule", "%v", err))
		}
	}
	for _, v := range a.Vars {
		if err := o.envVars.PutVar(c.Handle, name, v.Name, v.Value); err != nil {
			return EnvironmentInfo{}, verbatim(Invalid(op, "bad_var", "%v", err))
		}
	}
	defaultedEgress := o.defaultEnvEgress(c.Handle, name, a, creating, len(wantRules) > 0)

	// Everything above changed what a sandbox carrying this tag should see, so
	// the running ones are re-pushed rather than left to discover it at their
	// next resume — which for a pinned box is never. Secrets and vars ride
	// ResyncEnv; checkouts need their own nudge, exactly as `repo add` does.
	affected := o.sandboxesWithTag(c.Handle, name)
	o.resyncBoxes(ctx, affected)
	if len(a.Repos) > 0 {
		o.syncReposFanout(ctx, affected)
	}
	o.log.Info("environment set", "user", c.Handle, "env", name,
		"repos", len(a.Repos), "secrets", len(wantSecrets), "rules", len(wantRules),
		"vars", len(a.Vars), "default_egress", defaultedEgress, "resynced", len(affected))

	e, err := o.envs.Get(c.Handle, name)
	if err != nil {
		return EnvironmentInfo{}, envStoreError(op, name, err)
	}
	return o.composition(c.Handle).info(e), nil
}

// DeleteEnvironment removes the environment and reports what survives it.
//
// IT DELETES NO SECRET, NO REPO ATTACHMENT AND NO RULE-SET. Those are objects
// with their own lifetimes that merely happen to carry this tag, usually
// alongside others: a token is a credential the user pasted, an attachment is a
// checkout somebody may be sitting in, a rule-set may govern tags this
// environment never knew about. Removing a grouping is not permission to
// destroy the things grouped, so what is returned instead is the list of what
// still carries the tag — which is both the reassurance that nothing was thrown
// away and the map for somebody who did want it thrown away.
//
// Two things DO go, because they cannot outlive the name: the vars, which are
// keyed by (owner, tag, name) and would otherwise be unreachable rows that
// reappear the day the name is reused, and the template binding, which would
// otherwise keep pointing a tag nobody can see at a base image.
func (o *Ops) DeleteEnvironment(ctx context.Context, c Caller, name string) (EnvDeleteResult, error) {
	const op = "env.rm"
	if o.envs == nil {
		return EnvDeleteResult{}, Disabled(op, envDisabledSentence)
	}
	name, err := envName(op, name)
	if err != nil {
		return EnvDeleteResult{}, err
	}
	// Existence is checked through the store's own owner-scoped Get, so another
	// owner's environment and one nobody has are the same masked answer. The row
	// is KEPT rather than discarded, because its adoption record is what decides
	// which of the two destructive steps below may run.
	row, err := o.envs.Get(c.Handle, name)
	if err != nil {
		return EnvDeleteResult{}, envStoreError(op, name, err)
	}
	adopted := envs.Adopted{}
	if row.Adopted != nil {
		adopted = *row.Adopted
	}
	// Read the composition and the fan-out BEFORE the delete, for
	// DeleteSecret's reason: afterwards nothing can say which boxes were
	// receiving this environment's vars, and they would linger in every running
	// guest until its next resume.
	comp := o.composition(c.Handle)
	affected := o.sandboxesWithTag(c.Handle, name)
	res := EnvDeleteResult{
		Name:    name,
		Repos:   comp.repos[name],
		Secrets: comp.secrets[name],
		Rules:   comp.rules[name],
	}

	if err := o.envs.Delete(c.Handle, name); err != nil {
		return EnvDeleteResult{}, envStoreError(op, name, err)
	}
	if o.envVars != nil {
		// Best effort AFTER the row is gone: the environment IS deleted, and
		// failing the command here would report a completed removal as a
		// refused one. The orphaned rows are owner-and-tag scoped and are
		// cleared again by the next delete of the same name.
		//
		// ADOPTED VARS ARE NOT DELETED. The blanket DeleteVarsForTag is right
		// for an environment that brought its own — those rows are keyed by
		// (owner, tag, name) and would otherwise be unreachable — and wrong for
		// one that found them already sitting on somebody's tag, which is
		// configuration this verb was never given permission to destroy. So the
		// vars are read back and removed one at a time, skipping the inherited
		// names.
		res.KeptVars = o.deleteOwnVars(c.Handle, name, adopted.Vars)
	}
	// The one deletion this verb makes, and it is a deletion of something this
	// package created rather than something a person did. It runs before the
	// unbind for no reason except that both are best-effort cleanups and this
	// one reads on the same list the note above is about.
	if removed := o.reclaimDefaultEgress(c.Handle, name, affected); removed != "" {
		res.RemovedRule = removed
		res.Rules = slices.DeleteFunc(res.Rules, func(r string) bool { return r == removed })
	}
	// The binding goes for the same reason the vars do — a tag nobody can see
	// must not keep pointing at a base image — and it stays for the same reason
	// too. An adopted binding was somebody's `snapshot bind` long before this
	// environment existed, and un-pointing it would change what their EXISTING
	// sandboxes boot from, which is a far larger action than deleting a name.
	// The comparison is against the CURRENT binding, not merely "was one
	// adopted": a successful `env build` re-points the tag at a disk this
	// environment captured, and that snapshot is its own however the tag
	// started out. Only a binding that is still the one found on the day is
	// kept.
	switch {
	case o.templateTags == nil || comp.snapshot[name] == "":
	case adopted.Snapshot != "" && adopted.Snapshot == comp.snapshot[name]:
		res.KeptSnapshot = comp.snapshot[name]
	default:
		if b, err := o.templateTags.Unbind(c.Handle, name); err == nil {
			res.Unbound = b.Snapshot
		} else {
			o.log.Warn("could not unbind an environment's template", "user", c.Handle, "env", name, "err", err)
		}
	}
	o.resyncBoxes(ctx, affected)
	res.Resynced = affected
	nonNil(&res.Repos, &res.Secrets, &res.Rules, &res.Resynced)
	o.log.Info("environment removed", "user", c.Handle, "env", name,
		"still_tagged_repos", len(res.Repos), "still_tagged_secrets", len(res.Secrets),
		"still_tagged_rules", len(res.Rules), "removed_rule", res.RemovedRule,
		"kept_vars", len(res.KeptVars), "kept_snapshot", res.KeptSnapshot,
		"resynced", len(affected))
	return res, nil
}

// SetEnvVar sets one plain variable on an environment and re-pushes the
// sandboxes that were receiving it.
//
// The re-push is what makes this feel like setting a variable rather than
// filing a request, and it is the same rule PutSecret follows for the same
// reason: a var saved while a box is running does not otherwise reach that box
// until its next resume.
func (o *Ops) SetEnvVar(ctx context.Context, c Caller, env, name, value string) error {
	const op = "env.var.set"
	envName, err := o.envForVar(op, c, env)
	if err != nil {
		return err
	}
	if strings.TrimSpace(name) == "" {
		return Invalid(op, "missing_var_name", "an environment variable needs a name, as NAME=value")
	}
	// The store's sentences about the env-name grammar, the reserved loader
	// names and the characters /etc/environment cannot carry are already
	// user-facing and more specific than anything this layer could say, so they
	// pass through verbatim.
	if err := o.envVars.PutVar(c.Handle, envName, name, value); err != nil {
		return verbatim(Invalid(op, "bad_var", "%v", err))
	}
	affected := o.sandboxesWithTag(c.Handle, envName)
	o.resyncBoxes(ctx, affected)
	// The VALUE is deliberately not in the audit line. A var is declared
	// non-sensitive, but a log is a different audience from a terminal, and the
	// name is what an operator reading this needs.
	o.log.Info("env var set", "user", c.Handle, "env", envName, "var", name, "resynced", len(affected))
	return nil
}

// UnsetEnvVar removes one plain variable and re-pushes the sandboxes that were
// receiving it. The fan-out is computed before the delete for the reason
// DeleteSecret's is: afterwards the row is gone.
func (o *Ops) UnsetEnvVar(ctx context.Context, c Caller, env, name string) error {
	const op = "env.var.unset"
	envName, err := o.envForVar(op, c, env)
	if err != nil {
		return err
	}
	if strings.TrimSpace(name) == "" {
		return Invalid(op, "missing_var_name", "which variable? give its name")
	}
	affected := o.sandboxesWithTag(c.Handle, envName)
	if err := o.envVars.DeleteVar(c.Handle, envName, name); err != nil {
		if errors.Is(err, secrets.ErrNoSuchVar) {
			return Invalid(op, "no_such_var", "%s has no variable named %s", envName, name)
		}
		return verbatim(Invalid(op, "bad_var", "%v", err))
	}
	o.resyncBoxes(ctx, affected)
	o.log.Info("env var unset", "user", c.Handle, "env", envName, "var", name, "resynced", len(affected))
	return nil
}

// envForVar is the gate both var verbs share: the stores exist, the name parses,
// and the environment is one the caller actually has.
//
// Requiring the environment to exist is what keeps a var from being a fifth
// kind of loose tag row. Vars are keyed by tag, so nothing in the store would
// stop `env var set typo FOO=1` from writing a row that reaches whatever
// sandbox happens to carry `typo` — and nobody would ever see it again.
func (o *Ops) envForVar(op string, c Caller, env string) (string, error) {
	if o.envs == nil {
		return "", Disabled(op, envDisabledSentence)
	}
	if o.envVars == nil {
		return "", Disabled(op, envVarsDisabledSentence)
	}
	name, err := envName(op, env)
	if err != nil {
		return "", err
	}
	if _, err := o.envs.Get(c.Handle, name); err != nil {
		return "", envStoreError(op, name, err)
	}
	return name, nil
}

// ---------------------------------------------------------------------------
// The setup script
// ---------------------------------------------------------------------------

// MaxSetupScript caps what any transport may store as an environment's setup
// script. A setup script is a page of shell — `apt-get install`, `uv sync`, the
// three lines that make a checkout buildable — and 64 KiB is far more of that
// than anybody writes by hand. The cap is here rather than in a transport so
// the ssh channel and a future HTTP route cannot disagree about it, in the same
// way MaxTagsPerSandbox is one number both doors read.
const MaxSetupScript = 64 << 10

// EnvScript returns the environment's stored setup script and where it came
// from ("repo", "agent", "manual", or "" when nothing ever looked).
//
// It is a verb of its own rather than a field on EnvironmentInfo for the reason
// given there: a listing that inlined a page of shell per row would be
// unreadable, and a caller that wants the text wants only the text.
func (o *Ops) EnvScript(c Caller, name string) (script, from string, err error) {
	const op = "env.script"
	if o.envs == nil {
		return "", "", Disabled(op, envDisabledSentence)
	}
	name, err = envName(op, name)
	if err != nil {
		return "", "", err
	}
	e, err := o.envs.Get(c.Handle, name)
	if err != nil {
		return "", "", envStoreError(op, name, err)
	}
	return e.SetupScript, e.SetupFrom, nil
}

// SetEnvScript records a setup script and where it came from.
//
// Nothing is re-pushed and no sandbox is touched: a setup script is read by a
// BUILD, not by a running guest, so writing one changes what the next build
// does and nothing about the boxes carrying the tag today. That is the whole
// difference between this verb and SetEnvVar next door, and it is why this one
// takes no context — there is no fan-out to fund.
func (o *Ops) SetEnvScript(c Caller, name, script, from string) error {
	const op = "env.script.set"
	if o.envs == nil {
		return Disabled(op, envDisabledSentence)
	}
	name, err := envName(op, name)
	if err != nil {
		return err
	}
	if len(script) > MaxSetupScript {
		return Invalid(op, "script_too_long",
			"that setup script is %d bytes; the limit is %d", len(script), MaxSetupScript)
	}
	// The store owns the "where did this come from" grammar and refuses an
	// origin it does not know, which is what keeps a future fourth source from
	// being invented at a transport.
	if err := o.envs.SetScript(c.Handle, name, script, from); err != nil {
		return envStoreError(op, name, err)
	}
	o.log.Info("environment setup script set", "user", c.Handle, "env", name,
		"from", from, "bytes", len(script))
	o.forgetDrift(c.Handle, name)
	return nil
}

// AdoptRepoScript takes the .sparkbox/setup.sh out of an attached repository
// and makes it this environment's script, seed and all.
//
// It is the answer to the one verdict nothing else resolves. When an
// environment's script has DIVERGED — changed by a repair pass, or by somebody
// — no build will overwrite it, and that is the right default: the row is
// sometimes the only copy of a fix that was never committed. But it leaves a
// person holding two scripts and no way to say "the repository's is the one I
// want" short of copying the file through their own terminal.
//
// Explicit for exactly that reason. Everything automatic in this feature is
// gated on the row being an untouched copy of what a repository gave it; this
// is the only path that overwrites work, and a person has to ask for it by
// name.
func (o *Ops) AdoptRepoScript(ctx context.Context, c Caller, name string) (EnvironmentInfo, error) {
	const op = "env.script.from_repo"
	if o.envs == nil {
		return EnvironmentInfo{}, Disabled(op, envDisabledSentence)
	}
	name, err := envName(op, name)
	if err != nil {
		return EnvironmentInfo{}, err
	}
	// Owner-scoped first, so another owner's environment and one nobody has are
	// the same masked answer.
	if _, err := o.envs.Get(c.Handle, name); err != nil {
		return EnvironmentInfo{}, envStoreError(op, name, err)
	}
	if !o.canReadRepoScripts() {
		return EnvironmentInfo{}, Disabled(op,
			"this host cannot read files out of repositories, so there is nothing to take a setup script from.")
	}
	found, err := o.readRepoSetupScript(ctx, op, c.Handle, name)
	if err != nil {
		// github_unreachable, already phrased. Never mistaken for "no script".
		return EnvironmentInfo{}, err
	}
	if found.Script == "" {
		return EnvironmentInfo{}, &Error{
			Kind: KindConflict, Op: op, Code: "no_repo_script",
			Msg: "no repository attached to " + name + " has a " + SetupScriptPath + " to take.",
			Hint: "Attach one with `env set " + name + " --repo <owner>/<name>`, or write " +
				SetupScriptPath + " in a repository that already carries this tag.",
			Details:  map[string]any{"environment": name, "path": SetupScriptPath},
			Verbatim: true,
		}
	}
	if err := o.envs.SetSeededScript(c.Handle, name, found.Script); err != nil {
		return EnvironmentInfo{}, envStoreError(op, name, err)
	}
	o.log.Info("environment setup script taken from its repository on request",
		"user", c.Handle, "env", name, "slug", found.Slug, "bytes", len(found.Script))
	o.forgetDrift(c.Handle, name)
	return o.GetEnvironment(c, name)
}

// ---------------------------------------------------------------------------
// create --env
// ---------------------------------------------------------------------------

// resolveEnvTag turns `--env web` into the tag a create should carry, refusing
// anything that cannot boot the environment the caller named.
//
// It runs before the first write, beside nameIsFree and placeable, on Create's
// own rule: a create that cannot possibly succeed must not leave tag rows
// behind for a sandbox that never exists.
//
// Every not-ready state gets its own sentence, because they are four different
// situations and only one of them is a mistake. Somebody whose build is running
// needs to be told to wait; somebody whose environment has never been built
// needs to be told to build it; somebody whose build failed needs the reason
// they have already forgotten. "not found" for all three would send each of
// them looking for a typo they did not make.
func (o *Ops) resolveEnvTag(op, owner, want string) (string, error) {
	if o.envs == nil {
		return "", Disabled(op, envDisabledSentence)
	}
	name, err := envName(op, want)
	if err != nil {
		return "", err
	}
	e, err := o.envs.Get(owner, name)
	if err != nil {
		return "", envStoreError(op, name, err)
	}
	switch e.State {
	case envs.StateReady:
		return name, nil
	case envs.StateBuilding:
		return "", &Error{
			Kind: KindConflict, Op: op, Code: "env_building",
			Msg: "environment " + name + " is still building" + buildBoxPhrase(e) +
				". Wait for it to finish, or create the sandbox with --tag " + name +
				" to get its secrets and checkouts on the default image.",
			Details:  map[string]any{"environment": name, "state": string(e.State)},
			Verbatim: true,
		}
	case envs.StateFailed:
		// A FAILED BUILD AND A FAILED REBUILD ARE NOT THE SAME STATE, and until
		// `env rebuild` existed only the first could happen, which is why one
		// sentence used to cover both.
		//
		// The binding is written at the very END of a successful capture, so a
		// rebuild that fails anywhere leaves template_tags still pointing at the
		// previous good snapshot. The environment therefore still HAS an image
		// and still boots from it — while the row says `failed`. Telling that
		// person "has no base image" is false, and sending them to `--tag <name>
		// to use the default image` is false twice over: the tag resolves the
		// bound snapshot, so it would not give them the default image either.
		//
		// BuiltAt is the discriminator: it is stamped only on the transition
		// into `ready`, so a non-zero one means a capture succeeded once and
		// nothing since has unbound it.
		if e.BuiltAt != nil {
			return "", &Error{
				Kind: KindConflict, Op: op, Code: "env_build_failed",
				Msg: "environment " + name + "'s last build failed, so it is still on the image it built on " +
					e.BuiltAt.UTC().Format("2006-01-02") + "." + buildErrSentence(e),
				Hint: "Boot that image anyway with --tag " + name + ", or try again with `env rebuild " +
					name + "`.",
				Details: map[string]any{
					"environment": name, "state": string(e.State), "built_at": e.BuiltAt.UTC(),
				},
				Verbatim: true,
			}
		}
		return "", &Error{
			Kind: KindConflict, Op: op, Code: "env_build_failed",
			Msg: "environment " + name + " has no base image: its last build failed." +
				buildErrSentence(e),
			Hint:     "Build it again, or create the sandbox with --tag " + name + " to use the default image.",
			Details:  map[string]any{"environment": name, "state": string(e.State)},
			Verbatim: true,
		}
	default:
		return "", &Error{
			Kind: KindConflict, Op: op, Code: "env_not_built",
			Msg: "environment " + name + " has never been built, so there is no image to boot it from.",
			Hint: "Build it first, or create the sandbox with --tag " + name +
				" to get its secrets and checkouts on the default image.",
			Details:  map[string]any{"environment": name, "state": string(e.State)},
			Verbatim: true,
		}
	}
}

func buildBoxPhrase(e envs.Environment) string {
	if e.BuildBox == "" {
		return ""
	}
	return " (in " + e.BuildBox + ")"
}

// buildErrSentence puts a recorded build error in a sentence of its OWN, after
// the one that says what to do about it.
//
// It used to be spliced into the middle, and a real one read: "environment web's
// last build failed: the agent run exited 3: sparkbox: this box is configured,
// but .sparkbox/setup.sh does not run in it…, so it is still on the image it
// built on 2026-09-02." The two facts the reader has to have — there IS still an
// image, and `env rebuild` retries — arrive after a guest log line that can run
// to the width of the terminal, and the sentence has two colons before its verb.
// The clause somebody acts on now comes first and the evidence follows it.
func buildErrSentence(e envs.Environment) string {
	if e.BuildError == "" {
		return ""
	}
	// The recorded error is a summarised guest log line, so it ends however the
	// guest's last line ended; this owns the punctuation rather than doubling it.
	return " The build said: " + strings.TrimRight(e.BuildError, " \t.") + "."
}

// ---------------------------------------------------------------------------
// Composition
// ---------------------------------------------------------------------------

// envComposition is the four tag readers plus the vars, gathered once and
// indexed by tag. Every map is keyed by tag name, which for these purposes IS
// an environment name — a tag with no environment simply has no entry that
// anything reads.
type envComposition struct {
	repos    map[string][]string
	secrets  map[string][]string
	rules    map[string][]string
	snapshot map[string]string
	vars     func(tag string) []EnvVar
}

// composition reads every store an environment composes from, ONCE, for one
// owner.
//
// Each read is best effort. A store that stumbles degrades one field of the
// answer; it does not turn `env ls` into an error, because the composition is
// decoration on a row whose subject is the environment itself. That is the
// discipline Ops.info already applies to a sandbox's tags, and the reason it
// matters more here is that there are five stores to stumble.
func (o *Ops) composition(owner string) envComposition {
	comp := envComposition{
		repos:    map[string][]string{},
		secrets:  map[string][]string{},
		rules:    map[string][]string{},
		snapshot: map[string]string{},
	}
	if o.repos != nil {
		if list, err := o.repos.ListRepos(owner); err == nil {
			for _, r := range list {
				for _, t := range r.Tags {
					comp.repos[t] = append(comp.repos[t], r.Slug)
				}
			}
		} else {
			o.log.Warn("could not read repo attachments for an environment listing", "user", owner, "err", err)
		}
	}
	if o.secrets != nil {
		if list, err := o.secrets.ListSecrets(owner); err == nil {
			for _, s := range list {
				for _, t := range s.Tags {
					comp.secrets[t] = append(comp.secrets[t], s.Name)
				}
			}
		} else {
			o.log.Warn("could not read secrets for an environment listing", "user", owner, "err", err)
		}
	}
	if o.netrules != nil {
		if list, err := o.netrules.ListRules(owner); err == nil {
			for _, r := range list {
				for _, t := range r.Tags {
					comp.rules[t] = append(comp.rules[t], r.Name)
				}
			}
		} else {
			o.log.Warn("could not read egress rule-sets for an environment listing", "user", owner, "err", err)
		}
	}
	if o.templateTags != nil {
		if list, err := o.templateTags.BindingsForOwner(owner); err == nil {
			for _, b := range list {
				comp.snapshot[b.Tag] = b.Snapshot
			}
		} else {
			o.log.Warn("could not read template bindings for an environment listing", "user", owner, "err", err)
		}
	}
	for _, m := range []map[string][]string{comp.repos, comp.secrets, comp.rules} {
		for k := range m {
			sort.Strings(m[k])
		}
	}
	// Vars are the one field read per environment rather than in one pass:
	// EnvVars deliberately has no list-them-all method (see the interface), and
	// an environment listing is a handful of rows, not a fan-out.
	comp.vars = func(tag string) []EnvVar {
		if o.envVars == nil {
			return nil
		}
		list, err := o.envVars.VarsForTag(owner, tag)
		if err != nil {
			o.log.Warn("could not read an environment's vars", "user", owner, "env", tag, "err", err)
			return nil
		}
		out := make([]EnvVar, 0, len(list))
		for _, v := range list {
			out = append(out, EnvVar{Name: v.Name, Value: v.Value})
		}
		return out
	}
	return comp
}

// info projects a stored environment onto the public shape, filling in the
// composition. The setup script is reported as a presence and a size and is
// never carried out of this package by a listing — see EnvironmentInfo.
func (c envComposition) info(e envs.Environment) EnvironmentInfo {
	ei := EnvironmentInfo{
		Name:                e.Name,
		Description:         e.Description,
		State:               string(e.State),
		SetupFrom:           e.SetupFrom,
		HasSetup:            e.SetupScript != "",
		SetupBytes:          len(e.SetupScript),
		BuildBox:            e.BuildBox,
		BuildError:          e.BuildError,
		BuildSession:        e.BuildSession,
		BuildDenials:        append([]envs.BuildDeniedDomain(nil), e.BuildDenials...),
		BuildDenialOverflow: e.BuildDenialOverflow,
		BuiltAt:             e.BuiltAt,
		CreatedAt:           e.CreatedAt,
		UpdatedAt:           e.UpdatedAt,
		Repos:               c.repos[e.Name],
		Secrets:             c.secrets[e.Name],
		Rules:               c.rules[e.Name],
		Snapshot:            c.snapshot[e.Name],
	}
	if c.vars != nil {
		ei.Vars = c.vars(e.Name)
	}
	nonNil(&ei.Repos, &ei.Secrets, &ei.Rules)
	if ei.Vars == nil {
		ei.Vars = []EnvVar{}
	}
	return ei
}

// ---------------------------------------------------------------------------
// Adoption
// ---------------------------------------------------------------------------

// tagCarriers is everything already carrying a tag that no environment has
// claimed yet — the state `env create <that tag>` would silently inherit.
//
// It exists because the tag namespace is OLDER than environments and shared
// with four other stores. `ctl create scratch --tag web`, `repo add --tag web`
// and `net --tag web` all write rows with no environment anywhere, so a name
// that means nothing to this package can mean a great deal to somebody's
// sandboxes. An environment's name IS its tag, so naming one after such a tag
// is not a fresh start: it is an adoption, and it happens the instant the row
// is inserted, because every composition read joins on the name.
type tagCarriers struct {
	Repos     []string
	Secrets   []string
	Rules     []string
	Vars      []string
	Snapshot  string
	Sandboxes []string
}

// empty reports whether the tag is genuinely unused, which is the ordinary case
// and the one that needs no consent.
func (t tagCarriers) empty() bool {
	return len(t.Repos) == 0 && len(t.Secrets) == 0 && len(t.Rules) == 0 &&
		len(t.Vars) == 0 && t.Snapshot == "" && len(t.Sandboxes) == 0
}

// adopted narrows the carriers down to the two that `env rm` would DESTROY, and
// is what gets written alongside the row.
//
// Repos, secrets, rule-sets and sandboxes are deliberately not recorded: delete
// never touches them, it reports them, so nothing later needs to know whether
// they arrived before or after. Recording them anyway would be a second copy of
// a list the other stores already answer, kept at one instant and wrong from the
// next write onwards.
func (t tagCarriers) adopted() *envs.Adopted {
	a := envs.Adopted{Vars: slices.Clone(t.Vars), Snapshot: t.Snapshot}
	if a.Empty() {
		return nil
	}
	return &a
}

// deleteOwnVars removes every var on the tag EXCEPT the adopted ones, and
// returns the names it left standing.
//
// One delete per var where DeleteVarsForTag is a single statement, which is the
// price of the distinction and a cheap one: an environment holds a handful of
// variables, and the alternative — deleting the lot and writing the adopted ones
// back — has a window in which somebody's configuration exists nowhere.
//
// Every failure is logged and swallowed, matching the blanket delete it
// replaces: the environment row is already gone by the time this runs, and a
// var that outlives it is a stray row, not a broken command.
func (o *Ops) deleteOwnVars(owner, tag string, keep []string) []string {
	list, err := o.envVars.VarsForTag(owner, tag)
	if err != nil {
		o.log.Warn("could not read an environment's vars to delete them",
			"user", owner, "env", tag, "err", err)
		return nil
	}
	var kept []string
	for _, v := range list {
		if slices.Contains(keep, v.Name) {
			kept = append(kept, v.Name)
			continue
		}
		if err := o.envVars.DeleteVar(owner, tag, v.Name); err != nil {
			o.log.Warn("could not delete an environment's var",
				"user", owner, "env", tag, "var", v.Name, "err", err)
		}
	}
	sort.Strings(kept)
	return kept
}

// carriersOfTag reads what the tag holds right now. It is taken only on the
// create path, where it decides a refusal, so the cost is one composition read
// on the rarest of this verb's paths — `env set` typed against an environment
// that already exists reads none of it.
func (o *Ops) carriersOfTag(owner, tag string) tagCarriers {
	comp := o.composition(owner)
	t := tagCarriers{
		Repos:     comp.repos[tag],
		Secrets:   comp.secrets[tag],
		Rules:     comp.rules[tag],
		Snapshot:  comp.snapshot[tag],
		Sandboxes: o.sandboxesWithTag(owner, tag),
	}
	if comp.vars != nil {
		for _, v := range comp.vars(tag) {
			t.Vars = append(t.Vars, v.Name)
		}
	}
	sort.Strings(t.Vars)
	return t
}

// envTagInUse is the refusal for `env create` over a tag somebody is already
// using.
//
// It is a conflict rather than an invalid: the name is spelled perfectly well
// and the request is one the caller may genuinely want, so the answer is "say
// you meant it", not "say it differently". Everything the tag carries goes in
// Details, because the console and the REST API render their own sentence and
// the SSH channel prints this one — and the list is the whole content of the
// warning.
func envTagInUse(op, name string, t tagCarriers) *Error {
	var parts []string
	for _, s := range []struct {
		n    int
		one  string
		many string
	}{
		{len(t.Repos), "repository", "repositories"},
		{len(t.Secrets), "secret", "secrets"},
		{len(t.Rules), "egress rule-set", "egress rule-sets"},
		{len(t.Vars), "variable", "variables"},
		{len(t.Sandboxes), "sandbox", "sandboxes"},
	} {
		if s.n == 0 {
			continue
		}
		parts = append(parts, fmt.Sprintf("%d %s", s.n, plural(s.n, s.one, s.many)))
	}
	if t.Snapshot != "" {
		parts = append(parts, "a base image ("+t.Snapshot+")")
	}
	// The consequence is stated in general rather than itemised, because the
	// itemised version has to be true of whichever carriers happen to exist —
	// and a sentence about "those repositories" on a tag that has none reads
	// like a bug in the message, which is the last thing a warning can afford.
	msg := fmt.Sprintf("the tag %q is already carrying %s. An environment's name IS its tag, so "+
		"creating %s adopts all of it: everything on that tag becomes part of the environment, and "+
		"every sandbox carrying it becomes one of its machines.", name, joinAnd(parts), name)
	// Said out loud because it is the one consequence that is not visible in the
	// list, and it is a policy difference rather than a display one.
	if len(t.Sandboxes) > 0 {
		msg += " Because sandboxes already carry it, the environment is also created with " +
			"unrestricted egress rather than the default allowlist, so as not to cut them off."
	}
	return &Error{
		Kind: KindConflict, Op: op, Code: "env_tag_in_use",
		Msg: msg,
		Hint: "Re-run with --adopt to take it on as it stands, or choose a name nothing carries yet " +
			"and attach what you want with --repo, --secret and --var.",
		Details: map[string]any{
			"environment": name,
			"repos":       t.Repos, "secrets": t.Secrets, "rules": t.Rules,
			"vars": t.Vars, "snapshot": t.Snapshot, "sandboxes": t.Sandboxes,
		},
		Verbatim: true,
	}
}

// plural picks the noun form. Written out rather than reached for from a
// dependency because this package renders exactly one sentence that needs it.
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// joinAnd renders a list the way a person would say it: "a, b and c".
func joinAnd(parts []string) string {
	switch len(parts) {
	case 0:
		return "nothing"
	case 1:
		return parts[0]
	default:
		return strings.Join(parts[:len(parts)-1], ", ") + " and " + parts[len(parts)-1]
	}
}

// ---------------------------------------------------------------------------
// Attachment helpers
// ---------------------------------------------------------------------------

// checkRepoArgs is every refusal a repo attachment can decide locally, with no
// store read and no network. It is separate from attachRepoToEnv so the caller
// can run it over the whole list before writing anything.
func checkRepoArgs(op string, a RepoArgs) error {
	slug := strings.TrimSpace(a.Slug)
	if slug == "" {
		return Invalid(op, "missing_slug", "a repository is required, as <owner>/<name>")
	}
	if !repos.ValidSlug(slug) {
		return Invalid(op, "bad_slug",
			"%q is not an owner/name repository — write it the way github.com does, e.g. wandb/hivemind", a.Slug)
	}
	if _, err := NormalizeTags(a.Tags); err != nil {
		return Invalid(op, "bad_tag", "%v", err)
	}
	return nil
}

// attachRepoToEnv adds this environment's tag to a repo attachment, creating
// the attachment if the caller named one that does not exist yet.
//
// The tag set is a UNION of what the row already carries, the tags the caller
// named, and the environment. Replacing it would silently detach the repository
// from every other environment and hand-applied tag it was reaching, which is
// the opposite of what "attach this to web" asks for.
//
// Unlike AttachRepo this does NOT check the App can reach the repository. The
// check is a network round trip per repository and its answer is already
// reported by `repo check` and by `repo add`; making `env create` wait on
// github.com once per attachment would turn a local composition into a command
// that fails when github.com is slow. See the note in the report.
func (o *Ops) attachRepoToEnv(op, owner, env string, a RepoArgs) error {
	// Re-checked rather than assumed: PutEnvironment runs this same function
	// over every attachment before its first write, and this call is what keeps
	// that a belt-and-braces pass instead of the only one, should this ever
	// grow a second caller.
	if err := checkRepoArgs(op, a); err != nil {
		return err
	}
	slug := strings.TrimSpace(a.Slug)
	extra, _ := NormalizeTags(a.Tags)
	r := repos.Repo{Host: a.Host, Slug: slug, Ref: a.Ref, Path: a.Path, Access: repos.AccessRead}
	if a.Write {
		r.Access = repos.AccessWrite
	}
	want := union(extra, env)
	// The existing row's tags come first so an attachment shared with another
	// environment keeps reaching it.
	if list, err := o.repos.ListRepos(owner); err == nil {
		if cur, ok := findRepo(list, slug); ok {
			want = union(append(slices.Clone(cur.Tags), want...))
			// A field the caller did not restate keeps the value the row
			// already had. `env create web --repo wandb/hivemind` must not
			// silently reset a ref or a path somebody set with `repo add`.
			if r.Ref == "" {
				r.Ref = cur.Ref
			}
			if r.Path == "" {
				r.Path = cur.Path
			}
			if r.Host == "" {
				r.Host = cur.Host
			}
			if !a.Write {
				r.Access = cur.Access
			}
		}
	} else {
		o.log.Warn("could not read existing attachments while composing an environment",
			"user", owner, "env", env, "err", err)
	}
	if err := o.repos.PutRepo(owner, r, want); err != nil {
		return verbatim(Invalid(op, "bad_repo", "%v", err))
	}
	return nil
}

// resolveEnvSecrets maps the names a caller asked to tag onto the rows they
// must already be. A name nobody holds and a name somebody ELSE holds are the
// same refusal, from the same line, because ListSecrets is owner-scoped.
func (o *Ops) resolveEnvSecrets(op, owner string, names []string) ([]secrets.SecretMeta, error) {
	if len(names) == 0 {
		return nil, nil
	}
	list, err := o.secrets.ListSecrets(owner)
	if err != nil {
		return nil, Fail(op, err)
	}
	out := make([]secrets.SecretMeta, 0, len(names))
	for _, want := range names {
		want = strings.TrimSpace(want)
		i := slices.IndexFunc(list, func(m secrets.SecretMeta) bool { return m.Name == want })
		if i < 0 {
			return nil, Invalid(op, "no_such_secret",
				"no secret named %s — save it first with `secret set %s`", want, want)
		}
		out = append(out, list[i])
	}
	return out, nil
}

// defaultEnvEgress gives a brand-new environment a governed-by-default egress
// posture, and it is the one place this package creates a rule-set nobody
// typed. It reports whether it made one.
//
// THE PROBLEM IT SOLVES. sluice runs `--enforce --open-untagged`, under which a
// sandbox ABSENT from the policy snapshot is unfiltered. A sandbox is present
// only if one of its tags carries a rule-set — so "the builder is tagged, so it
// is governed" is FALSE, and an environment nobody wrote egress rules for got
// UNRESTRICTED egress. That is the wrong default for a box that runs a setup
// script somebody found in a repository, and it is much the wrong default for
// one running an unattended agent with the owner's decrypted credentials.
//
// WHAT AN EMPTY RULE-SET ACTUALLY MEANS, because the name suggests deny-all and
// it is not: sluice checks its BASE allowlist first and grants unconditionally
// (Policy.AllowedFor), so a governed sandbox with no patterns of its own
// reaches exactly the operator's trusted list — pypi, npm, crates, the Go
// proxy, github, api.anthropic.com — plus the domains its own repo attachments
// imply. The base list is a floor, not a ceiling. So this is "the trusted
// defaults" and not "nothing".
//
// WHY ONLY ON CREATE, AND ONLY WHEN NOTHING ELSE GOVERNS. resolveEnvRules'
// comment warns that quietly creating an empty rule-set would cut every sandbox
// on a tag down to the base allowlist — a policy nobody wrote, discovered as a
// build that cannot reach the internet. That warning is right, and it is about
// doing this to an environment that already exists. Doing it at the moment the
// environment is BORN is a default rather than a change: there is nothing yet
// to narrow, the rule-set is named after the environment and listed by
// `env show`, and widening it is `net` — an ordinary edit of an ordinary object.
//
// BEST EFFORT, NEVER FATAL. The environment and everything the caller actually
// asked for are already written by the time this runs. Failing the whole verb
// because a default could not be added would report failure for a command that
// mostly succeeded, so a failure here is logged and the environment is open —
// which is exactly the state every environment was in before this existed.
func (o *Ops) defaultEnvEgress(owner, name string, a EnvArgs, creating, attachedRules bool) bool {
	if o.netrules == nil || a.OpenEgress || attachedRules || !creating {
		return false
	}
	list, err := o.netrules.ListRules(owner)
	if err != nil {
		o.log.Warn("could not read egress rule-sets while defaulting an environment's",
			"user", owner, "env", name, "err", err)
		return false
	}
	// SANDBOXES CAN CARRY THE TAG BEFORE THE ENVIRONMENT EXISTS, which is the
	// case that breaks the "there is nothing yet to narrow" argument this
	// default rests on. Tags are free-form: `ctl create scratch --tag web` and
	// `ctl tag` write sandbox_tags with no environment of that name anywhere.
	// So somebody can have been running boxes tagged `web` with unrestricted
	// egress for months, and then `env create web` would cut all of them down
	// to an allowlist they never chose — a live narrowing dressed as a default,
	// which is exactly what resolveEnvRules' comment warns against.
	//
	// When that is the situation, the environment is created OPEN and the
	// owner can choose the policy deliberately. Better a default that
	// occasionally does not apply than one that occasionally cuts somebody's
	// running boxes off from the internet.
	if boxes := o.sandboxesWithTag(owner, name); len(boxes) > 0 {
		o.log.Info("not defaulting an environment's egress: sandboxes already carry that tag",
			"user", owner, "env", name, "sandboxes", len(boxes))
		return false
	}
	for _, m := range list {
		// Something already governs this tag: the owner has a policy for these
		// sandboxes and a second, narrower rule-set beside it is not a default.
		if slices.Contains(m.Tags, name) {
			return false
		}
		// A rule-set of this NAME already exists, carrying other tags. PutRule
		// is an upsert keyed on (owner, name), so writing ours would REPLACE
		// it: its allow list gone, and its tags re-pointed at this environment
		// — which un-governs every sandbox it was protecting. Tag namespaces
		// and rule-set namespaces are separate, so a collision here is somebody
		// having a `web` rule-set for their `prod` boxes and then making an
		// environment called `web`, which is an ordinary thing to do and must
		// not cost them their policy.
		if m.Name == name {
			o.log.Info("not defaulting an environment's egress: a rule-set of that name already exists",
				"user", owner, "env", name, "tags", m.Tags)
			return false
		}
	}
	if err := o.netrules.PutRule(owner, name,
		netrules.RuleSpec{Allow: slices.Clone(defaultEnvAllow)}, []string{name}); err != nil {
		o.log.Warn("could not give a new environment its default egress rule-set",
			"user", owner, "env", name, "err", err)
		return false
	}
	o.log.Info("environment given the default egress rule-set", "user", owner, "env", name)
	return true
}

// reclaimDefaultEgress deletes the rule-set defaultEnvEgress created for this
// environment, and nothing else.
//
// It exists because `env create` leaves an object behind that `env rm` used to
// strand. The auto-created rule-set is the one thing in an environment's
// composition that nobody typed, and rule-sets have no `ctl` verb at all — they
// are edited in the user console — so an owner who made and removed three
// environments from a terminal was left with three rule-sets they had never
// seen, could not list, and could not delete. That is not "we do not destroy
// what we did not create"; that is litter.
//
// THREE CONDITIONS, ALL OF THEM ABOUT NOT DELETING SOMEBODY'S POLICY. It has to
// still be the object this package wrote: named for the environment, carrying
// that tag and no other, and allowing exactly what defaultEnvAllow allows. An
// owner who added their internal mirror to it has made it theirs, and it is
// then reported as surviving the delete like every other rule-set.
//
// The fourth condition is the sandboxes, and it is the one that is about
// egress rather than about ownership. A rule-set is what makes a sandbox
// GOVERNED at all — sluice runs --open-untagged, so a box no rule-set covers is
// unfiltered — and deleting this one while boxes still carry the tag would
// widen their egress as a side effect of tidying up a name. defaultEnvEgress
// refuses to create one when sandboxes already carry the tag, for the mirror
// image of this reason; between the two, the rule-set is only ever created and
// only ever removed when nothing is running under it, and no policy push is
// needed here because no live sandbox changed.
func (o *Ops) reclaimDefaultEgress(owner, name string, tagged []string) string {
	if o.netrules == nil || len(tagged) > 0 {
		return ""
	}
	list, err := o.netrules.ListRules(owner)
	if err != nil {
		o.log.Warn("could not read egress rule-sets while removing an environment",
			"user", owner, "env", name, "err", err)
		return ""
	}
	i := slices.IndexFunc(list, func(m netrules.RuleMeta) bool { return m.Name == name })
	if i < 0 {
		return ""
	}
	m := list[i]
	if len(m.Tags) != 1 || m.Tags[0] != name || !sameDomainSet(m.Spec.Allow, defaultEnvAllow) {
		return ""
	}
	if err := o.netrules.DeleteRule(owner, name); err != nil {
		o.log.Warn("could not remove the egress rule-set an environment was given",
			"user", owner, "env", name, "err", err)
		return ""
	}
	o.log.Info("removed the default egress rule-set with its environment", "user", owner, "env", name)
	return name
}

// sameDomainSet compares two allow lists as sets, because order and repetition
// are not what makes a rule-set somebody else's.
func sameDomainSet(a, b []string) bool {
	x, y := slices.Clone(a), slices.Clone(b)
	slices.Sort(x)
	slices.Sort(y)
	return slices.Equal(slices.Compact(x), slices.Compact(y))
}

// defaultEnvAllow is what a new environment's rule-set allows ON TOP of the
// operator's base allowlist, which every governed sandbox already gets.
//
// IT IS THE COMMON TRUSTED SET, not a hand-picked minimum, and that is a
// correction of what it used to be. It started as the three Ubuntu archives and
// nothing else, on the argument that the base allowlist already carried the
// language registries and only apt was missing — which was true of the base
// allowlist on the machine it was written for, and true of nothing else. What
// an environment build actually reaches for is a moving target chosen by
// whatever the project needs: a registry for its container base image, a cloud
// SDK's API, a JDK download, a Rust toolchain. Each one that is missing is
// twenty minutes of build ending in a name that does not resolve, and the
// person reading that failure cannot tell it from a network fault.
//
// So the default is netrules.TrustedDomains — the set the vendor of the agent
// doing the building publishes as trusted, which is the same set the console's
// prefill button offers, so the two paths into a rule-set agree. It is wide.
// The trade is deliberate: a rule-set that stops a build for its own good is
// one people turn off entirely, and an environment governed by nothing is worse
// than one governed by this.
//
// FOUR NAMES ARE ADDED ON TOP of it. The three Ubuntu archives are redundant —
// sluice takes a bare name as "this domain and its subdomains" and the trusted
// set carries `ubuntu.com` — and they are kept because the very first
// instruction the agent prompt gives is `sudo apt-get -y`, so a reader deleting
// a name from this file should have to notice that. `docker.io` is NOT
// redundant: the trusted set names registry-1, auth, index and hub
// individually, which is every host a `docker pull` uses today and no host it
// might use tomorrow. Docker is on by default in every template and a project
// whose setup is `docker compose up` reaches a registry before it reaches
// anything else, so the bare name is the one that does not need revisiting.
//
// Both architectures, because a template is amd64 on CKS and arm64 on the DGX
// and the rule-set is written by the gateway without knowing which the builder
// will land on. ports.ubuntu.com serves arm64; archive/security serve amd64.
//
// It is an ordinary rule-set once written, so an owner who wants less can trim
// it and one who needs their internal mirror adds it — which is the whole
// reason this is a stored object and not a constant in the pusher. It is also
// why this list only reaches environments created from now on: an existing
// rule-set is somebody's policy, and `net add` is how it changes.
var defaultEnvAllow = append(slices.Clone(netrules.TrustedDomains),
	"archive.ubuntu.com",
	"security.ubuntu.com",
	"ports.ubuntu.com",
	"docker.io",
)

// resolveEnvRules is resolveEnvSecrets for egress rule-sets, and it refuses an
// unknown name for a sharper reason: a rule-set is SUBTRACTIVE, so an
// environment that quietly created an empty one would cut every sandbox
// carrying its tag down to the base allowlist — a policy nobody wrote,
// discovered as a build that cannot reach the internet.
func (o *Ops) resolveEnvRules(op, owner string, names []string) ([]netrules.RuleMeta, error) {
	if len(names) == 0 {
		return nil, nil
	}
	list, err := o.netrules.ListRules(owner)
	if err != nil {
		return nil, Fail(op, err)
	}
	out := make([]netrules.RuleMeta, 0, len(names))
	for _, want := range names {
		want = strings.TrimSpace(want)
		i := slices.IndexFunc(list, func(m netrules.RuleMeta) bool { return m.Name == want })
		if i < 0 {
			return nil, Invalid(op, "no_such_rule", "no egress rule-set named %s — see `net ls`", want)
		}
		out = append(out, list[i])
	}
	return out, nil
}

// sandboxesWithTag names the caller's OWN running sandboxes carrying tag.
//
// It walks the owner's list and asks the tag store per box rather than joining,
// because Tagger is deliberately the narrow pair TagsFor/SetTags and a
// list-boxes-by-tag method would be a second authority on a question the
// sandbox list already answers. Owner scoping is structural: the list itself is
// owner-scoped, so a tag string somebody else also uses cannot appear.
//
// Best effort throughout: this drives a re-push, and a tag lookup that stumbles
// must not turn a completed write into a failure.
func (o *Ops) sandboxesWithTag(owner, tag string) []string {
	if o.tags == nil || o.boxes == nil {
		return nil
	}
	var out []string
	for _, b := range o.boxes.ListByOwner(owner) {
		tags, err := o.tags.TagsFor(b.Name)
		if err != nil {
			o.log.Warn("could not read a sandbox's tags while composing an environment",
				"sandbox", b.Name, "err", err)
			continue
		}
		if slices.Contains(tags, tag) {
			out = append(out, b.Name)
		}
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------------------
// Small shared pieces
// ---------------------------------------------------------------------------

// The two disabled sentences, named so the several places that answer with them
// cannot drift into several different explanations of the same missing store.
const (
	envDisabledSentence     = "environments are not enabled on this host"
	envVarsDisabledSentence = "environment variables are not enabled on this host"
)

// envName normalizes and checks the name a caller gave.
//
// It lowercases, because the name IS the tag and every other door onto a tag
// lowercases (NormalizeTags does) — a user who types `Web` means the tag `web`,
// and refusing them a spelling lecture is worth two lines here. The grammar
// itself belongs to the store, which owns the sentence; this only refuses the
// empty string, which the store would report as a grammar failure without ever
// saying that nothing was typed.
func envName(op, name string) (string, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return "", Invalid(op, "missing_env_name", "which environment? give its name")
	}
	return name, nil
}

// envStoreError maps the envs store's sentinels onto this package's taxonomy.
//
// ErrNoSuchEnvironment becomes the masked NotFound, identical for an
// environment nobody has and for one belonging to somebody else — the store's
// queries are owner-scoped, so the two arrive here indistinguishable and must
// leave that way. The validation sentinels pass through verbatim: the store's
// sentences about the reserved name and the grammar are already user-facing and
// more specific than anything this layer could say about the same argument.
func envStoreError(op, name string, err error) *Error {
	switch {
	case errors.Is(err, envs.ErrNoSuchEnvironment):
		return NotFound(op, "environment", name)
	case errors.Is(err, envs.ErrReservedName):
		return verbatim(Invalid(op, "reserved_env_name", "%v", err))
	case errors.Is(err, envs.ErrTooManyEnvironments):
		return &Error{
			Kind: KindLimit, Op: op, Code: "too_many_environments",
			Msg:      err.Error(),
			Hint:     "Delete one you no longer use: env rm <name>",
			Verbatim: true,
			Err:      err,
		}
	case errors.Is(err, envs.ErrInvalidName):
		return verbatim(Invalid(op, "bad_env_name", "%v", err))
	default:
		return Fail(op, err)
	}
}

// union normalizes a tag list and folds in any extras, sorted and deduped. It
// is the ONE place attaching-means-adding is implemented, so no verb here can
// accidentally replace a tag set it meant to extend.
func union(tags []string, extra ...string) []string {
	out := append(slices.Clone(tags), extra...)
	slices.Sort(out)
	return slices.Compact(out)
}

// nonNil replaces nil slices with empty ones so a JSON encoding renders [] and
// not null. Every result in this file is a listing of things that may legally
// be absent, which is exactly the shape that reads as a bug when it serializes
// as null.
func nonNil(lists ...*[]string) {
	for _, l := range lists {
		if *l == nil {
			*l = []string{}
		}
	}
}
