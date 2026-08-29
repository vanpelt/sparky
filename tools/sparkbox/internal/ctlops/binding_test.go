package ctlops

// Tag templates: what a binding does to a create, and what it does to the
// snapshot it points at.
//
// The assertions here are almost all about what does NOT happen — no store is
// written before a refusal, no create silently boots the stock image, no
// stranger's snapshot is confirmed — because every one of those failures is
// invisible at the moment it occurs and expensive twenty minutes later.

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
)

// TestCreateBootsFromTheTagsBoundTemplate is the feature in one assertion: no
// new verb, no snapshot name typed, the tag alone decides the disk.
func TestCreateBootsFromTheTagsBoundTemplate(t *testing.T) {
	r := newRig(t)
	r.bindings.bind("alice", "cuda", "alicesnap")
	r.calls.reset()

	if _, err := r.ops.Create(context.Background(), alice(),
		CreateArgs{Name: "worker", Tags: []string{"cuda"}}); err != nil {
		t.Fatalf("create on a bound tag: %v", err)
	}
	got := r.calls.all()
	if indexOfCall(t, got, "Create worker owner=alice image=snap-alice-alicesnap") < 0 {
		t.Fatalf("the create did not use the bound template's image: %v", got)
	}
	// The resolution reads the tags Create computed, never a sandbox_tags join:
	// stampTags has not run at that point, so a join would find no rows.
	if i := indexOfCall(t, got, "BindingsForTags alice"); i < 0 {
		t.Fatalf("the resolver never ran: %v", got)
	} else if set := indexOfCall(t, got, "SetTags worker"); set >= 0 && set < i {
		t.Errorf("the image was resolved AFTER the tags were stamped: %v", got)
	}
}

// TestCreateRefusesTwoBoundSnapshots: a sandbox has exactly one rootfs, so two
// tags naming two disks is refused rather than decided by a precedence rule
// nobody would ever read.
func TestCreateRefusesTwoBoundSnapshots(t *testing.T) {
	r := newRig(t)
	r.bindings.bind("alice", "cuda", "alicesnap")
	r.bindings.bind("alice", "node20", "othersnap")
	r.calls.reset()

	_, err := r.ops.Create(context.Background(), alice(),
		CreateArgs{Name: "confused", Tags: []string{"cuda", "node20"}})
	if !IsKind(err, KindConflict) {
		t.Fatalf("err = %v, want KindConflict", err)
	}
	var e *Error
	errors.As(err, &e)
	if e.Code != "template_ambiguous" || !e.Verbatim {
		t.Errorf("got %s (verbatim=%v), want template_ambiguous", e.Code, e.Verbatim)
	}
	// Both tags AND both snapshots: the caller typed the tags and has no other
	// way to see which of them carries which binding.
	for _, want := range []string{"cuda", "node20", "alicesnap", "othersnap"} {
		if !strings.Contains(e.Msg, want) {
			t.Errorf("refusal %q does not name %q", e.Msg, want)
		}
	}
	if tags, _ := e.Details["tags"].([]string); !slices.Equal(tags, []string{"cuda", "node20"}) {
		t.Errorf("details tags = %v, want both in store order", e.Details["tags"])
	}
	if snaps, _ := e.Details["snapshots"].([]string); !slices.Equal(snaps, []string{"alicesnap", "othersnap"}) {
		t.Errorf("details snapshots = %v, want both", e.Details["snapshots"])
	}
	// The refusal lands before stampTags, which is the whole reason it is in
	// Create and not in build: a create that cannot succeed must not leave tag
	// rows behind for a sandbox that never exists.
	if got := r.calls.mutating(); len(got) != 0 {
		t.Fatalf("an ambiguous create still wrote something: %v", got)
	}
}

// Two tags agreeing on one snapshot is an ordinary thing to do, so what is
// refused is more than one distinct SNAPSHOT, not more than one binding.
func TestCreateWithTwoTagsBoundToTheSameSnapshotSucceeds(t *testing.T) {
	r := newRig(t)
	r.bindings.bind("alice", "cuda", "alicesnap")
	r.bindings.bind("alice", "ml", "alicesnap")
	r.calls.reset()

	if _, err := r.ops.Create(context.Background(), alice(),
		CreateArgs{Name: "agreed", Tags: []string{"cuda", "ml"}}); err != nil {
		t.Fatalf("create on two agreeing tags: %v", err)
	}
	if got := r.calls.all(); indexOfCall(t, got, "Create agreed owner=alice image=snap-alice-alicesnap") < 0 {
		t.Fatalf("the agreed template was not used: %v", got)
	}
}

