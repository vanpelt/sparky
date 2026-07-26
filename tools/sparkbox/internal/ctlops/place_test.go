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
