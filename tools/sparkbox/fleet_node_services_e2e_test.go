package sparkbox_test

// The two node-local services, over a real link: workload identity and the
// egress plane.
//
// Everything here is exercised apart in unit tests — the placement check in
// internal/fleet, the frames in internal/nodelink, the HTTP surface in
// internal/metadata. What only this file can answer is whether the wiring
// between them meets: a request made on the node's uplink has to travel a real
// SSH connection, be attributed to the AUTHENTICATED link name by the gateway's
// door, reach the hooks fleet.ServeLink installed, and come back as something
// the node can act on. Every one of those is a seam where the pieces could be
// individually right and jointly useless.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/fleet"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/metadata"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/netpush"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/nodelink"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/oidc"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/users"
)

// fakeNodeSluice stands in for the daemon on a node's own machine: the one
// piece that cannot be real in-process, since it attaches eBPF to tap devices.
// Everything between it and the gateway is genuine.
type fakeNodeSluice struct {
	mu      sync.Mutex
	on      bool
	applied map[string][]string
	usage   map[string]netpush.VMUsage
}

func (f *fakeNodeSluice) Enabled() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.on
}

func (f *fakeNodeSluice) Apply(_ context.Context, allow map[string][]string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.applied = allow
	return nil
}

func (f *fakeNodeSluice) Usage(context.Context, string) (map[string]netpush.VMUsage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.usage, nil
}

func (f *fakeNodeSluice) lastPolicy() map[string][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.applied
}

// gatewayIdentity is cmd/sparkbox's own fleetIdentity, restated here because
// that type lives in package main and cannot be imported. It is deliberately
// the same three lines: a real oidc.Issuer, and claims assembled from what the
// FLEET resolved rather than from anything the node sent.
type gatewayIdentity struct {
	issuer *oidc.Issuer
	users  metadata.Accounts
	defAud string
}

func (g gatewayIdentity) local(node string) metadata.Local {
	return metadata.Local{Issuer: g.issuer, Users: g.users, NodeName: node}
}

func (g gatewayIdentity) Issue(ctx context.Context, box *host.Sandbox, node, aud string) (string, time.Time, error) {
	if aud == "" {
		aud = g.defAud
	}
	tok, err := g.local(node).Issue(ctx, box, aud)
	if err != nil {
		return "", time.Time{}, err
	}
	return tok.JWT, tok.ExpiresAt, nil
}

func (g gatewayIdentity) Describe(ctx context.Context, box *host.Sandbox, node string) (string, fleet.Claims, error) {
	doc, err := g.local(node).Describe(ctx, box)
	if err != nil {
		return "", fleet.Claims{}, err
	}
	return doc.Issuer, fleet.Claims{
		Subject: doc.Subject, Owner: doc.Owner, GitHub: doc.GitHub, KeyFP: doc.KeyFP,
		Sandbox: doc.Sandbox, Image: doc.Image, Box: doc.Box,
	}, nil
}

type e2eAccounts map[string]users.User

func (a e2eAccounts) Get(handle string) (users.User, error) {
	u, ok := a[handle]
	if !ok {
		return users.User{}, users.ErrNoSuchUser
	}
	return u, nil
}

// withIssuer gives the gateway a real signing path, so what comes back over the
// link is a JWT that verifies rather than a fixture string.
func withIssuer(t *testing.T, fs *fleetStack) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	iss, err := oidc.New(oidc.Options{IssuerURL: "https://oidc.example.test", Signer: key})
	if err != nil {
		t.Fatal(err)
	}
	fs.flt.SetIdentity(gatewayIdentity{
		issuer: iss,
		users:  e2eAccounts{},
		defAud: "https://hivemind.example",
	})
}