// A tag with no binding, and a host with no binding store at all, both build the
// operator's default image — which is what every create did before this existed.
func TestCreateFallsBackToDefaultImage(t *testing.T) {
	ctx := context.Background()

	t.Run("no binding for this tag", func(t *testing.T) {
		r := newRig(t)
		r.bindings.bind("alice", "cuda", "alicesnap")
		r.calls.reset()
		if _, err := r.ops.Create(ctx, alice(), CreateArgs{Name: "plain", Tags: []string{"web"}}); err != nil {
			t.Fatalf("create: %v", err)
		}
		if got := r.calls.all(); indexOfCall(t, got, "Create plain owner=alice image=base") < 0 {
			t.Fatalf("an unbound tag did not take the default image: %v", got)
		}
	})

	t.Run("no binding store", func(t *testing.T) {
		r := newRig(t)
		r.ops.templateTags = nil
		r.calls.reset()
		if _, err := r.ops.Create(ctx, alice(), CreateArgs{Name: "plain", Tags: []string{"cuda"}}); err != nil {
			t.Fatalf("create on a host with no bindings: %v", err)
		}
		got := r.calls.all()
		if indexOfCall(t, got, "Create plain owner=alice image=base") < 0 {
			t.Fatalf("a host with no binding store did not take the default image: %v", got)
		}
		if indexOfCall(t, got, "BindingsForTags") >= 0 {
			t.Errorf("a host with no binding store still queried one: %v", got)
		}
	})

	t.Run("no tags at all", func(t *testing.T) {
		r := newRig(t)
		r.ops.tags = nil // no tag store, so defaultTags adds nothing either
		r.calls.reset()
		if _, err := r.ops.Create(ctx, alice(), CreateArgs{Name: "bare"}); err != nil {
			t.Fatalf("create: %v", err)
		}
		if got := r.calls.all(); indexOfCall(t, got, "BindingsForTags") >= 0 {
			t.Errorf("an untagged create paid for a binding query: %v", got)
		}
	})
}

// TestCreateRefusesADanglingBinding is the backstop for every surface that can
// delete a snapshot without passing DeleteSnapshot's refusal — the user console
// calls the manager directly. The one thing that must never happen is a silent
// boot of the stock image, which is invisible until an agent inside the guest
// cannot find its toolchain.
func TestCreateRefusesADanglingBinding(t *testing.T) {
	r := newRig(t)
	r.bindings.bind("alice", "cuda", "ghostsnap")
	r.calls.reset()

	_, err := r.ops.Create(context.Background(), alice(),
		CreateArgs{Name: "orphan", Tags: []string{"cuda"}})
	if !IsKind(err, KindConflict) {
		t.Fatalf("err = %v, want KindConflict", err)
	}
	var e *Error
	errors.As(err, &e)
	if e.Code != "template_missing" {
		t.Errorf("code = %s, want template_missing", e.Code)
	}
	if !strings.Contains(e.Msg, "cuda") || !strings.Contains(e.Msg, "ghostsnap") {
		t.Errorf("refusal %q names neither the tag nor the missing snapshot", e.Msg)
	}
	for _, c := range r.calls.all() {
		if strings.HasPrefix(c, "Create ") {
			t.Fatalf("a dangling binding fell back to the stock image: %s", c)
		}
	}
	if got := r.calls.mutating(); len(got) != 0 {
		t.Errorf("the refusal wrote something first: %v", got)
	}
}

