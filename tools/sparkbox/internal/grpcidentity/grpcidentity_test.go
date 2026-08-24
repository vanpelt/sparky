package grpcidentity

import (
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	nodev1 "github.com/vanpelt/sparky/tools/sparkbox/api/node/v1"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/metadata"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/nodecert"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/nodeenroll"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/nodelink"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/nodes"
	xssh "golang.org/x/crypto/ssh"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

type identityGateway struct {
	mu       sync.Mutex
	node     string
	request  nodelink.IdentityReq
	repos    nodelink.SelfReposReq
	credReq  nodelink.SelfRepoCredReq
	repoFail error
}

func (g *identityGateway) IdentityToken(_ context.Context, node string, request nodelink.IdentityReq) (nodelink.IdentityTokenResp, error) {
	g.mu.Lock()
	g.node, g.request = node, request
	g.mu.Unlock()
	return nodelink.IdentityTokenResp{
		Token: "signed.jwt", ExpiresAt: time.Unix(1_900_000_000, 0).UTC(),
	}, nil
}

func (g *identityGateway) IdentityDoc(_ context.Context, node string, request nodelink.IdentityReq) (nodelink.IdentityDocResp, error) {
	g.mu.Lock()
	g.node, g.request = node, request
	g.mu.Unlock()
	return nodelink.IdentityDocResp{
		Issuer: "https://oidc.example", Subject: "sparkbox:user:alice",
		Owner: "alice", GitHub: "alice-gh", KeyFP: "SHA256:key",
		Sandbox: request.Sandbox, Image: "universal", Box: node,
	}, nil
}

func (g *identityGateway) SelfRepos(_ context.Context, node string, request nodelink.SelfReposReq) (nodelink.SelfReposResp, error) {
	g.mu.Lock()
	g.node, g.repos = node, request
	failure := g.repoFail
	g.mu.Unlock()
	if failure != nil {
		return nodelink.SelfReposResp{}, failure
	}
	return nodelink.SelfReposResp{Repos: []nodelink.SelfRepoEntry{
		{Host: "github.com", Slug: "wandb/hivemind", Ref: "main", Access: "write"},
	}}, nil
}

func (g *identityGateway) SelfRepoCredential(_ context.Context, node string, request nodelink.SelfRepoCredReq) (nodelink.SelfRepoCredResp, error) {
	g.mu.Lock()
	g.node, g.credReq = node, request
	failure := g.repoFail
	g.mu.Unlock()
	if failure != nil {
		return nodelink.SelfRepoCredResp{}, failure
	}
	return nodelink.SelfRepoCredResp{
		Username: "x-access-token", Password: "ghs_token",
		ExpiresAt: time.Unix(1_900_000_000, 0).UTC(),
	}, nil
}

func TestGatewayIdentityUsesAuthenticatedNodeAndMapsEveryCall(t *testing.T) {
	authority, _, _, err := nodecert.NewCA("cluster-a")
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(authority.Certificate())
	gatewayLeaf := issueIdentityCertificate(t, authority,
		nodecert.Peer{Role: nodecert.RoleGateway, Name: "cluster-a"})
	nodeLeaf := issueIdentityCertificate(t, authority,
		nodecert.Peer{Role: nodecert.RoleNode, Name: "node-a"})

	backend := new(identityGateway)
	var revoked atomic.Bool
	service, err := NewServer(backend, func(string) bool { return revoked.Load() })
	if err != nil {
		t.Fatal(err)
	}
	serverTLS, err := nodecert.ServerTLSConfigForRole(
		gatewayLeaf, roots, nodecert.RoleNode, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewRPCServer(service, serverTLS)
	if err != nil {
		t.Fatal(err)
	}
	listener := bufconn.Listen(1 << 20)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() {
		server.Stop()
		_ = listener.Close()
	})

	clientTLS, err := nodecert.ClientTLSConfig(
		nodeLeaf, roots,
		nodecert.Peer{Role: nodecert.RoleGateway, Name: "cluster-a"}, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	dialCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := DialTLS(dialCtx, "bufnet", clientTLS,
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
			return listener.Dial()
		}),
		grpc.WithBlock(),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	box := &host.Sandbox{Name: "alpha", Owner: "node-asserted-owner"}
	token, err := client.Issue(context.Background(), box, "https://aud.example")
	if err != nil {
		t.Fatal(err)
	}
	if token.JWT != "signed.jwt" || token.ExpiresAt.IsZero() {
		t.Fatalf("token = %+v", token)
	}
	backend.mu.Lock()
	if backend.node != "node-a" ||
		backend.request != (nodelink.IdentityReq{Sandbox: "alpha", Aud: "https://aud.example"}) {
		t.Fatalf("gateway saw node=%q request=%+v", backend.node, backend.request)
	}
	backend.mu.Unlock()

	doc, err := client.Describe(context.Background(), box)
	if err != nil {
		t.Fatal(err)
	}
	if doc.Owner != "alice" || doc.Sandbox != "alpha" || doc.Box != "node-a" ||
		doc.GitHub != "alice-gh" || doc.KeyFP != "SHA256:key" {
		t.Fatalf("doc = %+v", doc)
	}
	// The repo pair rides the same service, and the assertion is the same one:
	// what the gateway is told about WHO is asking comes from the certificate
	// that terminated the connection, and the box the node named is the whole
	// rest of the request. The owner it carries — "node-asserted-owner" — is
	// deliberately wrong, and must never appear anywhere on the wire.
	manifest, err := client.Manifest(context.Background(), box)
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Repos) != 1 || manifest.Repos[0].Slug != "wandb/hivemind" ||
		manifest.Repos[0].Ref != "main" || manifest.Repos[0].Access != "write" {
		t.Fatalf("manifest = %+v", manifest.Repos)
	}
	credential, err := client.Credential(context.Background(), box, "wandb/hivemind")
	if err != nil {
		t.Fatal(err)
	}
	if credential.Password != "ghs_token" || credential.Username != "x-access-token" ||
		credential.ExpiresAt.IsZero() {
		t.Fatalf("credential = %+v", credential)
	}
	backend.mu.Lock()
	if backend.node != "node-a" || backend.repos.Sandbox != "alpha" ||
		backend.credReq != (nodelink.SelfRepoCredReq{Sandbox: "alpha", Slug: "wandb/hivemind"}) {
		t.Fatalf("gateway saw node=%q repos=%+v credential=%+v",
			backend.node, backend.repos, backend.credReq)
	}
	backend.mu.Unlock()

	// Revocation is checked on every RPC, not just on the TLS handshake, so an
	// already-open HTTP/2 connection loses authority immediately.
	revoked.Store(true)
	if _, err := client.Issue(context.Background(), box, "aud"); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("post-handshake revocation error = %v, want PermissionDenied", err)
	}
}