// claimsOf pulls the payload out of a compact JWS without verifying it: the
// signature is oidc's own contract and is tested there. What this file is
// asking is whose identity came back.
func claimsOf(t *testing.T, token string) map[string]any {
	t.Helper()
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 {
		t.Fatalf("not a JWT: %q", token)
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var claims map[string]any
	if err := json.Unmarshal(raw, &claims); err != nil {
		t.Fatal(err)
	}
	return claims
}

// The whole point of the identity relay, end to end: a sandbox built on the
// node gets a token signed by the gateway's key, describing the owner the
// LEDGER recorded, naming the machine the link authenticated as.
func TestNodeGetsAWorkloadTokenSignedByTheGateway(t *testing.T) {
	fs, n := newFleetStack(t)
	withIssuer(t, fs)
	defer fs.join(t, n)()

	box, err := fs.flt.CreateOn(context.Background(), n.name, "remote-box", "alice", "ubuntu", 1, 512)
	if err != nil {
		t.Fatalf("create on %s: %v", n.name, err)
	}
	if !n.holds(t, box.Name) {
		t.Fatalf("%s does not hold %q", n.name, box.Name)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var resp nodelink.IdentityTokenResp
	if err := n.uplink.Request(ctx, nodelink.TypeIdentityToken,
		nodelink.IdentityReq{Sandbox: "remote-box"}, &resp); err != nil {
		t.Fatalf("the node could not get a token for its own sandbox: %v", err)
	}
	claims := claimsOf(t, resp.Token)
	for field, want := range map[string]string{
		"iss":     "https://oidc.example.test",
		"sub":     "sparkbox:user:alice",
		"owner":   "alice",
		"sandbox": "remote-box",
		// The machine the LINK authenticated as, not one the payload named.
		"box": "node-b",
		"aud": "https://hivemind.example",
	} {
		if got, _ := claims[field].(string); got != want {
			t.Errorf("claim %s = %q, want %q", field, got, want)
		}
	}
	if resp.ExpiresAt.Before(time.Now()) {
		t.Errorf("token expires at %v, in the past", resp.ExpiresAt)
	}
	n.linkAlive(t)
}

// The unsigned half over the same link, which is what a guest reads out of
// /run/sparkbox/identity.json without burning a single-use jti.
func TestNodeCanDescribeItsSandboxWithoutMinting(t *testing.T) {
	fs, n := newFleetStack(t)
	withIssuer(t, fs)
	defer fs.join(t, n)()

	if _, err := fs.flt.CreateOn(context.Background(), n.name, "remote-box", "alice", "ubuntu", 1, 512); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var doc nodelink.IdentityDocResp
	if err := n.uplink.Request(ctx, nodelink.TypeIdentityDoc,
		nodelink.IdentityReq{Sandbox: "remote-box"}, &doc); err != nil {
		t.Fatalf("describe: %v", err)
	}
	if doc.Owner != "alice" || doc.Sandbox != "remote-box" || doc.Box != "node-b" {
		t.Errorf("doc = %+v", doc)
	}
	if doc.Issuer != "https://oidc.example.test" {
		t.Errorf("iss = %q", doc.Issuer)
	}
}

// The refusal that keeps the relay from being a fleet-wide minting oracle,
// asserted over a real link rather than against the fleet's method directly:
// the name the gateway checks against has to be the one the DOOR resolved from
// the node's key, and this is the only test that proves that plumbing.
func TestNodeCannotGetATokenForTheGatewaysOwnSandbox(t *testing.T) {
	fs, n := newFleetStack(t)
	withIssuer(t, fs)
	defer fs.join(t, n)()

	// A sandbox on the GATEWAY. The node has no business being spoken for
	// about it, however it learned the name.
	if _, err := fs.mgr.Create(context.Background(), "local-box", "alice", "ubuntu", 1, 512); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var resp nodelink.IdentityTokenResp
	err := n.uplink.Request(ctx, nodelink.TypeIdentityToken,
		nodelink.IdentityReq{Sandbox: "local-box"}, &resp)
	if err == nil {
		t.Fatalf("a node was handed a token for the gateway's own sandbox: %+v", claimsOf(t, resp.Token))
	}
	var typed *ctlops.Error
	if !errors.As(err, &typed) || typed.Code != nodelink.CodeNotYours {
		t.Fatalf("err = %v, want code %s", err, nodelink.CodeNotYours)
	}
	// And a name nothing places anywhere must be refused identically, so this
	// is no oracle for which sandboxes exist elsewhere in the fleet.
	var other nodelink.IdentityTokenResp
	missing := n.uplink.Request(ctx, nodelink.TypeIdentityToken,
		nodelink.IdentityReq{Sandbox: "no-such-box"}, &other)
	var missingTyped *ctlops.Error
	if !errors.As(missing, &missingTyped) || missingTyped.Code != typed.Code {
		t.Errorf("an unplaced name is refused differently (%v) from one on another machine (%v)", missing, err)
	}
	n.linkAlive(t)
}

// A node between reconnects must degrade, not hang: the metadata service turns
// this into the 503 a guest's own timer retries out of.
func TestIdentityRequestWithTheGatewayGoneFailsFast(t *testing.T) {
	fs, n := newFleetStack(t)
	withIssuer(t, fs)
	unplug := fs.join(t, n)
	unplug() // the gateway is now unreachable from this machine

	waitFor(t, "the node's uplink to notice the link is gone", func() bool { return !n.uplink.Linked() })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := n.uplink.Request(ctx, nodelink.TypeIdentityToken,
		nodelink.IdentityReq{Sandbox: "remote-box"}, &nodelink.IdentityTokenResp{})
	if !errors.Is(err, nodelink.ErrNoLink) {
		t.Fatalf("err = %v, want ErrNoLink so the guest is told 503 and retries", err)
	}
}

// ---------------------------------------------------------------------------
// The egress plane
// ---------------------------------------------------------------------------

// tagAllow governs the sandboxes it names and nothing else.
type tagAllow map[string][]string

func (r tagAllow) AllowForSandbox(sandbox, _ string) ([]string, bool, error) {
	allow, ok := r[sandbox]
	return allow, ok, nil
}

// A tagged sandbox on a node must actually be filtered — the gap this whole
// change closes. The gateway resolves the rule against the ledger's owner
// column and pushes it over the link; the node applies it to its own taps.
func TestEgressPolicyReachesASandboxOnANode(t *testing.T) {
	fs, n := newFleetStack(t)
	n.net.on = true // this machine runs a sluice
	defer fs.join(t, n)()

	fs.flt.SetRules(tagAllow{"remote-box": {"pypi.org", "github.com"}})
	if _, err := fs.flt.CreateOn(context.Background(), n.name, "remote-box", "alice", "ubuntu", 1, 512); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := fs.flt.PushNet(ctx); err != nil {
		t.Fatalf("PushNet: %v", err)
	}
	want := map[string][]string{"remote-box": {"pypi.org", "github.com"}}
	if got := n.net.lastPolicy(); !reflect.DeepEqual(got, want) {
		t.Errorf("the node's sluice was given %v, want %v", got, want)
	}
	n.linkAlive(t)
}

// And the read back: per-domain bandwidth for a VM on a node, which used to
// come back as zeroes because the gateway consulted its own meter.
func TestBandwidthForASandboxOnANodeComesFromThatNode(t *testing.T) {
	fs, n := newFleetStack(t)
	n.net.on = true
	n.net.usage = map[string]netpush.VMUsage{
		"remote-box": {Name: "remote-box", TxBytes: 4096, RxBytes: 8192,
			Domains: []netpush.DomainUsage{{Domain: "pypi.org", Resolved: true, TxBytes: 4096, RxBytes: 8192}}},
	}
	defer fs.join(t, n)()

	if _, err := fs.flt.CreateOn(context.Background(), n.name, "remote-box", "alice", "ubuntu", 1, 512); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	u, err := fs.flt.NetUsage(ctx, "remote-box")
	if err != nil {
		t.Fatalf("NetUsage: %v", err)
	}
	if u.TxBytes != 4096 || u.RxBytes != 8192 {
		t.Errorf("usage = %+v, want the node's own numbers", u)
	}
	if len(u.Domains) != 1 || u.Domains[0].Domain != "pypi.org" {
		t.Errorf("domains = %+v, want the node's per-domain breakdown", u.Domains)
	}
}

// A node with no sluice must REFUSE, not answer empty. An empty report renders
// in an owner's console as an idle VM, which is the silent failure this whole
// distinction exists to prevent.
func TestANodeWithoutSluiceRefusesRatherThanReportingZero(t *testing.T) {
	fs, n := newFleetStack(t) // n.net.on stays false: no sluice on this machine
	defer fs.join(t, n)()

	fs.flt.SetRules(tagAllow{"remote-box": {"pypi.org"}})
	if _, err := fs.flt.CreateOn(context.Background(), n.name, "remote-box", "alice", "ubuntu", 1, 512); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := fs.flt.NetUsage(ctx, "remote-box"); !isCode(err, nodelink.CodeNoSluice) {
		t.Errorf("usage on an unmetered node = %v, want code %s", err, nodelink.CodeNoSluice)
	}
	// And the push tolerates it: one unequipped machine must not stop the
	// gateway reconciling the rest of the fleet.
	if err := fs.flt.PushNet(ctx); err != nil {
		t.Errorf("PushNet reported an unequipped node as a failure: %v", err)
	}
	// The hello said so too, which is what `ctl node ls` marks.
	for _, st := range fs.flt.Nodes() {
		if st.Name == n.name && st.Egress {
			t.Errorf("%s reported egress control it does not have", n.name)
		}
	}
	n.linkAlive(t)
}

func isCode(err error, code string) bool {
	var typed *ctlops.Error
	return errors.As(err, &typed) && typed.Code == code
}