// TestCreateRefusesWhenTheBindingStoreFails. A store that cannot answer is fatal
// to the create, and that is the one rule in this file with no second chance:
// the listing column degrades to an empty cell because it decorates a row, but
// this read decides which rootfs boots. A `database is locked` that fell back to
// the stock image would hand somebody the wrong disk and say nothing, which is
// the same silent failure TestCreateRefusesADanglingBinding exists to refuse.
func TestCreateRefusesWhenTheBindingStoreFails(t *testing.T) {
	r := newRig(t)
	r.bindings.bind("alice", "cuda", "alicesnap")
	r.bindings.err = errors.New("database is locked")
	r.calls.reset()

	_, err := r.ops.Create(context.Background(), alice(),
		CreateArgs{Name: "unlucky", Tags: []string{"cuda"}})
	if !IsKind(err, KindInternal) {
		t.Fatalf("err = %v, want KindInternal", err)
	}
	for _, c := range r.calls.all() {
		if strings.HasPrefix(c, "Create ") {
			t.Fatalf("a store failure built a sandbox anyway: %s", c)
		}
	}
	// Same ordering guarantee as the ambiguity refusal: nothing is written for a
	// sandbox that never exists, tag rows included.
	if got := r.calls.mutating(); len(got) != 0 {
		t.Fatalf("the refusal wrote something first: %v", got)
	}
}

// A bind names a snapshot, so it is gated exactly like fork and rm: another
// owner's template is masked, and the store is never reached.
func TestBindTemplateMasksAnotherOwnersSnapshot(t *testing.T) {
	r := newRig(t)
	r.calls.reset()

	_, err := r.ops.BindTemplate(context.Background(), mallory(), "alicesnap", "cuda")
	if !IsKind(err, KindNotFound) {
		t.Fatalf("err = %v, want KindNotFound", err)
	}
	var e *Error
	errors.As(err, &e)
	if e.Msg != `no snapshot named "alicesnap"` {
		t.Errorf("msg = %q, want the masked snapshot sentence", e.Msg)
	}
	if got := r.calls.mutating(); len(got) != 0 {
		t.Fatalf("a masked bind still reached a store: %v", got)
	}
	// And alice's binding was never created, so a second probe cannot read it
	// back through her own listing either.
	if len(r.bindings.rows) != 0 {
		t.Errorf("rows = %v, want nothing written", r.bindings.rows)
	}
}

// The store's `default` refusal is the one sentence in this feature that
// explains a policy rather than a typo, and it reaches the user unrewritten.
func TestBindRefusesTheDefaultTagVerbatim(t *testing.T) {
	r := newRig(t)
	_, err := r.ops.BindTemplate(context.Background(), alice(), "alicesnap", "default")
	if !IsKind(err, KindInvalid) {
		t.Fatalf("err = %v, want KindInvalid", err)
	}
	var e *Error
	errors.As(err, &e)
	if !e.Verbatim {
		t.Error("the store's sentence must print unwrapped")
	}
	if e.Msg != defaultTagRefusal().Error() {
		t.Errorf("ctlops rewrote the store's sentence:\n got %q\nwant %q", e.Msg, defaultTagRefusal().Error())
	}
	// Tags are lowercased on the way in, so the shouted spelling is refused for
	// the same reason rather than by a character-set complaint.
	if _, err := r.ops.BindTemplate(context.Background(), alice(), "alicesnap", "DEFAULT"); !IsKind(err, KindInvalid) {
		t.Errorf("DEFAULT = %v, want the same refusal", err)
	}
}

// A re-point is otherwise completely silent: the same success line whether a
// binding was created or what every future box boots from was quietly changed.
func TestBindRepointReportsPrevious(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	r.tmpl.snaps["alice/newer"] = &host.Snapshot{
		Name: "newer", Owner: "alice", FromBox: "alicebox",
		Image: "snap-alice-newer", CreatedAt: time.Unix(0, 0).UTC(),
	}

	first, err := r.ops.BindTemplate(ctx, alice(), "alicesnap", "cuda")
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	if first.Previous != "" || first.Binding.Snapshot != "alicesnap" || first.Binding.Tag != "cuda" {
		t.Fatalf("first bind = %+v, want no previous", first)
	}
	again, err := r.ops.BindTemplate(ctx, alice(), "newer", "cuda")
	if err != nil {
		t.Fatalf("re-point: %v", err)
	}
	if again.Previous != "alicesnap" {
		t.Errorf("previous = %q, want the snapshot the tag stopped booting", again.Previous)
	}
	if again.Binding.Snapshot != "newer" {
		t.Errorf("binding = %+v, want the new snapshot", again.Binding)
	}
	// Two tags on one call has no meaning: a tag has one template.
	if _, err := r.ops.BindTemplate(ctx, alice(), "newer", "a,b"); !IsKind(err, KindInvalid) {
		t.Errorf("two tags = %v, want KindInvalid", err)
	}
}

