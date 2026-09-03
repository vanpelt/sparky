package ctlops

// The environment build: the verb that turns a composition into a disk.
//
// `env build web` creates ONE ordinary sandbox, tagged `web`, booted from the
// operator's base image, and asks its guest to run a setup script in the
// primary checkout. When the guest reports success the GATEWAY — never the
// guest — captures that sandbox and binds the capture to the tag `web`, so
// every later `create --env web` boots the disk the script produced. The
// builder is then destroyed.
//
// Four decisions shape every line below.
//
// THE DURABLE STATE IS THE ENVIRONMENT ROW, NOT A JOB. jobs.go's registry is
// in-memory and REST-only (jobs.go:16-21): a gateway restart forgets every Job
// it holds. A build outlives a restart — the builder VM is still there, holding
// a slot and the owner's decrypted secrets — so what says "a build is in
// flight" has to be a row in sqlite, which is exactly what build_state and
// build_box are. That is also what makes ReconcileEnvironmentBuilds possible;
// there is nothing a job registry could reconcile from.
//
// THE GUEST IS NEVER TOLD ANYTHING. StartSetup carries no script, no
// environment name and no owner. The guest fetches its own work from the
// metadata service over its tap (GET /self/setup) and reports on the same
// channel (POST /self/setup/result), and SetupFor answers from the sandbox
// RECORD the host resolved from that tap. So the owner check below is not
// belt-and-braces: sandbox names are global, `web-build` is a name anyone may
// take, and matching a builder by name alone would hand one owner's setup
// script — and with it the shape of their private toolchain — to another
// owner's box.
//
// EVERY REFUSAL COMES BEFORE THE FIRST WRITE, which for this verb means before
// SetState(building). There is no transaction spanning the environment store,
// the tag store and the VM manager, so ordering is the only thing that makes
// "it failed" mean "nothing moved" — Create's rule (sandbox.go:56-70) and
// PutEnvironment's, applied to a third set of stores. The snapshot and binding
// capabilities are refused UP FRONT for the sharper version of the same reason
// SnapshotToTag gives at template.go:198: twenty minutes of work whose whole
// point is the binding must not end in "this host cannot record one".
//
// PHASE B IS SCRIPT MODE ONLY. The agent path (`claude -p` writing the script
// it then leaves behind in the repo) is Phase C. The seam is the setup_from
// column, which already spells `agent`, and line 2 of the guest's fetch, which
// already carries a mode; nothing here needs to change to add it. Until then a
// person with no script is told so plainly, and told that the agent path does
// not exist yet, rather than being left to read a refusal as a bug.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/envs"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/ghapp"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/repos"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/secrets"
)

// DefaultEnvBuildTimeout is how long a build may stay in `building` before the
// reconciler gives up on it.
//
// It is generous because the thing it bounds is a person's setup script: an
// `apt-get`, a `uv sync`, a cold `npm ci` and a first `cargo build` on a fresh
// disk are each minutes, and a build cut off at twenty would be reported as a
// failure to somebody watching it work. It is bounded at all because the other
// end of the trade is a builder VM holding a slot, a 25 GB rootfs and the
// owner's decrypted secrets with nobody waiting on it.
const DefaultEnvBuildTimeout = 45 * time.Minute

// SetupScriptPath is the file the seed read looks for in an attached
// repository. It is the path the platform has been telling agents to write
// since deploy/refresh-agent-tools.sh started installing that instruction into
// every template's ~/.agents/AGENTS.md, and the one internal/guestdocs
// documents; it is named here rather than inlined because a second spelling of
// it would be a feature that silently never triggers.
const SetupScriptPath = ".sparkbox/setup.sh"

// SetupJob is what a builder is told to do, as this package produces it.
//
// It is field-for-field metadata.SetupJob and separate for the same unavoidable
// reason SetupReport is: internal/metadata imports this package, so this
// package cannot import it back. The two structs are CONVERTIBLE — keep the
// field lists identical or the one-line adapter in cmd/sparkbox stops
// compiling, which is the failure you want.
type SetupJob struct {
	Env     string
	Mode    string
	Payload string
}

// The two ways an environment can be built. Mirrors of metadata's constants,
// same values, for the same import-direction reason as the struct above.
const (
	// SetupModeScript runs .sparkbox/setup.sh. Deterministic, reviewable, and
	// what every later build of the environment takes once one exists.
	SetupModeScript = "script"
	// SetupModeAgent runs an agent against the platform's own dev-environment
	// guidance and keeps the .sparkbox/setup.sh it writes. It is how an
	// environment gets its FIRST script; the script is the deliverable, so the
	// second build of the same environment is a script build.
	SetupModeAgent = "agent"
)

// SetupReport is what the guest said happened, as this package receives it.
//
// It is field-for-field metadata.SetupResult, and it is a separate type for one
// unavoidable reason: internal/metadata imports this package (selflifecycle.go
// does, for ctlops.SelfSnapshotPlan), so ctlops cannot import metadata back.
// The two structs are CONVERTIBLE — same fields, same types, same order — so
// the adapter in cmd/sparkbox that bridges them is one conversion and no
// copying, exactly as selfLifecycleOps bridges SelfLifecycle today. Keep the
// field lists identical or that conversion stops compiling, which is the
// failure mode you want.
type SetupReport struct {
	OK       bool
	ExitCode int
	Script   string // the .sparkbox/setup.sh the run ended with; "" when unchanged
	Log      string // bounded tail, guest-authored
}

// maxBuildErrorRunes bounds what a guest's log tail may contribute to
// build_error. The column is rendered inline by `env ls`, by `env show` and by
// resolveEnvTag's refusal when somebody tries to boot a failed environment, so
// it has to stay a sentence rather than a transcript. The whole tail is not
// lost — it is in the builder, which is left paused precisely so it can be read
// there.
const maxBuildErrorRunes = 200

// builderName is the sandbox an environment builds in. It is an ORDINARY
// sandbox name in the global namespace, deliberately: `-build` is not reserved
// in internal/reserved and must not be, because that list exists for proxy
// routing collisions (the `-xterm` suffix), and a builder box is a box like any
// other — it gets a subdomain, a terminal and an ssh door, all of which are how
// somebody rescues a build that went wrong.
func builderName(env string) string { return env + "-build" }

// envBuildKey is the singleflight key: owner, then NUL, then the environment.
// The handle comes first so one owner's key can never collide with another's,
// and NUL is the separator because it is the one byte neither a handle nor a
// tag may contain — so no two distinct pairs can render to the same key. Same
// construction, and the same reasoning, as internal/launch's create key.
func envBuildKey(owner, env string) string { return owner + "\x00" + env }

// buildTimeout is the configured budget or the default.
func (o *Ops) buildTimeout() time.Duration {
	if o.envBuildTimeout > 0 {
		return o.envBuildTimeout
	}
	return DefaultEnvBuildTimeout
}

// ---------------------------------------------------------------------------
// build
// ---------------------------------------------------------------------------