func TestGatewayIdentityTLSHonorsRosterSerialRevocation(t *testing.T) {
	authority, _, _, err := nodecert.NewCA("cluster-a")
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(authority.Certificate())
	gatewayLeaf := issueIdentityCertificate(t, authority,
		nodecert.Peer{Role: nodecert.RoleGateway, Name: "cluster-a"})
	nodeLeaf := issueIdentityCertificate(t, authority,
		nodecert.Peer{Role: nodecert.RoleNode, Name: "node-a"})

	roster, err := nodes.Open(t.TempDir() + "/roster.db")
	if err != nil {
		t.Fatal(err)
	}
	defer roster.Close()
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sshPublic, err := xssh.NewPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	row, err := roster.Enroll("node-a", sshPublic)
	if err != nil {
		t.Fatal(err)
	}
	if err := roster.ApproveFP(row.FP, "operator"); err != nil {
		t.Fatal(err)
	}
	serial := nodecert.Serial(nodeLeaf.Leaf.SerialNumber)
	if err := roster.RecordCertificate("node-a", serial, nodeLeaf.Leaf.NotAfter); err != nil {
		t.Fatal(err)
	}

	serverTLS, err := nodecert.ServerTLSConfigForRole(
		gatewayLeaf, roots, nodecert.RoleNode,
		nodeenroll.RevocationCallback(roster, time.Now),
	)
	if err != nil {
		t.Fatal(err)
	}
	state := tls.ConnectionState{PeerCertificates: []*x509.Certificate{nodeLeaf.Leaf}}
	if err := serverTLS.VerifyConnection(state); err != nil {
		t.Fatalf("active roster serial: %v", err)
	}
	if err := roster.RevokeCertificate(serial); err != nil {
		t.Fatal(err)
	}
	if err := serverTLS.VerifyConnection(state); !errors.Is(err, nodecert.ErrRevoked) {
		t.Fatalf("revoked roster serial error = %v, want ErrRevoked", err)
	}
}

func TestTransportConstructorsRejectTLSWithoutSPIFFEVerification(t *testing.T) {
	authority, _, _, err := nodecert.NewCA("cluster-a")
	if err != nil {
		t.Fatal(err)
	}
	gatewayLeaf := issueIdentityCertificate(t, authority,
		nodecert.Peer{Role: nodecert.RoleGateway, Name: "cluster-a"})
	nodeLeaf := issueIdentityCertificate(t, authority,
		nodecert.Peer{Role: nodecert.RoleNode, Name: "node-a"})
	service, err := NewServer(new(identityGateway))
	if err != nil {
		t.Fatal(err)
	}
	weakServerTLS := &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{gatewayLeaf},
		ClientAuth:   tls.RequireAnyClientCert,
	}
	if _, err := NewRPCServer(service, weakServerTLS); err == nil {
		t.Fatal("server accepted mTLS without node SPIFFE verification")
	}
	weakClientTLS := &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{nodeLeaf},
	}
	if _, err := DialTLS(context.Background(), "unused:9444", weakClientTLS); err == nil {
		t.Fatal("client accepted TLS without gateway SPIFFE verification")
	}
}

