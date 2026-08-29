package ctlops

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/templates"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
)

// busySandboxes is a sandbox store that can report an in-flight disk operation,
// the way *host.Manager does. It is a wrapper rather than a method on the shared
// fake because the probe is an OPTIONAL type assertion: half of these tests are
// about a store that does not have it.
type busySandboxes struct {
	*fakeSandboxes
	op string
}

func (b *busySandboxes) DiskOperation(name string) (string, bool) {
	return b.op, b.op != ""
}

// selfRig is newRig with the tags a capture needs. Every test here starts from a
// sandbox that carries `default` and `web`, because that is the only shape in
// which the verb is allowed to work at all.
func selfRig(t *testing.T) *rig {
	t.Helper()
	r := newRig(t)
	if err := r.tagger.SetTags("alicebox", "alice", []string{"default", "web"}); err != nil {
		t.Fatal(err)
	}
	r.calls.reset()
	return r
}

func mustPlan(t *testing.T, r *rig, tag, name string) SelfSnapshotPlan {
	t.Helper()
	p, err := r.ops.PlanSelfSnapshot(context.Background(), alice(), "alicebox", tag, name)
	if err != nil {
		t.Fatalf("PlanSelfSnapshot(%q, %q): %v", tag, name, err)
	}
	return p
}

// refusedPlan asserts a refusal and that NOTHING moved. The second half is the
// point: this whole surface exists so that every "no" is delivered while the VM
// is still running, and a refusal that had already paused something would be a
// refusal nobody could act on.
func refusedPlan(t *testing.T, r *rig, tag, name, wantCode string, wantKind Kind) *Error {
	t.Helper()
	r.calls.reset()
	_, err := r.ops.PlanSelfSnapshot(context.Background(), alice(), "alicebox", tag, name)
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("PlanSelfSnapshot(%q): want *ctlops.Error, got %v", tag, err)
	}
	if e.Code != wantCode || e.Kind != wantKind {
		t.Errorf("PlanSelfSnapshot(%q): code=%q kind=%v, want %q %v", tag, e.Code, e.Kind, wantCode, wantKind)
	}
	if mutations := r.calls.mutating(); len(mutations) > 0 {
		t.Errorf("PlanSelfSnapshot(%q) reached %v — a plan mutates nothing", tag, mutations)
	}
	return e
}

func TestThePlanIsAPureRead(t *testing.T) {
	r := selfRig(t)
	p := mustPlan(t, r, "web", "")
	if p.Sandbox != "alicebox" || p.Tag != "web" {
		t.Errorf("plan = %+v", p)
	}
	// The rig's clock is fixed at the epoch, so the derived name is too.
	if p.Snapshot != "web-700101-0000" {
		t.Errorf("derived name = %q, want web-700101-0000", p.Snapshot)
	}
	if p.Bound != "" {
		t.Errorf("an unbound tag reported Bound=%q", p.Bound)
	}
	if len(p.Carriers) != 1 || p.Carriers[0].Name != "alicebox" || !p.Carriers[0].Self {
		t.Errorf("carriers = %+v, want just this sandbox marked as itself", p.Carriers)
	}
	if p.CtlHint != "ssh ctl@example.test" || p.SSHHint != "ssh alicebox.example.test" {
		t.Errorf("hints = %q / %q — a guest is never told its own domain, so these are host-authored",
			p.CtlHint, p.SSHHint)
	}
	if p.Token == "" {
		t.Error("no plan token; the commit has nothing to compare against")
	}
	if mutations := r.calls.mutating(); len(mutations) > 0 {
		t.Errorf("the plan reached %v", mutations)
	}
}

// TestThePlanReportsWhatARePointReplaces. Without this the person about to
// re-point a tag reads the same page whether they are creating a binding or
// silently changing what every future sandbox on it boots.
func TestThePlanReportsWhatARePointReplaces(t *testing.T) {
	r := selfRig(t)
	r.tmpl.snaps["alice/web-old"] = &host.Snapshot{
		Name: "web-old", Owner: "alice", FromBox: "blue-meadow",
		Image: "snap-alice-web-old", CreatedAt: time.Unix(0, 0).UTC(),
	}
	r.bindings.bind("alice", "web", "web-old")

	p := mustPlan(t, r, "web", "")
	if p.Bound != "web-old" || p.BoundFrom != "blue-meadow" {
		t.Errorf("plan = %+v, want the current binding and the box it came from", p)
	}
}

