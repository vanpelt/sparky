package ctlops

// What these pin is the half of `repo add` that is not a database write: the
// gate on the GitHub link, the default tag and the sentence that has to come
// with it, and the rule that a repository the App cannot see yet is still
// recorded. Every one of those is a decision this package makes and no store
// can make for it.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ghapp"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/repos"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/secrets"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/users"
)

// The real store and the real App must keep satisfying these structurally, for
// the reason the block in fakes_test.go states: a signature drift should fail
// this package's tests rather than the integrator's build.
var (
	_ Repos     = (*repos.Store)(nil)
	_ GitHubApp = (*ghapp.App)(nil)
)

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

// fakeRepos is the attachment store in a map. Like fakeSecrets it resolves the
// fan-out through the tagger, because "which of my sandboxes does this reach"
// is a join in the real store and a test that hard-coded the answer would prove
// nothing about the tag being what connects the two.
type fakeRepos struct {
	c     *calls
	rows  map[string]repos.Repo // "owner\x00host\x00slug" -> row
	tags  map[string][]string   // same key -> tags
	boxes *fakeTagger
	err   error
	// putErr fails only the write, so a test can prove nothing is reported as
	// attached when the row never landed.
	putErr error
}

func repoKey(owner, host, slug string) string {
	if host == "" {
		host = "github.com"
	}
	return owner + "\x00" + strings.ToLower(host) + "\x00" + strings.ToLower(slug)
}

func (f *fakeRepos) PutRepo(owner string, r repos.Repo, tags []string) error {
	f.c.add("repos.Put %s/%s tags=%v access=%s", owner, r.Slug, tags, r.Access)
	if f.putErr != nil {
		return f.putErr
	}
	if r.Host == "" {
		r.Host = "github.com"
	}
	r.Owner, r.Tags = owner, tags
	if r.CreatedAt.IsZero() {
		r.CreatedAt = time.Unix(0, 0).UTC()
	}
	k := repoKey(owner, r.Host, r.Slug)
	f.rows[k], f.tags[k] = r, tags
	return nil
}

func (f *fakeRepos) DeleteRepo(owner, host, slug string) error {
	f.c.add("repos.Delete %s/%s", owner, slug)
	if f.err != nil {
		return f.err
	}
	k := repoKey(owner, host, slug)
	if _, ok := f.rows[k]; !ok {
		return repos.ErrNoSuchRepo
	}
	delete(f.rows, k)
	delete(f.tags, k)
	return nil
}

func (f *fakeRepos) ListRepos(owner string) ([]repos.Repo, error) {
	f.c.add("repos.List %s", owner)
	if f.err != nil {
		return nil, f.err
	}
	var out []repos.Repo
	for _, r := range f.rows {
		if r.Owner == owner {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Slug < out[j].Slug })
	return out, nil
}

