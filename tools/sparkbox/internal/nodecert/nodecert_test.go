package nodecert

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

func issue(t *testing.T, ca *CA, peer Peer, ttl time.Duration) (tls.Certificate, *x509.Certificate) {
	t.Helper()
	key, csr, err := NewCSR(peer.Name)
	if err != nil {
		t.Fatal(err)
	}
	certPEM, _, _, err := ca.SignCSR(csr, peer, ttl)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(certPEM)
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	pair.Leaf = cert
	return pair, cert
}

func rootsFor(ca *CA) *x509.CertPool {
	roots := x509.NewCertPool()
	roots.AddCert(ca.Certificate())
	return roots
}

func handshake(t *testing.T, client, server *tls.Config) error {
	t.Helper()
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	clientConn := tls.Client(left, client)
	serverConn := tls.Server(right, server)
	errs := make(chan error, 2)
	go func() { errs <- clientConn.Handshake() }()
	go func() { errs <- serverConn.Handshake() }()
	a, b := <-errs, <-errs
	return errors.Join(a, b)
}

func TestExactSPIFFEIdentitiesAuthenticate(t *testing.T) {
	ca, certPEM, keyPEM, err := NewCA("prod")
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadCA(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	nodePair, nodeCert := issue(t, loaded, Peer{Role: RoleNode, Name: "node-a"}, time.Hour)
	gatewayPair, _ := issue(t, loaded, Peer{Role: RoleGateway, Name: "prod"}, time.Hour)
	client, err := ClientTLSConfig(gatewayPair, rootsFor(ca), Peer{Role: RoleNode, Name: "node-a"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	server, err := ServerTLSConfig(nodePair, rootsFor(ca), Peer{Role: RoleGateway, Name: "prod"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := handshake(t, client, server); err != nil {
		t.Fatal(err)
	}
	if got := nodeCert.URIs[0].String(); got != "spiffe://sparkbox/node/node-a" {
		t.Fatalf("node URI = %q", got)
	}
}

func TestWrongNodeAndRevokedSerialAreRejected(t *testing.T) {
	ca, _, _, err := NewCA("prod")
	if err != nil {
		t.Fatal(err)
	}
	nodePair, nodeCert := issue(t, ca, Peer{Role: RoleNode, Name: "node-a"}, time.Hour)
	gatewayPair, _ := issue(t, ca, Peer{Role: RoleGateway, Name: "prod"}, time.Hour)

	client, _ := ClientTLSConfig(gatewayPair, rootsFor(ca), Peer{Role: RoleNode, Name: "node-b"}, nil)
	server, _ := ServerTLSConfig(nodePair, rootsFor(ca), Peer{Role: RoleGateway, Name: "prod"}, nil)
	if err := handshake(t, client, server); !errors.Is(err, ErrIdentity) {
		t.Fatalf("wrong-node handshake error = %v", err)
	}

	client, _ = ClientTLSConfig(gatewayPair, rootsFor(ca), Peer{Role: RoleNode, Name: "node-a"},
		func(serial string) bool { return serial == Serial(nodeCert.SerialNumber) })
	if err := handshake(t, client, server); !errors.Is(err, ErrRevoked) {
		t.Fatalf("revoked handshake error = %v", err)
	}
}

func TestRoleServerAcceptsApprovedNodeIdentitiesAndRejectsOtherRolesOrRevocation(t *testing.T) {
	ca, _, _, err := NewCA("prod")
	if err != nil {
		t.Fatal(err)
	}
	gatewayPair, _ := issue(t, ca, Peer{Role: RoleGateway, Name: "prod"}, time.Hour)
	nodeA, nodeACert := issue(t, ca, Peer{Role: RoleNode, Name: "node-a"}, time.Hour)
	nodeB, _ := issue(t, ca, Peer{Role: RoleNode, Name: "node-b"}, time.Hour)

	server, err := ServerTLSConfigForRole(gatewayPair, rootsFor(ca), RoleNode, nil)
	if err != nil {
		t.Fatal(err)
	}
	for name, leaf := range map[string]tls.Certificate{"node-a": nodeA, "node-b": nodeB} {
		if err := server.VerifyConnection(tls.ConnectionState{
			PeerCertificates: []*x509.Certificate{leaf.Leaf},
		}); err != nil {
			t.Fatalf("%s verification: %v", name, err)
		}
	}

	if err := server.VerifyConnection(tls.ConnectionState{
		PeerCertificates: []*x509.Certificate{gatewayPair.Leaf},
	}); !errors.Is(err, ErrIdentity) {
		t.Fatalf("gateway-role client error = %v, want ErrIdentity", err)
	}

	revokingServer, _ := ServerTLSConfigForRole(
		gatewayPair, rootsFor(ca), RoleNode,
		func(serial string) bool { return serial == Serial(nodeACert.SerialNumber) },
	)
	if err := revokingServer.VerifyConnection(tls.ConnectionState{
		PeerCertificates: []*x509.Certificate{nodeA.Leaf},
	}); !errors.Is(err, ErrRevoked) {
		t.Fatalf("revoked node error = %v, want ErrRevoked", err)
	}
}

func TestCSRIdentityCannotOverrideRosterNameAndTTLIsCapped(t *testing.T) {
	ca, _, _, err := NewCA("prod")
	if err != nil {
		t.Fatal(err)
	}
	_, csr, err := NewCSR("attacker-chosen-name")
	if err != nil {
		t.Fatal(err)
	}
	certPEM, _, expiry, err := ca.SignCSR(csr, Peer{Role: RoleNode, Name: "approved-name"}, 30*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(certPEM)
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if got := cert.URIs[0].String(); got != "spiffe://sparkbox/node/approved-name" {
		t.Fatalf("URI = %q", got)
	}
	if remaining := time.Until(expiry); remaining > MaxTTL+time.Minute {
		t.Fatalf("leaf TTL = %v, exceeds cap %v", remaining, MaxTTL)
	}
}

func TestIdentityAcceptsOnlyCanonicalPlatformLabels(t *testing.T) {
	for _, role := range []Role{RoleGateway, RoleNode} {
		for _, name := range []string{"prod", "prod-a", "node-01", "prod.example", "Prod_1"} {
			identity, err := Identity(role, name)
			if err != nil {
				t.Fatalf("Identity(%q, %q): %v", role, name, err)
			}
			peer, err := ParseIdentity(identity.String())
			if err != nil {
				t.Fatalf("ParseIdentity(%q): %v", identity, err)
			}
			if peer != (Peer{Role: role, Name: name}) {
				t.Fatalf("round trip = %+v, want %s/%s", peer, role, name)
			}
		}
	}
	for _, name := range []string{
		"", "prod%2Fwest", "prod/west", "prod west", "prod@example", "prod\u00e9",
		strings.Repeat("a", 256),
	} {
		if identity, err := Identity(RoleGateway, name); !errors.Is(err, ErrIdentity) {
			t.Errorf("Identity(gateway, %q) = %v, %v; want ErrIdentity", name, identity, err)
		}
	}
}
