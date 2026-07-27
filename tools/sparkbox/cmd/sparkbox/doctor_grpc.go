package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	nodev1 "github.com/vanpelt/sparky/tools/sparkbox/api/node/v1"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/grpccontrol"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/hostsetup"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/nodecert"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/nodepki"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/nodes"
	"google.golang.org/grpc"
	_ "modernc.org/sqlite"
)

const doctorNodeHealthTimeout = 5 * time.Second

type doctorControlIdentity struct {
	leaf  tls.Certificate
	roots *x509.CertPool
	peer  nodecert.Peer
}

type doctorRosterNode struct {
	name          string
	status        string
	grpcAddr      string
	certSerial    string
	certExpiresAt *time.Time
	certRevokedAt *time.Time
}

type doctorControlDeps struct {
	now             func() time.Time
	loadGateway     func(hostsetup.Config, time.Time) (doctorControlIdentity, error)
	loadNode        func(hostsetup.Config, time.Time) (doctorControlIdentity, error)
	loadGatewayPeer func(string) (nodecert.Peer, error)
	loadRoster      func(context.Context, string) ([]doctorRosterNode, error)
	probe           func(context.Context, string, doctorControlIdentity, nodecert.Peer, nodecert.Revoked) (*nodev1.HealthResponse, error)
	dialTCP         func(context.Context, string) error
}

func defaultDoctorControlDeps() doctorControlDeps {
	return doctorControlDeps{
		now:             time.Now,
		loadGateway:     loadDoctorGatewayIdentity,
		loadNode:        loadDoctorNodeIdentity,
		loadGatewayPeer: nodepki.LoadGatewayIdentity,
		loadRoster:      loadDoctorRoster,
		probe:           probeDoctorNode,
		dialTCP:         dialDoctorTCP,
	}
}

// nodeControlHealthCheck replaces hostsetup's compatibility placeholder with a
// read-only diagnostic. A gateway authenticates to every configured approved
// node and calls NodeControl.Health. A node cannot impersonate the gateway to
// call its own listener, so its local half verifies the enrolled identity,
// authoritative gateway identity and listener without weakening the server's
// exact-gateway client-certificate requirement.
func nodeControlHealthCheck(ctx context.Context, cfg hostsetup.Config) hostsetup.Check {
	deps := defaultDoctorControlDeps()
	return hostsetup.Check{
		Name: "node control mTLS",
		Run: func(_ hostsetup.Probe, current hostsetup.Config) hostsetup.Result {
			return runNodeControlHealth(ctx, current, deps)
		},
	}
}

func runNodeControlHealth(ctx context.Context, cfg hostsetup.Config, deps doctorControlDeps) hostsetup.Result {
	if cfg.NodeControlTransport == "ssh" {
		return controlPass("disabled by --node-control-transport=ssh")
	}
	if cfg.Gateway != "" {
		return runNodeLocalControlHealth(ctx, cfg, deps)
	}
	return runGatewayControlHealth(ctx, cfg, deps)
}

