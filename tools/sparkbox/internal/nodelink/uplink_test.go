package nodelink

// The node's request channel for workload identity, guest route controls and
// repo attachments.

import (
	"bytes"
	"context"
	"errors"
	"strings"
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
		registerUplinkOps(c, "laptop", Hooks{
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
	node, _ := newPipePair(t, nil, func(c *Conn) { registerUplinkOps(c, "laptop", Hooks{}) })
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
		registerUplinkOps(c, "laptop", Hooks{
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

func TestGuestRouteRequestsReachGatewayWithAuthenticatedNode(t *testing.T) {
	var gotNode string
	node, _ := newPipePair(t, nil, func(c *Conn) {
		registerUplinkOps(c, "roster-node", Hooks{
			OnSelfVisibility: func(_ context.Context, n string, req SelfVisibilityReq) (SelfVisibilityResp, error) {
				gotNode = n
				return SelfVisibilityResp{Sandbox: req.Sandbox, Visibility: req.Visibility, Routes: 2}, nil
			},
			OnSelfPort: func(_ context.Context, n string, req SelfPortReq) (SelfPortResp, error) {
				gotNode = n
				return SelfPortResp{Sandbox: req.Sandbox, Port: req.Port}, nil
			},
		})
	})
	u := NewUplink()
	defer u.attach(node)()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var visibility SelfVisibilityResp
	if err := u.Request(ctx, TypeSelfVisibility,
		SelfVisibilityReq{Sandbox: "alices-box", Visibility: "public"}, &visibility); err != nil {
		t.Fatal(err)
	}
	if gotNode != "roster-node" || visibility.Visibility != "public" || visibility.Routes != 2 {
		t.Errorf("visibility response = %+v, node = %q", visibility, gotNode)
	}

	var port SelfPortResp
	if err := u.Request(ctx, TypeSelfPort, SelfPortReq{Sandbox: "alices-box", Port: 5173}, &port); err != nil {
		t.Fatal(err)
	}
	if gotNode != "roster-node" || port.Port != 5173 {
		t.Errorf("port response = %+v, node = %q", port, gotNode)
	}
}

func TestCertificateEnrollmentReachesHookWithAuthenticatedNode(t *testing.T) {
	var gotNode string
	var gotCSR []byte
	expires := time.Now().Add(time.Hour).UTC()
	node, _ := newPipePair(t, nil, func(c *Conn) {
		registerCertificateEnroll(c, "roster-node", Hooks{
			OnCertificateEnroll: func(_ context.Context, authenticated string, request CertificateEnrollRequest) (CertificateEnrollResponse, error) {
				gotNode = authenticated
				gotCSR = append([]byte(nil), request.CSRPEM...)
				return CertificateEnrollResponse{
					CertificatePEM:   []byte("leaf PEM"),
					CACertificatePEM: []byte("CA PEM"),
					GatewayIdentity:  "spiffe://sparkbox/gateway/cluster-a",
					GatewayGRPCAddr:  "gateway.tailnet:9444",
					Serial:           "1234abcd",
					ExpiresAt:        expires,
				}, nil
			},
		})
	})
	uplink := NewUplink()
	defer uplink.attach(node)()

	requestCSR := []byte("node CSR")
	response, err := uplink.EnrollCertificate(context.Background(), requestCSR)
	if err != nil {
		t.Fatal(err)
	}
	if gotNode != "roster-node" {
		t.Fatalf("hook node = %q, want authenticated roster-node", gotNode)
	}
	if !bytes.Equal(gotCSR, requestCSR) {
		t.Fatalf("hook CSR = %q", gotCSR)
	}
	if response.Serial != "1234abcd" || response.GatewayIdentity != "spiffe://sparkbox/gateway/cluster-a" ||
		response.GatewayGRPCAddr != "gateway.tailnet:9444" ||
		!response.ExpiresAt.Equal(expires) {
		t.Fatalf("response = %+v", response)
	}
}

func TestCertificateEnrollmentGatewayGRPCAddressIsOptionalButValidated(t *testing.T) {
	response := CertificateEnrollResponse{
		CertificatePEM:   []byte("leaf"),
		CACertificatePEM: []byte("CA"),
		GatewayIdentity:  "spiffe://sparkbox/gateway/cluster-a",
		Serial:           "serial",
		ExpiresAt:        time.Now().Add(time.Hour),
	}
	// An old gateway omits the address; this is the negotiated SSH fallback.
	if err := validateCertificateEnrollResponse(response); err != nil {
		t.Fatalf("empty gateway gRPC address: %v", err)
	}
	response.GatewayGRPCAddr = "gateway.tailnet:9444"
	if err := validateCertificateEnrollResponse(response); err != nil {
		t.Fatalf("valid gateway gRPC address: %v", err)
	}
	for _, bad := range []string{"gateway.tailnet", ":9444", "gateway.tailnet:0", "gateway.tailnet:09444"} {
		response.GatewayGRPCAddr = bad
		if err := validateCertificateEnrollResponse(response); err == nil {
			t.Errorf("gateway gRPC address %q was accepted", bad)
		}
	}
}

func TestCertificateEnrollmentBoundsCSRAndClassifiesMissingIssuer(t *testing.T) {
	uplink := NewUplink()
	_, err := uplink.EnrollCertificate(context.Background(), bytes.Repeat([]byte("x"), MaxCSRPEMBytes+1))
	var typed *ctlops.Error
	if !errors.As(err, &typed) || typed.Kind != ctlops.KindInvalid ||
		typed.Code != "certificate_request_too_large" {
		t.Fatalf("oversized CSR error = %#v", err)
	}

	node, _ := newPipePair(t, nil, func(c *Conn) {
		registerCertificateEnroll(c, "roster-node", Hooks{})
	})
	defer uplink.attach(node)()
	_, err = uplink.EnrollCertificate(context.Background(), []byte("CSR"))
	if !errors.As(err, &typed) || typed.Kind != ctlops.KindDisabled ||
		typed.Code != CodeNoCertificateIssuer {
		t.Fatalf("missing issuer error = %#v", err)
	}
}

func TestCertificateEnrollmentRejectsMalformedGatewayResponse(t *testing.T) {
	node, _ := newPipePair(t, nil, func(c *Conn) {
		registerCertificateEnroll(c, "roster-node", Hooks{
			OnCertificateEnroll: func(context.Context, string, CertificateEnrollRequest) (CertificateEnrollResponse, error) {
				return CertificateEnrollResponse{
					CertificatePEM:   []byte("leaf"),
					CACertificatePEM: []byte("CA"),
					GatewayIdentity:  "https://not-spiffe.example",
					Serial:           "serial",
					ExpiresAt:        time.Now().Add(time.Hour),
				}, nil
			},
		})
	})
	uplink := NewUplink()
	defer uplink.attach(node)()
	_, err := uplink.EnrollCertificate(context.Background(), []byte("CSR"))
	var typed *ctlops.Error
	if !errors.As(err, &typed) || typed.Kind != ctlops.KindInternal ||
		typed.Code != "bad_certificate_response" {
		t.Fatalf("malformed response error = %#v", err)
	}
}

// The repo pair travels the same way, and the assertion that matters is the
// same one: the hook is handed the AUTHENTICATED link name, and the payload
// carries nothing else the gateway could mistake for one.
func TestRepoRequestsReachGatewayWithAuthenticatedNode(t *testing.T) {
	var gotNode string
	var gotCred SelfRepoCredReq
	exp := time.Unix(1700000000, 0).UTC()
	node, _ := newPipePair(t, nil, func(c *Conn) {
		registerUplinkOps(c, "roster-node", Hooks{
			OnSelfRepos: func(_ context.Context, n string, req SelfReposReq) (SelfReposResp, error) {
				gotNode = n
				return SelfReposResp{Repos: []SelfRepoEntry{
					{Host: "github.com", Slug: "wandb/hivemind", Ref: "main", Access: "write"},
				}}, nil
			},
			OnSelfRepoCred: func(_ context.Context, n string, req SelfRepoCredReq) (SelfRepoCredResp, error) {
				gotNode, gotCred = n, req
				return SelfRepoCredResp{Username: "x-access-token", Password: "ghs_token", ExpiresAt: exp}, nil
			},
		})
	})
	u := NewUplink()
	defer u.attach(node)()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var manifest SelfReposResp
	if err := u.Request(ctx, TypeSelfRepos, SelfReposReq{Sandbox: "alices-box"}, &manifest); err != nil {
		t.Fatal(err)
	}
	if gotNode != "roster-node" {
		t.Errorf("node = %q; the hook must be handed the authenticated name", gotNode)
	}
	if len(manifest.Repos) != 1 || manifest.Repos[0].Slug != "wandb/hivemind" ||
		manifest.Repos[0].Ref != "main" || manifest.Repos[0].Access != "write" {
		t.Errorf("manifest = %+v", manifest.Repos)
	}

	var cred SelfRepoCredResp
	if err := u.Request(ctx, TypeSelfRepoCred,
		SelfRepoCredReq{Sandbox: "alices-box", Slug: "wandb/hivemind"}, &cred); err != nil {
		t.Fatal(err)
	}
	if gotNode != "roster-node" || gotCred.Sandbox != "alices-box" || gotCred.Slug != "wandb/hivemind" {
		t.Errorf("credential request = %+v, node = %q", gotCred, gotNode)
	}
	if cred.Password != "ghs_token" || !cred.ExpiresAt.Equal(exp) {
		t.Errorf("credential = %+v", cred)
	}
}

// A gateway that attaches no repositories must say so in a sentence the node
// can classify into the 501 a guest is told, rather than leaving the request to
// produce the unregistered-type error a version skew also produces — the two
// have entirely different fixes.
func TestRepoRequestsWithNoHookAreRefusedInWords(t *testing.T) {
	node, _ := newPipePair(t, nil, func(c *Conn) { registerUplinkOps(c, "roster-node", Hooks{}) })
	u := NewUplink()
	defer u.attach(node)()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	for _, test := range []struct {
		typ  string
		body any
	}{
		{typ: TypeSelfRepos, body: SelfReposReq{Sandbox: "x"}},
		{typ: TypeSelfRepoCred, body: SelfRepoCredReq{Sandbox: "x", Slug: "o/n"}},
	} {
		err := u.Request(ctx, test.typ, test.body, &SelfReposResp{})
		var typed *ctlops.Error
		if !errors.As(err, &typed) || typed.Code != CodeNoRepos {
			t.Errorf("%s: err = %v, want code %s", test.typ, err, CodeNoRepos)
		}
	}
}

// Malformed requests must be refused at the door rather than reaching the
// ledger as an empty-string lookup or the GitHub API as an unbounded path.
func TestRepoRequestsAreBoundedBeforeTheHook(t *testing.T) {
	reached := false
	node, _ := newPipePair(t, nil, func(c *Conn) {
		registerUplinkOps(c, "roster-node", Hooks{
			OnSelfRepos: func(context.Context, string, SelfReposReq) (SelfReposResp, error) {
				reached = true
				return SelfReposResp{}, nil
			},
			OnSelfRepoCred: func(context.Context, string, SelfRepoCredReq) (SelfRepoCredResp, error) {
				reached = true
				return SelfRepoCredResp{}, nil
			},
		})
	})
	u := NewUplink()
	defer u.attach(node)()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	for _, test := range []struct {
		name string
		typ  string
		body any
	}{
		{name: "no sandbox", typ: TypeSelfRepos, body: SelfReposReq{}},
		{name: "credential without a sandbox", typ: TypeSelfRepoCred, body: SelfRepoCredReq{Slug: "o/n"}},
		{name: "credential without a slug", typ: TypeSelfRepoCred, body: SelfRepoCredReq{Sandbox: "x"}},
		{name: "oversized slug", typ: TypeSelfRepoCred, body: SelfRepoCredReq{
			Sandbox: "x", Slug: strings.Repeat("a", MaxRepoSlugBytes+1),
		}},
	} {
		err := u.Request(ctx, test.typ, test.body, &SelfRepoCredResp{})
		var typed *ctlops.Error
		if !errors.As(err, &typed) || typed.Kind != ctlops.KindInvalid {
			t.Errorf("%s: err = %v, want an invalid-request error", test.name, err)
		}
	}
	if reached {
		t.Error("a malformed request reached the gateway's hook")
	}
}
