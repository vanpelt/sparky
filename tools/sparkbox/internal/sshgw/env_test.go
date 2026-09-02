package sshgw

// The `env` verbs through the door. The golden table next door pins the empty
// cases — create, show, ls, rm on an environment that composes nothing — so
// what is here is the two things that need more of a host than that fixture
// has: a composition with real secrets and variables in it, and the setup
// script, which is the one payload on this channel that arrives on stdin.

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/envs"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/secrets"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/templates"
)

// newCtlStackEnv is the stack with the secrets store wired as all three of its
// halves — values, tags, and plain variables — which the golden fixture
// deliberately is not. An environment composes secrets it does not own, so this
// is the only shape in which `--secret` and `--var` do anything at all.
func newCtlStackEnv(t *testing.T) (*ctlStack, *secrets.Store) {
	t.Helper()
	dir := t.TempDir()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	store, err := secrets.Open(filepath.Join(dir, "secrets.db"),
		secrets.DeriveKEK([]byte("env-test-key-material")), log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() }) //nolint:errcheck
	st := newCtlStackWith(t, testRoster(), func(cfg *ctlops.Config) {
		cfg.Secrets, cfg.SecretTags, cfg.EnvVars, cfg.Tags = store, store, store, store
	})
	return st, store
}

// TestControlEnvComposition walks a composed environment through the door:
// a secret that already existed gains the tag, two variables are set, one is
// unset again, and `show` renders the lot.
func TestControlEnvComposition(t *testing.T) {
	st, store := newCtlStackEnv(t)
	if err := store.PutSecret("alice", "GITHUB_TOKEN", "ghp_"+strings.Repeat("x", 36), nil); err != nil {
		t.Fatal(err)
	}

	s := st.run(t, "alice", "env", "create", "web",
		"--secret", "GITHUB_TOKEN", "--var", "NODE_ENV=test", "--var=PORT=8080")
	if s.code != 0 {
		t.Fatalf("create: exit %d, stderr %q", s.code, s.stderr.String())
	}
	want := "created web — 1 secret · 2 vars  (draft)\r\n" +
		"use it now with:  ssh new@hivemind.tools -- web\r\n"
	if s.out.String() != want {
		t.Errorf("create printed %q, want %q", s.out.String(), want)
	}

	// The union rule, checked at the store rather than in the rendering: the
	// secret keeps the `default` tag it already had, so it still reaches every
	// sandbox it was reaching.
	metas, err := store.ListSecrets("alice")
	if err != nil || len(metas) != 1 {
		t.Fatalf("ListSecrets = %v, %v", metas, err)
	}
	if got := strings.Join(metas[0].Tags, ","); got != "default,web" {
		t.Errorf("secret tags = %q, want %q — attaching must ADD", got, "default,web")
	}

	s = st.run(t, "alice", "env", "show", "web")
	for _, want := range []string{
		"web                  draft\r\n",
		"  secrets:\r\n    GITHUB_TOKEN\r\n",
		// A var's value IS printed; a secret's never is.
		"  vars:\r\n    NODE_ENV=test\r\n    PORT=8080\r\n",
	} {
		if !strings.Contains(s.out.String(), want) {
			t.Errorf("show is missing %q:\n%s", want, s.out.String())
		}
	}
	if strings.Contains(s.out.String(), "ghp_") {
		t.Errorf("show printed a secret's value:\n%s", s.out.String())
	}

	// `set` adds to what is there and --unset takes one variable away, and the
	// summary line counts what is LEFT rather than what there was.
	s = st.run(t, "alice", "env", "set", "web", "--var", "LOG=debug", "--unset", "PORT")
	if s.code != 0 {
		t.Fatalf("set: exit %d, stderr %q", s.code, s.stderr.String())
	}
	if got := s.out.String(); got != "updated web — 1 secret · 2 vars  (draft)\r\n" {
		t.Errorf("set printed %q", got)
	}
	vars, err := store.VarsForTag("alice", "web")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, v := range vars {
		names = append(names, v.Name)
	}
	if got := strings.Join(names, ","); got != "LOG,NODE_ENV" {
		t.Errorf("vars = %q, want %q", got, "LOG,NODE_ENV")
	}

	// The delete rule, which is the sharpest edge in the feature: the
	// environment and its variables go, the credential does not, and the user
	// is told so in the same breath.
	s = st.run(t, "alice", "env", "rm", "web")
	if s.code != 0 {
		t.Fatalf("rm: exit %d, stderr %q", s.code, s.stderr.String())
	}
	if !strings.Contains(s.out.String(), "still carry the tag \"web\" and were not deleted") ||
		!strings.Contains(s.out.String(), "secrets  GITHUB_TOKEN") {
		t.Errorf("rm did not report the surviving secret:\n%s", s.out.String())
	}
	if metas, err := store.ListSecrets("alice"); err != nil || len(metas) != 1 {
		t.Fatalf("the secret was destroyed by `env rm`: %v, %v", metas, err)
	}
	if vars, err := store.VarsForTag("alice", "web"); err != nil || len(vars) != 0 {
		t.Errorf("the environment's vars outlived it: %v, %v", vars, err)
	}
}