// BuildEnvironment starts a build and returns the environment as it now stands
// — in `building`, naming its builder box. It does NOT wait for the setup
// script: that runs for minutes inside the guest and lands back through
// SetupDone.
//
// What it does wait for is the create, which is bounded by Create's own
// fifteen-second dial budget. That is deliberate. A create that cannot happen —
// the name is taken, the host is at capacity, the template is missing — is a
// refusal the person typing this can act on immediately, and discovering it
// from a row that says `failed` a minute later would be strictly worse.
func (o *Ops) BuildEnvironment(ctx context.Context, c Caller, name string) (EnvironmentInfo, error) {
	const op = "env.build"

	// ---- refusals, every one of them before the first write ----

	if o.envs == nil {
		return EnvironmentInfo{}, Disabled(op, envDisabledSentence)
	}
	name, err := envName(op, name)
	if err != nil {
		return EnvironmentInfo{}, err
	}
	if err := o.envBuildable(op); err != nil {
		return EnvironmentInfo{}, err
	}
	// Owner-scoped, so another owner's environment and one nobody has are the
	// same masked answer from the same line.
	e, err := o.envs.Get(c.Handle, name)
	if err != nil {
		return EnvironmentInfo{}, envStoreError(op, name, err)
	}
	if e.State == envs.StateBuilding {
		return EnvironmentInfo{}, o.alreadyBuilding(op, e)
	}
	box := builderName(name)
	// A distinct refusal rather than Create's generic name_taken, because the
	// name was chosen by this command and not by the caller: somebody who runs
	// `env build web` twice after a failure needs to be told that the LAST
	// build's box is still sitting there, and what the two ways out of that
	// are. nameIsFree's sentence would send them looking for a sandbox they do
	// not remember making.
	if _, taken := o.boxes.Get(box); taken {
		return EnvironmentInfo{}, &Error{
			Kind: KindConflict, Op: op, Code: "build_box_exists",
			Msg: "a sandbox named " + box + " already exists, and that is the name this build needs. " +
				"If it is a builder left over from a previous attempt, finish it with `env capture " + name +
				"` or remove it with `rm " + box + "`.",
			Details:  map[string]any{"environment": name, "sandbox": box},
			Verbatim: true,
		}
	}

	// The script. The stored one wins; otherwise the attached repositories are
	// read for one, and finding one WRITES it to the row (setup_from=repo) —
	// the first write of this verb, and the last thing before the state moves.
	script, err := o.resolveSetupScript(ctx, op, c, e)
	if err != nil {
		return EnvironmentInfo{}, err
	}

	// NO SCRIPT ANYWHERE IS NOT A REFUSAL ANY MORE; it is the agent path. This
	// is the one line the whole of Phase C turns on, and the sentence it
	// replaced ("Having an agent write the script for you is not available
	// yet") is why that refusal named the missing feature instead of merely
	// failing.
	//
	// The agent path has its own refusals and every one of them is HERE, above
	// the first write, for the reason this file's header states: an agent build
	// discovered to be impossible after SetState(building) is forty-five
	// minutes of a row nobody can move.
	mode := SetupModeScript
	if script == "" {
		if err := o.agentBuildable(op, c.Handle, name); err != nil {
			return EnvironmentInfo{}, err
		}
		mode = SetupModeAgent
	}

	// ---- the work ----
	//
	// Detached from the caller's cancellation and bounded by the build budget,
	// behind a singleflight keyed on (owner, environment). The detachment is
	// internal/launch's argument: singleflight runs the closure on the LEADER's
	// goroutine, so a leader whose ssh session drops would otherwise abandon a
	// create a follower is still waiting on — and an abandoned create is the
	// state that leaves a name taken and tag rows written for a sandbox nobody
	// can see. The collapse is what stops a double-submitted `env build` from
	// racing two creates past the already-building refusal, which is a check
	// and not a lock.
	shared, cancel := context.WithTimeout(context.WithoutCancel(ctx), o.buildTimeout())
	defer cancel()
	got, err, _ := o.envBuilds.Do(envBuildKey(c.Handle, name), func() (any, error) {
		return o.startBuild(shared, c, name, box, mode, script)
	})
	if err != nil {
		return EnvironmentInfo{}, AsError(op, err)
	}
	info, ok := got.(EnvironmentInfo)
	if !ok {
		// Unreachable: startBuild returns an EnvironmentInfo or an error. It is
		// asserted rather than assumed because a type assertion on a shared
		// `any` is exactly where a later edit returning a pointer would panic
		// inside somebody else's goroutine.
		return EnvironmentInfo{}, Fail(op, errBadBuildShare)
	}
	return info, nil
}

// errBadBuildShare names the unreachable case above so the assertion has
// something to report other than a panic.
var errBadBuildShare = errors.New("ctlops: the shared environment build returned an unexpected value")

// startBuild is the collapsed half: move the row, make the builder, nudge it.
//
// Every failure after SetState leaves the row in `failed` with a sentence,
// never in `building`. A row stuck in `building` with no builder is the one
// state nothing can recover from on its own except the reconciler's timeout,
// and making somebody wait forty-five minutes to be told the create failed is
// not a trade worth taking.
func (o *Ops) startBuild(ctx context.Context, c Caller, name, box, mode, script string) (EnvironmentInfo, error) {
	const op = "env.build"

	// THE MODE IS STAMPED ON THE ROW BEFORE THE STATE MOVES, and this is the
	// only durable record that this build is an agent build.
	//
	// It has to be durable because two readers need it long after this call
	// returns and with nothing else to go on: SetupFor, answering a guest that
	// may boot minutes from now, and ReconcileEnvironmentBuilds, deciding after
	// a gateway restart whether an expired builder is a paused disk somebody
	// should finish by hand or an unattended agent that must be destroyed.
	//
	// setup_from is the column, and it is honest here rather than overloaded:
	// its contract is "where the setup script came from", an empty script with
	// from=agent means "an agent is writing one", and the store documents that
	// an empty script with a `from` is distinguishable from never having
	// looked. When the agent reports its script, recordReportedScript keeps the
	// `agent` origin it finds — which is how the row ends up saying, correctly,
	// that its script was written by an agent.
	if mode == SetupModeAgent {
		if err := o.envs.SetScript(c.Handle, name, "", envs.SetupFromAgent); err != nil {
			return EnvironmentInfo{}, envStoreError(op, name, err)
		}
	}

	if err := o.envs.SetState(c.Handle, name, envs.StateBuilding, box, ""); err != nil {
		return EnvironmentInfo{}, envStoreError(op, name, err)
	}
	// An ordinary create, with no special case anywhere: the tag is the
	// environment's name, so the builder gets that environment's secrets, vars,
	// checkouts and egress policy through the four joins that already exist.
	//
	// FromBase is the one thing that is not ordinary, and it is what makes a
	// rebuild reproducible: an environment that built from its OWN last
	// snapshot would accumulate every side effect of every previous build, and
	// the second rebuild would no longer be derivable from the script.
	if _, err := o.Create(ctx, c, CreateArgs{
		Name: box, Tags: []string{name}, FromBase: true,
	}); err != nil {
		e := AsError(op, err)
		o.markFailed(c.Handle, name, "", "the builder sandbox could not be created: "+e.Msg)
		return EnvironmentInfo{}, e
	}
	// THE POLICY GOES IN BEFORE THE GUEST IS TOLD TO START, and for agent mode
	// that ordering is the mitigation rather than an optimisation.
	//
	// Egress policy is otherwise pushed by a thirty-second sweep, so a sandbox
	// created between sweeps is absent from sluice's snapshot — and under
	// `--enforce --open-untagged`, absent means UNRESTRICTED. Every other
	// sandbox can wait: it was made by a person who is about to ssh into it.
	// This one starts an unattended agent with the owner's decrypted
	// credentials within seconds of booting, so waiting would mean the agent
	// spends its first half-minute with exactly the open egress the default
	// rule-set exists to deny it.
	if err := o.pushBuilderEgress(ctx, c.Handle, name, box, mode); err != nil {
		return EnvironmentInfo{}, err
	}
	if err := o.nudgeBuilder(ctx, c, name, box); err != nil {
		return EnvironmentInfo{}, err
	}
	o.log.Info("environment build started", "user", c.Handle, "env", name, "sandbox", box,
		"mode", mode, "script_bytes", len(script))
	return o.environmentInfo(c.Handle, name)
}

// pushBuilderEgress puts this builder into the egress policy before its guest
// is nudged. The two modes weigh a failure differently, and that is the whole
// design of this function.
//
// SCRIPT mode: a warning. The script is the owner's own, they wrote it or they
// read it in a pull request, and failing a build they were going to get over a
// policy push that will be retried by the sweep thirty seconds later would
// trade a real build for a brief window on code they already trust.
//
// AGENT mode: a REFUSAL, and the builder is failed. The security argument for
// running `claude -p --permission-mode bypassPermissions` unattended is that it
// happens in a governed box; if this host cannot confirm the box is governed,
// that argument does not hold, and starting anyway would be running the agent
// under a mitigation that is documented and absent. Refusing costs one build.
//
// A nil pusher is not a failure in either mode: it is a host with no egress
// control at all, where nothing was ever governed and saying so on this one
// verb would be arbitrary.
func (o *Ops) pushBuilderEgress(ctx context.Context, owner, name, box, mode string) error {
	const op = "env.build"
	if o.netPusher == nil {
		return nil
	}
	push, cancel := context.WithTimeout(context.WithoutCancel(ctx), PauseTimeout)
	defer cancel()
	err := o.netPusher.PushNet(push)
	if err == nil {
		return nil
	}
	if mode != SetupModeAgent {
		o.log.Warn("could not push egress policy before starting a build; the periodic sweep will",
			"user", owner, "env", name, "sandbox", box, "err", err)
		return nil
	}
	o.log.Error("refusing to start an agent build whose builder could not be given an egress policy",
		"user", owner, "env", name, "sandbox", box, "err", err)
	o.failBuild(ctx, owner, name, box,
		"the builder's egress policy could not be applied, and an agent is not run in a box whose egress is unknown")
	return &Error{
		Kind: KindUpstream, Op: op, Code: "env_egress_unavailable",
		Msg: "the egress policy for " + box + " could not be applied, so the agent that would write " +
			name + "'s setup script was not started.",
		Hint: "This is usually the egress daemon being briefly unavailable — try again. To build without " +
			"an agent, write " + SetupScriptPath + " in a repository attached to " + name + ".",
		Details:  map[string]any{"environment": name, "sandbox": box},
		Verbatim: true,
		Err:      err,
	}
}

