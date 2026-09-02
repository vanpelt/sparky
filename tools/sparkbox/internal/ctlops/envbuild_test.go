package ctlops

// What these pin is the half of `env build` that no store can decide: the order
// the refusals run in, the two places an owner term is a security boundary
// rather than a query filter, and the four half-failures a build can end in —
// each of which leaves a different combination of row, snapshot, binding and
// box behind.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"slices"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/envs"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/ghapp"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/repos"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/secrets"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
)

// The real App must keep satisfying the seed read structurally, for the reason
// the block in fakes_test.go states: a signature drift should fail this
// package's tests rather than the integrator's build. There is deliberately no
// twin assertion for SetupStarter — the one implementation is
// *envsync.Syncer, and envsync imports sshgw which imports this package, so a
// test here that named it would be an import cycle.
var _ RepoFileReader = (*ghapp.App)(nil)

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

// fakeRepoFiles is the seed read in a map. It answers ghapp's own sentinels
// rather than look-alikes, because seedSetupScript branches on all three with
// errors.Is and a fake that matched by message would let those branches rot.
type fakeRepoFiles struct {
	c     *calls
	files map[string]string // "owner/name@ref:path"
	errs  map[string]error  // same key -> the failure instead
	// asked records every (slug, ref, path) in order, so a test can prove the
	// walk stopped at the first hit rather than reading every attachment.
	asked []string
}

func repoFileKey(owner, name, ref, path string) string {
	return strings.ToLower(owner+"/"+name) + "@" + ref + ":" + path
}

func (f *fakeRepoFiles) ReadFile(_ context.Context, _ ghapp.Installation, owner, name, ref, path string) ([]byte, error) {
	k := repoFileKey(owner, name, ref, path)
	f.c.add("files.ReadFile %s", k)
	f.asked = append(f.asked, k)
	if err, ok := f.errs[k]; ok {
		return nil, err
	}
	body, ok := f.files[k]
	if !ok {
		return nil, ghapp.ErrNoSuchFile
	}
	return []byte(body), nil
}

// fakeSetupStarter is the guest nudge. It records the sandbox it was pointed
// at, which is the only thing worth pinning here: the nudge carries nothing
// else, on purpose.
type fakeSetupStarter struct {
	c       *calls
	err     error
	started []string
}

func (f *fakeSetupStarter) StartSetup(_ context.Context, box *host.Sandbox) error {
	f.c.add("StartSetup %s", box.Name)
	if f.err != nil {
		return f.err
	}
	f.started = append(f.started, box.Name)
	return nil
}

// buildRig is a rig with everything a build needs: the environment stores, the
// repo stores, a linked GitHub account, the seed reader and the nudge.
type buildRig struct {
	*rig
	envs    *fakeEnvs
	repos   *fakeRepos
	app     *fakeApp
	files   *fakeRepoFiles
	starter *fakeSetupStarter
}

func newBuildRig(t *testing.T) *buildRig {
	t.Helper()
	r := newRig(t)
	e, _, _ := withEnvs(r)
	rp, app := withRepos(r)
	linkGitHub(r, "alice", "alice-gh", "key", 1)
	linkGitHub(r, "mallory", "mallory-gh", "key", 2)
	files := &fakeRepoFiles{c: r.calls, files: map[string]string{}, errs: map[string]error{}}
	starter := &fakeSetupStarter{c: r.calls}
	r.ops.repoFiles, r.ops.setupStarter = files, starter
	return &buildRig{rig: r, envs: e, repos: rp, app: app, files: files, starter: starter}
}

// env seeds an environment row directly, without going through Ops, so a test
// can set up the state a build starts from without recording calls it would
// then have to filter out of its own assertions.
func (b *buildRig) env(owner, name string, mutate ...func(*envs.Environment)) envs.Environment {
	now := time.Unix(0, 0).UTC()
	e := envs.Environment{
		Owner: owner, Name: name, State: envs.StateDraft,
		CreatedAt: now, UpdatedAt: now,
	}
	for _, m := range mutate {
		m(&e)
	}
	b.envs.rows[envKey(owner, name)] = e
	return e
}

// attach seeds a repo attachment carrying the environment's tag.
func (b *buildRig) attach(owner, slug, ref string, tags ...string) {
	k := repoKey(owner, "github.com", slug)
	b.repos.rows[k] = repos.Repo{
		Owner: owner, Host: "github.com", Slug: slug, Ref: ref,
		Access: repos.AccessRead, Tags: tags, CreatedAt: time.Unix(0, 0).UTC(),
	}
	b.repos.tags[k] = tags
	// The App has to be installed on it or the seed read never gets as far as
	// the file. The fixture installs wandb/hivemind; everything else is added
	// here.
	b.app.installed[strings.ToLower(slug)] = ghapp.Installation{ID: 99, AccountLogin: owner}
}

// file publishes a file the seed read can find.
func (b *buildRig) file(slug, ref, path, body string) {
	owner, name, _ := repos.SplitSlug(slug)
	b.files.files[repoFileKey(owner, name, ref, path)] = body
}

// row is the environment as the store now holds it.
func (b *buildRig) row(t *testing.T, owner, name string) envs.Environment {
	t.Helper()
	e, ok := b.envs.rows[envKey(owner, name)]
	if !ok {
		t.Fatalf("no environment row for %s/%s", owner, name)
	}
	return e
}

const setupScript = "#!/bin/sh\nset -eu\nnpm ci\n"

// ---------------------------------------------------------------------------
// Refusals
// ---------------------------------------------------------------------------

