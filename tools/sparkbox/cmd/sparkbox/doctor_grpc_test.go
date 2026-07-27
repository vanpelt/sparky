package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	nodev1 "github.com/vanpelt/sparky/tools/sparkbox/api/node/v1"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/hostsetup"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/nodecert"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/nodepki"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/nodes"
)

func TestGatewayDoctorProbesOnlyCurrentApprovedCertificates(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	expires := now.Add(time.Hour)
	revokedAt := now.Add(-time.Minute)
	rows := []doctorRosterNode{
		{
			name: "healthy", status: nodes.StatusApproved,
			grpcAddr: "100.64.0.10:9443", certSerial: "current",
			certExpiresAt: &expires,
		},
		{
			name: "revoked", status: nodes.StatusApproved,
			grpcAddr: "100.64.0.11:9443", certSerial: "revoked",
			certExpiresAt: &expires, certRevokedAt: &revokedAt,
		},
		{
			name: "missing-address", status: nodes.StatusApproved,
			certSerial: "unused", certExpiresAt: &expires,
		},
	}
	var probes atomic.Int32
	deps := doctorControlDeps{
		now: func() time.Time { return now },
		loadRoster: func(context.Context, string) ([]doctorRosterNode, error) {
			return rows, nil
		},
		loadGateway: func(hostsetup.Config, time.Time) (doctorControlIdentity, error) {
			return doctorControlIdentity{}, nil
		},
		probe: func(
			_ context.Context,
			address string,
			_ doctorControlIdentity,
			expected nodecert.Peer,
			revoked nodecert.Revoked,
		) (*nodev1.HealthResponse, error) {
			probes.Add(1)
			if address != "100.64.0.10:9443" {
				t.Errorf("probed untrusted address %q", address)
			}
			if expected != (nodecert.Peer{Role: nodecert.RoleNode, Name: "healthy"}) {
				t.Errorf("expected identity = %+v", expected)
			}
			if revoked("current") {
				t.Error("the roster's exact current serial was rejected")
			}
			if !revoked("superseded") {
				t.Error("a non-current presented serial was accepted")
			}
			return &nodev1.HealthResponse{
				Node: "healthy", Status: nodev1.HealthStatus_HEALTH_STATUS_SERVING,
			}, nil
		},
	}

	cfg := hostsetup.DefaultConfig()
	cfg.NodeControlTransport = "auto"
	result := runNodeControlHealth(context.Background(), cfg, deps)
	if result.Status != hostsetup.Warn {
		t.Fatalf("status = %s, want WARN: %+v", result.Status, result)
	}
	if probes.Load() != 1 {
		t.Fatalf("probe count = %d, want exactly the one current configured node", probes.Load())
	}
	for _, want := range []string{"revoked: no current approved certificate", "missing-address: no approved gRPC address"} {
		if !strings.Contains(result.Detail, want) {
			t.Errorf("detail %q should mention %q", result.Detail, want)
		}
	}

	cfg.NodeControlTransport = "grpc"
	result = runNodeControlHealth(context.Background(), cfg, deps)
	if result.Status != hostsetup.Fail {
		t.Fatalf("forced gRPC status = %s, want FAIL", result.Status)
	}
}