func issueIdentityCertificate(t *testing.T, authority *nodecert.CA, identity nodecert.Peer) tls.Certificate {
	t.Helper()
	key, csr, err := nodecert.NewCSR(identity.Name)
	if err != nil {
		t.Fatal(err)
	}
	certificatePEM, _, _, err := authority.SignCSR(csr, identity, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key.(crypto.Signer))
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	certificate, err := tls.X509KeyPair(certificatePEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(certificatePEM)
	certificate.Leaf, err = x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return certificate
}

var _ nodev1.GatewayIdentityServer = (*Server)(nil)

type failingIdentityClient struct{ code codes.Code }

func (f failingIdentityClient) IssueToken(context.Context, *nodev1.IssueTokenRequest, ...grpc.CallOption) (*nodev1.IssueTokenResponse, error) {
	return nil, status.Error(f.code, "refused")
}

func (f failingIdentityClient) DescribeIdentity(context.Context, *nodev1.DescribeIdentityRequest, ...grpc.CallOption) (*nodev1.IdentityDescription, error) {
	return nil, status.Error(f.code, "refused")
}

func (f failingIdentityClient) ListRepos(context.Context, *nodev1.ListReposRequest, ...grpc.CallOption) (*nodev1.ListReposResponse, error) {
	return nil, status.Error(f.code, "refused")
}

func (f failingIdentityClient) IssueRepoCredential(context.Context, *nodev1.IssueRepoCredentialRequest, ...grpc.CallOption) (*nodev1.IssueRepoCredentialResponse, error) {
	return nil, status.Error(f.code, "refused")
}

func TestClientClassifiesRPCFailuresWithoutMakingAuthorizationFallbackEligible(t *testing.T) {
	box := &host.Sandbox{Name: "alpha"}
	for _, test := range []struct {
		name         string
		code         codes.Code
		want         error
		wantFallback bool
	}{
		{name: "audience", code: codes.InvalidArgument, want: metadata.ErrAudience},
		{name: "issuer disabled", code: codes.FailedPrecondition, want: metadata.ErrNoIssuer},
		{name: "transport", code: codes.Unavailable, want: metadata.ErrNoIssuer, wantFallback: true},
		{name: "authorization", code: codes.PermissionDenied},
	} {
		t.Run(test.name, func(t *testing.T) {
			client, err := WrapClient(failingIdentityClient{code: test.code})
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.Issue(context.Background(), box, "aud")
			if test.want != nil && !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
			if got := errors.Is(err, ErrUnavailable); got != test.wantFallback {
				t.Fatalf("fallback eligibility = %t, want %t (error %v)", got, test.wantFallback, err)
			}
		})
	}
}

func TestGatewayErrorsMapToStableRPCClasses(t *testing.T) {
	for _, test := range []struct {
		code string
		want codes.Code
	}{
		{code: nodelink.CodeIdentityAudience, want: codes.InvalidArgument},
		{code: nodelink.CodeNoIssuer, want: codes.FailedPrecondition},
		{code: nodelink.CodeNotYours, want: codes.PermissionDenied},
		{code: nodelink.CodeNoRepos, want: codes.Unimplemented},
		{code: nodelink.CodeNoSuchRepo, want: codes.NotFound},
		{code: nodelink.CodeRepoDenied, want: codes.PermissionDenied},
		{code: nodelink.CodeRepoUpstream, want: codes.FailedPrecondition},
	} {
		err := gatewayError(&ctlops.Error{Code: test.code, Msg: "refused"})
		if got := status.Code(err); got != test.want {
			t.Errorf("gateway code %q mapped to %s, want %s", test.code, got, test.want)
		}
	}
}

// The repo endpoints classify one code the identity endpoints deliberately do
// not, and share the rest. A miss anywhere in this table is a wrong HTTP status
// in the guest — and the one that matters is Unavailable staying the only
// fallback-eligible answer, so that a refusal is never replayed over the SSH
// uplink to be refused again inside the same ten-second budget.
func TestRepoClientClassifiesRefusals(t *testing.T) {
	box := &host.Sandbox{Name: "alpha"}
	for _, test := range []struct {
		name         string
		code         codes.Code
		want         error
		wantFallback bool
	}{
		{name: "denied", code: codes.PermissionDenied, want: metadata.ErrRepoDenied},
		{name: "unattached", code: codes.NotFound, want: metadata.ErrNoSuchRepo},
		{name: "not enabled or not implemented", code: codes.Unimplemented, want: metadata.ErrNotEnabled},
		{name: "upstream", code: codes.FailedPrecondition, want: metadata.ErrNoIssuer},
		{name: "transport", code: codes.Unavailable, want: metadata.ErrNoIssuer, wantFallback: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			client, err := WrapClient(failingIdentityClient{code: test.code})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := client.Credential(context.Background(), box, "wandb/hivemind"); !errors.Is(err, test.want) {
				t.Fatalf("credential error = %v, want %v", err, test.want)
			}
			_, err = client.Manifest(context.Background(), box)
			if !errors.Is(err, test.want) {
				t.Fatalf("manifest error = %v, want %v", err, test.want)
			}
			if got := errors.Is(err, ErrUnavailable); got != test.wantFallback {
				t.Fatalf("fallback eligibility = %t, want %t (error %v)", got, test.wantFallback, err)
			}
		})
	}
}