// TestBuildRefusesBeforeTheFirstWrite is the ordering rule stated as a test.
// Every case below must reach NO mutation at all — no state change, no create,
// no tag row — because there is no transaction spanning the three stores a
// build touches, so the order of the checks is the only thing that makes "it
// failed" mean "nothing moved".
func TestBuildRefusesBeforeTheFirstWrite(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*buildRig)
		kind  Kind
		code  string
		// want is a phrase the refusal must contain, because for half of these
		// the sentence IS the feature.
		want []string
	}{
		{
			name:  "environments not enabled",
			setup: func(b *buildRig) { b.ops.envs = nil },
			kind:  KindDisabled, code: "env_disabled",
			want: []string{"environments are not enabled"},
		},
		{
			name: "driver cannot snapshot",
			setup: func(b *buildRig) {
				b.env("alice", "web", withScript(setupScript))
				b.tmpl.on = false
			},
			kind: KindDisabled, code: "env_disabled",
			want: []string{"snapshots are not supported"},
		},
		{
			name: "no template bindings",
			setup: func(b *buildRig) {
				b.env("alice", "web", withScript(setupScript))
				b.ops.templateTags = nil
			},
			kind: KindDisabled, code: "env_disabled",
			want: []string{"template bindings are not enabled"},
		},
		{
			name: "no tag store",
			setup: func(b *buildRig) {
				b.env("alice", "web", withScript(setupScript))
				b.ops.tags = nil
			},
			kind: KindDisabled, code: "env_disabled",
			want: []string{"tagging is not enabled"},
		},
		{
			name:  "no such environment",
			setup: func(b *buildRig) {},
			kind:  KindNotFound, code: "environment_not_found",
			want: []string{"web"},
		},
		{
			// The masked answer: alice asking for mallory's environment must
			// get the byte-identical refusal she gets for one nobody has.
			name:  "somebody else's environment",
			setup: func(b *buildRig) { b.env("mallory", "web", withScript(setupScript)) },
			kind:  KindNotFound, code: "environment_not_found",
			want: []string{"web"},
		},
		{
			name: "already building",
			setup: func(b *buildRig) {
				b.env("alice", "web", withScript(setupScript), func(e *envs.Environment) {
					e.State, e.BuildBox = envs.StateBuilding, "web-build"
				})
			},
			kind: KindConflict, code: "env_already_building",
			want: []string{"already building", "web-build"},
		},
		{
			name: "the builder name is taken",
			setup: func(b *buildRig) {
				b.env("alice", "web", withScript(setupScript))
				b.boxes.boxes["web-build"] = &host.Sandbox{
					Name: "web-build", Owner: "alice", State: vmm.StateRunning,
				}
			},
			kind: KindConflict, code: "build_box_exists",
			want: []string{"web-build", "env capture web", "rm web-build"},
		},
		{
			// No script anywhere is the AGENT path, so what a person hits here
			// is the agent's own precondition — and hitting it must cost
			// nothing. Without this gate the builder boots, `claude -p` fails
			// to authenticate, and the real cause arrives as a 401 in a log
			// tail forty-five minutes' worth of budget later.
			name:  "no script anywhere and no agent credential",
			setup: func(b *buildRig) { b.env("alice", "web") },
			kind:  KindConflict, code: "env_no_agent_credential",
			want: []string{
				AgentCredential, "secret set " + AgentCredential,
				".sparkbox/setup.sh",
			},
		},
		{
			// The credential EXISTS but is tagged where no builder for this
			// environment will carry it. That is a different problem from not
			// having one and the repair is a retag, so it gets its own
			// sentence — and the sentence has to name the tags it actually has,
			// or the reader cannot tell why a token they can see in `secret ls`
			// is not reaching anything.
			name: "an agent credential tagged out of the builder's reach",
			setup: func(b *buildRig) {
				b.env("alice", "web")
				b.secret("alice", AgentCredential, "ci")
			},
			kind: KindConflict, code: "env_no_agent_credential",
			want: []string{AgentCredential, "ci", "--tag web"},
		},
		{
			// An attachment whose repository has no such file is not a script,
			// and neither is one that only has whitespace in it — so this falls
			// through to the agent path and its gate, exactly as an environment
			// with no repository at all does.
			name: "an attached repo with an empty setup script",
			setup: func(b *buildRig) {
				b.env("alice", "web")
				b.attach("alice", "wandb/hivemind", "main", "web")
				b.file("wandb/hivemind", "main", SetupScriptPath, "   \n\n")
			},
			kind: KindConflict, code: "env_no_agent_credential",
			want: []string{AgentCredential},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := newBuildRig(t)
			tc.setup(b)
			b.calls.reset()

			_, err := b.ops.BuildEnvironment(context.Background(), alice(), "web")
			var e *Error
			if !errors.As(err, &e) {
				t.Fatalf("BuildEnvironment = %v, want a typed error", err)
			}
			if e.Kind != tc.kind || e.Code != tc.code {
				t.Errorf("kind/code = %v/%s, want %v/%s", e.Kind, e.Code, tc.kind, tc.code)
			}
			whole := e.Msg + " " + e.Hint
			for _, want := range tc.want {
				if !strings.Contains(whole, want) {
					t.Errorf("refusal does not mention %q: %q", want, whole)
				}
			}
			// The point of the whole table.
			if got := b.calls.mutating(); len(got) > 0 {
				t.Errorf("a refused build mutated something: %v", got)
			}
		})
	}
}

// secret seeds a credential the owner holds, on the tags given. It writes the
// fake's tag map directly rather than going through PutSecret so a test can
// describe a token that is tagged out of reach without also asserting on the
// call that put it there.
func (b *buildRig) secret(owner, name string, tags ...string) {
	if len(tags) == 0 {
		tags = []string{secrets.DefaultTag}
	}
	b.rig.secrets.tags[secretKey(owner, name)] = tags
}

// TestAnEnvironmentWithNoScriptBuildsWithAnAgent is the Phase C path end to end
// on the gateway side, and it pins the two things that are easy to get subtly
// wrong.
//
// The first is that the row must SAY it is an agent build before the state
// moves. Two readers need that later with nothing else to go on: SetupFor,
// answering a guest that boots minutes from now, and the reconciler, deciding
// whether an expired builder is a paused disk to finish by hand or an
// unattended agent to destroy. Nothing in the guest's report carries the mode.
//
// The second is that the guest is handed a PROMPT and not a script, under the
// agent mode — a job that arrived as mode=script with a prompt in the payload
// would be run as a shell script, which is a sentence of English executed by
// bash.
func TestAnEnvironmentWithNoScriptBuildsWithAnAgent(t *testing.T) {
	b := newBuildRig(t)
	b.env("alice", "web")
	b.secret("alice", AgentCredential) // on `default`, which every builder carries

	if _, err := b.ops.BuildEnvironment(context.Background(), alice(), "web"); err != nil {
		t.Fatalf("an agent build was refused: %v", err)
	}

	row := b.envs.rows[envKey("alice", "web")]
	if row.State != envs.StateBuilding {
		t.Errorf("state = %q, want building", row.State)
	}
	if row.SetupFrom != envs.SetupFromAgent {
		t.Errorf("setup_from = %q, want %q — nothing downstream can tell this is an agent build without it",
			row.SetupFrom, envs.SetupFromAgent)
	}
	if row.SetupScript != "" {
		t.Errorf("an agent build started with a script already on the row: %q", row.SetupScript)
	}
	if !slices.Contains(b.starter.started, "web-build") {
		t.Errorf("the builder was never nudged: %v", b.starter.started)
	}

	// What the guest will actually be handed.
	box, ok := b.boxes.Get("web-build")
	if !ok {
		t.Fatal("no builder sandbox was created")
	}
	job, ok, err := b.ops.SetupFor(context.Background(), box)
	if err != nil || !ok {
		t.Fatalf("SetupFor on the agent builder = %+v/%v/%v", job, ok, err)
	}
	if job.Mode != SetupModeAgent {
		t.Fatalf("mode = %q, want %q", job.Mode, SetupModeAgent)
	}
	if job.Env != "web" {
		t.Errorf("env = %q, want web", job.Env)
	}
	// The prompt has to send the agent at the platform's own guidance rather
	// than restate it, and has to end with the file that is the deliverable.
	for _, want := range []string{"sparkbox docs dev-environment", SetupScriptPath, "web"} {
		if !strings.Contains(job.Payload, want) {
			t.Errorf("the agent prompt never mentions %q:\n%s", want, job.Payload)
		}
	}
	// Nobody is there to answer a question, and a prompt that invites one buys
	// a builder that sits until the budget expires.
	if !strings.Contains(job.Payload, "do not ask") && !strings.Contains(job.Payload, "Do not ask") {
		t.Errorf("the agent prompt does not tell the agent nobody can answer it:\n%s", job.Payload)
	}
}