func TestGatewayDoctorRequiresExactServingHealth(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	expires := now.Add(time.Hour)
	row := doctorRosterNode{
		name: "node-a", status: nodes.StatusApproved,
		grpcAddr: "100.64.0.10:9443", certSerial: "serial-a",
		certExpiresAt: &expires,
	}
	response := &nodev1.HealthResponse{
		Node: "somebody-else", Status: nodev1.HealthStatus_HEALTH_STATUS_SERVING,
	}
	deps := doctorControlDeps{
		now: func() time.Time { return now },
		loadRoster: func(context.Context, string) ([]doctorRosterNode, error) {
			return []doctorRosterNode{row}, nil
		},
		loadGateway: func(hostsetup.Config, time.Time) (doctorControlIdentity, error) {
			return doctorControlIdentity{}, nil
		},
		probe: func(
			context.Context, string, doctorControlIdentity, nodecert.Peer, nodecert.Revoked,
		) (*nodev1.HealthResponse, error) {
			return response, nil
		},
	}
	cfg := hostsetup.DefaultConfig()
	result := runNodeControlHealth(context.Background(), cfg, deps)
	if result.Status != hostsetup.Warn || !strings.Contains(result.Detail, "identity mismatch") {
		t.Fatalf("mismatched Health result = %+v", result)
	}

	response = &nodev1.HealthResponse{
		Node: "node-a", Status: nodev1.HealthStatus_HEALTH_STATUS_DEGRADED,
	}
	result = runNodeControlHealth(context.Background(), cfg, deps)
	if result.Status != hostsetup.Warn || !strings.Contains(result.Detail, "DEGRADED") {
		t.Fatalf("degraded Health result = %+v", result)
	}

	response = &nodev1.HealthResponse{
		Node: "node-a", Status: nodev1.HealthStatus_HEALTH_STATUS_SERVING,
	}
	result = runNodeControlHealth(context.Background(), cfg, deps)
	if result.Status != hostsetup.Pass || !strings.Contains(result.Detail, "exact mTLS identities") {
		t.Fatalf("serving Health result = %+v", result)
	}
}

func TestGatewayDoctorRosterReadFailureHonorsTransportMode(t *testing.T) {
	deps := doctorControlDeps{
		loadRoster: func(context.Context, string) ([]doctorRosterNode, error) {
			return nil, errors.New("read-only database unavailable")
		},
	}
	cfg := hostsetup.DefaultConfig()
	if result := runNodeControlHealth(context.Background(), cfg, deps); result.Status != hostsetup.Warn {
		t.Fatalf("auto status = %s, want WARN", result.Status)
	}
	cfg.NodeControlTransport = "grpc"
	if result := runNodeControlHealth(context.Background(), cfg, deps); result.Status != hostsetup.Fail {
		t.Fatalf("grpc status = %s, want FAIL", result.Status)
	}
}

func TestGatewayDoctorChecksConfiguredIdentityListener(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	var dials atomic.Int32
	deps := doctorControlDeps{
		now: func() time.Time { return now },
		loadRoster: func(context.Context, string) ([]doctorRosterNode, error) {
			return nil, nil
		},
		loadGateway: func(hostsetup.Config, time.Time) (doctorControlIdentity, error) {
			return doctorControlIdentity{}, nil
		},
		dialTCP: func(_ context.Context, address string) error {
			dials.Add(1)
			if address != "100.64.0.10:9444" {
				t.Errorf("dial address = %q", address)
			}
			return nil
		},
	}
	cfg := hostsetup.DefaultConfig()
	cfg.GatewayGRPCAddr = "100.64.0.10:9444"
	result := runNodeControlHealth(context.Background(), cfg, deps)
	if result.Status != hostsetup.Pass || !strings.Contains(result.Detail, "identity listener") {
		t.Fatalf("gateway identity result = %+v", result)
	}
	if dials.Load() != 1 {
		t.Fatalf("dials = %d, want 1", dials.Load())
	}
}