// TestControlEnvScript walks the setup script in and out again.
//
// The property under the round trip is that the bytes are the bytes: a shell
// script is not a credential, so nothing here strips a banner, matches a shape
// or rewrites an indent, and what comes back out is what a redirect can write
// straight into a file.
func TestControlEnvScript(t *testing.T) {
	st, _ := newCtlStackEnv(t)
	if s := st.run(t, "alice", "env", "create", "web"); s.code != 0 {
		t.Fatalf("create: %q", s.stderr.String())
	}

	const script = "#!/bin/sh\nset -eu\n\n  # indented on purpose\n  uv sync\n"
	s := st.newSession("alice")
	s.in = strings.NewReader(script)
	s.cmd = []string{"env", "script", "web", "--set"}
	st.gw.handleControl(s, "alice", st.log)
	if s.code != 0 {
		t.Fatalf("script --set: exit %d, stderr %q", s.code, s.stderr.String())
	}
	if !strings.HasPrefix(s.out.String(), "set the setup script on web (") {
		t.Errorf("script --set printed %q", s.out.String())
	}

	// Read back: stdout is the file and nothing else, so `env script web >
	// setup.sh` writes a script and not a report.
	s = st.run(t, "alice", "env", "script", "web")
	if s.code != 0 {
		t.Fatalf("script: exit %d, stderr %q", s.code, s.stderr.String())
	}
	// TrimSpace on the way in takes the trailing newline; writeScript puts
	// exactly one back.
	if got := s.out.String(); got != strings.TrimSpace(script)+"\n" {
		t.Errorf("script round-tripped as %q, want %q", got, strings.TrimSpace(script)+"\n")
	}
	if !strings.Contains(s.stderr.String(), "from manual") {
		t.Errorf("the provenance line is missing from stderr: %q", s.stderr.String())
	}

	// `show` reports it as a presence and a size — never the text, which would
	// make a listing unreadable.
	s = st.run(t, "alice", "env", "show", "web")
	if !strings.Contains(s.out.String(), "setup        52 bytes, from manual") {
		t.Errorf("show does not report the script: %q", s.out.String())
	}
	if strings.Contains(s.out.String(), "uv sync") {
		t.Errorf("show inlined the script:\n%s", s.out.String())
	}
}

