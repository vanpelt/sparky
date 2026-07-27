// Package nodepki persists the internal CA and short-lived mTLS leaf material
// used by the fleet gRPC control plane.
package nodepki

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/nodecert"
)

const (
	CACertFile          = "node_ca_cert.pem"
	CAKeyFile           = "node_ca_key.pem"
	GatewayCertFile     = "gateway_control_cert.pem"
	GatewayKeyFile      = "gateway_control_key.pem"
	NodeCertFile        = "node_control_cert.pem"
	NodeKeyFile         = "node_control_key.pem"
	GatewayIdentityFile = "gateway_control_identity"
	GatewayGRPCAddrFile = "gateway_control_addr"
)

// Authority is the gateway's durable internal CA.
type Authority struct {
	CA      *nodecert.CA
	CertPEM []byte
	Roots   *x509.CertPool
}

// LoadOrCreateAuthority loads a complete CA or atomically creates one. A
// partial CA is an error: silently replacing its missing half would strand
// every still-valid node certificate.
func LoadOrCreateAuthority(dir, clusterID string) (*Authority, error) {
	return LoadOrCreateAuthorityFrom(dir, dir, clusterID, false)
}

// LoadOrCreateAuthorityFrom keeps public CA state durable under stateDir while
// loading its private key exclusively from keyDir. requireKeys is the fleet
// host fail-closed policy: a missing private key is never silently replaced.
//
// A public CA certificate may be staged in keyDir on first boot by a secret
// fetcher. It is copied into stateDir before use and must match the durable copy
// on later boots. This lets a new host restore the public half without making
// private material part of its persistent state volume.
func LoadOrCreateAuthorityFrom(stateDir, keyDir, clusterID string, requireKeys bool) (*Authority, error) {
	if stateDir == "" {
		return nil, errors.New("nodepki: state directory is required")
	}
	if keyDir == "" {
		keyDir = stateDir
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, err
	}
	if !requireKeys {
		if err := os.MkdirAll(keyDir, 0o700); err != nil {
			return nil, err
		}
	}
	certPath, keyPath := filepath.Join(stateDir, CACertFile), filepath.Join(keyDir, CAKeyFile)
	certPEM, certErr := os.ReadFile(certPath)
	if errors.Is(certErr, os.ErrNotExist) && keyDir != stateDir {
		if staged, err := os.ReadFile(filepath.Join(keyDir, CACertFile)); err == nil {
			if err := writePublic(certPath, staged); err != nil {
				return nil, err
			}
			certPEM, certErr = staged, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	keyPEM, keyErr := os.ReadFile(keyPath)
	switch {
	case certErr == nil && keyErr == nil:
		ca, err := nodecert.LoadCA(certPEM, keyPEM)
		if err != nil {
			return nil, err
		}
		return authority(ca, certPEM)
	case requireKeys && errors.Is(keyErr, os.ErrNotExist):
		return nil, fmt.Errorf("nodepki: required CA private key %s is missing", keyPath)
	case errors.Is(certErr, os.ErrNotExist) && errors.Is(keyErr, os.ErrNotExist):
		ca, newCert, newKey, err := nodecert.NewCA(clusterID)
		if err != nil {
			return nil, err
		}
		if err := writePrivate(keyPath, newKey); err != nil {
			return nil, err
		}
		if err := writePublic(certPath, newCert); err != nil {
			return nil, err
		}
		return authority(ca, newCert)
	default:
		return nil, fmt.Errorf("nodepki: incomplete CA material (%v; %v)", certErr, keyErr)
	}
}

func authority(ca *nodecert.CA, certPEM []byte) (*Authority, error) {
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(certPEM) {
		return nil, errors.New("nodepki: CA certificate cannot be added to roots")
	}
	return &Authority{CA: ca, CertPEM: append([]byte(nil), certPEM...), Roots: roots}, nil
}

// GatewayCertificate loads a current gateway leaf or renews it with the same
// private key. Renewal starts before the final hour so a long-lived process
// never begins a listener with a certificate about to expire.
func (a *Authority) GatewayCertificate(dir, clusterID string, ttl time.Duration) (tls.Certificate, error) {
	return a.GatewayCertificateFrom(dir, dir, clusterID, ttl, false)
}

// GatewayCertificateFrom keeps the renewable public leaf in stateDir and its
// durable private key in keyDir. requireKeys has the same fail-closed semantics
// as LoadOrCreateAuthorityFrom.
func (a *Authority) GatewayCertificateFrom(
	stateDir, keyDir, clusterID string,
	ttl time.Duration,
	requireKeys bool,
) (tls.Certificate, error) {
	if a == nil || a.CA == nil {
		return tls.Certificate{}, errors.New("nodepki: authority is required")
	}
	if keyDir == "" {
		keyDir = stateDir
	}
	return a.loadOrIssue(
		stateDir, keyDir, GatewayKeyFile, GatewayCertFile,
		nodecert.Peer{Role: nodecert.RoleGateway, Name: clusterID}, ttl,
		requireKeys,
	)
}

func (a *Authority) loadOrIssue(
	stateDir, keyDir, keyFile, certFile string,
	peer nodecert.Peer,
	ttl time.Duration,
	requireKey bool,
) (tls.Certificate, error) {
	keyPath, certPath := filepath.Join(keyDir, keyFile), filepath.Join(stateDir, certFile)
	key, keyPEM, err := loadOrCreateKeyPolicy(keyPath, requireKey)
	if err != nil {
		return tls.Certificate{}, err
	}
	if certPEM, err := os.ReadFile(certPath); err == nil {
		leaf, err := tlsKeyPair(certPEM, keyPEM)
		if err == nil &&
			leaf.Leaf.NotAfter.After(time.Now().Add(time.Hour)) &&
			hasIdentity(leaf.Leaf, peer) &&
			a.verifyLeaf(leaf.Leaf) == nil {
			return leaf, nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return tls.Certificate{}, err
	}
	csr, err := csrFor(key, peer.Name)
	if err != nil {
		return tls.Certificate{}, err
	}
	certPEM, _, _, err := a.CA.SignCSR(csr, peer, ttl)
	if err != nil {
		return tls.Certificate{}, err
	}
	if err := writePublic(certPath, certPEM); err != nil {
		return tls.Certificate{}, err
	}
	return tlsKeyPair(certPEM, keyPEM)
}

func (a *Authority) verifyLeaf(leaf *x509.Certificate) error {
	if leaf == nil || a.Roots == nil {
		return errors.New("nodepki: gateway certificate has no trust roots")
	}
	_, err := leaf.Verify(x509.VerifyOptions{
		Roots:     a.Roots,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	})
	return err
}

// NodeCSR returns a CSR backed by the node's durable control-plane key. The CA
// installs the roster identity and ignores the CSR's requested identity.
func NodeCSR(dir, nodeName string) ([]byte, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	key, _, err := loadOrCreateKey(filepath.Join(dir, NodeKeyFile))
	if err != nil {
		return nil, err
	}
	return csrFor(key, nodeName)
}

// InstallNodeCertificate persists one approved enrollment reply and returns
// the usable leaf and roots. The key match is checked before either public file
// is replaced.
func InstallNodeCertificate(dir, nodeName string, certPEM, caPEM []byte) (tls.Certificate, *x509.CertPool, error) {
	keyPEM, err := os.ReadFile(filepath.Join(dir, NodeKeyFile))
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	leaf, err := tlsKeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		return tls.Certificate{}, nil, errors.New("nodepki: malformed CA certificate")
	}
	if _, err := leaf.Leaf.Verify(x509.VerifyOptions{
		Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}); err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("nodepki: enrolled certificate chain: %w", err)
	}
	if !hasIdentity(leaf.Leaf, nodecert.Peer{Role: nodecert.RoleNode, Name: nodeName}) {
		return tls.Certificate{}, nil, nodecert.ErrIdentity
	}
	if err := writePublic(filepath.Join(dir, NodeCertFile), certPEM); err != nil {
		return tls.Certificate{}, nil, err
	}
	if err := writePublic(filepath.Join(dir, CACertFile), caPEM); err != nil {
		return tls.Certificate{}, nil, err
	}
	return leaf, roots, nil
}

// LoadNodeCertificate loads an already-enrolled node identity.
func LoadNodeCertificate(dir, nodeName string) (tls.Certificate, *x509.CertPool, error) {
	certPEM, err := os.ReadFile(filepath.Join(dir, NodeCertFile))
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	keyPEM, err := os.ReadFile(filepath.Join(dir, NodeKeyFile))
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	caPEM, err := os.ReadFile(filepath.Join(dir, CACertFile))
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	leaf, err := tlsKeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	if !hasIdentity(leaf.Leaf, nodecert.Peer{Role: nodecert.RoleNode, Name: nodeName}) {
		return tls.Certificate{}, nil, nodecert.ErrIdentity
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		return tls.Certificate{}, nil, errors.New("nodepki: malformed CA certificate")
	}
	if _, err := leaf.Leaf.Verify(x509.VerifyOptions{
		Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}); err != nil {
		return tls.Certificate{}, nil, err
	}
	return leaf, roots, nil
}

// StoreGatewayIdentity persists the exact SPIFFE identity returned alongside
// a node enrollment response. It is public metadata, but authoritative: a node
// must not infer a gateway identity from DNS or its SSH address.
func StoreGatewayIdentity(dir, identity string) error {
	peer, err := GatewayPeer(identity)
	if err != nil {
		return err
	}
	canonical, _ := nodecert.Identity(peer.Role, peer.Name)
	return writePublic(filepath.Join(dir, GatewayIdentityFile), []byte(canonical.String()+"\n"))
}

func LoadGatewayIdentity(dir string) (nodecert.Peer, error) {
	data, err := os.ReadFile(filepath.Join(dir, GatewayIdentityFile))
	if err != nil {
		return nodecert.Peer{}, err
	}
	return GatewayPeer(strings.TrimSpace(string(data)))
}

// StoreGatewayGRPCAddr persists the endpoint authenticated nodes learn during
// SSH certificate enrollment. Empty is a compatibility signal from an older
// gateway and removes any stale endpoint so the node falls back to SSH.
func StoreGatewayGRPCAddr(dir, address string) error {
	path := filepath.Join(dir, GatewayGRPCAddrFile)
	if address == "" {
		err := os.Remove(path)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	normalized, err := NormalizeGRPCAddr(address)
	if err != nil {
		return err
	}
	return writePublic(path, []byte(normalized+"\n"))
}

func LoadGatewayGRPCAddr(dir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(dir, GatewayGRPCAddrFile))
	if err != nil {
		return "", err
	}
	return NormalizeGRPCAddr(strings.TrimSpace(string(data)))
}

// NormalizeGRPCAddr validates the concrete host:port form shared by enrollment
// and command wiring.
func NormalizeGRPCAddr(address string) (string, error) {
	if strings.TrimSpace(address) != address {
		return "", fmt.Errorf("nodepki: invalid gRPC address %q", address)
	}
	host, portText, err := net.SplitHostPort(address)
	if err != nil || strings.TrimSpace(host) == "" {
		return "", fmt.Errorf("nodepki: invalid gRPC address %q", address)
	}
	port, err := strconv.ParseUint(portText, 10, 16)
	if err != nil || port == 0 {
		return "", fmt.Errorf("nodepki: invalid gRPC address %q", address)
	}
	return net.JoinHostPort(host, strconv.FormatUint(port, 10)), nil
}

// GatewayPeer parses only the canonical Sparkbox gateway SPIFFE form.
func GatewayPeer(identity string) (nodecert.Peer, error) {
	peer, err := nodecert.ParseIdentity(identity)
	if err != nil || peer.Role != nodecert.RoleGateway {
		return nodecert.Peer{}, nodecert.ErrIdentity
	}
	return peer, nil
}

func loadOrCreateKey(path string) (crypto.Signer, []byte, error) {
	return loadOrCreateKeyPolicy(path, false)
}

func loadOrCreateKeyPolicy(path string, require bool) (crypto.Signer, []byte, error) {
	keyPEM, err := os.ReadFile(path)
	if err == nil {
		key, err := parseKey(keyPEM)
		return key, keyPEM, err
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, nil, err
	}
	if require {
		return nil, nil, fmt.Errorf("nodepki: required private key %s is missing", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, nil, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	if err := writePrivate(path, keyPEM); err != nil {
		return nil, nil, err
	}
	return key, keyPEM, nil
}

func parseKey(keyPEM []byte) (crypto.Signer, error) {
	block, _ := pem.Decode(keyPEM)
	if block == nil {
		return nil, errors.New("nodepki: malformed private key")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	key, ok := parsed.(crypto.Signer)
	if !ok {
		return nil, errors.New("nodepki: key cannot sign")
	}
	return key, nil
}

func csrFor(key crypto.Signer, commonName string) ([]byte, error) {
	if key == nil || commonName == "" {
		return nil, nodecert.ErrIdentity
	}
	der, err := x509.CreateCertificateRequest(rand.Reader,
		&x509.CertificateRequest{Subject: pkix.Name{CommonName: commonName}}, key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}), nil
}

func tlsKeyPair(certPEM, keyPEM []byte) (tls.Certificate, error) {
	leaf, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, err
	}
	if len(leaf.Certificate) == 0 {
		return tls.Certificate{}, errors.New("nodepki: empty certificate chain")
	}
	leaf.Leaf, err = x509.ParseCertificate(leaf.Certificate[0])
	return leaf, err
}

func hasIdentity(cert *x509.Certificate, peer nodecert.Peer) bool {
	want, err := nodecert.Identity(peer.Role, peer.Name)
	return err == nil && cert != nil && len(cert.URIs) == 1 && sameURL(cert.URIs[0], want)
}

func sameURL(a, b *url.URL) bool {
	return a != nil && b != nil && a.String() == b.String()
}

func writePrivate(path string, data []byte) error { return atomicWrite(path, data, 0o600) }
func writePublic(path string, data []byte) error  { return atomicWrite(path, data, 0o644) }

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) //nolint:errcheck
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close() //nolint:errcheck
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close() //nolint:errcheck
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close() //nolint:errcheck
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
