package ctlops

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/envs"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/netrules"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/users"
)

// ---------------------------------------------------------------------------
// Fixture helpers
// ---------------------------------------------------------------------------

// seedEnv puts an environment in the given state directly, bypassing the verbs,
// so a test about `--env` does not first have to run a build that does not exist
// until Phase B.
// ptr is EnvArgs.Description's optionality in a test's shape: nil is "leave the
// description alone", and this is how a test says "set it to this".
func ptr[T any](v T) *T { return &v }

func seedEnv(t *testing.T, e *fakeEnvs, owner, name string, st envs.State) {
	t.Helper()
	if _, err := e.Put(owner, name, "", nil); err != nil {
		t.Fatalf("seed %s/%s: %v", owner, name, err)
	}
	if st != envs.StateDraft {
		if err := e.SetState(owner, name, st, "", ""); err != nil {
			t.Fatalf("seed state %s/%s: %v", owner, name, err)
		}
	}
}

// ---------------------------------------------------------------------------
// Disabled
// ---------------------------------------------------------------------------

// A host with no environment store answers KindDisabled from every verb rather
// than panicking on a nil store, which is the contract every optional store in
// this package holds. `create --env` is in the table because silently ignoring
// the flag would be worse than refusing: the user gets a sandbox composed of
// nothing they asked for.
func TestEnvironmentVerbsDisabledWithoutAStore(t *testing.T) {
	r := newRig(t) // deliberately NOT withEnvs

	cases := []struct {
		name string
		call func() error
	}{
		{"list", func() error { _, err := r.ops.ListEnvironments(alice()); return err }},
		{"get", func() error { _, err := r.ops.GetEnvironment(alice(), "web"); return err }},
		{"put", func() error {
			_, err := r.ops.PutEnvironment(context.Background(), alice(), EnvArgs{Name: "web"})
			return err
		}},
		{"rm", func() error {
			_, err := r.ops.DeleteEnvironment(context.Background(), alice(), "web")
			return err
		}},
		{"var set", func() error { return r.ops.SetEnvVar(context.Background(), alice(), "web", "FOO", "1") }},
		{"var unset", func() error { return r.ops.UnsetEnvVar(context.Background(), alice(), "web", "FOO") }},
		{"create --env", func() error {
			_, err := r.ops.Create(context.Background(), alice(), CreateArgs{Name: "box", Env: "web"})
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			if err == nil {
				t.Fatal("want an error on a host with no environment store")
			}
			if !IsKind(err, KindDisabled) {
				t.Fatalf("kind = %v, want KindDisabled: %v", err.(*Error).Kind, err)
			}
		})
	}
	if r.ops.Capabilities().Environments {
		t.Error("Capabilities reported environments on a host with no store")
	}
}

// The var verbs are disabled on their own when the environment store is present
// but the var half is not: an environment still composes secrets, repos and
// rules there, so the whole feature must not go dark for a missing half.
func TestEnvVarVerbsDisabledWithoutTheVarStore(t *testing.T) {
	r := newRig(t)
	e, _, _ := withEnvs(r)
	r.ops.envVars = nil
	seedEnv(t, e, "alice", "web", envs.StateDraft)

	if err := r.ops.SetEnvVar(context.Background(), alice(), "web", "FOO", "1"); !IsKind(err, KindDisabled) {
		t.Fatalf("var set = %v, want KindDisabled", err)
	}
	// And the environment itself still lists.
	if _, err := r.ops.ListEnvironments(alice()); err != nil {
		t.Fatalf("env ls broke because the var store is missing: %v", err)
	}
}

func TestCapabilitiesReportsEnvironments(t *testing.T) {
	r := newRig(t)
	if r.ops.Capabilities().Environments {
		t.Fatal("environments reported without a store")
	}
	withEnvs(r)
	if !r.ops.Capabilities().Environments {
		t.Fatal("environments not reported with a store")
	}
}

// ---------------------------------------------------------------------------
// The masked answer
// ---------------------------------------------------------------------------

// An environment names somebody's private repository list and secret set, so
// "you have no environment called that" and "somebody else does" must be the
// SAME answer, byte for byte. This is the security boundary AGENTS.md names,
// and the assertion is on the rendered messages rather than on the kind,
// because a distinguishable sentence leaks existence just as effectively as a
// 403 would.
func TestGetEnvironmentMasksAnotherOwnersEnvironment(t *testing.T) {
	r := newRig(t)
	e, _, _ := withEnvs(r)
	seedEnv(t, e, "alice", "web", envs.StateDraft)
	r.calls.reset()

	_, missing := r.ops.GetEnvironment(mallory(), "nothing-here")
	_, notYours := r.ops.GetEnvironment(mallory(), "web")
	if missing == nil || notYours == nil {
		t.Fatalf("want two errors, got %v and %v", missing, notYours)
	}
	if missing.Error() == notYours.Error() {
		t.Fatal("the fixture is broken: both names should differ in the message but not in shape")
	}
	// The shape is what must be identical: same kind, same code, same sentence
	// modulo the name the caller themselves typed.
	me, ny := missing.(*Error), notYours.(*Error)
	if me.Kind != ny.Kind || me.Code != ny.Code {
		t.Fatalf("distinguishable classification: %+v vs %+v", me, ny)
	}
	if want := `no environment named "web"`; ny.Msg != want {
		t.Fatalf("msg = %q, want %q", ny.Msg, want)
	}
	if want := `no environment named "nothing-here"`; me.Msg != want {
		t.Fatalf("msg = %q, want %q", me.Msg, want)
	}
	if !IsKind(notYours, KindNotFound) {
		t.Fatalf("kind = %v, want KindNotFound", ny.Kind)
	}
	// And a cross-owner probe wrote nothing at all.
	if got := r.calls.mutating(); len(got) != 0 {
		t.Fatalf("a masked lookup mutated: %v", got)
	}
}

// The same masking on the verbs that could otherwise confirm existence by
// failing differently.
func TestEnvironmentMutationsMaskAnotherOwnersEnvironment(t *testing.T) {
	r := newRig(t)
	e, _, _ := withEnvs(r)
	seedEnv(t, e, "alice", "web", envs.StateDraft)

	if _, err := r.ops.DeleteEnvironment(context.Background(), mallory(), "web"); !IsKind(err, KindNotFound) {
		t.Fatalf("rm = %v, want KindNotFound", err)
	}
	if _, ok := e.rows[envKey("alice", "web")]; !ok {
		t.Fatal("a stranger's delete removed the environment")
	}
	if err := r.ops.SetEnvVar(context.Background(), mallory(), "web", "FOO", "1"); !IsKind(err, KindNotFound) {
		t.Fatalf("var set = %v, want KindNotFound", err)
	}
	if len(r.envVars.rows) != 0 {
		t.Fatalf("a stranger's var landed: %+v", r.envVars.rows)
	}
}

// ---------------------------------------------------------------------------
// Round trip
// ---------------------------------------------------------------------------

