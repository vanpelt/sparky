package ctlops

// Creating on a named machine: CreateArgs.Node.
//
// What is under test here is only the dispatch — that the name reaches the
// store, that a store which cannot place says so, and that naming a machine
// changes nothing about the order in which tags are written and rolled back.
// Which machine can actually take a sandbox is internal/fleet's decision and is
// tested there; this package authorizes and orders, it does not place.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
)

// placingSandboxes is a store that can build on a named machine — the shape
// internal/fleet has and *host.Manager deliberately does not.
type placingSandboxes struct {
	*fakeSandboxes
	err error // returned by CreateOn when set
}

func (p *placingSandboxes) CreateOn(ctx context.Context, node, name, owner, image string, vcpus, memMB int64) (*host.Sandbox, error) {
	p.c.add("CreateOn %s node=%s owner=%s image=%s", name, node, owner, image)
	if p.err != nil {
		return nil, p.err
	}
	b, err := p.fakeSandboxes.Create(ctx, name, owner, image, vcpus, memMB)
	if err != nil {
		return nil, err
	}
	b.Node = node
	p.boxes[name].Node = node
	return b, nil
}

func TestCreateOnANamedNode(t *testing.T) {
	r := newRig(t)
	store := &placingSandboxes{fakeSandboxes: r.boxes}
	r.ops.boxes = store
	r.calls.reset()

	box, err := r.ops.Create(context.Background(), alice(),
		CreateArgs{Name: "faraway", Node: "dgx", Tags: []string{"ml"}})
	if err != nil {
		t.Fatalf("create on dgx: %v", err)
	}
	if box.Node != "dgx" {
		t.Errorf("the record says node %q, want the machine that was asked for", box.Node)
	}
	calls := r.calls.all()
	if indexOfCall(t, calls, "CreateOn faraway node=dgx owner=alice image=base") < 0 {
		t.Fatalf("the named machine never reached the store: %v", calls)
	}
	// The ordering the whole method exists to guarantee is unchanged: the name
	// is checked, the tags are written, and only then is anything built.
	if got := r.calls.all(); indexOfCall(t, got, "SetTags faraway owner=alice tags=[ml]") >
		indexOfCall(t, got, "CreateOn faraway node=dgx owner=alice image=base") {
		t.Errorf("tags were stamped after the create: %v", got)
	}

	// And an unnamed create still takes the plain path, so nothing about a
	// single-box deployment changes.
	r.calls.reset()
	if _, err := r.ops.Create(context.Background(), alice(), CreateArgs{Name: "here"}); err != nil {
		t.Fatalf("create with no node: %v", err)
	}
	if got := r.calls.all(); indexOfCall(t, got, "Create here owner=alice image=base") < 0 {
		t.Fatalf("an unnamed create did not use the ordinary path: %v", got)
	}
}

// A host with one machine cannot answer a question about a second, and says so
// rather than quietly building the sandbox where the caller did not ask for it.
func TestCreateOnANamedNodeWithoutAFleet(t *testing.T) {
	r := newRig(t) // fakeSandboxes has no CreateOn, exactly as *host.Manager has none
	r.calls.reset()

	_, err := r.ops.Create(context.Background(), alice(), CreateArgs{Name: "faraway", Node: "dgx"})
	if !IsKind(err, KindDisabled) {
		t.Fatalf("err = %v, want KindDisabled", err)
	}
	if got := r.calls.mutating(); len(got) != 0 {
		t.Fatalf("a refused placement still reached a mutating store call: %v", got)
	}
}

// A refusal from the placing store rolls the tags back exactly as a refusal
// from an ordinary create does — the rollback is keyed on the create failing,
// not on which of the two calls made it.
func TestCreateOnARefusingNodeRollsTagsBack(t *testing.T) {
	r := newRig(t)
	r.ops.boxes = &placingSandboxes{fakeSandboxes: r.boxes, err: errors.New("node said no")}
	r.calls.reset()

	if _, err := r.ops.Create(context.Background(), alice(),
		CreateArgs{Name: "doomed", Node: "dgx", Tags: []string{"ml"}}); err == nil {
		t.Fatal("create: want an error")
	}
	if tags := r.tagger.tags["doomed"]; len(tags) != 0 {
		t.Errorf("tag rows survived a refused placement: %v", tags)
	}
}

// ---------------------------------------------------------------------------
// Placement that a template binding chose
//
// A snapshot is a file in ONE machine's image directory, so binding a tag to
// one quietly turns `--tag cuda` into a placement directive. These tests pin
// the three things that makes true: it follows the template, it says so when
// the caller asked for somewhere else, and it explains itself when the machine
// holding the template cannot take the sandbox.
// ---------------------------------------------------------------------------