// TestThePlanListsEveryCarrierWithItsState is the input to the subtlest warning
// this feature has: re-pointing does not re-base a sandbox that already exists,
// running OR paused. A carrier list without states could not say so.
func TestThePlanListsEveryCarrierWithItsState(t *testing.T) {
	r := selfRig(t)
	for name, state := range map[string]vmm.State{"amber-hill": vmm.StateRunning, "still-fjord": vmm.StatePaused} {
		r.boxes.boxes[name] = &host.Sandbox{Name: name, Owner: "alice", State: state}
		if err := r.tagger.SetTags(name, "alice", []string{"default", "web"}); err != nil {
			t.Fatal(err)
		}
	}
	// Somebody else's box on the same tag, and one of alice's that is not on it.
	r.boxes.boxes["mallorybox"] = &host.Sandbox{Name: "mallorybox", Owner: "mallory", State: vmm.StateRunning}
	if err := r.tagger.SetTags("mallorybox", "mallory", []string{"default", "web"}); err != nil {
		t.Fatal(err)
	}
	r.boxes.boxes["untagged"] = &host.Sandbox{Name: "untagged", Owner: "alice", State: vmm.StateRunning}

	p := mustPlan(t, r, "web", "")
	var got []string
	for _, c := range p.Carriers {
		got = append(got, c.Name+"="+c.State)
	}
	want := []string{"alicebox=running", "amber-hill=running", "still-fjord=paused"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("carriers = %v, want %v (this sandbox first, then alphabetical, and nobody else's)", got, want)
	}
	if !p.Carriers[0].Self {
		t.Error("the sandbox making the request is not marked as itself")
	}
}

func TestThePlanRefusesEverythingItShould(t *testing.T) {
	t.Run("the default tag", func(t *testing.T) {
		r := selfRig(t)
		e := refusedPlan(t, r, "default", "", "tag_is_default", KindDenied)
		// Refused HERE and not at bind, because every sandbox carries `default`
		// so "a tag you carry" does not exclude it — and because a bind-time
		// refusal would land two minutes after the pause with nobody watching.
		if !strings.Contains(e.Msg, "every sandbox you create carries that tag") {
			t.Errorf("refusal = %q", e.Msg)
		}
	})
	t.Run("a tag this sandbox does not carry", func(t *testing.T) {
		r := selfRig(t)
		e := refusedPlan(t, r, "cuda", "", "tag_not_on_sandbox", KindDenied)
		// It has to name what the box DOES carry, and both repairs: widen this
		// box's tags, or mint the binding from the operator door.
		for _, want := range []string{"default, web", "ssh ctl@example.test tags alicebox cuda web",
			"ssh ctl@example.test snapshot create alicebox <name> --tag cuda"} {
			if !strings.Contains(e.Msg, want) {
				t.Errorf("refusal missing %q:\n%s", want, e.Msg)
			}
		}
	})
	t.Run("a tag that is not a tag", func(t *testing.T) {
		r := selfRig(t)
		// Discovered here rather than by the store at bind time, which is two
		// minutes after the pause. Exit 2, not 3: this is a typo, not a refusal
		// about authority.
		refusedPlan(t, r, "Web_1", "", "bad_tag", KindInvalid)
	})
	t.Run("a name that is already taken", func(t *testing.T) {
		r := selfRig(t)
		r.tmpl.snaps["alice/web-700101-0000"] = &host.Snapshot{
			Name: "web-700101-0000", Owner: "alice", FromBox: "alicebox"}
		e := refusedPlan(t, r, "web", "", "snapshot_name_taken", KindConflict)
		if !strings.Contains(e.Msg, "sparkbox snapshot web my-name") {
			t.Errorf("refusal does not offer the escape hatch:\n%s", e.Msg)
		}
	})
	t.Run("a driver that cannot capture", func(t *testing.T) {
		r := selfRig(t)
		r.tmpl.on = false
		refusedPlan(t, r, "web", "", "snapshot_disabled", KindDisabled)
	})
	t.Run("a host with nowhere to record the binding", func(t *testing.T) {
		r := selfRig(t)
		r.ops.templateTags = nil
		// THE important one: without it the capture runs for two minutes and
		// then fails to bind, with the session already gone.
		refusedPlan(t, r, "web", "", "snapshot_disabled", KindDisabled)
	})
	t.Run("a binding store that cannot answer", func(t *testing.T) {
		r := selfRig(t)
		r.bindings.err = errors.New("database is locked")
		// Fatal rather than logged. The whole gesture is "re-point this tag",
		// so a plan that could not read the binding cannot say what it is about
		// to replace — and the token it hands the commit would be a fingerprint
		// of a fact it never established.
		refusedPlan(t, r, "web", "", "internal", KindInternal)
	})
	t.Run("another owner's sandbox", func(t *testing.T) {
		r := selfRig(t)
		_, err := r.ops.PlanSelfSnapshot(context.Background(), mallory(), "alicebox", "web", "")
		var e *Error
		if !errors.As(err, &e) || e.Kind != KindNotFound {
			t.Fatalf("cross-owner plan = %v, want a masked not-found", err)
		}
	})
}

