package ctlops

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
)

// indexOfCall returns the position of the first recorded call with the given
// prefix, or -1. Ordering assertions are the whole point of this file, so the
// helper deliberately reports position rather than mere presence.
func indexOfCall(t *testing.T, got []string, prefix string) int {
	t.Helper()
	for i, s := range got {
		if len(s) >= len(prefix) && s[:len(prefix)] == prefix {
			return i
		}
	}
	return -1
}

// TestCreateStampsTagsBeforeCreate pins the ordering constraint that made this
// package worth writing: host.Manager.Create fires the secret-env push on a
// goroutine, and the sandbox's tags decide what that push contains, so tags
// stamped afterwards race it and usually lose.
func TestCreateStampsTagsBeforeCreate(t *testing.T) {
	r := newRig(t)
	r.calls.reset()

	got, err := r.ops.Create(context.Background(), alice(), CreateArgs{Tags: []string{"ML", "prod,ml"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got.Name != "generated-name" {
		t.Errorf("name = %q, want the generated one", got.Name)
	}
	if !slices.Equal(got.Tags, []string{"ml", "prod"}) {
		t.Errorf("tags = %v, want normalized [ml prod]", got.Tags)
	}

	calls := r.calls.all()
	set := indexOfCall(t, calls, "SetTags generated-name")
	create := indexOfCall(t, calls, "Create generated-name")
	if set < 0 || create < 0 {
		t.Fatalf("missing calls in %v", calls)
	}
	if set > create {
		t.Errorf("tags stamped AFTER create — the secret-env push will race it:\n%v", calls)
	}
}

// TestCreateRollsBackTagsOnFailure: a sandbox that never came into being must
// not leave tag rows behind, or the next box to take that name inherits them.
func TestCreateRollsBackTagsOnFailure(t *testing.T) {
	r := newRig(t)
	r.boxes.err = errors.New("driver exploded")
	r.calls.reset()

	_, err := r.ops.Create(context.Background(), alice(), CreateArgs{Name: "doomed", Tags: []string{"ml"}})
	if err == nil {
		t.Fatal("Create: want an error")
	}
	if !IsKind(err, KindInternal) {
		t.Errorf("kind = %v, want KindInternal for a driver fault", err.(*Error).Kind)
	}
	if tags := r.tagger.tags["doomed"]; len(tags) != 0 {
		t.Errorf("tag rows survived a failed create: %v", tags)
	}
}

// TestForkStampsTagsBeforeFork: fork IS a create, so it inherits the ordering
// rule. internal/userconsole's fork gets this wrong today; ctlops is where it
// stops being possible to get wrong.
func TestForkStampsTagsBeforeFork(t *testing.T) {
	r := newRig(t)
	r.calls.reset()

	if _, err := r.ops.Fork(context.Background(), alice(), ForkArgs{
		Snapshot: "alicesnap", Name: "forked", Tags: []string{"ml"},
	}); err != nil {
		t.Fatalf("Fork: %v", err)
	}
	calls := r.calls.all()
	set := indexOfCall(t, calls, "SetTags forked")
	fork := indexOfCall(t, calls, "Fork alicesnap->forked")
	if set < 0 || fork < 0 {
		t.Fatalf("missing calls in %v", calls)
	}
	if set > fork {
		t.Errorf("tags stamped AFTER fork:\n%v", calls)
	}
}

func TestForkRollsBackTagsOnFailure(t *testing.T) {
	r := newRig(t)
	r.tmpl.err = errors.New("no space")

	_, err := r.ops.Fork(context.Background(), alice(), ForkArgs{
		Snapshot: "alicesnap", Name: "doomed", Tags: []string{"ml"},
	})
	if err == nil {
		t.Fatal("Fork: want an error")
	}
	if tags := r.tagger.tags["doomed"]; len(tags) != 0 {
		t.Errorf("tag rows survived a failed fork: %v", tags)
	}
}

// TestCreateNeverTouchesAnotherOwnersTags is the security regression: tag rows
// are keyed by NAME, and the name is whatever the caller sent. Stamping before
// resolving the name let any authenticated user replace — and then, via the
// rollback, delete — the tags of whoever already held it. That strips the
// victim's secret-env selection AND their per-tag egress allowlist, so a
// filtered VM comes back unfiltered.
func TestCreateNeverTouchesAnotherOwnersTags(t *testing.T) {
	r := newRig(t)
	if _, err := r.ops.SetTags(context.Background(), alice(), "alicebox", []string{"prod"}); err != nil {
		t.Fatalf("seed alice's tags: %v", err)
	}
	r.calls.reset()

	_, err := r.ops.Create(context.Background(), mallory(),
		CreateArgs{Name: "alicebox", Tags: []string{"probe"}})
	if !IsKind(err, KindConflict) {
		t.Fatalf("err = %v, want KindConflict for a taken name", err)
	}
	// Not merely "the tags survived": no write may be attempted at all, since a
	// store that allowed it would then be the only thing standing in the way.
	if got := r.calls.mutating(); len(got) != 0 {
		t.Errorf("a create onto a stranger's name reached a mutating store call: %v", got)
	}
	if tags := r.tagger.tags["alicebox"]; !slices.Equal(tags, []string{"prod"}) {
		t.Errorf("alice's tags are now %v, want [prod]", tags)
	}
}

// TestForkNeverTouchesAnotherOwnersTags: Fork gates the SOURCE snapshot and
// used to leave the DESTINATION name ungated, which is the same hole.
func TestForkNeverTouchesAnotherOwnersTags(t *testing.T) {
	r := newRig(t)
	if _, err := r.ops.SetTags(context.Background(), alice(), "alicebox", []string{"prod"}); err != nil {
		t.Fatalf("seed alice's tags: %v", err)
	}
	// mallory needs a snapshot of her own, or ownedSnapshot refuses first and
	// the test proves nothing about the destination gate.
	r.tmpl.snaps["mallory/msnap"] = &host.Snapshot{Name: "msnap", Owner: "mallory", FromBox: "mbox"}
	r.calls.reset()

	_, err := r.ops.Fork(context.Background(), mallory(),
		ForkArgs{Snapshot: "msnap", Name: "alicebox", Tags: []string{"probe"}})
	if !IsKind(err, KindConflict) {
		t.Fatalf("err = %v, want KindConflict for a taken name", err)
	}
	if got := r.calls.mutating(); len(got) != 0 {
		t.Errorf("a fork onto a stranger's name reached a mutating store call: %v", got)
	}
	if tags := r.tagger.tags["alicebox"]; !slices.Equal(tags, []string{"prod"}) {
		t.Errorf("alice's tags are now %v, want [prod]", tags)
	}
}

// TestCreateWithoutTagStore refuses rather than silently dropping the tags: a
// sandbox that comes up without the secrets its tags select is a debugging
// afternoon, and "tagging is not enabled" is one line.
func TestCreateWithoutTagStore(t *testing.T) {
	r := newRig(t)
	r.ops.tags = nil

	_, err := r.ops.Create(context.Background(), alice(), CreateArgs{Tags: []string{"ml"}})
	if !IsKind(err, KindDisabled) {
		t.Fatalf("err = %v, want KindDisabled", err)
	}
	// No tags asked for, no complaint.
	if _, err := r.ops.Create(context.Background(), alice(), CreateArgs{Name: "plain"}); err != nil {
		t.Fatalf("untagged create: %v", err)
	}
}

// TestSetTagsResyncsEnv: secrets follow tags, so the guest needs a re-push
// immediately rather than at its next resume.
func TestSetTagsResyncsEnv(t *testing.T) {
	r := newRig(t)
	r.calls.reset()

	got, err := r.ops.SetTags(context.Background(), alice(), "alicebox", []string{"b", "a", "a"})
	if err != nil {
		t.Fatalf("SetTags: %v", err)
	}
	if !slices.Equal(got, []string{"a", "b"}) {
		t.Errorf("tags = %v, want sorted and deduped [a b]", got)
	}
	calls := r.calls.all()
	if indexOfCall(t, calls, "ResyncEnv alicebox") < 0 {
		t.Errorf("no ResyncEnv after SetTags: %v", calls)
	}
	// Clearing is the empty set, not a no-op.
	if got, err := r.ops.SetTags(context.Background(), alice(), "alicebox", nil); err != nil || len(got) != 0 {
		t.Errorf("clear = %v, %v; want empty", got, err)
	}
	if tags := r.tagger.tags["alicebox"]; len(tags) != 0 {
		t.Errorf("tags survived a clear: %v", tags)
	}
}

// TestSandboxInfoCarriesURLs proves the projection, including that the terminal
// URL is the two-label host the browser terminal actually lives on.
func TestSandboxInfoCarriesURLs(t *testing.T) {
	r := newRig(t)
	got, err := r.ops.Get(context.Background(), alice(), "alicebox")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.URL != "https://alicebox.example.test" {
		t.Errorf("URL = %q", got.URL)
	}
	if got.TerminalURL != "https://alicebox.xterm.example.test" {
		t.Errorf("TerminalURL = %q", got.TerminalURL)
	}
	if got.Tags == nil {
		t.Error("Tags must never be nil — the REST edge encodes it as []")
	}
}

// TestAttachUsesTheResumedRecord: a paused box has SSHAddr cleared, so an
// Endpoint built from the pre-resume copy would send the terminal bridge to an
// empty address.
func TestAttachUsesTheResumedRecord(t *testing.T) {
	r := newRig(t)
	if err := r.boxes.Pause(context.Background(), "alicebox"); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if addr := r.boxes.boxes["alicebox"].SSHAddr; addr != "" {
		t.Fatalf("precondition: paused box still has SSHAddr %q", addr)
	}

	ep, err := r.ops.Attach(context.Background(), alice(), "alicebox")
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if ep.SSHAddr == "" {
		t.Error("Endpoint.SSHAddr is empty — Attach used the pre-resume record")
	}
	if ep.Name != "alicebox" || ep.SSHUser != "sparky" {
		t.Errorf("endpoint = %+v", ep)
	}
}