// A create carrying a bound tag lands on the machine holding the template, with
// no --node typed anywhere.
func TestBoundTemplatePlacesOnItsSnapshotsNode(t *testing.T) {
	r := newRig(t)
	r.ops.boxes = &placingSandboxes{fakeSandboxes: r.boxes}
	r.tmpl.snaps["alice/alicesnap"].Node = "dgx"
	r.bindings.bind("alice", "cuda", "alicesnap")
	r.calls.reset()

	if _, err := r.ops.Create(context.Background(), alice(),
		CreateArgs{Name: "trained", Tags: []string{"cuda"}}); err != nil {
		t.Fatalf("create on a bound tag: %v", err)
	}
	got := r.calls.all()
	if indexOfCall(t, got, "CreateOn trained node=dgx owner=alice image=snap-alice-alicesnap") < 0 {
		t.Fatalf("placement did not follow the template: %v", got)
	}
}

// TestBoundTemplateOnASingleMachineHostStillBuildsHere is the regression guard
// for the whole feature on every one-machine deployment.
//
// host.NewManager coerces an unset node name to "local" (manager.go:748) and
// load() re-stamps every snapshot record with it (:789), so a single-box host's
// snapshots ALL carry Node="local". If build() reads tpl.Node before asserting
// placer, every tag-templated create here answers "this host runs a single
// machine, so a sandbox can't be placed on a named one." The absence of
// CreateOn IS the statement that there is one machine and the template is on it.
func TestBoundTemplateOnASingleMachineHostStillBuildsHere(t *testing.T) {
	r := newRig(t) // fakeSandboxes has no CreateOn, exactly as *host.Manager has none
	r.tmpl.snaps["alice/alicesnap"].Node = "local"
	r.bindings.bind("alice", "cuda", "alicesnap")
	r.calls.reset()

	if _, err := r.ops.Create(context.Background(), alice(),
		CreateArgs{Name: "homebody", Tags: []string{"cuda"}}); err != nil {
		t.Fatalf("a bound tag on a single-machine host was refused: %v", err)
	}
	got := r.calls.all()
	if indexOfCall(t, got, "Create homebody owner=alice image=snap-alice-alicesnap") < 0 {
		t.Fatalf("the ordinary create path was not taken: %v", got)
	}
}

// An unstamped node is not a directive. A record written before hosts had names
// carries none, and on a fleet that must mean "let the store choose" rather than
// "place on the machine called empty string".
func TestBoundTemplateWithNoNodeIsNotAPlacementDirective(t *testing.T) {
	r := newRig(t)
	r.ops.boxes = &placingSandboxes{fakeSandboxes: r.boxes}
	r.tmpl.snaps["alice/alicesnap"].Node = ""
	r.bindings.bind("alice", "cuda", "alicesnap")
	r.calls.reset()

	if _, err := r.ops.Create(context.Background(), alice(),
		CreateArgs{Name: "unstamped", Tags: []string{"cuda"}}); err != nil {
		t.Fatalf("create: %v", err)
	}
	got := r.calls.all()
	if indexOfCall(t, got, "CreateOn unstamped") >= 0 {
		t.Fatalf("an empty node was treated as a machine name: %v", got)
	}
	if indexOfCall(t, got, "Create unstamped owner=alice image=snap-alice-alicesnap") < 0 {
		t.Fatalf("the template's image was not used: %v", got)
	}
}

// Both overrides are wrong, so the combination is a refusal: honouring --node
// would silently build from the default image, honouring the binding would
// silently build where the caller said not to.
func TestExplicitNodeAgainstABoundTemplateIsRefused(t *testing.T) {
	r := newRig(t)
	r.ops.boxes = &placingSandboxes{fakeSandboxes: r.boxes}
	r.tmpl.snaps["alice/alicesnap"].Node = "dgx"
	r.bindings.bind("alice", "cuda", "alicesnap")
	r.calls.reset()

	_, err := r.ops.Create(context.Background(), alice(),
		CreateArgs{Name: "contradiction", Node: "laptop", Tags: []string{"cuda"}})
	if !IsKind(err, KindConflict) {
		t.Fatalf("err = %v, want KindConflict", err)
	}
	var e *Error
	errors.As(err, &e)
	if e.Code != "template_node_conflict" || !e.Verbatim {
		t.Errorf("got %s (verbatim=%v), want template_node_conflict", e.Code, e.Verbatim)
	}
	for _, want := range []string{"cuda", "alicesnap", "dgx", "laptop"} {
		if !strings.Contains(e.Msg, want) {
			t.Errorf("refusal %q does not name %q", e.Msg, want)
		}
	}
	// MachineNamed answers with the machine the sentence is ABOUT, which is
	// where the template is rather than where the caller pointed.
	if node, ok := MachineNamed(err); !ok || node != "dgx" {
		t.Errorf("MachineNamed = %q/%v, want the machine holding the template", node, ok)
	}
	if got := r.calls.mutating(); len(got) != 0 {
		t.Fatalf("a refused create still wrote something: %v", got)
	}
}