// TestControlEnvScriptRefusals: an environment with no script, an empty pipe,
// and the argv grammar somebody will try first.
func TestControlEnvScriptRefusals(t *testing.T) {
	st, _ := newCtlStackEnv(t)
	if s := st.run(t, "alice", "env", "create", "web"); s.code != 0 {
		t.Fatalf("create: %q", s.stderr.String())
	}

	// No script: exit 1 with an empty stdout, because the command people run is
	// a redirect and a zero-byte "success" would truncate the file they were
	// about to edit.
	s := st.run(t, "alice", "env", "script", "web")
	if s.code != 1 || s.out.Len() != 0 {
		t.Errorf("script on an empty environment = exit %d, stdout %q", s.code, s.out.String())
	}
	if !strings.Contains(s.stderr.String(), "has no setup script") {
		t.Errorf("stderr = %q", s.stderr.String())
	}

	// An empty pipe is refused rather than stored, which would wipe a script
	// somebody had.
	s = st.newSession("alice")
	s.in = strings.NewReader("   \n\n")
	s.cmd = []string{"env", "script", "web", "--set"}
	st.gw.handleControl(s, "alice", st.log)
	if s.code != 2 || !strings.Contains(s.stderr.String(), "no script arrived on stdin") {
		t.Errorf("an empty pipe = exit %d, stderr %q", s.code, s.stderr.String())
	}

	// And a script named on the command line teaches the pipe rather than
	// storing a filename.
	s = st.run(t, "alice", "env", "script", "web", "./setup.sh")
	if s.code != 2 || !strings.Contains(s.stderr.String(), "read from stdin") {
		t.Errorf("a positional script = exit %d, stderr %q", s.code, s.stderr.String())
	}

	// The masking invariant, on both halves of the verb: alice's `web` is real,
	// and reading OR writing it as somebody else must read exactly like an
	// environment nobody has.
	const masked = "sparkbox: no environment named \"web\"\r\n"
	if s := st.run(t, "mallory", "env", "script", "web"); s.stderr.String() != masked || s.code != 1 {
		t.Errorf("reading a stranger's script = exit %d, stderr %q", s.code, s.stderr.String())
	}
	s = st.newSession("mallory")
	s.in = strings.NewReader("echo pwned\n")
	s.cmd = []string{"env", "script", "web", "--set"}
	st.gw.handleControl(s, "mallory", st.log)
	if s.stderr.String() != masked || s.code != 1 {
		t.Errorf("writing a stranger's script = exit %d, stderr %q", s.code, s.stderr.String())
	}
}

// TestControlEnvNotEnabled: a host with no environment store answers every env
// command with the same sentence, including the ones whose arguments are wrong,
// so a user learns what the host is rather than what they typed. It is the
// shape TestControlNodeOnASingleBox pins for the node verbs.
func TestControlEnvNotEnabled(t *testing.T) {
	st := newCtlStackWith(t, testRoster(), func(cfg *ctlops.Config) { cfg.Environments = nil })
	const want = "sparkbox: environments are not enabled on this host\r\n"
	for _, args := range [][]string{
		{"env"}, {"env", "ls"}, {"env", "show", "web"}, {"env", "create", "web"},
		{"env", "rm", "web"}, {"env", "script", "web"}, {"env", "wat"},
	} {
		s := st.run(t, "alice", args...)
		if s.stderr.String() != want || s.code != 1 {
			t.Errorf("%v = exit %d, stderr %q; want exit 1 and %q", args, s.code, s.stderr.String(), want)
		}
	}
}