// The whole shape in one pass: create, add to it, read it back, list it,
// delete it.
func TestEnvironmentRoundTrip(t *testing.T) {
	r := newRig(t)
	e, vars, rules := withEnvs(r)
	rp, _ := withRepos(r)
	linkGitHub(r, "alice", "alice-gh", users.GitHubViaDevice, 7)
	// The two objects an environment can only ATTACH, never create.
	if err := r.secrets.PutSecret("alice", "GH_TOKEN", "ghp_x", []string{"other"}); err != nil {
		t.Fatal(err)
	}
	rules.rows["alice\x00npm"] = netrules.RuleMeta{Name: "npm", Tags: []string{"other"},
		Spec: netrules.RuleSpec{Allow: []string{"registry.npmjs.org"}}}

	got, err := r.ops.PutEnvironment(context.Background(), alice(), EnvArgs{
		Name:        "Web", // upper-case on purpose: the name IS a tag, so it folds
		Description: ptr("the web stack"),
		Repos:       []RepoArgs{{Slug: "wandb/hivemind", Ref: "main"}},
		Secrets:     []string{"GH_TOKEN"},
		Rules:       []string{"npm"},
		Vars:        []EnvVar{{Name: "NODE_ENV", Value: "test"}},
	})
	if err != nil {
		t.Fatalf("PutEnvironment: %v", err)
	}
	if got.Name != "web" {
		t.Errorf("name = %q, want the folded tag %q", got.Name, "web")
	}
	if got.State != string(envs.StateDraft) {
		t.Errorf("state = %q, want draft — nothing has built it", got.State)
	}
	if !slices.Equal(got.Repos, []string{"wandb/hivemind"}) {
		t.Errorf("repos = %v", got.Repos)
	}
	if !slices.Equal(got.Secrets, []string{"GH_TOKEN"}) {
		t.Errorf("secrets = %v", got.Secrets)
	}
	if !slices.Equal(got.Rules, []string{"npm"}) {
		t.Errorf("rules = %v", got.Rules)
	}
	if len(got.Vars) != 1 || got.Vars[0].Name != "NODE_ENV" || got.Vars[0].Value != "test" {
		t.Errorf("vars = %+v", got.Vars)
	}
	if got.HasSetup || got.SetupBytes != 0 {
		t.Errorf("a draft environment reports a setup script: %+v", got)
	}

	// ATTACHING IS A UNION. The secret and the rule-set were reaching `other`
	// before this ran and must still be.
	if tags := r.secrets.tags[secretKey("alice", "GH_TOKEN")]; !slices.Equal(tags, []string{"other", "web"}) {
		t.Errorf("secret tags = %v, want the union [other web] — an environment must not steal a credential", tags)
	}
	if tags := rules.rows["alice\x00npm"].Tags; !slices.Equal(tags, []string{"other", "web"}) {
		t.Errorf("rule tags = %v, want the union [other web]", tags)
	}
	if allow := rules.rows["alice\x00npm"].Spec.Allow; !slices.Equal(allow, []string{"registry.npmjs.org"}) {
		t.Errorf("the allowlist changed: %v — env.set must carry the spec straight back", allow)
	}

	// Update: description changes, attachments are added to, nothing is lost.
	got, err = r.ops.PutEnvironment(context.Background(), alice(), EnvArgs{
		Name: "web", Description: ptr("the web stack, v2"),
		Vars: []EnvVar{{Name: "PORT", Value: "3000"}},
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if got.Description != "the web stack, v2" {
		t.Errorf("description = %q", got.Description)
	}
	if len(got.Vars) != 2 {
		t.Errorf("vars after update = %+v, want both", got.Vars)
	}
	if !slices.Equal(got.Repos, []string{"wandb/hivemind"}) {
		t.Errorf("an update dropped the repos: %v", got.Repos)
	}

	// The stored attachment kept the ref the first call set, because the second
	// did not restate it.
	if r0 := rp.rows[repoKey("alice", "github.com", "wandb/hivemind")]; r0.Ref != "main" {
		t.Errorf("repo ref = %q, want main preserved", r0.Ref)
	}

	list, err := r.ops.ListEnvironments(alice())
	if err != nil {
		t.Fatalf("ListEnvironments: %v", err)
	}
	if len(list) != 1 || list[0].Name != "web" {
		t.Fatalf("list = %+v", list)
	}
	if !slices.Equal(list[0].Secrets, []string{"GH_TOKEN"}) {
		t.Errorf("the listing lost the composition: %+v", list[0])
	}

	one, err := r.ops.GetEnvironment(alice(), "web")
	if err != nil {
		t.Fatalf("GetEnvironment: %v", err)
	}
	if one.Name != list[0].Name || len(one.Vars) != len(list[0].Vars) {
		t.Errorf("get and list disagree: %+v vs %+v", one, list[0])
	}

	if _, err := r.ops.DeleteEnvironment(context.Background(), alice(), "web"); err != nil {
		t.Fatalf("DeleteEnvironment: %v", err)
	}
	if _, err := r.ops.GetEnvironment(alice(), "web"); !IsKind(err, KindNotFound) {
		t.Fatalf("after rm: %v, want KindNotFound", err)
	}
	if len(vars.rows) != 0 {
		t.Errorf("the vars outlived their environment: %+v", vars.rows)
	}
	_ = e
}

// A listing on a host whose composition stores are empty is [] and not null,
// and every composed field is an empty slice for the same reason.
func TestEnvironmentListingNeverSerializesNull(t *testing.T) {
	r := newRig(t)
	e, _, _ := withEnvs(r)
	seedEnv(t, e, "alice", "web", envs.StateDraft)

	list, err := r.ops.ListEnvironments(alice())
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("list = %+v", list)
	}
	got := list[0]
	if got.Repos == nil || got.Secrets == nil || got.Rules == nil || got.Vars == nil {
		t.Fatalf("a nil composition slice serializes as null: %+v", got)
	}
}

// ---------------------------------------------------------------------------
// Refusals
// ---------------------------------------------------------------------------

// TestANewEnvironmentIsGovernedByDefault.
//
// sluice runs `--enforce --open-untagged`, under which a sandbox absent from
// the policy snapshot is UNFILTERED — and a sandbox is present only if one of
// its tags carries a rule-set. So "the builder is tagged, so it is governed"
// was false: an environment nobody wrote egress rules for got unrestricted
// egress, which is the wrong default for a box that runs a setup script from a
// repository and much the wrong default for one running an unattended agent
// with the owner's decrypted credentials in it.
//
// The empty rule-set this creates is NOT deny-all, which is the thing to keep
// straight: sluice checks its base allowlist first and grants unconditionally,
// so a governed sandbox with no patterns of its own reaches the operator's
// trusted list — the package registries, github, the model API — and not the
// rest of the internet.
func TestANewEnvironmentIsGovernedByDefault(t *testing.T) {
	t.Run("a plain create gets one", func(t *testing.T) {
		r := newRig(t)
		e, _, n := withEnvs(r)
		_ = e
		if _, err := r.ops.PutEnvironment(context.Background(), alice(),
			EnvArgs{Name: "web"}); err != nil {
			t.Fatalf("env create: %v", err)
		}
		got, ok := n.rows["alice\x00web"]
		if !ok {
			t.Fatalf("no default egress rule-set was created: %v", n.rows)
		}
		if !slices.Contains(got.Tags, "web") {
			t.Errorf("the default rule-set does not carry the environment's tag: %+v", got)
		}
		// The COMMON TRUSTED SET, plus the four names defaultEnvAllow adds to
		// it. A sample rather than the whole list, chosen to fail on the two
		// ways this default has actually been wrong: too narrow to install a
		// package (the apt archives, and npm/pypi standing in for the trusted
		// set itself) and too narrow to pull an image (the registries). Either
		// way a build ends in a name that does not resolve, twenty minutes in,
		// and the person reading that cannot tell it from a network fault.
		for _, want := range []string{
			"archive.ubuntu.com", "security.ubuntu.com", "ports.ubuntu.com",
			"registry.npmjs.org", "pypi.org", "github.com",
			"ghcr.io", "docker.io", "registry-1.docker.io",
			"production.cloudflare.docker.com", "production.cloudfront.docker.com",
		} {
			if !slices.Contains(got.Spec.Allow, want) {
				t.Errorf("the default rule-set does not allow %q: %v", want, got.Spec.Allow)
			}
		}
	})

	t.Run("it does not overwrite a rule-set that already has that name", func(t *testing.T) {
		// Rule-set names and tag names are separate namespaces, so somebody can
		// perfectly well have a `web` rule-set governing their `prod` boxes and
		// then make an environment called `web`. PutRule is an upsert keyed on
		// (owner, name), so writing the default would REPLACE it: the allow
		// list gone and the tags re-pointed, which silently un-governs every
		// sandbox it was protecting.
		r := newRig(t)
		withEnvs(r)
		if err := r.netrules.PutRule("alice", "web",
			netrules.RuleSpec{Allow: []string{"internal.corp"}}, []string{"prod"}); err != nil {
			t.Fatal(err)
		}
		if _, err := r.ops.PutEnvironment(context.Background(), alice(),
			EnvArgs{Name: "web"}); err != nil {
			t.Fatalf("env create: %v", err)
		}
		got := r.netrules.rows["alice\x00web"]
		if !slices.Contains(got.Spec.Allow, "internal.corp") {
			t.Errorf("creating an environment destroyed a same-named rule-set's allow list: %+v", got)
		}
		if !slices.Contains(got.Tags, "prod") {
			t.Errorf("creating an environment re-pointed a same-named rule-set's tags, un-governing "+
				"the sandboxes it protected: %+v", got)
		}
	})

	t.Run("sandboxes already carrying the tag keep their open egress", func(t *testing.T) {
		// Tags are free-form and independent of environments: `ctl create
		// scratch --tag web` writes sandbox_tags with no environment of that
		// name anywhere. So somebody can have been running boxes tagged `web`
		// with unrestricted egress for months, and `env create web` must not
		// cut all of them down to an allowlist they never chose. That is a live
		// narrowing dressed as a default, and it is the exact failure
		// resolveEnvRules' comment warns about.
		r := newRig(t)
		withEnvs(r)
		if _, err := r.ops.Create(context.Background(), alice(), CreateArgs{
			Name: "scratch", Tags: []string{"web"},
		}); err != nil {
			t.Fatal(err)
		}
		// Adopt, because a sandbox already carrying the tag is now precisely
		// what the adoption gate refuses without it — this test is the reason
		// that gate mentions egress in its own sentence.
		if _, err := r.ops.PutEnvironment(context.Background(), alice(),
			EnvArgs{Name: "web", Adopt: true}); err != nil {
			t.Fatalf("env create: %v", err)
		}
		if _, made := r.netrules.rows["alice\x00web"]; made {
			t.Error("creating an environment narrowed the egress of sandboxes that already carried its tag")
		}
	})

	t.Run("a later env set does not re-create one the owner deleted", func(t *testing.T) {
		// Deleting the default rule-set is how somebody deliberately opens an
		// existing environment's egress. If `env set` put it back, that choice
		// would be undone by an unrelated `--var` change — and the sandboxes
		// would silently narrow under them.
		r := newRig(t)
		withEnvs(r)
		ctx := context.Background()
		if _, err := r.ops.PutEnvironment(ctx, alice(), EnvArgs{Name: "web"}); err != nil {
			t.Fatal(err)
		}
		delete(r.netrules.rows, "alice\x00web")
		if _, err := r.ops.PutEnvironment(ctx, alice(),
			EnvArgs{Name: "web", Vars: []EnvVar{{Name: "LOG_LEVEL", Value: "debug"}}}); err != nil {
			t.Fatal(err)
		}
		if _, back := r.netrules.rows["alice\x00web"]; back {
			t.Error("`env set` re-created the default egress rule-set the owner had deleted")
		}
	})

	t.Run("--open-egress skips it", func(t *testing.T) {
		r := newRig(t)
		withEnvs(r)
		if _, err := r.ops.PutEnvironment(context.Background(), alice(),
			EnvArgs{Name: "web", OpenEgress: true}); err != nil {
			t.Fatalf("env create: %v", err)
		}
		if len(r.netrules.rows) != 0 {
			t.Errorf("--open-egress still created a rule-set: %v", r.netrules.rows)
		}
	})

	t.Run("a create that names its own rules gets no default", func(t *testing.T) {
		r := newRig(t)
		withEnvs(r)
		if err := r.netrules.PutRule("alice", "npm-only",
			netrules.RuleSpec{Allow: []string{"registry.npmjs.org"}}, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := r.ops.PutEnvironment(context.Background(), alice(),
			EnvArgs{Name: "web", Rules: []string{"npm-only"}}); err != nil {
			t.Fatalf("env create: %v", err)
		}
		if _, made := r.netrules.rows["alice\x00web"]; made {
			t.Error("a second, empty rule-set was created beside the one the caller named — " +
				"which would be a policy nobody wrote sitting next to the policy they did")
		}
	})

	t.Run("a second env set does not add another", func(t *testing.T) {
		r := newRig(t)
		withEnvs(r)
		ctx := context.Background()
		if _, err := r.ops.PutEnvironment(ctx, alice(), EnvArgs{Name: "web"}); err != nil {
			t.Fatal(err)
		}
		// The owner widens the default, the way somebody would in the console.
		if err := r.netrules.PutRule("alice", "web",
			netrules.RuleSpec{Allow: []string{"internal.example.com"}}, []string{"web"}); err != nil {
			t.Fatal(err)
		}
		// `create` and `set` are one verb, so this path runs again — and must
		// not put the narrow default back over the top of their edit.
		if _, err := r.ops.PutEnvironment(ctx, alice(),
			EnvArgs{Name: "web", Description: ptr("second call")}); err != nil {
			t.Fatal(err)
		}
		got := r.netrules.rows["alice\x00web"]
		if !slices.Contains(got.Spec.Allow, "internal.example.com") {
			t.Errorf("a second `env set` reverted the owner's egress edit: %+v", got)
		}
	})
}

// A description is written once and never typed again, so every later `env set`
// has to leave it alone. It is one field on a create-and-update struct where ""
// is also a real value — "clear it" — which is why EnvArgs.Description is a
// pointer: before it was, `env set web --var LOG_LEVEL=debug` sent no
// description, and the store set the column to the empty string it was handed.
func TestAnUnrelatedSetLeavesTheDescriptionAlone(t *testing.T) {
	r := newRig(t)
	withEnvs(r)
	ctx := context.Background()
	if _, err := r.ops.PutEnvironment(ctx, alice(),
		EnvArgs{Name: "web", Description: ptr("the web box")}); err != nil {
		t.Fatal(err)
	}
	got, err := r.ops.PutEnvironment(ctx, alice(),
		EnvArgs{Name: "web", Vars: []EnvVar{{Name: "LOG_LEVEL", Value: "debug"}}})
	if err != nil {
		t.Fatalf("env set: %v", err)
	}
	if got.Description != "the web box" {
		t.Errorf("description = %q, want it untouched by a var change", got.Description)
	}
	// And the empty string still clears it, because that is a request somebody
	// made rather than one they omitted.
	got, err = r.ops.PutEnvironment(ctx, alice(), EnvArgs{Name: "web", Description: ptr("")})
	if err != nil {
		t.Fatal(err)
	}
	if got.Description != "" {
		t.Errorf("description = %q, want it cleared", got.Description)
	}
}

// TestEnvRemoveTakesBackTheEgressRuleSetItCreated.
//
// `env rm` deletes no secret, no repo attachment and no rule-set somebody
// wrote — but the default egress rule-set is one this package writes itself,
// and rule-sets have no `ctl` verb at all, so one left behind by a removed
// environment was unlistable and undeletable from a terminal. Three
// environments made and removed left three of them.
//
// Every subtest below is the same question from the other side: is this still
// the object we created? Anything an owner has touched is theirs.
func TestEnvRemoveTakesBackTheEgressRuleSetItCreated(t *testing.T) {
	ctx := context.Background()

	t.Run("the one it created goes with it", func(t *testing.T) {
		r := newRig(t)
		withEnvs(r)
		if _, err := r.ops.PutEnvironment(ctx, alice(), EnvArgs{Name: "web"}); err != nil {
			t.Fatal(err)
		}
		if _, made := r.netrules.rows["alice\x00web"]; !made {
			t.Fatal("the default egress rule-set was never created, so this test proves nothing")
		}
		res, err := r.ops.DeleteEnvironment(ctx, alice(), "web")
		if err != nil {
			t.Fatalf("env rm: %v", err)
		}
		if _, left := r.netrules.rows["alice\x00web"]; left {
			t.Error("the rule-set `env create` made was stranded by `env rm`")
		}
		if res.RemovedRule != "web" {
			t.Errorf("RemovedRule = %q, want web — a deletion has to be reported", res.RemovedRule)
		}
		// And not ALSO listed as surviving, which is the note that promises
		// nothing was deleted.
		if slices.Contains(res.Rules, "web") {
			t.Errorf("the deleted rule-set was reported as still carrying the tag: %v", res.Rules)
		}
	})

	t.Run("one the owner widened survives", func(t *testing.T) {
		// Adding their internal mirror to it makes it their policy. It is then
		// reported as surviving, like every rule-set somebody wrote.
		r := newRig(t)
		withEnvs(r)
		if _, err := r.ops.PutEnvironment(ctx, alice(), EnvArgs{Name: "web"}); err != nil {
			t.Fatal(err)
		}
		if err := r.netrules.PutRule("alice", "web",
			netrules.RuleSpec{Allow: append(slices.Clone(defaultEnvAllow), "internal.corp")},
			[]string{"web"}); err != nil {
			t.Fatal(err)
		}
		res, err := r.ops.DeleteEnvironment(ctx, alice(), "web")
		if err != nil {
			t.Fatalf("env rm: %v", err)
		}
		if _, left := r.netrules.rows["alice\x00web"]; !left {
			t.Error("`env rm` deleted a rule-set the owner had edited")
		}
		if res.RemovedRule != "" {
			t.Errorf("RemovedRule = %q, want empty", res.RemovedRule)
		}
		if !slices.Contains(res.Rules, "web") {
			t.Errorf("the surviving rule-set was not reported: %v", res.Rules)
		}
	})

	t.Run("one governing another tag survives", func(t *testing.T) {
		// Same allow list, but it is now also the policy for `prod`. Deleting
		// it would un-govern boxes this environment never knew about.
		r := newRig(t)
		withEnvs(r)
		if _, err := r.ops.PutEnvironment(ctx, alice(), EnvArgs{Name: "web"}); err != nil {
			t.Fatal(err)
		}
		if err := r.netrules.PutRule("alice", "web",
			netrules.RuleSpec{Allow: slices.Clone(defaultEnvAllow)}, []string{"web", "prod"}); err != nil {
			t.Fatal(err)
		}
		if _, err := r.ops.DeleteEnvironment(ctx, alice(), "web"); err != nil {
			t.Fatalf("env rm: %v", err)
		}
		if _, left := r.netrules.rows["alice\x00web"]; !left {
			t.Error("`env rm` deleted a rule-set that was also governing another tag")
		}
	})

	t.Run("it stays while sandboxes still carry the tag", func(t *testing.T) {
		// The egress condition rather than the ownership one. A rule-set is
		// what makes a sandbox governed at all — sluice runs --open-untagged,
		// so a box no rule-set covers is unfiltered — and deleting this one
		// under a running box would WIDEN its egress as a side effect of
		// tidying up a name.
		r := newRig(t)
		withEnvs(r)
		if _, err := r.ops.PutEnvironment(ctx, alice(), EnvArgs{Name: "web"}); err != nil {
			t.Fatal(err)
		}
		if _, err := r.ops.Create(ctx, alice(), CreateArgs{Name: "scratch", Tags: []string{"web"}}); err != nil {
			t.Fatal(err)
		}
		res, err := r.ops.DeleteEnvironment(ctx, alice(), "web")
		if err != nil {
			t.Fatalf("env rm: %v", err)
		}
		if _, left := r.netrules.rows["alice\x00web"]; !left {
			t.Error("removing the environment un-governed a sandbox that is still running under its tag")
		}
		if res.RemovedRule != "" {
			t.Errorf("RemovedRule = %q, want empty", res.RemovedRule)
		}
	})

	t.Run("another owner's identically named rule-set is untouched", func(t *testing.T) {
		r := newRig(t)
		withEnvs(r)
		if err := r.netrules.PutRule("bob", "web",
			netrules.RuleSpec{Allow: slices.Clone(defaultEnvAllow)}, []string{"web"}); err != nil {
			t.Fatal(err)
		}
		if _, err := r.ops.PutEnvironment(ctx, alice(), EnvArgs{Name: "web"}); err != nil {
			t.Fatal(err)
		}
		if _, err := r.ops.DeleteEnvironment(ctx, alice(), "web"); err != nil {
			t.Fatalf("env rm: %v", err)
		}
		if _, left := r.netrules.rows["bob\x00web"]; !left {
			t.Error("alice's `env rm` deleted bob's rule-set")
		}
	})
}

// The store's sentences about the reserved word and the grammar reach the
// caller unrewritten, under this package's own stable codes.
func TestPutEnvironmentRefusals(t *testing.T) {
	r := newRig(t)
	withEnvs(r)

	cases := []struct {
		name string
		args EnvArgs
		kind Kind
		code string
	}{
		{"empty", EnvArgs{Name: "  "}, KindInvalid, "missing_env_name"},
		{"reserved", EnvArgs{Name: "default"}, KindInvalid, "reserved_env_name"},
		{"bad grammar", EnvArgs{Name: "-nope"}, KindInvalid, "bad_env_name"},
		{"unknown secret", EnvArgs{Name: "web", Secrets: []string{"NOPE"}}, KindInvalid, "no_such_secret"},
		{"unknown rule", EnvArgs{Name: "web", Rules: []string{"nope"}}, KindInvalid, "no_such_rule"},
		{"nameless var", EnvArgs{Name: "web", Vars: []EnvVar{{Value: "1"}}}, KindInvalid, "missing_var_name"},
		// The grammar refusals below live in the stores, and every one of them
		// used to fire from inside the WRITE loop — after the environment row
		// and any retagged secret had already committed. They are here to pin
		// the ordering, not the messages.
		{"reserved var name", EnvArgs{Name: "web", Vars: []EnvVar{{Name: "PATH", Value: "/opt/bin"}}}, KindInvalid, "bad_var"},
		{"lowercase var name", EnvArgs{Name: "web", Vars: []EnvVar{{Name: "node_env", Value: "dev"}}}, KindInvalid, "bad_var"},
		{"hash in var value", EnvArgs{Name: "web", Vars: []EnvVar{{Name: "API", Value: "a#b"}}}, KindInvalid, "bad_var"},
		{"oversized var value", EnvArgs{Name: "web", Vars: []EnvVar{{Name: "BIG", Value: strings.Repeat("x", 4097)}}}, KindInvalid, "bad_var"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := r.ops.PutEnvironment(context.Background(), alice(), tc.args)
			if err == nil {
				t.Fatal("want a refusal")
			}
			e := err.(*Error)
			if e.Kind != tc.kind || e.Code != tc.code {
				t.Fatalf("kind/code = %v/%q, want %v/%q (%v)", e.Kind, e.Code, tc.kind, tc.code, err)
			}
		})
	}
	// None of them left a row behind. An attachment that cannot be honoured
	// must not leave a half-composed environment for the user to find.
	if len(r.envs.rows) != 0 {
		t.Fatalf("a refused env.set wrote a row: %+v", r.envs.rows)
	}
}

// `default` is refused because an environment's name IS its tag and every
// sandbox carries that tag — the same refusal netrules and templates already
// make, and the sentence the store wrote must survive to the terminal.
func TestPutEnvironmentPassesTheReservedSentenceThrough(t *testing.T) {
	r := newRig(t)
	withEnvs(r)
	_, err := r.ops.PutEnvironment(context.Background(), alice(), EnvArgs{Name: "default"})
	if err == nil {
		t.Fatal("`default` was accepted as an environment name")
	}
	if !strings.Contains(err.Error(), "default") {
		t.Errorf("the refusal does not name the word: %v", err)
	}
	if !err.(*Error).Verbatim {
		t.Error("the store's sentence was wrapped instead of printed")
	}
}

// Attaching a repository through an environment goes through the SAME gate
// The failure this ordering exists to prevent, end to end: a command naming a
// real secret and a bad var must not retag the secret. The var rules live in
// the secret store, so before they were hoisted into the refusal block the
// sequence ran envs.Put, then RetagSecret, and only THEN discovered that PATH
// is reserved — reporting failure to a user whose credential had just been
// widened to a new tag.
func TestPutEnvironmentDoesNotRetagASecretWhenAVarIsRefused(t *testing.T) {
	r := newRig(t)
	withEnvs(r)
	if err := r.secrets.PutSecret("alice", "GH_TOKEN", "ghp_x", []string{"other"}); err != nil {
		t.Fatal(err)
	}

	_, err := r.ops.PutEnvironment(context.Background(), alice(), EnvArgs{
		Name:    "web",
		Secrets: []string{"GH_TOKEN"},
		Vars:    []EnvVar{{Name: "PATH", Value: "/opt/bin"}},
	})
	if err == nil {
		t.Fatal("a reserved var name was accepted")
	}
	if code := err.(*Error).Code; code != "bad_var" {
		t.Fatalf("code = %q, want bad_var", code)
	}
	if tags := r.secrets.tags[secretKey("alice", "GH_TOKEN")]; !slices.Equal(tags, []string{"other"}) {
		t.Errorf("secret tags = %v, want [other] — a refused command widened a credential's scope", tags)
	}
	if len(r.envs.rows) != 0 {
		t.Errorf("a refused env.set wrote an environment row: %+v", r.envs.rows)
	}
}

// A malformed slug is refused before the environment row is written, and in the
// same order `repo add` refuses it: grammar before the GitHub-link gate. alice
// has no link in the base fixture, so a slug checked AFTER that gate would
// answer github_not_linked here and only reveal the typo on the second attempt.
func TestPutEnvironmentRefusesABadSlugBeforeWriting(t *testing.T) {
	r := newRig(t)
	withEnvs(r)
	withRepos(r)
	_, err := r.ops.PutEnvironment(context.Background(), alice(), EnvArgs{
		Name: "web", Repos: []RepoArgs{{Slug: "not-a-slug"}},
	})
	if err == nil {
		t.Fatal("want a refusal")
	}
	if code := err.(*Error).Code; code != "bad_slug" {
		t.Fatalf("code = %q, want bad_slug", code)
	}
	if len(r.envs.rows) != 0 {
		t.Fatalf("a refused env.set wrote an environment row: %+v", r.envs.rows)
	}
}

// `repo add` applies. An environment must not be a side door onto the verb that
// decides which GitHub installation reads somebody's private source.
func TestPutEnvironmentAppliesTheRepoAttachGate(t *testing.T) {
	r := newRig(t)
	withEnvs(r)
	withRepos(r)
	// alice has no GitHub link at all in the base fixture.
	_, err := r.ops.PutEnvironment(context.Background(), alice(), EnvArgs{
		Name: "web", Repos: []RepoArgs{{Slug: "wandb/hivemind"}},
	})
	if err == nil {
		t.Fatal("a repo was attached by an account with no GitHub link")
	}
	if code := err.(*Error).Code; code != "github_not_linked" {
		t.Fatalf("code = %q, want github_not_linked", code)
	}
	if len(r.envs.rows) != 0 {
		t.Fatalf("the environment row was written before the gate refused: %+v", r.envs.rows)
	}

	// An assertion-strength link is refused too — the rule AttachGate exists for.
	linkGitHub(r, "alice", "alice-gh", "assertion", 7)
	_, err = r.ops.PutEnvironment(context.Background(), alice(), EnvArgs{
		Name: "web", Repos: []RepoArgs{{Slug: "wandb/hivemind"}},
	})
	if err == nil || err.(*Error).Code != "github_link_too_weak" {
		t.Fatalf("an assertion link attached a repo: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Delete
// ---------------------------------------------------------------------------

// Removing a grouping must never destroy a user's credential, a checkout they
// may be working in, or a policy that governs other tags. `env rm` reports what
// still carries the tag and deletes NONE of it.
func TestDeleteEnvironmentKeepsTheThingsItGrouped(t *testing.T) {
	r := newRig(t)
	e, vars, rules := withEnvs(r)
	rp, _ := withRepos(r)
	linkGitHub(r, "alice", "alice-gh", users.GitHubViaDevice, 7)
	if err := r.secrets.PutSecret("alice", "GH_TOKEN", "ghp_x", nil); err != nil {
		t.Fatal(err)
	}
	rules.rows["alice\x00npm"] = netrules.RuleMeta{Name: "npm",
		Spec: netrules.RuleSpec{Allow: []string{"registry.npmjs.org"}}}
	if _, err := r.ops.PutEnvironment(context.Background(), alice(), EnvArgs{
		Name:    "web",
		Repos:   []RepoArgs{{Slug: "wandb/hivemind"}},
		Secrets: []string{"GH_TOKEN"},
		Rules:   []string{"npm"},
		Vars:    []EnvVar{{Name: "NODE_ENV", Value: "test"}},
	}); err != nil {
		t.Fatalf("setup: %v", err)
	}
	// A binding on the same tag, which DOES go: it would otherwise point a tag
	// nobody can see at a base image.
	r.bindings.bind("alice", "web", "alicesnap")

	res, err := r.ops.DeleteEnvironment(context.Background(), alice(), "web")
	if err != nil {
		t.Fatalf("DeleteEnvironment: %v", err)
	}
	if !slices.Equal(res.Repos, []string{"wandb/hivemind"}) {
		t.Errorf("result.Repos = %v, want the still-attached repo named", res.Repos)
	}
	if !slices.Equal(res.Secrets, []string{"GH_TOKEN"}) {
		t.Errorf("result.Secrets = %v", res.Secrets)
	}
	if !slices.Equal(res.Rules, []string{"npm"}) {
		t.Errorf("result.Rules = %v", res.Rules)
	}
	if res.Unbound != "alicesnap" {
		t.Errorf("result.Unbound = %q, want the snapshot the tag was bound to", res.Unbound)
	}

	// And now the point of the whole test: none of them are gone.
	if _, ok := r.secrets.vals[secretKey("alice", "GH_TOKEN")]; !ok {
		t.Error("env rm DELETED A CREDENTIAL")
	}
	if _, ok := rp.rows[repoKey("alice", "github.com", "wandb/hivemind")]; !ok {
		t.Error("env rm deleted a repo attachment")
	}
	if _, ok := rules.rows["alice\x00npm"]; !ok {
		t.Error("env rm deleted an egress rule-set")
	}
	// The two things that cannot outlive the name did go.
	if _, ok := e.rows[envKey("alice", "web")]; ok {
		t.Error("the environment row survived")
	}
	if len(vars.rows) != 0 {
		t.Errorf("vars survived their environment: %+v", vars.rows)
	}
}

// The result's slices are never nil, so a transport that prints "still carries
// the tag" gets [] rather than null for an environment that grouped nothing.
func TestDeleteEnvironmentResultIsNeverNull(t *testing.T) {
	r := newRig(t)
	e, _, _ := withEnvs(r)
	seedEnv(t, e, "alice", "web", envs.StateDraft)

	res, err := r.ops.DeleteEnvironment(context.Background(), alice(), "web")
	if err != nil {
		t.Fatal(err)
	}
	if res.Repos == nil || res.Secrets == nil || res.Rules == nil || res.Resynced == nil {
		t.Fatalf("nil slices in %+v", res)
	}
}

// ---------------------------------------------------------------------------
// Vars
// ---------------------------------------------------------------------------

// A var change reaches running sandboxes the same way a secret change does: a
// var saved while a box is running must not wait for a resume that, for a
// pinned box, never comes.
func TestEnvVarChangeResyncsRunningSandboxes(t *testing.T) {
	r := newRig(t)
	e, vars, _ := withEnvs(r)
	seedEnv(t, e, "alice", "web", envs.StateDraft)
	// alicebox is alice's, and carries the tag.
	if err := r.tagger.SetTags("alicebox", "alice", []string{"web"}); err != nil {
		t.Fatal(err)
	}
	r.calls.reset()

	if err := r.ops.SetEnvVar(context.Background(), alice(), "web", "NODE_ENV", "test"); err != nil {
		t.Fatalf("SetEnvVar: %v", err)
	}
	if !r.calls.has("ResyncEnv alicebox") {
		t.Fatalf("the running sandbox was not re-pushed:\n%v", r.calls.all())
	}
	if got := vars.rows[varKey("alice", "web", "NODE_ENV")].Value; got != "test" {
		t.Fatalf("value = %q", got)
	}

	r.calls.reset()
	if err := r.ops.UnsetEnvVar(context.Background(), alice(), "web", "NODE_ENV"); err != nil {
		t.Fatalf("UnsetEnvVar: %v", err)
	}
	if !r.calls.has("ResyncEnv alicebox") {
		t.Fatalf("the running sandbox was not re-pushed after an unset:\n%v", r.calls.all())
	}
	if len(vars.rows) != 0 {
		t.Fatalf("the var survived the unset: %+v", vars.rows)
	}
}

// Unsetting a var nobody set is its own answer, not a silent success: somebody
// typed a name and expects it gone, and a no-op would let a typo read as done.
func TestUnsetEnvVarRefusesAMissingVar(t *testing.T) {
	r := newRig(t)
	e, _, _ := withEnvs(r)
	seedEnv(t, e, "alice", "web", envs.StateDraft)

	err := r.ops.UnsetEnvVar(context.Background(), alice(), "web", "NOPE")
	if err == nil {
		t.Fatal("want a refusal")
	}
	if code := err.(*Error).Code; code != "no_such_var" {
		t.Fatalf("code = %q, want no_such_var", code)
	}
}

// A var can only be set on an environment that exists, so a typo cannot write a
// row keyed by a tag nobody will ever look at again.
func TestSetEnvVarRequiresTheEnvironment(t *testing.T) {
	r := newRig(t)
	_, vars, _ := withEnvs(r)

	if err := r.ops.SetEnvVar(context.Background(), alice(), "typo", "FOO", "1"); !IsKind(err, KindNotFound) {
		t.Fatalf("err = %v, want KindNotFound", err)
	}
	if len(vars.rows) != 0 {
		t.Fatalf("a var landed under an environment that does not exist: %+v", vars.rows)
	}
}

// ---------------------------------------------------------------------------
// Best-effort composition
// ---------------------------------------------------------------------------

// A store hiccup degrades ONE field of the answer. It must never turn `env ls`
// into an error — the composition is decoration on a row whose subject is the
// environment itself, which is the discipline Ops.info already applies to a
// sandbox's tags.
func TestEnvironmentListingSurvivesAStoreHiccup(t *testing.T) {
	r := newRig(t)
	e, vars, rules := withEnvs(r)
	rp, _ := withRepos(r)
	seedEnv(t, e, "alice", "web", envs.StateDraft)

	rp.err = errUnavailable
	rules.err = errUnavailable
	vars.err = errUnavailable
	r.secrets.err = errUnavailable
	r.bindings.err = errUnavailable

	list, err := r.ops.ListEnvironments(alice())
	if err != nil {
		t.Fatalf("a store hiccup turned env ls into an error: %v", err)
	}
	if len(list) != 1 || list[0].Name != "web" {
		t.Fatalf("list = %+v", list)
	}
	got := list[0]
	if len(got.Repos) != 0 || len(got.Secrets) != 0 || len(got.Rules) != 0 || len(got.Vars) != 0 {
		t.Fatalf("a failed store invented a composition: %+v", got)
	}
}

// ---------------------------------------------------------------------------
// create --env
// ---------------------------------------------------------------------------

// The tag an environment names is UNIONED with the tags the caller typed, not
// substituted for them: `--env web --tag ci` asked for both, and dropping
// either half silently withholds secrets, checkouts or egress.
func TestCreateWithEnvUnionsWithExplicitTags(t *testing.T) {
	r := newRig(t)
	e, _, _ := withEnvs(r)
	seedEnv(t, e, "alice", "web", envs.StateReady)

	got, err := r.ops.Create(context.Background(), alice(), CreateArgs{
		Name: "box", Env: "web", Tags: []string{"ci"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// `default` still rides along, and the whole set is sorted.
	if !slices.Equal(got.Tags, []string{"ci", "default", "web"}) {
		t.Fatalf("tags = %v, want [ci default web]", got.Tags)
	}
}

// Naming the same environment twice, or naming it in both --env and --tag, is
// one tag and not two.
func TestCreateWithEnvIsIdempotentAgainstAnEqualTag(t *testing.T) {
	r := newRig(t)
	e, _, _ := withEnvs(r)
	seedEnv(t, e, "alice", "web", envs.StateReady)

	got, err := r.ops.Create(context.Background(), alice(), CreateArgs{
		Name: "box", Env: "web", Tags: []string{"web"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !slices.Equal(got.Tags, []string{"default", "web"}) {
		t.Fatalf("tags = %v, want [default web]", got.Tags)
	}
}

// The ordering invariant create_test.go pins for the template and --ref
// refusals, applied to --env: a create that cannot possibly succeed must leave
// NO sandbox, NO tag rows and NO repo-ref rows behind.
func TestCreateWithABadEnvWritesNothing(t *testing.T) {
	r := newRig(t)
	e, _, _ := withEnvs(r)
	rp, _ := withRepos(r)
	linkGitHub(r, "alice", "alice-gh", users.GitHubViaDevice, 7)
	attach(t, r, "alice", "wandb/hivemind", "main", "hm")
	seedEnv(t, e, "alice", "building-one", envs.StateBuilding)
	seedEnv(t, e, "alice", "draft-one", envs.StateDraft)
	seedEnv(t, e, "alice", "failed-one", envs.StateFailed)

	cases := []struct {
		name string
		env  string
		kind Kind
		code string
	}{
		{"unknown", "nope", KindNotFound, "environment_not_found"},
		{"another owner's", "someone-elses", KindNotFound, "environment_not_found"},
		{"still building", "building-one", KindConflict, "env_building"},
		{"never built", "draft-one", KindConflict, "env_not_built"},
		{"build failed", "failed-one", KindConflict, "env_build_failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r.calls.reset()
			_, err := r.ops.Create(context.Background(), alice(), CreateArgs{
				Name: "box", Env: tc.env, Tags: []string{"hm"},
				Refs: []RepoRef{{Slug: "wandb/hivemind", Ref: "feat/x"}},
			})
			if err == nil {
				t.Fatal("a create naming an unusable environment was accepted")
			}
			ce := err.(*Error)
			if ce.Kind != tc.kind || ce.Code != tc.code {
				t.Fatalf("kind/code = %v/%q, want %v/%q (%v)", ce.Kind, ce.Code, tc.kind, tc.code, err)
			}
			if _, ok := r.boxes.boxes["box"]; ok {
				t.Error("the sandbox was built despite the refusal")
			}
			if tags, _ := r.tagger.TagsFor("box"); len(tags) != 0 {
				t.Errorf("tag rows were stranded by a refused create: %v", tags)
			}
			if len(rp.refs) != 0 {
				t.Errorf("ref override rows were written for a refused create: %+v", rp.refs)
			}
			if got := r.calls.mutating(); len(got) != 0 {
				t.Errorf("a refused create mutated: %v", got)
			}
		})
	}
	// Each not-ready state gets its OWN sentence: somebody whose build is
	// running needs to be told to wait, not to go looking for a typo.
	var msgs []string
	for _, env := range []string{"building-one", "draft-one", "failed-one"} {
		_, err := r.ops.Create(context.Background(), alice(), CreateArgs{Name: "box2", Env: env})
		msgs = append(msgs, err.Error())
	}
	for i := range msgs {
		for j := i + 1; j < len(msgs); j++ {
			if msgs[i] == msgs[j] {
				t.Fatalf("two build states share one sentence: %q", msgs[i])
			}
		}
	}
}

// TestAFailedBuildSaysWhatToDoBeforeItSaysWhatWentWrong.
//
// The recorded build error is a summarised line of guest log and can run to the
// width of a terminal. Spliced into the middle of the refusal — which is where
// it used to go — it pushed the two facts the reader has to have (whether there
// is still an image, and what to type) past the fold, in a sentence with two
// colons before its verb. The order is the fix: what happened, what to do,
// then the evidence.
func TestAFailedBuildSaysWhatToDoBeforeItSaysWhatWentWrong(t *testing.T) {
	r := newRig(t)
	e, _, _ := withEnvs(r)
	const boom = "the agent run exited 3: sparkbox: this box is configured, but " +
		".sparkbox/setup.sh does not run in it, so no later build could reproduce it"

	// A REBUILD that failed: it was ready once, so template_tags still points
	// at the snapshot that build produced and the environment still boots.
	seedEnv(t, e, "alice", "rebuilt", envs.StateReady)
	if err := e.SetState("alice", "rebuilt", envs.StateFailed, "rebuilt-build", boom); err != nil {
		t.Fatal(err)
	}
	// A FIRST build that failed: nothing was ever bound.
	seedEnv(t, e, "alice", "never", envs.StateFailed)
	if err := e.SetState("alice", "never", envs.StateFailed, "never-build", boom); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		env   string
		lead  string
		after string
	}{
		{"rebuilt", "still on the image it built on", "Boot that image anyway"},
		{"never", "has no base image", "Build it again"},
	} {
		t.Run(tc.env, func(t *testing.T) {
			_, err := r.ops.Create(context.Background(), alice(), CreateArgs{Name: "box", Env: tc.env})
			if err == nil {
				t.Fatal("a create naming a failed environment was accepted")
			}
			ce := err.(*Error)
			lead, said := strings.Index(ce.Msg, tc.lead), strings.Index(ce.Msg, boom)
			if lead < 0 {
				t.Fatalf("the message does not say what state the environment is in: %q", ce.Msg)
			}
			if said < 0 {
				t.Fatalf("the message drops the build error entirely: %q", ce.Msg)
			}
			if said < lead {
				t.Errorf("the guest log comes before the clause the reader acts on: %q", ce.Msg)
			}
			// Its own sentence, so a long error cannot swallow the one before it.
			if !strings.Contains(ce.Msg, ". The build said: ") {
				t.Errorf("the build error is not a sentence of its own: %q", ce.Msg)
			}
			if strings.Contains(ce.Msg, "reproduce it..") {
				t.Errorf("the message doubled the punctuation the guest already ended with: %q", ce.Msg)
			}
			if !strings.Contains(ce.Hint, tc.after) {
				t.Errorf("hint = %q, want it to point at %q", ce.Hint, tc.after)
			}
		})
	}
}

// A create naming a ready environment is placed on that tag and boots whatever
// the tag binds, exactly as a hand-typed --tag would.
func TestCreateWithEnvBootsTheBoundTemplate(t *testing.T) {
	r := newRig(t)
	e, _, _ := withEnvs(r)
	seedEnv(t, e, "alice", "web", envs.StateReady)
	r.bindings.bind("alice", "web", "alicesnap")
	r.calls.reset()

	if _, err := r.ops.Create(context.Background(), alice(), CreateArgs{Name: "box", Env: "web"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !r.calls.has("Create box owner=alice image=snap-alice-alicesnap") {
		t.Fatalf("the environment's template was not used:\n%v", r.calls.all())
	}
}

// --from-base ignores the binding and boots the operator default. It is how a
// rebuild gets a clean universal image: an environment that booted from its own
// last snapshot would accumulate every side effect of every previous build.
func TestCreateFromBaseIgnoresABoundTemplate(t *testing.T) {
	r := newRig(t)
	e, _, _ := withEnvs(r)
	seedEnv(t, e, "alice", "web", envs.StateReady)
	r.bindings.bind("alice", "web", "alicesnap")
	r.calls.reset()

	if _, err := r.ops.Create(context.Background(), alice(), CreateArgs{
		Name: "box", Env: "web", FromBase: true,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !r.calls.has("Create box owner=alice image=base") {
		t.Fatalf("--from-base did not take the default image:\n%v", r.calls.all())
	}
	// The tag is still stamped: --from-base is about the disk, not about the
	// secrets, checkouts and egress the environment composes.
	if tags, _ := r.tagger.TagsFor("box"); !slices.Contains(tags, "web") {
		t.Fatalf("tags = %v, want the environment's tag stamped", tags)
	}
}

// --from-base also skips the binding lookup entirely, which is the case it
// exists to recover from: a tag whose bound snapshot is gone refuses every
// ordinary create, and a rebuild has to be able to start anyway.
func TestCreateFromBaseSurvivesAMissingSnapshot(t *testing.T) {
	r := newRig(t)
	e, _, _ := withEnvs(r)
	seedEnv(t, e, "alice", "web", envs.StateReady)
	r.bindings.bind("alice", "web", "deleted-snap")

	if _, err := r.ops.Create(context.Background(), alice(), CreateArgs{Name: "box", Env: "web"}); err == nil {
		t.Fatal("a binding pointing at a missing snapshot must refuse an ordinary create")
	}
	if _, err := r.ops.Create(context.Background(), alice(), CreateArgs{
		Name: "box", Env: "web", FromBase: true,
	}); err != nil {
		t.Fatalf("--from-base could not escape a dangling binding: %v", err)
	}
}

// A tagged repo attachment reaches a sandbox created through --env, which is
// the whole point of the object: the environment IS the tag.
func TestCreateWithEnvCarriesTheEnvironmentsAttachments(t *testing.T) {
	r := newRig(t)
	e, _, _ := withEnvs(r)
	withRepos(r)
	linkGitHub(r, "alice", "alice-gh", users.GitHubViaDevice, 7)
	seedEnv(t, e, "alice", "web", envs.StateReady)
	if _, err := r.ops.PutEnvironment(context.Background(), alice(), EnvArgs{
		Name: "web", Repos: []RepoArgs{{Slug: "wandb/hivemind"}},
	}); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if _, err := r.ops.Create(context.Background(), alice(), CreateArgs{
		Name: "box", Env: "web", Refs: []RepoRef{{Ref: "feat/x"}},
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	refs := r.ops.repos.(*fakeRepos).refs["alice\x00box"]
	if len(refs) != 1 || refs[0].Slug != "wandb/hivemind" {
		t.Fatalf("the environment's attachment was not visible to --ref: %+v", refs)
	}
}

// ---------------------------------------------------------------------------

// errUnavailable is a store that stumbled.
var errUnavailable = errors.New("store unavailable")

// Compile-time proof the fakes still satisfy the narrow interfaces after any
// edit to either side.
var (
	_ Environments = (*fakeEnvs)(nil)
	_ EnvVars      = (*fakeEnvVars)(nil)
	_ NetRules     = (*fakeNetRules)(nil)
	_ SecretTags   = (*fakeSecrets)(nil)
)

// ---------------------------------------------------------------------------
// Adoption
// ---------------------------------------------------------------------------

// TestEnvCreateRefusesATagAlreadyInUse is the adoption gate.
//
// An environment's name IS its tag, and tags are OLDER than environments and
// shared with four other stores: `ctl create scratch --tag web`, `repo add
// --tag web` and `net --tag web` all write rows with no environment anywhere.
// So `env create web` can silently take ownership of configuration somebody has
// been running for months — and it takes it the instant the row exists, because
// every composition read joins on the name.
//
// One subtest per kind of carrier, because each comes from a different store and
// a gate that consulted four of the five would be indistinguishable from one
// that worked, right up until the day somebody's fifth thing got adopted.
func TestEnvCreateRefusesATagAlreadyInUse(t *testing.T) {
	for _, tc := range []struct {
		name string
		// seed puts one carrier on the tag `web` and nothing else.
		seed func(t *testing.T, r *rig)
	}{{
		name: "a repo attachment",
		seed: func(t *testing.T, r *rig) {
			rp, _ := withRepos(r)
			linkGitHub(r, "alice", "alice-gh", users.GitHubViaDevice, 7)
			if _, err := r.ops.AttachRepo(context.Background(), alice(),
				RepoArgs{Slug: "wandb/hivemind", Tags: []string{"web"}}); err != nil {
				t.Fatal(err)
			}
			_ = rp
		},
	}, {
		name: "a secret",
		seed: func(t *testing.T, r *rig) {
			if err := r.secrets.PutSecret("alice", "GITHUB_TOKEN", "x", []string{"web"}); err != nil {
				t.Fatal(err)
			}
		},
	}, {
		name: "an egress rule-set",
		seed: func(t *testing.T, r *rig) {
			if err := r.netrules.PutRule("alice", "npm-only",
				netrules.RuleSpec{Allow: []string{"registry.npmjs.org"}}, []string{"web"}); err != nil {
				t.Fatal(err)
			}
		},
	}, {
		name: "a plain variable",
		seed: func(t *testing.T, r *rig) {
			if err := r.envVars.PutVar("alice", "web", "NODE_ENV", "test"); err != nil {
				t.Fatal(err)
			}
		},
	}, {
		name: "a bound base image",
		seed: func(t *testing.T, r *rig) {
			r.bindings.bind("alice", "web", "web-260902-1142")
		},
	}, {
		name: "a sandbox already carrying the tag",
		seed: func(t *testing.T, r *rig) {
			if _, err := r.ops.Create(context.Background(), alice(),
				CreateArgs{Name: "scratch", Tags: []string{"web"}}); err != nil {
				t.Fatal(err)
			}
		},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			withEnvs(r)
			tc.seed(t, r)

			_, err := r.ops.PutEnvironment(context.Background(), alice(), EnvArgs{Name: "web"})
			var e *Error
			if !errors.As(err, &e) {
				t.Fatalf("err = %v, want a *Error", err)
			}
			if e.Kind != KindConflict || e.Code != "env_tag_in_use" {
				t.Errorf("got %s/%s, want conflict/env_tag_in_use", e.Kind, e.Code)
			}
			// The refusal runs with the others, BEFORE the first write. A
			// half-created environment left behind by a gate that fired too late
			// is worse than no gate: the tag would already be adopted, and the
			// error would say it was not.
			if len(r.envs.rows) != 0 {
				t.Errorf("a refused create still wrote an environment: %v", r.envs.rows)
			}

			// And the same call with consent goes through.
			if _, err := r.ops.PutEnvironment(context.Background(), alice(),
				EnvArgs{Name: "web", Adopt: true}); err != nil {
				t.Fatalf("--adopt was refused: %v", err)
			}
			if len(r.envs.rows) != 1 {
				t.Errorf("an adopted create wrote %d environments, want 1", len(r.envs.rows))
			}
		})
	}
}

// TestEnvCreateOnAFreeTagNeedsNoConsent is the other half, and the one that
// keeps the gate from becoming a tax on the ordinary case: a name nothing is
// using is created without anybody being asked anything.
func TestEnvCreateOnAFreeTagNeedsNoConsent(t *testing.T) {
	r := newRig(t)
	withEnvs(r)
	if _, err := r.ops.PutEnvironment(context.Background(), alice(), EnvArgs{Name: "web"}); err != nil {
		t.Fatalf("env create on an unused name: %v", err)
	}
	// Nothing was adopted, so nothing is recorded — an empty record and no
	// record are the same state and must not be two.
	if got := r.envs.rows[envKey("alice", "web")]; got.Adopted != nil {
		t.Errorf("adoption record on a free tag: %+v", got.Adopted)
	}
}

// TestEnvSetOnAnExistingEnvironmentIsNotAnAdoption pins that the gate is a
// CREATE gate.
//
// `create` and `set` are one verb on one handler, so without the creating check
// every later edit would re-ask a question the owner already answered — and
// `env set web --var LOG_LEVEL=debug` would start failing the moment the
// environment had a single repo on it.
func TestEnvSetOnAnExistingEnvironmentIsNotAnAdoption(t *testing.T) {
	r := newRig(t)
	withEnvs(r)
	ctx := context.Background()
	if _, err := r.ops.PutEnvironment(ctx, alice(), EnvArgs{Name: "web"}); err != nil {
		t.Fatal(err)
	}
	if err := r.envVars.PutVar("alice", "web", "NODE_ENV", "test"); err != nil {
		t.Fatal(err)
	}
	// The tag now carries a var. A second call must not be refused for it.
	if _, err := r.ops.PutEnvironment(ctx, alice(),
		EnvArgs{Name: "web", Vars: []EnvVar{{Name: "LOG_LEVEL", Value: "debug"}}}); err != nil {
		t.Fatalf("env set on an existing environment was refused: %v", err)
	}
}

// TestEnvRmKeepsWhatItAdopted is the reason the adoption record is stored at
// all.
//
// `env rm` destroys the tag's variables and unbinds its base image, on the
// argument that they "cannot outlive the name". That argument is true only for
// the ones the environment brought with it. An environment created over
// somebody's existing tag and then deleted used to take their configuration
// with it — permanently, with no confirmation and no way back.
func TestEnvRmKeepsWhatItAdopted(t *testing.T) {
	r := newRig(t)
	withEnvs(r)
	ctx := context.Background()

	// The world before the environment: a var and a base image on the tag.
	if err := r.envVars.PutVar("alice", "web", "INHERITED", "yes"); err != nil {
		t.Fatal(err)
	}
	r.bindings.bind("alice", "web", "old-disk")

	if _, err := r.ops.PutEnvironment(ctx, alice(), EnvArgs{Name: "web", Adopt: true}); err != nil {
		t.Fatal(err)
	}
	// A var the environment DID create. It is the control: without one, a
	// delete that simply skipped every var would pass this test.
	if _, err := r.ops.PutEnvironment(ctx, alice(),
		EnvArgs{Name: "web", Vars: []EnvVar{{Name: "MINE", Value: "1"}}}); err != nil {
		t.Fatal(err)
	}

	res, err := r.ops.DeleteEnvironment(ctx, alice(), "web")
	if err != nil {
		t.Fatalf("env rm: %v", err)
	}
	if _, gone := r.envVars.rows[varKey("alice", "web", "INHERITED")]; !gone {
		t.Error("env rm destroyed a variable that was on the tag before the environment existed")
	}
	if _, still := r.envVars.rows[varKey("alice", "web", "MINE")]; still {
		t.Error("env rm kept a variable the environment created — only adopted ones survive")
	}
	if b, bound := r.bindings.rows[bindKey("alice", "web")]; !bound || b.Snapshot != "old-disk" {
		t.Error("env rm unbound a base image that was bound before the environment adopted the tag")
	}
	// Reported, not merely done: somebody who knows the rule assumes the worst
	// about configuration they cannot see.
	if len(res.KeptVars) != 1 || res.KeptVars[0] != "INHERITED" {
		t.Errorf("kept_vars = %v, want [INHERITED]", res.KeptVars)
	}
	if res.KeptSnapshot != "old-disk" {
		t.Errorf("kept_snapshot = %q, want old-disk", res.KeptSnapshot)
	}
	if res.Unbound != "" {
		t.Errorf("unbound = %q, want empty — the binding was adopted and stays", res.Unbound)
	}
}

// TestEnvRmStillTakesItsOwn is the mirror, and it matters as much: the keeping
// is scoped to what was adopted, so an ordinary environment's variables and its
// own captured disk still go with it. Otherwise every deleted environment would
// leave unreachable (owner, tag, name) rows behind that reappear the day the
// name is reused.
func TestEnvRmStillTakesItsOwn(t *testing.T) {
	r := newRig(t)
	withEnvs(r)
	ctx := context.Background()
	if _, err := r.ops.PutEnvironment(ctx, alice(),
		EnvArgs{Name: "web", Vars: []EnvVar{{Name: "NODE_ENV", Value: "test"}}}); err != nil {
		t.Fatal(err)
	}
	r.bindings.bind("alice", "web", "web-disk")

	res, err := r.ops.DeleteEnvironment(ctx, alice(), "web")
	if err != nil {
		t.Fatalf("env rm: %v", err)
	}
	if _, still := r.envVars.rows[varKey("alice", "web", "NODE_ENV")]; still {
		t.Error("env rm left its own variable behind")
	}
	if _, bound := r.bindings.rows[bindKey("alice", "web")]; bound {
		t.Error("env rm left its own template binding pointing at a tag nobody can see")
	}
	if res.Unbound != "web-disk" {
		t.Errorf("unbound = %q, want web-disk", res.Unbound)
	}
	if len(res.KeptVars) != 0 || res.KeptSnapshot != "" {
		t.Errorf("an environment that adopted nothing reported keeping something: %+v", res)
	}
}

// TestEnvRmDropsABindingTheBuildReplaced is the edge between the two rules
// above.
//
// A successful `env build` re-points the tag at a disk this environment
// captured, so that snapshot is the environment's own however the tag started
// out. Keeping it because SOME binding was once adopted would leave a dead tag
// pointing at an image nobody can reach — which is the exact thing the unbind
// exists to prevent.
func TestEnvRmDropsABindingTheBuildReplaced(t *testing.T) {
	r := newRig(t)
	withEnvs(r)
	ctx := context.Background()
	r.bindings.bind("alice", "web", "old-disk")
	if _, err := r.ops.PutEnvironment(ctx, alice(), EnvArgs{Name: "web", Adopt: true}); err != nil {
		t.Fatal(err)
	}
	// What a build does at the end of a successful capture.
	r.bindings.bind("alice", "web", "web-260903-0900")

	res, err := r.ops.DeleteEnvironment(ctx, alice(), "web")
	if err != nil {
		t.Fatalf("env rm: %v", err)
	}
	if res.Unbound != "web-260903-0900" {
		t.Errorf("unbound = %q, want the snapshot the build bound", res.Unbound)
	}
	if res.KeptSnapshot != "" {
		t.Errorf("kept_snapshot = %q, want empty — the adopted binding is long gone", res.KeptSnapshot)
	}
}

// TestEnvironmentsForTags is the launch door's selector: owner-scoped, filtered
// to the tags asked about, newest first.
func TestEnvironmentsForTags(t *testing.T) {
	r := newRig(t)
	withEnvs(r)
	ctx := context.Background()
	for _, name := range []string{"web", "ci", "prod"} {
		if _, err := r.ops.PutEnvironment(ctx, alice(), EnvArgs{Name: name}); err != nil {
			t.Fatal(err)
		}
	}
	// The fake stamps one fixed time, so the order has to come from somewhere;
	// spreading them apart is what makes "newest first" an assertion rather
	// than a coincidence.
	for i, name := range []string{"ci", "web"} {
		row := r.envs.rows[envKey("alice", name)]
		row.UpdatedAt = time.Unix(int64(i+1)*1000, 0).UTC()
		r.envs.rows[envKey("alice", name)] = row
	}

	got, err := r.ops.EnvironmentsForTags(alice(), []string{"web", "ci", "default", "nope"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d environments, want 2 (web and ci; default and nope are not environments)", len(got))
	}
	if got[0].Name != "web" || got[1].Name != "ci" {
		t.Errorf("order = %s, %s; want web then ci (most recently updated first)", got[0].Name, got[1].Name)
	}

	// Owner scoping, which for this method is the whole security story: it
	// takes tag NAMES, and two people routinely share them.
	if out, err := r.ops.EnvironmentsForTags(mallory(), []string{"web", "ci"}); err != nil || len(out) != 0 {
		t.Errorf("another owner saw %v (err %v), want nothing", out, err)
	}
}