// TestAnApiKeyIsAlsoAnAgentCredential.
//
// docs/onboarding-users.md documents ANTHROPIC_API_KEY in the same breath as
// the OAuth token ("works too if you bill by API"), so a gate that knew only
// one name would refuse an agent build for somebody who is perfectly well
// authenticated — a FALSE refusal in a check whose entire justification is that
// it saves people from a false failure.
func TestAnApiKeyIsAlsoAnAgentCredential(t *testing.T) {
	for _, name := range AgentCredentials {
		t.Run(name, func(t *testing.T) {
			b := newBuildRig(t)
			b.env("alice", "web")
			b.secret("alice", name)
			if _, err := b.ops.BuildEnvironment(context.Background(), alice(), "web"); err != nil {
				t.Fatalf("an agent build was refused with %s on `default`: %v", name, err)
			}
		})
	}
}

// TestAFailedBuildStillRecordsTheScriptTheRunWrote.
//
// The guest reports the .sparkbox/setup.sh the run ENDED with whether it
// succeeded or not, and that file is the record of what happened rather than a
// reward for succeeding. Losing it is silently expensive in agent mode: an
// agent can write the script and still fail — its own timeout, a dev server
// that would not start — and the failure message then tells the owner to finish
// by hand with `env capture`. If the script were dropped, that capture would
// bind the disk with NO script on the row, leaving the environment in agent
// mode so every later build ran another agent to write a file already sitting
// in the checkout.
func TestAFailedBuildStillRecordsTheScriptTheRunWrote(t *testing.T) {
	b := newBuildRig(t)
	b.building("alice", "web", "web-build", func(e *envs.Environment) {
		e.SetupFrom, e.SetupScript = envs.SetupFromAgent, ""
	})
	box, _ := b.boxes.Get("web-build")

	const written = "#!/usr/bin/env bash\nuv sync\n"
	if err := b.ops.SetupDone(context.Background(), box, SetupReport{
		OK: false, ExitCode: 5, Script: written, Log: "the dev server never came up\n",
	}); err != nil {
		t.Fatalf("SetupDone: %v", err)
	}
	b.ops.awaitEnvBuilds()

	row := b.envs.rows[envKey("alice", "web")]
	if row.State != envs.StateFailed {
		t.Errorf("state = %q, want failed", row.State)
	}
	if row.SetupScript != written {
		t.Errorf("setup_sh = %q, want the script the failed run wrote — otherwise `env capture` binds a "+
			"disk with no script and the environment stays in agent mode forever", row.SetupScript)
	}
	if row.SetupFrom != envs.SetupFromAgent {
		t.Errorf("setup_from = %q, want agent", row.SetupFrom)
	}
}

// TestAnAgentBuildRefusesWhenItsEgressPolicyCannotBeApplied.
//
// The security argument for running `claude -p --permission-mode
// bypassPermissions` unattended is that it happens in a GOVERNED box. Egress
// policy is otherwise pushed by a thirty-second sweep, so a sandbox created
// between sweeps is absent from sluice's snapshot — and absent means
// unrestricted. If the push fails, that argument does not hold, and starting
// anyway would run the agent under a mitigation that is documented and absent.
//
// A script build only warns: the script is the owner's own and they have read
// it, so failing a build they were going to get, over a window the sweep closes
// thirty seconds later, is the worse trade.
func TestAnAgentBuildRefusesWhenItsEgressPolicyCannotBeApplied(t *testing.T) {
	for _, tc := range []struct {
		name       string
		script     string
		from       string
		wantRefuse bool
	}{
		{name: "agent mode refuses", from: envs.SetupFromAgent, wantRefuse: true},
		{name: "script mode proceeds", script: setupScript, from: envs.SetupFromManual},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newBuildRig(t)
			b.env("alice", "web", func(e *envs.Environment) {
				e.SetupScript, e.SetupFrom = tc.script, tc.from
			})
			b.secret("alice", AgentCredential)
			b.ops.netPusher = failingNetPusher{}

			_, err := b.ops.BuildEnvironment(context.Background(), alice(), "web")
			if tc.wantRefuse {
				if err == nil {
					t.Fatal("an agent build started in a box whose egress policy could not be applied")
				}
				if got := AsError("env.build", err).Code; got != "env_egress_unavailable" {
					t.Errorf("code = %q, want env_egress_unavailable: %v", got, err)
				}
				if slices.Contains(b.starter.started, "web-build") {
					t.Error("the agent was nudged anyway")
				}
				return
			}
			if err != nil {
				t.Fatalf("a script build was refused over an egress push: %v", err)
			}
			if !slices.Contains(b.starter.started, "web-build") {
				t.Error("a script build was not started")
			}
		})
	}
}

type failingNetPusher struct{}

func (failingNetPusher) PushNet(context.Context) error { return errors.New("sluice is not answering") }

// TestAnOverrunAgentBuildDestroysItsBuilder is the one place the two modes are
// treated differently after they start, and the asymmetry is deliberate.
//
// A script build's failed builder is a debugging surface worth keeping. An
// overrun AGENT build's builder is, by definition, one whose guest never
// reported — so the likeliest thing in it is an agent still running, with a
// shell, egress and the owner's decrypted credentials, and nobody watching.
// There is also nothing to keep: an agent build that overran has not written
// the script that was its whole deliverable.
func TestAnOverrunAgentBuildDestroysItsBuilder(t *testing.T) {
	for _, tc := range []struct {
		name         string
		from         string
		script       string
		wantDestroy  bool
		wantPause    bool
		wantBoxOnRow string
	}{
		{
			name: "a script build is paused and kept",
			from: envs.SetupFromManual, script: setupScript,
			wantPause: true, wantBoxOnRow: "web-build",
		},
		{
			name: "an agent build is destroyed",
			from: envs.SetupFromAgent, script: "",
			wantDestroy: true, wantBoxOnRow: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newBuildRig(t)
			b.building("alice", "web", "web-build", func(e *envs.Environment) {
				e.SetupFrom, e.SetupScript = tc.from, tc.script
			})
			// Push the row's clock well past the budget.
			row := b.envs.rows[envKey("alice", "web")]
			row.UpdatedAt = b.ops.now().Add(-2 * DefaultEnvBuildTimeout)
			b.envs.rows[envKey("alice", "web")] = row
			b.calls.reset()

			b.ops.ReconcileEnvironmentBuilds(context.Background())

			_, stillThere := b.boxes.Get("web-build")
			if tc.wantDestroy && stillThere {
				t.Error("an overrun agent builder was left in place")
			}
			if !tc.wantDestroy && !stillThere {
				t.Error("a script builder was destroyed instead of kept")
			}
			if tc.wantPause && !b.calls.has("Pause web-build") {
				t.Errorf("a script builder was not paused: %v", b.calls.all())
			}
			after := b.envs.rows[envKey("alice", "web")]
			if after.State != envs.StateFailed {
				t.Errorf("state = %q, want failed", after.State)
			}
			// A row naming a builder says "there is something to go and look
			// at". Saying that about a VM that was just destroyed is the one
			// lie this path must not tell.
			if after.BuildBox != tc.wantBoxOnRow {
				t.Errorf("build_box = %q, want %q", after.BuildBox, tc.wantBoxOnRow)
			}
		})
	}
}