// nudgeBuilder asks the builder's guest to fetch and run its job.
//
// A guest that cannot take the job fails the build IMMEDIATELY rather than
// leaving it to the reconciler's timeout. The builder is left PAUSED and named
// in the sentence, on the same terms as every other build failure: the box is
// where the evidence is, and a person can ssh in, do the work by hand and run
// `env capture`.
func (o *Ops) nudgeBuilder(ctx context.Context, c Caller, name, box string) error {
	const op = "env.build"
	if o.setupStarter == nil {
		o.failBuild(ctx, c.Handle, name, box,
			"this host cannot start a setup run inside a sandbox, so the builder has nothing to do")
		return Disabled(op, "environment builds are not enabled on this host")
	}
	b, ok := o.boxes.Get(box)
	if !ok {
		o.markFailed(c.Handle, name, "", "the builder sandbox disappeared before it could be started")
		return Fail(op, errBuilderVanished)
	}
	if err := o.setupStarter.StartSetup(ctx, b); err != nil {
		// The one failure a person can act on, and the one they are most likely
		// to hit right after this ships: every template built before the guest
		// unit existed. Saying "the builder never reported" for it — which is
		// what the timeout would eventually say — names the wrong cause
		// entirely.
		if errors.Is(err, host.ErrNoEnvSetup) {
			o.failBuild(ctx, c.Handle, name, box,
				"the base image in "+box+" predates environment builds and cannot run a setup script")
			return &Error{
				Kind: KindConflict, Op: op, Code: "env_setup_unsupported",
				Msg: "the base image this host boots predates environment builds, so " + box +
					" cannot run a setup script.",
				Hint:     "Refresh the host's base image, then run this again.",
				Details:  map[string]any{"environment": name, "sandbox": box},
				Verbatim: true,
			}
		}
		o.failBuild(ctx, c.Handle, name, box, "the builder could not be asked to run the setup script")
		o.log.Error("could not start an environment's setup run",
			"user", c.Handle, "env", name, "sandbox", box, "err", err)
		return Fail(op, err)
	}
	return nil
}

var errBuilderVanished = errors.New("the builder sandbox disappeared before it could be started")

// envBuildable is the capability gate every build path shares, refused UP
// FRONT.
//
// Snapshots and bindings are the whole point of a build — the disk and the
// pointer to it — so a host without them cannot do this at all, and finding
// that out after the setup script has run would waste the expensive half to
// reach a state that was knowable in a nanosecond. Tagging is here for the same
// reason one step removed: the builder is created WITH a tag, so a host with no
// tag store would refuse the create, after the row had already moved to
// `building`.
func (o *Ops) envBuildable(op string) error {
	if o.templates == nil || !o.templates.Snapshotter() {
		return Disabled(op, "snapshots are not supported by this driver, so an environment cannot be built")
	}
	if o.templateTags == nil {
		return Disabled(op, "template bindings are not enabled on this host")
	}
	if o.tags == nil {
		return Disabled(op, "tagging is not enabled on this host")
	}
	return nil
}

// alreadyBuilding is its own refusal rather than a generic conflict because
// somebody re-running the command is asking where their build is, and the
// answer is a box name they can ssh into and watch.
func (o *Ops) alreadyBuilding(op string, e envs.Environment) *Error {
	msg := "environment " + e.Name + " is already building" + buildBoxPhrase(e) + "."
	hint := "Watch it with `env show " + e.Name + "`."
	if e.BuildBox != "" {
		hint = "Watch it with `env show " + e.Name + "`, or look inside with `" +
			o.sshHint(e.BuildBox) + "`. If that build is stuck, `rm " + e.BuildBox + "` and start again."
	}
	return &Error{
		Kind: KindConflict, Op: op, Code: "env_already_building",
		Msg: msg, Hint: hint,
		Details:  map[string]any{"environment": e.Name, "sandbox": e.BuildBox},
		Verbatim: true,
	}
}

// AgentCredentials are the secrets that can sign an agent in, EITHER of which
// is enough. The first is the name the platform's own `secret set` tip uses,
// which is what makes the refusal below repairable by copying one line; the
// second is the API-billing path docs/onboarding-users.md documents in the same
// breath ("`ANTHROPIC_API_KEY` works too if you bill by API").
//
// Both, because checking only the first would refuse an agent build for
// somebody who is perfectly well authenticated — a false refusal in a gate
// whose entire justification is that it saves people from a false failure.
var AgentCredentials = []string{"CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_API_KEY"}

// AgentCredential is the one the repair sentence names, because a token is what
// most people have and `claude setup-token` is one command.
const AgentCredential = "CLAUDE_CODE_OAUTH_TOKEN"

// agentBuildable is every refusal the AGENT path can decide before anything
// moves. Script mode reaches none of it.
//
// There is exactly one, and the design doc argues for it at length: refuse when
// the owner has no agent credential that will reach the builder. The
// alternative is genuinely bad and is what happens without this check — the
// builder boots, `claude -p` hits an authentication error it cannot answer,
// exits 1, and the person is told "the setup run exited 1" with the real cause
// three layers down in a log tail. Costing a VM boot to produce a sentence this
// host can produce from a listing is the wrong trade.
//
// IT DECRYPTS NOTHING. ListSecrets returns SecretMeta — name, tags, version,
// timestamps and deliberately no value at all — and the store does not even
// select the ciphertext column. So this answers on a host whose KEK is broken,
// which is correct: whether a credential EXISTS is not a secret from the person
// who stored it.
func (o *Ops) agentBuildable(op, owner, env string) error {
	if o.secrets == nil {
		// No secret store means the token can never be delivered, so an agent
		// build cannot work here — but a script build still can, and saying so
		// is the difference between a dead end and a next step.
		return &Error{
			Kind: KindDisabled, Op: op, Code: "env_no_setup",
			Msg: "environment " + env + " has no setup script, and this host cannot run an agent to write one " +
				"because it has no secret store to deliver the agent's credential from.",
			Hint: "Write " + SetupScriptPath + " in a repository attached to " + env +
				", or pipe one in with `env script " + env + " --set`.",
			Details:  map[string]any{"environment": env, "path": SetupScriptPath},
			Verbatim: true,
		}
	}
	list, err := o.secrets.ListSecrets(owner)
	if err != nil {
		return Fail(op, err)
	}
	// THE TAG PREDICATE IS THE WHOLE CHECK, and getting it wrong in either
	// direction is worse than not checking. The builder is created with the
	// environment's tag, and defaultTags adds `default` — so its tag set is
	// exactly those two, and a secret reaches it if it carries either one.
	//
	// Narrower ("tagged with the environment") would refuse the exact command
	// the platform tells everybody to run, which stores the token on `default`
	// with no --tag at all. Broader ("any tag") would pass a token tagged `ci`
	// that is never going to arrive, and we would be back to discovering it
	// from a 401 in a builder.
	var outOfReach *secrets.SecretMeta
	for _, m := range list {
		if !slices.Contains(AgentCredentials, m.Name) {
			continue
		}
		if slices.Contains(m.Tags, env) || slices.Contains(m.Tags, secrets.DefaultTag) {
			return nil
		}
		// Remembered, not returned: another credential later in the list may
		// still be in reach, and refusing on the first out-of-reach one would
		// refuse a build that was going to work.
		if outOfReach == nil {
			outOfReach = &m
		}
	}
	if outOfReach != nil {
		// The credential exists but is tagged out of reach. That is a different
		// problem from not having one, and the repair is a retag rather than a
		// login, so it gets its own sentence.
		return &Error{
			Kind: KindConflict, Op: op, Code: "env_no_agent_credential",
			Msg: "environment " + env + " has no setup script, so building it means having an agent write one — " +
				"and your " + outOfReach.Name + " is tagged " + strings.Join(outOfReach.Tags, ", ") +
				", which no builder for " + env + " will carry.",
			Hint: "Re-save it so it reaches this environment: `claude setup-token | ssh ctl@<gateway> secret set " +
				outOfReach.Name + " --tag " + env + "`. Or write " + SetupScriptPath +
				" yourself and skip the agent entirely.",
			Details:  map[string]any{"environment": env, "secret": outOfReach.Name, "tags": outOfReach.Tags},
			Verbatim: true,
		}
	}
	return &Error{
		Kind: KindConflict, Op: op, Code: "env_no_agent_credential",
		Msg: "environment " + env + " has no setup script, so building it means having an agent write one — " +
			"and you have no " + AgentCredential + " for the builder to sign in with.",
		Hint: "Save one with `claude setup-token | ssh ctl@<gateway> secret set " + AgentCredential +
			"`, then run this again. Or write " + SetupScriptPath +
			" in a repository attached to " + env + " and it will be used instead of an agent.",
		Details:  map[string]any{"environment": env, "secret": AgentCredential},
		Verbatim: true,
	}
}

