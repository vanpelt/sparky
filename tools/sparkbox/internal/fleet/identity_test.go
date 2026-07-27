package fleet_test

// The identity relay's authorization, which is one check and carries all the
// weight: a node may be spoken for about the sandboxes the LEDGER places on it,
// and about nothing else.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/fleet"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/nodelink"
)

// recordingIdentity is a signing path that mints a predictable string and
// remembers exactly what it was asked to speak about. What it records is the
// whole point of these tests: the claims must be built from what the FLEET
// resolved, never from what the node sent.
type recordingIdentity struct {
	box  *host.Sandbox
	node string
	aud  string
	err  error
}

func (r *recordingIdentity) Issue(_ context.Context, box *host.Sandbox, node, aud string) (string, time.Time, error) {
	r.box, r.node, r.aud = box, node, aud
	if r.err != nil {
		return "", time.Time{}, r.err
	}
	return "jwt-for-" + box.Name, time.Unix(1700000000, 0), nil
}

func (r *recordingIdentity) Describe(_ context.Context, box *host.Sandbox, node string) (string, fleet.Claims, error) {
	r.box, r.node = box, node
	if r.err != nil {
		return "", fleet.Claims{}, r.err
	}
	return "https://oidc.example.test", fleet.Claims{
		Subject: "sparkbox:user:" + box.Owner, Owner: box.Owner,
		Sandbox: box.Name, Image: box.Image, Box: node,
	}, nil
}

// identityFleet builds a gateway with one node attached and a sandbox placed on
// it, which is the shape every test below varies one thing about.
func identityFleet(t *testing.T) (*fleet.Fleet, *fakeNode, *recordingIdentity) {
	t.Helper()
	mgr := newManager(t, host.Options{})
	index := newIndex(t)
	f := newFleet(t, mgr, index)
	id := &recordingIdentity{}
	f.SetIdentity(id)

	n := newFakeNode("laptop")
	attach(t, f, n, &host.Sandbox{Name: "alices-box", Owner: "alice", Image: "universal", KeyFP: "SHA256:aaa"})
	place(t, index, "alices-box", "alice", "laptop")
	return f, n, id
}

func TestIdentityTokenIsIssuedForASandboxOnTheAskingNode(t *testing.T) {
	f, _, id := identityFleet(t)
	resp, err := f.IdentityToken(context.Background(), "laptop",
		nodelink.IdentityReq{Sandbox: "alices-box", Aud: "https://hivemind.example"})
	if err != nil {
		t.Fatalf("IdentityToken: %v", err)
	}
	if resp.Token != "jwt-for-alices-box" {
		t.Errorf("token = %q", resp.Token)
	}
	// The `box` claim comes from the AUTHENTICATED link name, not the payload.
	if id.node != "laptop" {
		t.Errorf("node = %q, want laptop", id.node)
	}
	// And the owner from the ledger, not from anything the node reported.
	if id.box.Owner != "alice" {
		t.Errorf("owner = %q, want alice", id.box.Owner)
	}
	if id.aud != "https://hivemind.example" {
		t.Errorf("aud = %q", id.aud)
	}
}

// The check the whole relay rests on. A second machine asking about a sandbox
// the ledger places on the first must be refused — otherwise any linked machine
// mints tokens for the whole fleet by guessing names, and the tokens are
// indistinguishable from real ones.
func TestIdentityRefusesASandboxOnAnotherNode(t *testing.T) {
	f, _, id := identityFleet(t)
	other := newFakeNode("intruder")
	attach(t, f, other)

	_, err := f.IdentityToken(context.Background(), "intruder",
		nodelink.IdentityReq{Sandbox: "alices-box"})
	if err == nil {
		t.Fatal("a node was spoken for about a sandbox the ledger places elsewhere")
	}
	var typed *ctlops.Error
	if !errors.As(err, &typed) || typed.Code != nodelink.CodeNotYours {
		t.Fatalf("err = %v (code %v), want %s", err, codeOf(err), nodelink.CodeNotYours)
	}
	if id.box != nil {
		t.Error("the signing path was reached for a refused request")
	}
}