// withScript is the row mutator every case above shares.
func withScript(s string) func(*envs.Environment) {
	return func(e *envs.Environment) { e.SetupScript, e.SetupFrom = s, envs.SetupFromManual }
}

// TestBuildRefusesWithoutANudgeChannel is separate from the table because it is
// the one refusal that lands AFTER the create — the host can make a builder but
// has no way to start it — and what matters is that it does not leave a running
// box and a row saying `building`.
func TestBuildRefusesWithoutANudgeChannel(t *testing.T) {
	b := newBuildRig(t)
	b.env("alice", "web", withScript(setupScript))
	b.ops.setupStarter = nil

	_, err := b.ops.BuildEnvironment(context.Background(), alice(), "web")
	var e *Error
	if !errors.As(err, &e) || e.Kind != KindDisabled {
		t.Fatalf("BuildEnvironment = %v, want KindDisabled", err)
	}
	row := b.row(t, "alice", "web")
	if row.State != envs.StateFailed {
		t.Errorf("state = %q, want %q", row.State, envs.StateFailed)
	}
	if !strings.Contains(row.BuildError, "nothing to do") {
		t.Errorf("build_error = %q", row.BuildError)
	}
	if !b.calls.has("Pause web-build") {
		t.Errorf("the builder was not paused: %v", b.calls.all())
	}
}

// ---------------------------------------------------------------------------
// Resolving the script
// ---------------------------------------------------------------------------

// TestBuildPrefersTheStoredScript is the rule that keeps a hand-fixed script
// from being silently overwritten by the repository on the next rebuild.
func TestBuildPrefersTheStoredScript(t *testing.T) {
	b := newBuildRig(t)
	b.env("alice", "web", withScript("#!/bin/sh\necho the one somebody typed\n"))
	b.attach("alice", "wandb/hivemind", "main", "web")
	b.file("wandb/hivemind", "main", SetupScriptPath, setupScript)
	b.calls.reset()

	if _, err := b.ops.BuildEnvironment(context.Background(), alice(), "web"); err != nil {
		t.Fatalf("BuildEnvironment: %v", err)
	}
	if len(b.files.asked) != 0 {
		t.Errorf("the repository was read even though the row had a script: %v", b.files.asked)
	}
	row := b.row(t, "alice", "web")
	if !strings.Contains(row.SetupScript, "somebody typed") || row.SetupFrom != envs.SetupFromManual {
		t.Errorf("script/origin = %q/%q, want the stored pair untouched", row.SetupScript, row.SetupFrom)
	}
}

// TestBuildSeedsFromTheFirstAttachedRepository pins the walk: slug order, first
// hit wins, the ref comes from the attachment, and the hit is RECORDED on the
// row with setup_from=repo so the next build takes the stored branch.
func TestBuildSeedsFromTheFirstAttachedRepository(t *testing.T) {
	b := newBuildRig(t)
	b.env("alice", "web")
	// Two attachments carrying the tag, plus one that does not. Only the first
	// two may be read, in slug order, and the walk must stop at the hit.
	b.attach("alice", "acme/api", "trunk", "web")
	b.attach("alice", "wandb/hivemind", "main", "web")
	b.attach("alice", "acme/unrelated", "main", "other")
	b.file("acme/api", "trunk", SetupScriptPath, setupScript)
	b.file("wandb/hivemind", "main", SetupScriptPath, "#!/bin/sh\nnever\n")
	b.calls.reset()

	if _, err := b.ops.BuildEnvironment(context.Background(), alice(), "web"); err != nil {
		t.Fatalf("BuildEnvironment: %v", err)
	}
	want := []string{repoFileKey("acme", "api", "trunk", SetupScriptPath)}
	if len(b.files.asked) != 1 || b.files.asked[0] != want[0] {
		t.Fatalf("read %v, want exactly %v (slug order, stop at the first hit)", b.files.asked, want)
	}
	row := b.row(t, "alice", "web")
	if row.SetupScript != setupScript || row.SetupFrom != envs.SetupFromRepo {
		t.Errorf("script/origin = %q/%q, want the seeded pair", row.SetupScript, row.SetupFrom)
	}
}

// TestBuildSeedSkipsWhatItCannotRead: a repository the App is not installed on,
// and one with no such file, say nothing about the next attachment in the list.
func TestBuildSeedSkipsWhatItCannotRead(t *testing.T) {
	b := newBuildRig(t)
	b.env("alice", "web")
	b.attach("alice", "acme/api", "trunk", "web")      // no file
	b.attach("alice", "acme/gone", "main", "web")      // installed, forbidden
	b.attach("alice", "wandb/hivemind", "main", "web") // the hit
	b.files.errs[repoFileKey("acme", "gone", "main", SetupScriptPath)] = ghapp.ErrForbidden
	b.file("wandb/hivemind", "main", SetupScriptPath, setupScript)
	// And one the App cannot see at all, which must not even be read.
	b.repos.rows[repoKey("alice", "github.com", "acme/private")] = repos.Repo{
		Owner: "alice", Host: "github.com", Slug: "acme/private", Tags: []string{"web"},
	}
	b.repos.tags[repoKey("alice", "github.com", "acme/private")] = []string{"web"}

	if _, err := b.ops.BuildEnvironment(context.Background(), alice(), "web"); err != nil {
		t.Fatalf("BuildEnvironment: %v", err)
	}
	if got := b.row(t, "alice", "web").SetupScript; got != setupScript {
		t.Errorf("script = %q, want the one from the third attachment", got)
	}
	for _, k := range b.files.asked {
		if strings.Contains(k, "private") {
			t.Errorf("read a repository the App is not installed on: %v", b.files.asked)
		}
	}
}