// TestTheBareFormTeachesTheModel: `sparkbox snapshot` with no argument has three
// answers, and two of them are the only place a user learns what a tag is for
// here.
func TestTheBareFormTeachesTheModel(t *testing.T) {
	t.Run("exactly one candidate", func(t *testing.T) {
		r := selfRig(t)
		p := mustPlan(t, r, "", "")
		if p.Tag != "web" {
			t.Errorf("tag = %q, want the sandbox's one non-default tag", p.Tag)
		}
	})
	t.Run("no candidate", func(t *testing.T) {
		r := selfRig(t)
		if err := r.tagger.SetTags("alicebox", "alice", []string{"default"}); err != nil {
			t.Fatal(err)
		}
		e := refusedPlan(t, r, "", "", "no_candidate_tag", KindInvalid)
		if !strings.Contains(e.Msg, "ssh ctl@example.test tags alicebox web") {
			t.Errorf("refusal does not say how to get a tag:\n%s", e.Msg)
		}
	})
	t.Run("several candidates", func(t *testing.T) {
		r := selfRig(t)
		if err := r.tagger.SetTags("alicebox", "alice", []string{"default", "cuda", "web"}); err != nil {
			t.Fatal(err)
		}
		e := refusedPlan(t, r, "", "", "several_candidate_tags", KindInvalid)
		for _, want := range []string{"sparkbox snapshot cuda", "sparkbox snapshot web"} {
			if !strings.Contains(e.Msg, want) {
				t.Errorf("refusal does not list %q:\n%s", want, e.Msg)
			}
		}
		if strings.Contains(e.Msg, "snapshot default") {
			t.Errorf("`default` was offered as a candidate:\n%s", e.Msg)
		}
	})
}

// TestTheDiskBusyProbeIsOptionalAndNeverAGate. lockDiskOperation is a plain
// blocking mutex, so a capture issued during an archive would otherwise say
// "your session is about to end" and then leave the box running for fifteen
// minutes. The warning is racy by nature, which is exactly why it must not gate.
func TestTheDiskBusyProbeIsOptionalAndNeverAGate(t *testing.T) {
	t.Run("a store without the probe", func(t *testing.T) {
		r := selfRig(t)
		p := mustPlan(t, r, "web", "")
		if p.Busy != "" {
			t.Errorf("Busy = %q on a store that cannot answer the question", p.Busy)
		}
	})
	t.Run("a store with one", func(t *testing.T) {
		r := selfRig(t)
		r.ops.boxes = &busySandboxes{fakeSandboxes: r.boxes, op: "archive"}
		p := mustPlan(t, r, "web", "")
		if p.Busy != "archive" {
			t.Errorf("Busy = %q, want the operation already holding the disk", p.Busy)
		}
	})
}