func runGatewayControlHealth(ctx context.Context, cfg hostsetup.Config, deps doctorControlDeps) hostsetup.Result {
	rows, err := deps.loadRoster(ctx, filepath.Join(cfg.StateDir, "sparkbox.db"))
	if err != nil {
		return controlProblem(cfg,
			"cannot read approved node gRPC configuration: "+err.Error(),
			"repair the roster database and re-run doctor; the probe opens it read-only")
	}
	if len(rows) == 0 && strings.TrimSpace(cfg.GatewayGRPCAddr) == "" {
		return controlPass("no approved remote nodes")
	}

	now := deps.now().UTC()
	var issues []string
	var targets []doctorRosterNode
	for _, row := range rows {
		switch {
		case strings.TrimSpace(row.grpcAddr) == "":
			issues = append(issues, row.name+": no approved gRPC address")
		case rosterCertificateRevoked(row, now)(row.certSerial):
			// Fail closed before dialing. The callback used by TLS below repeats
			// this exact snapshot check for the serial the peer presents.
			issues = append(issues, row.name+": no current approved certificate")
		default:
			targets = append(targets, row)
		}
	}

	var identity doctorControlIdentity
	if len(targets) > 0 || strings.TrimSpace(cfg.GatewayGRPCAddr) != "" {
		identity, err = deps.loadGateway(cfg, now)
		if err != nil {
			issues = append(issues, "gateway identity: "+err.Error())
			targets = nil
		} else if strings.TrimSpace(cfg.GatewayGRPCAddr) != "" {
			probeCtx, cancel := context.WithTimeout(ctx, doctorNodeHealthTimeout)
			err := deps.dialTCP(probeCtx, cfg.GatewayGRPCAddr)
			cancel()
			if err != nil {
				issues = append(issues,
					"gateway identity listener "+cfg.GatewayGRPCAddr+": "+err.Error())
			}
		}
	}

	type outcome struct {
		name  string
		issue string
	}
	outcomes := make(chan outcome, len(targets))
	for _, row := range targets {
		row := row
		go func() {
			probeCtx, cancel := context.WithTimeout(ctx, doctorNodeHealthTimeout)
			defer cancel()
			expected := nodecert.Peer{Role: nodecert.RoleNode, Name: row.name}
			response, probeErr := deps.probe(
				probeCtx, row.grpcAddr, identity, expected,
				rosterCertificateRevoked(row, now),
			)
			result := outcome{name: row.name}
			switch {
			case probeErr != nil:
				result.issue = probeErr.Error()
			case response == nil:
				result.issue = "empty Health response"
			case response.GetNode() != row.name:
				result.issue = fmt.Sprintf("Health identity mismatch: got node %q", response.GetNode())
			case response.GetStatus() != nodev1.HealthStatus_HEALTH_STATUS_SERVING:
				result.issue = "Health status " + response.GetStatus().String()
			}
			outcomes <- result
		}()
	}
	serving := 0
	for range targets {
		result := <-outcomes
		if result.issue != "" {
			issues = append(issues, result.name+": "+result.issue)
		} else {
			serving++
		}
	}

	sort.Strings(issues)
	if len(issues) > 0 {
		detail := fmt.Sprintf("%d/%d approved node gRPC endpoint(s) serving; %s",
			serving, len(rows), strings.Join(issues, "; "))
		return controlProblem(cfg, detail,
			"repair approved gRPC addresses/certificates and node health; auto mode retains SSH fallback")
	}
	if len(rows) == 0 {
		return controlPass("gateway identity listener " + cfg.GatewayGRPCAddr +
			" reachable with a current certificate; no approved remote nodes")
	}
	if cfg.GatewayGRPCAddr != "" {
		return controlPass(fmt.Sprintf(
			"%d approved node gRPC endpoint(s) serving with exact mTLS identities; gateway identity listener %s reachable",
			len(rows), cfg.GatewayGRPCAddr,
		))
	}
	return controlPass(fmt.Sprintf("%d approved node gRPC endpoint(s) serving with exact mTLS identities", len(rows)))
}

func runNodeLocalControlHealth(ctx context.Context, cfg hostsetup.Config, deps doctorControlDeps) hostsetup.Result {
	if strings.TrimSpace(cfg.NodeGRPCAddr) == "" {
		if cfg.NodeControlTransport == "grpc" {
			return controlFail("gRPC control is required but --node-grpc-addr is empty",
				"configure this node's concrete tailnet listener address")
		}
		return controlPass("gRPC listener not configured; SSH fallback selected")
	}

	now := deps.now().UTC()
	identity, err := deps.loadNode(cfg, now)
	if err != nil {
		return controlProblem(cfg, "node identity: "+err.Error(),
			"re-enroll this approved node to install a current mTLS certificate")
	}
	gateway, err := deps.loadGatewayPeer(cfg.StateDir)
	if err != nil {
		return controlProblem(cfg, "authoritative gateway identity: "+err.Error(),
			"re-enroll the node; never infer the gateway SPIFFE identity from DNS")
	}
	if gateway.Role != nodecert.RoleGateway {
		return controlProblem(cfg, "authoritative gateway identity has role "+string(gateway.Role),
			"re-enroll the node to restore the exact gateway identity")
	}

	probeCtx, cancel := context.WithTimeout(ctx, doctorNodeHealthTimeout)
	defer cancel()
	if err := deps.dialTCP(probeCtx, cfg.NodeGRPCAddr); err != nil {
		return controlProblem(cfg, "node gRPC listener "+cfg.NodeGRPCAddr+": "+err.Error(),
			"start sparkbox and verify this tailnet address is local and reachable")
	}
	return controlPass(fmt.Sprintf(
		"node %s certificate valid until %s; listener %s reachable; expects gateway %s",
		identity.peer.Name, identity.leaf.Leaf.NotAfter.UTC().Format(time.RFC3339),
		cfg.NodeGRPCAddr, gateway.Name,
	))
}

