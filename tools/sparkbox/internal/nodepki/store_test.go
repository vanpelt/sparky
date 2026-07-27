package nodepki

import (
	"crypto/x509"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/nodecert"
)

func TestAuthorityPersistsAndGatewayLeafRenewsSafely(t *testing.T) {
	dir := t.TempDir()
	first, err := LoadOrCreateAuthority(dir, "cluster-a")
	if err != nil {
		t.Fatal(err)
	}
	again, err := LoadOrCreateAuthority(dir, "cluster-a")
	if err != nil {
		t.Fatal(err)
	}
	if string(first.CertPEM) != string(again.CertPEM) {
		t.Fatal("authority changed across restart")
	}
	leaf, err := first.GatewayCertificate(dir, "cluster-a", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !hasIdentity(leaf.Leaf, nodecert.Peer{Role: nodecert.RoleGateway, Name: "cluster-a"}) {
		t.Fatal("gateway leaf has the wrong identity")
	}
	info, err := os.Stat(filepath.Join(dir, CAKeyFile))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("CA key mode = %o", info.Mode().Perm())
	}
}

func TestGatewayPrivateKeysUseKeyDirAndRequireKeysFailsClosed(t *testing.T) {
	stateDir, keyDir := t.TempDir(), t.TempDir()
	authority, err := LoadOrCreateAuthorityFrom(stateDir, keyDir, "cluster-a", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := authority.GatewayCertificateFrom(
		stateDir, keyDir, "cluster-a", time.Hour, false,
	); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{CAKeyFile, GatewayKeyFile} {
		if _, err := os.Stat(filepath.Join(keyDir, name)); err != nil {
			t.Fatalf("private key %s was not stored in key-dir: %v", name, err)
		}
		if _, err := os.Stat(filepath.Join(stateDir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("private key %s leaked into state-dir: %v", name, err)
		}
	}
	for _, name := range []string{CACertFile, GatewayCertFile} {
		if _, err := os.Stat(filepath.Join(stateDir, name)); err != nil {
			t.Fatalf("public certificate %s was not durable in state-dir: %v", name, err)
		}
	}

	if _, err := LoadOrCreateAuthorityFrom(stateDir, keyDir, "cluster-a", true); err != nil {
		t.Fatalf("require-keys rejected present CA key: %v", err)
	}
	if err := os.Remove(filepath.Join(keyDir, GatewayKeyFile)); err != nil {
		t.Fatal(err)
	}
	if _, err := authority.GatewayCertificateFrom(
		stateDir, keyDir, "cluster-a", time.Hour, true,
	); err == nil {
		t.Fatal("require-keys regenerated a missing gateway private key")
	}
	if err := os.Remove(filepath.Join(keyDir, CAKeyFile)); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateAuthorityFrom(stateDir, keyDir, "cluster-a", true); err == nil {
		t.Fatal("require-keys regenerated a missing CA private key")
	}
}

func TestAuthorityRestoresPublicCertificateFromHydratedKeyDir(t *testing.T) {
	seedDir := t.TempDir()
	seed, err := LoadOrCreateAuthority(seedDir, "cluster-a")
	if err != nil {
		t.Fatal(err)
	}
	seedKey, err := os.ReadFile(filepath.Join(seedDir, CAKeyFile))
	if err != nil {
		t.Fatal(err)
	}
	stateDir, keyDir := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(keyDir, CACertFile), seed.CertPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(keyDir, CAKeyFile), seedKey, 0o600); err != nil {
		t.Fatal(err)
	}
	restored, err := LoadOrCreateAuthorityFrom(stateDir, keyDir, "cluster-a", true)
	if err != nil {
		t.Fatal(err)
	}
	if string(restored.CertPEM) != string(seed.CertPEM) {
		t.Fatal("restored authority differs from hydrated authority")
	}
	if _, err := os.Stat(filepath.Join(stateDir, CACertFile)); err != nil {
		t.Fatalf("public CA certificate was not copied into durable state: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stateDir, CAKeyFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("CA private key leaked into durable state: %v", err)
	}
}

func TestGatewayCertificateRejectsCachedLeafFromAnotherAuthority(t *testing.T) {
	stateDir, keyDir := t.TempDir(), t.TempDir()
	current, err := LoadOrCreateAuthorityFrom(stateDir, keyDir, "cluster-a", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := current.GatewayCertificateFrom(
		stateDir, keyDir, "cluster-a", 4*time.Hour, false,
	); err != nil {
		t.Fatal(err)
	}
	other, err := LoadOrCreateAuthority(t.TempDir(), "cluster-b")
	if err != nil {
		t.Fatal(err)
	}
	keyPEM, err := os.ReadFile(filepath.Join(keyDir, GatewayKeyFile))
	if err != nil {
		t.Fatal(err)
	}
	key, err := parseKey(keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	csr, err := csrFor(key, "cluster-a")
	if err != nil {
		t.Fatal(err)
	}
	foreignPEM, _, _, err := other.CA.SignCSR(
		csr,
		nodecert.Peer{Role: nodecert.RoleGateway, Name: "cluster-a"},
		4*time.Hour,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := writePublic(filepath.Join(stateDir, GatewayCertFile), foreignPEM); err != nil {
		t.Fatal(err)
	}

	renewed, err := current.GatewayCertificateFrom(
		stateDir, keyDir, "cluster-a", 4*time.Hour, false,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := renewed.Leaf.Verify(x509.VerifyOptions{
		Roots: current.Roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		t.Fatalf("cached foreign leaf was reused instead of reissued: %v", err)
	}
}

func TestNodeEnrollmentUsesDurableKeyAndExactIdentity(t *testing.T) {
	gatewayDir, nodeDir := t.TempDir(), t.TempDir()
	authority, err := LoadOrCreateAuthority(gatewayDir, "cluster-a")
	if err != nil {
		t.Fatal(err)
	}
	firstCSR, err := NodeCSR(nodeDir, "node-a")
	if err != nil {
		t.Fatal(err)
	}
	secondCSR, err := NodeCSR(nodeDir, "node-a")
	if err != nil {
		t.Fatal(err)
	}
	firstReq := parseCSR(t, firstCSR)
	secondReq := parseCSR(t, secondCSR)
	if !publicKeyEqual(t, firstReq.RawSubjectPublicKeyInfo, secondReq.RawSubjectPublicKeyInfo) {
		t.Fatal("node CSR key changed across attempts")
	}
	certPEM, _, _, err := authority.CA.SignCSR(firstCSR,
		nodecert.Peer{Role: nodecert.RoleNode, Name: "node-a"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	installed, _, err := InstallNodeCertificate(nodeDir, "node-a", certPEM, authority.CertPEM)
	if err != nil {
		t.Fatal(err)
	}
	loaded, _, err := LoadNodeCertificate(nodeDir, "node-a")
	if err != nil {
		t.Fatal(err)
	}
	if installed.Leaf.SerialNumber.Cmp(loaded.Leaf.SerialNumber) != 0 {
		t.Fatal("loaded a different node certificate")
	}
	if _, _, err := LoadNodeCertificate(nodeDir, "node-b"); !errors.Is(err, nodecert.ErrIdentity) {
		t.Fatalf("wrong node identity error = %v", err)
	}
}

func TestInstallRejectsCertificateForAnotherKey(t *testing.T) {
	gatewayDir, aDir, bDir := t.TempDir(), t.TempDir(), t.TempDir()
	authority, err := LoadOrCreateAuthority(gatewayDir, "cluster-a")
	if err != nil {
		t.Fatal(err)
	}
	csr, err := NodeCSR(aDir, "node-a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NodeCSR(bDir, "node-b"); err != nil {
		t.Fatal(err)
	}
	certPEM, _, _, err := authority.CA.SignCSR(csr,
		nodecert.Peer{Role: nodecert.RoleNode, Name: "node-a"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := InstallNodeCertificate(bDir, "node-b", certPEM, authority.CertPEM); err == nil {
		t.Fatal("installed a certificate over a different private key")
	}
	if _, err := os.Stat(filepath.Join(bDir, NodeCertFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rejected certificate was persisted: %v", err)
	}
}

func TestGatewayIdentityRoundTripsAndRejectsNonCanonicalForms(t *testing.T) {
	dir := t.TempDir()
	const identity = "spiffe://sparkbox/gateway/cluster-a"
	if err := StoreGatewayIdentity(dir, identity); err != nil {
		t.Fatal(err)
	}
	peer, err := LoadGatewayIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	if peer.Role != nodecert.RoleGateway || peer.Name != "cluster-a" {
		t.Fatalf("peer = %+v", peer)
	}
	for _, bad := range []string{
		"spiffe://sparkbox/node/cluster-a",
		"spiffe://sparkbox/gateway/cluster-a/extra",
		"https://sparkbox/gateway/cluster-a",
	} {
		if _, err := GatewayPeer(bad); !errors.Is(err, nodecert.ErrIdentity) {
			t.Errorf("GatewayPeer(%q) error = %v", bad, err)
		}
	}
}

func TestGatewayGRPCAddressPersistsAndEmptyCompatibilityReplyClearsIt(t *testing.T) {
	dir := t.TempDir()
	if err := StoreGatewayGRPCAddr(dir, "node-gateway.ts.net:9444"); err != nil {
		t.Fatal(err)
	}
	address, err := LoadGatewayGRPCAddr(dir)
	if err != nil {
		t.Fatal(err)
	}
	if address != "node-gateway.ts.net:9444" {
		t.Fatalf("address = %q", address)
	}
	if err := StoreGatewayGRPCAddr(dir, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadGatewayGRPCAddr(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("empty compatibility reply left a stale endpoint: %v", err)
	}
	for _, bad := range []string{
		"gateway.ts.net",
		":9444",
		"gateway.ts.net:0",
		" gateway.ts.net:9444 ",
	} {
		if _, err := NormalizeGRPCAddr(bad); err == nil {
			t.Errorf("NormalizeGRPCAddr(%q) succeeded", bad)
		}
	}
}

func parseCSR(t *testing.T, data []byte) *x509.CertificateRequest {
	t.Helper()
	block, _ := pem.Decode(data)
	if block == nil {
		t.Fatal("malformed CSR")
	}
	req, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func publicKeyEqual(t *testing.T, a, b []byte) bool {
	t.Helper()
	return string(a) == string(b)
}
