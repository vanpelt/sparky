package ctlops

import (
	"context"
	"testing"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/secrets"
)

// TestPutSecretResyncsTheSandboxesThatSelectIt is the behaviour that makes
// `secret set` feel like setting a variable rather than filing a request: a
// value saved while a box is running has to reach that box now. Without the
// re-push it lands at the next resume, which for a pinned sandbox is never.
func TestPutSecretResyncsTheSandboxesThatSelectIt(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	mustNoErr(t, r.tagger.SetTags("alicebox", "alice", []string{"web"}))

	res, err := r.ops.PutSecret(ctx, alice(), "TOKEN", "s3cret", []string{"web"})
	if err != nil {
		t.Fatalf("PutSecret: %v", err)
	}
	if len(res.Resynced) != 1 || res.Resynced[0] != "alicebox" {
		t.Fatalf("resynced %v, want [alicebox]", res.Resynced)
	}
	if !r.calls.has("ResyncEnv alicebox") {
		t.Errorf("the box selecting the secret was never re-pushed: %v", r.calls.all())
	}
}

// A secret nothing selects still saves, and reports that it reached nothing.
// This is the tag mistake the feature invites, and the caller needs the empty
// fan-out to say so — see the note sshgw prints on it.
func TestPutSecretReportsWhenNothingSelectsIt(t *testing.T) {
	r := newRig(t)
	res, err := r.ops.PutSecret(context.Background(), alice(), "TOKEN", "v", []string{"nobody-has-this"})
	if err != nil {
		t.Fatalf("PutSecret: %v", err)
	}
	if len(res.Resynced) != 0 {
		t.Fatalf("resynced %v, want nothing", res.Resynced)
	}
}

// The two halves of the default have to agree, or the join between them finds
// nothing: an untagged secret and an untagged new sandbox must meet.
func TestUntaggedSecretAndUntaggedSandboxMeetOnTheDefaultTag(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()

	box, err := r.ops.Create(ctx, alice(), CreateArgs{Name: "fresh"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	tags, err := r.tagger.TagsFor(box.Name)
	if err != nil {
		t.Fatal(err)
	}
	if len(tags) != 1 || tags[0] != secrets.DefaultTag {
		t.Fatalf("a new sandbox has tags %v, want [%s]", tags, secrets.DefaultTag)
	}

	res, err := r.ops.PutSecret(ctx, alice(), "CLAUDE_CODE_OAUTH_TOKEN", "tok", nil)
	if err != nil {
		t.Fatalf("PutSecret: %v", err)
	}
	if len(res.Resynced) != 1 || res.Resynced[0] != "fresh" {
		t.Fatalf("an untagged secret reached %v, want [fresh] — the defaults do not meet", res.Resynced)
	}
	if len(res.Tags) != 1 || res.Tags[0] != secrets.DefaultTag {
		t.Errorf("reported tags %v, want the effective [%s]", res.Tags, secrets.DefaultTag)
	}
}

// An explicit clear still clears. Defaulting the empty set at SetTags too would
// make the default impossible to opt out of, which is the difference between a
// helpful default and a policy.
func TestClearingASandboxesTagsIsStillPossible(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	if _, err := r.ops.Create(ctx, alice(), CreateArgs{Name: "fresh"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, _, err := r.ops.SetTags(ctx, alice(), "fresh", nil)
	if err != nil {
		t.Fatalf("SetTags: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("tags after an explicit clear = %v, want none", got)
	}
}

// DeleteSecret has to compute the fan-out BEFORE the row goes away, or the
// variable lingers in every running guest until its next resume — a deletion
// that does not delete.
func TestDeleteSecretStripsItFromTheSandboxesThatHadIt(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	mustNoErr(t, r.tagger.SetTags("alicebox", "alice", []string{"web"}))
	if _, err := r.ops.PutSecret(ctx, alice(), "TOKEN", "v", []string{"web"}); err != nil {
		t.Fatalf("PutSecret: %v", err)
	}
	r.calls.reset()

	affected, err := r.ops.DeleteSecret(ctx, alice(), "TOKEN")
	if err != nil {
		t.Fatalf("DeleteSecret: %v", err)
	}
	if len(affected) != 1 || affected[0] != "alicebox" {
		t.Fatalf("affected %v, want [alicebox]", affected)
	}
	if !r.calls.has("ResyncEnv alicebox") {
		t.Errorf("the box still holds the deleted value: %v", r.calls.all())
	}
}

// Owner scoping is structural: the handle is not an argument, so naming another
// user's env var reaches only your own namespace — here, nothing at all.
func TestSecretsAreScopedToTheCaller(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	if _, err := r.ops.PutSecret(ctx, alice(), "TOKEN", "alices", nil); err != nil {
		t.Fatalf("PutSecret: %v", err)
	}
	metas, err := r.ops.ListSecrets(mallory())
	if err != nil {
		t.Fatalf("ListSecrets: %v", err)
	}
	if len(metas) != 0 {
		t.Fatalf("mallory sees %v, want none of alice's", metas)
	}
	if _, err := r.ops.DeleteSecret(ctx, mallory(), "TOKEN"); err == nil {
		t.Fatal("mallory deleted a secret named like alice's")
	}
	if metas, _ := r.ops.ListSecrets(alice()); len(metas) != 1 {
		t.Fatalf("alice's secret did not survive mallory's delete: %v", metas)
	}
}

// An empty value is a mistake every time — a pipe that produced nothing, a
// command that failed — and storing it would replace a working credential with
// one that silently does not authenticate.
func TestPutSecretRefusesAnEmptyValue(t *testing.T) {
	r := newRig(t)
	_, err := r.ops.PutSecret(context.Background(), alice(), "TOKEN", "", nil)
	if !IsKind(err, KindInvalid) {
		t.Fatalf("err = %v, want KindInvalid", err)
	}
}

// A host with no secret store answers KindDisabled rather than panicking on a
// nil interface, exactly as the other optional stores do.
func TestSecretOpsOnAHostWithoutThem(t *testing.T) {
	r := newRig(t)
	r.ops.secrets = nil
	ctx := context.Background()

	if _, err := r.ops.ListSecrets(alice()); !IsKind(err, KindDisabled) {
		t.Errorf("ListSecrets = %v, want KindDisabled", err)
	}
	if _, err := r.ops.PutSecret(ctx, alice(), "T", "v", nil); !IsKind(err, KindDisabled) {
		t.Errorf("PutSecret = %v, want KindDisabled", err)
	}
	if _, err := r.ops.DeleteSecret(ctx, alice(), "T"); !IsKind(err, KindDisabled) {
		t.Errorf("DeleteSecret = %v, want KindDisabled", err)
	}
}

func mustNoErr(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
