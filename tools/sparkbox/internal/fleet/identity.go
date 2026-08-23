package fleet

// Workload identity for a sandbox on another machine.
//
// This is the only pair of requests in the system that travel node -> gateway,
// and the reason is narrow: a sandbox's id token is signed by the fleet's OIDC
// key, that key lives on the gateway, and it must not be copied anywhere else.
// Everything else about the metadata service — deciding WHICH sandbox is asking
// — is a property of the tap the request arrived on and is answered by the
// machine holding it, unchanged, on every host. See internal/metadata.
//
// # What makes this safe
//
// A node names a sandbox. That is the entire request: no owner, no image, no
// claims. Everything that ends up in the token, the gateway resolves for itself
// — the owner from the placement ledger, the sandbox's own facts from the cache
// that node's inventory already populated, the `box` claim from the
// AUTHENTICATED link name. So there is nothing a node can assert its way into.
//
// The one check that carries the weight is the first one: the ledger must place
// that sandbox on the node that asked. Without it any linked machine could mint
// a token for any sandbox in the fleet by guessing its name, and the token
// would be indistinguishable from a real one — signed by the right key, with
// the right owner, for a VM on somebody else's hardware.
//
// # What it does NOT grant
//
// A node holding a sandbox can already read every token that sandbox is given:
// it holds the rootfs, it holds the guest's memory, and it is the host end of
// the tap the token crosses. Relaying the mint therefore adds no capability to
// a machine the gateway has already trusted with the VM. The placement check is
// what keeps that trust scoped to the VMs it was granted for, rather than
// widened to the fleet.

import (
	"context"
	"errors"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/nodelink"
)

// Identity mints workload credentials for a sandbox. It is the gateway's own
// signing path, reached from here on behalf of a machine that has none.
//
// Box is the machine the sandbox runs on, for the `box` claim. It is a separate
// argument rather than a field on the sandbox because the record this package
// hands over is assembled from the ledger and a node's cache, and the machine
// name has to come from the link — not from either of those.
type Identity interface {
	Issue(ctx context.Context, box *host.Sandbox, node, aud string) (jwt string, expires time.Time, err error)
	Describe(ctx context.Context, box *host.Sandbox, node string) (issuer string, claims Claims, err error)
}

// Claims is the unsigned half of an identity: what a guest reads out of
// /run/sparkbox/identity.json. It carries no registered JWT claims because
// nothing here is signed.
type Claims struct {
	Subject   string
	Owner     string
	GitHub    string
	KeyFP     string
	Sandbox   string
	SandboxID string
	Image     string
	Box       string
}

// SetIdentity installs the signing path. Nil — the default — means this
// deployment issues no workload identity, and a node that asks is told so.
func (f *Fleet) SetIdentity(id Identity) { f.identity = id }

// IdentityToken answers a node asking for one of its sandboxes' id tokens.
//
// node is the AUTHENTICATED link name. It is the first argument for the same
// reason it is on every event hook: nothing in the request body may be allowed
// to stand in for it.
func (f *Fleet) IdentityToken(ctx context.Context, node string, req nodelink.IdentityReq) (nodelink.IdentityTokenResp, error) {
	box, err := f.identityBox(node, req.Sandbox)
	if err != nil {
		return nodelink.IdentityTokenResp{}, err
	}
	jwt, exp, err := f.identity.Issue(ctx, box, node, req.Aud)
	if err != nil {
		return nodelink.IdentityTokenResp{}, identityFailure(err)
	}
	f.log.Info("minted id token for a sandbox on another machine",
		"sandbox", box.Name, "owner", box.Owner, "node", node, "aud", req.Aud, "exp", exp)
	return nodelink.IdentityTokenResp{Token: jwt, ExpiresAt: exp}, nil
}

// IdentityDoc answers the same question without minting anything, so a guest
// can read who it is without burning a single-use jti.
func (f *Fleet) IdentityDoc(ctx context.Context, node string, req nodelink.IdentityReq) (nodelink.IdentityDocResp, error) {
	box, err := f.identityBox(node, req.Sandbox)
	if err != nil {
		return nodelink.IdentityDocResp{}, err
	}
	issuer, c, err := f.identity.Describe(ctx, box, node)
	if err != nil {
		return nodelink.IdentityDocResp{}, identityFailure(err)
	}
	return nodelink.IdentityDocResp{
		Issuer: issuer, Subject: c.Subject, Owner: c.Owner, GitHub: c.GitHub,
		KeyFP: c.KeyFP, Sandbox: c.Sandbox, SandboxID: c.SandboxID,
		Image: c.Image, Box: c.Box,
	}, nil
}

// identityBox is the authorization, and it is the whole of it.
//
// It resolves the sandbox the way the gateway would for any other purpose —
// through the ledger — and then refuses unless the ledger places it on the
// machine that asked. The record handed back carries the LEDGER's owner, not
// the node's, because Fleet.Get runs every remote record through serve, which
// stamps the owner column over whatever the machine reported.
func (f *Fleet) identityBox(node, name string) (*host.Sandbox, error) {
	if f.identity == nil {
		return nil, &ctlops.Error{
			Kind: ctlops.KindDisabled, Op: nodelink.OpLink, Code: nodelink.CodeNoIssuer, Verbatim: true,
			Msg: "this gateway issues no workload identity: it has no OIDC signing key configured.",
		}
	}
	box, ok := f.Get(name)
	if !ok || box.Node != node {
		// One sentence for both, deliberately. "That sandbox is not on your
		// machine" and "there is no such sandbox" are the same answer to a
		// machine that should not be asking, and telling the two apart would
		// turn this into an oracle for which names exist elsewhere in the
		// fleet. The gateway's own log line below carries the distinction for
		// whoever has to investigate it.
		f.log.Warn("refused an identity request for a sandbox that machine does not hold",
			"node", node, "sandbox", name, "found", ok,
			"placed_on", placedOn(box, ok))
		return nil, &ctlops.Error{
			Kind: ctlops.KindDenied, Op: nodelink.OpLink, Code: nodelink.CodeNotYours, Verbatim: true,
			Msg: "this gateway places no sandbox named " + name + " on " + node + ".",
		}
	}
	// An owner-less row is a sandbox the ledger cannot speak for, and a token
	// whose subject would be "sparkbox:user:" — a valid-looking credential for
	// nobody. Refusing is the only safe reading.
	if box.Owner == "" {
		return nil, &ctlops.Error{
			Kind: ctlops.KindDenied, Op: nodelink.OpLink, Code: nodelink.CodeNotYours, Verbatim: true,
			Msg: "this gateway has no owner recorded for " + name + ", so it can issue no identity for it.",
		}
	}
	return box, nil
}

func placedOn(box *host.Sandbox, ok bool) string {
	if !ok || box == nil {
		return ""
	}
	return box.Node
}

// identityFailure keeps a typed refusal typed and turns everything else into
// one. A raw signing error would otherwise cross the link as an untyped string
// and be rebuilt by the node as a generic failure, which the guest is told is a
// 500 — right, but by accident.
func identityFailure(err error) error {
	var typed *ctlops.Error
	if errors.As(err, &typed) {
		return err
	}
	return &ctlops.Error{
		Kind: ctlops.KindInternal, Op: nodelink.OpLink, Code: "identity_mint_failed", Verbatim: true,
		Msg: "this gateway could not issue an identity for that sandbox.",
	}
}
