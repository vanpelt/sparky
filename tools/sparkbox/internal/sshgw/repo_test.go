package sshgw

// Two things are pinned here: the `repo add` flag grammar, and the sentences
// the channel prints once a repo store exists — which the golden table cannot
// hold, because its stack deliberately has none.
//
// The sentence that matters most is the `default` note. An untagged attachment
// clones into every sandbox the user makes from then on, and the design says
// that warning belongs at attach time rather than in a document. A test is the
// only thing that keeps a warning from being tidied away later by someone who
// reads it as noise.

import (
	"context"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/ghapp"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/repos"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/users"
)

// ---------------------------------------------------------------------------
// The parser
// ---------------------------------------------------------------------------

func TestParseRepoAdd(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want ctlops.RepoArgs
	}{{
		name: "just a slug",
		args: []string{"wandb/hivemind"},
		want: ctlops.RepoArgs{Slug: "wandb/hivemind"},
	}, {
		name: "tags in both spellings accumulate",
		args: []string{"wandb/hivemind", "--tag", "hm", "-t=web"},
		want: ctlops.RepoArgs{Slug: "wandb/hivemind", Tags: []string{"hm", "web"}},
	}, {
		// Splitting here would be a second copy of a rule ctlops.NormalizeTags
		// already owns, and two copies is two chances to disagree.
		name: "a comma list stays whole for NormalizeTags to split",
		args: []string{"wandb/hivemind", "--tag", "hm,web"},
		want: ctlops.RepoArgs{Slug: "wandb/hivemind", Tags: []string{"hm", "web"}},
	}, {
		name: "the flags after the slug",
		args: []string{"wandb/hivemind", "--write", "--ref", "main", "--path", "src/hm"},
		want: ctlops.RepoArgs{Slug: "wandb/hivemind", Write: true, Ref: "main", Path: "src/hm"},
	}, {
		name: "the attached spellings",
		args: []string{"--ref=main", "--path=src/hm", "wandb/hivemind"},
		want: ctlops.RepoArgs{Slug: "wandb/hivemind", Ref: "main", Path: "src/hm"},
	}, {
		// Repeating a flag that names one thing is a correction, not a list —
		// splitNodeFlag's rule, applied to the flags that behave like it.
		name: "the last --ref wins",
		args: []string{"wandb/hivemind", "--ref", "main", "--ref", "dev"},
		want: ctlops.RepoArgs{Slug: "wandb/hivemind", Ref: "dev"},
	}, {
		name: "--branch is --ref",
		args: []string{"wandb/hivemind", "--branch", "dev"},
		want: ctlops.RepoArgs{Slug: "wandb/hivemind", Ref: "dev"},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseRepoAdd(tc.args)
			if err != nil {
				t.Fatalf("parseRepoAdd(%v) = %v", tc.args, err)
			}
			if got.Slug != tc.want.Slug || got.Ref != tc.want.Ref || got.Path != tc.want.Path ||
				got.Write != tc.want.Write || strings.Join(got.Tags, ",") != strings.Join(tc.want.Tags, ",") {
				t.Errorf("parseRepoAdd(%v) = %+v, want %+v", tc.args, got, tc.want)
			}
		})
	}
}

