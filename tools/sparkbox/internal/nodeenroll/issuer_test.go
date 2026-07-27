package nodeenroll

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	xssh "golang.org/x/crypto/ssh"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/nodecert"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/nodelink"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/nodes"
)

type issuerRig struct {
	issuer *Issuer
	roster *nodes.Store
	ca     *nodecert.CA
	row    nodes.Node
}

func newIssuerRig(t *testing.T, approved bool, ttl time.Duration) issuerRig {
	t.Helper()
	roster, err := nodes.Open(filepath.Join(t.TempDir(), "roster.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { roster.Close() })
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sshKey, err := xssh.NewPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	row, err := roster.Enroll("node-a", sshKey)
	if err != nil {
		t.Fatal(err)
	}
	if approved {
		if err := roster.ApproveFP(row.FP, "operator"); err != nil {
			t.Fatal(err)
		}
	}
	authority, _, _, err := nodecert.NewCA("cluster-a")
	if err != nil {
		t.Fatal(err)
	}
	issuer, err := New(authority, roster, "cluster-a", ttl)
	if err != nil {
		t.Fatal(err)
	}
	return issuerRig{issuer: issuer, roster: roster, ca: authority, row: row}
}

func csr(t *testing.T, commonName string) []byte {
	t.Helper()
	_, request, err := nodecert.NewCSR(commonName)
	if err != nil {
		t.Fatal(err)
	}
	return request
}

func parseCertificate(t *testing.T, certificatePEM []byte) *x509.Certificate {
	t.Helper()
	block, _ := pem.Decode(certificatePEM)
	if block == nil {
		t.Fatal("certificate response is not PEM")
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return certificate
}

func requireDenied(t *testing.T, err error) {
	t.Helper()
	var typed *ctlops.Error
	if !errors.As(err, &typed) || typed.Kind != ctlops.KindDenied ||
		typed.Code != nodelink.CodeCertificateNotApproved {
		t.Fatalf("error = %#v, want denied/%s", err, nodelink.CodeCertificateNotApproved)
	}
}

func TestPendingAndDisabledRosterRowsCannotReceiveCertificates(t *testing.T) {
	rig := newIssuerRig(t, false, time.Hour)
	request := nodelink.CertificateEnrollRequest{CSRPEM: csr(t, "node-a")}

	response, err := rig.issuer.Issue(context.Background(), "node-a", request)
	requireDenied(t, err)
	if len(response.CertificatePEM) != 0 {
		t.Fatal("pending node received certificate material")
	}
	pending, err := rig.roster.Get("node-a")
	if err != nil {
		t.Fatal(err)
	}
	if pending.CertSerial != "" || pending.CertExpiresAt != nil {
		t.Fatalf("pending row recorded certificate metadata: %+v", pending)
	}

	if err := rig.roster.ApproveFP(rig.row.FP, "operator"); err != nil {
		t.Fatal(err)
	}
	approved, err := rig.issuer.Issue(context.Background(), "node-a", request)
	if err != nil {
		t.Fatal(err)
	}
	if err := rig.roster.Disable("node-a"); err != nil {
		t.Fatal(err)
	}
	response, err = rig.issuer.Issue(context.Background(), "node-a", request)
	requireDenied(t, err)
	if len(response.CertificatePEM) != 0 {
		t.Fatal("disabled node received certificate material")
	}
	if _, active := rig.roster.LookupActiveCertificate(approved.Serial, time.Now()); active {
		t.Fatal("disabled row's previous certificate remains active")
	}
}

func TestCSRIdentityCannotOverrideAuthenticatedRosterName(t *testing.T) {
	rig := newIssuerRig(t, true, 30*24*time.Hour)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	attackerURI, _ := url.Parse("spiffe://sparkbox/node/attacker")
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject:  pkix.Name{CommonName: "attacker"},
		DNSNames: []string{"attacker.example"},
		URIs:     []*url.URL{attackerURI},
	}, key)
	if err != nil {
		t.Fatal(err)
	}
	request := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})
	response, err := rig.issuer.Issue(context.Background(), "node-a",
		nodelink.CertificateEnrollRequest{CSRPEM: request})
	if err != nil {
		t.Fatal(err)
	}

	certificate := parseCertificate(t, response.CertificatePEM)
	if certificate.Subject.CommonName != "node-a" {
		t.Fatalf("certificate CommonName = %q", certificate.Subject.CommonName)
	}
	if len(certificate.URIs) != 1 || certificate.URIs[0].String() != "spiffe://sparkbox/node/node-a" {
		t.Fatalf("certificate URIs = %v", certificate.URIs)
	}
	if len(certificate.DNSNames) != 0 {
		t.Fatalf("CSR-controlled DNS SANs survived: %v", certificate.DNSNames)
	}
	if response.GatewayIdentity != "spiffe://sparkbox/gateway/cluster-a" {
		t.Fatalf("gateway identity = %q", response.GatewayIdentity)
	}
	if remaining := time.Until(response.ExpiresAt); remaining > nodecert.MaxTTL+time.Minute {
		t.Fatalf("issuer TTL = %v, exceeds %v", remaining, nodecert.MaxTTL)
	}
	caCertificate := parseCertificate(t, response.CACertificatePEM)
	if !caCertificate.Equal(rig.ca.Certificate()) {
		t.Fatal("response contains a different CA certificate")
	}
}

