package sshgw

// The `env` verbs through the door. The golden table next door pins the empty
// cases — create, show, ls, rm on an environment that composes nothing — so
// what is here is the two things that need more of a host than that fixture
// has: a composition with real secrets and variables in it, and the setup
// script, which is the one payload on this channel that arrives on stdin.

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/secrets"
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