// An unbind of a tag nobody bound is the masked not-found, on its own sentence:
// `no snapshot named "cuda"` would send the reader looking for a snapshot.
func TestUnbindUnknownTagIsNotFound(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	r.bindings.bind("alice", "cuda", "alicesnap")

	_, err := r.ops.UnbindTemplate(ctx, alice(), "nope")
	if !IsKind(err, KindNotFound) {
		t.Fatalf("err = %v, want KindNotFound", err)
	}
	var e *Error
	errors.As(err, &e)
	if e.Code != "template_binding_not_found" || e.Msg != `no tag "nope" has a template bound` {
		t.Errorf("got %s/%q, want the template_binding sentence", e.Code, e.Msg)
	}
	// Another owner's tag is indistinguishable from an unbound one, because the
	// store's query carries the owner.
	if _, err := r.ops.UnbindTemplate(ctx, mallory(), "cuda"); !IsKind(err, KindNotFound) {
		t.Errorf("cross-owner unbind = %v, want the same masked answer", err)
	}
	if _, ok := r.bindings.rows[bindKey("alice", "cuda")]; !ok {
		t.Fatal("mallory's unbind removed alice's binding")
	}

	b, err := r.ops.UnbindTemplate(ctx, alice(), "cuda")
	if err != nil {
		t.Fatalf("unbind own tag: %v", err)
	}
	if b.Snapshot != "alicesnap" {
		t.Errorf("unbind reported %+v; it must name the snapshot the tag stopped pointing at", b)
	}
}

// Deleting a bound snapshot would turn every future create on its tags into the
// template_missing refusal, so it is refused while anything depends on it.
func TestDeleteSnapshotRefusedWhileBound(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	r.bindings.bind("alice", "cuda", "alicesnap")
	r.calls.reset()

	err := r.ops.DeleteSnapshot(ctx, alice(), "alicesnap")
	if !IsKind(err, KindConflict) {
		t.Fatalf("err = %v, want KindConflict", err)
	}
	var e *Error
	errors.As(err, &e)
	if e.Code != "snapshot_bound" || !strings.Contains(e.Msg, "cuda") {
		t.Errorf("got %s/%q, want snapshot_bound naming the tag", e.Code, e.Msg)
	}
	if !strings.Contains(e.Hint, "unbind") {
		t.Errorf("hint = %q, want the way out", e.Hint)
	}
	if r.calls.has("DeleteSnapshot alicesnap owner=alice") {
		t.Fatal("the driver was asked to delete a bound template anyway")
	}

	// Unbound, it goes.
	if _, err := r.ops.UnbindTemplate(ctx, alice(), "cuda"); err != nil {
		t.Fatalf("unbind: %v", err)
	}
	if err := r.ops.DeleteSnapshot(ctx, alice(), "alicesnap"); err != nil {
		t.Fatalf("delete after unbind: %v", err)
	}
}

// The bound-tags column, and its one rule: the bindings are decoration on the
// row, not its subject, so a store hiccup drops the column rather than the
// listing.
func TestListSnapshotsCarriesBoundTags(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	r.bindings.bind("alice", "ml", "alicesnap")
	r.bindings.bind("alice", "cuda", "alicesnap")

	snaps, err := r.ops.ListSnapshots(ctx, alice())
	if err != nil || len(snaps) != 1 {
		t.Fatalf("ListSnapshots = %v, %v", snaps, err)
	}
	if !slices.Equal(snaps[0].BoundTags, []string{"cuda", "ml"}) {
		t.Errorf("bound_tags = %v, want both in store order", snaps[0].BoundTags)
	}
	// A single-machine host reports no node at all, so its payload is
	// byte-identical to the one that shipped and never prints "on local".
	r.tmpl.snaps["alice/alicesnap"].Node = "local"
	if snaps, _ := r.ops.ListSnapshots(ctx, alice()); snaps[0].Node != "" {
		t.Errorf("node = %q on a host with no placer; it must stay empty", snaps[0].Node)
	}

	r.bindings.err = errors.New("database is locked")
	snaps, err = r.ops.ListSnapshots(ctx, alice())
	if err != nil {
		t.Fatalf("a binding-store failure took the listing down: %v", err)
	}
	if len(snaps) != 1 || len(snaps[0].BoundTags) != 0 {
		t.Errorf("degraded listing = %+v, want the row with an empty column", snaps)
	}
}