func (f *fakeRepos) SandboxesForRepo(owner, host, slug string) ([]string, error) {
	f.c.add("repos.SandboxesFor %s/%s", owner, slug)
	if f.err != nil {
		return nil, f.err
	}
	want := map[string]bool{}
	for _, t := range f.tags[repoKey(owner, host, slug)] {
		want[t] = true
	}
	var out []string
	for box, tags := range f.boxes.tags {
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

// fakeApp answers the two questions ctlops asks of the App. installed names the
// slugs it covers; refuse and upstream turn one probe into each of the two
// failures that are rendered differently.
type fakeApp struct {
	c         *calls
	installed map[string]ghapp.Installation
	refuse    error // Authorize's answer when non-nil
	upstream  bool  // github.com does not answer
}

func (f *fakeApp) InstallationFor(ctx context.Context, owner, name string) (ghapp.Installation, error) {
	f.c.add("app.InstallationFor %s/%s", owner, name)
	if f.upstream {
		return ghapp.Installation{}, ghapp.ErrUpstream
	}
	inst, ok := f.installed[strings.ToLower(owner+"/"+name)]
	if !ok {
		return ghapp.Installation{}, ghapp.ErrNotInstalled
	}
	return inst, nil
}

func (f *fakeApp) Authorize(ctx context.Context, inst ghapp.Installation, githubID int64, githubLogin string) error {
	f.c.add("app.Authorize %d/%s", githubID, githubLogin)
	return f.refuse
}

func (f *fakeApp) InstallURL() string { return "https://github.com/apps/sparkbox/installations/new" }

// withRepos gives a rig the two optional stores this file is about. It sets the
// unexported fields directly, which is this package's own idiom for reshaping a
// host after New (errors_test.go nils stores the same way) and keeps the shared
// fakes_test.go rig untouched.
func withRepos(r *rig) (*fakeRepos, *fakeApp) {
	rp := &fakeRepos{c: r.calls, rows: map[string]repos.Repo{}, tags: map[string][]string{}, boxes: r.tagger}
	app := &fakeApp{c: r.calls, installed: map[string]ghapp.Installation{
		"wandb/hivemind": {ID: 42, AccountID: 7, AccountLogin: "wandb", AccountType: "Organization"},
	}}
	r.ops.repos, r.ops.ghApp = rp, app
	return rp, app
}

// linkGitHub gives a handle the link a repo attach requires. via is the whole
// point of the helper: the difference between a strong link and an assertion is
// what the gate turns on.
func linkGitHub(r *rig, handle, login, via string, id int64) {
	u := r.accts.users[handle]
	at := time.Unix(0, 0).UTC()
	u.GitHubLogin, u.GitHubVia, u.GitHubID, u.GitHubVerifiedAt = login, via, id, &at
	r.accts.users[handle] = u
}

// ---------------------------------------------------------------------------
// Attach
// ---------------------------------------------------------------------------

// The two halves of the default have to meet — an untagged repo and an untagged
// new sandbox — and the caller has to be TOLD that is what happened, because
// the consequence is a clone in every sandbox they make from now on.
func TestAttachRepoDefaultsToTheDefaultTagAndReportsIt(t *testing.T) {
	r := newRig(t)
	rp, _ := withRepos(r)
	linkGitHub(r, "alice", "alice-gh", users.GitHubViaKeys, 99)

	res, err := r.ops.AttachRepo(context.Background(), alice(), RepoArgs{Slug: "wandb/hivemind"})
	if err != nil {
		t.Fatalf("AttachRepo: %v", err)
	}
	if !res.Defaulted {
		t.Error("an untagged attach did not report that it used the default tag")
	}
	if len(res.Repo.Tags) != 1 || res.Repo.Tags[0] != secrets.DefaultTag {
		t.Fatalf("stored tags %v, want [%s]", res.Repo.Tags, secrets.DefaultTag)
	}
	// The store is where it has to have landed: unlike a secret, the store does
	// not stamp the default itself, so a result-only default would attach the
	// repository to nothing at all.
	if got := rp.tags[repoKey("alice", "github.com", "wandb/hivemind")]; len(got) != 1 || got[0] != secrets.DefaultTag {
		t.Fatalf("the row was written with tags %v, want [%s]", got, secrets.DefaultTag)
	}
	if res.Repo.Access != repos.AccessRead {
		t.Errorf("access %q, want %q — write is a thing you ask for", res.Repo.Access, repos.AccessRead)
	}
}

// An explicit tag is not defaulted, and --write is carried through.
func TestAttachRepoWithTagsAndWrite(t *testing.T) {
	r := newRig(t)
	withRepos(r)
	linkGitHub(r, "alice", "alice-gh", users.GitHubViaKeys, 99)
	mustNoErr(t, r.tagger.SetTags("alicebox", "alice", []string{"hm"}))

	res, err := r.ops.AttachRepo(context.Background(), alice(),
		RepoArgs{Slug: "wandb/hivemind", Tags: []string{"HM,web"}, Write: true, Ref: "main"})
	if err != nil {
		t.Fatalf("AttachRepo: %v", err)
	}
	if res.Defaulted {
		t.Error("a tagged attach reported the default tag")
	}
	// NormalizeTags did the splitting and the folding, not a copy of it here.
	if strings.Join(res.Repo.Tags, ",") != "hm,web" {
		t.Errorf("tags %v, want [hm web]", res.Repo.Tags)
	}
	if res.Repo.Access != repos.AccessWrite || res.Repo.Ref != "main" {
		t.Errorf("stored %+v, want write access on main", res.Repo)
	}
	// The fan-out is what tells the user the attachment reaches something.
	if len(res.Sandboxes) != 1 || res.Sandboxes[0] != "alicebox" {
		t.Errorf("sandboxes %v, want [alicebox]", res.Sandboxes)
	}
}

// A repository nothing carries still attaches, and says it reaches nothing —
// the tag mistake this feature invites, caught while the user is still looking.
func TestAttachRepoReportsWhenNothingSelectsIt(t *testing.T) {
	r := newRig(t)
	withRepos(r)
	linkGitHub(r, "alice", "alice-gh", users.GitHubViaKeys, 99)

	res, err := r.ops.AttachRepo(context.Background(), alice(),
		RepoArgs{Slug: "wandb/hivemind", Tags: []string{"nobody-has-this"}})
	if err != nil {
		t.Fatalf("AttachRepo: %v", err)
	}
	if res.Sandboxes == nil || len(res.Sandboxes) != 0 {
		t.Fatalf("sandboxes %v, want an empty non-nil slice", res.Sandboxes)
	}
}

// The design's whole argument for the inline check: an attachment the App
// cannot see is RECORDED and REPORTED, never silently accepted and never
// refused. Refusing would leave the user with nothing stored and a URL to
// visit; silence leaves them with a clone that fails at boot in a log.
func TestAttachRepoRecordsAnAttachmentTheAppCannotReach(t *testing.T) {
	r := newRig(t)
	rp, app := withRepos(r)
	linkGitHub(r, "alice", "alice-gh", users.GitHubViaKeys, 99)

	res, err := r.ops.AttachRepo(context.Background(), alice(), RepoArgs{Slug: "wandb/secret-thing"})
	if err != nil {
		t.Fatalf("AttachRepo refused an uninstalled repo: %v", err)
	}
	if _, ok := rp.rows[repoKey("alice", "github.com", "wandb/secret-thing")]; !ok {
		t.Fatal("the attachment was not recorded")
	}
	if !res.Check.Checked || res.Check.Reachable {
		t.Fatalf("check %+v, want checked and unreachable", res.Check)
	}
	if res.Check.Reason == "" {
		t.Error("nothing told the user why it is unreachable")
	}
	if res.Check.InstallURL != app.InstallURL() {
		t.Errorf("install URL %q, want the App's — it is the way out of this state", res.Check.InstallURL)
	}
}

// github.com being down says nothing about the attachment, so the write stands
// and the check simply has no answer. A failure here would tell the user their
// repo did not save, which is false.
func TestAttachRepoSurvivesGitHubBeingDown(t *testing.T) {
	r := newRig(t)
	rp, app := withRepos(r)
	app.upstream = true
	linkGitHub(r, "alice", "alice-gh", users.GitHubViaKeys, 99)

	res, err := r.ops.AttachRepo(context.Background(), alice(), RepoArgs{Slug: "wandb/hivemind"})
	if err != nil {
		t.Fatalf("AttachRepo: %v", err)
	}
	if _, ok := rp.rows[repoKey("alice", "github.com", "wandb/hivemind")]; !ok {
		t.Fatal("the attachment was not recorded")
	}
	if res.Check.Reachable {
		t.Error("an unanswered probe reported the repo reachable")
	}
}

// A host with no App attaches without checking anything. Checked=false is the
// statement "this host has no App", which is different from "the App cannot
// reach this" and is fixed by a different person.
func TestAttachRepoOnAHostWithNoApp(t *testing.T) {
	r := newRig(t)
	withRepos(r)
	r.ops.ghApp = nil
	linkGitHub(r, "alice", "alice-gh", users.GitHubViaKeys, 99)

	res, err := r.ops.AttachRepo(context.Background(), alice(), RepoArgs{Slug: "wandb/hivemind"})
	if err != nil {
		t.Fatalf("AttachRepo: %v", err)
	}
	if res.Check.Checked || res.Check.Reachable {
		t.Errorf("check %+v, want an unchecked one", res.Check)
	}
}

// ---------------------------------------------------------------------------
// The gate
// ---------------------------------------------------------------------------

// §3.3's rule: an `assertion` link is a third party's word for who this is, and
// it must not reach a verb that grants access to source code — the same reason
// it may not adopt keys.
func TestAttachRepoRefusesAWeakGitHubLink(t *testing.T) {
	r := newRig(t)
	rp, _ := withRepos(r)
	linkGitHub(r, "alice", "alice-gh", users.GitHubViaAssertion, 99)

	_, err := r.ops.AttachRepo(context.Background(), alice(), RepoArgs{Slug: "wandb/hivemind"})
	if !IsKind(err, KindDenied) {
		t.Fatalf("AttachRepo with an assertion link = %v, want KindDenied", err)
	}
	var e *Error
	errors.As(err, &e)
	if e.Code != "github_link_too_weak" {
		t.Errorf("code %q, want github_link_too_weak", e.Code)
	}
	if len(rp.rows) != 0 {
		t.Error("a refused attach wrote a row anyway")
	}
	if r.calls.has("repos.Put alice/wandb/hivemind tags=[default] access=read") {
		t.Error("the store was reached at all")
	}
}

func TestAttachRepoRefusesAnAccountWithNoLink(t *testing.T) {
	r := newRig(t)
	rp, _ := withRepos(r)

	_, err := r.ops.AttachRepo(context.Background(), alice(), RepoArgs{Slug: "wandb/hivemind"})
	if !IsKind(err, KindConflict) {
		t.Fatalf("AttachRepo with no link = %v, want KindConflict", err)
	}
	if len(rp.rows) != 0 {
		t.Error("a refused attach wrote a row anyway")
	}
}

// A login with no verification timestamp is a half-written row, not a link.
func TestAttachRepoRefusesAnUnverifiedLink(t *testing.T) {
	r := newRig(t)
	withRepos(r)
	u := r.accts.users["alice"]
	u.GitHubLogin, u.GitHubVia = "alice-gh", users.GitHubViaKeys
	r.accts.users["alice"] = u

	_, err := r.ops.AttachRepo(context.Background(), alice(), RepoArgs{Slug: "wandb/hivemind"})
	if !IsKind(err, KindConflict) {
		t.Fatalf("AttachRepo with an unverified link = %v, want KindConflict", err)
	}
}

// A malformed slug is the caller's mistake: exit 2, not a server fault.
func TestAttachRepoRefusesSomethingThatIsNotASlug(t *testing.T) {
	r := newRig(t)
	withRepos(r)
	linkGitHub(r, "alice", "alice-gh", users.GitHubViaKeys, 99)

	for _, arg := range []string{"", "hivemind", "https://github.com/wandb/hivemind", "wandb/hivemind.git", "a/b/c"} {
		_, err := r.ops.AttachRepo(context.Background(), alice(), RepoArgs{Slug: arg})
		if !IsKind(err, KindInvalid) {
			t.Errorf("AttachRepo(%q) = %v, want KindInvalid", arg, err)
			continue
		}
		var e *Error
		errors.As(err, &e)
		if e.ExitCode() != 2 {
			t.Errorf("AttachRepo(%q) exits %d, want 2", arg, e.ExitCode())
		}
	}
}

// The store's own sentence about a ref it cannot take is more specific than
// anything this layer would say, so it prints bare rather than wrapped.
func TestAttachRepoPassesTheStoresRefusalThrough(t *testing.T) {
	r := newRig(t)
	rp, _ := withRepos(r)
	rp.putErr = errors.New("invalid repo: ref \"--upload-pack=x\" (want a branch or tag name, no leading '-')")
	linkGitHub(r, "alice", "alice-gh", users.GitHubViaKeys, 99)

	_, err := r.ops.AttachRepo(context.Background(), alice(),
		RepoArgs{Slug: "wandb/hivemind", Ref: "--upload-pack=x"})
	var e *Error
	if !errors.As(err, &e) || e.Kind != KindInvalid || !e.Verbatim {
		t.Fatalf("AttachRepo = %v, want a verbatim KindInvalid", err)
	}
	if !strings.Contains(e.Msg, "upload-pack") {
		t.Errorf("the store's sentence was reworded: %q", e.Msg)
	}
}

// ---------------------------------------------------------------------------
// Detach, list, check
// ---------------------------------------------------------------------------

// The fan-out has to be read before the row goes, or nothing can say which
// boxes were selecting it. Those clones are left where they are — this is the
// list of places to go looking, not a list of things that were deleted.
func TestDetachRepoNamesTheSandboxesItWasReaching(t *testing.T) {
	r := newRig(t)
	withRepos(r)
	linkGitHub(r, "alice", "alice-gh", users.GitHubViaKeys, 99)
	mustNoErr(t, r.tagger.SetTags("alicebox", "alice", []string{"hm"}))
	if _, err := r.ops.AttachRepo(context.Background(), alice(),
		RepoArgs{Slug: "wandb/hivemind", Tags: []string{"hm"}}); err != nil {
		t.Fatal(err)
	}

	affected, err := r.ops.DetachRepo(context.Background(), alice(), "", "wandb/hivemind")
	if err != nil {
		t.Fatalf("DetachRepo: %v", err)
	}
	if len(affected) != 1 || affected[0] != "alicebox" {
		t.Fatalf("affected %v, want [alicebox]", affected)
	}
	// Detaching is a manifest change, never a push: an existing checkout is a
	// directory somebody may be working in.
	if r.calls.has("ResyncEnv alicebox") {
		t.Error("detaching a repo pushed to a running sandbox")
	}
}

func TestDetachRepoThatIsNotAttached(t *testing.T) {
	r := newRig(t)
	withRepos(r)

	_, err := r.ops.DetachRepo(context.Background(), alice(), "", "wandb/hivemind")
	if !IsKind(err, KindNotFound) {
		t.Fatalf("DetachRepo of an unattached repo = %v, want KindNotFound", err)
	}
	if got := err.Error(); got != `no repo "wandb/hivemind"` {
		t.Errorf("message %q", got)
	}
}

// The masking invariant, at the only place a repo can be named: mallory's
// answer for alice's attachment is byte-identical to her answer for one that
// was never attached at all.
func TestReposAreScopedToTheCaller(t *testing.T) {
	r := newRig(t)
	withRepos(r)
	linkGitHub(r, "alice", "alice-gh", users.GitHubViaKeys, 99)
	linkGitHub(r, "mallory", "mallory-gh", users.GitHubViaKeys, 100)
	if _, err := r.ops.AttachRepo(context.Background(), alice(), RepoArgs{Slug: "wandb/hivemind"}); err != nil {
		t.Fatal(err)
	}

	list, err := r.ops.ListRepos(mallory())
	if err != nil || len(list) != 0 {
		t.Fatalf("ListRepos(mallory) = %v, %v; want empty", list, err)
	}
	_, gone := r.ops.DetachRepo(context.Background(), mallory(), "", "wandb/hivemind")
	if !IsKind(gone, KindNotFound) {
		t.Fatalf("DetachRepo(mallory) = %v, want KindNotFound", gone)
	}
	// The two sentences differ only in the name the caller typed, which is the
	// whole of the masking property: nothing in either says whether the row is
	// there.
	_, never := r.ops.DetachRepo(context.Background(), mallory(), "", "wandb/nothing")
	if gone.Error() != `no repo "wandb/hivemind"` || never.Error() != `no repo "wandb/nothing"` {
		t.Errorf("someone else's repo answers %q where an absent one answers %q", gone, never)
	}
	// And the owner still has it, so "empty" is not vacuous.
	if mine, _ := r.ops.ListRepos(alice()); len(mine) != 1 {
		t.Errorf("ListRepos(alice) = %d rows, want 1", len(mine))
	}
}

func TestListReposIsNeverNil(t *testing.T) {
	r := newRig(t)
	withRepos(r)
	list, err := r.ops.ListRepos(alice())
	if err != nil || list == nil || len(list) != 0 {
		t.Fatalf("ListRepos = %v, %v; want an empty non-nil slice", list, err)
	}
}

// `repo check` is the verb that exists because every other surface reports
// success: one row per attachment, each with its own answer.
func TestCheckReposReportsPerAttachment(t *testing.T) {
	r := newRig(t)
	withRepos(r)
	linkGitHub(r, "alice", "alice-gh", users.GitHubViaKeys, 99)
	for _, slug := range []string{"wandb/hivemind", "wandb/secret-thing"} {
		if _, err := r.ops.AttachRepo(context.Background(), alice(), RepoArgs{Slug: slug}); err != nil {
			t.Fatal(err)
		}
	}

	checks, err := r.ops.CheckRepos(context.Background(), alice())
	if err != nil {
		t.Fatalf("CheckRepos: %v", err)
	}
	if len(checks) != 2 {
		t.Fatalf("checks %+v, want 2", checks)
	}
	byslug := map[string]RepoCheck{}
	for _, c := range checks {
		byslug[c.Slug] = c
	}
	if !byslug["wandb/hivemind"].Reachable {
		t.Errorf("an installed repo reported %+v", byslug["wandb/hivemind"])
	}
	if byslug["wandb/secret-thing"].Reachable || byslug["wandb/secret-thing"].Reason == "" {
		t.Errorf("an uninstalled repo reported %+v", byslug["wandb/secret-thing"])
	}
}

// A refusal from Authorize is a fact about this repository — the user is not in
// that org, or the App cannot see the membership — so it lands on the row with
// its own sentence rather than replacing the whole answer.
func TestCheckReposReportsAnAuthorizationRefusalOnTheRow(t *testing.T) {
	r := newRig(t)
	_, app := withRepos(r)
	linkGitHub(r, "alice", "alice-gh", users.GitHubViaKeys, 99)
	if _, err := r.ops.AttachRepo(context.Background(), alice(), RepoArgs{Slug: "wandb/hivemind"}); err != nil {
		t.Fatal(err)
	}
	app.refuse = ghapp.ErrForbidden

	checks, err := r.ops.CheckRepos(context.Background(), alice())
	if err != nil {
		t.Fatalf("CheckRepos: %v", err)
	}
	if len(checks) != 1 || checks[0].Reachable || checks[0].Reason == "" {
		t.Fatalf("checks %+v, want one unreachable row with a reason", checks)
	}
}

// github.com not answering is one 502, not a page of confident wrong lines.
func TestCheckReposAnswersUpstreamWhenGitHubIsDown(t *testing.T) {
	r := newRig(t)
	_, app := withRepos(r)
	linkGitHub(r, "alice", "alice-gh", users.GitHubViaKeys, 99)
	if _, err := r.ops.AttachRepo(context.Background(), alice(), RepoArgs{Slug: "wandb/hivemind"}); err != nil {
		t.Fatal(err)
	}
	app.upstream = true

	_, err := r.ops.CheckRepos(context.Background(), alice())
	if !IsKind(err, KindUpstream) {
		t.Fatalf("CheckRepos with github.com down = %v, want KindUpstream", err)
	}
	var e *Error
	errors.As(err, &e)
	if e.HTTPStatus() != 502 {
		t.Errorf("HTTP status %d, want 502", e.HTTPStatus())
	}
}

// ---------------------------------------------------------------------------
// Hosts that have not configured this
// ---------------------------------------------------------------------------

// A host with no repo store answers every verb the same way, and says so about
// the HOST rather than about the command.
func TestRepoOpsOnAHostWithoutThem(t *testing.T) {
	r := newRig(t)
	withRepos(r)
	r.ops.repos = nil
	ctx := context.Background()

	for name, run := range map[string]func() error{
		"ls":    func() error { _, err := r.ops.ListRepos(alice()); return err },
		"add":   func() error { _, err := r.ops.AttachRepo(ctx, alice(), RepoArgs{Slug: "a/b"}); return err },
		"rm":    func() error { _, err := r.ops.DetachRepo(ctx, alice(), "", "a/b"); return err },
		"check": func() error { _, err := r.ops.CheckRepos(ctx, alice()); return err },
	} {
		err := run()
		if !IsKind(err, KindDisabled) {
			t.Errorf("repo %s = %v, want KindDisabled", name, err)
			continue
		}
		var e *Error
		errors.As(err, &e)
		if e.Msg != "repo attachments are not enabled on this host" {
			t.Errorf("repo %s says %q", name, e.Msg)
		}
		if !e.Verbatim || e.ExitCode() != 1 || e.HTTPStatus() != 501 {
			t.Errorf("repo %s = verbatim %v, exit %d, status %d", name, e.Verbatim, e.ExitCode(), e.HTTPStatus())
		}
	}
}

// A host with a repo store and no App can still attach — that is Part 7's first
// milestone, and a public repository needs no credential at all — but it cannot
// answer the two questions that need one.
func TestAppOnlyVerbsOnAHostWithNoApp(t *testing.T) {
	r := newRig(t)
	withRepos(r)
	r.ops.ghApp = nil

	if _, err := r.ops.CheckRepos(context.Background(), alice()); !IsKind(err, KindDisabled) {
		t.Errorf("CheckRepos with no App = %v, want KindDisabled", err)
	}
	if _, err := r.ops.GitHubInstallURL(alice()); !IsKind(err, KindDisabled) {
		t.Errorf("GitHubInstallURL with no App = %v, want KindDisabled", err)
	}
	if caps := r.ops.Capabilities(); !caps.Repos || caps.GitHubApp {
		t.Errorf("capabilities %+v, want repos without an app", caps)
	}
}

func TestGitHubInstallURLIsTheHostsOwnApp(t *testing.T) {
	r := newRig(t)
	_, app := withRepos(r)
	url, err := r.ops.GitHubInstallURL(alice())
	if err != nil || url != app.InstallURL() {
		t.Fatalf("GitHubInstallURL = %q, %v", url, err)
	}
}

// ---------------------------------------------------------------------------
// Retagging and attaching reach the boxes that are already running
// ---------------------------------------------------------------------------

// The bug this closes: a sandbox created after the feature shipped would accept
// a tag, report the tag, and check nothing out — because only the boot pass
// reconciled checkouts, and the box had already booted. Tags decide which repos
// a sandbox gets in exactly the way they decide which secrets it gets, and
// SetTags has always re-pushed the secrets.
func TestSetTagsChecksOutTheReposTheNewTagsImply(t *testing.T) {
	r := newRig(t)
	if _, _, err := r.ops.SetTags(context.Background(), alice(), "alicebox", []string{"hm"}); err != nil {
		t.Fatal(err)
	}
	if !r.calls.has("ResyncRepos alicebox") {
		t.Errorf("no checkout sync after a tag change: %v", r.calls.all())
	}
	if !r.calls.has("ResyncEnv alicebox") {
		t.Errorf("the secret re-push regressed: %v", r.calls.all())
	}
}

// A guest too old to check anything out must produce a sentence, not silence.
// The tags ARE set either way, so this is a note beside a success and never an
// error: failing the call would report a durable change as a rejected one and
// have the user run it again.
func TestSetTagsReportsAGuestThatCannotCheckOut(t *testing.T) {
	r := newRig(t)
	r.boxes.repoSyncErr = fmt.Errorf("%w: alicebox", host.ErrNoRepoSupport)

	tags, note, err := r.ops.SetTags(context.Background(), alice(), "alicebox", []string{"hm"})
	if err != nil {
		t.Fatalf("a guest that cannot check out failed the whole call: %v", err)
	}
	if len(tags) != 1 || tags[0] != "hm" {
		t.Errorf("tags = %v, want the change to have landed anyway", tags)
	}
	if !strings.Contains(note, "recreate") {
		t.Errorf("note = %q, want it to say what to do about an old sandbox", note)
	}

	// An unreachable guest is a different sentence with the same shape: the
	// attachment stands and the next boot reconciles it.
	r.boxes.repoSyncErr = errors.New("dial alicebox: connection refused")
	_, note, err = r.ops.SetTags(context.Background(), alice(), "alicebox", []string{"hm"})
	if err != nil {
		t.Fatalf("an unreachable guest failed the whole call: %v", err)
	}
	if !strings.Contains(note, "next start") {
		t.Errorf("note = %q, want it to say the checkout still happens later", note)
	}
}

// Attaching is the same event as retagging seen from the other side: one
// changes which repos a tag names, the other which tags a box has, and both end
// with a running guest whose checkouts no longer match what its owner asked for.
func TestAttachRepoChecksOutIntoTheSandboxesAlreadyCarryingTheTag(t *testing.T) {
	r := newRig(t)
	withRepos(r)
	linkGitHub(r, "alice", "alice-gh", users.GitHubViaKeys, 99)
	mustNoErr(t, r.tagger.SetTags("alicebox", "alice", []string{"hm"}))
	r.calls.reset()

	res, err := r.ops.AttachRepo(context.Background(), alice(),
		RepoArgs{Slug: "wandb/hivemind", Tags: []string{"hm"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Sandboxes) != 1 || res.Sandboxes[0] != "alicebox" {
		t.Fatalf("fan-out = %v, want alicebox", res.Sandboxes)
	}
	if !r.calls.has("ResyncRepos alicebox") {
		t.Errorf("attaching did not reach the box already carrying the tag: %v", r.calls.all())
	}
	if len(res.Notes) != 0 {
		t.Errorf("notes = %v, want none when every box took the job", res.Notes)
	}
}

// The fan-out's failures are reported per box and never fail the attach: the
// row is written before any guest is touched, and telling the user it did not
// save would be false.
func TestAttachRepoReportsPerSandboxAndStillAttaches(t *testing.T) {
	r := newRig(t)
	withRepos(r)
	linkGitHub(r, "alice", "alice-gh", users.GitHubViaKeys, 99)
	mustNoErr(t, r.tagger.SetTags("alicebox", "alice", []string{"hm"}))
	r.boxes.repoSyncErr = fmt.Errorf("%w: alicebox", host.ErrNoRepoSupport)

	res, err := r.ops.AttachRepo(context.Background(), alice(),
		RepoArgs{Slug: "wandb/hivemind", Tags: []string{"hm"}})
	if err != nil {
		t.Fatalf("a guest that cannot check out failed the attach: %v", err)
	}
	if res.Repo.Slug != "wandb/hivemind" {
		t.Errorf("the attachment did not land: %+v", res.Repo)
	}
	if len(res.Notes) != 1 || !strings.Contains(res.Notes[0], "alicebox") {
		t.Errorf("notes = %v, want one naming the box that could not check out", res.Notes)
	}
}