// TestThePlanTokenMovesWithTheFactsItReported, and only with those: the commit
// compares it, so a token that ignored a re-point would let somebody confirm
// warnings that were already false, and one that moved on an unrendered field
// would make them re-run the command for nothing.
func TestThePlanTokenMovesWithTheFactsItReported(t *testing.T) {
	r := selfRig(t)
	before := mustPlan(t, r, "web", "")
	if again := mustPlan(t, r, "web", before.Snapshot); again.Token != before.Token {
		t.Fatalf("the same facts produced two tokens (%q, %q)", before.Token, again.Token)
	}

	r.tmpl.snaps["alice/web-old"] = &host.Snapshot{
		Name: "web-old", Owner: "alice", FromBox: "blue-meadow", Image: "snap-alice-web-old"}
	r.bindings.bind("alice", "web", "web-old")
	if after := mustPlan(t, r, "web", before.Snapshot); after.Token == before.Token {
		t.Error("the tag was re-pointed under the user and the token did not move")
	}

	r.bindings.rows = map[string]templates.Binding{}
	r.boxes.boxes["amber-hill"] = &host.Sandbox{Name: "amber-hill", Owner: "alice", State: vmm.StateRunning}
	if err := r.tagger.SetTags("amber-hill", "alice", []string{"default", "web"}); err != nil {
		t.Fatal(err)
	}
	if after := mustPlan(t, r, "web", before.Snapshot); after.Token == before.Token {
		t.Error("a fourth sandbox picked up the tag and the token did not move")
	}
}

// TestSnapshotNameFor pins the arithmetic. snapNameRe admits 41 characters, the
// stamp is 11 and the separator 1, so a tag gets exactly 29 — and a tag longer
// than that is truncated rather than refused, because refusing a tag somebody is
// ALREADY using, at the moment they want to capture, is the worse outcome.
func TestSnapshotNameFor(t *testing.T) {
	at := time.Date(2026, 8, 29, 14, 12, 0, 0, time.UTC)
	for _, tc := range []struct{ tag, want string }{
		{"w", "w-260829-1412"},
		{"web", "web-260829-1412"},
		{strings.Repeat("a", 28), strings.Repeat("a", 28) + "-260829-1412"},
		{strings.Repeat("a", 29), strings.Repeat("a", 29) + "-260829-1412"},
		{strings.Repeat("a", 30), strings.Repeat("a", 29) + "-260829-1412"},
		{strings.Repeat("a", 40), strings.Repeat("a", 29) + "-260829-1412"},
		// A truncation landing on a dash must not leave one: `snap--…` is legal
		// but reads as a bug, and the trailing dash carries no information.
		{strings.Repeat("a", 29) + "-bbbbbbbbbbb", strings.Repeat("a", 29) + "-260829-1412"},
	} {
		got := snapshotNameFor(tc.tag, at)
		if got != tc.want {
			t.Errorf("snapshotNameFor(%q) = %q, want %q", tc.tag, got, tc.want)
		}
		if !selfSnapNameRe.MatchString(got) {
			t.Errorf("snapshotNameFor(%q) = %q, which the manager would refuse", tc.tag, got)
		}
	}
	// A local clock must not change the name: two people in two timezones
	// capturing the same tag in the same minute have to collide, not silently
	// produce two templates.
	if utc, local := snapshotNameFor("web", at), snapshotNameFor("web", at.In(time.FixedZone("x", 9*3600))); utc != local {
		t.Errorf("timezone changed the derived name: %q vs %q", utc, local)
	}
}