// noSetupScript is the refusal that has to teach, because it is the one a
// person hits when the feature is working exactly as designed and they simply
// have not written the file yet. It names both ways to fix it and says plainly
// that the third way — having an agent write it — does not exist yet, so that
// its absence does not read as a bug in the two that do.
func (o *Ops) noSetupScript(op, name string) *Error {
	return &Error{
		Kind: KindConflict, Op: op, Code: "env_no_setup",
		Msg: "environment " + name + " has no setup script, so a build would have nothing to run.",
		Hint: "Write " + SetupScriptPath + " in a repository attached to " + name +
			" and it will be picked up automatically, or pipe one in with `env script " + name +
			" --set`. `sparkbox docs dev-environment` describes what belongs in it. " +
			"Having an agent write the script for you is not available yet.",
		Details:  map[string]any{"environment": name, "path": SetupScriptPath},
		Verbatim: true,
	}
}

// ---------------------------------------------------------------------------
// The setup script: stored, or seeded from a repository
// ---------------------------------------------------------------------------

// RepoFileReader is the seed read: one file out of one repository, as bytes.
//
// The BYTES are the whole point. GitHubApp deliberately excludes MintToken
// (ops.go:257-270 — "the discipline that keeps a token out of a log is much
// easier to hold when there is no token in the room"), and this interface keeps
// that property: the installation token that reads a private repository is
// minted, spent and discarded inside internal/ghapp, and what crosses into this
// package is a file. *ghapp.App satisfies it, alongside GitHubApp, from the
// same value.
type RepoFileReader interface {
	ReadFile(ctx context.Context, inst ghapp.Installation, owner, name, ref, path string) ([]byte, error)
}

// resolveSetupScript answers with the script a build should run, writing it to
// the row when it had to go and find it.
//
// THE STORED SCRIPT WINS. A script somebody typed, or one a previous build
// captured, is the record of what this environment actually is; re-reading the
// repository on every build would silently overwrite a hand-fixed script the
// next time anybody rebuilt, which is the failure mode that makes people stop
// trusting the feature. The repository is consulted only when the row has
// nothing — that is what "seed" means.
func (o *Ops) resolveSetupScript(ctx context.Context, op string, c Caller, e envs.Environment) (string, error) {
	if strings.TrimSpace(e.SetupScript) != "" {
		return e.SetupScript, nil
	}
	return o.seedSetupScript(ctx, op, c, e.Name)
}

// seedSetupScript reads .sparkbox/setup.sh out of the environment's attached
// repositories, first hit wins, and records it.
//
// FIRST HIT IN SLUG ORDER, not "the only one" and not a merge. An environment
// with two repositories where both ship a setup script is a composition its
// owner has to disambiguate, and this file has no basis for choosing between
// them — but refusing the build outright would be a refusal nobody can act on
// from the sentence, and running both would run somebody's install twice. So it
// picks deterministically, says which one it picked in the audit line, and
// leaves `env script --set` as the override. The stored script wins from then
// on, so the choice is made once.
//
// Every per-repository failure is SKIPPED rather than fatal. A repository the
// App is not installed on, or one whose ref no longer exists, says nothing
// about the next attachment in the list. The one exception is github.com not
// answering at all: that is reported, because otherwise a network blip would
// present as "you have no setup script" and send somebody off to write one they
// already have.
func (o *Ops) seedSetupScript(ctx context.Context, op string, c Caller, name string) (string, error) {
	if o.repos == nil || o.ghApp == nil || o.repoFiles == nil {
		return "", nil
	}
	list, err := o.repos.ListRepos(c.Handle)
	if err != nil {
		o.log.Warn("could not read repo attachments while seeding a setup script",
			"user", c.Handle, "env", name, "err", err)
		return "", nil
	}
	mine := make([]repos.Repo, 0, len(list))
	for _, r := range list {
		if slicesContainsFold(r.Tags, name) {
			mine = append(mine, r)
		}
	}
	sort.Slice(mine, func(i, j int) bool { return mine[i].Slug < mine[j].Slug })

	var upstream error
	for _, r := range mine {
		owner, repoName, ok := repos.SplitSlug(r.Slug)
		if !ok {
			continue
		}
		inst, err := o.ghApp.InstallationFor(ctx, owner, repoName)
		if err != nil {
			if errors.Is(err, ghapp.ErrUpstream) {
				upstream = err
			}
			continue
		}
		// r.Ref may be empty, which ReadFile takes to mean the repository's
		// default branch — the same thing the checkout the script will run in
		// was made from.
		b, err := o.repoFiles.ReadFile(ctx, inst, owner, repoName, r.Ref, SetupScriptPath)
		switch {
		case errors.Is(err, ghapp.ErrNoSuchFile):
			continue
		case errors.Is(err, ghapp.ErrUpstream):
			upstream = err
			continue
		case err != nil:
			o.log.Warn("could not read a setup script from an attached repository",
				"user", c.Handle, "env", name, "slug", r.Slug, "err", err)
			continue
		}
		script := string(b)
		if strings.TrimSpace(script) == "" {
			// A file that is there and says nothing is not a script. Treating
			// it as one would start a build that captures an untouched base
			// image and calls it an environment.
			continue
		}
		if len(script) > MaxSetupScript {
			// Refused, never truncated: half a script is what every future fork
			// of this environment would run.
			o.log.Warn("an attached repository's setup script is too large to use",
				"user", c.Handle, "env", name, "slug", r.Slug, "bytes", len(script), "max", MaxSetupScript)
			continue
		}
		if err := o.envs.SetScript(c.Handle, name, script, envs.SetupFromRepo); err != nil {
			return "", envStoreError(op, name, err)
		}
		o.log.Info("environment setup script seeded from a repository",
			"user", c.Handle, "env", name, "slug", r.Slug, "ref", r.Ref, "bytes", len(script))
		return script, nil
	}
	if upstream != nil {
		return "", &Error{
			Kind: KindUpstream, Op: op, Code: "github_unreachable",
			Msg: "github.com did not answer, so there is no way to tell whether " + name +
				" has a setup script in one of its repositories. Try again in a moment.",
			Verbatim: true, Err: upstream,
		}
	}
	return "", nil
}