func TestParseRepoAddRefusals(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string // a substring of the sentence
	}{
		{"nothing at all", nil, "name the repository"},
		{"only flags", []string{"--write"}, "name the repository"},
		{"a dangling --ref", []string{"wandb/hivemind", "--ref"}, "--ref needs a value"},
		{"an empty --path", []string{"wandb/hivemind", "--path="}, "--path needs a value"},
		{"a flag nobody has", []string{"wandb/hivemind", "--depth", "1"}, `unknown flag "--depth"`},
		{"--write with a value", []string{"wandb/hivemind", "--write=yes"}, "--write takes no value"},
		// The likeliest cause is a forgotten --tag, and taking the first while
		// dropping the second would be the worst available reading.
		{"two repositories", []string{"wandb/hivemind", "wandb/sparky"}, "one repository at a time"},
		{"a dangling --tag", []string{"wandb/hivemind", "--tag"}, "--tag needs a value"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseRepoAdd(tc.args)
			if err == nil {
				t.Fatalf("parseRepoAdd(%v) was accepted", tc.args)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("parseRepoAdd(%v) = %q, want it to mention %q", tc.args, err, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// The channel, on a host that has the stores
// ---------------------------------------------------------------------------

// fakeRepoStore is the attachment store in a map. The gateway's own tests never
// open a repo store, so this is also what proves ctlops.Repos is satisfiable by
// something other than *repos.Store — the reason it is an interface.
type fakeRepoStore struct {
	rows  map[string]repos.Repo
	boxes map[string][]string // sandbox -> tags
}

func (f *fakeRepoStore) PutRepo(owner string, r repos.Repo, tags []string) error {
	r.Owner, r.Tags, r.Host = owner, tags, "github.com"
	r.CreatedAt = time.Unix(0, 0).UTC()
	f.rows[owner+"/"+strings.ToLower(r.Slug)] = r
	return nil
}

func (f *fakeRepoStore) DeleteRepo(owner, host, slug string) error {
	k := owner + "/" + strings.ToLower(slug)
	if _, ok := f.rows[k]; !ok {
		return repos.ErrNoSuchRepo
	}
	delete(f.rows, k)
	return nil
}

func (f *fakeRepoStore) ListRepos(owner string) ([]repos.Repo, error) {
	var out []repos.Repo
	for _, r := range f.rows {
		if r.Owner == owner {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Slug < out[j].Slug })
	return out, nil
}

func (f *fakeRepoStore) SandboxesForRepo(owner, host, slug string) ([]string, error) {
	r, ok := f.rows[owner+"/"+strings.ToLower(slug)]
	if !ok {
		return nil, nil
	}
	want := map[string]bool{}
	for _, t := range r.Tags {
		want[t] = true
	}
	var out []string
	for box, tags := range f.boxes {
		for _, t := range tags {
			if want[t] {
				out = append(out, box)
				break
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

// fakeGitHubApp reports every repository under `wandb` as installed and
// everything else as not, which is the two answers the ctl surface renders
// differently.
type fakeGitHubApp struct{}

func (fakeGitHubApp) InstallationFor(ctx context.Context, owner, name string) (ghapp.Installation, error) {
	if !strings.EqualFold(owner, "wandb") {
		return ghapp.Installation{}, ghapp.ErrNotInstalled
	}
	return ghapp.Installation{ID: 42, AccountID: 7, AccountLogin: "wandb", AccountType: "Organization"}, nil
}

func (fakeGitHubApp) Authorize(ctx context.Context, inst ghapp.Installation, githubID int64, githubLogin string) error {
	return nil
}

func (fakeGitHubApp) InstallURL() string { return "https://github.com/apps/sparkbox/installations/new" }

// newRepoStack is the ctl stack with the two stores this file is about, wired
// through the same window main has: each tweak gets the config the gateway
// would have built for itself, before the Ops is constructed from it.
func newRepoStack(t *testing.T) (*ctlStack, *fakeRepoStore) {
	t.Helper()
	store := &fakeRepoStore{rows: map[string]repos.Repo{}, boxes: map[string][]string{}}
	st := newCtlStackWith(t, testRoster(), func(cfg *ctlops.Config) {
		cfg.Repos = store
		cfg.GitHubApp = fakeGitHubApp{}
	})
	return st, store
}

// TestControlRepoDisabled: the default stack has no repo store, so every repo
// command answers with a statement about the HOST — including the ones whose
// arguments are wrong, so the user learns what this host is rather than what
// they typed.
func TestControlRepoOnAHostWithoutARepoStore(t *testing.T) {
	st := newCtlStack(t)
	const want = "sparkbox: repo attachments are not enabled on this host\r\n"
	for _, args := range [][]string{
		{"repo"}, {"repo", "ls"}, {"repo", "add", "wandb/hivemind"}, {"repo", "rm"}, {"repo", "check"}, {"repo", "wat"},
	} {
		s := st.run(t, "alice", args...)
		if s.stderr.String() != want || s.code != 1 {
			t.Errorf("%v = exit %d, stderr %q; want exit 1 and %q", args, s.code, s.stderr.String(), want)
		}
	}
	// And the App half is its own bit: no key on this host, so no URL to print.
	s := st.run(t, "alice", "github", "install")
	if got, want := s.stderr.String(), "sparkbox: no GitHub App is configured on this host\r\n"; got != want || s.code != 1 {
		t.Errorf("github install = exit %d, stderr %q; want exit 1 and %q", s.code, got, want)
	}
}

func TestControlRepoGrammarOnAnEnabledHost(t *testing.T) {
	st, _ := newRepoStack(t)

	for _, tc := range []struct {
		name     string
		args     []string
		wantOut  string
		wantErr  string
		wantExit int
	}{{
		name: "no subcommand prints the usage", args: []string{"repo"},
		wantErr: repoUsage, wantExit: 2,
	}, {
		name: "an unknown subcommand names it", args: []string{"repo", "wat"},
		wantErr: "unknown repo command \"wat\"\r\n" + repoUsage, wantExit: 2,
	}, {
		name: "rm with nothing to remove", args: []string{"repo", "rm"},
		wantErr: repoUsage, wantExit: 2,
	}, {
		name: "ls with nothing attached", args: []string{"repo", "ls"},
		wantOut: "no repos attached — attach one with:\r\n" +
			"  ssh ctl@hivemind.tools repo add wandb/hivemind --tag hm\r\n",
		wantExit: 0,
	}, {
		name: "check with nothing attached", args: []string{"repo", "check"},
		wantOut: "no repos attached — attach one with:\r\n" +
			"  ssh ctl@hivemind.tools repo add wandb/hivemind --tag hm\r\n",
		wantExit: 0,
	}, {
		name: "add with nothing to add", args: []string{"repo", "add"},
		wantErr: "sparkbox: name the repository to attach, as <owner>/<name>\r\n" + repoUsage, wantExit: 2,
	}, {
		// The gate: alice has no GitHub link on this stack, so nothing is
		// attached and the sentence says how to get one.
		name: "add without a GitHub link", args: []string{"repo", "add", "wandb/hivemind"},
		wantErr: "sparkbox: no GitHub account linked, so there is nobody to clone this as\r\n", wantExit: 1,
	}, {
		name: "rm of something not attached", args: []string{"repo", "rm", "wandb/hivemind"},
		wantErr: "sparkbox: no repo \"wandb/hivemind\"\r\n", wantExit: 1,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			s := st.run(t, "alice", tc.args...)
			if got := s.out.String(); got != tc.wantOut {
				t.Errorf("stdout = %q, want %q", got, tc.wantOut)
			}
			if got := s.stderr.String(); got != tc.wantErr {
				t.Errorf("stderr = %q, want %q", got, tc.wantErr)
			}
			if s.code != tc.wantExit {
				t.Errorf("exit = %d, want %d", s.code, tc.wantExit)
			}
			if !s.exited {
				t.Error("command never called Exit; the ssh client would hang")
			}
			for stream, text := range map[string]string{"stdout": s.out.String(), "stderr": s.stderr.String()} {
				if strings.Contains(strings.ReplaceAll(text, "\r\n", ""), "\n") {
					t.Errorf("%s contains a bare \\n: %q", stream, text)
				}
			}
		})
	}
}

// An `assertion` link is a third party's word for who this is. It may not
// attach a repo, for the same reason it may not adopt a key — and the refusal
// has to say which of the two problems it is, or the user re-runs `github link`
// and gets the same answer.
func TestControlRepoAddRefusesAWeakLink(t *testing.T) {
	st, _ := newRepoStack(t)
	if err := st.users.LinkGitHub("alice", "alice-gh", users.GitHubViaAssertion, 99); err != nil {
		t.Fatal(err)
	}
	s := st.run(t, "alice", "repo", "add", "wandb/hivemind")
	want := "sparkbox: the link to github.com/alice-gh was not proved directly with GitHub, " +
		"so it cannot attach repos\r\n"
	if s.stderr.String() != want || s.code != 1 {
		t.Fatalf("add = exit %d, stderr %q; want exit 1 and %q", s.code, s.stderr.String(), want)
	}
}

// The happy path, and the warning that has to come with it. Mutating, so it
// runs on its own stack.
func TestControlRepoAddListAndDetach(t *testing.T) {
	st, store := newRepoStack(t)
	if err := st.users.LinkGitHub("alice", "alice-gh", users.GitHubViaKeys, 99); err != nil {
		t.Fatal(err)
	}
	store.boxes["alicebox"] = []string{"hm"}

	// Untagged: it lands on `default`, and the note says what that means. This
	// is docs/github-repos-design.md §2.2's requirement, at the only moment
	// anybody is reading.
	s := st.run(t, "alice", "repo", "add", "wandb/hivemind")
	if s.code != 0 {
		t.Fatalf("add = exit %d, stderr %q", s.code, s.stderr.String())
	}
	want := "attached wandb/hivemind  (tags: default; read) — no sandbox of yours carries that tag yet\r\n" +
		"note: with no --tag this went on the `default` tag, which every new sandbox of\r\n" +
		"      yours carries — so every sandbox you create from now on will clone it.\r\n" +
		"      narrow it with `repo add wandb/hivemind --tag <t>`, or undo it with " +
		"`repo rm wandb/hivemind`.\r\n"
	if s.out.String() != want {
		t.Fatalf("add printed %q,\nwant %q", s.out.String(), want)
	}

	// Re-attaching with a tag replaces the tag set wholesale, so the warning
	// stops and the fan-out appears.
	s = st.run(t, "alice", "repo", "add", "wandb/hivemind", "--tag", "hm", "--ref", "main")
	if s.code != 0 || !strings.HasPrefix(s.out.String(),
		"attached wandb/hivemind  (tags: hm; read) — alicebox carries that tag\r\n") {
		t.Fatalf("re-attach = exit %d, stdout %q", s.code, s.out.String())
	}
	if !strings.Contains(s.out.String(), "existing sandboxes are not cloned into") {
		t.Error("nothing said that a running sandbox is not cloned into — see §2.2")
	}

	s = st.run(t, "alice", "repo", "ls")
	if got := s.out.String(); !strings.Contains(got, "wandb/hivemind") || !strings.Contains(got, "@main") {
		t.Errorf("ls printed %q", got)
	}

	s = st.run(t, "alice", "repo", "check")
	if got, want := s.out.String(), "wandb/hivemind                           ok           \r\n"; got != want {
		t.Errorf("check printed %q, want %q", got, want)
	}

	// A repository the App is not installed on attaches anyway, and says so
	// with the way out — the failure the design says is otherwise invisible
	// until a clone fails inside a VM at boot.
	s = st.run(t, "alice", "repo", "add", "someoneelse/private", "--tag", "hm")
	if s.code != 0 {
		t.Fatalf("add of an uninstalled repo = exit %d, stderr %q", s.code, s.stderr.String())
	}
	if !strings.Contains(s.out.String(), "attached someoneelse/private") {
		t.Errorf("the attachment was not reported: %q", s.out.String())
	}
	if !strings.Contains(s.out.String(), "https://github.com/apps/sparkbox/installations/new") {
		t.Errorf("nothing printed the install URL: %q", s.out.String())
	}

	// Detaching names the sandbox that already has a clone, and says it is left
	// alone: a checkout is a directory somebody may be working in.
	s = st.run(t, "alice", "repo", "rm", "wandb/hivemind")
	if s.code != 0 || !strings.HasPrefix(s.out.String(), "detached wandb/hivemind\r\n") {
		t.Fatalf("rm = exit %d, stdout %q, stderr %q", s.code, s.out.String(), s.stderr.String())
	}
	if !strings.Contains(s.out.String(), "alicebox already has a clone of it") {
		t.Errorf("rm printed %q", s.out.String())
	}
	if _, still := store.rows["alice/wandb/hivemind"]; still {
		t.Error("the row is still in the store")
	}

	for _, s := range []*ctlSession{s} {
		for stream, text := range map[string]string{"stdout": s.out.String(), "stderr": s.stderr.String()} {
			if strings.Contains(strings.ReplaceAll(text, "\r\n", ""), "\n") {
				t.Errorf("%s contains a bare \\n: %q", stream, text)
			}
		}
	}
}

// `github install` prints the URL of the App this host actually holds, plus the
// next command — the two halves somebody needs in one screen.
func TestControlGitHubInstall(t *testing.T) {
	st, _ := newRepoStack(t)
	s := st.run(t, "alice", "github", "install")
	want := "install the GitHub App on the repositories you want in your sandboxes:\r\n" +
		"  https://github.com/apps/sparkbox/installations/new\r\n" +
		"then attach one:  ssh ctl@hivemind.tools repo add <owner>/<name> --tag <t>\r\n"
	if s.out.String() != want || s.code != 0 {
		t.Fatalf("github install = exit %d, stdout %q, want %q", s.code, s.out.String(), want)
	}
}