// A name nothing places anywhere must be refused with the SAME error as one
// placed on somebody else: telling the two apart would make this an oracle for
// which sandbox names exist elsewhere in the fleet.
func TestIdentityRefusesAnUnknownSandboxIndistinguishably(t *testing.T) {
	f, _, _ := identityFleet(t)
	_, missing := f.IdentityToken(context.Background(), "laptop",
		nodelink.IdentityReq{Sandbox: "no-such-box"})
	_, elsewhere := f.IdentityToken(context.Background(), "laptop",
		nodelink.IdentityReq{Sandbox: "alices-box"})
	if missing == nil {
		t.Fatal("an unplaced name was accepted")
	}
	if elsewhere != nil {
		t.Fatalf("the control case failed: %v", elsewhere)
	}
	// Now ask a machine that holds neither, so both are refusals, and compare.
	other := newFakeNode("intruder")
	attach(t, f, other)
	_, a := f.IdentityToken(context.Background(), "intruder", nodelink.IdentityReq{Sandbox: "no-such-box"})
	_, b := f.IdentityToken(context.Background(), "intruder", nodelink.IdentityReq{Sandbox: "alices-box"})
	if codeOf(a) != codeOf(b) {
		t.Errorf("refusal codes differ: unknown=%s placed-elsewhere=%s", codeOf(a), codeOf(b))
	}
}

// A gateway with no signing key must say so rather than leave the node waiting
// or answer with an unregistered-type error a version skew also produces.
func TestIdentityRefusesWhenTheGatewayHasNoIssuer(t *testing.T) {
	mgr := newManager(t, host.Options{})
	f := newFleet(t, mgr, newIndex(t))
	_, err := f.IdentityToken(context.Background(), "laptop", nodelink.IdentityReq{Sandbox: "anything"})
	if codeOf(err) != nodelink.CodeNoIssuer {
		t.Fatalf("err = %v (code %s), want %s", err, codeOf(err), nodelink.CodeNoIssuer)
	}
}

// The unsigned half routes through exactly the same gate.
func TestIdentityDocHonoursThePlacementCheck(t *testing.T) {
	f, _, _ := identityFleet(t)
	doc, err := f.IdentityDoc(context.Background(), "laptop", nodelink.IdentityReq{Sandbox: "alices-box"})
	if err != nil {
		t.Fatalf("IdentityDoc: %v", err)
	}
	if doc.Owner != "alice" || doc.Box != "laptop" || doc.Issuer != "https://oidc.example.test" {
		t.Errorf("doc = %+v", doc)
	}
	other := newFakeNode("intruder")
	attach(t, f, other)
	if _, err := f.IdentityDoc(context.Background(), "intruder", nodelink.IdentityReq{Sandbox: "alices-box"}); codeOf(err) != nodelink.CodeNotYours {
		t.Errorf("cross-node /identity = %v, want %s", err, nodelink.CodeNotYours)
	}
}

// A signing failure must not cross the link as a raw string: the node maps
// typed codes onto the status its guest is told, and an untyped error would be
// classified by accident.
func TestIdentityMintFailureIsTyped(t *testing.T) {
	f, _, id := identityFleet(t)
	id.err = errors.New("the key is on fire")
	_, err := f.IdentityToken(context.Background(), "laptop", nodelink.IdentityReq{Sandbox: "alices-box"})
	var typed *ctlops.Error
	if !errors.As(err, &typed) {
		t.Fatalf("err = %v, want a typed error", err)
	}
	if strings.Contains(typed.Msg, "on fire") {
		t.Errorf("the internal cause leaked to the node: %q", typed.Msg)
	}
}

func codeOf(err error) string {
	var typed *ctlops.Error
	if errors.As(err, &typed) {
		return typed.Code
	}
	return ""
}