func TestRenewalAtomicallyReplacesActiveSerialAndRevocationCallback(t *testing.T) {
	rig := newIssuerRig(t, true, time.Hour)
	request := nodelink.CertificateEnrollRequest{CSRPEM: csr(t, "anything")}
	first, err := rig.issuer.Issue(context.Background(), "node-a", request)
	if err != nil {
		t.Fatal(err)
	}
	second, err := rig.issuer.Issue(context.Background(), "node-a", request)
	if err != nil {
		t.Fatal(err)
	}
	if first.Serial == second.Serial {
		t.Fatal("renewal reused a certificate serial")
	}
	now := time.Now().UTC()
	if _, active := rig.roster.LookupActiveCertificate(first.Serial, now); active {
		t.Fatal("old serial remains active after renewal")
	}
	if row, active := rig.roster.LookupActiveCertificate(second.Serial, now); !active || row.Name != "node-a" {
		t.Fatalf("new serial lookup = %+v, %v", row, active)
	}
	revoked := RevocationCallback(rig.roster, func() time.Time { return now })
	if !revoked(first.Serial) {
		t.Fatal("revocation callback accepted replaced serial")
	}
	if revoked(second.Serial) {
		t.Fatal("revocation callback rejected current serial")
	}
	if !RevocationCallback(nil, nil)(second.Serial) {
		t.Fatal("nil roster revocation callback did not fail closed")
	}
}

func TestMalformedCSRIsTypedAndNeverRecorded(t *testing.T) {
	rig := newIssuerRig(t, true, time.Hour)
	response, err := rig.issuer.Issue(context.Background(), "node-a",
		nodelink.CertificateEnrollRequest{CSRPEM: []byte("not a CSR")})
	var typed *ctlops.Error
	if !errors.As(err, &typed) || typed.Kind != ctlops.KindInvalid || typed.Code != "bad_certificate_csr" {
		t.Fatalf("malformed CSR error = %#v", err)
	}
	if len(response.CertificatePEM) != 0 {
		t.Fatal("malformed CSR received certificate material")
	}
	row, err := rig.roster.Get("node-a")
	if err != nil {
		t.Fatal(err)
	}
	if row.CertSerial != "" || row.CertExpiresAt != nil {
		t.Fatalf("malformed CSR changed roster: %+v", row)
	}
}

func TestNewIssuerValidatesDependencies(t *testing.T) {
	rig := newIssuerRig(t, true, time.Hour)
	if _, err := New(nil, rig.roster, "cluster-a", time.Hour); err == nil {
		t.Fatal("New accepted nil authority")
	}
	if _, err := New(rig.ca, nil, "cluster-a", time.Hour); err == nil {
		t.Fatal("New accepted nil roster")
	}
	if _, err := New(rig.ca, rig.roster, "", time.Hour); err == nil {
		t.Fatal("New accepted empty gateway identity")
	}
}