func controlPass(detail string) hostsetup.Result {
	return hostsetup.Result{Status: hostsetup.Pass, Detail: detail}
}

func controlFail(detail, hint string) hostsetup.Result {
	return hostsetup.Result{Status: hostsetup.Fail, Detail: detail, Hint: hint}
}

func controlProblem(cfg hostsetup.Config, detail, hint string) hostsetup.Result {
	if cfg.NodeControlTransport == "grpc" {
		return controlFail(detail, hint)
	}
	return hostsetup.Result{Status: hostsetup.Warn, Detail: detail, Hint: hint}
}

func rosterCertificateRevoked(row doctorRosterNode, now time.Time) nodecert.Revoked {
	return func(serial string) bool {
		return row.status != nodes.StatusApproved ||
			row.certSerial == "" ||
			serial != row.certSerial ||
			row.certExpiresAt == nil ||
			!row.certExpiresAt.After(now) ||
			row.certRevokedAt != nil
	}
}

func loadDoctorGatewayIdentity(cfg hostsetup.Config, now time.Time) (doctorControlIdentity, error) {
	expected := nodecert.Peer{}
	if cfg.ClusterID != "" {
		expected = nodecert.Peer{Role: nodecert.RoleGateway, Name: cfg.ClusterID}
	}
	identity, err := loadDoctorIdentity(
		cfg.StateDir, nodepki.GatewayCertFile, nodepki.GatewayKeyFile,
		nodecert.RoleGateway, expected, x509.ExtKeyUsageClientAuth, now,
	)
	if err == nil && cfg.GatewayGRPCAddr != "" {
		err = verifyDoctorUsage(identity.leaf.Leaf, identity.roots, x509.ExtKeyUsageServerAuth, now)
	}
	return identity, err
}

func loadDoctorNodeIdentity(cfg hostsetup.Config, now time.Time) (doctorControlIdentity, error) {
	expected := nodecert.Peer{}
	if cfg.NodeName != "" {
		expected = nodecert.Peer{Role: nodecert.RoleNode, Name: cfg.NodeName}
	}
	identity, err := loadDoctorIdentity(
		cfg.StateDir, nodepki.NodeCertFile, nodepki.NodeKeyFile,
		nodecert.RoleNode, expected, x509.ExtKeyUsageServerAuth, now,
	)
	if err == nil {
		err = verifyDoctorUsage(identity.leaf.Leaf, identity.roots, x509.ExtKeyUsageClientAuth, now)
	}
	return identity, err
}