// TestBuildSeedReportsGitHubBeingDown is the one per-repository failure that is
// NOT skipped into silence. "You have no setup script" is a different statement
// from "we could not look", and sending somebody off to write a file they
// already have is the failure worth a distinct error.
func TestBuildSeedReportsGitHubBeingDown(t *testing.T) {
	b := newBuildRig(t)
	b.env("alice", "web")
	b.attach("alice", "wandb/hivemind", "main", "web")
	b.files.errs[repoFileKey("wandb", "hivemind", "main", SetupScriptPath)] = ghapp.ErrUpstream
	b.calls.reset()

	_, err := b.ops.BuildEnvironment(context.Background(), alice(), "web")
	var e *Error
	if !errors.As(err, &e) || e.Kind != KindUpstream || e.Code != "github_unreachable" {
		t.Fatalf("BuildEnvironment = %v, want a KindUpstream github_unreachable", err)
	}
	if got := b.calls.mutating(); len(got) > 0 {
		t.Errorf("an upstream failure mutated something: %v", got)
	}
}

// ---------------------------------------------------------------------------
// The happy path, start to finish
// ---------------------------------------------------------------------------

// TestBuildStartsTheBuilderAndNudgesIt pins what BuildEnvironment leaves behind:
// a row in `building` naming the box, a sandbox tagged with the environment and
// booted FROM BASE, and exactly one nudge.
func TestBuildStartsTheBuilderAndNudgesIt(t *testing.T) {
	b := newBuildRig(t)
	b.env("alice", "web", withScript(setupScript))
	// A binding exists, so FromBase is observable: without it the builder would
	// boot the environment's own last snapshot and accumulate its side effects.
	b.bindings.bind("alice", "web", "alicesnap")
	b.calls.reset()

	info, err := b.ops.BuildEnvironment(context.Background(), alice(), "web")
	if err != nil {
		t.Fatalf("BuildEnvironment: %v", err)
	}
	if info.State != string(envs.StateBuilding) || info.BuildBox != "web-build" {
		t.Errorf("info = %+v, want building in web-build", info)
	}
	if !b.calls.has("Create web-build owner=alice image=base") {
		t.Fatalf("the builder did not boot from the base image: %v", b.calls.all())
	}
	if got := b.tagger.tags["web-build"]; len(got) != 2 || got[0] != "default" || got[1] != "web" {
		t.Errorf("builder tags = %v, want [default web]", got)
	}
	if len(b.starter.started) != 1 || b.starter.started[0] != "web-build" {
		t.Errorf("nudges = %v, want exactly [web-build]", b.starter.started)
	}
	row := b.row(t, "alice", "web")
	if row.State != envs.StateBuilding || row.BuildBox != "web-build" || row.BuildError != "" {
		t.Errorf("row = %+v", row)
	}
}

// TestBuildFailsTheRowWhenTheCreateFails: a row left in `building` with no
// builder is the one state nothing recovers from except a 45-minute timeout, so
// a create that fails must land as `failed` immediately.
func TestBuildFailsTheRowWhenTheCreateFails(t *testing.T) {
	b := newBuildRig(t)
	b.env("alice", "web", withScript(setupScript))
	b.boxes.err = errors.New("no capacity")

	if _, err := b.ops.BuildEnvironment(context.Background(), alice(), "web"); err == nil {
		t.Fatal("BuildEnvironment succeeded with a failing manager")
	}
	row := b.row(t, "alice", "web")
	if row.State != envs.StateFailed {
		t.Fatalf("state = %q, want %q", row.State, envs.StateFailed)
	}
	if !strings.Contains(row.BuildError, "could not be created") {
		t.Errorf("build_error = %q", row.BuildError)
	}
	if row.BuildBox != "" {
		t.Errorf("build_box = %q, want empty: there is no builder to name", row.BuildBox)
	}
}

// TestBuildFailsWhenTheGuestPredatesTheFeature is the failure every template
// built before the guest unit existed will produce, and the reason it gets its
// own sentence: the timeout's "the builder never reported" names the wrong
// cause entirely.
func TestBuildFailsWhenTheGuestPredatesTheFeature(t *testing.T) {
	b := newBuildRig(t)
	b.env("alice", "web", withScript(setupScript))
	b.starter.err = host.ErrNoEnvSetup

	_, err := b.ops.BuildEnvironment(context.Background(), alice(), "web")
	var e *Error
	if !errors.As(err, &e) || e.Code != "env_setup_unsupported" {
		t.Fatalf("BuildEnvironment = %v, want env_setup_unsupported", err)
	}
	row := b.row(t, "alice", "web")
	if row.State != envs.StateFailed || row.BuildBox != "web-build" {
		t.Errorf("row = %+v, want failed naming its builder", row)
	}
	if !strings.Contains(row.BuildError, "predates environment builds") {
		t.Errorf("build_error = %q", row.BuildError)
	}
	// Paused and KEPT: the box is where the evidence is.
	if !b.calls.has("Pause web-build") {
		t.Errorf("the builder was not paused: %v", b.calls.all())
	}
	if _, ok := b.boxes.boxes["web-build"]; !ok {
		t.Error("the builder was destroyed; a failed build must keep it")
	}
}

// ---------------------------------------------------------------------------
// The guest door
// ---------------------------------------------------------------------------

// building puts an environment into the state the guest door answers from, with
// a real builder sandbox behind it.
func (b *buildRig) building(owner, env, box string, mutate ...func(*envs.Environment)) {
	m := append([]func(*envs.Environment){withScript(setupScript), func(e *envs.Environment) {
		e.State, e.BuildBox = envs.StateBuilding, box
	}}, mutate...)
	b.env(owner, env, m...)
	b.boxes.boxes[box] = &host.Sandbox{
		Name: box, Owner: owner, State: vmm.StateRunning,
		SSHAddr: "127.0.0.1:2200", SSHUser: "sparky", CreatedAt: time.Unix(0, 0).UTC(),
	}
}

// TestSetupForAnswersOnlyItsOwnBuilder is the whole authorization story of the
// guest door, and the cross-owner case is the one that matters: sandbox names
// are a single global namespace and envs.Building() is the one store query with
// no owner term, so matching a builder by NAME alone hands one owner's setup
// script — the shape of their private toolchain — to another owner's box.
func TestSetupForAnswersOnlyItsOwnBuilder(t *testing.T) {
	b := newBuildRig(t)
	b.building("alice", "web", "web-build")

	t.Run("the builder itself", func(t *testing.T) {
		box, _ := b.boxes.Get("web-build")
		job, ok, err := b.ops.SetupFor(context.Background(), box)
		if err != nil || !ok {
			t.Fatalf("SetupFor = %+v/%v/%v", job, ok, err)
		}
		if job.Payload != setupScript || job.Env != "web" {
			t.Errorf("payload/env = %q/%q", job.Payload, job.Env)
		}
		if job.Mode != SetupModeScript {
			t.Errorf("mode = %q, want %q — a row with a script is a script build",
				job.Mode, SetupModeScript)
		}
	})

	t.Run("a sandbox of another owner with the same name", func(t *testing.T) {
		// Mallory takes the name in her own namespace. The row still says
		// web-build; only the owner comparison keeps them apart.
		imposter := &host.Sandbox{Name: "web-build", Owner: "mallory", State: vmm.StateRunning}
		job, ok, err := b.ops.SetupFor(context.Background(), imposter)
		if err != nil {
			t.Fatalf("SetupFor: %v", err)
		}
		if ok || job.Payload != "" || job.Env != "" {
			t.Fatalf("a cross-owner sandbox was handed a setup job: %+v", job)
		}
	})

	t.Run("an ordinary sandbox carrying the environment's tag", func(t *testing.T) {
		// alicebox is alice's and could well be tagged `web`. It is not the
		// builder, so it has no job — which is why the resolution is
		// build_box and not the tag join.
		if err := b.tagger.SetTags("alicebox", "alice", []string{"web"}); err != nil {
			t.Fatal(err)
		}
		box, _ := b.boxes.Get("alicebox")
		_, ok, err := b.ops.SetupFor(context.Background(), box)
		if err != nil || ok {
			t.Fatalf("SetupFor(alicebox) = %v/%v, want no job", ok, err)
		}
	})
}

