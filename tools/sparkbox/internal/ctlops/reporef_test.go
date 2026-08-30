package ctlops

import (
	"context"
	"strings"
	"testing"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/repos"
)

// attach seeds an attachment on a tag, straight into the fake store, so these
// tests are about --ref rather than about `repo add`'s own gate.
func attach(t *testing.T, r *rig, owner, slug, ref string, tags ...string) {
	t.Helper()
	if err := r.ops.repos.PutRepo(owner, repos.Repo{Slug: slug, Ref: ref}, tags); err != nil {
		t.Fatal(err)
	}
}

// The happy path, and the only property that matters at this layer: the branch
// the caller asked for is recorded against THAT sandbox, keyed by the
// attachment it names, before the box is built.
func TestCreateWithRefRecordsThePerSandboxBranch(t *testing.T) {
	r := newRig(t)
	rp, _ := withRepos(r)
	attach(t, r, "alice", "wandb/hivemind", "main", "hm")

	if _, err := r.ops.Create(context.Background(), alice(), CreateArgs{
		Name: "box", Tags: []string{"hm"}, Refs: []RepoRef{{Ref: "feat/x"}},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	got := rp.refs["alice\x00box"]
	if len(got) != 1 || got[0].Slug != "wandb/hivemind" || got[0].Ref != "feat/x" {
		t.Fatalf("recorded refs = %+v, want one wandb/hivemind=feat/x", got)
	}
}

// A --ref naming a repository the tags do not select is a REFUSAL, not a
// no-op. Somebody typed a branch and expects to find it checked out; a create
// that quietly ignored the flag would hand them a box on the wrong branch and
// no reason to look at the command they ran.
func TestCreateRefusesARefForAnUnattachedRepo(t *testing.T) {
	r := newRig(t)
	rp, _ := withRepos(r)
	attach(t, r, "alice", "wandb/hivemind", "main", "hm")

	_, err := r.ops.Create(context.Background(), alice(), CreateArgs{
		Name: "box", Tags: []string{"hm"}, Refs: []RepoRef{{Slug: "wandb/other", Ref: "feat/x"}},
	})
	if err == nil {
		t.Fatal("a --ref for a repository this sandbox has no attachment for was accepted")
	}
	if !strings.Contains(err.Error(), "wandb/hivemind") {
		t.Errorf("the refusal does not say what IS attached: %v", err)
	}
	// And it refused before the first write: no sandbox, no tag rows, no
	// override rows for a box that never existed.
	if _, ok := r.boxes.boxes["box"]; ok {
		t.Error("the sandbox was built despite the refusal")
	}
	if len(rp.refs) != 0 {
		t.Errorf("override rows were written for a refused create: %+v", rp.refs)
	}
	if tags, _ := r.tagger.TagsFor("box"); len(tags) != 0 {
		t.Errorf("tag rows were stranded by a refused create: %v", tags)
	}
}

// The bare form names no repository, so it is only unambiguous when the tags
// select one. With more, the refusal has to show the spelling that works.
func TestCreateRefusesABareRefWhenSeveralReposAreAttached(t *testing.T) {
	r := newRig(t)
	withRepos(r)
	attach(t, r, "alice", "wandb/hivemind", "main", "hm")
	attach(t, r, "alice", "wandb/other", "main", "hm")

	_, err := r.ops.Create(context.Background(), alice(), CreateArgs{
		Name: "box", Tags: []string{"hm"}, Refs: []RepoRef{{Ref: "feat/x"}},
	})
	if err == nil {
		t.Fatal("an ambiguous bare --ref was accepted")
	}
	if !strings.Contains(err.Error(), "--ref wandb/") || !strings.Contains(err.Error(), "=feat/x") {
		t.Errorf("the refusal does not show the scoped spelling: %v", err)
	}
}

// --ref with nothing attached at all is its own sentence: the fix is `repo
// add`, not a different branch name.
func TestCreateRefusesARefWithNothingAttached(t *testing.T) {
	r := newRig(t)
	withRepos(r)

	_, err := r.ops.Create(context.Background(), alice(), CreateArgs{
		Name: "box", Tags: []string{"hm"}, Refs: []RepoRef{{Ref: "feat/x"}},
	})
	if err == nil {
		t.Fatal("a --ref was accepted on a tag with no repository attached")
	}
	if !strings.Contains(err.Error(), "repo add") {
		t.Errorf("the refusal does not name the fix: %v", err)
	}
}

// A scoped flag wins over a bare one however they were ordered, and repeating
// either is a correction rather than a list.
func TestRefPrecedenceIsScopedOverBare(t *testing.T) {
	r := newRig(t)
	rp, _ := withRepos(r)
	attach(t, r, "alice", "wandb/hivemind", "main", "hm")

	if _, err := r.ops.Create(context.Background(), alice(), CreateArgs{
		Name: "box", Tags: []string{"hm"},
		Refs: []RepoRef{{Slug: "wandb/hivemind", Ref: "scoped"}, {Ref: "bare"}},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	got := rp.refs["alice\x00box"]
	if len(got) != 1 || got[0].Ref != "scoped" {
		t.Fatalf("recorded refs = %+v, want the scoped flag to win", got)
	}
}

// github.com is case-insensitive on both halves of a slug, and the store's
// column says so with COLLATE NOCASE. The Go-side match has to agree, or a
// --ref typed the way GitHub renders it is refused as unattached.
func TestRefMatchesASlugWithoutRegardToCase(t *testing.T) {
	r := newRig(t)
	rp, _ := withRepos(r)
	attach(t, r, "alice", "wandb/hivemind", "main", "hm")

	if _, err := r.ops.Create(context.Background(), alice(), CreateArgs{
		Name: "box", Tags: []string{"hm"}, Refs: []RepoRef{{Slug: "WandB/HiveMind", Ref: "feat/x"}},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	got := rp.refs["alice\x00box"]
	if len(got) != 1 || got[0].Slug != "wandb/hivemind" {
		t.Fatalf("recorded refs = %+v; the stored slug must be the attachment's, not the caller's spelling", got)
	}
}

// A fork is the case --ref exists for: the snapshot already holds the checkout,
// so this is the only way to ask for a different branch.
func TestForkCarriesTheRef(t *testing.T) {
	r := newRig(t)
	rp, _ := withRepos(r)
	attach(t, r, "alice", "wandb/hivemind", "main", "hm")

	if _, err := r.ops.Fork(context.Background(), alice(), ForkArgs{
		Snapshot: "alicesnap", Name: "forked", Tags: []string{"hm"},
		Refs: []RepoRef{{Ref: "feat/x"}},
	}); err != nil {
		t.Fatalf("fork: %v", err)
	}
	if got := rp.refs["alice\x00forked"]; len(got) != 1 || got[0].Ref != "feat/x" {
		t.Fatalf("recorded refs = %+v", got)
	}
}

// A create with no --ref still states the whole answer for that name, and that
// is defence in depth rather than a wasted write.
//
// Destroy clears a sandbox's overrides, but best-effort: a store that was down
// leaves the row and a WARN. Names are reusable, so the next `box` — possibly
// somebody else's, if the name was freed — would silently boot on a branch its
// creator never asked for. Writing the empty answer here means the only way to
// inherit an override is to ask for one.
func TestCreateWithoutRefClearsAStaleOverride(t *testing.T) {
	r := newRig(t)
	rp, _ := withRepos(r)
	attach(t, r, "alice", "wandb/hivemind", "main", "hm")
	// The row a failed destroy left behind, under a name now being reused.
	if err := rp.SetSandboxRefs("alice", "box", []repos.SandboxRef{
		{Host: "github.com", Slug: "wandb/hivemind", Ref: "someone-elses-branch"},
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := r.ops.Create(context.Background(), alice(), CreateArgs{Name: "box", Tags: []string{"hm"}}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if got := rp.refs["alice\x00box"]; len(got) != 0 {
		t.Errorf("a fresh sandbox inherited a stale override: %+v", got)
	}
}