func loadDoctorIdentity(
	dir, certFile, keyFile string,
	role nodecert.Role,
	expected nodecert.Peer,
	usage x509.ExtKeyUsage,
	now time.Time,
) (doctorControlIdentity, error) {
	certPEM, err := os.ReadFile(filepath.Join(dir, certFile))
	if err != nil {
		return doctorControlIdentity{}, err
	}
	keyPEM, err := os.ReadFile(filepath.Join(dir, keyFile))
	if err != nil {
		return doctorControlIdentity{}, err
	}
	caPEM, err := os.ReadFile(filepath.Join(dir, nodepki.CACertFile))
	if err != nil {
		return doctorControlIdentity{}, err
	}
	leaf, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return doctorControlIdentity{}, fmt.Errorf("load certificate/key pair: %w", err)
	}
	if len(leaf.Certificate) == 0 {
		return doctorControlIdentity{}, errors.New("certificate chain is empty")
	}
	leaf.Leaf, err = x509.ParseCertificate(leaf.Certificate[0])
	if err != nil {
		return doctorControlIdentity{}, fmt.Errorf("parse leaf certificate: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		return doctorControlIdentity{}, errors.New("malformed node CA certificate")
	}
	if _, err := leaf.Leaf.Verify(x509.VerifyOptions{
		Roots: roots, KeyUsages: []x509.ExtKeyUsage{usage}, CurrentTime: now,
	}); err != nil {
		return doctorControlIdentity{}, fmt.Errorf("verify certificate chain: %w", err)
	}
	peer, err := nodecert.PeerFromCertificate(leaf.Leaf)
	if err != nil || peer.Role != role {
		return doctorControlIdentity{}, fmt.Errorf("%w: got %v, want role %s",
			nodecert.ErrIdentity, leaf.Leaf.URIs, role)
	}
	if expected.Role != "" && peer != expected {
		want, _ := nodecert.Identity(expected.Role, expected.Name)
		return doctorControlIdentity{}, fmt.Errorf("%w: got %s/%s, want %s",
			nodecert.ErrIdentity, peer.Role, peer.Name, want)
	}
	return doctorControlIdentity{leaf: leaf, roots: roots, peer: peer}, nil
}

func verifyDoctorUsage(
	leaf *x509.Certificate,
	roots *x509.CertPool,
	usage x509.ExtKeyUsage,
	now time.Time,
) error {
	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots: roots, KeyUsages: []x509.ExtKeyUsage{usage}, CurrentTime: now,
	}); err != nil {
		return fmt.Errorf("verify certificate chain: %w", err)
	}
	return nil
}

// loadDoctorRoster is intentionally not nodes.Open: doctor is diagnostic and
// must never apply a migration, create a database, or take a write lock. The
// URI opens the existing store in read-only/query-only mode.
func loadDoctorRoster(ctx context.Context, path string) ([]doctorRosterNode, error) {
	query := url.Values{}
	query.Set("mode", "ro")
	query.Add("_pragma", "query_only(1)")
	query.Add("_pragma", "busy_timeout(5000)")
	dsn := (&url.URL{Scheme: "file", Path: path, RawQuery: query.Encode()}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	defer db.Close() //nolint:errcheck

	rows, err := db.QueryContext(ctx, `
		SELECT name, status, grpc_addr, cert_serial,
		       cert_expires_at, cert_revoked_at
		FROM nodes
		WHERE status = ?
		ORDER BY name
	`, nodes.StatusApproved)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck

	var out []doctorRosterNode
	for rows.Next() {
		var row doctorRosterNode
		var expires, revoked sql.NullTime
		if err := rows.Scan(
			&row.name, &row.status, &row.grpcAddr, &row.certSerial,
			&expires, &revoked,
		); err != nil {
			return nil, err
		}
		if expires.Valid {
			value := expires.Time.UTC()
			row.certExpiresAt = &value
		}
		if revoked.Valid {
			value := revoked.Time.UTC()
			row.certRevokedAt = &value
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

func probeDoctorNode(
	ctx context.Context,
	address string,
	identity doctorControlIdentity,
	expected nodecert.Peer,
	revoked nodecert.Revoked,
) (*nodev1.HealthResponse, error) {
	tlsConfig, err := nodecert.ClientTLSConfig(identity.leaf, identity.roots, expected, revoked)
	if err != nil {
		return nil, err
	}
	client, err := grpccontrol.DialTLS(ctx, address, tlsConfig, grpc.WithBlock())
	if err != nil {
		return nil, err
	}
	defer client.Close() //nolint:errcheck
	return client.Health(ctx)
}

func dialDoctorTCP(ctx context.Context, address string) error {
	dialer := net.Dialer{}
	connection, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return err
	}
	return connection.Close()
}