// TestParseEnvSet pins the grammar. The refusals matter more than the
// acceptances: this door does not fold leftovers into anything, so a flag it
// did not understand has to be a refusal or it is a silently missing secret.
func TestParseEnvSet(t *testing.T) {
	for _, tc := range []struct {
		name      string
		args      []string
		wantName  string
		wantRepos []string
		wantSecs  []string
		wantRules []string
		wantVars  []ctlops.EnvVar
		wantUnset []string
		wantDesc  string
	}{
		{name: "just a name", args: []string{"web"}, wantName: "web"},
		{name: "a repo", args: []string{"web", "--repo", "wandb/hivemind"},
			wantName: "web", wantRepos: []string{"wandb/hivemind"}},
		{name: "the equals form", args: []string{"web", "--repo=wandb/hivemind"},
			wantName: "web", wantRepos: []string{"wandb/hivemind"}},
		{name: "flags before the name", args: []string{"--secret", "TOKEN", "web"},
			wantName: "web", wantSecs: []string{"TOKEN"}},
		{name: "everything accumulates", args: []string{
			"web", "--repo", "a/b", "--repo", "c/d", "--secret", "T", "--rule", "npm",
			"--var", "K=1", "--var", "J=2", "--unset", "OLD", "--description", "the web box"},
			wantName: "web", wantRepos: []string{"a/b", "c/d"}, wantSecs: []string{"T"},
			wantRules: []string{"npm"},
			wantVars:  []ctlops.EnvVar{{Name: "K", Value: "1"}, {Name: "J", Value: "2"}},
			wantUnset: []string{"OLD"}, wantDesc: "the web box"},
		// A variable's value may hold the separator, and the attached spelling
		// splits at the FIRST '=' for the flag and the next one for the pair.
		{name: "an equals in a value", args: []string{"web", "--var=DSN=postgres://x?a=b"},
			wantName: "web", wantVars: []ctlops.EnvVar{{Name: "DSN", Value: "postgres://x?a=b"}}},
		// An empty value is a legitimate variable: `--var DEBUG=` sets it to
		// the empty string, which is not the same as not setting it.
		{name: "an empty value", args: []string{"web", "--var", "DEBUG="},
			wantName: "web", wantVars: []ctlops.EnvVar{{Name: "DEBUG", Value: ""}}},
		// Last description wins; repeating a flag that names one thing is a
		// correction rather than a list.
		{name: "the last description wins", args: []string{"web", "--description", "a", "--desc", "b"},
			wantName: "web", wantDesc: "b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, unset, err := parseEnvSet(tc.args)
			if err != nil {
				t.Fatalf("parseEnvSet(%q) errored: %v", tc.args, err)
			}
			if a.Name != tc.wantName {
				t.Errorf("name = %q, want %q", a.Name, tc.wantName)
			}
			var slugs []string
			for _, r := range a.Repos {
				slugs = append(slugs, r.Slug)
			}
			eq(t, "repos", slugs, tc.wantRepos)
			eq(t, "secrets", a.Secrets, tc.wantSecs)
			eq(t, "rules", a.Rules, tc.wantRules)
			eq(t, "unset", unset, tc.wantUnset)
			if len(a.Vars) != len(tc.wantVars) {
				t.Fatalf("vars = %#v, want %#v", a.Vars, tc.wantVars)
			}
			for i := range a.Vars {
				if a.Vars[i] != tc.wantVars[i] {
					t.Errorf("var %d = %#v, want %#v", i, a.Vars[i], tc.wantVars[i])
				}
			}
			if a.Description != tc.wantDesc {
				t.Errorf("description = %q, want %q", a.Description, tc.wantDesc)
			}
		})
	}
}

func TestParseEnvSetRejects(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{}, "name the environment"},
		{[]string{"--repo", "a/b"}, "name the environment"},
		{[]string{"web", "prod"}, "one environment at a time"},
		{[]string{"web", "--repo"}, "--repo needs a value"},
		{[]string{"web", "--repo="}, "--repo needs a value"},
		{[]string{"web", "--secret"}, "--secret needs a value"},
		{[]string{"web", "--var", "NODE_ENV"}, "NAME=value"},
		{[]string{"web", "--unset", "K=1"}, "--unset takes a variable's name"},
		{[]string{"web", "--var", "K=1", "--unset", "K"}, "both set and unset"},
		// The typo that would otherwise compose an environment missing exactly
		// the thing the user named.
		{[]string{"web", "--secrat", "TOKEN"}, "unknown flag"},
	} {
		_, _, err := parseEnvSet(tc.args)
		if err == nil {
			t.Errorf("parseEnvSet(%q) was accepted", tc.args)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("parseEnvSet(%q) = %v, want a sentence holding %q", tc.args, err, tc.want)
		}
	}
}

func eq(t *testing.T, what string, got, want []string) {
	t.Helper()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("%s = %#v, want %#v", what, got, want)
	}
}

// ---------------------------------------------------------------------------
// The build
// ---------------------------------------------------------------------------

// fakeSetupStarter stands in for the envsync syncer: it records the builders it
// was asked to start and never touches a guest. The real one runs a systemd
// unit inside a VM, which is exactly the half of this feature no test in this
// tree can reach.
type fakeSetupStarter struct {
	mu    sync.Mutex
	boxes []string
	err   error
}

func (f *fakeSetupStarter) StartSetup(_ context.Context, box *host.Sandbox) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.boxes = append(f.boxes, box.Name)
	return nil
}