// TestSetupDoneSuccessCapturesBindsAndDestroys is the happy end of a build.
func TestSetupDoneSuccessCapturesBindsAndDestroys(t *testing.T) {
	b := newBuildRig(t)
	b.building("alice", "web", "web-build")
	box, _ := b.boxes.Get("web-build")
	b.calls.reset()

	revised := "#!/bin/sh\nnpm ci --omit=dev\n"
	if err := b.ops.SetupDone(context.Background(), box, SetupReport{
		OK: true, ExitCode: 0, Script: revised, Log: "done\n",
	}); err != nil {
		t.Fatalf("SetupDone: %v", err)
	}
	b.ops.awaitEnvBuilds()

	row := b.row(t, "alice", "web")
	if row.State != envs.StateReady {
		t.Fatalf("state = %q, want %q", row.State, envs.StateReady)
	}
	if row.BuildBox != "" {
		t.Errorf("build_box = %q, want cleared: a ready environment has no builder", row.BuildBox)
	}
	if row.BuiltAt == nil {
		t.Error("built_at was not stamped")
	}
	// The script the run ENDED with, not the one it started from.
	if row.SetupScript != revised {
		t.Errorf("script = %q, want the reported one", row.SetupScript)
	}
	// The ORIGIN is not re-litigated by the report: this environment's script
	// was typed by a person, and the guest handing back an edited copy of it
	// does not make it a repository's. That column decides how a LATER build
	// resolves, and the answer there is still "never overwrite this one".
	if row.SetupFrom != envs.SetupFromManual {
		t.Errorf("setup_from = %q, want %q", row.SetupFrom, envs.SetupFromManual)
	}
	// Captured, bound and destroyed, in that order.
	snap := snapshotNameFor("web", time.Unix(0, 0).UTC())
	if !b.calls.has("Snapshot box=web-build name=" + snap + " owner=alice") {
		t.Errorf("no capture: %v", b.calls.all())
	}
	if got := b.bindings.rows[bindKey("alice", "web")].Snapshot; got != snap {
		t.Errorf("tag web is bound to %q, want %q", got, snap)
	}
	if !b.calls.has("Destroy web-build") {
		t.Errorf("the builder was not destroyed: %v", b.calls.all())
	}
	if _, ok := b.boxes.boxes["web-build"]; ok {
		t.Error("the builder is still there after a successful build")
	}
}

// TestSetupDoneStampsRepoOnAScriptWithNoOrigin is the other half of the rule
// above: a row whose origin column was never written gets `repo`, because a
// script the guest hands back came out of the checkout.
func TestSetupDoneStampsRepoOnAScriptWithNoOrigin(t *testing.T) {
	b := newBuildRig(t)
	b.building("alice", "web", "web-build")
	e := b.row(t, "alice", "web")
	e.SetupFrom = ""
	b.envs.rows[envKey("alice", "web")] = e
	box, _ := b.boxes.Get("web-build")

	if err := b.ops.SetupDone(context.Background(), box, SetupReport{
		OK: true, Script: "#!/bin/sh\nnew\n",
	}); err != nil {
		t.Fatalf("SetupDone: %v", err)
	}
	b.ops.awaitEnvBuilds()

	if got := b.row(t, "alice", "web").SetupFrom; got != envs.SetupFromRepo {
		t.Errorf("setup_from = %q, want %q", got, envs.SetupFromRepo)
	}
}

// TestSetupDoneRefusesAnOversizedScript: refused, never truncated. Half a
// script is what every future fork of this environment would run — and the
// build itself still finishes, because the disk is what it was for.
func TestSetupDoneRefusesAnOversizedScript(t *testing.T) {
	b := newBuildRig(t)
	b.building("alice", "web", "web-build")
	box, _ := b.boxes.Get("web-build")

	if err := b.ops.SetupDone(context.Background(), box, SetupReport{
		OK: true, Script: strings.Repeat("x", MaxSetupScript+1),
	}); err != nil {
		t.Fatalf("SetupDone: %v", err)
	}
	b.ops.awaitEnvBuilds()

	row := b.row(t, "alice", "web")
	if row.SetupScript != setupScript {
		t.Errorf("script = %d bytes, want the previous one kept whole", len(row.SetupScript))
	}
	if row.State != envs.StateReady {
		t.Errorf("state = %q, want the build to have finished anyway", row.State)
	}
}

// TestSetupDoneFailureLeavesTheBuilderPaused is the recovery story: the box
// holds the half-built filesystem, the log and the checkout the script died in,
// and `env capture` is the way to finish from it.
func TestSetupDoneFailureLeavesTheBuilderPaused(t *testing.T) {
	b := newBuildRig(t)
	b.building("alice", "web", "web-build")
	box, _ := b.boxes.Get("web-build")

	if err := b.ops.SetupDone(context.Background(), box, SetupReport{
		OK: false, ExitCode: 42, Log: "npm ERR! code E404\nnpm ERR! 404 not found\n",
	}); err != nil {
		t.Fatalf("SetupDone: %v", err)
	}
	b.ops.awaitEnvBuilds()

	row := b.row(t, "alice", "web")
	if row.State != envs.StateFailed || row.BuildBox != "web-build" {
		t.Fatalf("row = %+v, want failed naming its builder", row)
	}
	for _, want := range []string{"exited 42", "404 not found", "web-build", "env capture web"} {
		if !strings.Contains(row.BuildError, want) {
			t.Errorf("build_error does not mention %q: %q", want, row.BuildError)
		}
	}
	if !b.calls.has("Pause web-build") {
		t.Errorf("the builder was not paused: %v", b.calls.all())
	}
	if _, ok := b.boxes.boxes["web-build"]; !ok {
		t.Fatal("a failed build destroyed its builder")
	}
	// Nothing was captured and nothing was bound.
	if _, bound := b.bindings.rows[bindKey("alice", "web")]; bound {
		t.Error("a failed build bound a template")
	}
}