// TestSnapshotToTagCapturesThenBinds. The order is not a preference: binding
// first would leave the tag pointing at an image that does not exist, so every
// create on it in that window would resolve to a missing rootfs — a failure that
// spreads forward to other people's sandboxes.
func TestSnapshotToTagCapturesThenBinds(t *testing.T) {
	r := selfRig(t)
	res, err := r.ops.SnapshotToTag(context.Background(), alice(), SnapshotToTagArgs{
		Sandbox: "alicebox", Name: "web-700101-0000", Tag: "web",
	})
	if err != nil {
		t.Fatalf("SnapshotToTag: %v", err)
	}
	if !res.Bound || res.Tag != "web" || res.Snapshot.Name != "web-700101-0000" {
		t.Errorf("result = %+v", res)
	}
	if len(res.Snapshot.BoundTags) != 1 || res.Snapshot.BoundTags[0] != "web" {
		t.Errorf("bound tags = %v — the result names the tag in one field and not the other",
			res.Snapshot.BoundTags)
	}
	calls := r.calls.all()
	capture := indexOfCall(t, calls, "Snapshot box=alicebox name=web-700101-0000 owner=alice")
	bind := indexOfCall(t, calls, "Bind alice tag=web snapshot=web-700101-0000")
	if capture < 0 || bind < 0 || capture > bind {
		t.Errorf("capture at %d, bind at %d — the capture has to come first: a tag bound to an image "+
			"that does not exist yet breaks every create in that window\n%v", capture, bind, calls)
	}
}

// TestABindFailureKeepsTheSnapshot. The expensive, unrepeatable half succeeded —
// the box is paused now, and a re-run captures a different filesystem — so the
// answer is a sentence carrying the one-command repair, not a rollback to a
// state `snapshot rm` already reaches.
func TestABindFailureKeepsTheSnapshot(t *testing.T) {
	r := selfRig(t)
	r.bindings.err = errors.New("database is locked")
	res, err := r.ops.SnapshotToTag(context.Background(), alice(), SnapshotToTagArgs{
		Sandbox: "alicebox", Name: "web-700101-0000", Tag: "web",
	})
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("SnapshotToTag: want *ctlops.Error, got %v", err)
	}
	if e.Code != "snapshot_not_bound" || e.Kind != KindConflict {
		t.Errorf("error = %v/%q, want conflict/snapshot_not_bound", e.Kind, e.Code)
	}
	if !strings.Contains(e.Msg, "ssh ctl@example.test snapshot bind web-700101-0000 --tag web") {
		t.Errorf("the repair command is not in the sentence:\n%s", e.Msg)
	}
	if res.Snapshot.Name != "web-700101-0000" {
		t.Errorf("the populated snapshot did not come back with the error: %+v", res.Snapshot)
	}
	if res.Bound {
		t.Error("Bound is true on a binding that failed")
	}
	if _, ok := r.tmpl.snaps["alice/web-700101-0000"]; !ok {
		t.Error("the snapshot was rolled back; two minutes of the user's time went with it")
	}
}

// TestSnapshotToTagWithNoTagIsExactlyCreateSnapshot, which is what keeps every
// caller that never asked for a binding byte-identical to what shipped.
func TestSnapshotToTagWithNoTagIsExactlyCreateSnapshot(t *testing.T) {
	r := selfRig(t)
	res, err := r.ops.SnapshotToTag(context.Background(), alice(), SnapshotToTagArgs{
		Sandbox: "alicebox", Name: "plain",
	})
	if err != nil {
		t.Fatalf("SnapshotToTag: %v", err)
	}
	if res.Bound || res.Tag != "" {
		t.Errorf("result = %+v, want no binding at all", res)
	}
	for _, call := range r.calls.all() {
		if strings.HasPrefix(call, "Bind ") {
			t.Errorf("an untagged capture reached the binding store: %s", call)
		}
	}
}

// TestSnapshotToTagRefusesABindingItCannotRecordBeforeCapturing: two minutes of
// work whose whole point was the binding must not end in "this host has nowhere
// to put one".
func TestSnapshotToTagRefusesABindingItCannotRecordBeforeCapturing(t *testing.T) {
	r := selfRig(t)
	r.ops.templateTags = nil
	_, err := r.ops.SnapshotToTag(context.Background(), alice(), SnapshotToTagArgs{
		Sandbox: "alicebox", Name: "web-700101-0000", Tag: "web",
	})
	if !IsKind(err, KindDisabled) {
		t.Fatalf("SnapshotToTag = %v, want KindDisabled", err)
	}
	if mutations := r.calls.mutating(); len(mutations) > 0 {
		t.Errorf("it captured anyway: %v", mutations)
	}
}