func TestNodeDoctorChecksIdentityMetadataAndListener(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	expires := now.Add(time.Hour)
	var dials atomic.Int32
	deps := doctorControlDeps{
		now: func() time.Time { return now },
		loadNode: func(hostsetup.Config, time.Time) (doctorControlIdentity, error) {
			return doctorControlIdentity{
				leaf: tls.Certificate{Leaf: &x509.Certificate{NotAfter: expires}},
				peer: nodecert.Peer{Role: nodecert.RoleNode, Name: "gpu-a"},
			}, nil
		},
		loadGatewayPeer: func(string) (nodecert.Peer, error) {
			return nodecert.Peer{Role: nodecert.RoleGateway, Name: "cluster-a"}, nil
		},
		dialTCP: func(_ context.Context, address string) error {
			dials.Add(1)
			if address != "100.64.0.20:9443" {
				t.Errorf("dial address = %q", address)
			}
			return nil
		},
	}
	cfg := hostsetup.DefaultConfig()
	cfg.Gateway = "gateway.example:2222"
	cfg.NodeGRPCAddr = "100.64.0.20:9443"
	result := runNodeControlHealth(context.Background(), cfg, deps)
	if result.Status != hostsetup.Pass || !strings.Contains(result.Detail, "expects gateway cluster-a") {
		t.Fatalf("node result = %+v", result)
	}
	if dials.Load() != 1 {
		t.Fatalf("dials = %d, want 1", dials.Load())
	}

	deps.loadGatewayPeer = func(string) (nodecert.Peer, error) {
		return nodecert.Peer{}, errors.New("missing enrollment metadata")
	}
	result = runNodeControlHealth(context.Background(), cfg, deps)
	if result.Status != hostsetup.Warn {
		t.Fatalf("missing metadata in auto = %s, want WARN", result.Status)
	}
	cfg.NodeControlTransport = "grpc"
	result = runNodeControlHealth(context.Background(), cfg, deps)
	if result.Status != hostsetup.Fail {
		t.Fatalf("missing metadata in grpc = %s, want FAIL", result.Status)
	}
}

func TestLoadDoctorRosterIsReadOnlyAndApprovedOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sparkbox.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
		CREATE TABLE nodes (
			name TEXT, status TEXT, grpc_addr TEXT, cert_serial TEXT,
			cert_expires_at TIMESTAMP, cert_revoked_at TIMESTAMP
		);
		INSERT INTO nodes VALUES
			('approved-a', 'approved', '100.64.0.10:9443', 'serial-a', '2026-07-28T12:00:00Z', NULL),
			('pending-a', 'pending', '100.64.0.11:9443', '', NULL, NULL);
	`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o444); err != nil {
		t.Fatal(err)
	}

	rows, err := loadDoctorRoster(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].name != "approved-a" || rows[0].certSerial != "serial-a" {
		t.Fatalf("rows = %+v", rows)
	}

	missing := filepath.Join(filepath.Dir(path), "missing.db")
	if _, err := loadDoctorRoster(context.Background(), missing); err == nil {
		t.Fatal("read-only doctor unexpectedly created a missing roster")
	}
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing roster was created or stat failed unexpectedly: %v", err)
	}
}

func TestLoadDoctorNodeIdentityRejectsConfiguredNameMismatch(t *testing.T) {
	dir := t.TempDir()
	authority, caCert, _, err := nodecert.NewCA("prod-a")
	if err != nil {
		t.Fatal(err)
	}
	csr, err := nodepki.NodeCSR(dir, "gpu-a")
	if err != nil {
		t.Fatal(err)
	}
	cert, _, _, err := authority.SignCSR(
		csr, nodecert.Peer{Role: nodecert.RoleNode, Name: "gpu-a"}, time.Hour,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := nodepki.InstallNodeCertificate(dir, "gpu-a", cert, caCert); err != nil {
		t.Fatal(err)
	}

	cfg := hostsetup.DefaultConfig()
	cfg.StateDir = dir
	cfg.Gateway = "gateway.example:2222"
	cfg.NodeName = "gpu-b"
	if _, err := loadDoctorNodeIdentity(cfg, time.Now().UTC()); !errors.Is(err, nodecert.ErrIdentity) {
		t.Fatalf("configured name mismatch error = %v, want %v", err, nodecert.ErrIdentity)
	}

	cfg.NodeName = "gpu-a"
	identity, err := loadDoctorNodeIdentity(cfg, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if identity.peer.Name != "gpu-a" {
		t.Fatalf("peer = %+v", identity.peer)
	}
}