// TestSetupDoneCaptureFailureLeavesFailed: the expensive half is where a build
// can still lose, and its failure has the same shape as the script's — paused
// builder, sentence on the row.
func TestSetupDoneCaptureFailureLeavesFailed(t *testing.T) {
	b := newBuildRig(t)
	b.building("alice", "web", "web-build")
	box, _ := b.boxes.Get("web-build")
	b.tmpl.err = errors.New("the disk is busy")

	if err := b.ops.SetupDone(context.Background(), box, SetupReport{OK: true}); err != nil {
		t.Fatalf("SetupDone: %v", err)
	}
	b.ops.awaitEnvBuilds()

	row := b.row(t, "alice", "web")
	if row.State != envs.StateFailed || row.BuildBox != "web-build" {
		t.Fatalf("row = %+v, want failed naming its builder", row)
	}
	if !strings.Contains(row.BuildError, "snapshot could not be taken") {
		t.Errorf("build_error = %q", row.BuildError)
	}
	if _, ok := b.boxes.boxes["web-build"]; !ok {
		t.Error("a failed capture destroyed its builder")
	}
}

// TestSetupDoneDestroyFailureStillLeavesReady is the asymmetry worth pinning:
// once the disk exists and the tag points at it, the environment IS built, and
// reporting that as a failure would send somebody to rebuild — paying the whole
// cost again to reach the state they are already in.
func TestSetupDoneDestroyFailureStillLeavesReady(t *testing.T) {
	b := newBuildRig(t)
	b.building("alice", "web", "web-build")
	box, _ := b.boxes.Get("web-build")

	// The manager refuses the destroy only. Set after the capture would have
	// used it — fakeSandboxes.err fails every mutation, so the destroy is
	// arranged through a second manager state instead.
	b.boxes.destroyErr = errors.New("the driver is wedged")

	if err := b.ops.SetupDone(context.Background(), box, SetupReport{OK: true}); err != nil {
		t.Fatalf("SetupDone: %v", err)
	}
	b.ops.awaitEnvBuilds()

	row := b.row(t, "alice", "web")
	if row.State != envs.StateReady {
		t.Fatalf("state = %q, want %q — the expensive half succeeded", row.State, envs.StateReady)
	}
	if got := b.bindings.rows[bindKey("alice", "web")].Snapshot; got == "" {
		t.Error("the tag was not bound")
	}
	if _, ok := b.boxes.boxes["web-build"]; !ok {
		t.Error("the leftover builder vanished; the test's premise is wrong")
	}
}

// TestSetupDoneIgnoresASandboxWithNoBuild: a box that is not the builder of
// anything reported a result. Neither an error nor an action — the guest could
// act on neither, and the metadata layer has already written its acceptance.
func TestSetupDoneIgnoresASandboxWithNoBuild(t *testing.T) {
	b := newBuildRig(t)
	box, _ := b.boxes.Get("alicebox")
	b.calls.reset()

	if err := b.ops.SetupDone(context.Background(), box, SetupReport{OK: true}); err != nil {
		t.Fatalf("SetupDone = %v, want nil", err)
	}
	b.ops.awaitEnvBuilds()
	if got := b.calls.mutating(); len(got) > 0 {
		t.Errorf("a stray result mutated something: %v", got)
	}
}

// TestSetupDoneRefusesACrossOwnerBuilder is SetupFor's owner check on the write
// side. A capture triggered by a stranger's box would re-point another owner's
// template at a disk that stranger authored.
func TestSetupDoneRefusesACrossOwnerBuilder(t *testing.T) {
	b := newBuildRig(t)
	b.building("alice", "web", "web-build")
	b.calls.reset()

	imposter := &host.Sandbox{Name: "web-build", Owner: "mallory", State: vmm.StateRunning}
	if err := b.ops.SetupDone(context.Background(), imposter, SetupReport{OK: true}); err != nil {
		t.Fatalf("SetupDone = %v, want a silent no-op", err)
	}
	b.ops.awaitEnvBuilds()
	if got := b.calls.mutating(); len(got) > 0 {
		t.Errorf("a cross-owner result mutated something: %v", got)
	}
	if row := b.row(t, "alice", "web"); row.State != envs.StateBuilding {
		t.Errorf("state = %q, want the build untouched", row.State)
	}
}

// ---------------------------------------------------------------------------
// capture
// ---------------------------------------------------------------------------

func TestCaptureEnvironmentFinishesAFailedBuildByHand(t *testing.T) {
	b := newBuildRig(t)
	// The state `env capture` exists for: a build that failed, its builder
	// still there, somebody having fixed it by hand.
	b.env("alice", "web", withScript(setupScript), func(e *envs.Environment) {
		e.State, e.BuildBox, e.BuildError = envs.StateFailed, "web-build", "the setup script exited 1"
	})
	b.boxes.boxes["web-build"] = &host.Sandbox{
		Name: "web-build", Owner: "alice", State: vmm.StatePaused,
	}
	b.calls.reset()

	info, err := b.ops.CaptureEnvironment(context.Background(), alice(), "web")
	if err != nil {
		t.Fatalf("CaptureEnvironment: %v", err)
	}
	if info.State != string(envs.StateReady) || info.BuildError != "" {
		t.Errorf("info = %+v, want ready with the stale error cleared", info)
	}
	snap := snapshotNameFor("web", time.Unix(0, 0).UTC())
	if got := b.bindings.rows[bindKey("alice", "web")].Snapshot; got != snap {
		t.Errorf("tag web is bound to %q, want %q", got, snap)
	}
	if _, ok := b.boxes.boxes["web-build"]; ok {
		t.Error("the builder survived a successful capture")
	}
}

