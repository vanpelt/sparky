package nodelink

// The node's request channel, and the identity verbs that are the only thing
// riding it.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
)

// A node between reconnects must fail immediately and recognisably rather than
// hang until somebody else's deadline: the metadata service turns exactly this
// error into the 503 a guest's timer retries out of.
func TestUplinkWithoutALinkFailsImmediately(t *testing.T) {
	u := NewUplink()
	if u.Linked() {
		t.Error("a fresh uplink reported a link")
	}
	// A context with plenty of budget: if this blocks, it blocks for the whole
	// of it, which is the failure being ruled out.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- u.Request(ctx, TypeIdentityToken, IdentityReq{Sandbox: "x"}, &IdentityTokenResp{}) }()
	select {
	case err := <-done:
		if !errors.Is(err, ErrNoLink) {
			t.Errorf("err = %v, want ErrNoLink", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("an unlinked uplink blocked instead of failing")
	}
}

// The nil receiver is the same answer, so callers need no nil guard.
func TestNilUplinkIsUsable(t *testing.T) {
	var u *Uplink
	if u.Linked() {
		t.Error("a nil uplink reported a link")
	}
	if err := u.Request(context.Background(), TypeIdentityToken, nil, nil); !errors.Is(err, ErrNoLink) {
		t.Errorf("err = %v, want ErrNoLink", err)
	}
}

// Detach must only ever clear the connection it installed. A link tearing
// itself down after a new one has replaced it would otherwise unbind the live
// one, and the node would go silently request-less until the NEXT reconnect.
func TestUplinkDetachDoesNotUnbindItsSuccessor(t *testing.T) {
	u := NewUplink()
	first, _ := newPipePair(t, nil, nil)
	second, _ := newPipePair(t, nil, nil)

	releaseFirst := u.attach(first)
	releaseSecond := u.attach(second)
	releaseFirst() // the dead link tidying up, after the new one arrived
	if !u.Linked() {
		t.Fatal("the superseded link's detach unbound its replacement")
	}
	releaseSecond()
	if u.Linked() {
		t.Error("the live link's detach left the uplink bound")
	}
}

// The whole node -> gateway round trip: the node names a sandbox, the gateway's
// hook answers, and the reply comes back typed.
func TestIdentityRequestReachesTheGatewayHook(t *testing.T) {
	var gotNode string
	var gotReq IdentityReq
	exp := time.Unix(1700000000, 0).UTC()

	// The GATEWAY is the side that registers these, which is the inversion
	// worth being explicit about: every other Handle in this package is on the
	// node.
	node, _ := newPipePair(t, nil, func(c *Conn) {
		registerIdentityOps(c, "laptop", Hooks{
			OnIdentityToken: func(_ context.Context, n string, r IdentityReq) (IdentityTokenResp, error) {
				gotNode, gotReq = n, r
				return IdentityTokenResp{Token: "jwt", ExpiresAt: exp}, nil
			},
		})
	})

	u := NewUplink()
	defer u.attach(node)()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var resp IdentityTokenResp
	if err := u.Request(ctx, TypeIdentityToken, IdentityReq{Sandbox: "alices-box", Aud: "https://aud"}, &resp); err != nil {
		t.Fatalf("Request: %v", err)
	}
	if resp.Token != "jwt" || !resp.ExpiresAt.Equal(exp) {
		t.Errorf("resp = %+v", resp)
	}
	if gotNode != "laptop" {
		t.Errorf("node = %q; the hook must be handed the authenticated name", gotNode)
	}
	if gotReq.Sandbox != "alices-box" || gotReq.Aud != "https://aud" {
		t.Errorf("req = %+v", gotReq)
	}
}

// A gateway with no signing path must answer in a sentence the node can
// classify, not with the unregistered-type error a version skew produces —
// otherwise an operator debugging a tokenless guest is sent looking for a
// protocol mismatch that is not there.
func TestIdentityRequestWithNoHookIsRefusedInWords(t *testing.T) {
	node, _ := newPipePair(t, nil, func(c *Conn) { registerIdentityOps(c, "laptop", Hooks{}) })
	u := NewUplink()
	defer u.attach(node)()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := u.Request(ctx, TypeIdentityToken, IdentityReq{Sandbox: "x"}, &IdentityTokenResp{})
	var typed *ctlops.Error
	if !errors.As(err, &typed) || typed.Code != CodeNoIssuer {
		t.Fatalf("err = %v, want code %s", err, CodeNoIssuer)
	}
}

// A request naming no sandbox is the caller's mistake and must be told so,
// rather than reaching the ledger as an empty-string lookup.
func TestIdentityRequestWithoutASandboxIsInvalid(t *testing.T) {
	reached := false
	node, _ := newPipePair(t, nil, func(c *Conn) {
		registerIdentityOps(c, "laptop", Hooks{
			OnIdentityToken: func(context.Context, string, IdentityReq) (IdentityTokenResp, error) {
				reached = true
				return IdentityTokenResp{}, nil
			},
		})
	})
	u := NewUplink()
	defer u.attach(node)()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := u.Request(ctx, TypeIdentityToken, IdentityReq{}, &IdentityTokenResp{})
	var typed *ctlops.Error
	if !errors.As(err, &typed) || typed.Kind != ctlops.KindInvalid {
		t.Fatalf("err = %v, want an invalid-request error", err)
	}
	if reached {
		t.Error("a nameless request reached the gateway's hook")
	}
}