func (f *fakeSetupStarter) started() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.boxes...)
}

// newCtlStackBuild is the stack a build actually needs: the secrets store for
// the composition, a template-binding store so a capture has somewhere to point
// the tag, and a stand-in for the guest nudge. The environment store is opened
// here rather than taken from the fixture so the test can put a row into states
// only a guest's report reaches — chiefly `failed`, which is the state whose
// rendering matters most and the one no mock driver can produce.
func newCtlStackBuild(t *testing.T) (*ctlStack, *envs.Store, *fakeSetupStarter) {
	t.Helper()
	dir := t.TempDir()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	sec, err := secrets.Open(filepath.Join(dir, "secrets.db"),
		secrets.DeriveKEK([]byte("env-build-test-key-material")), log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sec.Close() }) //nolint:errcheck
	envStore, err := envs.Open(filepath.Join(dir, "envs.db"), log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { envStore.Close() }) //nolint:errcheck
	tmpl, err := templates.Open(filepath.Join(dir, "templates.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tmpl.Close() }) //nolint:errcheck
	starter := &fakeSetupStarter{}
	st := newCtlStackWith(t, testRoster(), func(cfg *ctlops.Config) {
		cfg.Secrets, cfg.SecretTags, cfg.EnvVars, cfg.Tags = sec, sec, sec, sec
		cfg.Environments = envStore
		cfg.TemplateTags = tmpl
		cfg.SetupStarter = starter
		// RepoFiles is deliberately nil: this host has no GitHub App, which is
		// the commonest shape there is, and it is what makes the "no setup
		// script" refusal below reachable at all.
	})
	return st, envStore, starter
}

// TestControlEnvBuildStartsAndSaysSo walks the verb somebody actually types:
// a build refused for the one reason it usually is, the script piped in, the
// build started, and the two things the announcement has to say — the builder's
// name and that this keeps running without them.
func TestControlEnvBuildStartsAndSaysSo(t *testing.T) {
	st, _, starter := newCtlStackBuild(t)

	if s := st.run(t, "alice", "env", "create", "web"); s.code != 0 {
		t.Fatalf("create: exit %d (%s)", s.code, s.stderr.String())
	}

	// Nothing to run and no agent credential either, so the refusal is the
	// agent path's precondition — and it still has to TEACH: the condition is
	// in the message and the ways out are in the hint, which the ctl channel
	// would drop entirely without failEnvBuild.
	s := st.run(t, "alice", "env", "build", "web")
	if s.code != 1 {
		t.Fatalf("build with no script: exit %d, want 1 (%s)", s.code, s.stderr.String())
	}
	for _, want := range []string{
		"has no setup script",
		".sparkbox/setup.sh",
		"CLAUDE_CODE_OAUTH_TOKEN",
	} {
		if !strings.Contains(s.stderr.String(), want) {
			t.Errorf("the refusal never says %q:\n%s", want, s.stderr.String())
		}
	}
	if len(starter.started()) != 0 {
		t.Fatalf("a build with no script started a guest: %v", starter.started())
	}
	if _, ok := st.mgr.Get("web-build"); ok {
		t.Fatal("a refused build left a builder sandbox behind")
	}

	// The script, on stdin, as a file and never as an argument.
	sess := st.newSession("alice")
	sess.cmd = []string{"env", "script", "web", "--set"}
	sess.in = strings.NewReader("#!/usr/bin/env bash\nnpm ci\n")
	st.gw.handleControl(sess, "alice", st.log)
	if sess.code != 0 {
		t.Fatalf("script --set: exit %d (%s)", sess.code, sess.stderr.String())
	}

	s = st.run(t, "alice", "env", "build", "web")
	if s.code != 0 {
		t.Fatalf("build: exit %d (%s)", s.code, s.stderr.String())
	}
	want := "building web in web-build. this takes minutes and keeps going if you disconnect.\r\n" +
		"  watch it      ssh ctl@hivemind.tools env show web\r\n" +
		"  look inside   ssh web-build@hivemind.tools\r\n" +
		"when it succeeds, `ssh new@hivemind.tools -- --env web` boots from the disk it built.\r\n"
	if s.out.String() != want {
		t.Errorf("build printed\n%q\nwant\n%q", s.out.String(), want)
	}
	if got := starter.started(); len(got) != 1 || got[0] != "web-build" {
		t.Fatalf("the guest nudge went to %v, want [web-build]", got)
	}
	if _, ok := st.mgr.Get("web-build"); !ok {
		t.Fatal("the build reported success without creating its builder")
	}

	// `show` while it runs: the state, the box, and how to get inside it. This
	// is the page the announcement above sends people to.
	s = st.run(t, "alice", "env", "show", "web")
	for _, want := range []string{
		"web                  building\r\n",
		"  building in  web-build — running the setup script; this takes minutes\r\n",
		"               ssh web-build@hivemind.tools\r\n",
	} {
		if !strings.Contains(s.out.String(), want) {
			t.Errorf("show is missing %q:\n%s", want, s.out.String())
		}
	}

	// A second build while the first is in flight is a conflict, not a second
	// builder — and the sentence answers the question that was actually being
	// asked, which is where the first one got to.
	s = st.run(t, "alice", "env", "build", "web")
	if s.code != 1 || !strings.Contains(s.stderr.String(), "already building (in web-build)") {
		t.Errorf("a re-run during a build printed %q (exit %d)", s.stderr.String(), s.code)
	}
	if got := starter.started(); len(got) != 1 {
		t.Errorf("the re-run nudged the guest again: %v", got)
	}
}

// TestControlEnvCaptureFinishesByHand is the recovery path end to end: a build
// that failed, the rendering that tells somebody how to rescue it, and the
// capture that adopts the disk they fixed.
//
// The `failed` state is written directly because nothing in this tree can
// produce it honestly — it arrives from a guest reporting a non-zero exit over
// the metadata service, and there is no guest here. What is under test is the
// half that is ours: what a person reads, and what `env capture` then does.
func TestControlEnvCaptureFinishesByHand(t *testing.T) {
	st, envStore, _ := newCtlStackBuild(t)

	if s := st.run(t, "alice", "env", "create", "web"); s.code != 0 {
		t.Fatalf("create: exit %d (%s)", s.code, s.stderr.String())
	}
	if _, err := st.mgr.Create(context.Background(), "web-build", "alice", "ubuntu", 1, 512); err != nil {
		t.Fatal(err)
	}
	const reason = "the setup script exited 1: E: Unable to locate package libpq-dev"
	if err := envStore.SetState("alice", "web", envs.StateFailed, "web-build", reason); err != nil {
		t.Fatal(err)
	}

	s := st.run(t, "alice", "env", "show", "web")
	for _, want := range []string{
		"  build error  " + reason + "\r\n",
		"  builder      web-build — kept and paused, with the half-built disk in it\r\n",
		"               fix it by hand:     ssh web-build@hivemind.tools\r\n",
		"               keep what you fix:  ssh ctl@hivemind.tools env capture web\r\n",
	} {
		if !strings.Contains(s.out.String(), want) {
			t.Errorf("show is missing %q:\n%s", want, s.out.String())
		}
	}

	// And the verb that page points at. It announces before it blocks, because
	// a capture pauses a VM and packs a disk with the person watching.
	s = st.run(t, "alice", "env", "capture", "web")
	if s.code != 0 {
		t.Fatalf("capture: exit %d (%s)", s.code, s.stderr.String())
	}
	if !strings.HasPrefix(s.out.String(), "capturing web-build (pause + pack; this takes minutes)…\r\n") {
		t.Errorf("capture blocked without saying it would:\n%s", s.out.String())
	}
	for _, want := range []string{
		"web is ready — captured to ",
		"the builder is gone; sandboxes boot that disk now:\r\n",
		"  ssh new@hivemind.tools -- --env web\r\n",
	} {
		if !strings.Contains(s.out.String(), want) {
			t.Errorf("capture is missing %q:\n%s", want, s.out.String())
		}
	}
	if _, ok := st.mgr.Get("web-build"); ok {
		t.Error("the builder outlived a successful capture")
	}

	// The environment now boots from a disk, which is the whole point, and
	// `show` says which one.
	e, err := envStore.Get("alice", "web")
	if err != nil {
		t.Fatal(err)
	}
	if e.State != envs.StateReady || e.BuildBox != "" || e.BuildError != "" {
		t.Fatalf("after a capture the row is %+v", e)
	}
	s = st.run(t, "alice", "env", "show", "web")
	if strings.Contains(s.out.String(), "none — sandboxes on this tag boot the stock image") {
		t.Errorf("a ready environment still boots the stock image:\n%s", s.out.String())
	}
}

// TestControlEnvCaptureWithNoBuilder: the environment is real, the builder is
// not. A refusal, and one that names both ways forward rather than only the
// state it is in.
func TestControlEnvCaptureWithNoBuilder(t *testing.T) {
	st, _, _ := newCtlStackBuild(t)
	if s := st.run(t, "alice", "env", "create", "web"); s.code != 0 {
		t.Fatalf("create: exit %d (%s)", s.code, s.stderr.String())
	}
	s := st.run(t, "alice", "env", "capture", "web")
	if s.code != 1 {
		t.Fatalf("capture with no builder: exit %d, want 1", s.code)
	}
	if s.out.Len() != 0 {
		t.Errorf("a refused capture announced work it never did: %q", s.out.String())
	}
	for _, want := range []string{"has no builder sandbox", "env build web"} {
		if !strings.Contains(s.stderr.String(), want) {
			t.Errorf("the refusal never says %q:\n%s", want, s.stderr.String())
		}
	}
}

// TestControlEnvBuildIsMasked: a stranger's `env build` and `env capture` on an
// environment they do not own must read exactly like one that was never
// created. Both refusals come from the owner-scoped store read, and this is the
// assertion that keeps a future short-circuit from confirming the name.
func TestControlEnvBuildIsMasked(t *testing.T) {
	st, _, starter := newCtlStackBuild(t)
	if s := st.run(t, "alice", "env", "create", "web"); s.code != 0 {
		t.Fatalf("create: exit %d (%s)", s.code, s.stderr.String())
	}
	const want = "sparkbox: no environment named \"web\"\r\n"
	for _, args := range [][]string{{"env", "build", "web"}, {"env", "capture", "web"}} {
		s := st.run(t, "mallory", args...)
		if s.stderr.String() != want || s.code != 1 {
			t.Errorf("%v as a stranger: %q exit %d, want %q exit 1",
				args, s.stderr.String(), s.code, want)
		}
	}
	if got := starter.started(); len(got) != 0 {
		t.Errorf("a stranger's build reached a guest: %v", got)
	}
}

// TestControlEnvBuildWithoutAGuestNudge is the degraded host, asserted: a
// control plane with no way to start a setup run refuses the build rather than
// creating a builder sandbox that would sit there forever with nothing to do.
func TestControlEnvBuildWithoutAGuestNudge(t *testing.T) {
	dir := t.TempDir()
	tmpl, err := templates.Open(filepath.Join(dir, "templates.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tmpl.Close() }) //nolint:errcheck
	st := newCtlStackWith(t, testRoster(), func(cfg *ctlops.Config) {
		cfg.TemplateTags = tmpl
		cfg.SetupStarter = nil
	})
	if s := st.run(t, "alice", "env", "create", "web"); s.code != 0 {
		t.Fatalf("create: exit %d (%s)", s.code, s.stderr.String())
	}
	sess := st.newSession("alice")
	sess.cmd = []string{"env", "script", "web", "--set"}
	sess.in = strings.NewReader("echo hi\n")
	st.gw.handleControl(sess, "alice", st.log)
	if sess.code != 0 {
		t.Fatalf("script --set: exit %d (%s)", sess.code, sess.stderr.String())
	}
	s := st.run(t, "alice", "env", "build", "web")
	if s.code == 0 {
		t.Fatalf("a host that cannot nudge a guest started a build anyway:\n%s", s.out.String())
	}
	if !strings.Contains(s.stderr.String(), "not enabled on this host") {
		t.Errorf("the refusal reads %q", s.stderr.String())
	}
}
