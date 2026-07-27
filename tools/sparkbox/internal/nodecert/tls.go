package nodecert

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
)

// Revoked reports whether a leaf certificate serial has been revoked.
type Revoked func(serial string) bool

// ClientTLSConfig verifies a server against roots and an exact SPIFFE identity.
// Leaf may be either a gateway or node identity because both directions host a
// gRPC service.
func ClientTLSConfig(leaf tls.Certificate, roots *x509.CertPool, expected Peer, revoked Revoked) (*tls.Config, error) {
	if roots == nil {
		return nil, errors.New("nodecert: client needs CA roots")
	}
	if _, err := Identity(expected.Role, expected.Name); err != nil {
		return nil, err
	}
	return &tls.Config{
		MinVersion:         tls.VersionTLS13,
		Certificates:       []tls.Certificate{leaf},
		InsecureSkipVerify: true, // exact chain + URI verification is below
		VerifyConnection: func(cs tls.ConnectionState) error {
			return verifyConnection(cs, roots, expected, x509.ExtKeyUsageServerAuth, revoked)
		},
	}, nil
}

// ServerTLSConfig requires a client certificate chained to roots with an exact
// SPIFFE identity.
func ServerTLSConfig(leaf tls.Certificate, roots *x509.CertPool, expected Peer, revoked Revoked) (*tls.Config, error) {
	if roots == nil {
		return nil, errors.New("nodecert: server needs CA roots")
	}
	if _, err := Identity(expected.Role, expected.Name); err != nil {
		return nil, err
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{leaf},
		ClientAuth:   tls.RequireAnyClientCert,
		VerifyConnection: func(cs tls.ConnectionState) error {
			return verifyConnection(cs, roots, expected, x509.ExtKeyUsageClientAuth, revoked)
		},
	}, nil
}

// ServerTLSConfigForRole verifies a client certificate from any member of one
// Sparkbox role. GatewayIdentity uses it because one listener serves every
// approved node; authorization still uses the exact node name recovered from
// the authenticated certificate on each RPC.
func ServerTLSConfigForRole(leaf tls.Certificate, roots *x509.CertPool, role Role, revoked Revoked) (*tls.Config, error) {
	if roots == nil {
		return nil, errors.New("nodecert: server needs CA roots")
	}
	if role != RoleGateway && role != RoleNode {
		return nil, ErrIdentity
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{leaf},
		ClientAuth:   tls.RequireAnyClientCert,
		VerifyConnection: func(cs tls.ConnectionState) error {
			_, err := verifyConnectionRole(cs, roots, role, x509.ExtKeyUsageClientAuth, revoked)
			return err
		},
	}, nil
}

func verifyConnection(cs tls.ConnectionState, roots *x509.CertPool, expected Peer, usage x509.ExtKeyUsage, revoked Revoked) error {
	peer, err := verifyConnectionRole(cs, roots, expected.Role, usage, revoked)
	if err != nil {
		return err
	}
	if peer != expected {
		want, _ := Identity(expected.Role, expected.Name)
		return fmt.Errorf("%w: got %s/%s, want %s", ErrIdentity, peer.Role, peer.Name, want)
	}
	return nil
}

func verifyConnectionRole(cs tls.ConnectionState, roots *x509.CertPool, role Role, usage x509.ExtKeyUsage, revoked Revoked) (Peer, error) {
	if len(cs.PeerCertificates) == 0 {
		return Peer{}, errors.New("nodecert: peer presented no certificate")
	}
	leaf := cs.PeerCertificates[0]
	intermediates := x509.NewCertPool()
	for _, cert := range cs.PeerCertificates[1:] {
		intermediates.AddCert(cert)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots:         roots,
		Intermediates: intermediates,
		KeyUsages:     []x509.ExtKeyUsage{usage},
	}); err != nil {
		return Peer{}, fmt.Errorf("nodecert: certificate chain: %w", err)
	}
	peer, err := PeerFromCertificate(leaf)
	if err != nil || peer.Role != role {
		return Peer{}, fmt.Errorf("%w: got %v, want role %s", ErrIdentity, leaf.URIs, role)
	}
	if revoked != nil && revoked(Serial(leaf.SerialNumber)) {
		return Peer{}, ErrRevoked
	}
	return peer, nil
}

// PeerFromCertificate returns the one canonical Sparkbox SPIFFE identity on a
// leaf certificate.
func PeerFromCertificate(leaf *x509.Certificate) (Peer, error) {
	if leaf == nil || len(leaf.URIs) != 1 {
		return Peer{}, ErrIdentity
	}
	return ParseIdentity(leaf.URIs[0].String())
}