func TestCaptureEnvironmentRefusals(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*buildRig)
		kind  Kind
		code  string
		want  string
	}{
		{
			name:  "environments not enabled",
			setup: func(b *buildRig) { b.ops.envs = nil },
			kind:  KindDisabled, code: "env_disabled", want: "not enabled",
		},
		{
			name:  "no such environment",
			setup: func(b *buildRig) {},
			kind:  KindNotFound, code: "environment_not_found", want: "web",
		},
		{
			name:  "somebody else's environment",
			setup: func(b *buildRig) { b.env("mallory", "web") },
			kind:  KindNotFound, code: "environment_not_found", want: "web",
		},
		{
			name:  "no builder was ever made",
			setup: func(b *buildRig) { b.env("alice", "web") },
			kind:  KindConflict, code: "no_build_box", want: "env build web",
		},
		{
			name: "the builder is gone",
			setup: func(b *buildRig) {
				b.env("alice", "web", func(e *envs.Environment) { e.BuildBox = "web-build" })
			},
			kind: KindConflict, code: "build_box_gone", want: "web-build",
		},
		{
			// The builder name now belongs to somebody else. Capturing it would
			// snapshot a stranger's disk onto this owner's tag.
			name: "the builder name belongs to another owner",
			setup: func(b *buildRig) {
				b.env("alice", "web", func(e *envs.Environment) { e.BuildBox = "web-build" })
				b.boxes.boxes["web-build"] = &host.Sandbox{
					Name: "web-build", Owner: "mallory", State: vmm.StatePaused,
				}
			},
			kind: KindConflict, code: "build_box_gone", want: "web-build",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := newBuildRig(t)
			tc.setup(b)
			b.calls.reset()

			_, err := b.ops.CaptureEnvironment(context.Background(), alice(), "web")
			var e *Error
			if !errors.As(err, &e) {
				t.Fatalf("CaptureEnvironment = %v, want a typed error", err)
			}
			if e.Kind != tc.kind || e.Code != tc.code {
				t.Errorf("kind/code = %v/%s, want %v/%s", e.Kind, e.Code, tc.kind, tc.code)
			}
			if !strings.Contains(e.Msg+" "+e.Hint, tc.want) {
				t.Errorf("refusal does not mention %q: %q / %q", tc.want, e.Msg, e.Hint)
			}
			if got := b.calls.mutating(); len(got) > 0 {
				t.Errorf("a refused capture mutated something: %v", got)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// The reconciler
// ---------------------------------------------------------------------------

func TestReconcileEnvironmentBuilds(t *testing.T) {
	// The clock the reconciler reads. Every row's updated_at is set relative to
	// it, so nothing here sleeps.
	now := time.Unix(0, 0).UTC().Add(24 * time.Hour)

	newReconcileRig := func(t *testing.T) *buildRig {
		b := newBuildRig(t)
		b.ops.now = func() time.Time { return now }
		return b
	}

	t.Run("the builder is gone", func(t *testing.T) {
		b := newReconcileRig(t)
		b.env("alice", "web", withScript(setupScript), func(e *envs.Environment) {
			e.State, e.BuildBox, e.UpdatedAt = envs.StateBuilding, "web-build", now
		})
		b.ops.ReconcileEnvironmentBuilds(context.Background())

		row := b.row(t, "alice", "web")
		if row.State != envs.StateFailed {
			t.Fatalf("state = %q, want %q", row.State, envs.StateFailed)
		}
		if !strings.Contains(row.BuildError, "web-build is gone") {
			t.Errorf("build_error = %q", row.BuildError)
		}
	})

	t.Run("the builder name now belongs to another owner", func(t *testing.T) {
		b := newReconcileRig(t)
		b.env("alice", "web", withScript(setupScript), func(e *envs.Environment) {
			e.State, e.BuildBox, e.UpdatedAt = envs.StateBuilding, "web-build", now
		})
		b.boxes.boxes["web-build"] = &host.Sandbox{
			Name: "web-build", Owner: "mallory", State: vmm.StateRunning,
		}
		b.ops.ReconcileEnvironmentBuilds(context.Background())

		if row := b.row(t, "alice", "web"); row.State != envs.StateFailed {
			t.Fatalf("state = %q, want %q", row.State, envs.StateFailed)
		}
		// And mallory's box was not touched.
		if b.calls.has("Pause web-build") {
			t.Error("the reconciler paused a sandbox belonging to another owner")
		}
	})

	t.Run("the build is older than the budget", func(t *testing.T) {
		b := newReconcileRig(t)
		b.building("alice", "web", "web-build")
		e := b.row(t, "alice", "web")
		e.UpdatedAt = now.Add(-2 * DefaultEnvBuildTimeout)
		b.envs.rows[envKey("alice", "web")] = e
		b.calls.reset()

		b.ops.ReconcileEnvironmentBuilds(context.Background())

		row := b.row(t, "alice", "web")
		if row.State != envs.StateFailed || row.BuildBox != "web-build" {
			t.Fatalf("row = %+v, want failed naming its builder", row)
		}
		if !strings.Contains(row.BuildError, "without reporting a result") {
			t.Errorf("build_error = %q", row.BuildError)
		}
		// Paused, not destroyed: the disk is still the best evidence there is.
		if !b.calls.has("Pause web-build") {
			t.Errorf("the stale builder was not paused: %v", b.calls.all())
		}
		if _, ok := b.boxes.boxes["web-build"]; !ok {
			t.Error("the stale builder was destroyed")
		}
	})

	t.Run("a build still inside its budget is left alone", func(t *testing.T) {
		b := newReconcileRig(t)
		b.building("alice", "web", "web-build")
		e := b.row(t, "alice", "web")
		e.UpdatedAt = now.Add(-2 * time.Minute)
		b.envs.rows[envKey("alice", "web")] = e
		b.calls.reset()

		b.ops.ReconcileEnvironmentBuilds(context.Background())

		if row := b.row(t, "alice", "web"); row.State != envs.StateBuilding {
			t.Fatalf("state = %q, want the build left alone", row.State)
		}
		if got := b.calls.mutating(); len(got) > 0 {
			t.Errorf("a live build was disturbed: %v", got)
		}
	})

	t.Run("a store hiccup fails nothing", func(t *testing.T) {
		b := newReconcileRig(t)
		b.building("alice", "web", "web-build")
		b.envs.buildingErr = errors.New("the database is locked")
		b.calls.reset()

		b.ops.ReconcileEnvironmentBuilds(context.Background())

		if row := b.row(t, "alice", "web"); row.State != envs.StateBuilding {
			t.Errorf("state = %q, want the build untouched", row.State)
		}
		if got := b.calls.mutating(); len(got) > 0 {
			t.Errorf("an unreadable sweep mutated something: %v", got)
		}
	})

	t.Run("a host with no environment store does nothing", func(t *testing.T) {
		r := newRig(t) // deliberately NOT withEnvs
		r.ops.ReconcileEnvironmentBuilds(context.Background())
		if got := r.calls.mutating(); len(got) > 0 {
			t.Errorf("mutations on a host with no environments: %v", got)
		}
	})
}

// ---------------------------------------------------------------------------
// The log summary
// ---------------------------------------------------------------------------

// TestSummarizeBuildLog: every byte of a guest's log tail is untrusted, it
// lands in a database column, and three surfaces print it to a terminal.
func TestSummarizeBuildLog(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"whitespace only", "\n\n   \n", ""},
		{"the last line that said anything", "one\ntwo\nthree\n\n", "three"},
		{"whitespace collapses", "a \t\t  b", "a b"},
		{
			// An ANSI escape in a refusal is a terminal a stranger can drive.
			"control bytes become spaces",
			"npm \x1b[31mERR!\x1b[0m gone\x00now",
			"npm [31mERR! [0m gone now",
		},
		{
			"a partial rune at the guest's cut",
			"finished \xff\xfe",
			"finished",
		},
		{
			"long lines are cut to a sentence",
			strings.Repeat("x", maxBuildErrorRunes+50),
			strings.Repeat("x", maxBuildErrorRunes) + "…",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := summarizeBuildLog(tc.in); got != tc.want {
				t.Errorf("summarizeBuildLog(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