// Naming the machine the template is already on is agreement, not a conflict.
func TestExplicitNodeMatchingTheBindingIsAllowed(t *testing.T) {
	r := newRig(t)
	r.ops.boxes = &placingSandboxes{fakeSandboxes: r.boxes}
	r.tmpl.snaps["alice/alicesnap"].Node = "dgx"
	r.bindings.bind("alice", "cuda", "alicesnap")
	r.calls.reset()

	if _, err := r.ops.Create(context.Background(), alice(),
		CreateArgs{Name: "agreeing", Node: "dgx", Tags: []string{"cuda"}}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if got := r.calls.all(); indexOfCall(t, got, "CreateOn agreeing node=dgx owner=alice image=snap-alice-alicesnap") < 0 {
		t.Fatalf("a matching --node did not build: %v", got)
	}
}

// atCapacity is the shape internal/fleet raises when the machine a placement
// chose cannot take the sandbox: already classified, already curated, and about
// a machine the caller never named.
func atCapacity() *Error {
	return &Error{
		Kind: KindCapacity, Op: "create", Code: "host_at_capacity",
		Msg:      "host is at capacity (30000/32000 MB allocated)",
		Hint:     "Try again shortly, or pause a sandbox.",
		Details:  map[string]any{"used_mb": int64(30000), "budget_mb": int64(32000)},
		Verbatim: true,
	}
}

// Without the rewrite, `--tag cuda` fails with a sentence about a machine the
// user never mentioned and nothing anywhere connects the two.
func TestTemplatePlacementFailureExplainsTheBinding(t *testing.T) {
	r := newRig(t)
	r.ops.boxes = &placingSandboxes{fakeSandboxes: r.boxes, err: atCapacity()}
	r.tmpl.snaps["alice/alicesnap"].Node = "dgx"
	r.bindings.bind("alice", "cuda", "alicesnap")
	r.calls.reset()

	_, err := r.ops.Create(context.Background(), alice(),
		CreateArgs{Name: "doomed", Tags: []string{"cuda"}})
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("err = %v, want a *ctlops.Error", err)
	}
	// What happened is unchanged; only the explanation grew.
	if e.Kind != KindCapacity || e.Code != "host_at_capacity" || e.HTTPStatus() != 503 {
		t.Errorf("classification = %v/%s/%d, want the fleet's own", e.Kind, e.Code, e.HTTPStatus())
	}
	if !strings.Contains(e.Msg, "host is at capacity") {
		t.Errorf("msg %q dropped the original sentence", e.Msg)
	}
	// The explanation is in Msg, not Hint: sshgw prints only Msg.
	for _, want := range []string{"cuda", "alicesnap", "dgx"} {
		if !strings.Contains(e.Msg, want) {
			t.Errorf("msg %q does not name %q", e.Msg, want)
		}
	}
	if e.Details["template_tag"] != "cuda" || e.Details["template_snapshot"] != "alicesnap" ||
		e.Details["node"] != "dgx" || e.Details["used_mb"] != int64(30000) {
		t.Errorf("details = %v, want the binding added and the original kept", e.Details)
	}
	if tags := r.tagger.tags["doomed"]; len(tags) != 0 {
		t.Errorf("tag rows survived a failed placement: %v", tags)
	}
}

// AsError hands back the SAME pointer for an already-classified error, so
// rewriting in place would edit internal/fleet's own value — still held by
// whatever raised it. templatePlacementFailed must copy first.
func TestTemplatePlacementFailureDoesNotRewriteTheOriginalError(t *testing.T) {
	r := newRig(t)
	orig := atCapacity()
	wantMsg, wantHint, wantDetails := orig.Msg, orig.Hint, len(orig.Details)
	r.ops.boxes = &placingSandboxes{fakeSandboxes: r.boxes, err: orig}
	r.tmpl.snaps["alice/alicesnap"].Node = "dgx"
	r.bindings.bind("alice", "cuda", "alicesnap")

	got, err := r.ops.Create(context.Background(), alice(),
		CreateArgs{Name: "doomed", Tags: []string{"cuda"}})
	if err == nil {
		t.Fatalf("create = %+v, want a failure", got)
	}
	if orig.Msg != wantMsg {
		t.Errorf("the fleet's own error was rewritten: %q, want %q", orig.Msg, wantMsg)
	}
	if orig.Hint != wantHint {
		t.Errorf("the fleet's own hint was rewritten: %q", orig.Hint)
	}
	if len(orig.Details) != wantDetails {
		t.Errorf("the fleet's own details map was written into: %v", orig.Details)
	}
	if _, added := orig.Details["template_tag"]; added {
		t.Error("the details map was shared rather than cloned")
	}
	var e *Error
	errors.As(err, &e)
	if e == orig {
		t.Error("the returned error is the same value the store raised")
	}
}

// A failure with no binding behind it is left exactly as it came: the caller
// named the machine themselves and already knows why the sentence is about it.
func TestTemplatePlacementFailureLeavesAnExplicitNodeAlone(t *testing.T) {
	r := newRig(t)
	r.ops.boxes = &placingSandboxes{fakeSandboxes: r.boxes, err: atCapacity()}
	r.calls.reset()

	_, err := r.ops.Create(context.Background(), alice(),
		CreateArgs{Name: "doomed", Node: "dgx", Tags: []string{"ml"}})
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("err = %v, want a *ctlops.Error", err)
	}
	if e.Msg != atCapacity().Msg {
		t.Errorf("an unbound placement failure was rewritten: %q", e.Msg)
	}
	if _, added := e.Details["template_tag"]; added {
		t.Errorf("details gained a binding that was never involved: %v", e.Details)
	}
}
