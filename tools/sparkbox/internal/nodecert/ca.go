// Package nodecert issues and verifies the short-lived mTLS identities used by
// Sparkbox's gRPC control plane. SSH roster approval remains the bootstrap
// ceremony; this package turns an approved CSR into an exact SPIFFE identity.
package nodecert

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"strings"
	"time"
)

const (
	TrustDomain = "sparkbox"
	DefaultTTL  = 24 * time.Hour
	MaxTTL      = 7 * 24 * time.Hour
)

var (
	ErrIdentity = errors.New("invalid Sparkbox SPIFFE identity")
	ErrRevoked  = errors.New("certificate is revoked")
)

type Role string

const (
	RoleGateway Role = "gateway"
	RoleNode    Role = "node"
)

// Identity returns the canonical SPIFFE URI for one gateway cluster or roster
// node name.
func Identity(role Role, name string) (*url.URL, error) {
	if role != RoleGateway && role != RoleNode {
		return nil, ErrIdentity
	}
	if name == "" || strings.ContainsAny(name, "/?#") {
		return nil, ErrIdentity
	}
	return url.Parse(fmt.Sprintf("spiffe://%s/%s/%s", TrustDomain, role, name))
}

// ParseIdentity accepts only the canonical Sparkbox SPIFFE form and returns the
// authenticated role and name it contains. It is intentionally stricter than a
// generic SPIFFE parser: this control plane has one trust domain and two roles,
// and accepting an equivalent-but-differently-encoded URI would make
// certificate identity comparisons ambiguous.
func ParseIdentity(identity string) (Peer, error) {
	parsed, err := url.Parse(identity)
	if err != nil || parsed.Scheme != "spiffe" || parsed.Host != TrustDomain ||
		parsed.User != nil || parsed.Opaque != "" ||
		parsed.RawQuery != "" || parsed.Fragment != "" {
		return Peer{}, ErrIdentity
	}
	segments := strings.Split(strings.TrimPrefix(parsed.EscapedPath(), "/"), "/")
	if len(segments) != 2 || segments[0] == "" || segments[1] == "" {
		return Peer{}, ErrIdentity
	}
	name, err := url.PathUnescape(segments[1])
	if err != nil {
		return Peer{}, ErrIdentity
	}
	peer := Peer{Role: Role(segments[0]), Name: name}
	canonical, err := Identity(peer.Role, peer.Name)
	if err != nil || canonical.String() != parsed.String() {
		return Peer{}, ErrIdentity
	}
	return peer, nil
}

// Peer is the exact certificate identity a connection expects.
type Peer struct {
	Role Role
	Name string
}

// CA owns the internal certificate authority.
type CA struct {
	cert *x509.Certificate
	key  crypto.Signer
	now  func() time.Time
}

// NewCA creates an internal CA. The returned PEM values are suitable for
// storing in the gateway's private state directory.
func NewCA(clusterID string) (authority *CA, certPEM, keyPEM []byte, err error) {
	if clusterID == "" {
		return nil, nil, nil, ErrIdentity
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, nil, err
	}
	now := time.Now().UTC()
	serial, err := randomSerial()
	if err != nil {
		return nil, nil, nil, err
	}
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "Sparkbox " + clusterID + " internal CA"},
		NotBefore:             now.Add(-5 * time.Minute),
		NotAfter:              now.AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, key.Public(), key)
	if err != nil {
		return nil, nil, nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, nil, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, nil, err
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	return &CA{cert: cert, key: key, now: time.Now}, certPEM, keyPEM, nil
}

// LoadCA restores an internal CA from PEM.
func LoadCA(certPEM, keyPEM []byte) (*CA, error) {
	certBlock, _ := pem.Decode(certPEM)
	keyBlock, _ := pem.Decode(keyPEM)
	if certBlock == nil || keyBlock == nil {
		return nil, errors.New("nodecert: malformed CA PEM")
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, err
	}
	keyAny, err := x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, err
	}
	key, ok := keyAny.(crypto.Signer)
	if !ok || !cert.IsCA {
		return nil, errors.New("nodecert: PEM is not a signing CA")
	}
	if !publicKeysEqual(cert.PublicKey, key.Public()) {
		return nil, errors.New("nodecert: CA certificate and key do not match")
	}
	return &CA{cert: cert, key: key, now: time.Now}, nil
}

// Certificate returns a defensive copy of the CA certificate.
func (ca *CA) Certificate() *x509.Certificate {
	if ca == nil || ca.cert == nil {
		return nil
	}
	return ca.cert
}

// SignCSR signs an approved peer's CSR with its authoritative identity. The CSR
// cannot choose its own URI SAN.
func (ca *CA) SignCSR(csrPEM []byte, peer Peer, ttl time.Duration) (certPEM []byte, serial string, expiry time.Time, err error) {
	if ca == nil || ca.cert == nil || ca.key == nil {
		return nil, "", time.Time{}, errors.New("nodecert: nil CA")
	}
	id, err := Identity(peer.Role, peer.Name)
	if err != nil {
		return nil, "", time.Time{}, err
	}
	block, _ := pem.Decode(csrPEM)
	if block == nil || block.Type != "CERTIFICATE REQUEST" {
		return nil, "", time.Time{}, errors.New("nodecert: malformed CSR PEM")
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return nil, "", time.Time{}, err
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, "", time.Time{}, fmt.Errorf("nodecert: CSR signature: %w", err)
	}
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	if ttl > MaxTTL {
		ttl = MaxTTL
	}
	now := ca.now().UTC()
	number, err := randomSerial()
	if err != nil {
		return nil, "", time.Time{}, err
	}
	expiry = now.Add(ttl)
	if expiry.After(ca.cert.NotAfter) {
		expiry = ca.cert.NotAfter
	}
	template := &x509.Certificate{
		SerialNumber: number,
		Subject:      pkix.Name{CommonName: peer.Name},
		URIs:         []*url.URL{id},
		NotBefore:    now.Add(-5 * time.Minute),
		NotAfter:     expiry,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca.cert, csr.PublicKey, ca.key)
	if err != nil {
		return nil, "", time.Time{}, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		Serial(number), expiry, nil
}

// NewCSR creates a P-256 private key and CSR. The CA ignores every requested
// SAN and installs the roster-authoritative SPIFFE URI itself.
func NewCSR(commonName string) (crypto.Signer, []byte, error) {
	if commonName == "" {
		return nil, nil, ErrIdentity
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	der, err := x509.CreateCertificateRequest(rand.Reader,
		&x509.CertificateRequest{Subject: pkix.Name{CommonName: commonName}}, key)
	if err != nil {
		return nil, nil, err
	}
	return key, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}), nil
}

// Serial is the stable lowercase hex encoding stored in the node roster.
func Serial(n *big.Int) string {
	if n == nil {
		return ""
	}
	return hex.EncodeToString(n.Bytes())
}

func randomSerial() (*big.Int, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return nil, err
	}
	// Keep the value positive in every parser.
	raw[0] &= 0x7f
	raw[0] |= 0x01
	return new(big.Int).SetBytes(raw), nil
}

func publicKeysEqual(a, b any) bool {
	ader, err := x509.MarshalPKIXPublicKey(a)
	if err != nil {
		return false
	}
	bder, err := x509.MarshalPKIXPublicKey(b)
	return err == nil && string(ader) == string(bder)
}