// slicesContainsFold is the tag comparison this file needs. Tags reach the
// stores lowercased (NormalizeTags, envName), so a fold is belt and braces —
// but a hand-written sandbox_tags row, or a store whose collation differs, must
// not silently exclude an attachment from its own environment.
func slicesContainsFold(list []string, want string) bool {
	for _, s := range list {
		if strings.EqualFold(s, want) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// The guest door: metadata.EnvSetup, implemented on *Ops
// ---------------------------------------------------------------------------
//
// These two methods are the gateway half of internal/metadata's EnvSetup. They
// are on *Ops rather than on an adapter type because an adapter would hold the
// same pointer, take the same *host.Sandbox, and add a second place for the
// owner check to be forgotten. The one thing that DOES need an adapter is the
// result type — see SetupReport — and that adapter belongs in cmd/sparkbox
// beside selfLifecycleOps, because internal/metadata imports this package and
// so this package cannot import it back.

// SetupFor returns the setup script this sandbox is supposed to run, or
// ok=false when it has no job.
//
// THE OWNER TERM IS THE SECURITY BOUNDARY, and it is not redundant with the
// name. envs.Building() is the one query in that store with no owner scope — by
// design, because the reconciler acts for no person — and sandbox names are a
// single global namespace. So a row whose build_box is `web-build` matches by
// name against ANY owner's sandbox called `web-build`, and without the owner
// comparison below a stranger who created that name would be handed somebody
// else's setup script: the shape of their private toolchain, and often the
// names of their internal repositories and services.
//
// A name match with an owner mismatch is logged, because it is either an
// accident worth seeing or somebody probing, and neither should be silent.
func (o *Ops) SetupFor(ctx context.Context, b *host.Sandbox) (SetupJob, bool, error) {
	if o.envs == nil || b == nil {
		return SetupJob{}, false, nil
	}
	e, found, err := o.buildingFor(b)
	if err != nil || !found {
		return SetupJob{}, false, err
	}

	// WHICH MODE IS READ OFF THE ROW, never off the request, for the same
	// reason the environment is: nothing a guest sends names anything. The row
	// says `agent` because BuildEnvironment stamped it there before the state
	// moved, which is also what lets the reconciler tell an abandoned agent
	// build from an abandoned script build minutes later, with the builder gone
	// and only the row to go on.
	if e.SetupFrom == envs.SetupFromAgent && strings.TrimSpace(e.SetupScript) == "" {
		return SetupJob{Env: e.Name, Mode: SetupModeAgent, Payload: AgentRunner(e.Name)}, true, nil
	}

	if strings.TrimSpace(e.SetupScript) == "" {
		// A build with no script and no agent stamp cannot be started by
		// BuildEnvironment, so reaching here means the script was cleared under
		// a running build. "No job" is the honest answer; the reconciler's
		// timeout is what ends the row.
		o.log.Warn("a builder asked for a setup script its environment no longer has",
			"user", e.Owner, "env", e.Name, "sandbox", b.Name)
		return SetupJob{}, false, nil
	}
	return SetupJob{Env: e.Name, Mode: SetupModeScript, Payload: e.SetupScript}, true, nil
}

// AgentRunner is the payload an agent build ships to the guest: a shell script
// that runs an agent against this platform's dev-environment guidance and then
// RUNS WHAT THE AGENT WROTE.
//
// THE GATEWAY SHIPS THE RUNNER, NOT THE PROMPT, and that is worth the paragraph
// because it looks like an indirection for nothing. nodelink.SelfSetupResp
// gained `mode` as an omitempty field, so a node running an older build
// unmarshals a job and DROPS it — and that node's own metadata service then
// renders the hardcoded line 2 it shipped with, `script`. If the payload were a
// bare prompt, that guest would run several paragraphs of English through bash,
// backticks and all. Shipping a shell script as the payload makes the same skew
// merely DEGRADE: the old guest runs the agent correctly and only misses the
// artifact check the newer worker adds, which is a build that might over-report
// success rather than a build that executes prose.
//
// It is also less code. The guest's agent branch is the mode case and the
// artifact check; there is no prompt file to stage, chmod, and reason about
// against the privilege drop.
//
// The prompts travel inside QUOTED heredocs, so the shell expands none of them
// — no $(...), no backtick, no $VAR — and the only value interpolated into the
// script proper is the environment name, which the store has already
// constrained to [a-z0-9-]. The terminator is checked rather than assumed.
//
// EXPORTED so deploy's tests can run the real thing inside the real guest
// worker. That crossing is the point: this string is written by the gateway and
// executed by a guest, the two live in different packages, and both bugs review
// caught in the previous phase were at exactly this kind of seam. A test that
// asserts on a copy of the script would not have caught either.
//
// WHY IT REPLAYS THE SCRIPT, which is the part hardware taught rather than
// review. The first agent build on real hardware wrote a genuinely good script
// — it read the repo's own self-hosting doc and used `sparkbox whoami` instead
// of hardcoding a domain — and the build reported `ready`. The next `env
// rebuild` of that environment died on `cd: selfhost: No such file or
// directory`, a directory the agent believed existed. Nothing was wrong with
// the code: the agent does the work interactively and writes the script at the
// end FROM MEMORY, and the only thing checked was that the file was non-empty.
//
// So the failure surfaced at the worst possible moment — the first time
// somebody depended on the environment reproducing itself — and it surfaced as
// a broken rebuild rather than as a build that never claimed to work. Running
// the script here moves that discovery back into the build that produced it,
// where there is still a builder box, an agent, and nobody waiting.
//
// A REPLAY IS NOT A FRESH-CHECKOUT RUN, and it is worth being honest about the
// gap. This runs the script over a box the agent has already configured, so it
// proves the script is valid shell, that it references nothing that never
// existed, and that it is safe to run twice — which is exactly the class of
// mistake writing-from-memory produces. It cannot prove the script would work
// on a fresh disk, because a step the agent performed by hand and forgot to
// write down succeeds here by being already done. Closing THAT gap needs a
// second VM, and this is the ninety percent that needs none.
func AgentRunner(env string) string {
	prompt, repair := agentPrompt(env), repairPrompt()
	for _, p := range []*string{&prompt, &repair} {
		if strings.Contains(*p, agentPromptEOF) {
			// Unreachable with host-authored constants, and asserted anyway:
			// the day somebody makes a prompt configurable, this is what stops
			// a terminator in it from ending the heredoc early and running the
			// rest.
			*p = strings.ReplaceAll(*p, agentPromptEOF, "(redacted)")
		}
	}
	return `#!/usr/bin/env bash
# Written by the sparkbox gateway for an environment build. It runs an agent
# against this platform's dev-environment guidance and expects it to leave
# ` + SetupScriptPath + ` behind; that file, not this box, is the deliverable —
# so this script then runs it, and a file that does not run is not a build.
set -uo pipefail

# Resolved here rather than left to PATH: /etc/environment has been sourced by
# now, so PATH is whatever the owner's variables made it.
claude_bin=$(command -v claude 2>/dev/null || true)
[ -n "$claude_bin" ] || claude_bin=/usr/local/bin/claude
if [ ! -x "$claude_bin" ]; then
  echo "sparkbox: this sandbox has no agent to write a setup script with; run \` + "`" + `sparkbox update-tools\` + "`" + `" >&2
  exit 127
fi

# The interpreter the script is checked and replayed WITH, chosen the way the
# guest worker chooses the one it will run the script with on every later build
# — bash when there is one, sh on the slim template — so that what passes here
# is what runs there.
sparkbox_sh=$(command -v bash 2>/dev/null || command -v sh 2>/dev/null || echo /bin/sh)
sparkbox_setup=` + SetupScriptPath + `
sparkbox_replay=$(mktemp 2>/dev/null || echo /tmp/sparkbox-env-replay.log)

# A bound on ONE replay, not a budget for the build: the guest worker's own
# timeout is the thing that ends a run, and this only stops a setup script that
# starts a server in the foreground from eating all of it. A script that needs
# more than ten minutes to re-run over work already done is a script with a
# problem worth failing on.
sparkbox_replay_timeout=600

sparkbox_agent() {
  # --permission-mode bypassPermissions is required, not preferred: under -p the
  # ` + "`auto`" + ` mode this platform seeds is downgraded to ` + "`default`" + `, every Write and
  # Bash is denied, and the run still exits 0.
  #
  # --no-session-persistence keeps the transcript out of ~/.claude/projects. This
  # disk becomes the environment's template and is copied into every fork of it,
  # and nothing in the capture path strips that directory.
  "$claude_bin" -p "$1" \
    --permission-mode bypassPermissions \
    --output-format text \
    --no-session-persistence
}

# sparkbox_verify: is this a script, and does it run?
#
# Syntax first, because a parse error is a different sentence from a failure and
# costs nothing to separate. Then the run itself, captured to a file AND echoed:
# the log is what the owner reads on a failed build, and the file is what the
# repair pass gets to read.
sparkbox_verify() {
  if ! "$sparkbox_sh" -n "$sparkbox_setup" > "$sparkbox_replay" 2>&1; then
    echo "sparkbox: $sparkbox_setup is not valid shell:" >&2
    cat "$sparkbox_replay"
    return 1
  fi
  echo "sparkbox: running $sparkbox_setup to check that it reproduces this box" >&2
  if command -v timeout >/dev/null 2>&1; then
    timeout -k 30 "$sparkbox_replay_timeout" "$sparkbox_sh" "$sparkbox_setup" \
      </dev/null > "$sparkbox_replay" 2>&1
  else
    "$sparkbox_sh" "$sparkbox_setup" </dev/null > "$sparkbox_replay" 2>&1
  fi
  sparkbox_rc=$?
  cat "$sparkbox_replay"
  return $sparkbox_rc
}

sparkbox_agent "$(cat <<'` + agentPromptEOF + `'
` + prompt + `
` + agentPromptEOF + `
)"

if [ ! -f "$sparkbox_setup" ]; then
  # Nothing was written, so there is nothing to check. The guest worker turns
  # this into the sentence the owner reads, and saying it here as well would
  # put two different failures in one log for one thing going wrong.
  exit 0
fi

if sparkbox_verify; then
  exit 0
fi

echo "sparkbox: the script the agent wrote does not run here; asking it once to fix that" >&2
sparkbox_agent "$(cat <<'` + agentPromptEOF + `'
` + repair + `
` + agentPromptEOF + `
)

--- what happened when the script was run ---
$(tail -c 4000 "$sparkbox_replay" 2>/dev/null)"

if [ ! -f "$sparkbox_setup" ]; then
  echo "sparkbox: the repair pass left no $sparkbox_setup behind, so there is nothing to build from" >&2
  exit 3
fi

if sparkbox_verify; then
  exit 0
fi

# The LAST NON-EMPTY LINE of this log becomes the environment's recorded build
# error (summarizeBuildLog), so it is written to say the one thing that is both
# true and surprising: the box is fine, and the script is not.
echo "sparkbox: this box is configured, but $sparkbox_setup does not run in it, so no later build could reproduce it" >&2
exit 3
`
}

// agentPromptEOF is the heredoc terminator carrying a prompt. Distinctive on
// purpose: it has to be a string no prompt would contain by accident.
const agentPromptEOF = "SPARKBOX_AGENT_PROMPT_EOF"

// agentPrompt is what the agent in a builder VM is asked to do.
//
// HOST-AUTHORED, AND THAT IS THE POINT. It is the one string in an agent build
// that does not come from the owner, and it travels base64 on a line the guest
// hands to a shell — so it is a constant here rather than anything assembled
// from stored text, and the only thing interpolated into it is an environment
// name the store already constrained to [a-z0-9-].
//
// It does not restate the platform's guidance, it POINTS at it. `sparkbox docs
// dev-environment` is served to the guest by internal/guestdocs over the same
// metadata port this job arrived on, and the platform-owned ~/.agents/AGENTS.md
// is already loaded as user-scope memory in every template. Copying either one
// here would create a second copy to drift.
//
// The last two steps are the ones that matter most and are the easiest to read
// as boilerplate. The DELIVERABLE IS THE SCRIPT, not the running box: the guest
// reports .sparkbox/setup.sh back, the row records it, and the next build of
// this environment is an ordinary script build. An agent that configures a
// perfect box and writes nothing down has produced a disk nobody can reproduce
// — which is the state this whole feature exists to get out of.
//
// Step 5 tells the agent the truth about what happens next rather than leaving
// it to be discovered: the script IS run, immediately, and a script that does
// not run fails the build. Asking for idempotence without saying it will be
// checked got scripts that cloned into a directory that already existed.
func agentPrompt(env string) string {
	return `You are configuring a fresh Sparkbox microVM so this project runs in it.
Nobody is watching and nobody can answer a question, so do not ask any.

Start by running ` + "`sparkbox docs dev-environment`" + ` and follow it. It is short, and it
is this platform's own guidance for exactly this job.

Then, in this directory:

1. Work out what this project is and install what it needs — system packages
   with ` + "`sudo apt-get -y`" + ` first, then the project's own package manager.
2. Start its dev server as a ` + "`systemd --user`" + ` unit listening on 0.0.0.0, and run
   ` + "`sparkbox set-port PORT`" + ` for the port a person should open.
3. Prove it works before you consider it done: ` + "`curl -fsS http://localhost:PORT`" + `
   must succeed.
4. Write down everything you did as ` + SetupScriptPath + ` in this directory: a
   plain bash script that takes a fresh checkout to the same running state.
5. Run it — ` + "`bash " + SetupScriptPath + "`" + ` — right here, and fix it until it exits 0.

Step 4 is the deliverable and step 5 is how you find out whether it is real. You
are writing that file from memory, and the mistakes that come from doing that —
a directory you created by hand, a step in the wrong order, a path that only
ever existed in this session — are invisible until somebody rebuilds from it
months later. Running it is how you see them now.

Step 5 also means every step must be safe to repeat, because the work of steps
1 to 3 is already done by the time you run it: ` + "`mkdir -p`" + ` rather than ` + "`mkdir`" + `,
skip a clone whose directory exists, and let an already-installed package be
fine. This is checked automatically the moment you finish, and a script that
does not run cleanly here fails the build.

The disk you leave behind becomes the environment "` + env + `", and every later
build of it runs your script instead of running you — so an environment whose
box works but whose script does not is a failure, not a success. If you cannot
get the project running, still write ` + SetupScriptPath + ` with the part that does
work, make that part run cleanly, and say plainly in your final message what is
missing and why.

Do not commit, push, or open a pull request; somebody will review this file.
`
}

// repairPrompt is the second and last agent invocation of a build: the script
// the first one wrote did not run, and this asks for it to be fixed.
//
// It is a SEPARATE, FRESH agent rather than a continuation, because there is no
// session to continue — the first run is deliberately started with
// --no-session-persistence so its transcript never lands on a disk that becomes
// a template. Everything this one needs is therefore stated: the file is in the
// checkout it starts in, and the failure is appended to this text by the shell
// from the replay it just captured.
//
// ONE ROUND, not a loop. A second agent that cannot make a script run is not
// usually one round away from making it run, the build has a wall-clock budget
// shared with the run that got here, and a failed build keeps its builder
// paused — so a person can look at the box, the log and the script, which is
// more useful than a third machine-written guess.
//
// The refusal in the third paragraph is the one that matters. The cheapest way
// to make a failing script exit 0 is to stop it doing anything, and this run is
// graded by exit status alone; saying so is what stops "fix it" from being read
// as "make the error go away".
func repairPrompt() string {
	return `The setup script in this checkout was written to configure this microVM, and
it does not run. Fix it. Nobody is watching and nobody can answer a question,
so do not ask any.

Read ` + SetupScriptPath + `, work out from the output below why it failed, and
rewrite the file so that running it again here exits 0. It is being run over a
box that is ALREADY configured, so every step has to be safe to repeat:
` + "`mkdir -p`" + ` rather than ` + "`mkdir`" + `, skip a clone whose directory is already there,
and let an already-installed package be fine.

Do not make it pass by making it do less. Deleting the step that failed, or
wrapping the script in ` + "`|| true`" + `, produces a file that exits 0 and builds
nothing — and the next build of this environment runs that file instead of
running you, on a fresh checkout where the work really does need doing. If a
step genuinely cannot work here, keep it and say so in your final message.

Do not commit, push, or open a pull request.
`
}

// SetupDone records what the guest reported and RETURNS. The work it triggers —
// capture, bind, destroy — runs on its own goroutine, because the caller is the
// guest that the capture is about to pause, and a response written after that
// reaches a kernel which is not running.
func (o *Ops) SetupDone(ctx context.Context, b *host.Sandbox, r SetupReport) error {
	const op = "env.build.result"
	if o.envs == nil {
		return Disabled(op, envDisabledSentence)
	}
	if b == nil {
		return Invalid(op, "missing_sandbox", "a sandbox is required")
	}
	e, found, err := o.buildingFor(b)
	if err != nil {
		return Fail(op, err)
	}
	if !found {
		// Not this host's business: a box that is not the builder of anything
		// reported a result. It is answered with a no-op rather than an error
		// because the guest cannot act on either, and the metadata layer has
		// already written its acceptance by the time this runs.
		o.log.Warn("a sandbox reported an environment setup result with no build to land on",
			"sandbox", b.Name, "owner", b.Owner, "ok", r.OK, "exit", r.ExitCode)
		return nil
	}

	owner, name, box := e.Owner, e.Name, b.Name
	summary := summarizeBuildLog(r.Log)
	o.log.Info("environment setup reported", "user", owner, "env", name, "sandbox", box,
		"ok", r.OK, "exit", r.ExitCode, "script_bytes", len(r.Script), "log", summary)

	// Detached and budgeted, for the reason the metadata layer acks first: this
	// goroutine outlives the request by minutes, and its first act pauses the
	// VM the request came from.
	work, cancel := context.WithTimeout(context.WithoutCancel(ctx), o.buildTimeout())
	o.envBuildsWG.Add(1)
	go func() {
		defer cancel()
		defer o.envBuildsWG.Done()
		o.envBuilds.Do(envBuildKey(owner, name), func() (any, error) { //nolint:errcheck
			o.completeBuild(work, e, box, r, summary)
			return nil, nil
		})
	}()
	return nil
}

// completeBuild is the whole of a reported build's second half.
func (o *Ops) completeBuild(ctx context.Context, e envs.Environment, box string, r SetupReport, summary string) {
	owner, name := e.Owner, e.Name

	// RECORDED BEFORE THE OUTCOME IS DECIDED, and that ordering is the whole of
	// this paragraph. The guest reports the .sparkbox/setup.sh the run ENDED
	// with whether the run succeeded or not, and that file is the record of
	// what happened rather than a reward for succeeding.
	//
	// It matters most in agent mode, where losing it is silently expensive: an
	// agent can write the script and still fail — its own timeout, a failed
	// `curl`, a dev server that would not start — and the row's message then
	// tells the owner to finish by hand with `env capture`. Before this, that
	// capture bound the disk while the row still had NO script, so the
	// environment stayed in agent mode and every later build ran another agent
	// to write a file that was already sitting in the checkout.
	o.recordReportedScript(e, r)

	if !r.OK {
		// THE BUILDER IS KEPT, AND PAUSED. It holds the half-built filesystem,
		// the log, and the checkout the script failed in — which is everything
		// a person needs to finish the job by hand — and it is the reason
		// `env capture` exists. Destroying it would throw all of that away at
		// the exact moment somebody wants it.
		// WHAT EXITED, named for what it was. In script mode the thing that
		// exited IS the setup script. In agent mode it is the runner around an
		// agent, and telling somebody whose agent never wrote a script that
		// "the setup script exited 3" sends them looking for a file that does
		// not exist. The predicate is SetupFor's, and e is the row as the build
		// started, so recordReportedScript above cannot have changed the
		// answer.
		what := "the setup script"
		if e.SetupFrom == envs.SetupFromAgent && strings.TrimSpace(e.SetupScript) == "" {
			what = "the agent run"
		}
		reason := fmt.Sprintf("%s exited %d", what, r.ExitCode)
		if summary != "" {
			reason += ": " + summary
		}
		o.failBuild(ctx, owner, name, box, reason)
		return
	}

	if err := o.captureBuild(ctx, owner, name, box); err != nil {
		o.failBuild(ctx, owner, name, box,
			"the setup script succeeded but the snapshot could not be taken: "+AsError("env.build", err).Msg)
	}
}

// recordReportedScript stores the script the guest says it ran, when there is
// one and it is usable.
//
// setup_from keeps whatever it already said, falling back to `repo`: a script
// the guest hands back came out of the checkout, and the origin column's job is
// to say how a LATER build should decide, not to re-litigate this one. It is a
// warning and never a failure — the disk is what the build is for, and refusing
// to capture a good build because the record of its script would not write is
// the wrong trade.
func (o *Ops) recordReportedScript(e envs.Environment, r SetupReport) {
	if strings.TrimSpace(r.Script) == "" {
		return
	}
	if len(r.Script) > MaxSetupScript {
		o.log.Warn("a builder reported a setup script too large to store",
			"user", e.Owner, "env", e.Name, "bytes", len(r.Script), "max", MaxSetupScript)
		return
	}
	if r.Script == e.SetupScript {
		return
	}
	from := e.SetupFrom
	if from == "" {
		from = envs.SetupFromRepo
	}
	if err := o.envs.SetScript(e.Owner, e.Name, r.Script, from); err != nil {
		o.log.Warn("could not record the setup script a builder reported",
			"user", e.Owner, "env", e.Name, "err", err)
	}
}

// ---------------------------------------------------------------------------
// capture
// ---------------------------------------------------------------------------

// CaptureEnvironment finishes a build by hand: take the environment's builder
// exactly as it stands, capture it, bind the capture to the tag, destroy the
// builder and mark the environment ready.
//
// This is the recovery path, and it is why a failed build leaves its builder
// paused instead of destroying it. Somebody ssh's into `web-build`, finds the
// missing apt package, installs it, and runs this — and the environment is
// built, from the disk they just fixed, with no second run of a script that was
// never going to work.
//
// It is synchronous, like `snapshot create`, because a capture is minutes and
// the person who typed it is watching.
func (o *Ops) CaptureEnvironment(ctx context.Context, c Caller, name string) (EnvironmentInfo, error) {
	const op = "env.capture"
	if o.envs == nil {
		return EnvironmentInfo{}, Disabled(op, envDisabledSentence)
	}
	name, err := envName(op, name)
	if err != nil {
		return EnvironmentInfo{}, err
	}
	if err := o.envBuildable(op); err != nil {
		return EnvironmentInfo{}, err
	}
	e, err := o.envs.Get(c.Handle, name)
	if err != nil {
		return EnvironmentInfo{}, envStoreError(op, name, err)
	}
	if e.BuildBox == "" {
		return EnvironmentInfo{}, &Error{
			Kind: KindConflict, Op: op, Code: "no_build_box",
			Msg: "environment " + name + " has no builder sandbox, so there is nothing to capture.",
			Hint: "Start one with `env build " + name + "`, or point the tag at an existing snapshot with " +
				"`snapshot bind <snapshot> --tag " + name + "`.",
			Details:  map[string]any{"environment": name},
			Verbatim: true,
		}
	}
	// The owner gate, so a builder recorded on this row that some other owner
	// now holds the name of is refused here rather than captured.
	if _, err := o.owned(op, e.BuildBox, c); err != nil {
		return EnvironmentInfo{}, &Error{
			Kind: KindConflict, Op: op, Code: "build_box_gone",
			Msg:      "environment " + name + "'s builder sandbox " + e.BuildBox + " is gone, so there is nothing to capture.",
			Hint:     "Start a fresh build with `env build " + name + "`.",
			Details:  map[string]any{"environment": name, "sandbox": e.BuildBox},
			Verbatim: true,
		}
	}

	box := e.BuildBox
	// The same key a build uses, so a manual capture and a guest's late result
	// cannot both try to snapshot the same box.
	shared, cancel := context.WithTimeout(context.WithoutCancel(ctx), o.buildTimeout())
	defer cancel()
	_, err, _ = o.envBuilds.Do(envBuildKey(c.Handle, name), func() (any, error) {
		if err := o.captureBuild(shared, c.Handle, name, box); err != nil {
			// Recorded from INSIDE the flight, on the detached context: the
			// caller's may be cancelled by now, and a failure that could not be
			// written down leaves a row saying `failed` with no reason on it.
			o.failBuild(shared, c.Handle, name, box,
				"the snapshot could not be taken: "+AsError(op, err).Msg)
			return nil, err
		}
		return nil, nil
	})
	if err != nil {
		return EnvironmentInfo{}, AsError(op, err)
	}
	return o.environmentInfo(c.Handle, name)
}

// captureBuild is the expensive, unrepeatable half, shared by the reported
// build and the manual capture: capture, bind, destroy, ready.
//
// SnapshotToTag is called rather than reimplemented, and that is most of the
// value here. It strips the managed secret block before it packs (every fork
// copies the rootfs byte-for-byte, so that block cannot ride along), refreshes
// the agent CLIs, pauses the guest so the filesystem is flushed, captures, and
// binds — in that order, capture first, because binding first would leave the
// tag pointing at an image that does not exist and every create on it in that
// window would resolve to a missing rootfs.
//
// IF THE CAPTURE AND BIND SUCCEED, THE ENVIRONMENT IS READY EVEN IF THE DESTROY
// FAILS. The expensive half is done, the disk exists and the tag points at it,
// and reporting that as a failure would send somebody to rebuild an environment
// that is already finished — paying the whole cost again to reach the state
// they are already in. What a failed destroy leaves behind is one sandbox,
// which is visible in `list` and removable with one command, and it is logged
// with that command in it.
func (o *Ops) captureBuild(ctx context.Context, owner, name, box string) error {
	c := Caller{Handle: owner}
	snap := snapshotNameFor(name, o.now())
	if _, err := o.SnapshotToTag(ctx, c, SnapshotToTagArgs{
		Sandbox: box, Name: snap, Tag: name,
	}); err != nil {
		return err
	}
	if err := o.boxes.Destroy(ctx, box); err != nil {
		o.log.Error("could not destroy an environment's builder after a successful build",
			"user", owner, "env", name, "sandbox", box, "snapshot", snap, "err", err,
			"next", "the environment is ready; remove the leftover box with `rm "+box+"`")
	}
	// build_box is cleared because the build is over: the column names the
	// builder that exists RIGHT NOW, and a ready environment has none. When the
	// destroy above failed the box outlives the column, which is why that log
	// line carries its name.
	if err := o.envs.SetState(owner, name, envs.StateReady, "", ""); err != nil {
		o.log.Error("could not mark an environment ready after capturing it",
			"user", owner, "env", name, "snapshot", snap, "err", err)
		return err
	}
	o.log.Info("environment built", "user", owner, "env", name, "sandbox", box, "snapshot", snap)
	return nil
}

// ---------------------------------------------------------------------------
// The reconciler
// ---------------------------------------------------------------------------

// ReconcileEnvironmentBuilds settles the builds that nobody is waiting on any
// more. It is meant to run once at startup and then on a slow ticker.
//
// A gateway restart in the middle of a build leaves a row saying `building`, a
// build_box naming a sandbox that may or may not exist, and an owner's
// decrypted secrets inside it. Nothing in this process can tell whether that
// guest's oneshot completed, half-completed, or is running still, so resuming
// is not on the table — and the environment row is the only durable record
// there is, which is exactly why the build state does not live in the job
// registry.
//
// It walks EVERY owner's rows, through the one store query that is not
// owner-scoped, because the caller is the process itself and acts for no
// person. That is also why nothing here is rendered to anybody: the outcome is
// a row and a log line.
//
// Three cases, and the third is the one that matters most:
//
//   - the builder is gone -> failed. Nothing can finish a build whose box does
//     not exist, and leaving the row in `building` would refuse every
//     `create --env` on it forever.
//   - the build is older than the budget -> failed, builder LEFT PAUSED. The
//     guest may have died mid-script; the disk is still the best evidence and
//     `env capture` is still the way out.
//   - anything else -> LEFT ALONE. This is the case a naive sweep gets wrong:
//     a build that is two minutes into a twenty-minute `cargo build` looks
//     exactly like a stranded one from out here, and failing it would destroy
//     work that was going to succeed. The result POST may still land.
func (o *Ops) ReconcileEnvironmentBuilds(ctx context.Context) {
	if o.envs == nil {
		return
	}
	list, err := o.envs.Building()
	if err != nil {
		o.log.Error("could not read the environments still building", "err", err)
		return
	}
	cutoff := o.buildTimeout()
	for _, e := range list {
		switch {
		case e.BuildBox == "":
			o.markFailed(e.Owner, e.Name, "",
				"the build recorded no builder sandbox, so nothing could have finished it")
		case !o.builderAlive(e):
			o.markFailed(e.Owner, e.Name, "",
				"its builder sandbox "+e.BuildBox+" is gone, so the build could not finish")
		case o.now().Sub(buildStartedAt(e)) > cutoff:
			o.expireBuild(ctx, e, cutoff)
		default:
			// Still plausibly running. Say nothing and look again next sweep.
		}
	}
}

// expireBuild ends a build that overran its budget, and it is the ONE place the
// two modes are treated differently after they start.
//
// A SCRIPT build's builder is PAUSED and kept. It holds a half-built filesystem,
// a log and the checkout the script was working in — everything somebody needs
// to finish the job by hand and keep it with `env capture`. Throwing that away
// at the moment it becomes useful is the trade this whole feature refuses.
//
// An AGENT build's builder is DESTROYED. The reason is not tidiness and it is
// not the disk: an overrun agent build is, by definition, a build whose guest
// never reported — so the most likely state of that VM is an agent still
// RUNNING in it, with a shell, network egress, and /etc/environment full of the
// owner's decrypted credentials, with nobody watching. Pausing would leave it
// resumable by the next ssh; keeping it running is worse. There is also much
// less to lose: an agent build that overran has, by construction, not written
// the script that was its whole deliverable.
//
// The box is cleared from the row ONLY IF THE DESTROY SUCCEEDED. A row that
// names no builder tells the reader nothing is left to look at, so saying that
// about a VM which is in fact still sitting there would be the one lie this
// path must not tell — and the sentence would drop the name they need to go and
// remove it by hand.
func (o *Ops) expireBuild(ctx context.Context, e envs.Environment, cutoff time.Duration) {
	reason := "the build ran longer than " + cutoff.String() + " without reporting a result"
	if e.SetupFrom != envs.SetupFromAgent || strings.TrimSpace(e.SetupScript) != "" {
		o.failBuild(ctx, e.Owner, e.Name, e.BuildBox, reason)
		return
	}
	reason = "the agent writing its setup script ran longer than " + cutoff.String() +
		" without reporting a result, so the builder was destroyed rather than left running unattended"
	if o.boxes == nil {
		o.markFailedBox(e.Owner, e.Name, e.BuildBox, reason, false)
		return
	}
	// PauseTimeout and NOT the build budget. ReconcileEnvironmentBuilds walks
	// every owner's building rows on ONE goroutine on a ten-minute ticker, so a
	// destroy that blocks blocks the whole sweep — and with the build budget
	// (45 minutes by default) a single unreachable node would stall every other
	// owner's reconciliation for longer than the interval between sweeps. Every
	// other Destroy in this package is bounded the same way.
	kill, cancel := context.WithTimeout(context.WithoutCancel(ctx), PauseTimeout)
	defer cancel()
	if err := o.boxes.Destroy(kill, e.BuildBox); err != nil {
		o.log.Error("could not destroy an overrun agent builder",
			"user", e.Owner, "env", e.Name, "sandbox", e.BuildBox, "err", err)
		o.markFailedBox(e.Owner, e.Name, e.BuildBox, reason+" — and it could not be stopped", false)
		return
	}
	o.log.Info("destroyed an overrun agent builder",
		"user", e.Owner, "env", e.Name, "sandbox", e.BuildBox)
	o.markFailed(e.Owner, e.Name, "", reason)
}

// builderAlive reports whether the row's builder is a sandbox this owner still
// has. The owner comparison is the same one SetupFor makes and for the same
// reason: the name is global, so a box someone ELSE now holds under that name
// is not this environment's builder and must not be paused, captured or
// counted as alive.
func (o *Ops) builderAlive(e envs.Environment) bool {
	if o.boxes == nil {
		return false
	}
	b, ok := o.boxes.Get(e.BuildBox)
	return ok && b.Owner == e.Owner
}

// buildStartedAt is when the clock on this build started. UpdatedAt is the
// right column: SetState stamps it when the row moves to `building`, and
// nothing else writes the row while a build is in flight. BuiltAt deliberately
// is not — it means "when the bound snapshot was captured", so an environment
// rebuilt today still carries March, and using it would time out a fresh build
// instantly on any environment that had ever succeeded.
func buildStartedAt(e envs.Environment) time.Time {
	if !e.UpdatedAt.IsZero() {
		return e.UpdatedAt
	}
	return e.CreatedAt
}

// ---------------------------------------------------------------------------
// Shared pieces
// ---------------------------------------------------------------------------

// buildingFor resolves the sandbox to the environment it is building, or
// reports that it has no job. See SetupFor for why the owner comparison is a
// security boundary and not a tidiness check.
func (o *Ops) buildingFor(b *host.Sandbox) (envs.Environment, bool, error) {
	list, err := o.envs.Building()
	if err != nil {
		return envs.Environment{}, false, err
	}
	for _, e := range list {
		if e.BuildBox != b.Name {
			continue
		}
		if e.Owner != b.Owner {
			o.log.Warn("a sandbox matched a builder name it does not own",
				"sandbox", b.Name, "sandbox_owner", b.Owner, "env", e.Name, "env_owner", e.Owner)
			continue
		}
		return e, true, nil
	}
	return envs.Environment{}, false, nil
}

// failBuild records a failure and PAUSES the builder, leaving the box in place.
//
// Pausing rather than destroying is the whole recovery story: the box holds the
// half-built filesystem and the log, `env capture` can finish from it, and a
// paused box costs disk rather than RAM. Pausing rather than leaving it running
// is the other half — an unattended VM that nobody is waiting on should not
// hold a running slot, and the idle reaper would pause it shortly anyway, less
// predictably.
//
// The pause is best effort and never changes the outcome: a build that failed
// has failed, and reporting a pause error instead would replace a sentence
// somebody can act on with one they cannot.
func (o *Ops) failBuild(ctx context.Context, owner, name, box, reason string) {
	if box != "" && o.boxes != nil {
		pctx, cancel := withBudget(ctx, PauseTimeout)
		if err := o.boxes.Pause(pctx, box); err != nil {
			o.log.Warn("could not pause a failed environment builder",
				"user", owner, "env", name, "sandbox", box, "err", err)
		}
		cancel()
	}
	o.markFailed(owner, name, box, reason)
}

// markFailed writes the failure onto the row, with the sentence that tells
// somebody what to do next.
//
// The box name is IN the message and not only in the column, because build_box
// is cleared when there is no box to name and because the message is what
// `create --env` prints back at whoever tries to boot the environment later
// (see resolveEnvTag). A failure a person cannot locate is a failure they
// cannot fix.
func (o *Ops) markFailed(owner, name, box, reason string) {
	o.markFailedBox(owner, name, box, reason, true)
}

// markFailedBox is markFailed with the one thing the sentence assumes made
// explicit: whether that builder is actually PAUSED and finishable.
//
// It exists because expireBuild has a case where it is not. An agent builder
// that overran and could not be destroyed is still there, was never paused, and
// offering `env capture` on it would offer to snapshot a VM with an agent
// possibly still writing to its filesystem. The name has to stay on the row —
// dropping it would tell somebody nothing is left when something is — so the
// name is kept and the sentence changes.
func (o *Ops) markFailedBox(owner, name, box, reason string, paused bool) {
	msg := reason
	switch {
	case box != "" && paused:
		msg += ". The builder " + box + " is paused — `" + o.sshHint(box) +
			"` to look, then `env capture " + name + "` to finish it, or `rm " + box + "` to start over"
	case box != "":
		msg += ". The builder " + box + " could not be stopped and may still be running — remove it with `rm " +
			box + "`"
	}
	if err := o.envs.SetState(owner, name, envs.StateFailed, box, msg); err != nil {
		o.log.Error("could not record an environment build failure",
			"user", owner, "env", name, "sandbox", box, "err", err)
		return
	}
	o.log.Warn("environment build failed", "user", owner, "env", name, "sandbox", box, "reason", reason)
}

// environmentInfo re-reads a row and composes it, so a build returns what is
// now true rather than what the caller was told to expect.
func (o *Ops) environmentInfo(owner, name string) (EnvironmentInfo, error) {
	e, err := o.envs.Get(owner, name)
	if err != nil {
		return EnvironmentInfo{}, envStoreError("env.build", name, err)
	}
	return o.composition(owner).info(e), nil
}

// summarizeBuildLog turns a guest-authored log tail into one short, safe line.
//
// EVERY BYTE OF THIS IS UNTRUSTED. It is written by a script running in
// somebody's sandbox, it lands in a database column, and it is printed to a
// terminal by three different surfaces — so it is reduced to the last line that
// said anything, stripped of control characters (an ANSI escape in a refusal is
// a terminal a stranger can drive), collapsed, and cut to a sentence. The full
// tail is not lost: it is in the builder, which is left paused so it can be
// read there, and it is in the host log.
func summarizeBuildLog(log string) string {
	line := ""
	for _, l := range strings.Split(log, "\n") {
		if strings.TrimSpace(l) != "" {
			line = l
		}
	}
	var b strings.Builder
	prevSpace := false
	n := 0
	for _, r := range strings.TrimSpace(line) {
		if r == unicode.ReplacementChar || r < 0x20 || r == 0x7f || unicode.IsSpace(r) {
			// One space stands in for any run of whitespace or control bytes,
			// which also collapses a partial UTF-8 rune at the guest's cut.
			if !prevSpace && n > 0 {
				b.WriteByte(' ')
				prevSpace = true
				n++
			}
			continue
		}
		if n >= maxBuildErrorRunes {
			b.WriteString("…")
			break
		}
		b.WriteRune(r)
		prevSpace = false
		n++
	}
	return strings.TrimSpace(b.String())
}
